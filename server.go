package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Server wires HTTP routes to the Service.
type Server struct {
	svc     *Service
	sel     *Selection
	cfg     Config
	mock    bool
	resDir  string
	web     fs.FS
	failMu  sync.Mutex
	resFail map[string]resFailure

	csrf   string
	opener *Opener
	noOpen bool
	docker bool
}

type resFailure struct {
	at  time.Time
	err error
}

var (
	msgIDRe  = regexp.MustCompile(`^[A-Za-z0-9_\-]{1,100}$`)
	threadRe = regexp.MustCompile(`^[A-Za-z0-9_\-]{1,100}$`)
	fileIDRe = regexp.MustCompile(`^[0-9a-f]{20}$`)
)

func NewServer(svc *Service, sel *Selection, cfg Config, mock bool, web fs.FS) *Server {
	s := &Server{svc: svc, sel: sel, cfg: cfg, mock: mock, web: web, resDir: filepath.Join(cfg.DataDir, "cache", "res"), resFail: map[string]resFailure{}, csrf: newCSRFToken(), docker: inDocker()}
	s.opener = NewOpener(runtime.GOOS)
	s.noOpen = (s.docker && s.opener.HostURL == "") || strings.EqualFold(os.Getenv("FEISHU_DISABLE_OPEN"), "1")
	if s.cfg.DownloadDir != "" && !cfg.DisableDownload {
		s.svc.dl = NewDownloadManager(s.cfg.DownloadDir, s.cfg.HostDownloadDir, filepath.Join(cfg.DataDir, "downloads", "index.json"), s.dlFetch)
	}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/auth/status", s.handleAuthStatus)
	mux.HandleFunc("GET /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	mux.HandleFunc("GET "+s.callbackPath(), s.handleCallback)
	mux.HandleFunc("GET /api/chats", s.handleChats)
	mux.HandleFunc("GET /api/selection", s.handleGetSelection)
	mux.HandleFunc("POST /api/selection", s.handlePostSelection)
	mux.HandleFunc("PUT /api/selection", s.handlePostSelection)
	mux.HandleFunc("GET /api/topics", s.handleTopics)
	mux.HandleFunc("GET /api/topics/my", s.handleMyTopics)
	mux.HandleFunc("GET /api/topics/{thread_id}/messages", s.handleThread)
	mux.HandleFunc("POST /api/topics/new", s.handleNewTopic)
	mux.HandleFunc("POST /api/topics/{thread_id}/reply", s.handleReply)
	mux.HandleFunc("POST /api/topics/{thread_id}/queue/{queue_id}/send", s.handleQueueSend)
	mux.HandleFunc("DELETE /api/topics/{thread_id}/queue/{queue_id}", s.handleQueueCancel)
	mux.HandleFunc("GET /api/chats/{chat_id}/recent", s.handleRecent)
	mux.HandleFunc("POST /api/upload", s.handleUpload)
	mux.HandleFunc("DELETE /api/upload/{id}", s.handleUnstage)
	mux.HandleFunc("GET /api/resource", s.handleResource)
	mux.HandleFunc("GET /api/csrf", s.handleCSRF)
	mux.HandleFunc("GET /api/downloads", s.handleDownloads)
	mux.HandleFunc("GET /api/files/{id}/content", s.handleFileContent)
	mux.HandleFunc("POST /api/files/open", s.handleFileOpen)
	mux.HandleFunc("POST /api/files/reveal", s.handleFileReveal)
	mux.HandleFunc("POST /api/files/redownload", s.handleFileRedownload)
	mux.HandleFunc("POST /api/files/download", s.handleFileRedownload)
	mux.HandleFunc("POST /api/topics/{thread_id}/materials/expand", s.handleExpandMaterials)
	mux.HandleFunc("GET /api/notifications/settings", s.handleGetNotifySettings)
	mux.HandleFunc("POST /api/notifications/settings", s.handlePostNotifySettings)
	mux.HandleFunc("POST /api/notifications/test", s.handleTestNotify)
	mux.HandleFunc("POST /api/notifications/complete", s.handleNotifyComplete)
	mux.HandleFunc("GET /api/read_state", s.handleGetReadState)
	mux.HandleFunc("POST /api/read_state", s.handlePostReadState)
	if s.mock {
		mux.HandleFunc("GET /api/mock/avatar", handleMockAvatar)
	}
	mux.Handle("/", noCache(http.FileServer(http.FS(s.web))))
	return s.guard(mux)
}

func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		if strings.HasSuffix(r.URL.Path, ".webmanifest") { // not in Go's built-in MIME table
			w.Header().Set("Content-Type", "application/manifest+json; charset=utf-8")
		}
		h.ServeHTTP(w, r)
	})
}

