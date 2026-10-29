package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// ErrLoginRequired means there is no valid user authorization (not logged in,
// or the refresh token expired / was revoked).
var ErrLoginRequired = errors.New("尚未登录飞书或登录已过期，请点击「用飞书登录」")

// UserInfo is the logged-in user (authen/v1/user_info).
type UserInfo struct {
	OpenID    string `json:"open_id"`
	Name      string `json:"name"`
	EnName    string `json:"en_name,omitempty"`
	AvatarURL string `json:"avatar_url,omitempty"`
}

// tokenFile is what is persisted in data/token.json (mode 0600).
type tokenFile struct {
	AppID            string    `json:"app_id"`
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token,omitempty"`
	ExpiresAt        time.Time `json:"expires_at"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at,omitempty"`
	Scope            string    `json:"scope,omitempty"`
	User             UserInfo  `json:"user"`
}

// AuthStatus is returned by GET /api/auth/status.
type AuthStatus struct {
	LoggedIn   bool      `json:"logged_in"`
	User       *UserInfo `json:"user,omitempty"`
	ExpiresAt  time.Time `json:"expires_at,omitempty"`
	CanRefresh bool      `json:"can_refresh"`
	Scope      string    `json:"scope,omitempty"`
}

// OAuthError is an error returned by the OAuth token endpoint.
type OAuthError struct {
	Code       int
	Err        string
	Desc       string
	HTTPStatus int
}

func (e *OAuthError) Error() string {
	return fmt.Sprintf("飞书授权错误 code=%d error=%s desc=%s (HTTP %d)", e.Code, e.Err, e.Desc, e.HTTPStatus)
}

// Auth implements the OAuth authorization-code flow with persistent,
// auto-refreshing user_access_token storage.
type Auth struct {
	cfg  Config
	mock bool
	hc   *http.Client
	path string

	mu  sync.Mutex
	tok *tokenFile

	smu    sync.Mutex
	states map[string]time.Time
}

const stateTTL = 10 * time.Minute

// NewAuth creates the authenticator and loads data/token.json if present.
func NewAuth(cfg Config, mock bool, path string) *Auth {
	a := &Auth{cfg: cfg, mock: mock, hc: &http.Client{Timeout: 25 * time.Second}, path: path, states: map[string]time.Time{}}
	if b, err := os.ReadFile(path); err == nil {
		var tf tokenFile
		if json.Unmarshal(b, &tf) == nil && tf.AccessToken != "" && (tf.AppID == cfg.AppID || mock) {
			a.tok = &tf
		}
	}
	return a
}

func (a *Auth) accountsURL() string {
	if a.cfg.AccountsURL != "" {
		return strings.TrimRight(a.cfg.AccountsURL, "/")
	}
	if strings.Contains(a.cfg.BaseURL, "larksuite") {
		return "https://accounts.larksuite.com"
	}
	return "https://accounts.feishu.cn"
}

// AuthorizeURL builds the authorization page URL (authen/v1/authorize).
func (a *Auth) AuthorizeURL(state string) string {
	q := url.Values{}
	q.Set("client_id", a.cfg.AppID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", a.cfg.RedirectURI)
	q.Set("scope", strings.Join(a.cfg.Scopes, " "))
	q.Set("state", state)
	return a.accountsURL() + "/open-apis/authen/v1/authorize?" + q.Encode()
}

// NewState creates a single-use random CSRF state.
func (a *Auth) NewState() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	s := base64.RawURLEncoding.EncodeToString(b)
	a.smu.Lock()
	defer a.smu.Unlock()
	now := time.Now()
	for k, t := range a.states {
		if now.After(t) {
			delete(a.states, k)
		}
	}
	a.states[s] = now.Add(stateTTL)
	return s
}

// ConsumeState validates and removes a state value (single use).
func (a *Auth) ConsumeState(s string) bool {
	if s == "" {
		return false
	}
	a.smu.Lock()
	defer a.smu.Unlock()
	exp, ok := a.states[s]
	if !ok {
		return false
	}
	delete(a.states, s)
	return time.Now().Before(exp)
}

