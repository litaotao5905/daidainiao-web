package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBuildReplyText(t *testing.T) {
	got := buildReplyText("hi @张伟伟 和 @张伟，@陌生人", []ReplyMention{
		{ID: "ou_1", Name: "张伟"}, {ID: "ou_2", Name: "张伟伟"}, {ID: "bad id", Name: "陌生人"}, {ID: "ou_3", Name: `x"<`},
	})
	want := `hi <at user_id="ou_2">张伟伟</at> 和 <at user_id="ou_1">张伟</at>，@陌生人`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// The real client must reply as the user, in the thread, with the documented
// body, and must ask for original card JSON when reading messages.
func TestFeishuClientReplyAndCardParam(t *testing.T) {
	var replies, lists int32
	mux := http.NewServeMux()
	mux.HandleFunc("/open-apis/im/v1/messages/om_root/reply", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&replies, 1)
		if r.Method != http.MethodPost {
			t.Errorf("method %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer u-access" {
			t.Errorf("reply must use the user token, got %q", got)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("content-type %q", ct)
		}
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		if body["msg_type"] != "text" || body["reply_in_thread"] != true || body["uuid"] != "u-1" {
			t.Errorf("body: %v", body)
		}
		var content map[string]string
		_ = json.Unmarshal([]byte(body["content"].(string)), &content)
		if content["text"] != "你好\n第二行" {
			t.Errorf("content: %v", content)
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
			"message_id": "om_new", "root_id": "om_root", "thread_id": "omt_1", "msg_type": "text", "create_time": "1700000000000",
			"sender": map[string]any{"id": "ou_me", "id_type": "open_id", "sender_type": "user"},
			"body":   map[string]any{"content": body["content"]}}})
	})
	mux.HandleFunc("/open-apis/im/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&lists, 1)
		if r.URL.Query().Get("card_msg_content_type") != "user_card_content" {
			t.Errorf("list must request original card JSON: %v", r.URL.Query())
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"items": []any{}}})
	})
	mux.HandleFunc("/open-apis/im/v1/messages/om_fwd", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("card_msg_content_type") != "user_card_content" {
			t.Errorf("get must request original card JSON: %v", r.URL.Query())
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"items": []map[string]any{
			{"message_id": "om_fwd", "msg_type": "merge_forward"},
			{"message_id": "om_sub", "msg_type": "text", "upper_message_id": "om_fwd"}}}})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	cfg := defaultConfig()
	cfg.AppID, cfg.AppSecret, cfg.BaseURL = "cli_test", "secret", ts.URL
	c := NewFeishuClient(ts.URL, "cli_test", "secret", loggedInAuth(t, cfg, "u-access", "r1", time.Hour), "auto")
	c.gap = 0
	ctx := context.Background()
	m, err := c.ReplyMessage(ctx, "om_root", "text", `{"text":"你好\n第二行"}`, "u-1", true)
	if err != nil || m.MessageID != "om_new" || m.ThreadID != "omt_1" {
		t.Fatalf("reply: %+v %v", m, err)
	}
	if _, err := c.ListMessages(ctx, "thread", "omt_1", "", false); err != nil {
		t.Fatal(err)
	}
	items, err := c.GetMessage(ctx, "om_fwd")
	if err != nil || len(items) != 2 || items[1].UpperMessageID != "om_fwd" {
		t.Fatalf("get: %+v %v", items, err)
	}
}

// Sending is not retried on 5xx unless a uuid makes it idempotent.
func TestReplyNotRetriedWithoutUUID(t *testing.T) {
	var n int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(502)
		io.WriteString(w, `{"code":0,"msg":"bad gateway"}`)
	}))
	defer ts.Close()
	cfg := defaultConfig()
	cfg.AppID, cfg.AppSecret, cfg.BaseURL = "cli_test", "secret", ts.URL
	c := NewFeishuClient(ts.URL, "cli_test", "secret", loggedInAuth(t, cfg, "u-access", "r1", time.Hour), "auto")
	c.gap = 0
	if _, err := c.ReplyMessage(context.Background(), "om_root", "text", `{"text":"x"}`, "", true); err == nil {
		t.Fatal("expected error")
	}
	if n != 1 {
		t.Fatalf("a message without uuid was sent %d times", n)
	}
}

