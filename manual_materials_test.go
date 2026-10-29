package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type materialSpy struct {
	API
	reads   atomic.Int32
	names   atomic.Int32
	exports atomic.Int32
}

func (a *materialSpy) DriveMetas(ctx context.Context, refs []DriveRef) (map[string]DriveResource, error) {
	a.names.Add(1)
	return a.API.DriveMetas(ctx, refs)
}
func (a *materialSpy) DriveList(ctx context.Context, token, page string) (*DriveListPage, error) {
	a.reads.Add(1)
	return a.API.DriveList(ctx, token, page)
}
func (a *materialSpy) WikiResolve(ctx context.Context, token string) (*DriveResource, error) {
	a.names.Add(1)
	return a.API.WikiResolve(ctx, token)
}
func (a *materialSpy) DriveDownload(ctx context.Context, token string) (*Download, error) {
	a.reads.Add(1)
	return a.API.DriveDownload(ctx, token)
}
func (a *materialSpy) DriveExport(ctx context.Context, token, kind string) (*Download, error) {
	a.exports.Add(1)
	return a.API.DriveExport(ctx, token, kind)
}

func TestManualCatalogueNeverScansOrDownloadsOnRefresh(t *testing.T) {
	e := newDLEnv(t)
	spy := &materialSpy{API: e.api}
	e.svc.api = spy
	var tr *ThreadResp
	for _, refresh := range []bool{false, true, false} {
		var err error
		tr, err = e.svc.Thread(context.Background(), "omt_mock_002", refresh)
		if err != nil {
			t.Fatal(err)
		}
	}
	if spy.reads.Load() != 0 || spy.exports.Load() != 0 || e.api.dl.Load() != 0 {
		t.Fatal("opening/refreshing a topic fetched cloud materials")
	}
	var file DLEntry
	for _, item := range tr.Downloads {
		if item.FileKey == "boxmockseed01" {
			file = item
		}
	}
	if file.ID == "" || file.Status != dlNotDownloaded {
		t.Fatalf("file catalogue: %+v", tr.Downloads)
	}
	w, _ := e.post("/api/files/download", file.ID)
	if w.Code != 200 {
		t.Fatalf("manual click: %d %s", w.Code, w.Body.String())
	}
	waitEntryByID(t, e.svc.dl, file.ID)
	if spy.reads.Load() != 1 {
		t.Fatalf("expected only selected file download, got %d", spy.reads.Load())
	}
	checked, _ := e.svc.dl.Get(file.ID)
	before, _ := os.ReadFile(checked.Path)
	e.api.SetDriveFile("boxmockseed01", "发版checklist.txt", []byte("new cloud content"), time.Now().Add(time.Hour).UnixMilli())
	if _, err := e.svc.Thread(context.Background(), "omt_mock_002", true); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(checked.Path)
	if string(before) != string(after) || spy.reads.Load() != 1 {
		t.Fatal("topic refresh replaced local content")
	}
	if e.svc.dl.Base() != e.srv.cfg.DownloadDir {
		t.Fatal("download root changed")
	}
}

func TestOnlineExportIsManualAndStoredWithOfficeExtension(t *testing.T) {
	e := newDLEnv(t)
	spy := &materialSpy{API: e.api}
	e.svc.api = spy
	a := classifyAttachment(Attachment{URL: "https://mock.feishu.cn/docx/docmockmanual", Name: "需求文档", MessageID: "om_doc"})
	items := e.svc.dl.Catalog(tc1, []Attachment{a})
	if len(items) != 1 || items[0].Status != dlNotDownloaded || spy.exports.Load() != 0 {
		t.Fatalf("export started during registration: %+v", items)
	}
	if err := e.svc.dl.Redownload(items[0].ID); err != nil {
		t.Fatal(err)
	}
	waitEntryByID(t, e.svc.dl, items[0].ID)
	item, _ := e.svc.dl.Get(items[0].ID)
	if spy.exports.Load() != 1 || !strings.HasSuffix(item.Path, ".docx") || item.ContentHash == "" {
		t.Fatalf("export result: %+v", item)
	}
	for _, candidate := range e.svc.dl.List(tc1.TopicKey) {
		if candidate.ID != item.ID && candidate.Status == dlDone {
			t.Fatal("unselected resource downloaded")
		}
	}
}

