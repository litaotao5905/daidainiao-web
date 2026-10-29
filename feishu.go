package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// API is the subset of the Feishu Open API used by the app. It is
// implemented by FeishuClient (real) and MockAPI (sample data).
type API interface {
	DrivePermissions(context.Context, string, string) (*DrivePermissionSnapshot, error)
	DriveClosePrivate(context.Context, string, string) error
	DriveList(context.Context, string, string) (*DriveListPage, error)
	WikiResolve(context.Context, string) (*DriveResource, error)
	DriveMetas(context.Context, []DriveRef) (map[string]DriveResource, error)
	ListChats(ctx context.Context, pageToken string) (*ChatPage, error)
	ListMessages(ctx context.Context, containerType, containerID, pageToken string, desc bool) (*MsgPage, error)
	// GetMessage returns the message plus, for merge_forward messages, all of
	// its sub-messages (they carry UpperMessageID).
	GetMessage(ctx context.Context, messageID string) ([]RawMessage, error)
	// ReplyMessage replies to messageID as the logged-in user.
	ReplyMessage(ctx context.Context, messageID, msgType, content, uuid string, inThread bool) (*RawMessage, error)
	// SendMessage sends a new root message to a chat as the logged-in user
	// (im/v1/messages?receive_id_type=chat_id).
	SendMessage(ctx context.Context, chatID, msgType, content, uuid string) (*RawMessage, error)
	// UploadIM uploads an image (kind "image", im/v1/images) or a file (kind
	// "file", im/v1/files) with the user identity and returns its key.
	UploadIM(ctx context.Context, kind, name string, data []byte) (string, error)
	// DriveUpload puts a file into the user's Drive root folder ("我的空间")
	// and returns the file token.
	DriveUpload(ctx context.Context, name string, data []byte) (string, error)
	// DriveURL returns the access link of a Drive file token.
	DriveURL(ctx context.Context, token string) (string, error)
	// DriveMeta returns the metadata (title, latest_modify_time) of a Drive file token.
	DriveMeta(ctx context.Context, token string) (*DriveDocMeta, error)
	// DriveShareChat grants the destination chat view access to one Drive file.
	// The composer calls it only when the sender turns encryption off.
	DriveShareChat(ctx context.Context, token, chatID string) error
	// DriveShareMember grants a specific member (user or bot open_id) access to the Drive file.
	DriveShareMember(ctx context.Context, token, memberType, memberID, perm string) (*DrivePermissionMember, error)
	// DriveSetLinkShare updates the link sharing permission of the Drive file ("closed", "tenant_readable", etc.).
	DriveSetLinkShare(ctx context.Context, token, entity string) error
	// DriveDownload downloads a Drive file (drive/v1/files/:token/download,
	// user identity).
	DriveDownload(ctx context.Context, token string) (*Download, error)
	DriveExport(ctx context.Context, token, kind string) (*Download, error)
	BatchUsers(ctx context.Context, openIDs []string) ([]User, error)
	Download(ctx context.Context, messageID, fileKey, resType string) (*Download, error)
	// SendBotCard sends an interactive card to a user via tenant_access_token (app bot).
	SendBotCard(ctx context.Context, openID string, card any) error
}

// DriveDocMeta contains metadata about a Feishu Drive document.
type DriveDocMeta struct {
	OwnerID          string `json:"owner_id"`
	DocToken         string `json:"doc_token"`
	DocType          string `json:"doc_type"`
	Title            string `json:"title"`
	LatestModifyTime int64  `json:"latest_modify_time"`
	URL              string `json:"url"`
}

type ChatPage struct {
	Items     []Chat
	HasMore   bool
	PageToken string
}

type MsgPage struct {
	Items     []RawMessage
	HasMore   bool
	PageToken string
}

type Download struct {
	Body        io.ReadCloser
	ContentType string
	Filename    string
	Size        int64
}

// ErrNotConfigured is returned when no App ID / Secret is available.
var ErrNotConfigured = errors.New("未配置飞书 App ID / App Secret（请设置 config.json 或环境变量 FEISHU_APP_ID / FEISHU_APP_SECRET，或使用 --mock）")

// FeishuError is an error reported by the Open API.
type FeishuError struct {
	Code       int
	Msg        string
	HTTPStatus int
}

func (e *FeishuError) Error() string {
	return fmt.Sprintf("飞书接口错误 code=%d msg=%s (HTTP %d)", e.Code, e.Msg, e.HTTPStatus)
}

// identity selects which access token a request is made with.
type identity int

const (
	idUser   identity = iota // user_access_token (OAuth login) — used for everything
	idTenant                 // tenant_access_token — only used for message resource download
)

func (i identity) String() string {
	if i == idTenant {
		return "应用身份(tenant_access_token)"
	}
	return "用户身份(user_access_token)"
}

