package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const workspaceTestChat = "oc_mock_dev"

func workspaceHTTP(s *Server, method, path string, body any, csrf, local bool) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, "http://localhost"+path, bytes.NewReader(b))
	if local {
		req.RemoteAddr = "127.0.0.1:10001"
	}
	req.Header.Set("Content-Type", "application/json")
	if csrf {
		req.Header.Set(csrfHeader, s.csrf)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func workspaceJSON[T any](t *testing.T, w *httptest.ResponseRecorder, status int) T {
	t.Helper()
	var out T
	if w.Code != status {
		t.Fatalf("HTTP %d, expected %d: %s", w.Code, status, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid response: %v (%s)", err, w.Body)
	}
	return out
}

func workspaceBind(t *testing.T, s *Server, chatID string, ws ChatWorkspace) ChatWorkspace {
	t.Helper()
	return workspaceJSON[ChatWorkspace](t, workspaceHTTP(s, "POST", "/api/workspace", map[string]any{
		"chat_id": chatID, "items": ws.Items, "attach_context": ws.AttachContext,
	}, true, true), http.StatusOK)
}

func workspaceTestFiles(t *testing.T, s *Server, svc *Service) string {
	t.Helper()
	root := t.TempDir()
	s.cfg.DownloadDir = root
	s.svc.dl = NewDownloadManager(root, "", filepath.Join(s.cfg.DataDir, "workspace-test-downloads.json"), s.dlFetch)
	return root
}

func TestWorkspaceConfiguration(t *testing.T) {
	s, svc := newTestServer(t)
	s.docker = false
	root := t.TempDir()
	got := workspaceBind(t, s, workspaceTestChat, ChatWorkspace{AttachContext: true, Items: []WorkspaceItem{
		{Name: "本地", Kind: workspaceLocal, LocalPath: root},
		{URL: "https://mock.feishu.cn/wiki/mockWikiHome"},
		{URL: "https://mock.feishu.cn/drive/folder/mockWorkspaceFolder"},
	}})
	if !got.AttachContext || len(got.Items) != 3 || got.Items[1].SpaceID != "mockWikiSpace" ||
		got.Items[1].Token != "mockWikiHome" || got.Items[2].Kind != workspaceDrive {
		t.Fatalf("unexpected bindings: %+v", got)
	}
	for _, item := range got.Items {
		if item.ID == "" || item.CreatedAt == 0 {
			t.Fatalf("missing generated identity: %+v", item)
		}
	}
	other := workspaceJSON[ChatWorkspace](t, workspaceHTTP(s, "GET", "/api/workspace?chat_id=oc_mock_proj", nil, false, true), 200)
	if len(other.Items) != 0 || other.AttachContext {
		t.Fatalf("settings leaked across chats: %+v", other)
	}
	remote := workspaceJSON[ChatWorkspace](t, workspaceHTTP(s, "GET", "/api/workspace?chat_id="+workspaceTestChat, nil, false, false), 200)
	if remote.Items[0].LocalPath != "" {
		t.Fatalf("remote response exposed local path: %+v", remote)
	}
	svc.auth.mu.Lock()
	svc.auth.tok.User.OpenID = "ou_workspace_other"
	svc.auth.mu.Unlock()
	other = workspaceJSON[ChatWorkspace](t, workspaceHTTP(s, "GET", "/api/workspace?chat_id="+workspaceTestChat, nil, false, true), 200)
	if len(other.Items) != 0 {
		t.Fatalf("settings leaked across users: %+v", other)
	}
	reopened := NewService(svc.api, svc.cache, svc.cfg, svc.auth)
	defer reopened.Close()
	persisted, err := reopened.chatWorkspace(uLi, workspaceTestChat)
	if err != nil || len(persisted.Items) != 3 || persisted.Items[0].ID != got.Items[0].ID {
		t.Fatalf("not persisted: %+v, %v", persisted, err)
	}
	if st, err := os.Stat(svc.workspaceSettingsPath()); err != nil || st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("settings mode: %v, %v", st, err)
	}
}

func TestWorkspaceBrowseAndLocalGuards(t *testing.T) {
	s, svc := newTestServer(t)
	s.docker = false
	dir := workspaceTestFiles(t, s, svc)
	local := filepath.Join(dir, "child")
	outside := t.TempDir()
	if err := os.Mkdir(local, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "note.txt"), []byte("workspace note"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	hasSymlinks := os.Symlink(outside, filepath.Join(local, "escape")) == nil
	ws := workspaceBind(t, s, workspaceTestChat, ChatWorkspace{Items: []WorkspaceItem{{Kind: workspaceLocal, LocalPath: local}}})
	item := ws.Items[0]
	browse := workspaceJSON[struct {
		Dir         string
		Breadcrumbs []struct{ Name, Path string }
		Items       []struct{ Name, Path string }
	}](t, workspaceHTTP(s, "GET", "/api/workspace/local/browse?dir="+url.QueryEscape(dir), nil, true, true), 200)
	if len(browse.Items) != 1 || browse.Items[0].Name != "child" || len(browse.Breadcrumbs) == 0 {
		t.Fatalf("browse: %+v", browse)
	}
	tree := "/api/workspace/tree?chat_id=" + workspaceTestChat + "&item_id=" + item.ID
	page := workspaceJSON[workspaceTreePage](t, workspaceHTTP(s, "GET", tree, nil, true, true), 200)
	if len(page.Items) != 1 || page.Items[0].RelPath != "note.txt" {
		t.Fatalf("tree should hide paths outside the root: %+v", page)
	}
	stageReq := workspaceFileRequest{ChatID: workspaceTestChat, ItemID: item.ID, RelPath: "note.txt"}
	stage := workspaceJSON[Staged](t, workspaceHTTP(s, "POST", "/api/workspace/stage", stageReq, true, true), 200)
	if stage.Name != "note.txt" || stage.Size != 14 || stage.ID == "" {
		t.Fatalf("staged attachment: %+v", stage)
	}
	_, stagedBytes, err := svc.loadStaged(stage.ID)
	if err != nil || string(stagedBytes) != "workspace note" {
		t.Fatalf("staged bytes %q: %v", stagedBytes, err)
	}
	s.opener.HostURL = ""
	s.opener.GOOS = "darwin"
	var revealed string
	s.opener.Run = func(_ string, args []string, _ time.Duration) error { revealed = args[len(args)-1]; return nil }
	s.noOpen = false
	workspaceJSON[map[string]bool](t, workspaceHTTP(s, "POST", "/api/workspace/reveal", stageReq, true, true), 200)
	if revealed != filepath.Join(item.LocalPath, "note.txt") {
		t.Fatalf("reveal path: %q", revealed)
	}
	for _, path := range []string{"../", outside} {
		workspaceJSON[apiError](t, workspaceHTTP(s, "GET", tree+"&node="+url.QueryEscape(path), nil, true, true), 403)
	}
	if hasSymlinks {
		workspaceJSON[apiError](t, workspaceHTTP(s, "GET", tree+"&node=escape", nil, true, true), 403)
		escape := stageReq
		escape.RelPath = "escape/secret.txt"
		workspaceJSON[apiError](t, workspaceHTTP(s, "POST", "/api/workspace/stage", escape, true, true), 403)
		workspaceJSON[apiError](t, workspaceHTTP(s, "POST", "/api/workspace/preview", escape, true, true), 403)
	}
	transfer := workspaceTransferRequest{ChatID: workspaceTestChat, ItemID: item.ID, Subpath: "../", Source: workspaceSource{Kind: "im"}}
	workspaceJSON[apiError](t, workspaceHTTP(s, "POST", "/api/workspace/transfer", transfer, true, true), 403)
	workspaceJSON[apiError](t, workspaceHTTP(s, "POST", "/api/workspace", map[string]any{
		"chat_id": workspaceTestChat, "items": []WorkspaceItem{{Kind: workspaceLocal, LocalPath: filepath.Join(local, "note.txt")}},
	}, true, true), 400)
	for _, req := range []struct {
		method, path string
		body         any
	}{
		{"GET", tree, nil}, {"GET", "/api/workspace/local/browse", nil},
		{"POST", "/api/workspace/local/pick", nil}, {"POST", "/api/workspace/stage", stageReq},
		{"POST", "/api/workspace/reveal", stageReq}, {"POST", "/api/workspace/preview", stageReq},
		{"POST", "/api/workspace/sync_local", stageReq}, {"POST", "/api/workspace/transfer", transfer},
	} {
		workspaceJSON[apiError](t, workspaceHTTP(s, req.method, req.path, req.body, true, false), 403)
		workspaceJSON[apiError](t, workspaceHTTP(s, req.method, req.path, req.body, false, true), 403)
	}
	s.docker = true
	pick := workspaceJSON[map[string]any](t, workspaceHTTP(s, "POST", "/api/workspace/local/pick", map[string]any{}, true, true), 200)
	if pick["fallback"] != true || pick["reason"] == "" {
		t.Fatalf("Docker picker fallback: %+v", pick)
	}
	for _, path := range []string{"/api/workspace", "/api/workspace/transfer", "/api/workspace/preview"} {
		workspaceJSON[apiError](t, workspaceHTTP(s, "POST", path, map[string]any{}, false, true), 403)
	}
	workspaceJSON[apiError](t, workspaceHTTP(s, "DELETE", "/api/workspace/preview_cache", nil, false, true), 403)
	svc.auth.Logout()
	workspaceJSON[apiError](t, workspaceHTTP(s, "GET", "/api/workspace?chat_id="+workspaceTestChat, nil, true, true), 401)
	workspaceJSON[apiError](t, workspaceHTTP(s, "POST", "/api/workspace/transfer", transfer, true, true), 401)
}

type workspaceTestAPI struct {
	API
	calls      []string
	downloads  atomic.Int32
	failDelete bool
}

func (a *workspaceTestAPI) Download(ctx context.Context, messageID, fileKey, kind string) (*Download, error) {
	a.downloads.Add(1)
	return a.API.Download(ctx, messageID, fileKey, kind)
}
func (a *workspaceTestAPI) DriveEnsurePrivateSubfolder(ctx context.Context, name string) (string, error) {
	a.calls = append(a.calls, "folder:"+name)
	return a.API.DriveEnsurePrivateSubfolder(ctx, name)
}
func (a *workspaceTestAPI) DriveUploadToFolder(ctx context.Context, folder, name string, r io.Reader, size int64) (string, error) {
	a.calls = append(a.calls, "upload:"+folder)
	return a.API.DriveUploadToFolder(ctx, folder, name, r, size)
}
func (a *workspaceTestAPI) DriveCopyFile(ctx context.Context, token, kind, folder, name string) (*DriveResource, error) {
	a.calls = append(a.calls, "copy:"+token+":"+folder)
	return a.API.DriveCopyFile(ctx, token, kind, folder, name)
}
func (a *workspaceTestAPI) WikiMoveDocToSpace(ctx context.Context, space, parent, kind, token string) (*WikiMoveResult, error) {
	a.calls = append(a.calls, "move:"+space+":"+parent+":"+kind)
	return a.API.WikiMoveDocToSpace(ctx, space, parent, kind, token)
}
func (a *workspaceTestAPI) DriveDeleteFile(ctx context.Context, token, kind string) error {
	a.calls = append(a.calls, "delete:"+token)
	if a.failDelete {
		return fmt.Errorf("模拟缓存删除失败")
	}
	return a.API.DriveDeleteFile(ctx, token, kind)
}

func TestWorkspaceTransferMatrix(t *testing.T) {
	s, svc := newTestServer(t)
	spy := &workspaceTestAPI{API: svc.api}
	svc.api = spy
	workspaceTestFiles(t, s, svc)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "归档"), 0o700); err != nil {
		t.Fatal(err)
	}
	ws := workspaceBind(t, s, workspaceTestChat, ChatWorkspace{Items: []WorkspaceItem{
		{Kind: workspaceLocal, LocalPath: root},
		{URL: "https://mock.feishu.cn/drive/folder/mockWorkspaceFolder"},
		{Kind: workspaceWiki, SpaceID: "mockWikiSpace", Name: "知识库"},
	}})
	local, drive, wiki := ws.Items[0], ws.Items[1], ws.Items[2]
	entries := svc.dl.Catalog(TopicCtx{ChatID: workspaceTestChat, TopicKey: "topic"}, []Attachment{
		{Kind: "file", MessageID: "om_mock_a05", FileKey: "file_mock_log", ResType: "file", Name: "source.log"},
	})
	if len(entries) != 1 {
		t.Fatalf("download catalogue: %+v", entries)
	}
	_, saved, _, err := svc.dl.Ensure(context.Background(), "file", "om_mock_a05", "file_mock_log")
	if err != nil {
		t.Fatal(err)
	}
	localBytes := []byte("locally edited file")
	if err := os.WriteFile(saved, localBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	sources := []workspaceSource{
		{Kind: "im", MessageID: "om_mock_a05", FileKey: "file_mock_log", ResType: "file", Name: "message.log"},
		{Kind: "local_dl", DLID: entries[0].ID, Name: "download.log"},
		{Kind: "wiki", URL: "https://mock.feishu.cn/wiki/MockWikiRollback789", Name: "项目方案"},
	}
	localTransfer := workspaceTransferRequest{ChatID: workspaceTestChat, ItemID: local.ID, Subpath: "归档"}
	driveTransfer := workspaceTransferRequest{ChatID: workspaceTestChat, ItemID: drive.ID, Node: "mockWorkspaceReports", Path: []string{"mockWorkspaceReports"}}
	wikiTransfer := workspaceTransferRequest{ChatID: workspaceTestChat, ItemID: wiki.ID, Node: "mockWikiArchive"}
	for _, req := range []workspaceTransferRequest{localTransfer, driveTransfer, wikiTransfer} {
		for _, src := range sources {
			req.Source = src
			spy.calls = nil
			targetKind := map[string]string{local.ID: "local", drive.ID: "drive", wiki.ID: "wiki"}[req.ItemID]
			t.Run(targetKind+"/"+src.Kind, func(t *testing.T) {
				res := workspaceJSON[workspaceTransferResult](t, workspaceHTTP(s, "POST", "/api/workspace/transfer", req, true, true), 200)
				switch req.ItemID {
				case local.ID:
					b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(res.RelPath)))
					if err != nil {
						t.Fatal(err)
					}
					if src.Kind == "wiki" {
						assertWorkspaceOffice(t, res.Name, b, ".docx", "word/document.xml")
					} else if !bytes.Equal(b, localBytes) {
						t.Fatalf("expected existing local copy, got %q", b)
					}
				case drive.ID:
					d, err := spy.API.DriveList(context.Background(), req.Node, "")
					if err != nil || len(d.Files) < 2 {
						t.Fatalf("cloud folder not refreshed: %+v %v", d, err)
					}
					if len(spy.calls) != 1 || (src.Kind == "wiki") != strings.HasPrefix(spy.calls[0], "copy:") {
						t.Fatalf("cloud folder sequence: %v", spy.calls)
					}
				case wiki.ID:
					kind, action := "file", "upload:"
					if src.Kind == "wiki" {
						kind, action = "docx", "copy:docmockMockWikiRollback789:"
					}
					want := []string{"folder:" + workspaceStagingFolder, action + "mockPrivate" + workspaceStagingFolder, "move:mockWikiSpace:mockWikiArchive:" + kind}
					if strings.Join(spy.calls, "|") != strings.Join(want, "|") {
						t.Fatalf("expected copy/upload to staging before move, got %v", spy.calls)
					}
					node, err := spy.API.WikiNodeInfo(context.Background(), res.Token)
					if err != nil || node.ParentNodeToken != req.Node || node.ObjType != kind || node.ObjToken == "docmockMockWikiRollback789" {
						t.Fatalf("moved node: %+v, %v", node, err)
					}
				}
			})
		}
	}
	if calls := spy.downloads.Load(); calls != 1 {
		t.Fatalf("the three IM and local transfers should reuse the saved copy, %d downloads", calls)
	}
	localTransfer.Source = sources[0]
	dup := workspaceJSON[workspaceTransferResult](t, workspaceHTTP(s, "POST", "/api/workspace/transfer", localTransfer, true, true), 200)
	if dup.Name != "message (1).log" {
		t.Fatalf("non-clobbering rename: %+v", dup)
	}
	driveTransfer.Node = "outsideCloudFolder"
	driveTransfer.Path = nil
	driveTransfer.Source = sources[0]
	workspaceJSON[apiError](t, workspaceHTTP(s, "POST", "/api/workspace/transfer", driveTransfer, true, true), 403)
	wikiTransfer.ItemID = workspaceBind(t, s, "oc_mock_proj", ChatWorkspace{Items: []WorkspaceItem{
		{URL: "https://mock.feishu.cn/wiki/mockWikiArchive"},
	}}).Items[0].ID
	wikiTransfer.ChatID, wikiTransfer.Node, wikiTransfer.Source = "oc_mock_proj", "mockWikiHome", sources[2]
	workspaceJSON[apiError](t, workspaceHTTP(s, "POST", "/api/workspace/transfer", wikiTransfer, true, true), 403)
	localTransfer.Source = workspaceSource{Kind: "im", MessageID: "om_mock_b04", FileKey: "file_mock_csv", Name: "预算.csv"}
	fresh := workspaceJSON[workspaceTransferResult](t, workspaceHTTP(s, "POST", "/api/workspace/transfer", localTransfer, true, true), 200)
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(fresh.RelPath)))
	if err != nil || !bytes.Contains(b, []byte("预算")) || spy.downloads.Load() != 2 {
		t.Fatalf("uncached IM transfer: %q %v", b, err)
	}
	localTransfer.Source = workspaceSource{Kind: "drive", ResourceType: "file", ResourceToken: "boxmockseed01"}
	res := workspaceJSON[workspaceTransferResult](t, workspaceHTTP(s, "POST", "/api/workspace/transfer", localTransfer, true, true), 200)
	b, err = os.ReadFile(filepath.Join(root, filepath.FromSlash(res.RelPath)))
	if err != nil || !bytes.Contains(b, []byte("boxmockseed01")) {
		t.Fatalf("Drive download: %q %v", b, err)
	}
}

