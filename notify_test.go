package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

type recordCardAPI struct {
	MockAPI
	cards []sentCard
}

type sentCard struct {
	openID string
	card   any
}

func (r *recordCardAPI) SendBotCard(ctx context.Context, openID string, card any) error {
	r.cards = append(r.cards, sentCard{openID: openID, card: card})
	return nil
}

func TestNotificationSettingsAndDeDupe(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.DataDir = dir
	cfg.Normalize()
	auth := NewAuth(cfg, true, filepath.Join(dir, "token.json"))
	if err := auth.Callback(context.Background(), "mock"); err != nil {
		t.Fatal(err)
	}
	api := &recordCardAPI{MockAPI: *NewMockAPI(auth)}
	svc := NewService(api, NewCache(filepath.Join(dir, "cache")), cfg, auth)
	t.Cleanup(func() { svc.Close() })

	// 1. Initial settings
	st := svc.NotifySettings()
	if !st.FeishuEnabled || !st.BrowserEnabled || !st.SoundEnabled {
		t.Fatalf("expected defaults all true, got %+v", st)
	}

	// 2. Update settings
	svc.UpdateNotifySettings(NotificationSettings{
		FeishuEnabled:  true,
		BrowserEnabled: false,
		SoundEnabled:   true,
	})
	st2 := svc.NotifySettings()
	if st2.BrowserEnabled {
		t.Fatalf("expected browser_enabled=false, got %+v", st2)
	}

	// 3. Test send notification
	notified, err := svc.NotifyTaskComplete(context.Background(), "omt_test_1", "群聊A", "话题A", "ou_user_1", "已完成代码编写与测试", "http://localhost:8088")
	if err != nil {
		t.Fatalf("NotifyTaskComplete error: %v", err)
	}
	if !notified {
		t.Fatalf("expected notified=true")
	}
	if len(api.cards) != 1 {
		t.Fatalf("expected 1 card sent, got %d", len(api.cards))
	}
	if api.cards[0].openID != "ou_user_1" {
		t.Fatalf("expected target ou_user_1, got %s", api.cards[0].openID)
	}

	// 4. De-duplication: second call within 45s should be ignored
	notified2, err := svc.NotifyTaskComplete(context.Background(), "omt_test_1", "群聊A", "话题A", "ou_user_1", "已完成代码编写与测试", "http://localhost:8088")
	if err != nil {
		t.Fatalf("NotifyTaskComplete 2nd error: %v", err)
	}
	if notified2 {
		t.Fatalf("expected notified2=false due to deduplication")
	}
	if len(api.cards) != 1 {
		t.Fatalf("expected still 1 card, got %d", len(api.cards))
	}

	// 5. Another thread should succeed
	notified3, err := svc.NotifyTaskComplete(context.Background(), "omt_test_2", "群聊B", "话题B", "ou_user_1", "数据分析完成", "http://localhost:8088")
	if err != nil || !notified3 {
		t.Fatalf("expected notified3=true, err=%v", err)
	}
	if len(api.cards) != 2 {
		t.Fatalf("expected 2 cards sent, got %d", len(api.cards))
	}
}

func TestNotificationHTTPHandlers(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.DataDir = dir
	cfg.Normalize()
	auth := NewAuth(cfg, true, filepath.Join(dir, "token.json"))
	if err := auth.Callback(context.Background(), "mock"); err != nil {
		t.Fatal(err)
	}
	api := &recordCardAPI{MockAPI: *NewMockAPI(auth)}
	svc := NewService(api, NewCache(filepath.Join(dir, "cache")), cfg, auth)
	t.Cleanup(func() { svc.Close() })
	sel, _, err := LoadSelection(filepath.Join(dir, "selection.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(svc, sel, cfg, true, nil)
	handler := server.Handler()

	do := func(method, path string, body []byte) *httptest.ResponseRecorder {
		var rd *bytes.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		} else {
			rd = bytes.NewReader([]byte{})
		}
		req := httptest.NewRequest(method, path, rd)
		req.Host = "127.0.0.1:8088"
		req.RemoteAddr = "127.0.0.1:50000"
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// 1. GET /api/notifications/settings
	w := do("GET", "/api/notifications/settings", nil)
	if w.Code != 200 {
		t.Fatalf("GET settings status %d: %s", w.Code, w.Body.String())
	}
	var st NotificationSettings
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if !st.FeishuEnabled {
		t.Fatalf("expected FeishuEnabled true")
	}

	// 2. POST /api/notifications/settings
	body, _ := json.Marshal(NotificationSettings{FeishuEnabled: false, BrowserEnabled: true, SoundEnabled: false})
	w = do("POST", "/api/notifications/settings", body)
	if w.Code != 200 {
		t.Fatalf("POST settings status %d: %s", w.Code, w.Body.String())
	}
	var st2 NotificationSettings
	_ = json.Unmarshal(w.Body.Bytes(), &st2)
	if st2.FeishuEnabled || st2.SoundEnabled || !st2.BrowserEnabled {
		t.Fatalf("unexpected settings update: %+v", st2)
	}

	// 3. POST /api/notifications/test
	w = do("POST", "/api/notifications/test", nil)
	if w.Code != 200 {
		t.Fatalf("POST test status %d: %s", w.Code, w.Body.String())
	}
	if len(api.cards) != 1 {
		t.Fatalf("expected 1 test card sent, got %d", len(api.cards))
	}

	// 4. POST /api/notifications/complete
	compBody, _ := json.Marshal(map[string]any{
		"thread_id":   "omt_http_1",
		"chat_name":   "测试群",
		"topic_title": "测试话题",
		"snippet":     "执行结束",
	})
	w = do("POST", "/api/notifications/complete", compBody)
	if w.Code != 200 {
		t.Fatalf("POST complete status %d: %s", w.Code, w.Body.String())
	}
}

func TestNotifyWatcherTaskCompletion(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.DataDir = dir
	cfg.Normalize()
	auth := NewAuth(cfg, true, filepath.Join(dir, "token.json"))
	if err := auth.Callback(context.Background(), "mock"); err != nil {
		t.Fatal(err)
	}
	api := &recordCardAPI{MockAPI: *NewMockAPI(auth)}
	svc := NewService(api, NewCache(filepath.Join(dir, "cache")), cfg, auth)
	t.Cleanup(func() { svc.Close() })

	// Watch task
	svc.WatchTask("omt_watch_test", "oc_1", "群A", "话题A", "ou_user_1")
	svc.notify.mu.Lock()
	_, exists := svc.notify.watched["omt_watch_test"]
	svc.notify.mu.Unlock()
	if !exists {
		t.Fatalf("expected omt_watch_test to be watched")
	}

	// Unwatch task
	svc.notify.Unwatch("omt_watch_test")
	svc.notify.mu.Lock()
	_, exists = svc.notify.watched["omt_watch_test"]
	svc.notify.mu.Unlock()
	if exists {
		t.Fatalf("expected omt_watch_test to be unwatched")
	}
	_ = time.Second
}
