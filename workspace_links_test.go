package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type workspaceLinkSpy struct {
	API
	mu        sync.Mutex
	uploads   int
	deletes   []string
	exportRev int
}

func (a *workspaceLinkSpy) DriveUploadToFolder(ctx context.Context, folder, name string, r io.Reader, size int64) (string, error) {
	a.mu.Lock()
	a.uploads++
	a.mu.Unlock()
	return a.API.DriveUploadToFolder(ctx, folder, name, r, size)
}

func (a *workspaceLinkSpy) DriveDeleteFile(ctx context.Context, token, kind string) error {
	a.mu.Lock()
	a.deletes = append(a.deletes, token)
	a.mu.Unlock()
	return a.API.DriveDeleteFile(ctx, token, kind)
}

func (a *workspaceLinkSpy) DriveExport(ctx context.Context, token, kind string) (*Download, error) {
	d, err := a.API.DriveExport(ctx, token, kind)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	rev := a.exportRev
	a.mu.Unlock()
	if rev == 0 {
		return d, nil
	}
	body, err := io.ReadAll(d.Body)
	d.Body.Close()
	if err != nil {
		return nil, err
	}
	body = append(body, []byte(fmt.Sprintf("\nrev-%d", rev))...)
	return &Download{Body: io.NopCloser(bytes.NewReader(body)), Filename: d.Filename, ContentType: d.ContentType, Size: int64(len(body))}, nil
}

func (a *workspaceLinkSpy) uploadCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.uploads
}

func (a *workspaceLinkSpy) deletedTokens() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := append([]string(nil), a.deletes...)
	return out
}