func TestManualDownloadCSRFAndRestartDoNotResumeUnrequestedJobs(t *testing.T) {
	e := newDLEnv(t)
	item := e.svc.dl.Catalog(tc1, []Attachment{att("file", "manual.txt", "om_manual", "filemanual")})[0]
	w, _ := e.do("POST", "/api/files/download", fmt.Sprintf(`{"id":%q}`, item.ID), func(r *http.Request) { r.Header.Del(csrfHeader) })
	if w.Code != 403 || e.api.dl.Load() != 0 {
		t.Fatalf("download without CSRF: %d", w.Code)
	}
	dm := e.svc.dl
	dm.mu.Lock()
	dm.entries[item.Key].Status = dlRunning
	dm.saveLocked()
	dm.mu.Unlock()
	loaded := NewDownloadManager(dm.base, dm.hostBase, dm.idxPath, dm.fetch)
	got, _ := loaded.Get(item.ID)
	if got.Status != dlNotDownloaded || len(loaded.flights) != 0 {
		t.Fatalf("unfinished job resumed automatically: %+v", got)
	}
}

func TestExpandFolderDoesNotDownloadAndRejectsUnrelatedFolder(t *testing.T) {
	e := newDLEnv(t)
	_, err := e.api.ReplyMessage(context.Background(), "om_mock_b01", "text", `{"text":"https://mock.feishu.cn/drive/folder/manualroot"}`, "folder-manual", true)
	if err != nil {
		t.Fatal(err)
	}
	tree := &treeAPI{MockAPI: e.api.MockAPI, pages: map[string]DriveListPage{"manualroot:": {Files: []DriveResource{{Token: "boxmockfoldermanual", Type: "file", Name: "手动材料.pdf"}}}}}
	e.svc.api = tree
	if _, err = e.svc.Thread(context.Background(), "omt_mock_002", true); err != nil {
		t.Fatal(err)
	}
	if tree.listCalls != 0 {
		t.Fatal("folder automatically expanded")
	}
	w, _ := e.do("POST", "/api/topics/omt_mock_002/materials/expand", `{"token":"not-in-topic"}`, func(r *http.Request) { r.Header.Set(csrfHeader, e.csrf) })
	if w.Code != 404 {
		t.Fatalf("unrelated folder: %d %s", w.Code, w.Body.String())
	}
	w, _ = e.do("POST", "/api/topics/omt_mock_002/materials/expand", `{"token":"manualroot"}`, func(r *http.Request) { r.Header.Set(csrfHeader, e.csrf) })
	if w.Code != 200 || tree.listCalls != 1 {
		t.Fatalf("explicit expansion: %d %s", w.Code, w.Body.String())
	}
	var tr ThreadResp
	_ = json.Unmarshal(w.Body.Bytes(), &tr)
	found := false
	for _, item := range tr.Downloads {
		if item.FileKey == "boxmockfoldermanual" {
			found = true
			if item.Status != dlNotDownloaded || item.Rel != "" {
				t.Fatalf("expanded file downloaded: %+v", item)
			}
		}
	}
	if !found {
		t.Fatal("expanded folder contents not catalogued")
	}
}

