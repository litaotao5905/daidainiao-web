package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func topicFilesRunning(s *Service) bool {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	for _, rec := range s.fileJobs {
		select {
		case <-rec.done:
		default:
			return true
		}
	}
	return false
}
func waitTopicFiles(t *testing.T, s *Service) {
	t.Helper()
	limit := time.Now().Add(10 * time.Second)
	for topicFilesRunning(s) {
		if time.Now().After(limit) {
			t.Fatal("topic file jobs did not settle")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestClassifyCloudResources(t *testing.T) {
	cases := []struct{ url, kind, typ string }{
		{"https://x.feishu.cn/docx/docabc", "doc", "docx"},
		{"https://x.feishu.cn/drive/file/fileabc", "file", "file"},
		{"https://x.feishu.cn/file/fileabc", "file", "file"},
		{"https://x.feishu.cn/drive/folder/foldabc", "folder", "folder"},
		{"https://x.feishu.cn/wiki/wikiabc", "unresolved", "wiki"},
	}
	for _, c := range cases {
		a := classifyAttachment(Attachment{URL: c.url})
		if a.Kind != c.kind || a.ResourceType != c.typ {
			t.Fatalf("%s: %+v", c.url, a)
		}
	}
	if _, ok := driveRef("https://evil.test/file/abcdef"); ok {
		t.Fatal("non-Feishu URL accepted")
	}
}

func TestCloudFilesRemainManualOnEveryTopicLoad(t *testing.T) {
	e := newDLEnv(t)
	tr, err := e.svc.Thread(context.Background(), "omt_mock_002", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range tr.Downloads {
		if v.Status != dlNotDownloaded {
			t.Fatalf("automatic download: %+v", v)
		}
	}
	waitTopicFiles(t, e.svc)
	for range []bool{false, true} {
		ctx := context.Background()
		if _, err := e.svc.Thread(ctx, "omt_mock_002", true); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range e.svc.dl.List(topicContext(tr).TopicKey) {
		if v.Status != dlNotDownloaded {
			t.Fatalf("refresh fetched a resource: %+v", v)
		}
	}
	if e.api.dl.Load() != 0 {
		t.Fatal("message resource fetched without a click")
	}
}

type treeAPI struct {
	*MockAPI
	pages     map[string]DriveListPage
	nodes     map[string]DriveResource
	listCalls int
}

func (a *treeAPI) DriveList(ctx context.Context, folder, page string) (*DriveListPage, error) {
	a.listCalls++
	p, ok := a.pages[folder+":"+page]
	if !ok {
		return nil, fmt.Errorf("folder access denied")
	}
	return &p, nil
}
func (a *treeAPI) WikiResolve(ctx context.Context, token string) (*DriveResource, error) {
	r, ok := a.nodes[token]
	if !ok {
		return nil, fmt.Errorf("wiki access denied")
	}
	return &r, nil
}

func TestRecursiveFoldersWikiClassificationAndPartialScan(t *testing.T) {
	e := newDLEnv(t)
	tree := &treeAPI{MockAPI: e.api.MockAPI, pages: map[string]DriveListPage{
		"root:":    {Files: []DriveResource{{Token: "boxmocktree1", Type: "file", Name: "report.pdf"}, {Token: "sub", Type: "folder", Name: "材料", URL: "https://x.feishu.cn/drive/folder/sub"}}, HasMore: true, NextPage: "two"},
		"root:two": {Files: []DriveResource{{Token: "docone", Type: "docx", Name: "业务文档", URL: "https://x.feishu.cn/docx/docone"}}},
		"sub:":     {Files: []DriveResource{{Token: "root", Type: "folder", Name: "循环"}, {Token: "boxmocktree2", Type: "file", Name: "report.pdf"}}},
	}, nodes: map[string]DriveResource{"wikifile": {Token: "boxmocktree1", Type: "file", Name: "report.pdf"}, "wikidoc": {Token: "doctwo", Type: "sheet", Name: "在线表格"}}}
	tree.SetDriveFile("boxmocktree1", "report.pdf", []byte("one"), 1000)
	tree.SetDriveFile("boxmocktree2", "report.pdf", []byte("two"), 1000)
	e.svc.api = tree
	raw := []Attachment{
		classifyAttachment(Attachment{URL: "https://x.feishu.cn/drive/folder/root", Name: "资料", MessageID: "omroot"}),
		classifyAttachment(Attachment{URL: "https://x.feishu.cn/wiki/wikifile", MessageID: "omroot"}),
		classifyAttachment(Attachment{URL: "https://x.feishu.cn/wiki/wikidoc", MessageID: "omroot"}),
	}
	out, warns := e.svc.resolveTopicResources(context.Background(), raw)
	if len(warns) != 0 || tree.listCalls != 3 {
		t.Fatalf("scan: %+v calls=%d", warns, tree.listCalls)
	}
	count := map[string]int{}
	for _, a := range out {
		count[a.Kind]++
	}
	if count["file"] != 2 || count["doc"] != 2 || count["folder"] != 2 {
		t.Fatalf("classifications: %+v", out)
	}
	files := []Attachment{}
	for _, a := range out {
		if a.Kind == "file" {
			files = append(files, a)
		}
	}
	entries := e.svc.dl.Register(tc1, files)
	for _, v := range entries {
		if err := e.svc.dl.waitFile(context.Background(), v.ID); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.svc.dl.List(tc1.TopicKey)) != 2 {
		t.Fatal("Wiki/direct/folder duplicate downloaded twice")
	}
	for _, v := range e.svc.dl.List(tc1.TopicKey) {
		if v.SourcePath == "" {
			t.Fatal("folder structure lost")
		}
	}
	// Removing a remote file preserves its local copy and marks its source.
	remaining := []Attachment{}
	for _, a := range out {
		if a.FileKey != "boxmocktree2" {
			remaining = append(remaining, a)
		}
	}
	e.svc.dl.markFolderSources(tc1.TopicKey, remaining, true)
	for _, v := range e.svc.dl.List(tc1.TopicKey) {
		if v.FileKey == "boxmocktree2" && (!v.RemoteMissing || v.Status != dlDone) {
			t.Fatalf("removed file lost local copy: %+v", v)
		}
	}
	// A permission error is partial, never evidence of remote deletion.
	delete(tree.pages, "sub:")
	_, warns = e.svc.resolveTopicResources(context.Background(), raw)
	if len(warns) == 0 {
		t.Fatal("partial scan marked complete")
	}
}

func TestCloudFailureKeepsVersionAndLocalBackup(t *testing.T) {
	ff := newFakeFetch(t)
	ff.names["boxone"] = "plan.txt"
	ff.data["boxone"] = []byte("original")
	dm, base := newTestDM(t, ff)
	a := classifyAttachment(Attachment{URL: "https://x.feishu.cn/file/boxone", MessageID: "om1", Name: "plan.txt"})
	dm.Register(tc1, []Attachment{a})
	v := waitEntry(t, dm, a)
	dm.mu.Lock()
	dm.entries[v.Key].CloudModTime = 100
	dm.mu.Unlock()
	os.WriteFile(v.Path, []byte("local modification"), 0o600)
	ff.mu.Lock()
	ff.err = fmt.Errorf("network interrupted")
	ff.mu.Unlock()
	dm.TriggerUpdate(v.Key, 200)
	if err := dm.waitFile(context.Background(), v.ID); err == nil {
		t.Fatal("refresh failure was hidden")
	}
	v, _ = dm.Get(v.ID)
	if v.CloudModTime != 100 || v.Status != dlDone || v.SyncStatus != "failed" {
		t.Fatalf("failed version incorrectly committed: %+v", v)
	}
	b, _ := os.ReadFile(v.Path)
	if string(b) != "local modification" {
		t.Fatal("old copy overwritten")
	}
	ff.mu.Lock()
	ff.err = nil
	ff.data["boxone"] = []byte("cloud latest")
	ff.mu.Unlock()
	dm.Redownload(v.ID)
	if err := dm.waitFile(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	v, _ = dm.Get(v.ID)
	b, _ = os.ReadFile(filepath.Join(base, filepath.FromSlash(v.BackupRel)))
	if string(b) != "local modification" {
		t.Fatalf("local backup lost: %q", b)
	}
}

func TestSendAndQueuedDispatchDoNotWaitForBackgroundFiles(t *testing.T) {
	e := newDLEnv(t)
	release := make(chan struct{})
	defer close(release)
	e.svc.api = &slowMetaAPI{API: e.api, release: release}
	tr, err := e.svc.Thread(context.Background(), "omt_mock_002", true)
	if err != nil {
		t.Fatal(err)
	}
	if !tr.FileSync.NamesLoading {
		t.Fatal("unknown names were not queued separately")
	}
	target := composeTarget{chatID: tr.Topic.ChatID, rootID: tr.Topic.RootMessageID, threadID: tr.Topic.ThreadID}
	for _, queued := range []bool{false, true} {
		result := make(chan error, 1)
		go func() {
			var res *ComposeResult
			var err error
			if queued {
				item := &QueuedItem{ID: "nonblocking-queue", ChatID: target.chatID, RootID: target.rootID, ThreadID: target.threadID, Text: "send while files sync", UUID: "nonblocking-queued", Status: "sending"}
				res, err = e.svc.dispatchQueuedItem(context.Background(), target.threadID, item)
			} else {
				res, err = e.svc.compose(context.Background(), target, "send while files sync", nil, "nonblocking-send", nil)
			}
			if err == nil && (len(res.Messages) != 1 || len(res.Warnings) != 0) {
				err = fmt.Errorf("message or warnings: %+v", res)
			}
			result <- err
		}()
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatalf("queued=%v waited for blocked historical metadata", queued)
		}
	}
	if e.api.dl.Load() != 0 {
		t.Fatal("sending started a file download")
	}
}

func TestLegacyMergedPathsSplitWithoutOverwritingAnotherResource(t *testing.T) {
	ff := newFakeFetch(t)
	dm, base := newTestDM(t, ff)
	a := att("file", "report.txt", "m1", "legacyA")
	b := att("file", "report.txt", "m2", "legacyB")
	dm.Register(tc1, []Attachment{a, b})
	first := waitEntry(t, dm, a)
	second := waitEntry(t, dm, b)
	dm.mu.Lock()
	dm.entries[second.Key].Rel = first.Rel
	dm.saveLocked()
	dm.mu.Unlock()
	reloaded := NewDownloadManager(base, "", dm.idxPath, ff.fetch)
	reloaded.Register(tc1, []Attachment{a, b})
	if err := reloaded.Redownload(second.ID); err != nil {
		t.Fatal(err)
	}
	if err := reloaded.waitFile(context.Background(), second.ID); err != nil {
		t.Fatal(err)
	}
	after, _ := reloaded.Get(second.ID)
	if after.Rel == first.Rel {
		t.Fatal("legacy merged path still shared by distinct resources")
	}
	bytes, _ := os.ReadFile(first.Path)
	if string(bytes) != "content of legacyA" {
		t.Fatal("refresh overwrote another resource")
	}
}

type slowMetaAPI struct {
	API
	release chan struct{}
}

func (a *slowMetaAPI) DriveMetas(ctx context.Context, r []DriveRef) (map[string]DriveResource, error) {
	select {
	case <-a.release:
		return a.API.DriveMetas(ctx, r)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func TestNewMessageRegistersFileWithoutDownloading(t *testing.T) {
	e := newDLEnv(t)
	release := make(chan struct{})
	e.svc.api = &slowMetaAPI{API: e.api, release: release}
	tr, err := e.svc.Thread(context.Background(), "omt_mock_002", true)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	e.api.SetDriveFile("boxmocknewincoming", "new-incoming.txt", []byte("new reply file"), time.Now().UnixMilli())
	_, err = e.api.ReplyMessage(context.Background(), tr.Topic.RootMessageID, "text", `{"text":"new incoming https://mock.feishu.cn/file/boxmocknewincoming"}`, "incoming-during-sync", true)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	next, err := e.svc.Thread(context.Background(), tr.Topic.ThreadID, true)
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range next.Attachments {
		if a.ResourceToken == "boxmocknewincoming" {
			found = true
		}
	}
	if !found {
		t.Fatal("new file hidden until old scan finishes")
	}
	waitTopicFiles(t, e.svc)
	for _, entry := range e.svc.dl.List(topicContext(tr).TopicKey) {
		if entry.FileKey == "boxmocknewincoming" {
			if entry.Status != dlNotDownloaded || entry.Rel != "" {
				t.Fatalf("incoming file was fetched: %+v", entry)
			}
			return
		}
	}
	t.Fatal("message arriving during scan was dropped")
}

type longThreadAPI struct {
	API
	calls  int
	repeat bool
	fail   bool
}

func (a *longThreadAPI) ListMessages(ctx context.Context, typ, id, token string, desc bool) (*MsgPage, error) {
	if a.fail {
		return nil, fmt.Errorf("message service unavailable")
	}
	a.calls++
	next := fmt.Sprint(a.calls)
	if a.repeat {
		next = "repeated"
	}
	return &MsgPage{Items: []RawMessage{{MessageID: fmt.Sprintf("om_long_%d", a.calls), CreateTime: fmt.Sprint(a.calls)}}, HasMore: a.calls < 23, PageToken: next}, nil
}
func TestAllThreadPagesAndRefreshFailureAreExplicit(t *testing.T) {
	_, s := newTestServer(t)
	api := &longThreadAPI{API: s.api}
	s.api = api
	msgs, err := s.threadMessages(context.Background(), "long", nil, true)
	if err != nil || len(msgs) != 23 || api.calls != 23 {
		t.Fatalf("thread silently truncated: %d pages=%d err=%v", len(msgs), api.calls, err)
	}
	api.fail = true
	if _, err := s.threadMessages(context.Background(), "long", nil, true); err == nil {
		t.Fatal("forced refresh silently reused stale history")
	}
	api.fail = false
	api.repeat = true
	api.calls = 0
	if _, err := s.threadMessages(context.Background(), "cycle", nil, true); err == nil {
		t.Fatal("pagination cycle was treated as complete")
	}
}