func assertWorkspaceOffice(t *testing.T, name string, data []byte, ext, part string) {
	t.Helper()
	if filepath.Ext(name) != ext {
		t.Fatalf("unexpected Office extension: %s", name)
	}
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	f, err := z.Open(part)
	if err != nil {
		t.Fatalf("missing %s: %v", part, err)
	}
	defer f.Close()
	content, err := io.ReadAll(f)
	if err != nil || !bytes.HasPrefix(content, []byte("<")) {
		t.Fatalf("invalid office XML: %q %v", content, err)
	}
}

func TestWorkspaceOfficeExportTypes(t *testing.T) {
	s, _ := newTestServer(t)
	ws := workspaceBind(t, s, workspaceTestChat, ChatWorkspace{Items: []WorkspaceItem{{Kind: workspaceLocal, LocalPath: t.TempDir()}}})
	item := ws.Items[0]
	for _, typ := range []struct{ typeName, ext, part string }{
		{"doc", ".docx", "word/document.xml"}, {"docx", ".docx", "word/document.xml"},
		{"sheet", ".xlsx", "xl/workbook.xml"}, {"bitable", ".xlsx", "xl/workbook.xml"}, {"slides", ".pptx", "ppt/presentation.xml"},
	} {
		src := workspaceSource{Kind: "doc", ResourceType: typ.typeName, ResourceToken: "documentToken", Name: typ.typeName}
		req := workspaceTransferRequest{ChatID: workspaceTestChat, ItemID: item.ID, Source: src}
		res := workspaceJSON[workspaceTransferResult](t, workspaceHTTP(s, "POST", "/api/workspace/transfer", req, true, true), 200)
		data, err := os.ReadFile(filepath.Join(item.LocalPath, res.RelPath))
		if err != nil {
			t.Fatal(err)
		}
		assertWorkspaceOffice(t, res.Name, data, typ.ext, typ.part)
	}
	for _, typ := range []string{"mindnote", "minutes"} {
		req := workspaceTransferRequest{ChatID: workspaceTestChat, ItemID: item.ID, Source: workspaceSource{Kind: "doc", ResourceType: typ, ResourceToken: "unsupported"}}
		e := workspaceJSON[apiError](t, workspaceHTTP(s, "POST", "/api/workspace/transfer", req, true, true), 400)
		if !strings.Contains(e.Error, "Office") {
			t.Fatalf("missing unsupported-export message: %+v", e)
		}
	}
}

