package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------- helpers ----------

type fakeFetch struct {
	dir   string
	calls atomic.Int32
	mu    sync.Mutex
	data  map[string][]byte // file key -> bytes
	names map[string]string
	err   error
	delay time.Duration
}

func newFakeFetch(t *testing.T) *fakeFetch {
	return &fakeFetch{dir: t.TempDir(), data: map[string][]byte{}, names: map[string]string{}}
}

func (f *fakeFetch) fetch(ctx context.Context, e DLEntry, force bool) (*dlFetched, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	b := f.data[e.FileKey]
	if b == nil {
		b = []byte("content of " + e.FileKey)
	}
	p := filepath.Join(f.dir, "src-"+e.FileKey)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return nil, err
	}
	return &dlFetched{Path: p, Name: f.names[e.FileKey], Cleanup: func(bool) { os.Remove(p) }}, nil
}

func newTestDM(t *testing.T, ff *fakeFetch) (*DownloadManager, string) {
	t.Helper()
	base := filepath.Join(t.TempDir(), "FeishuTopics")
	return NewDownloadManager(base, "", filepath.Join(t.TempDir(), "index.json"), ff.fetch), base
}

func att(kind, name, msg, key string) Attachment {
	rt := "file"
	if kind == "image" {
		rt = "image"
	}
	return Attachment{ID: rt + ":" + msg + ":" + key, Kind: kind, Name: name, MessageID: msg, FileKey: key, ResType: rt, Time: 1760000000000}
}

func waitEntry(t *testing.T, dm *DownloadManager, a Attachment) DLEntry {
	t.Helper()
	rt, mid, fk, _ := dlSpec(a)
	v, _, found, _ := dm.Ensure(context.Background(), rt, mid, fk)
	if !found {
		t.Fatalf("entry not registered: %s", a.ID)
	}
	return v
}

var tc1 = TopicCtx{ChatID: "oc_1", ChatName: "产品/研发 群", TopicKey: "om_root_aaaaaa", TopicTitle: "发版计划: Q4?"}