func TestFeishuClientFallsBackWithoutCardParam(t *testing.T) {
	var withParam, without int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("card_msg_content_type") != "" {
			atomic.AddInt32(&withParam, 1)
			json.NewEncoder(w).Encode(map[string]any{"code": 230001, "msg": "invalid param"})
			return
		}
		atomic.AddInt32(&without, 1)
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"items": []any{}}})
	}))
	defer ts.Close()
	cfg := defaultConfig()
	cfg.AppID, cfg.AppSecret, cfg.BaseURL = "cli_test", "secret", ts.URL
	c := NewFeishuClient(ts.URL, "cli_test", "secret", loggedInAuth(t, cfg, "u-access", "r1", time.Hour), "auto")
	c.gap = 0
	for i := 0; i < 2; i++ {
		if _, err := c.ListMessages(context.Background(), "chat", "oc_1", "", false); err != nil {
			t.Fatal(err)
		}
	}
	if withParam != 1 || without != 2 {
		t.Fatalf("param tried %d times, fallback used %d times", withParam, without)
	}
}

func TestFeishuClientDoesNotDisableCardParamOnOther230001(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		json.NewEncoder(w).Encode(map[string]any{"code": 230001, "msg": "Your request contains an invalid request parameter, ext=invalid container_id."})
	}))
	defer ts.Close()
	cfg := defaultConfig()
	cfg.AppID, cfg.AppSecret, cfg.BaseURL = "cli_test", "secret", ts.URL
	c := NewFeishuClient(ts.URL, "cli_test", "secret", loggedInAuth(t, cfg, "u-access", "r1", time.Hour), "auto")
	c.gap = 0
	_, err := c.ListMessages(context.Background(), "chat", "bad_chat", "", false)
	if err == nil {
		t.Fatal("expected error")
	}
	if c.noCardParam.Load() {
		t.Fatal("noCardParam should NOT be true when error was invalid container_id")
	}
}

func TestScopeKindAndRequiredScopes(t *testing.T) {
	for code, want := range map[int]string{99991679: "scope_missing", 230027: "scope_missing", 99991672: "app_scope_missing", 230035: ""} {
		if got := scopeKind(code); got != want {
			t.Errorf("scopeKind(%d)=%q want %q", code, got, want)
		}
	}
	got := requiredScopes("Unauthorized. ... Please request user re-authorization and try again. required one of these privileges: [im:message, im:message.send_as_user].应用未获取所需的用户授权：[im:message, im:message.send_as_user]")
	if len(got) != 2 || got[0] != "im:message" || got[1] != "im:message.send_as_user" {
		t.Fatalf("scopes: %v", got)
	}
}

func TestReplyScopesInConfig(t *testing.T) {
	cfg := defaultConfig()
	cfg.Normalize()
	for _, s := range ReplyScopes {
		if !containsScope(cfg.Scopes, s) {
			t.Errorf("default scopes lack %s", s)
		}
	}
	// an older config.json lists scopes explicitly: reply scopes are added
	cfg = defaultConfig()
	cfg.Scopes = []string{"offline_access", "im:chat:readonly"}
	cfg.Normalize()
	if !containsScope(cfg.Scopes, ScopeSendAsUser) || !containsScope(cfg.Scopes, "im:message") {
		t.Fatalf("explicit scopes: %v", cfg.Scopes)
	}
	// read-only mode asks for neither
	cfg = defaultConfig()
	cfg.DisableReply = true
	cfg.Normalize()
	if containsScope(cfg.Scopes, ScopeSendAsUser) || containsScope(cfg.Scopes, "im:message") || !containsScope(cfg.Scopes, "im:message:readonly") {
		t.Fatalf("disable_reply scopes: %v", cfg.Scopes)
	}
}