type tokenResp struct {
	Code             int    `json:"code"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	AccessToken      string `json:"access_token"`
	ExpiresIn        int    `json:"expires_in"`
	RefreshToken     string `json:"refresh_token"`
	RefreshExpiresIn int    `json:"refresh_token_expires_in"`
	Scope            string `json:"scope"`
	TokenType        string `json:"token_type"`
}

// postToken calls POST authen/v2/oauth/token (JSON body).
func (a *Auth) postToken(ctx context.Context, body map[string]string) (*tokenResp, error) {
	body["client_id"] = a.cfg.AppID
	body["client_secret"] = a.cfg.AppSecret
	payload, _ := json.Marshal(body)
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.BaseURL+apiPrefix+"/authen/v2/oauth/token", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		resp, err := a.hc.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			last = fmt.Errorf("请求飞书授权接口失败: %w", scrubErr(err))
			if e := sleepCtx(ctx, backoff(attempt, nil)); e != nil {
				return nil, e
			}
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		var tr tokenResp
		_ = json.Unmarshal(b, &tr)
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			last = &OAuthError{Code: tr.Code, Err: tr.Error, Desc: tr.ErrorDescription, HTTPStatus: resp.StatusCode}
			if e := sleepCtx(ctx, backoff(attempt, resp)); e != nil {
				return nil, e
			}
			continue
		}
		if tr.Code != 0 || tr.AccessToken == "" {
			return nil, &OAuthError{Code: tr.Code, Err: tr.Error, Desc: tr.ErrorDescription, HTTPStatus: resp.StatusCode}
		}
		return &tr, nil
	}
	return nil, last
}

func (a *Auth) fromResp(tr *tokenResp, user UserInfo, prev *tokenFile) *tokenFile {
	now := time.Now()
	tf := &tokenFile{AppID: a.cfg.AppID, AccessToken: tr.AccessToken, Scope: tr.Scope, User: user}
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	tf.ExpiresAt = now.Add(ttl)
	if tr.RefreshToken != "" {
		tf.RefreshToken = tr.RefreshToken
		rttl := time.Duration(tr.RefreshExpiresIn) * time.Second
		if rttl <= 0 {
			rttl = 7 * 24 * time.Hour
		}
		tf.RefreshExpiresAt = now.Add(rttl)
	}
	return tf
}

// FetchUserInfo calls GET authen/v1/user_info with the given access token.
func (a *Auth) FetchUserInfo(ctx context.Context, accessToken string) (UserInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.BaseURL+apiPrefix+"/authen/v1/user_info", nil)
	if err != nil {
		return UserInfo{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := a.hc.Do(req)
	if err != nil {
		return UserInfo{}, fmt.Errorf("获取用户信息失败: %w", scrubErr(err))
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env struct {
		Code int      `json:"code"`
		Msg  string   `json:"msg"`
		Data UserInfo `json:"data"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return UserInfo{}, &FeishuError{Msg: "无法解析用户信息响应", HTTPStatus: resp.StatusCode}
	}
	if env.Code != 0 {
		return UserInfo{}, &FeishuError{Code: env.Code, Msg: env.Msg, HTTPStatus: resp.StatusCode}
	}
	return env.Data, nil
}

