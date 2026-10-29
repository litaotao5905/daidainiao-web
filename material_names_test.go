package main

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

type nameTestAPI struct {
	API
	calls     atomic.Int32
	downloads atomic.Int32
	release   <-chan struct{}
	fail      bool
}

func (a *nameTestAPI) DriveDownload(ctx context.Context, token string) (*Download, error) {
	a.downloads.Add(1)
	return a.API.DriveDownload(ctx, token)
}

func (a *nameTestAPI) DriveMetas(ctx context.Context, refs []DriveRef) (map[string]DriveResource, error) {
	a.calls.Add(1)
	if a.release != nil {
		select {
		case <-a.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if a.fail {
		return nil, fmt.Errorf("metadata denied")
	}
	return a.API.DriveMetas(ctx, refs)
}
func nameOnlyThread() *ThreadResp {
	return &ThreadResp{Topic: Topic{ThreadID: "omt_names", RootMessageID: tc1.TopicKey, ChatID: tc1.ChatID}, Attachments: []Attachment{{ID: "name-test", MessageID: "om_name", URL: "https://mock.feishu.cn/file/boxmockseed01", Name: "https://mock.feishu.cn/file/boxmockseed01"}}}
}
func TestMissingFileNameIsNonblockingCachedAndNeverDownloads(t *testing.T) {
	e := newDLEnv(t)
	release := make(chan struct{})
	api := &nameTestAPI{API: e.api, release: release}
	e.svc.api = api
	start := time.Now()
	tr := e.svc.registerTopicFiles(context.Background(), nameOnlyThread())
	if time.Since(start) > time.Second || !tr.FileSync.NamesLoading || tr.Downloads[0].Status != dlNotDownloaded {
		close(release)
		t.Fatalf("name lookup blocked or downloaded: %+v", tr)
	}
	for i := 0; i < 5; i++ {
		e.svc.registerTopicFiles(context.Background(), nameOnlyThread())
	}
	close(release)
	waitTopicFiles(t, e.svc)
	tr = e.svc.registerTopicFiles(context.Background(), nameOnlyThread())
	if tr.Attachments[0].Name != "发版checklist.txt" || tr.Downloads[0].NameHint != "发版checklist.txt" || tr.Downloads[0].Status != dlNotDownloaded || api.calls.Load() != 1 || e.api.dl.Load() != 0 {
		t.Fatalf("name-only result: %+v calls=%d", tr, api.calls.Load())
	}
	// A new service with an empty download catalogue reads the persistent name cache.
	svc := NewService(api, NewCache(e.svc.cache.dir), e.svc.cfg, e.svc.auth)
	t.Cleanup(func() { waitTopicFiles(t, svc) })
	again := svc.registerTopicFiles(context.Background(), nameOnlyThread())
	if again.Attachments[0].Name != "发版checklist.txt" || again.FileSync.NamesLoading || api.calls.Load() != 1 {
		t.Fatal("cached name was queried again after restart")
	}
}
func TestNameLookupFailureKeepsURLAndThrottlesRetries(t *testing.T) {
	e := newDLEnv(t)
	api := &nameTestAPI{API: e.api, fail: true}
	e.svc.api = api
	e.svc.registerTopicFiles(context.Background(), nameOnlyThread())
	waitTopicFiles(t, e.svc)
	for i := 0; i < 5; i++ {
		tr := e.svc.registerTopicFiles(context.Background(), nameOnlyThread())
		if tr.Attachments[0].Name != tr.Attachments[0].URL || tr.Downloads[0].Status != dlNotDownloaded || tr.FileSync.NamesLoading {
			t.Fatalf("failed name lookup changed resource: %+v", tr)
		}
	}
	if api.calls.Load() != 1 || e.api.dl.Load() != 0 {
		t.Fatal("failed lookup caused repeated requests or a download")
	}
}
func TestKnownDownloadedNameDoesNotQueryCloud(t *testing.T) {
	e := newDLEnv(t)
	a := classifyAttachment(nameOnlyThread().Attachments[0])
	item := e.svc.dl.Catalog(tc1, []Attachment{a})[0]
	if err := e.svc.dl.Redownload(item.ID); err != nil {
		t.Fatal(err)
	}
	waitEntryByID(t, e.svc.dl, item.ID)
	before, _ := e.svc.dl.Get(item.ID)
	api := &nameTestAPI{API: e.api}
	e.svc.api = api
	tr := e.svc.registerTopicFiles(context.Background(), nameOnlyThread())
	after, _ := e.svc.dl.Get(item.ID)
	if tr.Attachments[0].Name != "发版checklist.txt" || tr.FileSync.NamesLoading || api.calls.Load() != 0 || before.Rel != after.Rel || before.ContentHash != after.ContentHash {
		t.Fatal("known local name triggered a cloud lookup or changed the file")
	}
}

func TestRepeatedLinkRefreshesRenamedTitleWithoutDownloading(t *testing.T) {
	e := newDLEnv(t)
	api := &nameTestAPI{API: e.api}
	e.svc.api = api
	e.api.SetDriveFile("boxmockseed01", "示例_V1.3.html", []byte("old"), 1000)
	thread := func(ref int64) *ThreadResp {
		tr := nameOnlyThread()
		tr.Attachments[0].LastReferenced = ref
		return tr
	}
	e.svc.registerTopicFiles(context.Background(), thread(1000))
	waitTopicFiles(t, e.svc)
	e.api.SetDriveFile("boxmockseed01", "示例_V1.4.html", []byte("new"), 2000)
	tr := e.svc.registerTopicFiles(context.Background(), thread(time.Now().Add(time.Hour).UnixMilli()))
	if !tr.FileSync.NamesLoading {
		t.Fatal("new reference did not invalidate cached title")
	}
	waitTopicFiles(t, e.svc)
	for i := 0; i < 5; i++ {
		tr = e.svc.registerTopicFiles(context.Background(), thread(1000))
		if len(tr.Attachments) != 1 || tr.Attachments[0].Name != "示例_V1.4.html" || tr.Downloads[0].CloudName != "示例_V1.4.html" || tr.Downloads[0].Status != dlNotDownloaded {
			t.Fatalf("rename/dedup failed: %+v", tr)
		}
	}
	if api.calls.Load() != 2 || e.api.dl.Load() != 0 || api.downloads.Load() != 0 {
		t.Fatalf("polling queried/downloaded resources: calls=%d", api.calls.Load())
	}
}

func TestExplicitNameRefreshPreservesDownloadedCopy(t *testing.T) {
	e := newDLEnv(t)
	e.api.SetDriveFile("boxmockseed01", "示例_V1.3.html", []byte("local copy"), 1000)
	a := classifyAttachment(nameOnlyThread().Attachments[0])
	item := e.svc.dl.Catalog(tc1, []Attachment{a})[0]
	if err := e.svc.dl.Redownload(item.ID); err != nil {
		t.Fatal(err)
	}
	waitEntryByID(t, e.svc.dl, item.ID)
	before, _ := e.svc.dl.Get(item.ID)
	e.api.SetDriveFile("boxmockseed01", "示例_V1.4.html", []byte("new remote bytes"), 2000)
	api := &nameTestAPI{API: e.api}
	e.svc.api = api
	ctx := context.WithValue(context.Background(), refreshMaterialNamesKey{}, true)
	e.svc.registerTopicFiles(ctx, nameOnlyThread())
	waitTopicFiles(t, e.svc)
	after, _ := e.svc.dl.Get(item.ID)
	if after.CloudName != "示例_V1.4.html" || before.Name != after.Name || before.Rel != after.Rel || before.ContentHash != after.ContentHash || before.CloudModTime != after.CloudModTime || before.Status != after.Status || api.downloads.Load() != 0 || api.calls.Load() != 1 {
		t.Fatalf("title refresh altered the local copy: before=%+v after=%+v", before, after)
	}
}

type orderedNameAPI struct {
	API
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (a *orderedNameAPI) DriveMetas(ctx context.Context, refs []DriveRef) (map[string]DriveResource, error) {
	name := "示例_V1.4.html"
	if a.calls.Add(1) == 1 {
		close(a.started)
		select {
		case <-a.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		name = "示例_V1.3.html"
	}
	return map[string]DriveResource{"file:boxmockseed01": {Token: "boxmockseed01", Type: "file", Name: name}}, nil
}
func TestOlderTitleResponseCannotOverwriteNewerReference(t *testing.T) {
	e := newDLEnv(t)
	api := &orderedNameAPI{API: e.api, started: make(chan struct{}), release: make(chan struct{})}
	e.svc.api = api
	tr := nameOnlyThread()
	tr.Attachments[0].LastReferenced = 1000
	e.svc.registerTopicFiles(context.Background(), tr)
	<-api.started
	tr = nameOnlyThread()
	tr.Attachments[0].LastReferenced = time.Now().Add(time.Hour).UnixMilli()
	e.svc.registerTopicFiles(context.Background(), tr)
	limit := time.Now().Add(5 * time.Second)
	for {
		var cached materialName
		e.svc.cache.Get(materialNameCacheKey(e.svc.auth.UserID(), classifyAttachment(tr.Attachments[0])), &cached)
		if cached.Name == "示例_V1.4.html" {
			break
		}
		if time.Now().After(limit) {
			close(api.release)
			t.Fatal("new title response did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	close(api.release)
	waitTopicFiles(t, e.svc)
	result := e.svc.registerTopicFiles(context.Background(), nameOnlyThread())
	if result.Attachments[0].Name != "示例_V1.4.html" || result.Downloads[0].CloudName != "示例_V1.4.html" || api.calls.Load() != 2 {
		t.Fatalf("older response regressed title: %+v", result)
	}
}

func TestRepeatedLinkTracksLatestMessageAndEditTime(t *testing.T) {
	e := newDLEnv(t)
	var msgs []RawMessage
	for i := 0; i < 3; i++ {
		m := RawMessage{MessageID: fmt.Sprintf("om_ref_%d", i), MsgType: "text", CreateTime: strconv.FormatInt(int64(1000000000000+i*1000), 10)}
		m.Body.Content = `{"text":"https://mock.feishu.cn/file/boxmockseed01"}`
		if i == 1 {
			m.UpdateTime = "1000000009000"
		}
		msgs = append(msgs, m)
	}
	r := e.svc.render(context.Background(), msgs)
	if len(r.atts) != 1 || r.atts[0].LastReferenced != 1000000009000 || r.atts[0].MessageID != "om_ref_0" {
		t.Fatalf("dedup lost latest reference or original identity: %+v", r.atts)
	}
}

func TestFailedRenameRevalidationRetainsNameAndAllowsManualRetry(t *testing.T) {
	e := newDLEnv(t)
	api := &nameTestAPI{API: e.api}
	e.svc.api = api
	e.svc.registerTopicFiles(context.Background(), nameOnlyThread())
	waitTopicFiles(t, e.svc)
	api.fail = true
	tr := nameOnlyThread()
	tr.Attachments[0].LastReferenced = time.Now().Add(time.Hour).UnixMilli()
	e.svc.registerTopicFiles(context.Background(), tr)
	waitTopicFiles(t, e.svc)
	result := e.svc.registerTopicFiles(context.Background(), tr)
	if result.Attachments[0].Name != "发版checklist.txt" || result.FileSync.NamesLoading || api.calls.Load() != 2 {
		t.Fatalf("failed revalidation lost name or retried repeatedly: %+v", result)
	}
	api.fail = false
	e.api.SetDriveFile("boxmockseed01", "示例_V1.4.html", []byte("new"), 2000)
	e.svc.registerTopicFiles(context.WithValue(context.Background(), refreshMaterialNamesKey{}, true), tr)
	waitTopicFiles(t, e.svc)
	result = e.svc.registerTopicFiles(context.Background(), tr)
	if result.Attachments[0].Name != "示例_V1.4.html" || api.calls.Load() != 3 || e.api.dl.Load() != 0 {
		t.Fatalf("explicit retry failed: %+v", result)
	}
}