func jsonReq(t *testing.T, h http.Handler, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "localhost:8080"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestMockReplyEndToEnd(t *testing.T) {
	srv, svc := newTestServer(t)
	h := srv.Handler()
	ctx := context.Background()

	before, err := svc.Thread(ctx, "omt_mock_002", false)
	if err != nil {
		t.Fatal(err)
	}
	// the thread is now cached; replying must invalidate it
	rec, out := jsonReq(t, h, "POST", "/api/topics/omt_mock_002/reply", `{"text":"收到，我来补充。\n第二行 @李娜","mentions":[{"id":"ou_mock_lina","name":"李娜"}],"uuid":"abc-1"}`)
	if rec.Code != 200 {
		t.Fatalf("reply: %d %s", rec.Code, rec.Body.String())
	}
	msg := out["message"].(map[string]any)
	if msg["sender_name"] == "" || msg["id"] == "" || !strings.Contains(msg["plain"].(string), "收到，我来补充。") {
		t.Fatalf("message: %v", msg)
	}
	after, err := svc.Thread(ctx, "omt_mock_002", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Messages) != len(before.Messages)+1 || after.Topic.ReplyCount != before.Topic.ReplyCount+1 {
		t.Fatalf("thread cache not refreshed: %d -> %d messages", len(before.Messages), len(after.Messages))
	}
	last := after.Messages[len(after.Messages)-1]
	if last.ID != msg["id"] || last.SenderID == "" {
		t.Fatalf("new message is not last: %+v", last)
	}
	var sawAt bool
	for _, p := range last.Paragraphs {
		for _, n := range p {
			if n.Type == "at" && n.Text == "@李娜" {
				sawAt = true
			}
		}
	}
	if !sawAt {
		t.Fatalf("@ mention not rendered: %+v", last.Paragraphs)
	}
	// same uuid -> deduplicated by the (mock) API
	_, out2 := jsonReq(t, h, "POST", "/api/topics/omt_mock_002/reply", `{"text":"收到，我来补充。\n第二行 @李娜","uuid":"abc-1"}`)
	if out2["message"].(map[string]any)["id"] != msg["id"] {
		t.Fatal("uuid dedupe failed")
	}
	// validation
	if rec, _ := jsonReq(t, h, "POST", "/api/topics/omt_mock_002/reply", `{"text":"   "}`); rec.Code != 400 {
		t.Fatalf("empty reply: %d", rec.Code)
	}
	if rec, _ := jsonReq(t, h, "POST", "/api/topics/omt_mock_002/reply", `not json`); rec.Code != 400 {
		t.Fatalf("bad json: %d", rec.Code)
	}
	if rec, _ := jsonReq(t, h, "POST", "/api/topics/bad%20id/reply", `{"text":"x"}`); rec.Code != 400 {
		t.Fatalf("bad id: %d", rec.Code)
	}
	// content type is enforced for state-changing requests
	req := httptest.NewRequest("POST", "/api/topics/omt_mock_002/reply", strings.NewReader(`{"text":"x"}`))
	req.Host = "localhost:8080"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("content-type check: %d", rr.Code)
	}
}

func TestReplyMissingScopeIsReportedForRelogin(t *testing.T) {
	srv, _ := newTestServer(t)
	rec, out := jsonReq(t, srv.Handler(), "POST", "/api/topics/omt_mock_002/reply", `{"text":"请发送 !scope 测试"}`)
	if rec.Code != http.StatusBadGateway || out["kind"] != "scope_missing" || out["code"].(float64) != 99991679 {
		t.Fatalf("scope error: %d %v", rec.Code, out)
	}
	scopes, _ := out["scopes"].([]any)
	if len(scopes) != 2 || scopes[1] != "im:message.send_as_user" || !strings.Contains(out["hint"].(string), "重新") {
		t.Fatalf("scopes/hint: %v", out)
	}
	// other send failures carry a readable hint but no re-login kind
	rec, out = jsonReq(t, srv.Handler(), "POST", "/api/topics/omt_mock_002/reply", `{"text":"!muted"}`)
	if out["kind"] != nil || out["code"].(float64) != 230035 || out["hint"] == "" {
		t.Fatalf("muted: %v", out)
	}
}