// FeishuClient talks to the real Open API. All reads use the logged-in
// user's user_access_token; the tenant token is only a fallback for resource
// downloads (the documented resource API does not accept user tokens).
type FeishuClient struct {
	base      string
	appID     string
	appSecret string
	auth      *Auth
	resAuth   string
	hc        *http.Client

	mu       sync.Mutex
	folderMu sync.Mutex
	token    string
	expire   time.Time

	rlMu   sync.Mutex
	rlNext time.Time
	gap    time.Duration
	// noCardParam is set once the server rejected card_msg_content_type and fallback succeeded.
	noCardParam atomic.Bool
	// rootTok caches the user's Drive root folder token.
	rootTok  string
	rootUser string
}

// DriveTopicUploader is implemented by FeishuClient to upload attachments into
// a unified cache folder ("网页呆呆鸟/<topicFolder>") in Feishu Drive.
type DriveTopicUploader interface {
	DriveUploadToTopic(ctx context.Context, topicFolder, name string, data []byte) (fileToken, folderPath string, err error)
}

const DrivePrivateRootName = "网页呆呆鸟"

func NewFeishuClient(base, appID, appSecret string, auth *Auth, resourceAuth string) *FeishuClient {
	if resourceAuth == "" {
		resourceAuth = "auto"
	}
	return &FeishuClient{
		base:      strings.TrimRight(base, "/"),
		appID:     appID,
		appSecret: appSecret,
		auth:      auth,
		resAuth:   resourceAuth,
		hc:        &http.Client{},
		gap:       25 * time.Millisecond, // ~40 req/s, below the documented 50/s
	}
}

const apiPrefix = "/open-apis"

func (c *FeishuClient) wait(ctx context.Context) error {
	c.rlMu.Lock()
	now := time.Now()
	at := c.rlNext
	if at.Before(now) {
		at = now
	}
	c.rlNext = at.Add(c.gap)
	c.rlMu.Unlock()
	return sleepCtx(ctx, time.Until(at))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func backoff(attempt int, resp *http.Response) time.Duration {
	if resp != nil {
		if v := resp.Header.Get("Retry-After"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 30 {
				return time.Duration(n) * time.Second
			}
		}
		// Feishu: seconds until the rate-limit window resets.
		if v := resp.Header.Get("x-ogw-ratelimit-reset"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 30 {
				return time.Duration(n)*time.Second + 100*time.Millisecond
			}
		}
	}
	d := 500 * time.Millisecond << uint(attempt)
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return d + time.Duration(rand.Intn(250))*time.Millisecond
}

type tenantTokenResp struct {
	Code              int    `json:"code"`
	Msg               string `json:"msg"`
	TenantAccessToken string `json:"tenant_access_token"`
	Expire            int    `json:"expire"`
}

// accessToken returns an access token for the given identity.
func (c *FeishuClient) accessToken(ctx context.Context, id identity) (string, error) {
	if id == idUser {
		if c.auth == nil {
			return "", ErrLoginRequired
		}
		return c.auth.AccessToken(ctx)
	}
	return c.getTenantToken(ctx)
}

// renew obtains a fresh token after the API rejected `stale`.
func (c *FeishuClient) renew(ctx context.Context, id identity, stale string) error {
	if id == idUser {
		if c.auth == nil {
			return ErrLoginRequired
		}
		_, err := c.auth.Refresh(ctx, stale)
		return err
	}
	c.invalidate()
	return nil
}

// getTenantToken returns a cached tenant_access_token, refreshing when needed.
func (c *FeishuClient) getTenantToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expire) {
		return c.token, nil
	}
	if c.appID == "" || c.appSecret == "" {
		return "", ErrNotConfigured
	}
	body, _ := json.Marshal(map[string]string{"app_id": c.appID, "app_secret": c.appSecret})
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if err := c.wait(ctx); err != nil {
			return "", err
		}
		rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		req, _ := http.NewRequestWithContext(rctx, http.MethodPost, c.base+apiPrefix+"/auth/v3/tenant_access_token/internal", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		resp, err := c.hc.Do(req)
		if err != nil {
			cancel()
			last = fmt.Errorf("获取 tenant_access_token 网络错误: %w", scrubErr(err))
			if e := sleepCtx(ctx, backoff(attempt, nil)); e != nil {
				return "", e
			}
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		var tr tenantTokenResp
		_ = json.Unmarshal(b, &tr)
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			last = &FeishuError{Code: tr.Code, Msg: tr.Msg, HTTPStatus: resp.StatusCode}
			if e := sleepCtx(ctx, backoff(attempt, resp)); e != nil {
				return "", e
			}
			continue
		}
		if tr.Code != 0 || tr.TenantAccessToken == "" {
			return "", &FeishuError{Code: tr.Code, Msg: "获取 tenant_access_token 失败（请检查 App ID / App Secret）: " + tr.Msg, HTTPStatus: resp.StatusCode}
		}
		c.token = tr.TenantAccessToken
		ttl := time.Duration(tr.Expire) * time.Second
		if ttl <= 0 {
			ttl = 30 * time.Minute
		}
		margin := 2 * time.Minute
		if ttl < 2*margin {
			margin = ttl / 4
		}
		c.expire = time.Now().Add(ttl - margin)
		return c.token, nil
	}
	return "", last
}

