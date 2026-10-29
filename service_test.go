package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*Server, *Service) {
	t.Helper()
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.DataDir = dir
	cfg.Normalize()
	auth := NewAuth(cfg, true, filepath.Join(dir, "token.json"))
	if err := auth.Callback(context.Background(), "mock"); err != nil {
		t.Fatal(err)
	}
	svc := NewService(NewMockAPI(auth), NewCache(filepath.Join(dir, "cache")), cfg, auth)
	t.Cleanup(func() {
		svc.Close()
		waitTopicFiles(t, svc)
	})
	sel, _, err := LoadSelection(filepath.Join(dir, "selection.json"))
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(svc, sel, cfg, true, nil), svc
}

func TestTopicsAndThread(t *testing.T) {
	_, svc := newTestServer(t)
	ctx := context.Background()
	resp, err := svc.Topics(ctx, "oc_mock_dev", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Topics) != 3 {
		t.Fatalf("want 3 topics (non-thread messages excluded), got %d", len(resp.Topics))
	}
	for i := 1; i < len(resp.Topics); i++ {
		if resp.Topics[i-1].LastTime < resp.Topics[i].LastTime {
			t.Fatal("topics not sorted by last activity")
		}
	}
	th, err := svc.Thread(ctx, "omt_mock_001", false)
	if err != nil {
		t.Fatal(err)
	}
	if th.Topic.ReplyCount != 5 || len(th.Messages) != 6 || !th.Messages[0].IsRoot {
		t.Fatalf("thread: replies=%d msgs=%d", th.Topic.ReplyCount, len(th.Messages))
	}
	for i := 1; i < len(th.Messages); i++ {
		if th.Messages[i-1].Time > th.Messages[i].Time {
			t.Fatal("messages not chronological")
		}
	}
	if th.Messages[0].SenderName != "张伟" {
		t.Fatalf("sender not resolved: %q", th.Messages[0].SenderName)
	}
	if len(th.Attachments) != 4 {
		t.Fatalf("attachments=%d", len(th.Attachments))
	}
}