// ---------- tests ----------

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"a/b\\c.txt":                      "a_b_c.txt",
		"../../etc/passwd":                ".._.._etc_passwd",
		"..":                              "_",
		"":                                "_",
		"  .hidden. ":                     ".hidden",
		"a:b*c?d\"e<f>g|h.pdf":            "a_b_c_d_e_f_g_h.pdf",
		"CON.txt":                         "_CON.txt",
		"com1":                            "_com1",
		"nul":                             "_nul",
		"ok\x00\x01name\u202e.txt":        "okname.txt",
		"报告：最终版（v2）.docx":                 "报告：最终版（v2）.docx",
		"trailing dot.":                   "trailing dot",
		strings.Repeat("长", 300) + ".pdf": strings.Repeat("长", 116) + ".pdf",
	}
	for in, want := range cases {
		if got := sanitizeName(in, 120); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDocumentsDirFor(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	read := func(content string) func(string) ([]byte, error) {
		return func(p string) ([]byte, error) {
			if strings.HasSuffix(p, "user-dirs.dirs") && content != "" {
				return []byte(content), nil
			}
			return nil, os.ErrNotExist
		}
	}
	if got := documentsDirFor("darwin", "/Users/li", env(nil), read(""), nil); got != filepath.Join("/Users/li", "Documents") {
		t.Errorf("darwin: %q", got)
	}
	if got := documentsDirFor("linux", "/home/li", env(nil), read(`XDG_DOCUMENTS_DIR="$HOME/文档"`), nil); got != filepath.Join("/home/li", "文档") {
		t.Errorf("linux localized: %q", got)
	}
	if got := documentsDirFor("linux", "/home/li", env(nil), read(`XDG_DOCUMENTS_DIR="$HOME/"`), nil); got != filepath.Join("/home/li", "Documents") {
		t.Errorf("linux unset (=$HOME): %q", got)
	}
	if got := documentsDirFor("linux", "/home/li", env(nil), read(""), nil); got != filepath.Join("/home/li", "Documents") {
		t.Errorf("linux default: %q", got)
	}
	if got := documentsDirFor("linux", "/home/li", env(map[string]string{"XDG_CONFIG_HOME": "/cfg"}), func(p string) ([]byte, error) {
		if p == filepath.Join("/cfg", "user-dirs.dirs") {
			return []byte("XDG_DOCUMENTS_DIR=\"/data/docs\"\n"), nil
		}
		return nil, os.ErrNotExist
	}, nil); got != "/data/docs" {
		t.Errorf("linux XDG_CONFIG_HOME: %q", got)
	}
	win := func() (string, error) { return `D:\OneDrive\文档`, nil }
	if got := documentsDirFor("windows", `C:\Users\li`, env(nil), nil, win); got != `D:\OneDrive\文档` {
		t.Errorf("windows known folder: %q", got)
	}
	fail := func() (string, error) { return "", errors.New("x") }
	if got := documentsDirFor("windows", `C:\Users\li`, env(map[string]string{"USERPROFILE": `C:\Users\li`}), nil, fail); got != filepath.Join(`C:\Users\li`, "Documents") {
		t.Errorf("windows fallback: %q", got)
	}
}

func TestResolveDownloadDir(t *testing.T) {
	cfg := defaultConfig()
	cfg.DataDir = "/data"
	cfg.DownloadDir = filepath.Join(t.TempDir(), "x")
	if got := resolveDownloadDir(cfg, false); got != cfg.DownloadDir {
		t.Errorf("explicit dir: %q", got)
	}
	cfg.DownloadDir = ""
	t.Setenv("FEISHU_IN_DOCKER", "1")
	if got := resolveDownloadDir(cfg, false); got != filepath.Join("/data", "FeishuTopics") {
		t.Errorf("docker default: %q", got)
	}
	t.Setenv("FEISHU_IN_DOCKER", "0")
	got := resolveDownloadDir(cfg, true)
	if !strings.HasSuffix(got, "FeishuTopics-mock") {
		t.Errorf("mock default: %q", got)
	}
	if docs := documentsDir(); docs != "" && filepath.Dir(got) != docs {
		t.Errorf("default should live in Documents %q: %q", docs, got)
	}
	c2 := defaultConfig()
	t.Setenv("FEISHU_DOWNLOAD_DIR", "/tmp/dl-env")
	c2.ApplyEnv()
	if c2.DownloadDir != "/tmp/dl-env" {
		t.Errorf("env: %q", c2.DownloadDir)
	}
}

func TestDownloadDedupeAndSizeChange(t *testing.T) {
	ff := newFakeFetch(t)
	ff.names["fk1"] = "回滚方案.pdf"
	dm, base := newTestDM(t, ff)
	a := att("file", "回滚方案.pdf", "om_1", "fk1")

	dm.Register(tc1, []Attachment{a})
	v := waitEntry(t, dm, a)
	if v.Status != dlDone || ff.calls.Load() != 1 {
		t.Fatalf("first load: %+v calls=%d", v, ff.calls.Load())
	}
	want := filepath.Join(base, "产品_研发 群", "发版计划_ Q4__"+dlShort("om_root_aaaaaa"), "回滚方案.pdf")
	if v.Path != want {
		t.Fatalf("path %q, want %q", v.Path, want)
	}
	if b, _ := os.ReadFile(want); string(b) != "content of fk1" {
		t.Fatalf("content %q", b)
	}
	// a second load (even from a new manager reading the index) must not download again
	for i := 0; i < 3; i++ {
		dm.Register(tc1, []Attachment{a})
	}
	dm2 := NewDownloadManager(base, "", dm.idxPath, ff.fetch)
	dm2.Register(tc1, []Attachment{a})
	waitEntry(t, dm2, a)
	if ff.calls.Load() != 1 {
		t.Fatalf("re-downloaded an existing file: calls=%d", ff.calls.Load())
	}

	// Local edits remain usable until an update is requested.
	if err := os.WriteFile(want, []byte("local changes"), 0o644); err != nil {
		t.Fatal(err)
	}
	dm.Register(tc1, []Attachment{a})
	v = waitEntry(t, dm, a)
	if ff.calls.Load() != 1 {
		t.Fatal("local edits were overwritten on ordinary registration")
	}
	if err := dm.Redownload(v.ID); err != nil {
		t.Fatal(err)
	}
	v = waitEntry(t, dm, a)
	if v.Status != dlDone || v.BackupRel == "" {
		t.Fatalf("local edits not backed up: %+v", v)
	}
	backup, _ := os.ReadFile(filepath.Join(base, filepath.FromSlash(v.BackupRel)))
	if string(backup) != "local changes" {
		t.Fatalf("backup: %q", backup)
	}
	os.Remove(want)
	dm.Register(tc1, []Attachment{a})
	v = waitEntry(t, dm, a)
	if v.Status != dlDone || ff.calls.Load() != 3 {
		t.Fatalf("deleted file not restored: %+v", v)
	}

}

func TestDownloadSingleflightAndLimit(t *testing.T) {
	ff := newFakeFetch(t)
	ff.delay = 80 * time.Millisecond
	dm, _ := newTestDM(t, ff)
	var atts []Attachment
	for i := 0; i < 8; i++ {
		atts = append(atts, att("file", "f.bin", "om_c", "k"+string(rune('a'+i))))
	}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ { // five concurrent loads of the same conversation
		wg.Add(1)
		go func() { defer wg.Done(); dm.Register(tc1, atts) }()
	}
	wg.Wait()
	for _, a := range atts {
		waitEntry(t, dm, a)
	}
	if n := ff.calls.Load(); n != 8 {
		t.Fatalf("concurrent loads fetched %d times, want 8", n)
	}
	// names collide inside one topic dir: "f.bin", "f (2).bin", ...
	seen := map[string]bool{}
	for _, e := range dm.List("") {
		if seen[e.Path] {
			t.Fatalf("duplicate path %s", e.Path)
		}
		seen[e.Path] = true
	}
	if !seen[filepath.Join(dm.Base(), "产品_研发 群", "发版计划_ Q4__"+dlShort("om_root_aaaaaa"), "f (2).bin")] {
		t.Fatalf("collision naming missing: %v", seen)
	}
}

func TestDownloadCollisionWithExistingFile(t *testing.T) {
	ff := newFakeFetch(t)
	ff.names["k1"] = "report.pdf"
	dm, base := newTestDM(t, ff)
	dir := filepath.Join(base, "产品_研发 群", "发版计划_ Q4__"+dlShort("om_root_aaaaaa"))
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "report.pdf"), []byte("user's own file"), 0o644)
	a := att("file", "report.pdf", "om_1", "k1")
	dm.Register(tc1, []Attachment{a})
	v := waitEntry(t, dm, a)
	if filepath.Base(v.Path) != "report (2).pdf" {
		t.Fatalf("existing user file must not be overwritten: %s", v.Path)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "report.pdf")); string(b) != "user's own file" {
		t.Fatal("user's file modified")
	}
}

