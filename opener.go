package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// cmdRunner starts a program. If wait > 0 it reports the exit status of a
// program that finishes within that time; otherwise (or when it keeps running)
// it returns nil. Tests inject a fake so no program is ever launched.
type cmdRunner func(name string, args []string, wait time.Duration) error

func execRunner(name string, args []string, wait time.Duration) error {
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	if wait <= 0 {
		return nil
	}
	select {
	case err := <-done:
		return err
	case <-time.After(wait):
		return nil
	}
}

// Opener opens files / reveals them in the platform file manager. Arguments
// are always passed as separate argv entries (never through a shell).
type Opener struct {
	GOOS     string
	Run      cmdRunner
	LookPath func(string) (string, error)
	HostURL  string
}

func NewOpener(goos string) *Opener {
	run := cmdRunner(execRunner)
	if os.Getenv("FEISHU_OPEN_DRYRUN") == "1" { // UI tests: log instead of launching anything
		run = func(name string, args []string, _ time.Duration) error {
			log.Printf("open (dry-run): %s %q", name, args)
			return nil
		}
	}
	return &Opener{
		GOOS:     goos,
		Run:      run,
		LookPath: exec.LookPath,
		HostURL:  strings.TrimRight(strings.TrimSpace(os.Getenv("FEISHU_HOST_OPENER")), "/"),
	}
}

// dangerousExt are file types the default handler would execute rather than
// display; they are never opened from the UI (reveal in folder still works).
var dangerousExt = map[string]bool{
	"exe": true, "com": true, "bat": true, "cmd": true, "msi": true, "msp": true, "scr": true, "pif": true,
	"ps1": true, "psm1": true, "vbs": true, "vbe": true, "js": true, "jse": true, "wsf": true, "wsh": true,
	"hta": true, "cpl": true, "msc": true, "reg": true, "lnk": true, "url": true, "dll": true, "jar": true,
	"app": true, "command": true, "sh": true, "bash": true, "zsh": true, "csh": true, "desktop": true,
	"appimage": true, "run": true, "pkg": true, "mpkg": true, "workflow": true, "scpt": true, "terminal": true,
	"gadget": true, "inf": true, "chm": true, "settingcontent-ms": true,
}

var ErrExecutable = errors.New("出于安全考虑，不会直接打开可执行文件或脚本，请使用「在文件夹中显示」后自行处理")

// openCmd builds the command that opens path with the default application.
func openCmd(goos, path string) (string, []string) {
	switch goos {
	case "darwin":
		return "open", []string{path}
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", path}
	default:
		return "xdg-open", []string{path}
	}
}

// revealCmds builds the commands that show path in the file manager, in the
// order they should be tried.
func revealCmds(goos, path string, dbus bool) [][]string {
	switch goos {
	case "darwin":
		return [][]string{{"open", "-R", path}}
	case "windows":
		return [][]string{{"explorer.exe", "/select," + path}}
	default:
		var out [][]string
		if dbus {
			u := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
			out = append(out, []string{"dbus-send", "--session", "--print-reply", "--dest=org.freedesktop.FileManager1",
				"/org/freedesktop/FileManager1", "org.freedesktop.FileManager1.ShowItems", "array:string:" + u, "string:"})
		}
		return append(out, []string{"xdg-open", filepath.Dir(path)})
	}
}

func (o *Opener) callHost(path string, reveal bool) error {
	body, _ := json.Marshal(map[string]any{"path": path, "reveal": reveal})
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(o.HostURL+"/open", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	var res struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&res)
	if res.Error != "" {
		return errors.New(res.Error)
	}
	return fmt.Errorf("host opener HTTP %d", resp.StatusCode)
}

// Open opens a file with the default application.
func (o *Opener) Open(path string) error {
	if dangerousExt[strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))] {
		return ErrExecutable
	}
	if o.HostURL != "" {
		return o.callHost(path, false)
	}
	name, args := openCmd(o.GOOS, path)
	return o.Run(name, args, 0)
}

// Reveal shows the file selected in the platform file manager.
func (o *Opener) Reveal(path string) error {
	if o.HostURL != "" {
		return o.callHost(path, true)
	}
	dbus := false
	if o.GOOS != "darwin" && o.GOOS != "windows" && o.LookPath != nil {
		_, err := o.LookPath("dbus-send")
		dbus = err == nil
	}
	cmds := revealCmds(o.GOOS, path, dbus)
	var last error
	for _, c := range cmds {
		wait := time.Duration(0)
		if c[0] == "dbus-send" {
			wait = 3 * time.Second
		}
		if err := o.Run(c[0], c[1:], wait); err != nil {
			last = err
			continue
		}
		return nil
	}
	return last
}

// ServeHostOpener runs a tiny loopback service on the host OS so a local Docker
// container (e.g. OrbStack) can open files or reveal folders inside allowedDir.
func ServeHostOpener(addr, allowedDir string) error {
	baseClean := filepath.Clean(allowedDir)
	if !filepath.IsAbs(baseClean) {
		return fmt.Errorf("允许目录必须是绝对路径: %s", allowedDir)
	}
	localOpener := &Opener{GOOS: runtime.GOOS, Run: execRunner, LookPath: exec.LookPath}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "dir": baseClean})
	})
	mux.HandleFunc("POST /open", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Path   string `json:"path"`
			Reveal bool   `json:"reveal"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || req.Path == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无效的请求参数"})
			return
		}
		cleanPath := filepath.Clean(req.Path)
		rel, err := filepath.Rel(baseClean, cleanPath)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "只允许打开下载目录内的文件"})
			return
		}
		var runErr error
		if req.Reveal {
			runErr = localOpener.Reveal(cleanPath)
		} else {
			runErr = localOpener.Open(cleanPath)
		}
		if runErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": runErr.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	log.Printf("宿主机文件打开助手已启动：http://%s（允许目录：%s）", addr, baseClean)
	return http.ListenAndServe(addr, mux)
}
