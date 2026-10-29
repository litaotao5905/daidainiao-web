package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// loggedInAuth returns an Auth that already holds a user token.
func loggedInAuth(t *testing.T, cfg Config, access, refresh string, ttl time.Duration) *Auth {
	t.Helper()
	cfg.Normalize()
	a := NewAuth(cfg, false, filepath.Join(t.TempDir(), "token.json"))
	a.tok = &tokenFile{AppID: cfg.AppID, AccessToken: access, RefreshToken: refresh,
		ExpiresAt: time.Now().Add(ttl), RefreshExpiresAt: time.Now().Add(24 * time.Hour),
		User: UserInfo{OpenID: "ou_me", Name: "我"}}
	return a
}

// All data APIs must use the *user* token; retries (429, business rate limit)
// and the pagination parameters are exercised as well.
func TestFeishuClientUsesUserToken(t *testing.T) {
	var msgCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/open-apis/im/v1/chats", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer u-access" {
			t.Errorf("chats: Authorization=%q", got)
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"items": []map[string]any{{"chat_id": "oc_1", "name": "群"}}, "has_more": false}})
	})
	mux.HandleFunc("/open-apis/im/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer u-access" {
			t.Errorf("messages: Authorization=%q", got)
		}
		q := r.URL.Query()
		if q.Get("container_id_type") != "thread" || q.Get("container_id") != "omt_1" {
			t.Errorf("bad query %v", q)
		}
		switch atomic.AddInt32(&msgCalls, 1) {
		case 1:
			w.WriteHeader(http.StatusTooManyRequests)
			return
		case 2:
			json.NewEncoder(w).Encode(map[string]any{"code": 99991400, "msg": "rate limit"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "success", "data": map[string]any{
			"has_more": true, "page_token": "p2",
			"items": []map[string]any{{"message_id": "om_1", "msg_type": "text", "create_time": "1700000000000", "thread_id": "omt_1",
				"sender": map[string]any{"id": "ou_1", "id_type": "open_id", "sender_type": "user"},
				"body":   map[string]any{"content": `{"text":"hi"}`}}},
		}})
	})
	mux.HandleFunc("/open-apis/contact/v3/users/batch", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer u-access" {
			t.Errorf("users: Authorization=%q", got)
		}
		if got := r.URL.Query()["user_ids"]; len(got) != 2 {
			t.Errorf("user_ids=%v", got)
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"items": []map[string]any{
			{"open_id": "ou_1", "name": "测试", "avatar": map[string]any{"avatar_72": "https://a/72"}}}}})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	cfg := defaultConfig()
	cfg.AppID, cfg.AppSecret, cfg.BaseURL = "cli_test", "secret", ts.URL
	c := NewFeishuClient(ts.URL, "cli_test", "secret", loggedInAuth(t, cfg, "u-access", "r1", time.Hour), "auto")
	c.gap = 0
	ctx := context.Background()
	chats, err := c.ListChats(ctx, "")
	if err != nil || len(chats.Items) != 1 {
		t.Fatalf("chats: %+v %v", chats, err)
	}
	p, err := c.ListMessages(ctx, "thread", "omt_1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 1 || !p.HasMore || p.PageToken != "p2" || p.Items[0].Sender.ID != "ou_1" {
		t.Fatalf("page: %+v", p)
	}
	if msgCalls != 3 {
		t.Fatalf("expected 3 attempts (429, biz rate limit, ok), got %d", msgCalls)
	}
	us, err := c.BatchUsers(ctx, []string{"ou_1", "ou_2"})
	if err != nil || len(us) != 1 || us[0].Name != "测试" || us[0].Avatar != "https://a/72" {
		t.Fatalf("users: %+v %v", us, err)
	}
}

func TestFeishuClientRequiresLogin(t *testing.T) {
	cfg := defaultConfig()
	cfg.AppID, cfg.AppSecret = "cli_test", "secret"
	cfg.Normalize()
	auth := NewAuth(cfg, false, filepath.Join(t.TempDir(), "token.json"))
	c := NewFeishuClient("http://127.0.0.1:1", "cli_test", "secret", auth, "auto")
	if _, err := c.ListChats(context.Background(), ""); err != ErrLoginRequired {
		t.Fatalf("got %v", err)
	}
}

// Resource download: the official API only accepts the tenant token. "auto"
// tries the app identity first and then the user identity.
func TestResourceDownloadIdentityFallback(t *testing.T) {
	var tenantCalls, userCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/open-apis/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "t-app", "expire": 7200})
	})
	mux.HandleFunc("/open-apis/im/v1/messages/om_1/resources/", func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path[len("/open-apis/im/v1/messages/om_1/resources/"):]
		auth := r.Header.Get("Authorization")
		if auth == "Bearer t-app" {
			atomic.AddInt32(&tenantCalls, 1)
			if key == "bot_in_chat" {
				w.Header().Set("Content-Type", "image/png")
				w.Header().Set("Content-Disposition", `attachment; filename="a.png"`)
				w.Write([]byte("PNGDATA"))
				return
			}
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{"code": 234004, "msg": "App not in chat."})
			return
		}
		atomic.AddInt32(&userCalls, 1)
		if auth == "Bearer u-access" && key == "user_ok" {
			w.Write([]byte("USERDATA"))
			return
		}
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]any{"code": 99991668, "msg": "Invalid access token"})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	cfg := defaultConfig()
	cfg.AppID, cfg.AppSecret, cfg.BaseURL = "cli_test", "secret", ts.URL
	mk := func(mode string) *FeishuClient {
		c := NewFeishuClient(ts.URL, "cli_test", "secret", loggedInAuth(t, cfg, "u-access", "r1", time.Hour), mode)
		c.gap = 0
		return c
	}
	ctx := context.Background()
	read := func(d *Download) string { b, _ := io.ReadAll(d.Body); d.Body.Close(); return string(b) }

	c := mk("auto")
	d, err := c.Download(ctx, "om_1", "bot_in_chat", "image")
	if err != nil || read(d) != "PNGDATA" || d.Filename != "a.png" || userCalls != 0 {
		t.Fatalf("tenant success: %v user=%d", err, userCalls)
	}
	d, err = c.Download(ctx, "om_1", "user_ok", "file")
	if err != nil || read(d) != "USERDATA" {
		t.Fatalf("fallback to user identity: %v", err)
	}
	_, err = c.Download(ctx, "om_1", "nope", "file")
	fe, ok := err.(*FeishuError)
	if !ok || fe.Code != 234004 {
		t.Fatalf("want combined FeishuError 234004, got %v", err)
	}
	if !contains(fe.Msg, "tenant_access_token") || !contains(fe.Msg, "user_access_token") {
		t.Fatalf("combined message should mention both identities: %q", fe.Msg)
	}
	// user-only mode never touches the tenant token
	before := tenantCalls
	if _, err := mk("user").Download(ctx, "om_1", "user_ok", "file"); err != nil || tenantCalls != before {
		t.Fatalf("user-only mode: %v tenantCalls %d->%d", err, before, tenantCalls)
	}
	// tenant-only mode never touches the user token
	before = userCalls
	if _, err := mk("tenant").Download(ctx, "om_1", "nope", "file"); err == nil || userCalls != before {
		t.Fatalf("tenant-only mode: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
