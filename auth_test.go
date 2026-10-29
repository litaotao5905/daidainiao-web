package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeOAuth is a minimal Feishu: OAuth token endpoint, user_info and chats.
type fakeOAuth struct {
	t            *testing.T
	mu           sync.Mutex
	refreshCalls int32
	curRefresh   string // the only refresh token currently valid (single use)
	accessSeq    int32
	expiresIn    int
	validAccess  map[string]bool
}

func (f *fakeOAuth) handler() http.Handler {
	f.validAccess = map[string]bool{}
	mux := http.NewServeMux()
	mux.HandleFunc("/open-apis/authen/v2/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		if body["client_id"] != "cli_test" || body["client_secret"] != "secret" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{"code": 20024, "error": "invalid_client", "error_description": "bad client"})
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		issue := func() {
			n := atomic.AddInt32(&f.accessSeq, 1)
			access := "u-access-" + string(rune('0'+n))
			f.validAccess[access] = true
			f.curRefresh = "r-" + string(rune('0'+n))
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "access_token": access, "expires_in": f.expiresIn,
				"refresh_token": f.curRefresh, "refresh_token_expires_in": 604800, "scope": "offline_access im:chat:readonly", "token_type": "Bearer"})
		}
		switch body["grant_type"] {
		case "authorization_code":
			if body["code"] != "good-code" || body["redirect_uri"] != "http://localhost:8080/callback" {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]any{"code": 20024, "error": "invalid_grant", "error_description": "bad code"})
				return
			}
			issue()
		case "refresh_token":
			atomic.AddInt32(&f.refreshCalls, 1)
			if body["refresh_token"] != f.curRefresh || f.curRefresh == "" {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]any{"code": 20064, "error": "invalid_grant", "error_description": "refresh token revoked or used"})
				return
			}
			issue()
		default:
			f.t.Errorf("unexpected grant_type %q", body["grant_type"])
		}
	})
	mux.HandleFunc("/open-apis/authen/v1/user_info", func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		ok := f.validAccess[tok]
		f.mu.Unlock()
		if !ok {
			json.NewEncoder(w).Encode(map[string]any{"code": 99991668, "msg": "invalid"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"name": "张三", "open_id": "ou_zs", "avatar_url": "https://a/b"}})
	})
	mux.HandleFunc("/open-apis/im/v1/chats", func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		ok := f.validAccess[tok]
		f.mu.Unlock()
		if !ok {
			json.NewEncoder(w).Encode(map[string]any{"code": 99991677, "msg": "user access token expired"})
			return
		}
		w.Header().Set("X-Used-Token", tok)
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"items": []map[string]any{{"chat_id": "oc_1", "name": tok}}, "has_more": false}})
	})
	return mux
}

func realConfig(base string) Config {
	cfg := defaultConfig()
	cfg.AppID, cfg.AppSecret, cfg.BaseURL = "cli_test", "secret", base
	cfg.RedirectURI = "http://localhost:8080/callback"
	cfg.Normalize()
	return cfg
}

func TestOAuthStateIsSingleUseAndExpires(t *testing.T) {
	a := NewAuth(realConfig("http://x"), false, filepath.Join(t.TempDir(), "t.json"))
	s := a.NewState()
	if len(s) < 20 {
		t.Fatalf("state too short: %q", s)
	}
	if !a.ConsumeState(s) {
		t.Fatal("valid state rejected")
	}
	if a.ConsumeState(s) {
		t.Fatal("state must be single use")
	}
	if a.ConsumeState("") || a.ConsumeState("forged") {
		t.Fatal("unknown state accepted")
	}
	s2 := a.NewState()
	a.smu.Lock()
	a.states[s2] = time.Now().Add(-time.Second)
	a.smu.Unlock()
	if a.ConsumeState(s2) {
		t.Fatal("expired state accepted")
	}
}