func workspaceFileSHA(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestWorkspaceExportLinkPreviewAndSync(t *testing.T) {
	s, svc := newTestServer(t)
	mock := svc.api.(*MockAPI)
	spy := &workspaceLinkSpy{API: mock}
	svc.api = spy
	const (
		token       = "documentToken"
		exportedMod = int64(1_700_000_000_000)
		updatedMod  = int64(1_800_000_000_000)
		origin      = "https://mock.feishu.cn/docx/" + token
	)
	mock.SetDocModified("docx", token, exportedMod)
	root := t.TempDir()
	ws := workspaceBind(t, s, workspaceTestChat, ChatWorkspace{Items: []WorkspaceItem{{Kind: workspaceLocal, LocalPath: root}}})
	item := ws.Items[0]
	transfer := workspaceTransferRequest{
		ChatID: workspaceTestChat, ItemID: item.ID,
		Source: workspaceSource{Kind: "doc", ResourceType: "docx", ResourceToken: token, Name: "项目方案", URL: origin},
	}
	saved := workspaceJSON[workspaceTransferResult](t, workspaceHTTP(s, "POST", "/api/workspace/transfer", transfer, true, true), 200)
	savedPath, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(saved.RelPath)))
	if err != nil {
		t.Fatal(err)
	}
	index := workspaceLinkIndex{}
	if err := readWorkspaceJSON(svc.workspaceLinksPath(), &index); err != nil {
		t.Fatal(err)
	}
	rec := index[uLi][savedPath]
	sum := workspaceFileSHA(t, savedPath)
	if rec.ExportSHA256 != sum || rec.SourceURL != origin || rec.SourceType != "docx" || rec.SourceToken != token ||
		rec.SourceName != "项目方案" || rec.RemoteModified != exportedMod || rec.UpdatedAt == 0 || rec.LocalPath == "" {
		t.Fatalf("link record: %+v", rec)
	}
	if st, err := os.Stat(svc.workspaceLinksPath()); err != nil || st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("links mode: %v %v", st, err)
	}
	tree := "/api/workspace/tree?chat_id=" + workspaceTestChat + "&item_id=" + item.ID
	page := workspaceJSON[workspaceTreePage](t, workspaceHTTP(s, "GET", tree, nil, true, true), 200)
	var node workspaceNode
	for _, itemNode := range page.Items {
		if itemNode.RelPath == saved.RelPath {
			node = itemNode
		}
	}
	if node.SourceState != "linked" || node.SourceURL != origin {
		t.Fatalf("tree link state: %+v", node)
	}

	previewBody := workspaceFileRequest{ChatID: workspaceTestChat, ItemID: item.ID, RelPath: saved.RelPath}
	const preview = "/api/workspace/preview"
	view := workspaceJSON[workspacePreviewResult](t, workspaceHTTP(s, "POST", preview, previewBody, true, true), 200)
	if view.URL != origin || !view.Cached || view.Source != "feishu_origin" || view.Conflict != "" || spy.uploadCount() != 0 {
		t.Fatalf("unmodified preview uploaded or dropped the origin link: %+v uploads=%d", view, spy.uploadCount())
	}

	plain := workspaceJSON[workspaceTransferResult](t, workspaceHTTP(s, "POST", "/api/workspace/transfer", workspaceTransferRequest{
		ChatID: workspaceTestChat, ItemID: item.ID,
		Source: workspaceSource{Kind: "doc", ResourceType: "docx", ResourceToken: "noModToken", Name: "未标时间"},
	}, true, true), 200)
	plainPath, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(plain.RelPath)))
	if err != nil {
		t.Fatal(err)
	}
	plainView := workspaceJSON[workspacePreviewResult](t, workspaceHTTP(s, "POST", preview, workspaceFileRequest{
		ChatID: workspaceTestChat, ItemID: item.ID, RelPath: plain.RelPath,
	}, true, true), 200)
	if err := readWorkspaceJSON(svc.workspaceLinksPath(), &index); err != nil {
		t.Fatal(err)
	}
	if plainView.URL == "" || plainView.Source != "feishu_origin" || !plainView.Cached || index[uLi][plainPath].RemoteModified != 0 || spy.uploadCount() != 0 {
		t.Fatalf("missing cloud time should still open the origin: %+v record=%+v", plainView, index[uLi][plainPath])
	}

	mock.SetDocModified("docx", token, updatedMod)
	conflict := workspaceJSON[workspacePreviewResult](t, workspaceHTTP(s, "POST", preview, previewBody, true, true), 200)
	if conflict.Conflict != "cloud_updated" || conflict.SourceURL != origin || conflict.SourceName != "项目方案" ||
		conflict.RemoteModified != updatedMod || conflict.ExportedAt != rec.UpdatedAt || spy.uploadCount() != 0 {
		t.Fatalf("cloud update: %+v", conflict)
	}

	spy.mu.Lock()
	spy.exportRev = 1
	spy.mu.Unlock()
	synced := workspaceJSON[workspaceSyncResult](t, workspaceHTTP(s, "POST", "/api/workspace/sync_local", previewBody, true, true), 200)
	body, err := os.ReadFile(savedPath)
	if err != nil {
		t.Fatal(err)
	}
	if synced.URL != origin || synced.Name != filepath.Base(savedPath) || synced.RemoteModified != updatedMod || !bytes.Contains(body, []byte("\nrev-1")) {
		t.Fatalf("sync: %+v file=%q", synced, body)
	}
	if err := readWorkspaceJSON(svc.workspaceLinksPath(), &index); err != nil {
		t.Fatal(err)
	}
	rec = index[uLi][savedPath]
	if rec.ExportSHA256 != workspaceFileSHA(t, savedPath) || rec.RemoteModified != updatedMod || rec.UpdatedAt < conflict.ExportedAt {
		t.Fatalf("link was not refreshed: %+v", rec)
	}
	view = workspaceJSON[workspacePreviewResult](t, workspaceHTTP(s, "POST", preview, previewBody, true, true), 200)
	if view.URL != origin || view.Source != "feishu_origin" || !view.Cached || spy.uploadCount() != 0 {
		t.Fatalf("synced file should open the origin: %+v uploads=%d", view, spy.uploadCount())
	}

	edited := append(append([]byte{}, body...), []byte(" local-edit")...)
	if err := os.WriteFile(savedPath, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	page = workspaceJSON[workspaceTreePage](t, workspaceHTTP(s, "GET", tree, nil, true, true), 200)
	node = workspaceNode{}
	for _, itemNode := range page.Items {
		if itemNode.RelPath == saved.RelPath {
			node = itemNode
		}
	}
	if node.SourceState != "modified" || node.SourceURL != origin {
		t.Fatalf("modified badge: %+v", node)
	}
	first := workspaceJSON[workspacePreviewResult](t, workspaceHTTP(s, "POST", preview, previewBody, true, true), 200)
	firstRef, ok := driveRef(first.URL)
	if !ok || first.Source == "feishu_origin" || first.Cached || spy.uploadCount() != 1 {
		t.Fatalf("modified preview: %+v uploads=%d", first, spy.uploadCount())
	}
	edited = append(edited, []byte("-2")...)
	if err := os.WriteFile(savedPath, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	second := workspaceJSON[workspacePreviewResult](t, workspaceHTTP(s, "POST", preview, previewBody, true, true), 200)
	secondRef, ok := driveRef(second.URL)
	if !ok || second.URL == first.URL || spy.uploadCount() != 2 || !strings.Contains(strings.Join(spy.deletedTokens(), ","), firstRef.Token) {
		t.Fatalf("old preview was not replaced: %+v deletes=%v uploads=%d", second, spy.deletedTokens(), spy.uploadCount())
	}
	mock.mu.RLock()
	_, stillNamed := mock.driveNames[firstRef.Token]
	mock.mu.RUnlock()
	if stillNamed {
		t.Fatalf("old preview file %s is still in Drive", firstRef.Token)
	}
	cache := workspacePreviewIndex{}
	if err := readWorkspaceJSON(svc.workspacePreviewPath(), &cache); err != nil {
		t.Fatal(err)
	}
	kept := false
	for _, entry := range cache[uLi] {
		if entry.Token == firstRef.Token {
			t.Fatalf("old preview record remained: %+v", entry)
		}
		if entry.Token == secondRef.Token && entry.GroupKey == "local:"+savedPath {
			kept = true
		}
	}
	if !kept {
		t.Fatalf("new preview is missing its group key: %+v", cache[uLi])
	}

	refused := workspaceJSON[apiError](t, workspaceHTTP(s, "POST", "/api/workspace/sync_local", previewBody, true, true), 400)
	if !strings.Contains(refused.Error, "已修改") {
		t.Fatalf("sync should protect local edits: %+v", refused)
	}
	after, err := os.ReadFile(savedPath)
	if err != nil || !bytes.Equal(after, edited) {
		t.Fatalf("sync overwrote a local edit: %q %v", after, err)
	}
	if err := readWorkspaceJSON(svc.workspaceLinksPath(), &index); err != nil {
		t.Fatal(err)
	}
	if index[uLi][savedPath].ExportSHA256 != rec.ExportSHA256 {
		t.Fatalf("refused sync changed the link: %+v", index[uLi][savedPath])
	}
	unlinked := workspaceJSON[apiError](t, workspaceHTTP(s, "POST", "/api/workspace/sync_local", workspaceFileRequest{
		ChatID: workspaceTestChat, ItemID: item.ID, RelPath: "plain.txt",
	}, true, true), 404)
	if err := os.WriteFile(filepath.Join(root, "plain.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	unlinked = workspaceJSON[apiError](t, workspaceHTTP(s, "POST", "/api/workspace/sync_local", workspaceFileRequest{
		ChatID: workspaceTestChat, ItemID: item.ID, RelPath: "plain.txt",
	}, true, true), 400)
	if !strings.Contains(unlinked.Error, "没有关联") {
		t.Fatalf("unlinked sync: %+v", unlinked)
	}
}