func TestReplyDisabledAndLoggedOut(t *testing.T) {
	srv, svc := newTestServer(t)
	svc.cfg.DisableReply = true
	if rec, _ := jsonReq(t, srv.Handler(), "POST", "/api/topics/omt_mock_002/reply", `{"text":"x"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("disabled: %d", rec.Code)
	}
	svc.cfg.DisableReply = false
	svc.auth.Logout()
	if rec, out := jsonReq(t, srv.Handler(), "POST", "/api/topics/omt_mock_002/reply", `{"text":"x"}`); rec.Code != 401 || out["kind"] != "login_required" {
		t.Fatalf("logged out: %d %v", rec.Code, out)
	}
}

func TestAuthStatusReportsReplyAbility(t *testing.T) {
	srv, svc := newTestServer(t)
	_, out := jsonReq(t, srv.Handler(), "GET", "/api/auth/status", "")
	if out["reply_enabled"] != true || out["can_reply"] != true {
		t.Fatalf("status: %v", out)
	}
	// a token granted before the send scope existed -> must re-authorize
	svc.auth.mu.Lock()
	svc.auth.tok.Scope = "offline_access im:chat:readonly im:message:readonly"
	svc.auth.mu.Unlock()
	_, out = jsonReq(t, srv.Handler(), "GET", "/api/auth/status", "")
	if out["can_reply"] != false || out["reply_enabled"] != true {
		t.Fatalf("old token should not be able to reply: %v", out)
	}
}

func TestMergeForwardExpansionAndCards(t *testing.T) {
	_, svc := newTestServer(t)
	th, err := svc.Thread(context.Background(), "omt_mock_302", false)
	if err != nil {
		t.Fatal(err)
	}
	var fwd *Message
	byType := map[string]Message{}
	for i := range th.Messages {
		m := th.Messages[i]
		byType[m.MsgType] = m
		if m.MsgType == "merge_forward" {
			fwd = &th.Messages[i]
		}
	}
	if fwd == nil || len(fwd.Forwarded) != 4 {
		t.Fatalf("forward not expanded: %+v", fwd)
	}
	if fwd.Forwarded[0].SenderName != "张伟" || !strings.Contains(fwd.Forwarded[0].Plain, "回滚方案") {
		t.Fatalf("forwarded[0]: %+v", fwd.Forwarded[0])
	}
	if fwd.Forwarded[2].Card == nil || fwd.Forwarded[2].Card.Title != "✅ 已完成" {
		t.Fatalf("card inside forward: %+v", fwd.Forwarded[2])
	}
	nested := fwd.Forwarded[3]
	if nested.MsgType != "merge_forward" || len(nested.Forwarded) != 2 {
		t.Fatalf("nested forward: %+v", nested)
	}
	// resources inside a merged-forward message cannot be downloaded
	var sawNoDL, sawPlainLink bool
	for _, a := range th.Attachments {
		if a.MessageID == "om_mock_h02b" && a.NoDownload {
			sawNoDL = true
		}
		if a.Kind == "doc" && a.MessageID == "om_mock_h02e" {
			sawPlainLink = true
		}
	}
	if !sawNoDL || !sawPlainLink {
		t.Fatalf("forward attachments: %+v", th.Attachments)
	}
	if th.OpenURL != "https://applink.feishu.cn/client/chat/open?openChatId=oc_mock_bot" {
		t.Fatalf("open_url: %q", th.OpenURL)
	}
	for typ, want := range map[string]string{"share_chat": "设计评审", "share_user": "陈静", "todo": "整理 Q4 风险清单", "vote": "周三", "location": "世纪大道", "hongbao": "恭喜发财"} {
		if m, ok := byType[typ]; !ok || !strings.Contains(m.Plain, want) {
			t.Errorf("%s: %+v", typ, m.Plain)
		}
	}
	if byType["system"].Notice != "system" {
		t.Fatalf("system: %+v", byType["system"])
	}
	// bot cards thread
	th, err = svc.Thread(context.Background(), "omt_mock_301", false)
	if err != nil {
		t.Fatal(err)
	}
	var cards, missing int
	for _, m := range th.Messages {
		if m.Card != nil {
			cards++
		}
		if m.Notice == "card_unavailable" {
			missing++
			if strings.Contains(m.Plain, "请升级") {
				t.Fatal("placeholder leaked into plain text")
			}
		}
	}
	if cards != 4 || missing != 1 {
		t.Fatalf("cards=%d missing=%d", cards, missing)
	}
	if th.Topic.SenderType != "user" || th.Topic.Title == "" {
		t.Fatalf("topic: %+v", th.Topic)
	}
}

func TestMergeForwardFailureIsContained(t *testing.T) {
	_, svc := newTestServer(t)
	svc.api = failingForward{svc.api}
	th, err := svc.Thread(context.Background(), "omt_mock_302", false)
	if err != nil {
		t.Fatalf("a failing forward must not fail the thread: %v", err)
	}
	for _, m := range th.Messages {
		if m.MsgType == "merge_forward" && (m.Notice != "forward_failed" || len(m.Forwarded) != 0) {
			t.Fatalf("forward_failed expected: %+v", m)
		}
	}
}

type failingForward struct{ API }

func (failingForward) GetMessage(context.Context, string) ([]RawMessage, error) {
	return nil, &FeishuError{Code: 230002, Msg: "x", HTTPStatus: 400}
}