// Callback exchanges an authorization code for tokens, loads the user's
// profile and persists everything. The state must already be validated.
func (a *Auth) Callback(ctx context.Context, code string) error {
	if a.mock {
		if code != "mock" {
			return errors.New("无效的模拟授权码")
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		a.tok = &tokenFile{AppID: "mock", AccessToken: "u-mock-user-token", RefreshToken: "mock-refresh",
			ExpiresAt: time.Now().Add(2 * time.Hour), RefreshExpiresAt: time.Now().Add(7 * 24 * time.Hour),
			Scope: strings.Join(a.cfg.Scopes, " "), User: UserInfo{OpenID: uLi, Name: "李娜（模拟登录）", AvatarURL: "/api/mock/avatar?name=李娜&id=" + uLi}}
		return a.saveLocked()
	}
	if !a.cfg.Configured() {
		return ErrNotConfigured
	}
	tr, err := a.postToken(ctx, map[string]string{
		"grant_type":   "authorization_code",
		"code":         code,
		"redirect_uri": a.cfg.RedirectURI,
	})
	if err != nil {
		return err
	}
	user, err := a.FetchUserInfo(ctx, tr.AccessToken)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tok = a.fromResp(tr, user, nil)
	return a.saveLocked()
}

func (a *Auth) saveLocked() error {
	if a.tok == nil {
		return nil
	}
	b, _ := json.MarshalIndent(a.tok, "", "  ")
	return writeFileAtomic(a.path, b, 0o600)
}

func (a *Auth) clearLocked() {
	a.tok = nil
	_ = os.Remove(a.path)
}

// Logout forgets the stored tokens (local only).
func (a *Auth) Logout() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.clearLocked()
}

// Status reports login state without touching the network.
func (a *Auth) Status() AuthStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tok == nil || !a.usableLocked() {
		return AuthStatus{}
	}
	u := a.tok.User
	return AuthStatus{LoggedIn: true, User: &u, ExpiresAt: a.tok.ExpiresAt, CanRefresh: a.canRefreshLocked(), Scope: a.tok.Scope}
}

func (a *Auth) canRefreshLocked() bool {
	return a.tok != nil && a.tok.RefreshToken != "" && time.Now().Before(a.tok.RefreshExpiresAt)
}

func (a *Auth) usableLocked() bool {
	return time.Now().Before(a.tok.ExpiresAt) || a.canRefreshLocked()
}

func (a *Auth) LoggedIn() bool { return a.Status().LoggedIn }

// UserID returns the open_id of the logged-in user ("" if none).
func (a *Auth) UserID() string {
	s := a.Status()
	if s.User != nil {
		return s.User.OpenID
	}
	return ""
}

// AccessToken returns a valid user_access_token, refreshing it when it is
// about to expire. It returns ErrLoginRequired when the user must log in again.
func (a *Auth) AccessToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tok == nil {
		return "", ErrLoginRequired
	}
	if time.Until(a.tok.ExpiresAt) > 90*time.Second {
		return a.tok.AccessToken, nil
	}
	return a.refreshLocked(ctx)
}

// Refresh renews the token if stale is still the current access token
// (another goroutine may already have refreshed it).
func (a *Auth) Refresh(ctx context.Context, stale string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tok == nil {
		return "", ErrLoginRequired
	}
	if a.tok.AccessToken != stale {
		return a.tok.AccessToken, nil
	}
	return a.refreshLocked(ctx)
}

// refreshLocked uses the (single-use) refresh_token. The new pair is
// persisted before it is used anywhere else.
func (a *Auth) refreshLocked(ctx context.Context) (string, error) {
	if a.mock {
		a.tok.ExpiresAt = time.Now().Add(2 * time.Hour)
		_ = a.saveLocked()
		return a.tok.AccessToken, nil
	}
	if !a.canRefreshLocked() {
		a.clearLocked()
		return "", ErrLoginRequired
	}
	tr, err := a.postToken(ctx, map[string]string{"grant_type": "refresh_token", "refresh_token": a.tok.RefreshToken})
	if err != nil {
		var oe *OAuthError
		if errors.As(err, &oe) && oe.HTTPStatus >= 400 && oe.HTTPStatus < 500 && oe.HTTPStatus != 429 {
			// refresh_token expired / revoked / already used: must log in again
			a.clearLocked()
			return "", ErrLoginRequired
		}
		return "", err
	}
	next := a.fromResp(tr, a.tok.User, a.tok)
	if next.RefreshToken == "" { // offline_access not granted any more
		next.RefreshToken, next.RefreshExpiresAt = "", time.Time{}
	}
	a.tok = next
	if err := a.saveLocked(); err != nil {
		return "", fmt.Errorf("保存 token 失败: %w", err)
	}
	return a.tok.AccessToken, nil
}