func TestDownloadChatDirCollisionAndImageNames(t *testing.T) {
	ff := newFakeFetch(t)
	dm, _ := newTestDM(t, ff)
	ff.data["img_v3_aaaa1111"] = pngBytes
	a := att("image", "图片", "om_img", "img_v3_aaaa1111")
	dm.Register(tc1, []Attachment{a})
	v := waitEntry(t, dm, a)
	if !strings.HasPrefix(v.Name, "image_") || !strings.HasSuffix(v.Name, ".png") || !strings.Contains(v.Name, "aaa1111"[1:]) {
		t.Fatalf("image name %q (mime %s)", v.Name, v.Mime)
	}
	tc2 := TopicCtx{ChatID: "oc_2", ChatName: "产品/研发 群", TopicKey: "om_other", TopicTitle: "x"}
	a2 := att("file", "a.txt", "om_2", "k2")
	dm.Register(tc2, []Attachment{a2})
	v2 := waitEntry(t, dm, a2)
	c1 := strings.Split(filepath.ToSlash(strings.TrimPrefix(v.Path, dm.Base())), "/")[1]
	c2 := strings.Split(filepath.ToSlash(strings.TrimPrefix(v2.Path, dm.Base())), "/")[1]
	if c1 == c2 {
		t.Fatalf("two groups with one name share a folder: %s", c1)
	}
}

func TestDownloadFailureBackoffAndRetry(t *testing.T) {
	ff := newFakeFetch(t)
	ff.err = &FeishuError{Code: 234004, HTTPStatus: 400, Msg: "App not in chat."}
	dm, _ := newTestDM(t, ff)
	now := time.Now()
	dm.now = func() time.Time { return now }
	a := att("image", "图片", "om_1", "img_v3_x")
	dm.Register(tc1, []Attachment{a})
	v := waitEntry(t, dm, a)
	if v.Status != dlFailed || v.Code != 234004 || !strings.Contains(v.Reason, "机器人不在该群") || ff.calls.Load() != 1 {
		t.Fatalf("fail state %+v calls=%d", v, ff.calls.Load())
	}
	// within the backoff window nothing is retried
	dm.Register(tc1, []Attachment{a})
	waitEntry(t, dm, a)
	if ff.calls.Load() != 1 {
		t.Fatalf("retried inside backoff: %d", ff.calls.Load())
	}
	// later it is retried automatically; success clears the failure
	ff.mu.Lock()
	ff.err = nil
	ff.mu.Unlock()
	now = now.Add(time.Hour)
	dm.Register(tc1, []Attachment{a})
	v = waitEntry(t, dm, a)
	if v.Status != dlDone || v.Reason != "" || v.Tries != 0 {
		t.Fatalf("after retry %+v", v)
	}
	// manual retry ignores backoff
	ff.mu.Lock()
	ff.err = errors.New("boom")
	ff.mu.Unlock()
	if err := dm.Redownload(v.ID); err != nil {
		t.Fatal(err)
	}
	v = waitEntry(t, dm, a)
	if v.Status != dlDone || v.SyncStatus != "failed" || v.Reason == "" {
		t.Fatalf("failed refresh must retain old copy: %+v", v)
	}
	if err := dm.Redownload("00000000000000000000"); err == nil {
		t.Fatal("unknown id accepted")
	}
}

func TestBackoffGrowth(t *testing.T) {
	if dlBackoff(1, false) != 20*time.Second || dlBackoff(2, false) != 40*time.Second {
		t.Fatal("backoff should double")
	}
	if dlBackoff(30, false) != 15*time.Minute {
		t.Fatal("backoff should be capped")
	}
	if dlBackoff(1, true) < 5*time.Minute {
		t.Fatal("permanent refusals back off for minutes")
	}
}