// guard protects the local server against DNS rebinding / cross-site use:
// only localhost Host headers are served, and state-changing requests must
// be same-origin JSON.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		bindHost, _, _ := net.SplitHostPort(s.cfg.Addr)
		if !(host == "localhost" || host == "127.0.0.1" || host == "::1" || (bindHost != "" && bindHost != "0.0.0.0" && bindHost != "::" && host == bindHost)) && !allowAnyHost() {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if o := r.Header.Get("Origin"); o != "" {
				if u, err := url.Parse(o); err != nil || u.Host != r.Host {
					http.Error(w, "forbidden origin", http.StatusForbidden)
					return
				}
			}
			rawUpload := r.URL.Path == "/api/upload" && strings.HasPrefix(r.Header.Get("Content-Type"), "application/octet-stream")
			if !rawUpload && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				http.Error(w, "content-type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		if !strings.HasPrefix(r.URL.Path, "/api/resource") {
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Referrer-Policy", "no-referrer")
			if !strings.HasPrefix(r.URL.Path, "/api/") {
				w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob: https:; media-src 'self'; frame-src 'self'; style-src 'self' 'unsafe-inline'; object-src 'none'; base-uri 'none'")
			}
		}
		next.ServeHTTP(w, r)
	})
}

// allowAnyHost lets users who bind to a LAN address on purpose opt out.
func allowAnyHost() bool { return os.Getenv("ALLOW_ANY_HOST") == "1" }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

type apiError struct {
	// Kind lets the UI react: login_required | scope_missing (the user's token
	// lacks a scope: re-authorize) | app_scope_missing (the app has not been
	// granted the scope in the developer console yet).
	Kind   string   `json:"kind,omitempty"`
	Error  string   `json:"error"`
	Code   int      `json:"code,omitempty"`
	Hint   string   `json:"hint,omitempty"`
	Scopes []string `json:"scopes,omitempty"`
}

// scopeKind classifies "missing permission" API errors.
//
//	99991679: the user_access_token was not granted a required scope
//	230027:   "lack of necessary permissions" (incl. "user authorization not checked")
//	99991672 / 99991676 / 99991673: the app itself does not have the scope
func scopeKind(code int) string {
	switch code {
	case 99991679, 230027, 91204:
		return "scope_missing"
	case 99991672, 99991676, 99991673, 1061073:
		return "app_scope_missing"
	}
	return ""
}

var scopeListRe = regexp.MustCompile(`\[([A-Za-z0-9_:.,\s-]+)\]`)

// requiredScopes pulls the scope names out of messages such as
// "...required one of these privileges: [im:message, im:message.send_as_user]".
func requiredScopes(msg string) []string {
	var out []string
	for _, m := range scopeListRe.FindAllStringSubmatch(msg, -1) {
		for _, p := range strings.Split(m[1], ",") {
			p = strings.TrimSpace(p)
			if strings.Contains(p, ":") && !strings.ContainsAny(p, " \t") && !containsScope(out, p) {
				out = append(out, p)
			}
		}
	}
	return out
}

