package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUploadScopesFollowUploadMode(t *testing.T) {
	for _, mode := range []string{"", "auto", "im", "drive"} {
		c := defaultConfig()
		c.UploadMode = mode
		c.Normalize()
		if c.UploadMode != UploadDrive || !containsAll(c.Scopes, DriveNarrowScopes) || containsScope(c.Scopes, "im:resource") {
			t.Fatalf("legacy mode %s: %+v", mode, c.Scopes)
		}
		if c.uploadScopeOK([]string{"im:resource"}) || !c.uploadScopeOK(DriveNarrowScopes) {
			t.Fatal("only private Drive scopes may upload")
		}
	}
	c := defaultConfig()
	c.UploadMode = "off"
	c.DisableDownload = true
	c.Normalize()
	if c.UploadEnabled() || containsScope(c.Scopes, "drive:drive") {
		t.Fatal("off")
	}
	c = defaultConfig()
	c.DisableReply = true
	c.Normalize()
	if c.UploadEnabled() {
		t.Fatal("reply disabled")
	}
	c = defaultConfig()
	c.Scopes = append([]string{}, DriveNarrowScopes...)
	c.Normalize()
	if containsScope(c.Scopes, "drive:drive") {
		t.Fatal("narrow scopes should be respected")
	}
}

func TestPostRowsAndAssemble(t *testing.T) {
	rows := postRows("第一行 @李娜 看 https://example.com/a?b=1，谢谢\n\n第三行", []ReplyMention{{ID: "ou_1", Name: "李娜"}})
	if len(rows) != 3 {
		t.Fatalf("rows: %v", rows)
	}
	var tags []string
	for _, n := range rows[0] {
		tags = append(tags, n["tag"].(string))
	}
	if strings.Join(tags, ",") != "text,at,text,a,text" || rows[0][1]["user_id"] != "ou_1" || rows[0][3]["href"] != "https://example.com/a?b=1" {
		t.Fatalf("row0: %v", rows[0])
	}
	if rows[1][0]["text"] != " " {
		t.Fatalf("blank line: %v", rows[1])
	}
	img := &sendPart{Kind: "image", Key: "img_k1", Name: "a.png"}
	file := &sendPart{Kind: "file", Key: "file_k1", Name: "a.pdf"}
	link := &sendPart{Kind: "link", Name: "big.zip", URL: "https://x.feishu.cn/file/box1"}
	kinds := func(ms []outMsg) string {
		var s []string
		for _, m := range ms {
			s = append(s, m.Type)
		}
		return strings.Join(s, ",")
	}
	if got := kinds(assemble("hi", nil, nil)); got != "text" {
		t.Fatal(got)
	}
	if got := kinds(assemble("", nil, []*sendPart{img})); got != "image" {
		t.Fatal(got)
	}
	if got := kinds(assemble("hi", nil, []*sendPart{img, file, link})); got != "post,file" {
		t.Fatal(got)
	}
	if got := kinds(assemble("", nil, []*sendPart{file, file})); got != "file,file" {
		t.Fatal(got)
	}
	m := assemble("hi", nil, []*sendPart{img, link})[0]
	if !strings.Contains(m.Content, `"image_key":"img_k1"`) || !strings.Contains(m.Content, `"tag":"code_block","text":"https://x.feishu.cn/file/box1"`) || !strings.Contains(m.Content, `"zh_cn"`) {
		t.Fatalf("post content: %s", m.Content)
	}
}