func TestSafePath(t *testing.T) {
	ff := newFakeFetch(t)
	dm, base := newTestDM(t, ff)
	os.MkdirAll(filepath.Join(base, "g", "t"), 0o755)
	os.WriteFile(filepath.Join(base, "g", "t", "ok.txt"), []byte("x"), 0o644)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s"), 0o644)

	if p, err := dm.safePath("g/t/ok.txt"); err != nil || !strings.HasSuffix(p, "ok.txt") {
		t.Fatalf("valid path rejected: %v", err)
	}
	for _, bad := range []string{"../x", "g/../../x", "/etc/passwd", "", "g/t/../../../etc/passwd", "g\x00/t"} {
		if _, err := dm.safePath(bad); err == nil {
			t.Errorf("path %q accepted", bad)
		}
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(outside, filepath.Join(base, "g", "link")); err != nil {
			t.Fatal(err)
		}
		if _, err := dm.safePath("g/link/secret.txt"); !errors.Is(err, ErrOutsideDir) {
			t.Errorf("symlinked dir escaping the base accepted: %v", err)
		}
		if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(base, "g", "t", "file-link")); err != nil {
			t.Fatal(err)
		}
		if _, err := dm.safePath("g/t/file-link"); !errors.Is(err, ErrOutsideDir) {
			t.Errorf("symlinked file escaping the base accepted: %v", err)
		}
		if err := os.Symlink("ok.txt", filepath.Join(base, "g", "t", "inner")); err == nil {
			if _, err := dm.safePath("g/t/inner"); err != nil {
				t.Errorf("symlink inside the base rejected: %v", err)
			}
		}
	}
	if _, err := dm.safePath("g/t"); err == nil {
		t.Error("directory accepted")
	}
	// Resolve refuses ids that are not in the index
	if _, _, err := dm.Resolve("0123456789abcdef0123"); !errors.Is(err, errNoSuchFile) {
		t.Errorf("unknown id: %v", err)
	}
	// a tampered index entry pointing outside is refused
	dm.mu.Lock()
	dm.entries["k"] = &DLEntry{ID: "aaaaaaaaaaaaaaaaaaaa", Key: "k", Status: dlDone, Rel: "../" + filepath.Base(outside) + "/secret.txt", Size: 1}
	dm.mu.Unlock()
	if _, _, err := dm.Resolve("aaaaaaaaaaaaaaaaaaaa"); err == nil {
		t.Error("index entry escaping the folder resolved")
	}
}

type runCall struct {
	name string
	args []string
}

func fakeOpener(goos string, calls *[]runCall, fail map[string]error) *Opener {
	return &Opener{GOOS: goos, LookPath: func(n string) (string, error) {
		if n == "dbus-send" {
			return "/usr/bin/dbus-send", nil
		}
		return "", errors.New("nf")
	}, Run: func(name string, args []string, wait time.Duration) error {
		*calls = append(*calls, runCall{name, args})
		return fail[name]
	}}
}