func hintFor(code int) string {
	switch code {
	case 99991679, 230027:
		return "当前登录授权缺少所需权限（新增/修改权限后必须重新授权）：请确认开发者后台「用户身份权限」已开通并发布版本，然后退出并重新登录授权。"
	case 99991672, 99991676, 99991673, 1061073:
		return "应用缺少所需权限：请在开发者后台「权限管理」开通 README 中列出的权限，并创建版本、发布/审批后生效。"
	case 230018:
		return "群设置不允许你在该群发言。"
	case 230035:
		return "没有发言权限：群可能已禁言、你被限制发言，或受企业通信权限管控。"
	case 230071, 230072:
		return "该群或该消息不支持以话题形式回复。"
	case 230019:
		return "该话题不存在或已被删除。"
	case 230011, 230110:
		return "该消息已被撤回或删除，无法回复。"
	case 230022, 230028:
		return "消息内容触发了敏感信息/DLP 审核（例如明文手机号、邮箱），请修改后重试。"
	case 230025:
		return "消息内容过长。"
	case 232009:
		return "该群已解散。"
	case 230002, 234004:
		return "机器人不在该群中：请先把应用机器人添加到目标群聊。"
	case 230006:
		return "应用未启用机器人能力：请在开发者后台「添加应用能力」中启用机器人。"
	case 230073, 234040:
		return "该话题/消息对机器人不可见（群设置中「新成员可查看历史消息」被关闭时，机器人入群前的内容不可见）。"
	case 231203:
		return "该群类型不支持获取消息（例如开启了「防泄密模式」）。"
	case 234038:
		return "受限模式下不允许下载消息中的资源文件。"
	case 234043:
		return "该类型消息（合并转发子消息、卡片）不支持下载资源。"
	case 234009:
		return "外部群暂不支持该操作。"
	case 234007:
		return "应用未启用机器人能力，飞书的图片/文件上传接口需要它：请在开发者后台「添加应用能力」启用机器人并发布版本；或设置 upload_mode 为 drive（改用云空间链接）。"
	case 234006:
		return "附件超过飞书限制（图片 10 MB、文件 30 MB）。"
	case 234010:
		return "不能上传空文件。"
	case 234011:
		return "不支持的图片格式（支持 JPG/PNG/WEBP/GIF/BMP/ICO/TIFF/HEIC）。"
	case 234039:
		return "图片分辨率超限（GIF ≤ 2000×2000，其他 ≤ 12000×12000）。"
	case 230017:
		return "只能发送由当前身份上传的图片/文件（key 归属上传者）。"
	case 1061004, 91204:
		return "没有云空间的写入/读取权限：请确认已开通 drive:drive 并重新授权。"
	case 1061043, 1061045:
		return "云空间上传失败，请稍后重试并检查文件大小或云空间容量。"
	case 1061061, 1061101:
		return "云空间容量不足。"
	case 1063002, 1063004:
		return "无法设置附件私密权限，文件链接尚未发送；请检查文件所有权及应用授权。"
	case 970003:
		return "没有获取云空间文件元数据的权限（drive:drive 或 drive:drive.metadata:readonly）。"
	case 10003, 10014, 10012:
		return "App ID / App Secret 可能不正确。"
	}
	return ""
}

// resourceHint explains why a resource could not be downloaded. The official
// message-resource API only accepts tenant_access_token, so under user login
// it works only when the app's bot is in the chat.
func resourceHint(code int) string {
	const limit = "飞书「获取消息中的资源文件」接口仅支持应用身份（tenant_access_token），且要求机器人在该群内；用户身份无法下载。解决：把应用机器人加入该群，并开通 im:message:readonly 等应用身份权限；或在飞书中直接查看该文件。"
	switch code {
	case 234004, 234040, 234003, 230002, 99991672, 99991668, 99991663, 99991677, 234001:
		return limit
	}
	if h := hintFor(code); h != "" {
		return h
	}
	return limit
}