// All upload / send endpoints must be called with the user token and the
// documented request shapes.
func TestFeishuClientUploadAndSendAsUser(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	auth := func(t *testing.T, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer u-access" {
			t.Errorf("%s: Authorization=%q (must be the user token)", r.URL.Path, got)
		}
	}
	form := func(t *testing.T, r *http.Request) (map[string]string, []byte, string) {
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Fatal(err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		fields := map[string]string{}
		var data []byte
		var fname string
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(p)
			if p.FileName() != "" {
				data, fname = b, p.FileName()
			} else {
				fields[p.FormName()] = string(b)
			}
		}
		return fields, data, fname
	}
	ok := func(w http.ResponseWriter, data any) {
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "success", "data": data})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/open-apis/im/v1/images", func(w http.ResponseWriter, r *http.Request) {
		auth(t, r)
		f, data, name := form(t, r)
		if f["image_type"] != "message" || string(data) != "PNGDATA" || name != "p.png" {
			t.Errorf("images: %v %q %q", f, data, name)
		}
		mu.Lock()
		seen["image"] = true
		mu.Unlock()
		ok(w, map[string]string{"image_key": "img_v2_1"})
	})
	mux.HandleFunc("/open-apis/im/v1/files", func(w http.ResponseWriter, r *http.Request) {
		auth(t, r)
		f, data, _ := form(t, r)
		if f["file_type"] != "pdf" || f["file_name"] != "报告.pdf" || string(data) != "PDFDATA" {
			t.Errorf("files: %v %q", f, data)
		}
		mu.Lock()
		seen["file"] = true
		mu.Unlock()
		ok(w, map[string]string{"file_key": "file_v2_1"})
	})
	mux.HandleFunc("/open-apis/im/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		auth(t, r)
		var body map[string]string
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		if r.URL.Query().Get("receive_id_type") != "chat_id" || body["receive_id"] != "oc_1" || body["msg_type"] != "text" || body["uuid"] != "u-9" {
			t.Errorf("send: %v %v", r.URL.Query(), body)
		}
		mu.Lock()
		seen["send"] = true
		mu.Unlock()
		ok(w, map[string]any{"message_id": "om_root1", "msg_type": "text", "chat_id": "oc_1", "create_time": "1700000000000"})
	})
	mux.HandleFunc("/open-apis/drive/explorer/v2/root_folder/meta", func(w http.ResponseWriter, r *http.Request) {
		auth(t, r)
		mu.Lock()
		seen["root"] = true
		mu.Unlock()
		ok(w, map[string]string{"token": "fld_root"})
	})
	mux.HandleFunc("/open-apis/drive/v1/files/upload_all", func(w http.ResponseWriter, r *http.Request) {
		auth(t, r)
		f, data, _ := form(t, r)
		if f["parent_type"] != "explorer" || f["parent_node"] != "fld_root" || f["file_name"] != "big.bin" || f["size"] != "3" || string(data) != "abc" {
			t.Errorf("upload_all: %v %q", f, data)
		}
		mu.Lock()
		seen["drive"] = true
		mu.Unlock()
		ok(w, map[string]string{"file_token": "box_1"})
	})
	mux.HandleFunc("/open-apis/drive/v1/metas/batch_query", func(w http.ResponseWriter, r *http.Request) {
		auth(t, r)
		var body struct {
			Docs []map[string]string `json:"request_docs"`
			URL  bool                `json:"with_url"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		if !body.URL || len(body.Docs) != 1 || body.Docs[0]["doc_token"] != "box_1" || body.Docs[0]["doc_type"] != "file" {
			t.Errorf("batch_query: %s", b)
		}
		ok(w, map[string]any{"metas": []map[string]string{{"url": "https://x.feishu.cn/file/box_1"}}})
	})
	mux.HandleFunc("/open-apis/drive/v1/permissions/box_1/members", func(w http.ResponseWriter, r *http.Request) {
		auth(t, r)
		var body map[string]string
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		if r.Method != http.MethodPost || r.URL.Query().Get("type") != "file" || r.URL.Query().Get("need_notification") != "false" || body["member_type"] != "openchat" || body["member_id"] != "oc_1" || body["perm"] != "view" || body["type"] != "chat" {
			t.Errorf("members: %s %v %s", r.Method, r.URL.Query(), b)
		}
		mu.Lock()
		seen["share"] = true
		mu.Unlock()
		ok(w, map[string]any{"member": map[string]string{"member_type": "openchat", "member_id": "oc_1", "perm": "view", "type": "chat"}})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	cfg := defaultConfig()
	cfg.AppID, cfg.AppSecret, cfg.BaseURL = "cli_test", "secret", ts.URL
	c := NewFeishuClient(ts.URL, "cli_test", "secret", loggedInAuth(t, cfg, "u-access", "r1", time.Hour), "auto")
	c.gap = 0
	ctx := context.Background()
	if k, err := c.UploadIM(ctx, "image", "p.png", []byte("PNGDATA")); err != nil || k != "img_v2_1" {
		t.Fatalf("image: %q %v", k, err)
	}
	if k, err := c.UploadIM(ctx, "file", "报告.pdf", []byte("PDFDATA")); err != nil || k != "file_v2_1" {
		t.Fatalf("file: %q %v", k, err)
	}
	if m, err := c.SendMessage(ctx, "oc_1", "text", `{"text":"hi"}`, "u-9"); err != nil || m.MessageID != "om_root1" {
		t.Fatalf("send: %+v %v", m, err)
	}
	if _, err := c.rootFolder(ctx); err != nil {
		t.Fatal(err)
	}
	tok, err := c.driveUploadToFolder(ctx, "fld_root", "big.bin", []byte("abc"))
	if err != nil || tok != "box_1" {
		t.Fatalf("drive: %q %v", tok, err)
	}
	if _, err := c.driveUploadToFolder(ctx, "fld_root", "x", make([]byte, DriveMaxBytes+1)); err == nil {
		t.Fatal("drive upload must refuse > 20 MB")
	}
	if u, err := c.DriveURL(ctx, tok); err != nil || u != "https://x.feishu.cn/file/box_1" {
		t.Fatalf("url: %q %v", u, err)
	}
	if err := c.DriveShareChat(ctx, tok, "not-a-chat"); err == nil || seen["share"] {
		t.Fatal("group grant must reject a non-chat id before any request")
	}
	if err := c.DriveShareChat(ctx, tok, "oc_1"); err != nil || !seen["share"] {
		t.Fatalf("explicit group grant: %v", err)
	}
	for _, k := range []string{"image", "file", "send", "root", "drive"} {
		if !seen[k] {
			t.Errorf("endpoint %s not called", k)
		}
	}
}

func rawReq(t *testing.T, h http.Handler, method, path, ctype string, body []byte, hdr map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Host = "localhost:8080"
	req.Header.Set("Content-Type", ctype)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func stage(t *testing.T, h http.Handler, name string, data []byte) map[string]any {
	t.Helper()
	rec, out := rawReq(t, h, "POST", "/api/upload", "application/octet-stream", data, map[string]string{"X-File-Name": strings.ReplaceAll(name, " ", "%20")})
	if rec.Code != 200 {
		t.Fatalf("stage %s: %d %s", name, rec.Code, rec.Body.String())
	}
	return out
}

var pngBytes = mockPNG("test")

func TestNewTopicPendingThenReply(t *testing.T) {
	srv, svc := newTestServer(t)
	h := srv.Handler()
	ctx := context.Background()
	before, _ := svc.Topics(ctx, "oc_mock_empty", false)
	if len(before.Topics) != 0 {
		t.Fatalf("empty chat has topics: %d", len(before.Topics))
	}
	rec, out := jsonReq(t, h, "POST", "/api/topics/new", `{"chat_id":"oc_mock_empty","text":"大家好，讨论一下 @李娜 的方案","mentions":[{"id":"ou_mock_lina","name":"李娜"}],"uuid":"nt-1"}`)
	if rec.Code != 200 {
		t.Fatalf("new topic: %d %s", rec.Code, rec.Body.String())
	}
	topic := out["topic"].(map[string]any)
	tid := out["thread_id"].(string)
	if !strings.HasPrefix(tid, "new_om_") || topic["pending"] != true || topic["reply_count"].(float64) != 0 || topic["thread_id"] != tid {
		t.Fatalf("topic: %v thread=%s", topic, tid)
	}
	// the chat's topic list now contains the pending topic (the thread scan alone would not)
	list, err := svc.Topics(ctx, "oc_mock_empty", true)
	if err != nil || len(list.Topics) != 1 || list.Topics[0].ThreadID != tid || !list.Topics[0].Pending || !strings.Contains(list.Topics[0].Title, "大家好") {
		t.Fatalf("topics: %+v %v", list, err)
	}
	// same uuid: no second message
	_, out2 := jsonReq(t, h, "POST", "/api/topics/new", `{"chat_id":"oc_mock_empty","text":"大家好，讨论一下 @李娜 的方案","mentions":[{"id":"ou_mock_lina","name":"李娜"}],"uuid":"nt-1"}`)
	if out2["thread_id"] != tid {
		t.Fatalf("uuid dedupe: %v", out2["thread_id"])
	}
	// opening the pending topic shows the root message with 0 replies
	th, err := svc.Thread(ctx, tid, false)
	if err != nil || len(th.Messages) != 1 || !th.Topic.Pending || !th.Messages[0].IsRoot || th.Topic.ReplyCount != 0 {
		t.Fatalf("pending thread: %+v %v", th, err)
	}
	// first reply creates the real thread
	rec, out = jsonReq(t, h, "POST", "/api/topics/"+tid+"/reply", `{"text":"我先说两点"}`)
	if rec.Code != 200 {
		t.Fatalf("reply: %d %s", rec.Code, rec.Body.String())
	}
	real := out["thread_id"].(string)
	if real == tid || strings.HasPrefix(real, "new_") {
		t.Fatalf("thread id after reply: %s", real)
	}
	list, _ = svc.Topics(ctx, "oc_mock_empty", true)
	if len(list.Topics) != 1 || list.Topics[0].ThreadID != real || list.Topics[0].Pending || list.Topics[0].ReplyCount != 1 {
		t.Fatalf("topics after reply: %+v", list.Topics)
	}
	if left := svc.pendingList("oc_mock_empty"); len(left) != 0 {
		t.Fatalf("pending entry not cleaned up: %+v", left)
	}
	// the stale pending id redirects to the real thread
	th, err = svc.Thread(ctx, tid, false)
	if err != nil || th.Topic.ThreadID != real || len(th.Messages) != 2 {
		t.Fatalf("redirect: %+v %v", th, err)
	}
	// further replies work on the real id
	if rec, _ := jsonReq(t, h, "POST", "/api/topics/"+real+"/reply", `{"text":"再补充"}`); rec.Code != 200 {
		t.Fatalf("second reply: %d", rec.Code)
	}
}

func TestNewTopicValidationAndErrors(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	for body, want := range map[string]int{
		`{"chat_id":"oc_mock_dev","text":"  "}`:                         400,
		`{"chat_id":"bad id","text":"x"}`:                               400,
		`{"chat_id":"oc_mock_dev","text":"x","reply_to":"bad id"}`:      400,
		`{"chat_id":"oc_mock_dev","text":"x","reply_to":"om_nope"}`:     502,
		`{"chat_id":"oc_mock_dev","text":"x","reply_to":"om_mock_h01"}`: 400, // message of another chat
		`{"chat_id":"oc_mock_dev","text":"x","attachments":["zzz"]}`:    410,
		`nope`: 400,
	} {
		if rec, _ := jsonReq(t, h, "POST", "/api/topics/new", body); rec.Code != want {
			t.Errorf("%s -> %d, want %d", body, rec.Code, want)
		}
	}
	rec, out := jsonReq(t, h, "POST", "/api/topics/new", `{"chat_id":"oc_mock_dev","text":"!scope"}`)
	if rec.Code != 502 || out["kind"] != "scope_missing" {
		t.Fatalf("scope: %d %v", rec.Code, out)
	}
	if rec, out := jsonReq(t, h, "POST", "/api/topics/new", `{"chat_id":"oc_unknown","text":"x"}`); rec.Code != 502 || out["code"].(float64) != 230002 {
		t.Fatalf("unknown chat: %d %v", rec.Code, out)
	}
}

func TestStartTopicUnderExistingMessage(t *testing.T) {
	srv, svc := newTestServer(t)
	h := srv.Handler()
	ctx := context.Background()
	rec, out := jsonReq(t, h, "GET", "/api/chats/oc_mock_dev/recent", "")
	if rec.Code != 200 {
		t.Fatalf("recent: %d", rec.Code)
	}
	msgs := out["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("no flat messages offered")
	}
	first := msgs[0].(map[string]any)
	if first["id"] != "om_mock_p1" || !strings.Contains(first["text"].(string), "站会") {
		t.Fatalf("recent: %v", first)
	}
	for _, m := range msgs { // thread roots are already topics
		if strings.HasPrefix(m.(map[string]any)["id"].(string), "om_mock_a0") {
			t.Fatalf("thread message offered: %v", m)
		}
	}
	before, _ := svc.Topics(ctx, "oc_mock_dev", false)
	rec, out = jsonReq(t, h, "POST", "/api/topics/new", `{"chat_id":"oc_mock_dev","reply_to":"om_mock_p1","text":"补充一下站会议程"}`)
	if rec.Code != 200 {
		t.Fatalf("new under message: %d %s", rec.Code, rec.Body.String())
	}
	tid := out["thread_id"].(string)
	topic := out["topic"].(map[string]any)
	if strings.HasPrefix(tid, "new_") || topic["pending"] == true || topic["root_message_id"] != "om_mock_p1" || topic["reply_count"].(float64) != 1 {
		t.Fatalf("topic: %v tid=%s", topic, tid)
	}
	after, _ := svc.Topics(ctx, "oc_mock_dev", true)
	if len(after.Topics) != len(before.Topics)+1 {
		t.Fatalf("topics %d -> %d", len(before.Topics), len(after.Topics))
	}
	th, err := svc.Thread(ctx, tid, true)
	if err != nil || len(th.Messages) != 2 || th.Messages[0].ID != "om_mock_p1" {
		t.Fatalf("thread: %+v %v", th, err)
	}
}

func TestComposeWithAttachmentsMock(t *testing.T) {
	srv, svc := newTestServer(t)
	h := srv.Handler()
	img := stage(t, h, "屏幕 截图.png", pngBytes)
	file := stage(t, h, "方案.pdf", []byte("%PDF-1.4 hi"))
	rec, out := jsonReq(t, h, "POST", "/api/topics/new", `{"chat_id":"oc_mock_proj","text":"看图和文档","attachments":["`+img["upload_id"].(string)+`","`+file["upload_id"].(string)+`"]}`)
	if rec.Code != 200 {
		t.Fatalf("send: %d %v", rec.Code, out)
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["msg_type"] != "post" {
		t.Fatalf("attachments must travel together as private links: %v", msgs)
	}
	text := jc(msgs[0])
	if strings.Count(msgs[0].(map[string]any)["plain"].(string), "https://mock.feishu.cn/file/") != 2 || strings.Contains(text, `"type":"image"`) {
		t.Fatalf("private links: %s", text)
	}
	if _, _, err := svc.loadStaged(img["upload_id"].(string)); err == nil {
		t.Fatal("sent stage was not removed")
	}
	ma := svc.api.(*MockAPI)
	if len(ma.SharedChats()) != 0 || len(ma.SharedMembers()) != 2 {
		t.Fatal("wrong recipients")
	}
}

func TestUploadFallbackChain(t *testing.T) {
	srv, svc := newTestServer(t)
	h := srv.Handler()
	for _, mode := range []string{UploadAuto, UploadIM, UploadDrive} {
		svc.cfg.UploadMode = mode
		st := stage(t, h, "plain.png", pngBytes)
		rec, out := jsonReq(t, h, "POST", "/api/topics/omt_mock_002/reply", `{"text":"x","attachments":["`+st["upload_id"].(string)+`"]}`)
		if rec.Code != 200 || out["message"].(map[string]any)["msg_type"] != "post" {
			t.Fatalf("%s bypassed private Drive: %d %v", mode, rec.Code, out)
		}
	}
	st := stage(t, h, "noshare.png", pngBytes)
	rec, out := jsonReq(t, h, "POST", "/api/topics/omt_mock_002/reply", `{"text":"x","attachments":["`+st["upload_id"].(string)+`"]}`)
	if rec.Code == 200 {
		t.Fatalf("permission failure must block the message: %v", out)
	}
	if _, _, err := svc.loadStaged(st["upload_id"].(string)); err != nil {
		t.Fatal("retry stage lost")
	}
	ma := svc.api.(*MockAPI)
	if len(ma.SharedChats()) != 0 {
		t.Fatal("chat shared")
	}
}

func TestUploadLimitsAndSwitches(t *testing.T) {
	srv, svc := newTestServer(t)
	h := srv.Handler()
	// > 30 MB
	rec, out := rawReq(t, h, "POST", "/api/upload", "application/octet-stream", make([]byte, FileMaxBytes+10), map[string]string{"X-File-Name": "big.bin"})
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(out["error"].(string), "30 MB") {
		t.Fatalf("too large: %d %v", rec.Code, out)
	}
	// empty
	if rec, _ := rawReq(t, h, "POST", "/api/upload", "application/octet-stream", nil, map[string]string{"X-File-Name": "e.txt"}); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("empty: %d", rec.Code)
	}
	// image 10..30 MB is sent as a file
	big := append(append([]byte{}, pngBytes...), make([]byte, ImageMaxBytes)...)
	st := stage(t, h, "huge.png", big)
	if st["kind"] != "file" || st["note"] == "" {
		t.Fatalf("big image: %v", st)
	}
	// 20..30 MB is refused when only Drive is allowed
	svc.cfg.UploadMode = UploadDrive
	if rec, _ := rawReq(t, h, "POST", "/api/upload", "application/octet-stream", make([]byte, DriveMaxBytes+1), map[string]string{"X-File-Name": "d.bin"}); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("drive limit: %d", rec.Code)
	}
	// delete a staged upload
	st2 := stage(t, h, "k.txt", []byte("hello"))
	id := st2["upload_id"].(string)
	if rec, _ := rawReq(t, h, "DELETE", "/api/upload/"+id, "application/json", []byte("{}"), nil); rec.Code != 200 {
		t.Fatalf("delete: %d", rec.Code)
	}
	if _, _, err := svc.loadStaged(id); err == nil {
		t.Fatal("not deleted")
	}
	// off
	svc.cfg.UploadMode = UploadOff
	if rec, _ := rawReq(t, h, "POST", "/api/upload", "application/octet-stream", []byte("x"), map[string]string{"X-File-Name": "k.txt"}); rec.Code != http.StatusForbidden {
		t.Fatalf("off: %d", rec.Code)
	}
	// the raw upload content type is only accepted on /api/upload
	if rec, _ := rawReq(t, h, "POST", "/api/topics/new", "application/octet-stream", []byte(`{}`), nil); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("content-type: %d", rec.Code)
	}
	// foreign origin is refused
	if rec, _ := rawReq(t, h, "POST", "/api/upload", "application/octet-stream", []byte("x"), map[string]string{"Origin": "http://evil.example", "X-File-Name": "k"}); rec.Code != http.StatusForbidden {
		t.Fatalf("origin: %d", rec.Code)
	}
	svc.auth.Logout()
	if rec, out := rawReq(t, h, "POST", "/api/upload", "application/octet-stream", []byte("x"), map[string]string{"X-File-Name": "k"}); rec.Code != 401 || out["kind"] != "login_required" {
		t.Fatalf("logged out: %d %v", rec.Code, out)
	}
}

func TestAuthStatusReportsUpload(t *testing.T) {
	srv, svc := newTestServer(t)
	_, out := jsonReq(t, srv.Handler(), "GET", "/api/auth/status", "")
	if out["upload_enabled"] != true || out["can_upload"] != true || out["upload_mode"] != "drive" {
		t.Fatalf("status: %v", out)
	}
	if lim := out["limits"].(map[string]any); lim["image"].(float64) != ImageMaxBytes || lim["file"].(float64) != FileMaxBytes || lim["drive"].(float64) != DriveMaxBytes {
		t.Fatalf("limits: %v", lim)
	}
	// a token without any upload scope -> can_upload=false (re-login needed)
	svc.auth.mu.Lock()
	svc.auth.tok.Scope = "offline_access im:message im:message.send_as_user"
	svc.auth.mu.Unlock()
	_, out = jsonReq(t, srv.Handler(), "GET", "/api/auth/status", "")
	if out["can_upload"] != false || out["can_reply"] != true {
		t.Fatalf("no upload scope: %v", out)
	}
}

type mockTopicDriveAPI struct {
	*MockAPI
	folders []string
}

func (m *mockTopicDriveAPI) DriveUploadToTopic(ctx context.Context, topicFolder, name string, data []byte) (string, string, error) {
	m.folders = append(m.folders, topicFolder)
	tok, err := m.MockAPI.DriveUpload(ctx, name, data)
	if err != nil {
		return "", "", err
	}
	return tok, DrivePrivateRootName + "/" + topicFolder, nil
}

func TestDriveTopicUploaderSingleMessage(t *testing.T) {
	srv, svc := newTestServer(t)
	td := &mockTopicDriveAPI{MockAPI: NewMockAPI(svc.auth)}
	svc.api = td
	h := srv.Handler()

	f1 := stage(t, h, "需求文档.pdf", []byte("%PDF-1.4 test"))["upload_id"].(string)
	f2 := stage(t, h, "截图.png", pngBytes)["upload_id"].(string)
	rec, out := jsonReq(t, h, "POST", "/api/topics/new", `{"chat_id":"oc_mock_dev","reply_to":"om_mock_p1","text":"请帮我看看这两个附件","attachments":["`+f1+`","`+f2+`"]}`)
	if rec.Code != 200 {
		t.Fatalf("reply status=%d out=%v", rec.Code, out)
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 single message for text+file+image, got %d", len(msgs))
	}
	m := msgs[0].(map[string]any)
	if m["msg_type"] != "post" {
		t.Fatalf("expected post msg_type, got %v", m["msg_type"])
	}
	plain := m["plain"].(string)
	if !strings.Contains(plain, "请帮我看看这两个附件") || !strings.Contains(plain, "需求文档.pdf") || !strings.Contains(plain, DrivePrivateRootName+"/") {
		t.Fatalf("unexpected plain text: %s", plain)
	}
	if len(td.folders) != 2 || td.folders[0] == "" || td.folders[0] != td.folders[1] {
		t.Fatalf("expected both attachments in same topic folder, got %v", td.folders)
	}
}

func TestReplyQueueWhenDaidainiaoRunning(t *testing.T) {
	srv, svc := newTestServer(t)
	ma := NewMockAPI(svc.auth)
	svc.api = ma
	h := srv.Handler()

	// Inject a running Claude Code card into omt_mock_101
	runningCard := `{"title":"🤖 Claude Code · 运行中","elements":[[{"tag":"md","text":"状态 🤖 运行中 · 开始于 22:30:00 · 已用时 15秒\n当前动作 💻 执行命令: go test ./..."}]]}`
	ma.mu.Lock()
	rm := RawMessage{
		MessageID:  "om_claude_run_1",
		RootID:     "om_mock_100",
		ParentID:   "om_mock_100",
		ThreadID:   "omt_mock_101",
		ChatID:     "oc_mock_dev",
		CreateTime: strconv.FormatInt(time.Now().UnixMilli(), 10),
		MsgType:    "interactive",
	}
	rm.Sender.ID = "cli_daidainiao"
	rm.Sender.IDType = "app_id"
	rm.Sender.SenderType = "app"
	rm.Body.Content = runningCard
	ma.threads["omt_mock_101"] = append(ma.threads["omt_mock_101"], rm)
	ma.mu.Unlock()

	// Stage a file and send a reply while Daidainiao is running -> should be queued!
	f1 := stage(t, h, "补充资料.txt", []byte("queued attachment"))["upload_id"].(string)
	rec, out := jsonReq(t, h, "POST", "/api/topics/omt_mock_101/reply", `{"text":"等跑完帮我再看下这个","attachments":["`+f1+`"]}`)
	if rec.Code != 200 {
		t.Fatalf("reply status=%d out=%v", rec.Code, out)
	}
	if out["queued"] != true || out["bot_busy"] != true {
		t.Fatalf("expected queued=true and bot_busy=true, got %v", out)
	}
	qList := out["queue"].([]any)
	if len(qList) != 1 {
		t.Fatalf("expected 1 item in queue, got %d", len(qList))
	}
	qItem := qList[0].(map[string]any)
	qid := qItem["id"].(string)

	// Staged file must NOT be pruned while referenced by queue
	if _, _, err := svc.loadStaged(f1); err != nil {
		t.Fatalf("staged file should be preserved while queued: %v", err)
	}

	// Enqueue a second message, then test cancel on it
	rec2, out2 := jsonReq(t, h, "POST", "/api/topics/omt_mock_101/reply", `{"text":"第二条排队消息"}`)
	if rec2.Code != 200 || out2["queued"] != true {
		t.Fatalf("second reply should also queue: %v", out2)
	}
	qList2 := out2["queue"].([]any)
	if len(qList2) != 2 {
		t.Fatalf("expected 2 items in queue, got %d", len(qList2))
	}
	qid2 := qList2[1].(map[string]any)["id"].(string)

	// Cancel the second queued message
	recCancel, outCancel := jsonReq(t, h, "DELETE", "/api/topics/omt_mock_101/queue/"+qid2, `{}`)
	if recCancel.Code != 200 || len(outCancel["queue"].([]any)) != 1 {
		t.Fatalf("cancel failed: %d %v", recCancel.Code, outCancel)
	}

	// Now update Daidainiao's card to finished ("✅ Claude Code · 运行结束")
	finishedCard := `{"title":"✅ Claude Code · 运行结束","elements":[[{"tag":"md","text":"状态 ✅ 已完成 · 开始于 22:30:00 · 结束于 22:31:00\n本轮已结束，可直接在话题里继续对话"}]]}`
	ma.mu.Lock()
	msgs := ma.threads["omt_mock_101"]
	for i := range msgs {
		if msgs[i].MessageID == "om_claude_run_1" {
			msgs[i].Body.Content = finishedCard
		}
	}
	// Avoid the 1.5s transition sleep in unit test by marking wasBusy=false
	svc.initQueue().wasBusy["omt_mock_101"] = false
	ma.mu.Unlock()

	// Trigger queue check -> should dispatch qid automatically!
	svc.checkAndDispatchThread("omt_mock_101")
	if got := svc.QueueFor("omt_mock_101"); len(got) != 0 {
		t.Fatalf("expected queue to be empty after dispatch, got %+v (id=%s)", got, qid)
	}

	// Verify the message was actually appended to the Feishu thread
	tr, err := svc.Thread(context.Background(), "omt_mock_101", true)
	if err != nil {
		t.Fatalf("Thread: %v", err)
	}
	if !tr.BotBusy {
		t.Fatalf("expected BotBusy=true during post-send startup grace window right after dispatch")
	}
	found := false
	for _, m := range tr.Messages {
		if strings.Contains(m.Plain, "等跑完帮我再看下这个") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected queued message to be sent into thread, msgs=%+v", tr.Messages)
	}
}

func TestDriveShareModes(t *testing.T) {
	cases := []struct {
		mode             string
		expectChats      bool
		expectMember     bool
		expectLinkClosed bool
	}{
		{DriveShareBotOnly, false, true, true},
		{DriveShareChat, false, true, true},
		{DriveShareAll, false, true, true},
		{DriveShareNone, false, true, true},
	}

	for _, tc := range cases {
		t.Run("mode_"+tc.mode, func(t *testing.T) {
			srv, svc := newTestServer(t)
			ma := NewMockAPI(svc.auth)
			svc.api = ma
			svc.cfg.UploadMode = UploadDrive
			svc.cfg.DriveShareMode = tc.mode
			h := srv.Handler()

			f := stage(t, h, "测试文档.pdf", []byte("%PDF-1.4 mode test"))["upload_id"].(string)
			rec, out := jsonReq(t, h, "POST", "/api/topics/new", `{"chat_id":"oc_mock_dev","reply_to":"om_mock_p1","text":"分享测试","attachments":["`+f+`"]}`)
			if rec.Code != 200 {
				t.Fatalf("reply status=%d out=%v", rec.Code, out)
			}

			chats := ma.SharedChats()
			members := ma.SharedMembers()

			if tc.expectChats && len(chats) == 0 {
				t.Fatalf("expected chat to be shared, got 0")
			}
			if !tc.expectChats && len(chats) > 0 {
				t.Fatalf("expected chat NOT to be shared, got %v", chats)
			}

			if tc.expectMember && len(members) == 0 {
				t.Fatalf("expected member (bot) to be shared, got 0")
			}
			if !tc.expectMember && len(members) > 0 {
				t.Fatalf("expected member NOT to be shared, got %v", members)
			}

			var uploadToken string
			ma.mu.RLock()
			for tok := range ma.linkShares {
				uploadToken = tok
				break
			}
			ma.mu.RUnlock()

			if tc.expectLinkClosed && (uploadToken == "" || ma.LinkShare(uploadToken) != "closed") {
				t.Fatalf("expected link share to be 'closed', got %q", ma.LinkShare(uploadToken))
			}
			if !tc.expectLinkClosed && ma.LinkShare(uploadToken) == "closed" {
				t.Fatalf("did not expect link share to be closed")
			}
		})
	}
}