func TestHTTPEndpoints(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Host = "localhost:8080"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if w := get("/api/chats"); w.Code != 200 {
		t.Fatalf("chats %d", w.Code)
	}
	// selection round trip
	req := httptest.NewRequest("POST", "/api/selection", stringsReader(`{"chat_ids":["oc_mock_dev","oc_mock_dev"," "]}`))
	req.Host = "localhost:8080"
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var sel struct {
		ChatIDs []string `json:"chat_ids"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &sel)
	if w.Code != 200 || len(sel.ChatIDs) != 1 {
		t.Fatalf("selection: %d %s", w.Code, w.Body.String())
	}
	if w := get("/api/resource?message_id=../x&file_key=a&type=file"); w.Code != http.StatusBadRequest {
		t.Fatalf("path traversal not rejected: %d", w.Code)
	}
	w = get("/api/resource?message_id=om_mock_b03&file_key=file_mock_pdf&type=file&name=a.pdf")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/pdf" {
		t.Fatalf("pdf: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	req = httptest.NewRequest("GET", "/api/status", nil)
	req.Host = "evil.example"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("foreign host allowed: %d", w.Code)
	}
}

func stringsReader(s string) *strings.Reader { return strings.NewReader(s) }

// Feishu's thread container omits the opening message. The list and the open
// conversation must still title the topic from that message, not from the
// bot's "正在思考" reply.
func TestOmittedThreadRootRestored(t *testing.T) {
	_, svc := newTestServer(t)
	api := svc.api.(*MockAPI)
	ctx := context.Background()
	const (
		chat   = "oc_mock_think"
		thread = "omt_mock_think"
		rootID = "om_mock_think_root"
	)
	root := RawMessage{MessageID: rootID, MsgType: "text", CreateTime: "1000", UpdateTime: "1000", ChatID: chat, ThreadID: thread}
	root.Body.Content = jc(map[string]string{"text": "@_user_1 整理汇总下编制管理相关的材料。"})
	root.Sender.ID, root.Sender.IDType, root.Sender.SenderType = uZhang, "open_id", "user"
	root.Mentions = []Mention{{Key: "@_user_1", ID: appBot, IDType: "app_id", Name: "呆呆鸟"}}
	think := RawMessage{MessageID: "om_mock_think_1", RootID: rootID, ParentID: rootID, MsgType: "text", CreateTime: "2000", UpdateTime: "2000", ChatID: chat, ThreadID: thread}
	think.Body.Content = jc(map[string]string{"text": "🤖 正在思考…"})
	think.Sender.ID, think.Sender.IDType, think.Sender.SenderType = appBot, "app_id", "app"
	done := RawMessage{MessageID: "om_mock_think_2", RootID: rootID, ParentID: rootID, MsgType: "text", CreateTime: "3000", UpdateTime: "3000", ChatID: chat, ThreadID: thread}
	done.Body.Content = jc(map[string]string{"text": "已完成，用时 33 秒。"})
	done.Sender = think.Sender

	api.mu.Lock()
	api.threads[thread] = []RawMessage{root, think, done}
	api.chatMsgs[chat] = []RawMessage{root}
	api.hideThreadRoot = map[string]bool{thread: true}
	api.mu.Unlock()

	th, err := svc.Thread(ctx, thread, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(th.Messages) != 3 || !th.Messages[0].IsRoot || th.Messages[0].SenderName != "张伟" {
		t.Fatalf("open thread: msgs=%d root=%v sender=%q", len(th.Messages), len(th.Messages) > 0 && th.Messages[0].IsRoot, th.Messages[0].SenderName)
	}
	if !strings.Contains(th.Topic.Title, "整理汇总") || strings.Contains(th.Topic.Title, "正在思考") {
		t.Fatalf("open title=%q", th.Topic.Title)
	}

	th2, err := svc.Thread(ctx, thread, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(th2.Messages) != 3 || !strings.Contains(th2.Topic.Title, "整理汇总") || th2.Topic.SenderName != "张伟" {
		t.Fatalf("refresh dropped the opening message: msgs=%d title=%q sender=%q", len(th2.Messages), th2.Topic.Title, th2.Topic.SenderName)
	}

	// A list load must repair a thread cache that was stored without the opening message.
	svc.cache.Set(svc.k("thread:"+thread), []RawMessage{think, done})
	list, err := svc.Topics(ctx, chat, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Topics) != 1 || list.Topics[0].SenderName != "张伟" || !strings.Contains(list.Topics[0].Title, "整理汇总") || strings.Contains(list.Topics[0].Title, "正在思考") {
		t.Fatalf("list title=%q sender=%q n=%d", list.Topics[0].Title, list.Topics[0].SenderName, len(list.Topics))
	}
}

func TestMyTopicsAndAPI(t *testing.T) {
	srv, svc := newTestServer(t)
	// In mock mode, logged in user is uLi (李娜)
	res, err := svc.MyTopics(context.Background(), true)
	if err != nil {
		t.Fatalf("MyTopics: %v", err)
	}
	if len(res.Topics) == 0 {
		t.Fatalf("expected at least 1 topic related to uLi, got 0")
	}
	// Verify all returned topics are related to uLi and sorted descending
	for i, tp := range res.Topics {
		if !tp.UserInvolved && tp.SenderID != uLi {
			t.Errorf("topic %s (%s) expected related to %s", tp.ThreadID, tp.Title, uLi)
		}
		if i > 0 && res.Topics[i].LastTime > res.Topics[i-1].LastTime {
			t.Errorf("topics not sorted in descending order at index %d", i)
		}
	}

	// Test HTTP endpoint GET /api/topics/my
	h := srv.Handler()
	rec, out := jsonReq(t, h, "GET", "/api/topics/my", "")
	if rec.Code != 200 {
		t.Fatalf("GET /api/topics/my status=%d: %v", rec.Code, out)
	}
	topics, ok := out["topics"].([]any)
	if !ok || len(topics) != len(res.Topics) {
		t.Fatalf("mismatch in topics count: got %d, expected %d", len(topics), len(res.Topics))
	}

	// Test GET /api/topics?my=1 query param
	rec2, out2 := jsonReq(t, h, "GET", "/api/topics?my=1", "")
	if rec2.Code != 200 {
		t.Fatalf("GET /api/topics?my=1 status=%d: %v", rec2.Code, out2)
	}
	topics2, ok2 := out2["topics"].([]any)
	if !ok2 || len(topics2) != len(res.Topics) {
		t.Fatalf("mismatch in ?my=1 topics count: got %d, expected %d", len(topics2), len(res.Topics))
	}
}

func TestTopicAutoRefreshTTL(t *testing.T) {
	_, svc := newTestServer(t)
	api := svc.api.(*MockAPI)
	ctx := context.Background()

	// Initial fetch
	res1, err := svc.Topics(ctx, "oc_mock_dev", false)
	if err != nil {
		t.Fatal(err)
	}
	initCount := len(res1.Topics)

	// Inject a new topic message into mock chat
	const newThread = "omt_mock_auto_refresh"
	const newMsgID = "om_mock_auto_refresh_root"
	newRoot := RawMessage{
		MessageID:  newMsgID,
		ThreadID:   newThread,
		ChatID:     "oc_mock_dev",
		MsgType:    "text",
		CreateTime: strconv.FormatInt(time.Now().Add(time.Minute).UnixMilli(), 10),
		UpdateTime: strconv.FormatInt(time.Now().Add(time.Minute).UnixMilli(), 10),
	}
	newRoot.Body.Content = jc(map[string]string{"text": "在飞书中新增的话题测试"})
	newRoot.Sender.ID, newRoot.Sender.IDType, newRoot.Sender.SenderType = uLi, "open_id", "user"

	api.mu.Lock()
	api.threads[newThread] = []RawMessage{newRoot}
	api.chatMsgs["oc_mock_dev"] = append([]RawMessage{newRoot}, api.chatMsgs["oc_mock_dev"]...)
	api.mu.Unlock()

	// Invalidate the threadrefs cache to simulate 10s TTL expiration
	svc.cache.Delete(svc.k("threadrefs:oc_mock_dev"))

	// Subsequent fetch with refresh=false (silent background poll) MUST discover the new topic!
	res2, err := svc.Topics(ctx, "oc_mock_dev", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Topics) != initCount+1 {
		t.Fatalf("expected %d topics after auto refresh, got %d", initCount+1, len(res2.Topics))
	}
	if res2.Topics[0].ThreadID != newThread {
		t.Fatalf("expected newest topic %s at the top, got %s", newThread, res2.Topics[0].ThreadID)
	}
}

func TestChatsFilterDaidainiao(t *testing.T) {
	srv, svc := newTestServer(t)
	if m, ok := svc.api.(*MockAPI); ok {
		m.chats = append(m.chats, Chat{ChatID: "oc_ddn_test", Name: "【测试】呆呆鸟"})
	}
	h := srv.Handler()
	req := httptest.NewRequest("GET", "/api/chats", nil)
	req.Host = "localhost:8080"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code %d", w.Code)
	}
	var res struct {
		Chats []Chat `json:"chats"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if len(res.Chats) != 1 || res.Chats[0].ChatID != "oc_ddn_test" {
		t.Fatalf("chats filter failed: %+v", res.Chats)
	}
}