func TestFeishuExportUsesUserIdentityAndDownloadsCompletedJob(t *testing.T) {
	for _, kind := range []string{"docx", "sheet", "bitable", "slides"} {
		t.Run(kind, func(t *testing.T) {
			var creates, downloads atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer u-access" {
					t.Errorf("wrong export identity")
				}
				switch r.URL.Path {
				case "/open-apis/drive/v1/export_tasks":
					creates.Add(1)
					var body map[string]string
					_ = json.NewDecoder(r.Body).Decode(&body)
					if r.Method != "POST" || body["type"] != kind || body["file_extension"] != exportExtension(kind) {
						t.Errorf("export request: %+v", body)
					}
					fmt.Fprint(w, `{"code":0,"data":{"ticket":"one-ticket"}}`)
				case "/open-apis/drive/v1/export_tasks/one-ticket":
					if r.URL.Query().Get("token") != "source-token" {
						t.Errorf("missing source token")
					}
					fmt.Fprint(w, `{"code":0,"data":{"result":{"job_status":0,"file_token":"exported","file_name":"业务资料"}}}`)
				case "/open-apis/drive/v1/export_tasks/file/exported/download":
					downloads.Add(1)
					fmt.Fprint(w, "office bytes")
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer ts.Close()
			cfg := defaultConfig()
			cfg.DataDir = t.TempDir()
			c := NewFeishuClient(ts.URL, "cli_test", "secret", loggedInAuth(t, cfg, "u-access", "refresh", time.Hour), "auto")
			d, err := c.DriveExport(context.Background(), "source-token", kind)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Body.Close()
			data, _ := io.ReadAll(d.Body)
			if string(data) != "office bytes" || !strings.HasSuffix(d.Filename, "."+exportExtension(kind)) || creates.Load() != 1 || downloads.Load() != 1 {
				t.Fatalf("export result: %+v %s", d, data)
			}
		})
	}
}

func TestManualMissingCopyRemainsUndownloaded(t *testing.T) {
	ff := newFakeFetch(t)
	dm, _ := newTestDM(t, ff)
	a := att("file", "keep.txt", "om_keep", "keep")
	item := dm.Catalog(tc1, []Attachment{a})[0]
	if err := dm.Redownload(item.ID); err != nil {
		t.Fatal(err)
	}
	waitEntryByID(t, dm, item.ID)
	item, _ = dm.Get(item.ID)
	if err := os.Remove(item.Path); err != nil {
		t.Fatal(err)
	}
	dm.Catalog(tc1, []Attachment{a})
	missing, _ := dm.Get(item.ID)
	if missing.Status != dlMissing || ff.calls.Load() != 1 {
		t.Fatal("missing copy was automatically downloaded")
	}
	if filepath.Clean(dm.base) == "." {
		t.Fatal("invalid save root")
	}
}

func TestExportFailureOrRateLimitNeverDownloadsOrCreatesAnotherJob(t *testing.T) {
	for _, failure := range []string{"rate-limit", "job-failure", "missing-status"} {
		t.Run(failure, func(t *testing.T) {
			var creates, downloads int
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/export_tasks") {
					creates++
					if failure == "rate-limit" {
						w.WriteHeader(429)
						fmt.Fprint(w, `{"code":99991400,"msg":"rate limited"}`)
						return
					}
					fmt.Fprint(w, `{"code":0,"data":{"ticket":"job"}}`)
					return
				}
				if strings.Contains(r.URL.Path, "/file/") {
					downloads++
					fmt.Fprint(w, "unexpected")
					return
				}
				if failure == "missing-status" {
					fmt.Fprint(w, `{"code":0,"data":{"result":{}}}`)
				} else {
					fmt.Fprint(w, `{"code":0,"data":{"result":{"job_status":110,"job_error_msg":"no permission"}}}`)
				}
			}))
			defer ts.Close()
			cfg := defaultConfig()
			cfg.DataDir = t.TempDir()
			c := NewFeishuClient(ts.URL, "cli_test", "secret", loggedInAuth(t, cfg, "u-access", "refresh", time.Hour), "auto")
			_, err := c.DriveExport(context.Background(), "source", "docx")
			if err == nil || creates != 1 || downloads != 0 {
				t.Fatalf("failure incorrectly retried or downloaded: %v creates=%d downloads=%d", err, creates, downloads)
			}
		})
	}
}
