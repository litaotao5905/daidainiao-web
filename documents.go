package main

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// documentsDir resolves the user's "Documents" ("文档") folder:
//   - Windows: the known-folder API (follows OneDrive / redirected folders),
//     falling back to %USERPROFILE%\Documents
//   - Linux: XDG_DOCUMENTS_DIR from ~/.config/user-dirs.dirs (localized, e.g.
//     ~/文档), falling back to ~/Documents
//   - macOS: ~/Documents
func documentsDir() string {
	home, _ := os.UserHomeDir()
	return documentsDirFor(runtime.GOOS, home, os.Getenv, os.ReadFile, winKnownDocuments)
}

func documentsDirFor(goos, home string, getenv func(string) string, readFile func(string) ([]byte, error), winKnown func() (string, error)) string {
	switch goos {
	case "windows":
		if winKnown != nil {
			if p, err := winKnown(); err == nil && p != "" {
				return p
			}
		}
		if up := getenv("USERPROFILE"); up != "" {
			return filepath.Join(up, "Documents")
		}
	case "linux", "freebsd", "openbsd", "netbsd":
		cfgHome := getenv("XDG_CONFIG_HOME")
		if cfgHome == "" && home != "" {
			cfgHome = filepath.Join(home, ".config")
		}
		if cfgHome != "" && readFile != nil {
			if b, err := readFile(filepath.Join(cfgHome, "user-dirs.dirs")); err == nil {
				if p := parseXDGDocuments(string(b), home); p != "" {
					return p
				}
			}
		}
		if v := getenv("XDG_DOCUMENTS_DIR"); v != "" && filepath.IsAbs(v) {
			return v
		}
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, "Documents")
}

// parseXDGDocuments reads XDG_DOCUMENTS_DIR="$HOME/文档" from user-dirs.dirs.
func parseXDGDocuments(content, home string) string {
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "XDG_DOCUMENTS_DIR=") {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(line, "XDG_DOCUMENTS_DIR="))
		v = strings.Trim(v, `"'`)
		if strings.HasPrefix(v, "$HOME") {
			if home == "" {
				return ""
			}
			v = home + strings.TrimPrefix(v, "$HOME")
		}
		if v == "" || !filepath.IsAbs(v) || filepath.Clean(v) == filepath.Clean(home) {
			return "" // XDG uses $HOME itself to mean "unset"
		}
		return filepath.Clean(v)
	}
	return ""
}

// inDocker reports whether the process runs in a container.
func inDocker() bool {
	if v := os.Getenv("FEISHU_IN_DOCKER"); v != "" {
		return v == "1" || strings.EqualFold(v, "true")
	}
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

// resolveDownloadDir picks the base directory for downloaded files.
func resolveDownloadDir(cfg Config, mock bool) string {
	d := strings.TrimSpace(cfg.DownloadDir)
	if d != "" {
		if d == "~" || strings.HasPrefix(d, "~/") || strings.HasPrefix(d, `~\`) {
			if home, err := os.UserHomeDir(); err == nil {
				d = filepath.Join(home, strings.TrimLeft(d[1:], `/\`))
			}
		}
		if abs, err := filepath.Abs(d); err == nil {
			return abs
		}
		return d
	}
	name := "FeishuTopics"
	if mock {
		name = "FeishuTopics-mock"
	}
	if inDocker() {
		return filepath.Join(cfg.DataDir, name)
	}
	if docs := documentsDir(); docs != "" {
		return filepath.Join(docs, name)
	}
	return filepath.Join(cfg.DataDir, name)
}