func writeErr(w http.ResponseWriter, err error) {
	var fe *FeishuError
	var oe *OAuthError
	var ue *UploadError
	var br badRequest
	switch {
	case errors.Is(err, ErrLoginRequired):
		writeJSON(w, http.StatusUnauthorized, apiError{Error: err.Error(), Kind: "login_required"})
	case errors.As(err, &oe):
		writeJSON(w, http.StatusBadGateway, apiError{Error: oe.Error(), Code: oe.Code, Hint: "请检查 App ID / App Secret、重定向 URL 配置与授权 scope。"})
	case errors.Is(err, ErrNotConfigured):
		writeJSON(w, http.StatusServiceUnavailable, apiError{Error: err.Error(), Hint: "可先使用 --mock 体验界面。"})
	case errors.Is(err, ErrReplyInvalid), errors.As(err, &br):
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error()})
	case errors.Is(err, ErrReplyDisabled), errors.Is(err, ErrUploadDisabled):
		writeJSON(w, http.StatusForbidden, apiError{Error: err.Error()})
	case errors.Is(err, ErrUploadGone):
		writeJSON(w, http.StatusGone, apiError{Error: err.Error()})
	case errors.Is(err, ErrTooLarge):
		writeJSON(w, http.StatusRequestEntityTooLarge, apiError{Error: err.Error()})
	case errors.As(err, &ue):
		ae := apiError{Error: ue.Error()}
		for _, x := range ue.Errs {
			var f *FeishuError
			if errors.As(x, &f) {
				if ae.Code == 0 {
					ae.Code, ae.Hint, ae.Kind = f.Code, hintFor(f.Code), scopeKind(f.Code)
				}
				for _, sc := range requiredScopes(f.Msg) {
					if !containsScope(ae.Scopes, sc) {
						ae.Scopes = append(ae.Scopes, sc)
					}
				}
			}
		}
		writeJSON(w, http.StatusBadGateway, ae)
	case errors.As(err, &fe):
		status := http.StatusBadGateway
		if fe.HTTPStatus == http.StatusNotFound {
			status = http.StatusNotFound
		}
		writeJSON(w, status, apiError{Error: fe.Error(), Code: fe.Code, Hint: hintFor(fe.Code), Kind: scopeKind(fe.Code), Scopes: requiredScopes(fe.Msg)})
	case errors.Is(err, context.Canceled):
		writeJSON(w, 499, apiError{Error: "请求已取消"})
	default:
		log.Printf("error: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiError{Error: err.Error()})
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	dir := ""
	if s.svc.dl != nil {
		dir = s.svc.dl.Base()
		if s.svc.dl.hostBase != "" {
			dir = s.svc.dl.hostBase
		}
	}
	writeJSON(w, 200, map[string]any{
		"mock":         s.mock,
		"configured":   s.mock || s.cfg.Configured(),
		"logged_in":    s.svc.auth.LoggedIn(),
		"version":      AppVersion,
		"downloads":    s.svc.dl != nil,
		"download_dir": dir,
		"opener":       s.openerEnabled(),
		"docker":       s.docker,
	})
}

// callbackPath is the path part of the configured redirect_uri.
func (s *Server) callbackPath() string {
	u, err := url.Parse(s.cfg.RedirectURI)
	if err != nil || u.Path == "" || u.Path == "/" {
		return "/callback"
	}
	return u.Path
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	st := s.svc.auth.Status()
	driveLimit := DriveMaxBytes
	if _, ok := s.svc.api.(DriveTopicUploader); ok {
		driveLimit = FileMaxBytes
	}
	writeJSON(w, 200, map[string]any{
		"logged_in":    st.LoggedIn,
		"user":         st.User,
		"expires_at":   st.ExpiresAt,
		"can_refresh":  st.CanRefresh,
		"mock":         s.mock,
		"configured":   s.mock || s.cfg.Configured(),
		"redirect_uri": s.cfg.RedirectURI,
		"scopes":       s.cfg.Scopes,
		"scope":        st.Scope,
		// reply_enabled: composer allowed by config; can_reply: the current
		// token was granted the send scope (unknown scope list = assume yes)
		"reply_enabled": s.cfg.ReplyEnabled(),
		"can_reply":     s.cfg.ReplyEnabled() && (st.Scope == "" || containsScope(strings.Fields(st.Scope), ScopeSendAsUser)),
		// attachments: allowed by config / granted to the current token
		"upload_mode":    s.cfg.uploadModeOrAuto(),
		"upload_enabled": s.cfg.UploadEnabled(),
		"can_upload":     s.cfg.UploadEnabled() && s.cfg.uploadScopeOK(strings.Fields(st.Scope)),
		"limits":         map[string]int{"image": ImageMaxBytes, "file": FileMaxBytes, "drive": driveLimit, "max_attachments": maxAttachments},
	})
}

const stateCookie = "ft_oauth_state"

func authPage(w http.ResponseWriter, code int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	fmt.Fprintf(w, `<!doctype html><html lang="zh-CN"><meta charset="utf-8"><title>%s</title><body style="font:15px/1.7 system-ui,sans-serif;max-width:640px;margin:60px auto;padding:0 20px"><h2>%s</h2><p style="white-space:pre-wrap">%s</p><p><a href="/">返回首页</a></p></body></html>`,
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(msg))
}