// scrubErr strips URLs from transport errors (defensive: never leak anything).
func scrubErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}

func (c *FeishuClient) invalidate() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

// roundTrip performs one authenticated request, retrying on transport errors,
// HTTP 429 and 5xx with backoff. The caller owns resp.Body.
func (c *FeishuClient) roundTrip(ctx context.Context, id identity, method, path string, q url.Values) (*http.Response, string, error) {
	return c.roundTripBody(ctx, id, method, path, q, nil, true)
}

// roundTripBody is roundTrip with an optional JSON body. When idempotent is
// false (sending a message without a dedupe uuid) a 5xx answer is returned to
// the caller instead of being retried, so a message is never sent twice.
func (c *FeishuClient) roundTripBody(ctx context.Context, id identity, method, path string, q url.Values, body []byte, idempotent bool) (*http.Response, string, error) {
	return c.roundTripCT(ctx, id, method, path, q, body, "application/json; charset=utf-8", idempotent)
}

func (c *FeishuClient) roundTripCT(ctx context.Context, id identity, method, path string, q url.Values, body []byte, ctype string, idempotent bool) (*http.Response, string, error) {
	var last error
	for attempt := 0; attempt < 5; attempt++ {
		tok, err := c.accessToken(ctx, id)
		if err != nil {
			return nil, "", err
		}
		if err := c.wait(ctx); err != nil {
			return nil, "", err
		}
		u := c.base + apiPrefix + path
		if len(q) > 0 {
			u += "?" + q.Encode()
		}
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		if body != nil {
			req.Header.Set("Content-Type", ctype)
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, "", ctx.Err()
			}
			last = fmt.Errorf("请求飞书接口失败: %w", scrubErr(err))
			if !idempotent {
				return nil, "", last // the request may or may not have been processed
			}
			if e := sleepCtx(ctx, backoff(attempt, nil)); e != nil {
				return nil, "", e
			}
			continue
		}
		if resp.StatusCode >= 500 && !idempotent {
			return resp, tok, nil
		}
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			d := backoff(attempt, resp)
			last = &FeishuError{Code: 0, Msg: "服务繁忙或触发频率限制", HTTPStatus: resp.StatusCode}
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			if e := sleepCtx(ctx, d); e != nil {
				return nil, "", e
			}
			continue
		}
		return resp, tok, nil
	}
	return nil, "", last
}

type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func isTokenInvalid(code int) bool {
	return code == 99991663 || code == 99991661 || code == 99991668 || code == 99991677
}

func isRateLimited(code int) bool {
	return code == 99991400 || code == 230020 || code == 11232
}

// call performs a JSON GET and decodes envelope.data into out.
func (c *FeishuClient) call(ctx context.Context, id identity, path string, q url.Values, out any) error {
	return c.callReq(ctx, id, http.MethodGet, path, q, nil, true, out)
}

// callReq performs a JSON request and decodes envelope.data into out.
func (c *FeishuClient) callReq(ctx context.Context, id identity, method, path string, q url.Values, body []byte, idempotent bool, out any) error {
	return c.callReqCT(ctx, id, method, path, q, body, "application/json; charset=utf-8", 30*time.Second, idempotent, out)
}

