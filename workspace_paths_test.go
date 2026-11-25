package main

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkspaceLocalSearch(t *testing.T) {
	s, svc := newTestServer(t)
	s.docker = false
	host := t.TempDir()
	container := t.TempDir()
	download := t.TempDir()
	hostDownload := filepath.Join(t.TempDir(), "Feishu")
	s.cfg.HostHome, s.cfg.ContainerHome = host, container
	s.cfg.HostDownloadDir, s.cfg.DownloadDir = hostDownload, download
	svc.cfg = s.cfg
	for _, path := range []string{
		filepath.Join(container, "Desktop", "Match-top"),
		filepath.Join(container, "Desktop", "branch", "MATCH-nested"),
		filepath.Join(container, "Desktop", ".Match-hidden", "Match-child"),
		filepath.Join(container, "Desktop", "a", "b", "c", "d", "Match-too-deep"),
		filepath.Join(container, "Documents", "Match-docs"),
		filepath.Join(container, "Downloads", "Match-downloads"),
		filepath.Join(download, "Match-feishu"),
	} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(container, "Desktop", "Match-file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	search := "/api/workspace/local/search?q=mAtCh"
	scoped := search + "&dir=" + url.QueryEscape(filepath.Join(host, "Desktop"))
	got := workspaceJSON[[]workspaceLocalMatch](t, workspaceHTTP(s, "GET", scoped, nil, true, true), 200)
	if len(got) != 2 || got[0] != (workspaceLocalMatch{"Match-top", "~/Desktop/Match-top", "~/Desktop"}) ||
		got[1] != (workspaceLocalMatch{"MATCH-nested", "~/Desktop/branch/MATCH-nested", "~/Desktop/branch"}) {
		t.Fatalf("scoped search: %+v", got)
	}
	got = workspaceJSON[[]workspaceLocalMatch](t, workspaceHTTP(s, "GET", search, nil, true, true), 200)
	if len(got) != 5 || got[1].Name != "Match-docs" || got[2].Name != "Match-downloads" ||
		got[3].Path != filepath.Join(hostDownload, "Match-feishu") {
		t.Fatalf("default roots: %+v", got)
	}
	roundTrip, err := svc.workspaceResolveContainer(got[3].Path)
	if err != nil || roundTrip != filepath.Join(download, "Match-feishu") {
		t.Fatalf("download result path could not be resolved: %q, %v", roundTrip, err)
	}
	if err := os.Symlink(download, filepath.Join(container, "Desktop", "Match-link")); err == nil {
		got = workspaceJSON[[]workspaceLocalMatch](t, workspaceHTTP(s, "GET", scoped, nil, true, true), 200)
		if len(got) != 2 {
			t.Fatalf("search followed a link outside its starting directory: %+v", got)
		}
		workspaceJSON[apiError](t, workspaceHTTP(s, "GET", search+"&dir="+url.QueryEscape("~/Desktop/Match-link"), nil, true, true), 403)
	}
	workspaceJSON[apiError](t, workspaceHTTP(s, "GET", search+"&dir="+url.QueryEscape("~/.."), nil, true, true), 403)
	workspaceJSON[apiError](t, workspaceHTTP(s, "GET", search+"&dir="+url.QueryEscape("~/Desktop/Missing"), nil, true, true), 404)
	workspaceJSON[apiError](t, workspaceHTTP(s, "GET", search+"&dir="+url.QueryEscape("~/Desktop/Match-file"), nil, true, true), 400)
	workspaceJSON[apiError](t, workspaceHTTP(s, "GET", scoped, nil, false, true), 403)
	workspaceJSON[apiError](t, workspaceHTTP(s, "GET", scoped, nil, true, false), 403)
	svc.auth.Logout()
	workspaceJSON[apiError](t, workspaceHTTP(s, "GET", scoped, nil, true, true), 401)
}

func TestWorkspaceHostContainerMapping(t *testing.T) {
	hostRoot := t.TempDir()
	containerRoot := t.TempDir()
	containerDesktop := filepath.Join(containerRoot, "Desktop", "售前")
	if err := os.MkdirAll(containerDesktop, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(containerDesktop, "brief.txt"), []byte("demo"), 0o600); err != nil {
		t.Fatal(err)
	}

	s, svc := newTestServer(t)
	s.docker = true
	s.cfg.HostHome = hostRoot
	s.cfg.ContainerHome = containerRoot
	s.cfg.HostDownloadDir = filepath.Join(hostRoot, "Documents", "FeishuTopics")
	s.cfg.DownloadDir = filepath.Join(containerRoot, "Documents", "FeishuTopics")
	svc.cfg = s.cfg
	if err := os.MkdirAll(s.cfg.DownloadDir, 0o700); err != nil {
		t.Fatal(err)
	}

	hostPath := filepath.Join(hostRoot, "Desktop", "售前")
	container, err := svc.workspaceResolveContainer(hostPath)
	if err != nil || container != containerDesktop {
		t.Fatalf("resolve host path: %q err=%v", container, err)
	}
	tilde, err := svc.workspaceResolveContainer("~/Desktop/售前")
	if err != nil || tilde != containerDesktop {
		t.Fatalf("resolve tilde path: %q err=%v", tilde, err)
	}
	display := svc.workspaceDisplayPath(containerDesktop)
	if display != "~/Desktop/售前" && display != filepath.Join(hostRoot, "Desktop", "售前") {
		t.Fatalf("display path: %q", display)
	}

	ws := workspaceBind(t, s, workspaceTestChat, ChatWorkspace{Items: []WorkspaceItem{
		{Kind: workspaceLocal, LocalPath: "~/Desktop/售前"},
	}})
	item := ws.Items[0]
	if item.LocalPath == "" {
		t.Fatalf("binding lost display path: %+v", item)
	}

	tree := "/api/workspace/tree?chat_id=" + workspaceTestChat + "&item_id=" + item.ID
	page := workspaceJSON[workspaceTreePage](t, workspaceHTTP(s, "GET", tree, nil, true, true), 200)
	if len(page.Items) != 1 || page.Items[0].RelPath != "brief.txt" {
		t.Fatalf("tree through mapping: %+v", page)
	}

	places := workspaceJSON[struct {
		Items []workspacePlace
	}](t, workspaceHTTP(s, "GET", "/api/workspace/local/places", nil, true, true), 200)
	if len(places.Items) == 0 {
		t.Fatalf("expected quick places: %+v", places)
	}

	browse := workspaceJSON[map[string]any](t, workspaceHTTP(s, "GET", "/api/workspace/local/browse?dir="+url.QueryEscape("~/Desktop"), nil, true, true), 200)
	if browse["dir"] == "" || browse["breadcrumbs"] == nil {
		t.Fatalf("browse breadcrumbs: %+v", browse)
	}

	stage := workspaceJSON[Staged](t, workspaceHTTP(s, "POST", "/api/workspace/stage", workspaceFileRequest{
		ChatID: workspaceTestChat, ItemID: item.ID, RelPath: "brief.txt",
	}, true, true), 200)
	if stage.Name != "brief.txt" {
		t.Fatalf("stage: %+v", stage)
	}
	s.noOpen = false
	var revealed string
	s.opener.HostURL = ""
	s.opener.Run = func(_ string, args []string, _ time.Duration) error {
		revealed = args[len(args)-1]
		return nil
	}
	workspaceJSON[map[string]bool](t, workspaceHTTP(s, "POST", "/api/workspace/reveal", workspaceFileRequest{
		ChatID: workspaceTestChat, ItemID: item.ID, RelPath: "brief.txt",
	}, true, true), 200)
	want := svc.workspaceOpenerHostPath(filepath.Join(containerDesktop, "brief.txt"))
	if revealed != want {
		t.Fatalf("reveal host path: %q want %q", revealed, want)
	}
	outside := t.TempDir()
	workspaceJSON[apiError](t, workspaceHTTP(s, "GET", tree+"&node="+url.QueryEscape(filepath.Join("..", "..", "..", outside)), nil, true, true), 403)
}

func TestWorkspaceCloudBrowse(t *testing.T) {
	s, svc := newTestServer(t)
	drive := workspaceJSON[map[string]any](t, workspaceHTTP(s, "GET", "/api/workspace/cloud/browse?kind=feishu_folder", nil, true, true), 200)
	if drive["items"] == nil {
		t.Fatalf("drive browse: %+v", drive)
	}
	wiki := workspaceJSON[map[string]any](t, workspaceHTTP(s, "GET", "/api/workspace/cloud/browse?kind=wiki", nil, true, true), 200)
	if wiki["items"] == nil {
		t.Fatalf("wiki browse: %+v", wiki)
	}
	nodes := workspaceJSON[map[string]any](t, workspaceHTTP(s, "GET", "/api/workspace/cloud/browse?kind=wiki&space_id=mockWikiSpace", nil, true, true), 200)
	if nodes["items"] == nil {
		t.Fatalf("wiki nodes browse: %+v", nodes)
	}
	workspaceJSON[apiError](t, workspaceHTTP(s, "GET", "/api/workspace/cloud/browse?kind=bad", nil, true, true), 400)
	svc.auth.Logout()
	workspaceJSON[apiError](t, workspaceHTTP(s, "GET", "/api/workspace/cloud/browse?kind=feishu_folder", nil, false, true), 401)
}