func TestWorkspacePrivatePreviewCache(t *testing.T) {
	s, svc := newTestServer(t)
	spy := &workspaceTestAPI{API: svc.api}
	svc.api = spy
	im := workspaceSource{Kind: "im", MessageID: "om_mock_a05", FileKey: "file_mock_log", ResType: "file", Name: "msg.log"}
	const preview = "/api/workspace/preview"
	var parallel sync.WaitGroup
	batch := make([]*httptest.ResponseRecorder, 5)
	for i := range batch {
		parallel.Add(1)
		go func() {
			defer parallel.Done()
			batch[i] = workspaceHTTP(s, "POST", preview, im, true, true)
		}()
	}
	parallel.Wait()
	var sharedURL string
	misses := 0
	for _, w := range batch {
		r := workspaceJSON[workspacePreviewEntry](t, w, 200)
		if sharedURL != "" && r.URL != sharedURL {
			t.Fatalf("parallel preview link mismatch: %s vs %s", r.URL, sharedURL)
		}
		sharedURL = r.URL
		if !r.Cached {
			misses++
		}
	}
	if misses != 1 || spy.downloads.Load() != 1 {
		t.Fatalf("preview was not idempotent: %d misses, %d fetches", misses, spy.downloads.Load())
	}
	ref, _ := driveRef(sharedURL)
	perms, err := spy.DrivePermissions(context.Background(), ref.Token, "file")
	if err != nil || verifyPrivatePermissions(perms, uLi, "", "") != nil || len(perms.Members) != 0 {
		t.Fatalf("preview not private: %+v %v", perms, err)
	}
	cloud := workspaceSource{Kind: "wiki", URL: "https://mock.feishu.cn/wiki/mockWikiHome?x=1"}
	r := workspaceJSON[workspacePreviewEntry](t, workspaceHTTP(s, "POST", preview, cloud, true, true), 200)
	if r.URL != cloud.URL || r.Cached {
		t.Fatalf("cloud direct preview: %+v", r)
	}
	r = workspaceJSON[workspacePreviewEntry](t, workspaceHTTP(s, "POST", preview, workspaceSource{Kind: "wiki", ResourceToken: "mockWikiHome"}, true, true), 200)
	if !strings.HasSuffix(r.URL, "/wiki/mockWikiHome") {
		t.Fatalf("wiki lookup preview: %+v", r)
	}

	root := t.TempDir()
	path := filepath.Join(root, "name.txt")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws := workspaceBind(t, s, workspaceTestChat, ChatWorkspace{Items: []WorkspaceItem{{Kind: workspaceLocal, LocalPath: root}}})
	req := workspaceFileRequest{ChatID: workspaceTestChat, ItemID: ws.Items[0].ID, RelPath: "name.txt"}
	a := workspaceJSON[workspacePreviewEntry](t, workspaceHTTP(s, "POST", preview, req, true, true), 200)
	b := workspaceJSON[workspacePreviewEntry](t, workspaceHTTP(s, "POST", preview, req, true, true), 200)
	if a.Cached || !b.Cached || a.URL != b.URL {
		t.Fatalf("local preview cache: %+v %+v", a, b)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	c := workspaceJSON[workspacePreviewEntry](t, workspaceHTTP(s, "POST", preview, req, true, true), 200)
	if c.URL == b.URL || c.Cached {
		t.Fatalf("same size and mtime with different content was cached: %+v", c)
	}
	oldRef, _ := driveRef(a.URL)
	retired := false
	for _, call := range spy.calls {
		if call == "delete:"+oldRef.Token {
			retired = true
		}
	}
	if !retired {
		t.Fatalf("previous local preview was not retired: %v", spy.calls)
	}

	idx := workspacePreviewIndex{}
	if err := readWorkspaceJSON(svc.workspacePreviewPath(), &idx); err != nil {
		t.Fatal(err)
	}
	idx["ou_other"] = map[string]workspacePreviewEntry{"key": {Token: "untouched", URL: "https://mock.feishu.cn/file/untouched"}}
	if err := writeWorkspaceJSON(svc.workspacePreviewPath(), idx); err != nil {
		t.Fatal(err)
	}
	cleared := workspaceJSON[map[string]int](t, workspaceHTTP(s, "DELETE", "/api/workspace/preview_cache", nil, true, true), 200)
	if cleared["deleted"] != 2 || cleared["failed"] != 0 {
		t.Fatalf("clear: %+v", cleared)
	}
	idx = workspacePreviewIndex{}
	if err := readWorkspaceJSON(svc.workspacePreviewPath(), &idx); err != nil || len(idx[uLi]) != 0 || len(idx["ou_other"]) != 1 {
		t.Fatalf("unexpected cache index: %+v %v", idx, err)
	}
	r = workspaceJSON[workspacePreviewEntry](t, workspaceHTTP(s, "POST", preview, im, true, true), 200)
	if r.Cached || r.URL == sharedURL {
		t.Fatalf("cleared preview reused: %+v", r)
	}
	spy.failDelete = true
	cleared = workspaceJSON[map[string]int](t, workspaceHTTP(s, "DELETE", "/api/workspace/preview_cache", nil, true, true), 200)
	if cleared["deleted"] != 0 || cleared["failed"] != 1 {
		t.Fatalf("best-effort cleanup: %+v", cleared)
	}
}

func TestWorkspaceLinksInCompose(t *testing.T) {
	const href = "https://mock.feishu.cn/wiki/mockWikiHome"
	result := assemble("整理资料\n\n群工作区\n- [项目知识库]("+href+")", nil, nil)
	if len(result) != 1 || result[0].Type != "post" || strings.Contains(result[0].Content, `"href"`) ||
		!strings.Contains(result[0].Content, `"tag":"code_block","text":"[项目知识库](`+href+`)"`) {
		t.Fatalf("workspace reference must preserve Feishu sharing permissions: %+v", result)
	}
	view := ParseContent(result[0].Type, result[0].Content, nil)
	if len(view.Attachments) != 1 || view.Attachments[0].URL != href || view.Attachments[0].Name != "项目知识库" {
		t.Fatalf("workspace reference must remain clickable in the app: %+v", view.Attachments)
	}
}

type workspaceSearchTestAPI struct {
	API
	noDocuments bool
	nodeLookups atomic.Int32
}

func (a *workspaceSearchTestAPI) DriveList(ctx context.Context, folder, page string) (*DriveListPage, error) {
	if folder == "mockDriveRoot" && page == "" {
		return &DriveListPage{HasMore: true, NextPage: "folders2"}, nil
	}
	return a.API.DriveList(ctx, folder, page)
}

func (a *workspaceSearchTestAPI) DriveSearchObjects(ctx context.Context, query string, kinds []string, offset, count int) (*DriveSearchPage, error) {
	if a.noDocuments {
		return nil, fmt.Errorf("搜索权限未开通")
	}
	return a.API.DriveSearchObjects(ctx, query, kinds, offset, count)
}

func (a *workspaceSearchTestAPI) WikiSearchNodes(ctx context.Context, query, spaceID, pageToken string) (*WikiSearchPage, error) {
	page, err := a.API.WikiSearchNodes(ctx, query, spaceID, pageToken)
	if err == nil {
		for i := range page.Items {
			page.Items[i].Title, page.Items[i].URL, page.Items[i].SpaceID = "", "", ""
		}
	}
	return page, err
}

func (a *workspaceSearchTestAPI) WikiNodeInfo(ctx context.Context, token string) (*WikiNodeDetail, error) {
	a.nodeLookups.Add(1)
	return a.API.WikiNodeInfo(ctx, token)
}

func TestWorkspaceCloudSearch(t *testing.T) {
	s, svc := newTestServer(t)
	mock := svc.api.(*MockAPI)
	mock.mu.Lock()
	mock.workspaceDrive = map[string][]DriveResource{
		"mockWorkspaceReports": {mockDriveFolder("mockBudgetHistory", "历史预算")},
	}
	mock.mu.Unlock()
	searchAPI := &workspaceSearchTestAPI{API: mock}
	svc.api = searchAPI
	search := "/api/workspace/cloud/search?kind="
	driveSearch := search + workspaceDrive + "&q=" + url.QueryEscape("预算")
	drive := workspaceJSON[workspaceCloudMatches](t, workspaceHTTP(s, "GET", driveSearch, nil, false, false), 200)
	if len(drive.Items) != 2 || drive.Warning != "" {
		t.Fatalf("drive results: %+v", drive)
	}
	folder, file := drive.Items[0], drive.Items[1]
	if !folder.CanBind || folder.Token != "mockBudgetHistory" || folder.URL == "" || folder.Hint != "我的空间 / 示例项目资料 / 归档" ||
		file.CanBind || file.Token != mockWorkspaceBudget.Token || file.URL != mockWorkspaceBudget.URL {
		t.Fatalf("folder and file search results: %+v", drive.Items)
	}
	searchAPI.noDocuments = true
	partial := workspaceJSON[workspaceCloudMatches](t, workspaceHTTP(s, "GET", driveSearch, nil, false, true), 200)
	if len(partial.Items) != 1 || partial.Items[0] != folder {
		t.Fatalf("document search failure dropped the folder result: %+v", partial)
	}

	wikiSearch := search + workspaceWiki + "&q=" + url.QueryEscape("项目")
	wiki := workspaceJSON[workspaceCloudMatches](t, workspaceHTTP(s, "GET", wikiSearch, nil, false, true), 200)
	if len(wiki.Items) != 2 || wiki.Warning != "" {
		t.Fatalf("wiki results: %+v", wiki)
	}
	space, node := wiki.Items[0], wiki.Items[1]
	if !space.CanBind || space.SpaceID != "mockWikiSpace" || space.Token != "" || space.URL == "" ||
		!node.CanBind || node.Token != "mockWikiHome" || node.SpaceID != space.SpaceID || node.Name != "项目首页" ||
		node.Hint != space.Name || node.URL != "https://mock.feishu.cn/wiki/mockWikiHome" || searchAPI.nodeLookups.Load() != 1 {
		t.Fatalf("space and hydrated wiki node: %+v (lookups: %d)", wiki.Items, searchAPI.nodeLookups.Load())
	}
	ws := workspaceBind(t, s, workspaceTestChat, ChatWorkspace{Items: []WorkspaceItem{
		{Kind: folder.Kind, Token: folder.Token, Name: folder.Name, URL: folder.URL},
		{Kind: space.Kind, SpaceID: space.SpaceID, Name: space.Name, URL: space.URL},
		{Kind: node.Kind, Token: node.Token, SpaceID: node.SpaceID, Name: node.Name, URL: node.URL},
		{URL: "https://example.feishu.cn/wiki/mockWikiHome"},
	}})
	if len(ws.Items) != 3 {
		t.Fatalf("search results should bind independently and only once: %+v", ws.Items)
	}
	workspaceJSON[apiError](t, workspaceHTTP(s, "GET", search+"unknown&q=x", nil, false, true), 400)
	svc.auth.Logout()
	workspaceJSON[apiError](t, workspaceHTTP(s, "GET", driveSearch, nil, true, true), 401)
}

func TestWorkspaceFeishuSearchAPI(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer u-access" {
			t.Errorf("expected user-authorized POST: %s %s", r.Method, r.URL)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		switch r.URL.Path {
		case "/open-apis/suite/docs-api/search/object":
			if body["search_key"] != "预算" || body["offset"] != float64(10) || body["count"] != float64(5) ||
				fmt.Sprint(body["docs_types"]) != "[doc file]" {
				t.Errorf("Drive search body: %+v", body)
			}
			fmt.Fprint(w, `{"code":0,"data":{"docs_entities":[{"docs_token":"doc1","docs_type":"docx","title":"项目预算","owner_id":"ou_creator"}],"has_more":true,"total":20}}`)
		case "/open-apis/wiki/v1/nodes/search":
			if body["query"] != "项目" || body["space_id"] != "space1" ||
				r.URL.Query().Get("page_size") != "50" || r.URL.Query().Get("page_token") != "page1" {
				t.Errorf("Wiki search request: query %s, body %+v", r.URL.RawQuery, body)
			}
			fmt.Fprint(w, `{"code":0,"data":{"items":[{"node_id":"node1","space_id":"space1","title":"项目首页","url":"https://demo.feishu.cn/wiki/node1","obj_type":8}],"has_more":true,"page_token":"page2"}}`)
		default:
			t.Errorf("unexpected API: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	cfg := realConfig(ts.URL)
	auth := loggedInAuth(t, cfg, "u-access", "refresh", time.Hour)
	client := NewFeishuClient(ts.URL, cfg.AppID, cfg.AppSecret, auth, "user")
	client.gap = 0
	ctx := context.WithValue(context.Background(), workspaceUserKey{}, "ou_me")
	drive, err := client.DriveSearchObjects(ctx, "预算", []string{"doc", "file"}, 10, 5)
	wantDoc := DriveResource{Token: "doc1", Type: "docx", Name: "项目预算", OwnerID: "ou_creator"}
	if err != nil || !drive.HasMore || drive.Total != 20 || len(drive.Items) != 1 || drive.Items[0] != wantDoc {
		t.Fatalf("parsed Drive results: %+v %v", drive, err)
	}
	wiki, err := client.WikiSearchNodes(ctx, "项目", "space1", "page1")
	wantNode := WikiSearchNode{NodeToken: "node1", SpaceID: "space1", Title: "项目首页", URL: "https://demo.feishu.cn/wiki/node1"}
	if err != nil || !wiki.HasMore || wiki.PageToken != "page2" || len(wiki.Items) != 1 || wiki.Items[0] != wantNode {
		t.Fatalf("parsed Wiki results: %+v %v", wiki, err)
	}
}

func TestWorkspaceFeishuAPI(t *testing.T) {
	var polls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer u-access" {
			t.Errorf("%s auth: %s", r.URL.Path, got)
		}
		if r.URL.Path == "/open-apis/wiki/v2/spaces/space1/nodes/move_docs_to_wiki" ||
			r.URL.Path == "/open-apis/drive/v1/files/doc1/copy" {
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			switch {
			case payload["obj_token"] == "doc1" && payload["obj_type"] == "docx" && payload["parent_wiki_token"] == "node1" && payload["apply"] == false:
				fmt.Fprint(w, `{"code":0,"data":{"task_id":"task1"}}`)
			case payload["type"] == "docx" && payload["name"] == "副本" && payload["folder_token"] == "target":
				fmt.Fprint(w, `{"code":0,"data":{"file":{"token":"copy1","type":"docx","name":"副本"}}}`)
			default:
				t.Errorf("invalid Feishu payload: %+v", payload)
			}
			return
		}
		q := r.URL.Query()
		switch r.URL.Path {
		case "/open-apis/wiki/v2/spaces":
			if q.Get("page_token") != "page1" {
				t.Errorf("wiki spaces query: %v", q)
			}
			fmt.Fprint(w, `{"code":0,"data":{"items":[{"space_id":"space1","name":"测试知识库"}],"has_more":true,"page_token":"page2"}}`)
		case "/open-apis/wiki/v2/spaces/space1/nodes":
			if q.Get("parent_node_token") != "node1" || q.Get("page_token") != "page1" {
				t.Errorf("wiki nodes query: %v", q)
			}
			fmt.Fprint(w, `{"code":0,"data":{"items":[{"space_id":"space1","node_token":"child","parent_node_token":"node1","obj_token":"doc1","obj_type":"docx","title":"子节点","has_child":true,"url":"https://test.feishu.cn/wiki/child"}]}}`)
		case "/open-apis/wiki/v2/spaces/get_node":
			if q.Get("token") != "child" {
				t.Errorf("wiki node query: %v", q)
			}
			fmt.Fprint(w, `{"code":0,"data":{"node":{"space_id":"space1","node_token":"child","parent_node_token":"node1","obj_token":"doc1","obj_type":"docx","title":"子节点","has_child":true,"url":"https://test.feishu.cn/wiki/child"}}}`)
		case "/open-apis/wiki/v2/tasks/task1":
			if q.Get("task_type") != "move" {
				t.Errorf("wiki task query: %v", q)
			}
			if polls.Add(1) == 1 {
				fmt.Fprint(w, `{"code":0,"data":{"task":{"move_result":[{"status":1,"status_msg":"processing"}]}}}`)
			} else {
				fmt.Fprint(w, `{"code":0,"data":{"task":{"move_result":[{"status":0,"node":{"node_token":"moved"}}]}}}`)
			}
		case "/open-apis/wiki/v2/tasks/failed":
			fmt.Fprint(w, `{"code":0,"data":{"task":{"move_result":[{"status":-1,"status_msg":"not supported"}]}}}`)
		case "/open-apis/drive/v1/files/copy1":
			if r.Method != http.MethodDelete || q.Get("type") != "docx" {
				t.Errorf("delete request: %s %v", r.Method, q)
			}
			fmt.Fprint(w, `{"code":0,"data":{}}`)
		default:
			t.Errorf("unexpected API: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	cfg := realConfig(ts.URL)
	auth := loggedInAuth(t, cfg, "u-access", "refresh", time.Hour)
	client := NewFeishuClient(ts.URL, cfg.AppID, cfg.AppSecret, auth, "user")
	client.gap = 0
	ctx := context.WithValue(context.Background(), workspaceUserKey{}, "ou_me")
	spaces, err := client.WikiSpaces(ctx, "page1")
	if err != nil || len(spaces.Items) != 1 || !spaces.HasMore || spaces.PageToken != "page2" {
		t.Fatalf("spaces: %+v %v", spaces, err)
	}
	nodes, err := client.WikiNodes(ctx, "space1", "node1", "page1")
	if err != nil || len(nodes.Items) != 1 || nodes.Items[0].NodeToken != "child" {
		t.Fatalf("nodes: %+v %v", nodes, err)
	}
	node, err := client.WikiNodeInfo(ctx, "child")
	if err != nil || node.SpaceID != "space1" || node.ObjToken != "doc1" || node.ObjType != "docx" || !node.HasChild || node.URL == "" {
		t.Fatalf("node: %+v %v", node, err)
	}
	copy, err := client.DriveCopyFile(ctx, "doc1", "docx", "target", "副本")
	if err != nil || copy.Token != "copy1" {
		t.Fatalf("copy: %+v %v", copy, err)
	}
	res, err := client.WikiMoveDocToSpace(ctx, "space1", "node1", "docx", "doc1")
	if err != nil || res.WikiToken != "moved" || polls.Load() != 2 {
		t.Fatalf("move: %+v %v", res, err)
	}
	if _, err := client.pollWikiMove(ctx, "failed", 0); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("move failure: %v", err)
	}
	if err := client.DriveDeleteFile(ctx, copy.Token, "docx"); err != nil {
		t.Fatal(err)
	}
	auth.mu.Lock()
	auth.tok.User.OpenID = "ou_other"
	auth.mu.Unlock()
	if _, err := client.WikiSpaces(ctx, "page1"); err != ErrLoginRequired {
		t.Fatalf("workspace must not switch identities: %v", err)
	}
}

func TestWorkspaceMockPreviewBytes(t *testing.T) {
	s, _ := newTestServer(t)
	src := workspaceSource{Kind: "im", MessageID: "om_mock_a05", FileKey: "file_mock_log", Name: "preview.log"}
	entry := workspaceJSON[workspacePreviewEntry](t, workspaceHTTP(s, "POST", "/api/workspace/preview", src, true, true), 200)
	ref, _ := driveRef(entry.URL)
	path := "/api/mock/workspace/file/" + ref.Token
	w := workspaceHTTP(s, "GET", path, nil, false, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "token refresh failed") || !strings.Contains(w.Header().Get("Content-Disposition"), "inline") {
		t.Fatalf("mock preview: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	workspaceJSON[map[string]int](t, workspaceHTTP(s, "DELETE", "/api/workspace/preview_cache", nil, true, true), 200)
	if w := workspaceHTTP(s, "GET", path, nil, false, true); w.Code != 404 {
		t.Fatalf("deleted preview still available: %d", w.Code)
	}
}