func TestWorkspaceWikiExportLink(t *testing.T) {
	s, svc := newTestServer(t)
	mock := svc.api.(*MockAPI)
	spy := &workspaceLinkSpy{API: mock}
	svc.api = spy
	const (
		wikiToken = "MockWikiRollback789"
		href      = "https://mock.feishu.cn/wiki/" + wikiToken
		mod       = int64(1_700_000_000_000)
	)
	mock.SetDocModified("docx", "docmock"+wikiToken, mod)
	root := t.TempDir()
	ws := workspaceBind(t, s, workspaceTestChat, ChatWorkspace{Items: []WorkspaceItem{{Kind: workspaceLocal, LocalPath: root}}})
	res := workspaceJSON[workspaceTransferResult](t, workspaceHTTP(s, "POST", "/api/workspace/transfer", workspaceTransferRequest{
		ChatID: workspaceTestChat, ItemID: ws.Items[0].ID,
		Source: workspaceSource{Kind: "wiki", URL: href, Name: "项目方案"},
	}, true, true), 200)
	savedPath, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(res.RelPath)))
	if err != nil {
		t.Fatal(err)
	}
	index := workspaceLinkIndex{}
	if err := readWorkspaceJSON(svc.workspaceLinksPath(), &index); err != nil {
		t.Fatal(err)
	}
	rec := index[uLi][savedPath]
	if rec.WikiToken != wikiToken || rec.SourceURL != href || rec.SourceType != "docx" || rec.SourceToken != "docmock"+wikiToken ||
		rec.ExportSHA256 != workspaceFileSHA(t, savedPath) || rec.RemoteModified != mod {
		t.Fatalf("wiki link: %+v", rec)
	}
	view := workspaceJSON[workspacePreviewResult](t, workspaceHTTP(s, "POST", "/api/workspace/preview", workspaceFileRequest{
		ChatID: workspaceTestChat, ItemID: ws.Items[0].ID, RelPath: res.RelPath,
	}, true, true), 200)
	if view.URL != href || view.Source != "feishu_origin" || !view.Cached || spy.uploadCount() != 0 {
		t.Fatalf("wiki preview: %+v uploads=%d", view, spy.uploadCount())
	}
}
