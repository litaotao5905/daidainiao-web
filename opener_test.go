package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestHostOpenerPickAndOpen(t *testing.T) {
	t.Setenv("FEISHU_OPEN_DRYRUN", "1")
	root := t.TempDir()
	t.Setenv("FEISHU_OPEN_DRYRUN_DIR", root)
	allowed := filepath.Join(root, "downloads") + "," + root
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		case "/pick":
			o := &Opener{GOOS: "darwin", Run: execRunner, LookPath: nil}
			path, cancelled, err := o.Pick(r.Context())
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			if cancelled {
				writeJSON(w, http.StatusOK, map[string]bool{"cancelled": true})
				return
			}
			if !openerAllowed(path, []string{filepath.Join(root, "downloads"), root}) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "denied"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"path": path})
		case "/open":
			var req struct {
				Path string `json:"path"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if !openerAllowed(req.Path, []string{filepath.Join(root, "downloads"), root}) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "denied"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	o := &Opener{HostURL: ts.URL}
	path, cancelled, err := o.Pick(context.Background())
	if err != nil || cancelled || path == "" {
		t.Fatalf("pick: %q cancelled=%v err=%v", path, cancelled, err)
	}
	if err := o.callHost(filepath.Join(root, "downloads", "note.txt"), true); err != nil {
		t.Fatalf("open allowed path: %v", err)
	}
	if err := o.callHost(filepath.Join(root, "..", "outside.txt"), false); err == nil {
		t.Fatal("expected forbidden outside allowed roots")
	}
	if _, err := parseHostOpenerDirs(allowed); err != nil {
		t.Fatal(err)
	}
}

func TestHostOpenerServeMuxPick(t *testing.T) {
	t.Setenv("FEISHU_OPEN_DRYRUN", "1")
	root := t.TempDir()
	t.Setenv("FEISHU_OPEN_DRYRUN_DIR", root)
	mux := http.NewServeMux()
	allowed := []string{filepath.Clean(root)}
	localOpener := &Opener{GOOS: "darwin", Run: execRunner}
	mux.HandleFunc("POST /pick", func(w http.ResponseWriter, r *http.Request) {
		path, cancelled, err := localOpener.Pick(r.Context())
		if cancelled || err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"cancelled": true, "error": errString(err)})
			return
		}
		if !openerAllowed(path, allowed) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "denied"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"path": path})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/pick", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["path"] == "" {
		t.Fatalf("expected dry-run path, got %+v", out)
	}
	if !filepath.IsAbs(out["path"]) {
		t.Fatalf("unexpected pick path: %+v", out)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}