func TestAuthorizeURL(t *testing.T) {
	a := NewAuth(realConfig("https://open.feishu.cn"), false, filepath.Join(t.TempDir(), "t.json"))
	u, err := url.Parse(a.AuthorizeURL("st123"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "accounts.feishu.cn" || u.Path != "/open-apis/authen/v1/authorize" {
		t.Fatalf("bad endpoint: %s", u)
	}
	if q.Get("client_id") != "cli_test" || q.Get("response_type") != "code" || q.Get("state") != "st123" || q.Get("redirect_uri") != "http://localhost:8080/callback" {
		t.Fatalf("bad query: %v", q)
	}
	scopes := strings.Fields(q.Get("scope"))
	want := map[string]bool{"offline_access": true, "im:chat:readonly": true, "im:message:readonly": true, "im:message.group_msg:get_as_user": true}
	for _, s := range scopes {
		delete(want, s)
	}
	if len(want) != 0 {
		t.Fatalf("missing scopes %v in %v", want, scopes)
	}
	lark := NewAuth(realConfig("https://open.larksuite.com"), false, filepath.Join(t.TempDir(), "t.json"))
	if !strings.HasPrefix(lark.AuthorizeURL("x"), "https://accounts.larksuite.com/") {
		t.Fatal("lark accounts host expected")
	}
}

// HTTP-level CSRF checks of /api/auth/login + /callback (mock flow).
func TestCallbackStateValidationHTTP(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.DataDir = dir
	cfg.Normalize()
	auth := NewAuth(cfg, true, filepath.Join(dir, "token.json"))
	svc := NewService(NewMockAPI(auth), NewCache(""), cfg, auth)
	sel, _, _ := LoadSelection(filepath.Join(dir, "sel.json"))
	h := NewServer(svc, sel, cfg, true, nil).Handler()
	do := func(path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Host = "localhost:8080"
		for _, c := range cookies {
			req.AddCookie(c)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	// not logged in -> API says login_required
	if w := do("/api/chats"); w.Code != 401 || !strings.Contains(w.Body.String(), "login_required") {
		t.Fatalf("expected 401 login_required, got %d %s", w.Code, w.Body.String())
	}
	// no cookie / forged state
	if w := do("/callback?code=mock&state=forged"); w.Code != 400 {
		t.Fatalf("forged state accepted: %d", w.Code)
	}
	// start login
	w := do("/api/auth/login")
	if w.Code != 302 {
		t.Fatalf("login: %d", w.Code)
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	state := loc.Query().Get("state")
	cookie := w.Result().Cookies()[0]
	if cookie.Name != stateCookie || !cookie.HttpOnly || cookie.Value != state {
		t.Fatalf("state cookie: %+v", cookie)
	}
	// valid state but missing cookie (login CSRF) -> rejected, and the state is NOT consumed by that attempt? it is: single use
	if w := do("/callback?code=mock&state=" + state); w.Code != 400 {
		t.Fatalf("missing cookie accepted: %d", w.Code)
	}
	if auth.LoggedIn() {
		t.Fatal("must not be logged in after failed callbacks")
	}
	// fresh round trip works
	w = do("/api/auth/login")
	loc, _ = url.Parse(w.Header().Get("Location"))
	state = loc.Query().Get("state")
	cookie = w.Result().Cookies()[0]
	// mismatching cookie -> rejected
	if w := do("/callback?code=mock&state="+state, &http.Cookie{Name: stateCookie, Value: "other"}); w.Code != 400 {
		t.Fatalf("mismatching cookie accepted: %d", w.Code)
	}
	w = do("/api/auth/login")
	loc, _ = url.Parse(w.Header().Get("Location"))
	state = loc.Query().Get("state")
	cookie = w.Result().Cookies()[0]
	if w := do("/callback?code=mock&state="+state, cookie); w.Code != 302 || w.Header().Get("Location") != "/" {
		t.Fatalf("valid callback: %d %s", w.Code, w.Body.String())
	}
	if !auth.LoggedIn() {
		t.Fatal("should be logged in")
	}
	// replay -> rejected
	if w := do("/callback?code=mock&state="+state, cookie); w.Code != 400 {
		t.Fatalf("replay accepted: %d", w.Code)
	}
	if w := do("/api/chats"); w.Code != 200 {
		t.Fatalf("chats after login: %d", w.Code)
	}
	st, err := os.Stat(filepath.Join(dir, "token.json"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("token.json perms: %v %v", st, err)
	}
	// logout
	req := httptest.NewRequest("POST", "/api/auth/logout", strings.NewReader("{}"))
	req.Host = "localhost:8080"
	req.Header.Set("Content-Type", "application/json")
	lw := httptest.NewRecorder()
	h.ServeHTTP(lw, req)
	if lw.Code != 200 || auth.LoggedIn() {
		t.Fatalf("logout: %d", lw.Code)
	}
	if _, err := os.Stat(filepath.Join(dir, "token.json")); !os.IsNotExist(err) {
		t.Fatal("token.json should be removed on logout")
	}
	if w := do("/api/chats"); w.Code != 401 {
		t.Fatalf("chats after logout: %d", w.Code)
	}
}

func TestCodeExchangeAndRefreshWithFakeFeishu(t *testing.T) {
	f := &fakeOAuth{t: t, expiresIn: 30} // < refresh margin: forces refresh on first use
	ts := httptest.NewServer(f.handler())
	defer ts.Close()
	cfg := realConfig(ts.URL)
	path := filepath.Join(t.TempDir(), "token.json")
	a := NewAuth(cfg, false, path)
	ctx := context.Background()

	if err := a.Callback(ctx, "bad-code"); err == nil {
		t.Fatal("bad code accepted")
	} else if oe, ok := err.(*OAuthError); !ok || oe.Err != "invalid_grant" {
		t.Fatalf("want OAuthError invalid_grant, got %v", err)
	}
	if err := a.Callback(ctx, "good-code"); err != nil {
		t.Fatal(err)
	}
	st := a.Status()
	if !st.LoggedIn || st.User.Name != "张三" || st.User.OpenID != "ou_zs" || !st.CanRefresh {
		t.Fatalf("status: %+v", st)
	}
	if fi, _ := os.Stat(path); fi == nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file perms: %v", fi)
	}

	// token expires in 30s => AccessToken refreshes (single-use refresh token rotates)
	f.expiresIn = 7200
	tok, err := a.AccessToken(ctx)
	if err != nil || tok != "u-access-2" || f.refreshCalls != 1 {
		t.Fatalf("refresh: tok=%q err=%v calls=%d", tok, err, f.refreshCalls)
	}
	// persisted before use: a new process sees the rotated tokens
	a2 := NewAuth(cfg, false, path)
	if tok2, err := a2.AccessToken(ctx); err != nil || tok2 != "u-access-2" || f.refreshCalls != 1 {
		t.Fatalf("reload: %q %v calls=%d", tok2, err, f.refreshCalls)
	}
	// still valid, no more refreshes
	if _, err := a.AccessToken(ctx); err != nil || f.refreshCalls != 1 {
		t.Fatalf("unexpected refresh, calls=%d", f.refreshCalls)
	}
	// concurrent Refresh(stale) calls only rotate once
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = a.Refresh(ctx, "u-access-2") }()
	}
	wg.Wait()
	if f.refreshCalls != 2 {
		t.Fatalf("refresh should be deduplicated, calls=%d", f.refreshCalls)
	}

	// refresh token revoked server-side -> must log in again and tokens are wiped
	f.mu.Lock()
	f.curRefresh = "revoked"
	f.mu.Unlock()
	cur, _ := a.AccessToken(ctx)
	if _, err := a.Refresh(ctx, cur); err != ErrLoginRequired {
		t.Fatalf("want ErrLoginRequired, got %v", err)
	}
	if a.LoggedIn() {
		t.Fatal("tokens should be cleared")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("token file should be removed")
	}
}

// An API call with an expired user token transparently refreshes and retries
// with the *new* token in the Authorization header.
func TestClientRefreshesOnExpiredUserToken(t *testing.T) {
	f := &fakeOAuth{t: t, expiresIn: 7200}
	ts := httptest.NewServer(f.handler())
	defer ts.Close()
	cfg := realConfig(ts.URL)
	a := NewAuth(cfg, false, filepath.Join(t.TempDir(), "token.json"))
	ctx := context.Background()
	if err := a.Callback(ctx, "good-code"); err != nil {
		t.Fatal(err)
	}
	// the server forgets the access token (e.g. revoked / expired early)
	f.mu.Lock()
	f.validAccess = map[string]bool{}
	f.mu.Unlock()
	c := NewFeishuClient(ts.URL, cfg.AppID, cfg.AppSecret, a, "auto")
	c.gap = 0
	chats, err := c.ListChats(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(chats.Items) != 1 || chats.Items[0].Name != "u-access-2" {
		t.Fatalf("expected retry with refreshed token, got %+v", chats.Items)
	}
	if f.refreshCalls != 1 {
		t.Fatalf("refreshCalls=%d", f.refreshCalls)
	}
}

func TestRealLoginRedirectsToFeishu(t *testing.T) {
	dir := t.TempDir()
	cfg := realConfig("https://open.feishu.cn")
	cfg.DataDir = dir
	auth := NewAuth(cfg, false, filepath.Join(dir, "token.json"))
	svc := NewService(NewMockAPI(auth), NewCache(""), cfg, auth)
	sel, _, _ := LoadSelection(filepath.Join(dir, "sel.json"))
	h := NewServer(svc, sel, cfg, false, nil).Handler()
	req := httptest.NewRequest("GET", "/api/auth/login", nil)
	req.Host = "localhost:8080"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	loc := w.Header().Get("Location")
	if w.Code != 302 || !strings.HasPrefix(loc, "https://accounts.feishu.cn/open-apis/authen/v1/authorize?") {
		t.Fatalf("login: %d %s", w.Code, loc)
	}
	// opened via a different host than redirect_uri -> explained, not redirected
	req = httptest.NewRequest("GET", "/api/auth/login", nil)
	req.Host = "127.0.0.1:8080"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("host mismatch should be rejected, got %d", w.Code)
	}
}