// handleLogin starts the OAuth authorization-code flow.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.mock && !s.cfg.Configured() {
		authPage(w, http.StatusServiceUnavailable, "尚未配置飞书凭证", ErrNotConfigured.Error())
		return
	}
	// The state cookie is host-bound: the app must be opened on the same host
	// as the redirect_uri (localhost), otherwise the callback cannot see it.
	if ru, err := url.Parse(s.cfg.RedirectURI); err == nil && ru.Host != "" && !strings.EqualFold(hostOnly(ru.Host), hostOnly(r.Host)) {
		authPage(w, http.StatusBadRequest, "访问地址与重定向 URL 不一致",
			fmt.Sprintf("请使用 %s://%s/ 打开本应用后再登录（当前为 %s）。\n登录回调地址为 %s。", ru.Scheme, ru.Host, r.Host, s.cfg.RedirectURI))
		return
	}
	state := s.svc.auth.NewState()
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: state, Path: "/", MaxAge: int(stateTTL.Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode})
	if s.mock {
		http.Redirect(w, r, s.callbackPath()+"?code=mock&state="+url.QueryEscape(state), http.StatusFound)
		return
	}
	http.Redirect(w, r, s.svc.auth.AuthorizeURL(state), http.StatusFound)
}

func hostOnly(h string) string {
	if x, _, err := net.SplitHostPort(h); err == nil {
		return strings.Trim(x, "[]")
	}
	return strings.Trim(h, "[]")
}

// handleCallback receives ?code=&state= from Feishu.
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, cerr := r.Cookie(stateCookie)
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	state := q.Get("state")
	// CSRF: the state must be one we issued (single use) AND match this browser's cookie
	if cerr != nil || c.Value == "" || c.Value != state || !s.svc.auth.ConsumeState(state) {
		authPage(w, http.StatusBadRequest, "登录失败：state 校验未通过", "state 缺失、已过期或与本浏览器不匹配（可能是重复回调或跨站请求）。请返回首页重新点击「用飞书登录」。")
		return
	}
	if e := q.Get("error"); e != "" {
		authPage(w, http.StatusBadRequest, "登录失败：授权被拒绝或出错", e+" "+q.Get("error_description"))
		return
	}
	code := q.Get("code")
	if code == "" {
		authPage(w, http.StatusBadRequest, "登录失败", "回调中缺少授权码 code。")
		return
	}
	if err := s.svc.auth.Callback(r.Context(), code); err != nil {
		log.Printf("oauth callback failed: %v", err)
		msg := err.Error()
		var oe *OAuthError
		if errors.As(err, &oe) {
			msg += "\n请确认：redirect_uri 已加入应用「安全设置 → 重定向 URL」；scope 已在「权限管理」中开通（用户身份）；App ID/Secret 正确。授权码 5 分钟内有效且只能使用一次。"
		}
		authPage(w, http.StatusBadGateway, "登录失败：换取 user_access_token 出错", msg)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.svc.auth.Logout()
	writeJSON(w, 200, map[string]any{"logged_in": false})
}

func (s *Server) handleChats(w http.ResponseWriter, r *http.Request) {
	chats, err := s.svc.Chats(r.Context(), r.URL.Query().Get("refresh") == "1")
	if err != nil {
		writeErr(w, err)
		return
	}
	var daidainiao []Chat
	for _, c := range chats {
		if strings.Contains(c.Name, "呆呆鸟") {
			daidainiao = append(daidainiao, c)
		}
	}
	if len(daidainiao) > 0 {
		chats = daidainiao
	}
	if chats == nil {
		chats = []Chat{}
	}
	writeJSON(w, 200, map[string]any{"chats": chats})
}

func (s *Server) handleGetSelection(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"chat_ids": s.sel.Get()})
}