func (c *FeishuClient) callReqCT(ctx context.Context, id identity, method, path string, q url.Values, body []byte, ctype string, timeout time.Duration, idempotent bool, out any) error {
	for attempt := 0; ; attempt++ {
		rctx, cancel := context.WithTimeout(ctx, timeout)
		resp, used, err := c.roundTripCT(rctx, id, method, path, q, body, ctype, idempotent)
		if err != nil {
			cancel()
			return err
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		cancel()
		var env envelope
		if jerr := json.Unmarshal(b, &env); jerr != nil {
			return &FeishuError{Msg: "无法解析响应: " + truncate(string(b), 120), HTTPStatus: resp.StatusCode}
		}
		if env.Code == 0 && resp.StatusCode < 400 {
			if out != nil && len(env.Data) > 0 {
				return json.Unmarshal(env.Data, out)
			}
			return nil
		}
		if isTokenInvalid(env.Code) && attempt < 1 {
			if err := c.renew(ctx, id, used); err != nil {
				return err
			}
			continue
		}
		if isRateLimited(env.Code) && attempt < 4 {
			if e := sleepCtx(ctx, backoff(attempt, resp)); e != nil {
				return e
			}
			continue
		}
		return &FeishuError{Code: env.Code, Msg: env.Msg, HTTPStatus: resp.StatusCode}
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ListChats: GET im/v1/chats (chats the bot belongs to).
func (c *FeishuClient) ListChats(ctx context.Context, pageToken string) (*ChatPage, error) {
	q := url.Values{}
	q.Set("page_size", "100")
	q.Set("sort_type", "ByCreateTimeAsc")
	if pageToken != "" {
		q.Set("page_token", pageToken)
	}
	var d struct {
		Items     []Chat `json:"items"`
		HasMore   bool   `json:"has_more"`
		PageToken string `json:"page_token"`
	}
	if err := c.call(ctx, idUser, "/im/v1/chats", q, &d); err != nil {
		return nil, err
	}
	return &ChatPage{Items: d.Items, HasMore: d.HasMore, PageToken: d.PageToken}, nil
}

// ListMessages: GET im/v1/messages (container_id_type=chat|thread).
func (c *FeishuClient) ListMessages(ctx context.Context, containerType, containerID, pageToken string, desc bool) (*MsgPage, error) {
	q := url.Values{}
	q.Set("container_id_type", containerType)
	q.Set("container_id", containerID)
	q.Set("page_size", "50")
	if containerType == "chat" {
		if desc {
			q.Set("sort_type", "ByCreateTimeDesc")
		} else {
			q.Set("sort_type", "ByCreateTimeAsc")
		}
	}
	if pageToken != "" {
		q.Set("page_token", pageToken)
	}
	var d struct {
		Items     []RawMessage `json:"items"`
		HasMore   bool         `json:"has_more"`
		PageToken string       `json:"page_token"`
	}
	if err := c.callCards(ctx, "/im/v1/messages", q, &d); err != nil {
		return nil, err
	}
	return &MsgPage{Items: d.Items, HasMore: d.HasMore, PageToken: d.PageToken}, nil
}

// callCards is a GET that asks for the original card JSON of interactive
// messages (card_msg_content_type=user_card_content). Without it the API
// returns a simplified structure in which many cards are only the placeholder
// "请升级至最新版本客户端，以查看内容". If the server rejects the parameter
// (230001) and omitting it succeeds, subsequent calls omit it.
func (c *FeishuClient) callCards(ctx context.Context, path string, q url.Values, out any) error {
	if !c.noCardParam.Load() {
		q2 := make(url.Values, len(q)+1)
		for k, v := range q {
			q2[k] = v
		}
		q2.Set("card_msg_content_type", "user_card_content")
		err := c.call(ctx, idUser, path, q2, out)
		var fe *FeishuError
		if err == nil || !errors.As(err, &fe) || fe.Code != 230001 {
			return err
		}
		// If the error message indicates an invalid parameter other than card_msg_content_type,
		// do not fallback and do not disable card_msg_content_type.
		msg := strings.ToLower(fe.Msg)
		if strings.Contains(msg, "container_id") || strings.Contains(msg, "page_token") || strings.Contains(msg, "chat_id") || strings.Contains(msg, "message_id") {
			return err
		}
		// Try fallback without card_msg_content_type. Only disable the parameter if fallback actually succeeds.
		if errFallback := c.call(ctx, idUser, path, q, out); errFallback == nil {
			c.noCardParam.Store(true)
			return nil
		} else {
			return err
		}
	}
	return c.call(ctx, idUser, path, q, out)
}

// GetMessage: GET im/v1/messages/:message_id. For merge_forward messages the
// items hold the message itself followed by all sub-messages.
func (c *FeishuClient) GetMessage(ctx context.Context, messageID string) ([]RawMessage, error) {
	var d struct {
		Items []RawMessage `json:"items"`
	}
	q := url.Values{}
	if err := c.callCards(ctx, "/im/v1/messages/"+url.PathEscape(messageID), q, &d); err != nil {
		return nil, err
	}
	return d.Items, nil
}

// ReplyMessage: POST im/v1/messages/:message_id/reply with the user token.
// Required user scopes: im:message + im:message.send_as_user.
func (c *FeishuClient) ReplyMessage(ctx context.Context, messageID, msgType, content, uuid string, inThread bool) (*RawMessage, error) {
	payload := map[string]any{"msg_type": msgType, "content": content, "reply_in_thread": inThread}
	if uuid != "" {
		payload["uuid"] = uuid
	}
	body, _ := json.Marshal(payload)
	var out RawMessage
	if err := c.callReq(ctx, idUser, http.MethodPost, "/im/v1/messages/"+url.PathEscape(messageID)+"/reply", nil, body, uuid != "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SendMessage: POST im/v1/messages?receive_id_type=chat_id with the user
// token. Required user scopes: im:message + im:message.send_as_user.
func (c *FeishuClient) SendMessage(ctx context.Context, chatID, msgType, content, uuid string) (*RawMessage, error) {
	payload := map[string]any{"receive_id": chatID, "msg_type": msgType, "content": content}
	if uuid != "" {
		payload["uuid"] = uuid
	}
	body, _ := json.Marshal(payload)
	q := url.Values{}
	q.Set("receive_id_type", "chat_id")
	var out RawMessage
	if err := c.callReq(ctx, idUser, http.MethodPost, "/im/v1/messages", q, body, uuid != "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SendBotCard sends an interactive card to a user via tenant_access_token (app bot).
// POST /im/v1/messages?receive_id_type=open_id
func (c *FeishuClient) SendBotCard(ctx context.Context, openID string, card any) error {
	if openID == "" {
		return errors.New("openID 不能为空")
	}
	cardBytes, err := json.Marshal(card)
	if err != nil {
		return err
	}
	payload := map[string]any{
		"receive_id": openID,
		"msg_type":   "interactive",
		"content":    string(cardBytes),
	}
	body, _ := json.Marshal(payload)
	q := url.Values{}
	q.Set("receive_id_type", "open_id")
	var out RawMessage
	return c.callReq(ctx, idTenant, http.MethodPost, "/im/v1/messages", q, body, false, &out)
}

// multipartBody builds a multipart/form-data body (fields in order, then the
// binary part).
func multipartBody(fields [][2]string, fileField, fileName string, data []byte) ([]byte, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			return nil, "", err
		}
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, fileField, strings.NewReplacer(`"`, "_", "\\", "_", "\r", "", "\n", "").Replace(fileName)))
	h.Set("Content-Type", "application/octet-stream")
	part, err := mw.CreatePart(h)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(data); err != nil {
		return nil, "", err
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

const uploadTimeout = 5 * time.Minute

// imFileType maps a file name to the file_type enum of im/v1/files.
func imFileType(name string) string {
	switch strings.ToLower(strings.TrimPrefix(filepathExt(name), ".")) {
	case "opus":
		return "opus"
	case "mp4":
		return "mp4"
	case "pdf":
		return "pdf"
	case "doc", "docx":
		return "doc"
	case "xls", "xlsx":
		return "xls"
	case "ppt", "pptx":
		return "ppt"
	}
	return "stream"
}

func filepathExt(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 && i > strings.LastIndexAny(name, "/\\") {
		return name[i:]
	}
	return ""
}

// UploadIM: POST im/v1/images (image_type=message) or im/v1/files with the
// user token. The current docs list both tenant_access_token and
// user_access_token; scope im:resource or im:resource:upload; the app needs
// the bot capability (error 234007 otherwise).
func (c *FeishuClient) UploadIM(ctx context.Context, kind, name string, data []byte) (string, error) {
	var (
		path   string
		fields [][2]string
		field  string
	)
	if kind == "image" {
		path, field = "/im/v1/images", "image"
		fields = [][2]string{{"image_type", "message"}}
	} else {
		path, field = "/im/v1/files", "file"
		fields = [][2]string{{"file_type", imFileType(name)}, {"file_name", name}}
	}
	body, ctype, err := multipartBody(fields, field, name, data)
	if err != nil {
		return "", err
	}
	var out struct {
		ImageKey string `json:"image_key"`
		FileKey  string `json:"file_key"`
	}
	if err := c.callReqCT(ctx, idUser, http.MethodPost, path, nil, body, ctype, uploadTimeout, true, &out); err != nil {
		return "", err
	}
	key := out.ImageKey
	if kind != "image" {
		key = out.FileKey
	}
	if key == "" {
		return "", &FeishuError{Msg: "上传成功但未返回 key"}
	}
	return key, nil
}

// rootFolder: GET drive/explorer/v2/root_folder/meta (user's 我的空间).
func (c *FeishuClient) rootFolder(ctx context.Context) (string, error) {
	uid := ""
	if c.auth != nil {
		uid = c.auth.UserID()
	}
	c.mu.Lock()
	t := c.rootTok
	if c.rootUser != uid {
		t = ""
	}
	c.mu.Unlock()
	if t != "" {
		return t, nil
	}
	var d struct {
		Token string `json:"token"`
	}
	if err := c.call(ctx, idUser, "/drive/explorer/v2/root_folder/meta", nil, &d); err != nil {
		return "", err
	}
	if d.Token == "" {
		return "", &FeishuError{Msg: "未能获取「我的空间」根文件夹 token"}
	}
	c.mu.Lock()
	c.rootTok, c.rootUser = d.Token, uid
	c.mu.Unlock()
	return d.Token, nil
}

func (c *FeishuClient) driveUploadToFolder(ctx context.Context, folder, name string, data []byte) (string, error) {
	if len(data) <= DriveMaxBytes {
		fields := [][2]string{
			{"file_name", name},
			{"parent_type", "explorer"},
			{"parent_node", folder},
			{"size", strconv.Itoa(len(data))},
		}
		body, ctype, err := multipartBody(fields, "file", name, data)
		if err != nil {
			return "", err
		}
		var out struct {
			FileToken string `json:"file_token"`
		}
		if err := c.callReqCT(ctx, idUser, http.MethodPost, "/drive/v1/files/upload_all", nil, body, ctype, uploadTimeout, true, &out); err != nil {
			return "", err
		}
		if out.FileToken == "" {
			return "", &FeishuError{Msg: "上传成功但未返回 file_token"}
		}
		return out.FileToken, nil
	}
	// Multipart upload for files > 20 MB
	prepBody, _ := json.Marshal(map[string]any{
		"file_name":   name,
		"parent_type": "explorer",
		"parent_node": folder,
		"size":        len(data),
	})
	var prep struct {
		UploadID  string `json:"upload_id"`
		BlockSize int    `json:"block_size"`
		BlockNum  int    `json:"block_num"`
	}
	if err := c.callReq(ctx, idUser, http.MethodPost, "/drive/v1/files/upload_prepare", nil, prepBody, false, &prep); err != nil {
		return "", err
	}
	if prep.UploadID == "" || prep.BlockSize <= 0 {
		return "", &FeishuError{Msg: "分片上传初始化失败"}
	}
	for seq, off := 0, 0; off < len(data); seq, off = seq+1, off+prep.BlockSize {
		end := off + prep.BlockSize
		if end > len(data) {
			end = len(data)
		}
		chunk := data[off:end]
		fields := [][2]string{
			{"upload_id", prep.UploadID},
			{"seq", strconv.Itoa(seq)},
			{"size", strconv.Itoa(len(chunk))},
		}
		body, ctype, err := multipartBody(fields, "file", name, chunk)
		if err != nil {
			return "", err
		}
		if err := c.callReqCT(ctx, idUser, http.MethodPost, "/drive/v1/files/upload_part", nil, body, ctype, uploadTimeout, true, nil); err != nil {
			return "", err
		}
	}
	finBody, _ := json.Marshal(map[string]any{
		"upload_id": prep.UploadID,
		"block_num": prep.BlockNum,
	})
	var fin struct {
		FileToken string `json:"file_token"`
	}
	if err := c.callReq(ctx, idUser, http.MethodPost, "/drive/v1/files/upload_finish", nil, finBody, false, &fin); err != nil {
		return "", err
	}
	if fin.FileToken == "" {
		return "", &FeishuError{Msg: "分片上传完成但未返回 file_token"}
	}
	return fin.FileToken, nil
}

// DriveUploadToTopic uploads a file into "网页呆呆鸟/<topicFolder>" in the
// user's Feishu Drive and returns (fileToken, folderPath, err).
func (c *FeishuClient) DriveUploadToTopic(ctx context.Context, topicFolder, name string, data []byte) (string, string, error) {
	folder, folderPath, err := c.ensurePrivateTopicFolder(ctx, topicFolder)
	if err != nil {
		return "", "", err
	}
	tok, err := c.driveUploadToFolder(ctx, folder, name, data)
	if err != nil {
		return "", "", err
	}
	return tok, folderPath, nil
}

// DriveUpload: POST drive/v1/files/upload_all (<= 20 MB) into the user's
// root folder with the user token. Scopes: drive:drive or drive:file:upload
// (+ drive:drive.metadata:readonly for the root-folder lookup).
func (c *FeishuClient) DriveUpload(ctx context.Context, name string, data []byte) (string, error) {
	token, _, err := c.DriveUploadToTopic(ctx, "", name, data)
	return token, err
}

// DriveURL: POST drive/v1/metas/batch_query with_url=true.
func (c *FeishuClient) DriveURL(ctx context.Context, token string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"request_docs": []map[string]string{{"doc_token": token, "doc_type": "file"}},
		"with_url":     true,
	})
	var d struct {
		Metas []struct {
			URL string `json:"url"`
		} `json:"metas"`
		Failed []struct {
			Code int `json:"code"`
		} `json:"failed_list"`
	}
	if err := c.callReq(ctx, idUser, http.MethodPost, "/drive/v1/metas/batch_query", nil, body, true, &d); err != nil {
		return "", err
	}
	if len(d.Metas) == 0 || d.Metas[0].URL == "" {
		code := 0
		if len(d.Failed) > 0 {
			code = d.Failed[0].Code
		}
		return "", &FeishuError{Code: code, Msg: "未能获取文件访问链接"}
	}
	return d.Metas[0].URL, nil
}

// DriveMeta: POST drive/v1/metas/batch_query to retrieve document metadata including latest modification time.
func (c *FeishuClient) DriveMeta(ctx context.Context, token string) (*DriveDocMeta, error) {
	body, _ := json.Marshal(map[string]any{
		"request_docs": []map[string]string{{"doc_token": token, "doc_type": "file"}},
		"with_url":     true,
	})
	var d struct {
		Metas []struct {
			OwnerID          string `json:"owner_id"`
			DocToken         string `json:"doc_token"`
			DocType          string `json:"doc_type"`
			Title            string `json:"title"`
			LatestModifyTime string `json:"latest_modify_time"`
			URL              string `json:"url"`
		} `json:"metas"`
		Failed []struct {
			Code int `json:"code"`
		} `json:"failed_list"`
	}
	if err := c.callReq(ctx, idUser, http.MethodPost, "/drive/v1/metas/batch_query", nil, body, true, &d); err != nil {
		return nil, err
	}
	if len(d.Metas) == 0 {
		code := 0
		if len(d.Failed) > 0 {
			code = d.Failed[0].Code
		}
		return nil, &FeishuError{Code: code, Msg: "未能获取文件元数据"}
	}
	m := d.Metas[0]
	modTime, _ := strconv.ParseInt(m.LatestModifyTime, 10, 64)
	if modTime > 0 && modTime < 1e11 {
		modTime *= 1000
	}
	return &DriveDocMeta{
		OwnerID:          m.OwnerID,
		DocToken:         m.DocToken,
		DocType:          m.DocType,
		Title:            m.Title,
		LatestModifyTime: modTime,
		URL:              m.URL,
	}, nil
}

// DriveShareChat grants the current chat view on one file. Link sharing stays
// closed; this does not open the file to the tenant or to any other chat.
func (c *FeishuClient) DriveShareChat(ctx context.Context, token, chatID string) error {
	if !strings.HasPrefix(chatID, "oc_") || !msgIDRe.MatchString(chatID) {
		return fmt.Errorf("群聊 ID 非法，停止授权")
	}
	member, err := c.DriveShareMember(ctx, token, "openchat", chatID, "view")
	if err != nil {
		return err
	}
	if member.MemberType == "openchat" && member.ID != chatID {
		return fmt.Errorf("群授权目标与当前群不一致")
	}
	if member.Perm != "" && member.Perm != "view" {
		return fmt.Errorf("群权限不是阅读，停止授权")
	}
	return nil
}

// DriveShareMember: POST drive/v1/permissions/:token/members?type=file with
// member_type=openid so a specific user or bot can access the file.
func (c *FeishuClient) DriveShareMember(ctx context.Context, token, memberType, memberID, perm string) (*DrivePermissionMember, error) {
	if memberType == "" {
		memberType = "openid"
	}
	if perm == "" {
		perm = "view"
	}
	collabType := "user"
	switch memberType {
	case "openchat":
		collabType = "chat"
	case "opendepartmentid":
		collabType = "department"
	case "groupid":
		collabType = "group"
	}
	body, _ := json.Marshal(map[string]string{
		"member_type": memberType,
		"member_id":   memberID,
		"perm":        perm,
		"type":        collabType,
	})
	q := url.Values{}
	q.Set("type", "file")
	if memberType == "openchat" {
		q.Set("need_notification", "false")
	}
	var result struct {
		Member *DrivePermissionMember `json:"member"`
	}
	if err := c.callReq(ctx, idUser, http.MethodPost, "/drive/v1/permissions/"+url.PathEscape(token)+"/members", q, body, true, &result); err != nil {
		return nil, err
	}
	if result.Member == nil || result.Member.ID == "" {
		target := "授权目标"
		switch memberType {
		case "openid":
			target = "呆呆鸟应用"
		case "openchat":
			target = "群聊"
		}
		return nil, fmt.Errorf("飞书未返回%s的规范身份，无法确认权限", target)
	}
	return result.Member, nil
}

// DriveSetLinkShare: PATCH drive/v1/permissions/:token/public?type=file with
// link_share_entity ("closed" for 未开启, "tenant_readable", etc.)
func (c *FeishuClient) DriveSetLinkShare(ctx context.Context, token, entity string) error {
	if entity == "" {
		entity = "closed"
	}
	body, _ := json.Marshal(map[string]string{"link_share_entity": entity})
	q := url.Values{}
	q.Set("type", "file")
	return c.callReq(ctx, idUser, http.MethodPatch, "/drive/v1/permissions/"+url.PathEscape(token)+"/public", q, body, true, nil)
}

// BatchUsers: GET contact/v3/users/batch (max 50 ids per request).
func (c *FeishuClient) BatchUsers(ctx context.Context, ids []string) ([]User, error) {
	var out []User
	for i := 0; i < len(ids); i += 50 {
		end := i + 50
		if end > len(ids) {
			end = len(ids)
		}
		q := url.Values{}
		q.Set("user_id_type", "open_id")
		for _, id := range ids[i:end] {
			q.Add("user_ids", id)
		}
		var d struct {
			Items []struct {
				OpenID string `json:"open_id"`
				Name   string `json:"name"`
				Avatar struct {
					Avatar72  string `json:"avatar_72"`
					Avatar240 string `json:"avatar_240"`
				} `json:"avatar"`
			} `json:"items"`
		}
		if err := c.call(ctx, idUser, "/contact/v3/users/batch", q, &d); err != nil {
			return out, err
		}
		for _, it := range d.Items {
			av := it.Avatar.Avatar72
			if av == "" {
				av = it.Avatar.Avatar240
			}
			out = append(out, User{OpenID: it.OpenID, Name: it.Name, Avatar: av})
		}
	}
	return out, nil
}

// Download: GET im/v1/messages/:message_id/resources/:file_key?type=image|file.
//
// The official doc lists only tenant_access_token for this endpoint (and the
// bot must be in the chat). Depending on config.resource_auth it tries the app
// identity, then the user identity ("auto"), or just one of them.
func (c *FeishuClient) Download(ctx context.Context, messageID, fileKey, resType string) (*Download, error) {
	if resType != "image" && resType != "file" {
		return nil, fmt.Errorf("type 必须是 image 或 file")
	}
	var order []identity
	switch c.resAuth {
	case "user":
		order = []identity{idUser}
	case "tenant":
		order = []identity{idTenant}
	default:
		order = []identity{idTenant, idUser}
	}
	var errs []string
	var first error
	for _, id := range order {
		d, err := c.downloadAs(ctx, id, messageID, fileKey, resType)
		if err == nil {
			return d, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrLoginRequired) && first != nil {
			// not logged in is irrelevant for the fallback attempt
			continue
		}
		if first == nil {
			first = err
		}
		var fe *FeishuError
		if errors.As(err, &fe) {
			errs = append(errs, fmt.Sprintf("%s: %d %s", id, fe.Code, truncate(fe.Msg, 80)))
		} else {
			errs = append(errs, fmt.Sprintf("%s: %s", id, truncate(err.Error(), 80)))
		}
	}
	var fe *FeishuError
	if errors.As(first, &fe) && len(errs) > 1 {
		return nil, &FeishuError{Code: fe.Code, Msg: strings.Join(errs, "；"), HTTPStatus: fe.HTTPStatus}
	}
	return nil, first
}

func (c *FeishuClient) downloadAs(ctx context.Context, id identity, messageID, fileKey, resType string) (*Download, error) {
	if id == idTenant && (c.appID == "" || c.appSecret == "") {
		return nil, ErrNotConfigured
	}
	q := url.Values{}
	q.Set("type", resType)
	path := "/im/v1/messages/" + url.PathEscape(messageID) + "/resources/" + url.PathEscape(fileKey)
	for attempt := 0; ; attempt++ {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		resp, used, err := c.roundTrip(rctx, id, http.MethodGet, path, q)
		if err != nil {
			cancel()
			return nil, err
		}
		if resp.StatusCode == http.StatusOK {
			return &Download{
				Body:        &cancelBody{ReadCloser: resp.Body, cancel: cancel},
				ContentType: resp.Header.Get("Content-Type"),
				Filename:    filenameFromDisposition(resp.Header.Get("Content-Disposition")),
				Size:        resp.ContentLength,
			}, nil
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		var env envelope
		_ = json.Unmarshal(b, &env)
		// Only the tenant token is renewed here: a user token rejected by this
		// (tenant-only) endpoint must not burn the single-use refresh token.
		if isTokenInvalid(env.Code) && attempt < 1 && id == idTenant {
			if err := c.renew(ctx, id, used); err != nil {
				return nil, err
			}
			continue
		}
		if env.Msg == "" {
			env.Msg = truncate(string(b), 120)
		}
		return nil, &FeishuError{Code: env.Code, Msg: env.Msg, HTTPStatus: resp.StatusCode}
	}
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

func filenameFromDisposition(v string) string {
	if v == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(v)
	if err != nil {
		return ""
	}
	return params["filename"]
}

// DriveDownload: GET drive/v1/files/:file_token/download with the user
// identity (docs list user_access_token; scope drive:file:download, drive:drive
// or drive:file:readonly). Online documents cannot be downloaded this way.
func (c *FeishuClient) DriveDownload(ctx context.Context, token string) (*Download, error) {
	path := "/drive/v1/files/" + url.PathEscape(token) + "/download"
	rctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	resp, _, err := c.roundTrip(rctx, idUser, http.MethodGet, path, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode == http.StatusOK {
		return &Download{
			Body:        &cancelBody{ReadCloser: resp.Body, cancel: cancel},
			ContentType: resp.Header.Get("Content-Type"),
			Filename:    filenameFromDisposition(resp.Header.Get("Content-Disposition")),
			Size:        resp.ContentLength,
		}, nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	cancel()
	var env envelope
	_ = json.Unmarshal(b, &env)
	if env.Msg == "" {
		switch resp.StatusCode {
		case http.StatusForbidden:
			env.Msg = "没有下载该云空间文件的权限"
		case http.StatusNotFound:
			env.Msg = "文件不存在或已被删除"
		default:
			env.Msg = truncate(string(b), 120)
		}
	}
	return nil, &FeishuError{Code: env.Code, Msg: env.Msg, HTTPStatus: resp.StatusCode}
}