func TestOpenerCommands(t *testing.T) {
	var calls []runCall
	check := func(goos, path string, reveal bool, want ...runCall) {
		t.Helper()
		calls = nil
		o := fakeOpener(goos, &calls, nil)
		var err error
		if reveal {
			err = o.Reveal(path)
		} else {
			err = o.Open(path)
		}
		if err != nil {
			t.Fatalf("%s: %v", goos, err)
		}
		if len(calls) != len(want) {
			t.Fatalf("%s reveal=%v calls=%v want %v", goos, reveal, calls, want)
		}
		for i := range want {
			if calls[i].name != want[i].name || strings.Join(calls[i].args, "\x00") != strings.Join(want[i].args, "\x00") {
				t.Fatalf("%s reveal=%v call %d = %v, want %v", goos, reveal, i, calls[i], want[i])
			}
		}
	}
	check("darwin", "/Users/li/Documents/a b.pdf", false, runCall{"open", []string{"/Users/li/Documents/a b.pdf"}})
	check("darwin", "/Users/li/Documents/a b.pdf", true, runCall{"open", []string{"-R", "/Users/li/Documents/a b.pdf"}})
	check("windows", `C:\Users\li\Documents\a & b.pdf`, false, runCall{"rundll32", []string{"url.dll,FileProtocolHandler", `C:\Users\li\Documents\a & b.pdf`}})
	check("windows", `C:\Users\li\Documents\a b.pdf`, true, runCall{"explorer.exe", []string{`/select,C:\Users\li\Documents\a b.pdf`}})
	check("linux", "/home/li/文档/a.pdf", false, runCall{"xdg-open", []string{"/home/li/文档/a.pdf"}})
	check("linux", "/home/li/文档/a b.pdf", true, runCall{"dbus-send", []string{"--session", "--print-reply", "--dest=org.freedesktop.FileManager1",
		"/org/freedesktop/FileManager1", "org.freedesktop.FileManager1.ShowItems", "array:string:file:///home/li/%E6%96%87%E6%A1%A3/a%20b.pdf", "string:"}})

	// no file manager service on D-Bus: fall back to opening the parent folder
	calls = nil
	o := fakeOpener("linux", &calls, map[string]error{"dbus-send": errors.New("no service")})
	if err := o.Reveal("/home/li/Documents/a.pdf"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[1].name != "xdg-open" || calls[1].args[0] != "/home/li/Documents" {
		t.Fatalf("fallback: %v", calls)
	}
	// executables and scripts are never opened
	calls = nil
	for _, n := range []string{"/x/setup.exe", "/x/run.SH", "/x/a.bat", "/x/App.app", "/x/p.ps1", "/x/t.jar", "/x/s.js"} {
		if err := fakeOpener("linux", &calls, nil).Open(n); !errors.Is(err, ErrExecutable) {
			t.Errorf("%s not refused: %v", n, err)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("a program was started for an executable: %v", calls)
	}
}

// ---------- server level ----------

type dlEnv struct {
	srv   *Server
	svc   *Service
	h     http.Handler
	calls *[]runCall
	csrf  string
	api   *countAPI
}

type countAPI struct {
	*MockAPI
	dl atomic.Int32
}

func (c *countAPI) Download(ctx context.Context, m, k, t string) (*Download, error) {
	c.dl.Add(1)
	return c.MockAPI.Download(ctx, m, k, t)
}

func newDLEnv(t *testing.T) *dlEnv {
	t.Helper()
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.DataDir = dir
	cfg.DownloadDir = filepath.Join(dir, "docs", "FeishuTopics")
	cfg.Normalize()
	auth := NewAuth(cfg, true, filepath.Join(dir, "token.json"))
	if err := auth.Callback(context.Background(), "mock"); err != nil {
		t.Fatal(err)
	}
	api := &countAPI{MockAPI: NewMockAPI(auth)}
	svc := NewService(api, NewCache(filepath.Join(dir, "cache", "json")), cfg, auth)
	sel, _, _ := LoadSelection(filepath.Join(dir, "selection.json"))
	t.Setenv("FEISHU_IN_DOCKER", "0")
	t.Setenv("FEISHU_DISABLE_OPEN", "")
	srv := NewServer(svc, sel, cfg, true, nil)
	var calls []runCall
	srv.opener = fakeOpener("linux", &calls, nil)
	srv.noOpen = false
	h := srv.Handler()
	t.Cleanup(func() { waitTopicFiles(t, svc) })
	return &dlEnv{srv: srv, svc: svc, h: h, calls: &calls, csrf: srv.csrf, api: api}
}

// loadAll opens every topic of the mock chats (as the UI would) and waits
// until all downloads settled.
func (e *dlEnv) loadAll(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, chat := range []string{"oc_mock_dev", "oc_mock_proj", "oc_mock_design"} {
		tp, err := e.svc.Topics(ctx, chat, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, tt := range tp.Topics {
			if _, err := e.svc.Thread(ctx, tt.ThreadID, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Simulate explicit download clicks after the catalogue has loaded.
	for _, item := range e.svc.dl.List("") {
		if item.Status == dlNotDownloaded {
			if err := e.svc.dl.Redownload(item.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		busy := topicFilesRunning(e.svc)
		for _, it := range e.svc.dl.List("") {
			if it.Status == dlWaiting || it.Status == dlRunning {
				busy = true
			}
		}
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("downloads did not settle")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *dlEnv) do(method, path, body string, mut func(*http.Request)) (*httptest.ResponseRecorder, map[string]any) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "localhost:8080"
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("Content-Type", "application/json")
	if mut != nil {
		mut(req)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func (e *dlEnv) post(path, id string) (*httptest.ResponseRecorder, map[string]any) {
	return e.do("POST", path, `{"id":"`+id+`"}`, func(r *http.Request) { r.Header.Set(csrfHeader, e.csrf) })
}

func (e *dlEnv) find(t *testing.T, name string) DLEntry {
	t.Helper()
	for _, it := range e.svc.dl.List("") {
		if it.Name == name {
			return it
		}
	}
	t.Fatalf("no downloaded file named %q in %v", name, names(e.svc.dl.List("")))
	return DLEntry{}
}

func names(l []DLEntry) []string {
	var o []string
	for _, e := range l {
		o = append(o, e.Name+"("+e.Status+")")
	}
	return o
}

func TestMockConversationDownloadFlow(t *testing.T) {
	e := newDLEnv(t)
	e.loadAll(t)
	base := e.svc.dl.Base()

	csv := e.find(t, "q4_budget.csv")
	if csv.Status != dlDone || !strings.HasPrefix(csv.Path, base) || !strings.Contains(csv.Path, "产品研发讨论群") {
		t.Fatalf("csv: %+v", csv)
	}
	if b, err := os.ReadFile(csv.Path); err != nil || !bytes.Contains(b, []byte("研发,320,210")) {
		t.Fatalf("csv content: %v %q", err, b)
	}
	// same Feishu file key, two messages, two original names
	e.find(t, "Q4发版风险评估.pdf")
	e.find(t, "客户验收材料-V3.pdf")
	// images get a generated name
	var img DLEntry
	for _, it := range e.svc.dl.List("") {
		if it.Kind == "image" && it.Status == dlDone {
			img = it
			break
		}
	}
	if !strings.HasPrefix(img.Name, "image_") || !strings.HasSuffix(img.Name, ".png") {
		t.Fatalf("image entry %+v", img)
	}
	// the Drive file linked in a message is downloaded under its real name
	drv := e.find(t, "发版checklist.txt")
	if drv.ResType != "drive" || drv.Status != dlDone {
		t.Fatalf("drive file: %+v", drv)
	}
	// images that Feishu refuses: compact failure with reason, not a banner
	var failed *DLEntry
	for _, it := range e.svc.dl.List("") {
		if it.Status == dlFailed {
			it := it
			failed = &it
		}
	}
	if failed == nil || failed.Code != 234004 || failed.Reason == "" {
		t.Fatalf("expected the restricted image to fail: %+v", failed)
	}
	// the resource proxy serves registered files from the local copy
	before := e.api.dl.Load()
	rec, _ := e.do("GET", "/api/resource?message_id=om_mock_b04&file_key=file_mock_csv&type=file", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "研发,320,210") || e.api.dl.Load() != before {
		t.Fatalf("resource: %d (api calls %d->%d)", rec.Code, before, e.api.dl.Load())
	}
	rec, _ = e.do("GET", "/api/resource?message_id=om_mock_f04&file_key=img_mock_restricted&type=image", "", nil)
	if rec.Code != 502 {
		t.Fatalf("failed resource should be 502, got %d", rec.Code)
	}
	// /api/files/{id}/content
	rec, _ = e.do("GET", "/api/files/"+csv.ID+"/content", "", nil)
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("content: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	// the resource cache copy is dropped once the file is in the folder
	if _, err := os.Stat(resBase(e.srv.cfg.DataDir, "om_mock_b04", "file_mock_csv", "file") + ".bin"); err == nil {
		t.Error("cache copy not removed")
	}
	// the thread response lists download states
	tr, _ := e.svc.Thread(context.Background(), "omt_mock_002", false)
	if len(tr.Downloads) == 0 {
		t.Fatalf("thread response without downloads")
	}
	// reloading everything must not download anything again
	n := e.api.dl.Load()
	e.loadAll(t)
	// (only the failed, backed-off image may not be retried either)
	if e.api.dl.Load() != n {
		t.Fatalf("reload fetched again: %d -> %d", n, e.api.dl.Load())
	}
	// listing endpoint, per topic and global
	rec, out := e.do("GET", "/api/downloads", "", nil)
	if rec.Code != 200 || out["enabled"] != true || out["opener"] != true || len(out["items"].([]any)) < 8 {
		t.Fatalf("downloads list: %d %v", rec.Code, out["items"])
	}
	// index file written
	if _, err := os.Stat(filepath.Join(e.srv.cfg.DataDir, "downloads", "index.json")); err != nil {
		t.Fatal(err)
	}
}

func TestFileOpenRevealSecurity(t *testing.T) {
	e := newDLEnv(t)
	e.loadAll(t)
	csv := e.find(t, "q4_budget.csv")

	// good request: opener is invoked with the validated path
	rec, out := e.post("/api/files/open", csv.ID)
	if rec.Code != 200 || out["ok"] != true || len(*e.calls) != 1 || (*e.calls)[0].name != "xdg-open" {
		t.Fatalf("open: %d %s calls=%v", rec.Code, rec.Body.String(), *e.calls)
	}
	real, _ := filepath.EvalSymlinks(csv.Path)
	if (*e.calls)[0].args[0] != real {
		t.Fatalf("opened %q, want %q", (*e.calls)[0].args[0], real)
	}
	rec, _ = e.post("/api/files/reveal", csv.ID)
	if rec.Code != 200 || len(*e.calls) != 2 {
		t.Fatalf("reveal: %d calls=%v", rec.Code, *e.calls)
	}

	n := len(*e.calls)
	deny := func(label string, want int, mut func(*http.Request), path, body string) {
		t.Helper()
		rec, _ := e.do("POST", path, body, mut)
		if rec.Code != want {
			t.Errorf("%s: status %d, want %d (%s)", label, rec.Code, want, rec.Body.String())
		}
	}
	body := `{"id":"` + csv.ID + `"}`
	for _, path := range []string{"/api/files/open", "/api/files/reveal"} {
		deny("no csrf header", 403, nil, path, body)
		deny("wrong csrf", 403, func(r *http.Request) { r.Header.Set(csrfHeader, "nope") }, path, body)
		deny("remote client", 403, func(r *http.Request) { r.Header.Set(csrfHeader, e.csrf); r.RemoteAddr = "192.168.1.20:4000" }, path, body)
		deny("foreign host header", 403, func(r *http.Request) { r.Header.Set(csrfHeader, e.csrf); r.Host = "evil.example.com" }, path, body)
		deny("lan host header", 403, func(r *http.Request) { r.Header.Set(csrfHeader, e.csrf); r.Host = "192.168.1.5:8080" }, path, body)
		deny("foreign origin", 403, func(r *http.Request) {
			r.Header.Set(csrfHeader, e.csrf)
			r.Header.Set("Origin", "https://evil.example.com")
		}, path, body)
		deny("cross-site fetch metadata", 403, func(r *http.Request) { r.Header.Set(csrfHeader, e.csrf); r.Header.Set("Sec-Fetch-Site", "cross-site") }, path, body)
		deny("form content type", 415, func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }, path, "id="+csv.ID)
		deny("path instead of id", 400, func(r *http.Request) { r.Header.Set(csrfHeader, e.csrf) }, path, `{"id":"/etc/passwd"}`)
		deny("traversal id", 400, func(r *http.Request) { r.Header.Set(csrfHeader, e.csrf) }, path, `{"id":"../../etc/passwd"}`)
		deny("unknown id", 404, func(r *http.Request) { r.Header.Set(csrfHeader, e.csrf) }, path, `{"id":"0123456789abcdef0123"}`)
	}
	// ALLOW_ANY_HOST must not weaken the local-only rule
	t.Setenv("ALLOW_ANY_HOST", "1")
	deny("ALLOW_ANY_HOST + foreign host", 403, func(r *http.Request) { r.Header.Set(csrfHeader, e.csrf); r.Host = "feishu-topics.local" }, "/api/files/open", body)
	if len(*e.calls) != n {
		t.Fatalf("a rejected request launched a program: %v", (*e.calls)[n:])
	}
	// the token comes from a same-origin GET
	_, tok := e.do("GET", "/api/csrf", "", nil)
	if tok["token"] != e.csrf {
		t.Fatal("csrf endpoint")
	}
	// a file that vanished cannot be opened
	os.Remove(csv.Path)
	if rec, _ := e.post("/api/files/open", csv.ID); rec.Code != 409 {
		t.Errorf("missing file: %d", rec.Code)
	}
	// an index entry whose file is a symlink to the outside is refused
	if runtime.GOOS != "windows" {
		secret := filepath.Join(t.TempDir(), "secret.txt")
		os.WriteFile(secret, []byte("x"), 0o644)
		bad := e.find(t, "ios17-whitescreen.log")
		os.Remove(bad.Path)
		if err := os.Symlink(secret, bad.Path); err != nil {
			t.Fatal(err)
		}
		// size of the symlink target equals the recorded size only by chance; force a match
		e.svc.dl.mu.Lock()
		e.svc.dl.entries[bad.Key].Size = 1
		e.svc.dl.mu.Unlock()
		if rec, _ := e.post("/api/files/open", bad.ID); rec.Code != 403 {
			t.Errorf("symlink escape: %d %s", rec.Code, rec.Body.String())
		}
		if len(*e.calls) != n {
			t.Fatal("symlink escape launched a program")
		}
	}
}

func TestFileOpenDisabledInDocker(t *testing.T) {
	e := newDLEnv(t)
	e.loadAll(t)
	e.srv.noOpen = true
	csv := e.find(t, "q4_budget.csv")
	rec, _ := e.post("/api/files/open", csv.ID)
	if rec.Code != 501 || len(*e.calls) != 0 {
		t.Fatalf("docker: %d calls=%v", rec.Code, *e.calls)
	}
	_, out := e.do("GET", "/api/downloads", "", nil)
	if out["opener"] != false {
		t.Fatal("opener must be reported as unavailable")
	}
}

func TestFileRedownloadEndpoint(t *testing.T) {
	e := newDLEnv(t)
	e.loadAll(t)
	csv := e.find(t, "q4_budget.csv")
	if rec, _ := e.do("POST", "/api/files/redownload", `{"id":"`+csv.ID+`"}`, nil); rec.Code != 403 {
		t.Fatalf("redownload without csrf: %d", rec.Code)
	}
	os.WriteFile(csv.Path, []byte("edited"), 0o644)
	n := e.api.dl.Load()
	rec, _ := e.post("/api/files/redownload", csv.ID)
	if rec.Code != 200 {
		t.Fatalf("redownload: %d %s", rec.Code, rec.Body.String())
	}
	waitEntryByID(t, e.svc.dl, csv.ID)
	if b, _ := os.ReadFile(csv.Path); !bytes.Contains(b, []byte("研发,320,210")) || e.api.dl.Load() != n+1 {
		t.Fatalf("not fetched again: %q calls %d->%d", b, n, e.api.dl.Load())
	}
}

func waitEntryByID(t *testing.T, dm *DownloadManager, id string) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if e, _ := dm.Get(id); e.Status == dlDone || e.Status == dlFailed {
			dm.mu.Lock()
			fl := dm.flights[e.Key]
			dm.mu.Unlock()
			if fl == nil {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("download did not finish")
}

func TestDownloadDisabledFallsBackToCache(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.DataDir = dir
	cfg.DownloadDir = filepath.Join(dir, "dl")
	cfg.DisableDownload = true
	cfg.Normalize()
	auth := NewAuth(cfg, true, filepath.Join(dir, "token.json"))
	_ = auth.Callback(context.Background(), "mock")
	svc := NewService(NewMockAPI(auth), NewCache(filepath.Join(dir, "cache")), cfg, auth)
	srv := NewServer(svc, nil, cfg, true, nil)
	if svc.dl != nil {
		t.Fatal("manager created although disabled")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/resource?message_id=om_mock_b04&file_key=file_mock_csv&type=file", nil)
	req.Host = "localhost:8080"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("legacy resource path: %d", rec.Code)
	}
}

func TestDuplicateMaterialDedupeAndSortOrder(t *testing.T) {
	ff := newFakeFetch(t)
	ff.data["key1"] = []byte("same")
	ff.data["key2"] = []byte("same")
	dm, base := newTestDM(t, ff)
	a := att("file", "report.pdf", "om1", "key1")
	b := att("file", "report.pdf", "om2", "key2")
	b.Time = a.Time + 1000
	dm.Register(tc1, []Attachment{a, b})
	v1 := waitEntry(t, dm, a)
	v2 := waitEntry(t, dm, b)
	if v1.Rel == v2.Rel || len(dm.List(tc1.TopicKey)) != 2 {
		t.Fatal("distinct resources must not merge by filename or bytes")
	}
	dm2 := NewDownloadManager(base, "", dm.idxPath, ff.fetch)
	if len(dm2.List(tc1.TopicKey)) != 2 {
		t.Fatal("restart removed distinct resources")
	}
	a2 := a
	a2.MessageID = "om3"
	a2.Time += 2000
	dm.Register(tc1, []Attachment{a, b, a2})
	waitEntry(t, dm, a2)
	if len(dm.List(tc1.TopicKey)) != 2 || ff.calls.Load() != 2 {
		t.Fatal("same resource repeated in a topic should reuse the download")
	}
}

func TestTopicFileUpdatedByNewerMessage(t *testing.T) {
	ff := newFakeFetch(t)
	ff.data["fk_v1"] = []byte("version 1 content of report")
	ff.names["fk_v1"] = "财务报表_2026.xlsx"
	ff.data["fk_v2"] = []byte("version 2 (updated and modified) content of report")
	ff.names["fk_v2"] = "财务报表_2026.xlsx"

	dm, base := newTestDM(t, ff)

	a1 := Attachment{
		ID: "file:om_1:fk_v1", Kind: "file", Name: "财务报表_2026.xlsx",
		MessageID: "om_1", FileKey: "fk_v1", ResType: "file", Time: 1760000010000,
	}
	a2 := Attachment{
		ID: "file:om_2:fk_v2", Kind: "file", Name: "财务报表_2026.xlsx",
		MessageID: "om_2", FileKey: "fk_v2", ResType: "file", Time: 1760000020000,
	}

	// First message introduces the file
	dm.RegisterLatest(tc1, []Attachment{a1}, []Attachment{a1})
	v1 := waitEntry(t, dm, a1)
	if v1.Status != dlDone {
		t.Fatalf("v1 status %s", v1.Status)
	}
	b1, err := os.ReadFile(v1.Path)
	if err != nil || string(b1) != "version 1 content of report" {
		t.Fatalf("v1 content mismatch: %v %q", err, b1)
	}

	// Newer message updates the file in the conversation
	dm.RegisterLatest(tc1, []Attachment{a2}, []Attachment{a1, a2})
	v2 := waitEntry(t, dm, a2)
	if v2.Status != dlDone {
		t.Fatalf("v2 status %s", v2.Status)
	}

	// Different remote resource keys with the same name must keep separate paths.
	if v2.Rel == v1.Rel {
		t.Fatalf("different resource keys must keep separate paths: %q", v1.Rel)
	}
	if !strings.Contains(v2.Name, "(2)") {
		t.Fatalf("same-name resources should have a collision suffix: %s", v2.Name)
	}

	// Content on disk must be updated to version 2
	b2, err := os.ReadFile(v2.Path)
	if err != nil || string(b2) != "version 2 (updated and modified) content of report" {
		t.Fatalf("v2 content not updated on disk: %v %q", err, b2)
	}

	// Both independent remote resources must remain on disk.
	topicDir := filepath.Dir(v1.Path)
	entries, _ := os.ReadDir(topicDir)
	if len(entries) != 2 {
		t.Fatalf("expected both versions in topic dir, found %d: %v", len(entries), entries)
	}
	_ = base
}

func TestTopicCloudDriveFileUpdate(t *testing.T) {
	e := newDLEnv(t)
	svc := e.svc
	api := e.api.MockAPI
	ctx := context.Background()

	// Initial load: omt_mock_002 contains drive link boxmockseed01 (发版checklist.txt)
	tr1, err := svc.Thread(ctx, "omt_mock_002", false)
	if err != nil {
		t.Fatal(err)
	}
	var drvEntry *DLEntry
	for _, it := range tr1.Downloads {
		if it.FileKey == "boxmockseed01" {
			e := it
			drvEntry = &e
			break
		}
	}
	if drvEntry == nil {
		t.Fatalf("drive file not found in thread downloads: dl=%+v atts=%+v", tr1.Downloads, tr1.Attachments)
	}

	if err := svc.dl.Redownload(drvEntry.ID); err != nil {
		t.Fatal(err)
	}
	waitEntryByID(t, svc.dl, drvEntry.ID)
	v1, ok := svc.dl.Get(drvEntry.ID)
	if !ok || v1.Status != dlDone {
		t.Fatalf("initial drive download not done: %+v", v1)
	}

	// Update the file in Feishu Drive (cloud update)
	updatedContent := []byte("2026-10-15 checklist v2.0 - all tasks verified in cloud")
	newModTime := time.Now().Add(2 * time.Hour).UnixMilli()
	api.SetDriveFile("boxmockseed01", "发版checklist.txt", updatedContent, newModTime)

	// User views / polls the thread
	tr2, err := svc.Thread(ctx, "omt_mock_002", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr2.Downloads) == 0 {
		t.Fatal("no downloads in tr2")
	}

	if err := svc.dl.Redownload(drvEntry.ID); err != nil {
		t.Fatal(err)
	}
	waitEntryByID(t, svc.dl, drvEntry.ID)
	v2, _ := svc.dl.Get(drvEntry.ID)
	if v2.Status != dlDone {
		t.Fatalf("updated drive download not done: %+v", v2)
	}

	b, err := os.ReadFile(v2.Path)
	if err != nil || !bytes.Equal(b, updatedContent) {
		t.Fatalf("cloud file update was not reflected on disk: %v %q", err, b)
	}
}

func TestTopicScopedResourceIdentity(t *testing.T) {
	ff := newFakeFetch(t)
	dm, _ := newTestDM(t, ff)
	a := att("file", "same.txt", "om_shared", "samekey")
	first := dm.Register(tc1, []Attachment{a})
	other := tc1
	other.TopicKey = "om_another"
	second := dm.Register(other, []Attachment{a})
	for _, v := range append(first, second...) {
		if err := dm.waitFile(context.Background(), v.ID); err != nil {
			t.Fatal(err)
		}
	}
	x, y := dm.List(tc1.TopicKey), dm.List(other.TopicKey)
	if len(x) != 1 || len(y) != 1 || x[0].Rel == y[0].Rel {
		t.Fatalf("cross-topic files collided: %+v %+v", x, y)
	}
}