func (s *Server) handlePostSelection(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChatIDs []string `json:"chat_ids"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, 400, apiError{Error: "请求体必须是 JSON: {\"chat_ids\": [...]}"})
		return
	}
	if err := s.sel.Set(body.ChatIDs); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"chat_ids": s.sel.Get()})
}

func (s *Server) handleMyTopics(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") == "1"
	resp, err := s.svc.MyTopics(r.Context(), refresh)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleTopics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("my") == "1" {
		s.handleMyTopics(w, r)
		return
	}
	refresh := q.Get("refresh") == "1"
	chatID := q.Get("chat_id")
	if chatID == "" {
		// all selected chats, merged
		merged := &TopicsResp{Topics: []Topic{}}
		for _, id := range s.sel.Get() {
			resp, err := s.svc.Topics(r.Context(), id, refresh)
			if err != nil {
				merged.Warnings = append(merged.Warnings, fmt.Sprintf("%s: %v", id, err))
				continue
			}
			merged.Topics = append(merged.Topics, resp.Topics...)
			merged.Warnings = append(merged.Warnings, resp.Warnings...)
		}
		sortTopics(merged.Topics)
		writeJSON(w, 200, merged)
		return
	}
	if !msgIDRe.MatchString(chatID) {
		writeJSON(w, 400, apiError{Error: "chat_id 非法"})
		return
	}
	resp, err := s.svc.Topics(r.Context(), chatID, refresh)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, resp)
}

func sortTopics(t []Topic) {
	sort.SliceStable(t, func(i, j int) bool { return t[i].LastTime > t[j].LastTime })
}

func (s *Server) handleThread(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("thread_id")
	if !threadRe.MatchString(id) {
		writeJSON(w, 400, apiError{Error: "thread_id 非法"})
		return
	}
	ctx := r.Context()
	if r.URL.Query().Get("refresh_names") == "1" {
		ctx = context.WithValue(ctx, refreshMaterialNamesKey{}, true)
	}
	resp, err := s.svc.Thread(ctx, id, r.URL.Query().Get("refresh") == "1")
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, resp)
}

// handleReply sends a thread reply as the logged-in user.
func (s *Server) handleReply(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("thread_id")
	if !threadRe.MatchString(id) {
		writeJSON(w, 400, apiError{Error: "thread_id 非法"})
		return
	}
	var req ReplyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, 400, apiError{Error: "请求体不是有效的 JSON"})
		return
	}
	res, err := s.svc.ReplyFull(r.Context(), id, req)
	if err != nil {
		writeErr(w, err)
		return
	}
	var first *Message
	if len(res.Messages) > 0 {
		first = &res.Messages[0]
	}
	writeJSON(w, 200, map[string]any{
		"message":    first,
		"messages":   res.Messages,
		"thread_id":  res.ThreadID,
		"warnings":   res.Warnings,
		"failed":     res.Failed,
		"queued":     res.Queued,
		"queue_item": res.QueueItem,
		"queue":      res.Queue,
		"bot_busy":   res.BotBusy,
		"bot_status": res.BotStatus,
	})
}

func (s *Server) handleQueueSend(w http.ResponseWriter, r *http.Request) {
	tid := r.PathValue("thread_id")
	qid := r.PathValue("queue_id")
	if !threadRe.MatchString(tid) || !msgIDRe.MatchString(qid) {
		writeJSON(w, 400, apiError{Error: "参数非法"})
		return
	}
	res, err := s.svc.SendQueuedNow(r.Context(), tid, qid)
	if err != nil {
		writeErr(w, err)
		return
	}
	var first *Message
	if len(res.Messages) > 0 {
		first = &res.Messages[0]
	}
	writeJSON(w, 200, map[string]any{
		"message":   first,
		"messages":  res.Messages,
		"thread_id": res.ThreadID,
		"warnings":  res.Warnings,
		"failed":    res.Failed,
		"queue":     res.Queue,
	})
}

func (s *Server) handleQueueCancel(w http.ResponseWriter, r *http.Request) {
	tid := r.PathValue("thread_id")
	qid := r.PathValue("queue_id")
	if !threadRe.MatchString(tid) || !msgIDRe.MatchString(qid) {
		writeJSON(w, 400, apiError{Error: "参数非法"})
		return
	}
	keepStaged := r.URL.Query().Get("reedit") == "1"
	removed, q, err := s.svc.CancelQueued(tid, qid, keepStaged)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok":      true,
		"removed": removed,
		"queue":   q,
	})
}

// handleNewTopic starts a topic (root message or in-thread reply to an
// existing message) as the logged-in user.
func (s *Server) handleNewTopic(w http.ResponseWriter, r *http.Request) {
	var req NewTopicRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, 400, apiError{Error: "请求体不是有效的 JSON"})
		return
	}
	res, err := s.svc.NewTopic(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) handleRecent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("chat_id")
	if !msgIDRe.MatchString(id) {
		writeJSON(w, 400, apiError{Error: "chat_id 非法"})
		return
	}
	items, err := s.svc.Recent(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"messages": items})
}

// handleUpload receives one attachment from the browser (raw body, name in
// the X-File-Name header, percent-encoded) and stages it on disk until the
// message is sent.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	name := r.Header.Get("X-File-Name")
	if n, err := url.PathUnescape(name); err == nil {
		name = n
	}
	st, err := s.svc.Stage(name, http.MaxBytesReader(w, r.Body, FileMaxBytes+1<<20))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			err = fmt.Errorf("%w：文件不能超过 30 MB（飞书限制）", ErrTooLarge)
		}
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, st)
}

func (s *Server) handleUnstage(w http.ResponseWriter, r *http.Request) {
	s.svc.Unstage(r.PathValue("id"))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------- resource proxy ----------

type resMeta struct {
	ContentType string `json:"content_type"`
	Filename    string `json:"filename"`
}

// inlineTypes may be rendered by the browser; everything else is forced to download.
var inlineTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "image/bmp": true,
	"application/pdf": true, "text/plain": true,
	"video/mp4": true, "video/webm": true, "audio/mpeg": true, "audio/ogg": true, "audio/mp4": true, "audio/wav": true, "audio/webm": true,
}

var textExts = map[string]bool{"txt": true, "md": true, "csv": true, "json": true, "log": true, "xml": true, "yaml": true, "yml": true, "ini": true, "sql": true, "go": true, "py": true, "js": true, "ts": true, "java": true, "sh": true, "html": true, "css": true}

func (s *Server) handleResource(w http.ResponseWriter, r *http.Request) {
	if !s.svc.auth.LoggedIn() {
		writeErr(w, ErrLoginRequired)
		return
	}
	q := r.URL.Query()
	messageID, fileKey, typ := q.Get("message_id"), q.Get("file_key"), q.Get("type")
	if !msgIDRe.MatchString(messageID) || !keyRe.MatchString(fileKey) || (typ != "image" && typ != "file") {
		writeJSON(w, 400, apiError{Error: "参数需要 message_id、file_key、type(image|file)"})
		return
	}
	nameHint := q.Get("name")
	if dm := s.svc.dl; dm != nil {
		// registered conversation file: served from the local download folder
		v, p, found, err := dm.Ensure(r.Context(), typ, messageID, fileKey)
		if found {
			if err != nil {
				s.writeResourceErr(w, err)
				return
			}
			s.serveFile(w, r, p, v.Name, v.Mime)
			return
		}
	}
	sum := sha256.Sum256([]byte(messageID + "|" + fileKey + "|" + typ))
	base := filepath.Join(s.resDir, hex.EncodeToString(sum[:]))
	meta, err := s.ensureResource(r.Context(), base, messageID, fileKey, typ)
	if err != nil {
		s.writeResourceErr(w, err)
		return
	}
	name := meta.Filename
	if name == "" {
		name = nameHint
	}
	s.serveFile(w, r, base+".bin", name, meta.ContentType)
}

func (s *Server) writeResourceErr(w http.ResponseWriter, err error) {
	var fe *FeishuError
	if errors.As(err, &fe) {
		status := http.StatusBadGateway
		if fe.HTTPStatus == http.StatusUnauthorized {
			status = http.StatusUnauthorized
		}
		writeJSON(w, status, apiError{Error: fe.Error(), Code: fe.Code, Hint: resourceHint(fe.Code)})
		return
	}
	if errors.Is(err, ErrLoginRequired) || errors.Is(err, ErrNotConfigured) {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusBadGateway, apiError{Error: err.Error()})
}

func (s *Server) ensureResource(ctx context.Context, base, messageID, fileKey, typ string) (resMeta, error) {
	var meta resMeta
	if b, err := os.ReadFile(base + ".json"); err == nil {
		if _, err := os.Stat(base + ".bin"); err == nil && json.Unmarshal(b, &meta) == nil {
			return meta, nil
		}
	}
	defer s.svc.lock("res:" + base)()
	if b, err := os.ReadFile(base + ".json"); err == nil {
		if _, err := os.Stat(base + ".bin"); err == nil && json.Unmarshal(b, &meta) == nil {
			return meta, nil
		}
	}
	s.failMu.Lock()
	if t, ok := s.resFail[base]; ok && time.Since(t.at) < 2*time.Minute {
		s.failMu.Unlock()
		return meta, t.err
	}
	s.failMu.Unlock()
	d, err := s.svc.api.Download(ctx, messageID, fileKey, typ)
	if err != nil {
		var fe *FeishuError
		if errors.As(err, &fe) { // remember permanent API refusals briefly
			s.failMu.Lock()
			s.resFail[base] = resFailure{at: time.Now(), err: err}
			s.failMu.Unlock()
		}
		return meta, err
	}
	defer d.Body.Close()
	if err := os.MkdirAll(s.resDir, 0o755); err != nil {
		return meta, err
	}
	tmp, err := os.CreateTemp(s.resDir, ".dl-*")
	if err != nil {
		return meta, err
	}
	defer os.Remove(tmp.Name())
	if _, err := copyWithDownloadProgress(ctx, tmp, io.LimitReader(d.Body, 101<<20), d.Size); err != nil {
		tmp.Close()
		return meta, err
	}
	if err := tmp.Close(); err != nil {
		return meta, err
	}
	meta = resMeta{ContentType: d.ContentType, Filename: d.Filename}
	mb, _ := json.Marshal(meta)
	if err := os.Rename(tmp.Name(), base+".bin"); err != nil {
		return meta, err
	}
	if err := writeFileAtomic(base+".json", mb, 0o600); err != nil {
		return meta, err
	}
	return meta, nil
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		h.ServeHTTP(sw, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			log.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, sw.code, time.Since(start).Round(time.Millisecond))
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) {
	w.code = c
	w.ResponseWriter.WriteHeader(c)
}

func (s *Server) webBaseURL(r *http.Request) string {
	if s.cfg.RedirectURI != "" {
		if u, err := url.Parse(s.cfg.RedirectURI); err == nil && u.Host != "" {
			return u.Scheme + "://" + u.Host
		}
	}
	proto := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		proto = "https"
	}
	host := r.Host
	if host == "" {
		host = "localhost:8088"
	}
	return proto + "://" + host
}

func (s *Server) handleGetNotifySettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.svc.NotifySettings())
}

func (s *Server) handlePostNotifySettings(w http.ResponseWriter, r *http.Request) {
	var ns NotificationSettings
	if err := json.NewDecoder(r.Body).Decode(&ns); err != nil {
		writeJSON(w, 400, apiError{Error: "请求体不是有效的 JSON"})
		return
	}
	s.svc.UpdateNotifySettings(ns)
	writeJSON(w, 200, s.svc.NotifySettings())
}

func (s *Server) handleTestNotify(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.requireLogin(); err != nil {
		writeErr(w, err)
		return
	}
	userOpenID := s.svc.auth.UserID()
	if userOpenID == "" {
		writeJSON(w, 400, apiError{Error: "当前未获取到登录用户的 open_id"})
		return
	}
	if err := s.svc.TestFeishuNotice(r.Context(), userOpenID, s.webBaseURL(r)); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "msg": "测试卡片已成功发送至飞书单聊"})
}

func (s *Server) handleNotifyComplete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ThreadID   string `json:"thread_id"`
		ChatName   string `json:"chat_name"`
		TopicTitle string `json:"topic_title"`
		Snippet    string `json:"snippet"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ThreadID == "" {
		writeJSON(w, 400, apiError{Error: "缺少 thread_id"})
		return
	}
	notified, err := s.svc.NotifyTaskComplete(r.Context(), req.ThreadID, req.ChatName, req.TopicTitle, s.svc.auth.UserID(), req.Snippet, s.webBaseURL(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "notified": notified})
}

func (s *Server) handleGetReadState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.svc.ReadState())
}

func (s *Server) handlePostReadState(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ThreadID    string           `json:"thread_id"`
		ReadTime    int64            `json:"read_time"`
		Initialized bool             `json:"initialized"`
		Threads     map[string]int64 `json:"threads"`
		All         bool             `json:"all"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, apiError{Error: "请求体不是有效的 JSON"})
		return
	}
	if body.ThreadID != "" {
		readTime := body.ReadTime
		if readTime <= 0 {
			readTime = time.Now().UnixMilli()
		}
		writeJSON(w, 200, s.svc.MarkTopicRead(body.ThreadID, readTime))
		return
	}
	rs := ReadState{
		Initialized: body.Initialized || body.All,
		Threads:     body.Threads,
	}
	writeJSON(w, 200, s.svc.UpdateReadState(rs))
}
