package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Size limits (official docs): im/v1/images 10 MB, im/v1/files 30 MB,
// drive/v1/files/upload_all 20 MB (larger Drive files need chunked upload,
// implemented by DriveTopicUploader; the composer caps attachments at 30 MB).
const (
	ImageMaxBytes  = 10 << 20
	FileMaxBytes   = 30 << 20
	DriveMaxBytes  = 20 << 20
	maxAttachments = 9
	stageTTL       = 24 * time.Hour
	pendingTTL     = 14 * 24 * time.Hour
)

var (
	// ErrUploadDisabled: attachments are switched off by config.
	ErrUploadDisabled = errors.New("已在配置中禁用附件（upload_mode=off 或 disable_reply）")
	// ErrUploadGone: a staged upload id is unknown or expired.
	ErrUploadGone = errors.New("附件已失效，请重新添加")
	// ErrTooLarge: attachment over the limit.
	ErrTooLarge = errors.New("附件过大")

	stageIDRe = regexp.MustCompile(`^[a-f0-9]{32}$`)
	urlRe     = regexp.MustCompile(`https?://[^\s<>"'，。；！？、）】》]+`)
	imageExts = map[string]bool{"png": true, "jpg": true, "jpeg": true, "gif": true, "webp": true, "bmp": true, "ico": true, "tif": true, "tiff": true, "heic": true}
)

// ---------- staging (browser -> local disk) ----------

// Staged describes a file the browser handed to the local server and that
// waits to be sent with the next message.
type Staged struct {
	DriveBotOpenID string                 `json:"drive_bot_open_id,omitempty"`
	DriveBotMember *DrivePermissionMember `json:"drive_bot_member,omitempty"`
	ID             string                 `json:"upload_id"`
	Name           string                 `json:"name"`
	Size           int64                  `json:"size"`
	Kind           string                 `json:"kind"` // image | file
	ContentType    string                 `json:"content_type,omitempty"`
	Created        int64                  `json:"created,omitempty"`
	DriveToken     string                 `json:"drive_token,omitempty"`
	DriveFolder    string                 `json:"drive_folder,omitempty"`
	DriveTopic     string                 `json:"drive_topic,omitempty"`
	DriveOwner     string                 `json:"drive_owner,omitempty"`
	// DriveShareChat is the chat granted view when the sender turns encryption off.
	DriveShareChat string `json:"drive_share_chat,omitempty"`
	// Note explains why an oversized image is sent as a file.
	Note string `json:"note,omitempty"`
}

func (s *Service) stageDir() string { return filepath.Join(s.cfg.DataDir, "cache", "upload") }

func cleanFileName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == '"' {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if out == "." || out == ".." || out == "/" {
		out = ""
	}
	if r := []rune(out); len(r) > 200 {
		// keep the extension
		ext := filepath.Ext(out)
		if len([]rune(ext)) > 20 {
			ext = ""
		}
		out = string(r[:200-len([]rune(ext))]) + ext
	}
	return out
}

// Stage stores an upload from the browser. Images up to 10 MB become image
// messages; everything else (and bigger images) is sent as a file (<= 30 MB).
func (s *Service) Stage(name string, r io.Reader) (*Staged, error) {
	if err := s.requireLogin(); err != nil {
		return nil, err
	}
	if !s.cfg.UploadEnabled() {
		return nil, ErrUploadDisabled
	}
	name = cleanFileName(name)
	if name == "" {
		name = "file"
	}
	data, err := io.ReadAll(io.LimitReader(r, FileMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%w：文件是空的", ErrTooLarge)
	}
	if len(data) > FileMaxBytes {
		return nil, fmt.Errorf("%w：文件不能超过 30 MB（飞书限制）", ErrTooLarge)
	}
	ct := strings.SplitN(http.DetectContentType(data[:min(len(data), 512)]), ";", 2)[0]
	if t := mime.TypeByExtension("." + fileExt(name)); t != "" && (ct == "application/octet-stream" || strings.HasPrefix(ct, "text/")) {
		ct = strings.SplitN(t, ";", 2)[0]
	}
	st := &Staged{Name: name, Size: int64(len(data)), Kind: "file", ContentType: ct, Created: time.Now().UnixMilli()}
	isImg := strings.HasPrefix(ct, "image/") || imageExts[fileExt(name)]
	if isImg && ct != "image/svg+xml" {
		if len(data) <= ImageMaxBytes {
			st.Kind = "image"
		} else {
			st.Note = "图片超过 10 MB，将作为文件发送"
		}
	}
	if _, hasTopicDrive := s.api.(DriveTopicUploader); !hasTopicDrive && s.cfg.uploadModeOrAuto() == UploadDrive && len(data) > DriveMaxBytes {
		return nil, fmt.Errorf("%w：upload_mode=drive 时单个文件不能超过 20 MB", ErrTooLarge)
	}
	var idb [16]byte
	_, _ = rand.Read(idb[:])
	st.ID = hex.EncodeToString(idb[:])
	dir := s.stageDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s.pruneStaged()
	if err := writeFileAtomic(filepath.Join(dir, st.ID+".bin"), data, 0o600); err != nil {
		return nil, err
	}
	meta, _ := json.Marshal(st)
	if err := writeFileAtomic(filepath.Join(dir, st.ID+".json"), meta, 0o600); err != nil {
		return nil, err
	}
	return st, nil
}

func (s *Service) loadStaged(id string) (*Staged, []byte, error) {
	if !stageIDRe.MatchString(id) {
		return nil, nil, ErrUploadGone
	}
	mb, err := os.ReadFile(filepath.Join(s.stageDir(), id+".json"))
	if err != nil {
		return nil, nil, ErrUploadGone
	}
	var st Staged
	if json.Unmarshal(mb, &st) != nil || st.ID != id {
		return nil, nil, ErrUploadGone
	}
	data, err := os.ReadFile(filepath.Join(s.stageDir(), id+".bin"))
	if err != nil {
		return nil, nil, ErrUploadGone
	}
	return &st, data, nil
}

// Unstage drops a staged upload (the user removed the chip).
func (s *Service) Unstage(id string) {
	if !stageIDRe.MatchString(id) {
		return
	}
	_ = os.Remove(filepath.Join(s.stageDir(), id+".bin"))
	_ = os.Remove(filepath.Join(s.stageDir(), id+".json"))
}

// pruneStaged removes uploads nobody sent within 24 h (unless still queued).
func (s *Service) pruneStaged() {
	ents, err := os.ReadDir(s.stageDir())
	if err != nil {
		return
	}
	for _, e := range ents {
		name := e.Name()
		id := strings.TrimSuffix(strings.TrimSuffix(name, ".bin"), ".json")
		if s.isStagedInQueue(id) {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > stageTTL {
			_ = os.Remove(filepath.Join(s.stageDir(), name))
		}
	}
}

// ---------- uploading to Feishu ----------

type sendPart struct {
	Kind       string // image | file | link
	Key        string // image_key / file_key / drive file_token
	Name       string
	URL        string // Drive link
	Folder     string // Drive folder path ("网页呆呆鸟/<topic>")
	SharedChat bool   // destination chat was granted view
	StageI     string // staged id
	Staged     *Staged
	Data       []byte
	Warn       string
}

// fallbackOK reports whether a failed im/v1 upload is worth retrying through
// Drive: everything except "this very file is not acceptable", login problems
// and cancellations (the Drive route would fail the same way).
func fallbackOK(err error) bool {
	if err == nil || errors.Is(err, ErrLoginRequired) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var fe *FeishuError
	if !errors.As(err, &fe) {
		return false
	}
	switch fe.Code {
	case 234006, 234010, 234011, 234039:
		return false
	}
	return true
}

// UploadError combines the failures of the IM route and the Drive route.
type UploadError struct {
	Name string
	Errs []error
}

func (e *UploadError) Error() string {
	parts := make([]string, 0, len(e.Errs))
	for _, x := range e.Errs {
		parts = append(parts, x.Error())
	}
	return "附件「" + e.Name + "」上传失败：" + strings.Join(parts, "；")
}

func (e *UploadError) Unwrap() []error { return e.Errs }

// uploadOne uploads one staged attachment. When talking to FeishuClient (which
// implements DriveTopicUploader) and mode != UploadIM, all attachments are
// uploaded into "网页呆呆鸟/<topicFolder>" in Feishu Drive so text and
// attachments are always sent together in a single message.
func (s *Service) uploadOne(ctx context.Context, st *Staged, data []byte, chatID, topicFolder string, botIDs []string, shareWithChat bool) (*sendPart, error) {
	if len(botIDs) != 1 || !strings.HasPrefix(botIDs[0], "ou_") {
		return nil, fmt.Errorf("请配置唯一的呆呆鸟应用 open_id，禁止群组或多目标授权")
	}
	if s.auth != nil && !s.auth.mock && botIDs[0] != RedDaidainiaoBotOpenID {
		return nil, fmt.Errorf("本项目只授权已确认的红色呆呆鸟应用，请核对 bot_open_ids；不能配置普通用户或其他目标")
	}
	if shareWithChat && (!strings.HasPrefix(chatID, "oc_") || !msgIDRe.MatchString(chatID)) {
		return nil, fmt.Errorf("无法确认目标群聊，停止授予群阅读权限")
	}
	unlock := s.lock("private-upload:" + st.ID)
	defer unlock()
	// Reload inside the single-flight lock; an earlier retry may have uploaded it.
	if saved, _, err := s.loadStaged(st.ID); err == nil {
		st = saved
	}
	if !shareWithChat && st.DriveShareChat != "" {
		return nil, fmt.Errorf("该附件已授予群阅读权限，无法再按加密发送，请重新添加附件")
	}
	if shareWithChat && st.DriveShareChat != "" && st.DriveShareChat != chatID {
		return nil, fmt.Errorf("该附件已授予其他群阅读权限，请重新添加附件")
	}
	owner := s.auth.UserID()
	if st.DriveToken != "" && st.DriveOwner != owner {
		return nil, fmt.Errorf("附件属于其他登录账号，请重新添加")
	}
	if v, ok := s.api.(interface {
		VerifyUploadParents(context.Context, string) error
	}); ok {
		checkTopic := topicFolder
		if st.DriveToken != "" {
			checkTopic = st.DriveTopic
		}
		if err := v.VerifyUploadParents(ctx, checkTopic); err != nil {
			return nil, fmt.Errorf("私密附件目录验证失败：%w", err)
		}
	}
	if st.DriveToken == "" {
		var token, folder string
		var err error
		if tu, ok := s.api.(DriveTopicUploader); ok {
			token, folder, err = tu.DriveUploadToTopic(ctx, topicFolder, st.Name, data)
		} else {
			token, err = s.api.DriveUpload(ctx, st.Name, data)
		}
		if err != nil {
			return nil, err
		}
		if token == "" {
			return nil, fmt.Errorf("上传未返回文件 token")
		}
		st.DriveToken, st.DriveFolder, st.DriveTopic, st.DriveOwner = token, folder, topicFolder, owner
		b, _ := json.Marshal(st)
		if err := writeFileAtomic(filepath.Join(s.stageDir(), st.ID+".json"), b, 0o600); err != nil {
			return nil, err
		}
	}
	token := st.DriveToken
	if err := s.api.DriveClosePrivate(ctx, token, "file"); err != nil {
		return nil, fmt.Errorf("关闭附件分享失败，文件链接尚未发送：%w", err)
	}
	// Read before adding the bot to avoid treating an API duplicate-member response as a failure.
	perms, err := s.api.DrivePermissions(ctx, token, "file")
	if err != nil {
		return nil, fmt.Errorf("读取附件权限失败，文件链接尚未发送：%w", err)
	}
	if st.DriveBotOpenID != "" && st.DriveBotOpenID != botIDs[0] {
		return nil, fmt.Errorf("附件已授权给另一目标应用，请重新添加附件")
	}
	allowChat := ""
	if shareWithChat {
		allowChat = chatID
	}
	var canonical *DrivePermissionMember
	if st.DriveBotMember != nil {
		canonical, _ = canonicalBotMember(perms, owner, botIDs[0], st.DriveBotMember, allowChat)
	}
	if canonical == nil {
		if st.DriveBotMember == nil {
			if err := verifyPrivatePermissions(perms, owner, "", ""); err != nil {
				return nil, err
			}
		}
		member, err := s.api.DriveShareMember(ctx, token, "openid", botIDs[0], "view")
		if err != nil {
			return nil, fmt.Errorf("授权呆呆鸟失败，文件链接尚未发送：%w", err)
		}
		if member == nil || member.ID == "" || (member.MemberType != "appid" && !(member.MemberType == "openid" && member.ID == botIDs[0])) {
			return nil, fmt.Errorf("未返回可验证的呆呆鸟应用身份，停止发送")
		}
		// Persist the successful grant before readback, so retries can finish even
		// if the subsequent read fails. The API may echo the requested open_id.
		st.DriveBotOpenID, st.DriveBotMember = botIDs[0], member
		b, _ := json.Marshal(st)
		if err := writeFileAtomic(filepath.Join(s.stageDir(), st.ID+".json"), b, 0o600); err != nil {
			return nil, err
		}
	}
	perms, err = s.api.DrivePermissions(ctx, token, "file")
	if err != nil {
		return nil, fmt.Errorf("验证附件权限失败：%w", err)
	}
	canonical, err = canonicalBotMember(perms, owner, botIDs[0], st.DriveBotMember, allowChat)
	if err != nil {
		return nil, err
	}
	if shareWithChat {
		already := false
		for _, m := range perms.Members {
			if isChatView(m, chatID) {
				already = true
				break
			}
		}
		if !already {
			member, err := s.api.DriveShareMember(ctx, token, "openchat", chatID, "view")
			if err != nil {
				return nil, fmt.Errorf("授予当前群阅读权限失败，文件链接尚未发送：%w", err)
			}
			if member == nil || (member.MemberType == "openchat" && member.ID != chatID) || (member.Perm != "" && member.Perm != "view") {
				return nil, fmt.Errorf("未确认群阅读权限已授予当前群，停止发送")
			}
			st.DriveShareChat = chatID
			b, _ := json.Marshal(st)
			if err := writeFileAtomic(filepath.Join(s.stageDir(), st.ID+".json"), b, 0o600); err != nil {
				return nil, err
			}
			perms, err = s.api.DrivePermissions(ctx, token, "file")
			if err != nil {
				return nil, fmt.Errorf("验证群阅读权限失败：%w", err)
			}
			canonical, err = canonicalBotMember(perms, owner, botIDs[0], st.DriveBotMember, chatID)
			if err != nil {
				return nil, err
			}
		} else if st.DriveShareChat != chatID {
			st.DriveShareChat = chatID
			b, _ := json.Marshal(st)
			if err := writeFileAtomic(filepath.Join(s.stageDir(), st.ID+".json"), b, 0o600); err != nil {
				return nil, err
			}
		}
	}
	chatCheck := ""
	if shareWithChat {
		chatCheck = chatID
	}
	if err := verifyPrivatePermissions(perms, owner, canonical.MemberType+":"+canonical.ID, chatCheck); err != nil {
		return nil, err
	}
	st.DriveBotMember = canonical
	b, _ := json.Marshal(st)
	if err := writeFileAtomic(filepath.Join(s.stageDir(), st.ID+".json"), b, 0o600); err != nil {
		return nil, err
	}

	meta, err := s.api.DriveMeta(ctx, token)
	if err != nil {
		return nil, err
	}
	if meta == nil || meta.OwnerID != owner {
		return nil, fmt.Errorf("无法确认附件所有者为当前登录用户，停止发送")
	}
	link, err := s.api.DriveURL(ctx, token)
	if err != nil {
		return nil, err
	}
	return &sendPart{Name: st.Name, StageI: st.ID, Staged: st, Data: data, Kind: "link", Key: token, URL: link, Folder: st.DriveFolder, SharedChat: st.DriveShareChat != ""}, nil
}

// ---------- message assembly ----------

// postRows turns composer text into rows of a Feishu `post` message: @ picks
// become `at` nodes. Cloud URLs use standalone code blocks: Feishu otherwise
// grants the destination chat access when the owner sends a normal hyperlink.
func postRows(text string, ms []ReplyMention) [][]map[string]any {
	valid := validMentions(ms)
	var rows [][]map[string]any
	for _, line := range strings.Split(text, "\n") {
		lineStart := len(rows)
		row := []map[string]any{}
		rest := line
		for rest != "" {
			// next mention or url, whichever is first
			idx, kind, mi, ml := -1, "", -1, 0
			for i, m := range valid {
				if p := strings.Index(rest, "@"+m.Name); p >= 0 && (idx < 0 || p < idx) {
					idx, kind, mi, ml = p, "at", i, len("@"+m.Name)
				}
			}
			if loc := urlRe.FindStringIndex(rest); loc != nil && (idx < 0 || loc[0] < idx) {
				idx, kind, ml = loc[0], "url", loc[1]-loc[0]
			}
			if idx < 0 {
				row = append(row, map[string]any{"tag": "text", "text": rest})
				break
			}
			if idx > 0 {
				row = append(row, map[string]any{"tag": "text", "text": rest[:idx]})
			}
			seg := rest[idx : idx+ml]
			if kind == "at" {
				row = append(row, map[string]any{"tag": "at", "user_id": valid[mi].ID, "user_name": valid[mi].Name})
			} else if isDocURL(seg) {
				if len(row) > 0 {
					rows = append(rows, row)
					row = nil
				}
				rows = append(rows, []map[string]any{{"tag": "code_block", "text": seg}})
			} else {
				row = append(row, map[string]any{"tag": "a", "text": seg, "href": seg})
			}
			rest = rest[idx+ml:]
		}
		if len(row) == 0 && len(rows) == lineStart {
			row = append(row, map[string]any{"tag": "text", "text": " "})
		}
		if len(row) > 0 {
			rows = append(rows, row)
		}
	}
	return rows
}

func validMentions(ms []ReplyMention) []ReplyMention {
	valid := make([]ReplyMention, 0, len(ms))
	for _, m := range ms {
		if keyRe.MatchString(m.ID) && m.Name != "" && !strings.ContainsAny(m.Name, "<>\"\n") {
			valid = append(valid, m)
		}
	}
	sort.SliceStable(valid, func(i, j int) bool { return len([]rune(valid[i].Name)) > len([]rune(valid[j].Name)) })
	return valid
}

type outMsg struct {
	Type    string
	Content string
	Parts   []*sendPart // attachments carried by this message
}

// assemble decides which Feishu messages carry the text and the parts:
// text-only -> text; text and/or images and/or Drive links -> one post (a
// lone image -> image); every IM file -> its own file message.
func assemble(text string, ms []ReplyMention, parts []*sendPart) []outMsg {
	var rich, files []*sendPart
	for _, p := range parts {
		if p.Kind == "file" {
			files = append(files, p)
		} else {
			rich = append(rich, p)
		}
	}
	var out []outMsg
	cloudText := false
	for _, u := range urlRe.FindAllString(text, -1) {
		cloudText = cloudText || isDocURL(u)
	}
	switch {
	case len(rich) == 0 && text != "" && !cloudText:
		c, _ := json.Marshal(map[string]string{"text": buildReplyText(text, ms)})
		out = append(out, outMsg{Type: "text", Content: string(c)})
	case len(rich) == 1 && text == "" && rich[0].Kind == "image":
		c, _ := json.Marshal(map[string]string{"image_key": rich[0].Key})
		out = append(out, outMsg{Type: "image", Content: string(c), Parts: rich})
	case len(rich) > 0 || cloudText:
		var rows [][]map[string]any
		if text != "" {
			rows = postRows(text, ms)
		}
		for _, p := range rich {
			if p.Kind == "image" {
				rows = append(rows, []map[string]any{{"tag": "img", "image_key": p.Key}})
			} else {
				label := "📎 " + p.Name
				if p.Folder != "" {
					label += "（飞书云空间「" + p.Folder + "」）"
				}
				if p.SharedChat {
					label += "（本群可阅读）"
				}
				rows = append(rows, postRows(label, nil)...)
				rows = append(rows, []map[string]any{{"tag": "code_block", "text": p.URL}})
			}
		}
		c, _ := json.Marshal(map[string]any{"zh_cn": map[string]any{"title": "", "content": rows}})
		out = append(out, outMsg{Type: "post", Content: string(c), Parts: rich})
	}
	for _, p := range files {
		c, _ := json.Marshal(map[string]string{"file_key": p.Key})
		out = append(out, outMsg{Type: "file", Content: string(c), Parts: []*sendPart{p}})
	}
	return out
}

// ---------- compose (new topic / reply) ----------

// ComposeResult is the outcome of sending a reply or starting a topic.
type ComposeResult struct {
	Messages []Message `json:"messages"`
	// ThreadID is the real thread of the sent messages (set once a reply was
	// sent in thread). For a brand-new root message it is the pending id.
	ThreadID string   `json:"thread_id,omitempty"`
	Topic    *Topic   `json:"topic,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	// Failed lists staged upload ids that were not sent (keep those chips).
	Failed []string `json:"failed,omitempty"`
	// Queued is true when Daidainiao is currently running a task and the reply
	// was placed in the topic's queue instead of sent immediately.
	Queued    bool         `json:"queued,omitempty"`
	QueueItem *QueuedItem  `json:"queue_item,omitempty"`
	Queue     []QueuedItem `json:"queue,omitempty"`
	BotBusy   bool         `json:"bot_busy,omitempty"`
	BotStatus string       `json:"bot_status,omitempty"`

	raws []RawMessage // the sent messages as returned by Feishu
}

type composeTarget struct {
	chatID        string
	rootID        string // message to reply to in thread; "" = send a new root message
	threadID      string // current thread id (replies), "" for new topics
	topicFolder   string // subfolder name under DrivePrivateRootName in Feishu Drive
	shareWithChat bool   // grant the destination chat view; false keeps the file private
}

func shareChatRequested(enc *bool, n int) bool {
	return n > 0 && enc != nil && !*enc
}

const pendingPrefix = "new_"

// pendingThreadID is the topic id of a root message that has no thread yet.
func pendingThreadID(messageID string) string { return pendingPrefix + messageID }

func isPendingThread(id string) (string, bool) {
	if strings.HasPrefix(id, pendingPrefix) && msgIDRe.MatchString(id[len(pendingPrefix):]) {
		return id[len(pendingPrefix):], true
	}
	return "", false
}

// RedDaidainiaoBotOpenID is the exact open_id of the red Daidainiao bot in Feishu.
const RedDaidainiaoBotOpenID = "ou_b57b7cff2d42fc926320f4b1337467e8"

func (s *Service) resolveBotIDs(ctx context.Context, tgt composeTarget, ms []ReplyMention) []string {
	// 1. Configured bot IDs (if explicitly provided in config.json)
	if len(s.cfg.BotOpenIDs) > 0 {
		return s.cfg.BotOpenIDs
	}

	// 2. In mock mode (unit tests)
	if s.auth != nil && s.auth.mock {
		return []string{"ou_mock_bot"}
	}

	// 3. Strictly only the red Daidainiao bot
	return []string{RedDaidainiaoBotOpenID}
}

func (s *Service) compose(ctx context.Context, tgt composeTarget, text string, ms []ReplyMention, uuid string, attIDs []string) (*ComposeResult, error) {
	if s.cfg.DisableReply {
		return nil, ErrReplyDisabled
	}
	text = strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), " \t\n")
	if len([]rune(text)) > maxReplyRunes || (strings.TrimSpace(text) == "" && len(attIDs) == 0) {
		return nil, ErrReplyInvalid
	}
	if strings.TrimSpace(text) == "" {
		text = ""
	}
	if len(attIDs) > 0 && !s.cfg.UploadEnabled() {
		return nil, ErrUploadDisabled
	}
	if len(attIDs) > maxAttachments {
		return nil, badRequest(fmt.Sprintf("一次最多发送 %d 个附件", maxAttachments))
	}
	if uuid != "" && !uuidRe.MatchString(uuid) {
		uuid = ""
	}
	topicFolder := tgt.topicFolder
	if topicFolder == "" && text != "" {
		topicFolder = sanitizeName(truncateRunes(dlFirstLine(text), 30), 40)
	}
	// 1. upload attachments
	var parts []*sendPart
	var warns []string
	var failed []string
	seenIDs := map[string]bool{}
	botIDs := s.resolveBotIDs(ctx, tgt, ms)
	for _, id := range attIDs {
		if seenIDs[id] {
			continue
		}
		seenIDs[id] = true
		st, data, err := s.loadStaged(id)
		if err != nil {
			return nil, err
		}
		p, err := s.uploadOne(ctx, st, data, tgt.chatID, topicFolder, botIDs, tgt.shareWithChat)
		if err != nil {
			return nil, err
		}
		if p.Warn != "" {
			warns = append(warns, p.Warn)
		}
		parts = append(parts, p)
	}
	msgs := assemble(text, ms, parts)
	if len(msgs) == 0 {
		return nil, ErrReplyInvalid
	}
	// 2. send
	var sent []RawMessage
	var sentOut []outMsg
	firstID := ""
	for i, om := range msgs {
		u := ""
		if uuid != "" {
			u = fmt.Sprintf("%s-%d", truncateRunes(uuid, 44), i)
		}
		var resp *RawMessage
		var err error
		switch {
		case i == 0 && tgt.rootID == "":
			resp, err = s.api.SendMessage(ctx, tgt.chatID, om.Type, om.Content, u)
		case tgt.rootID != "":
			resp, err = s.api.ReplyMessage(ctx, tgt.rootID, om.Type, om.Content, u, true)
		default:
			resp, err = s.api.ReplyMessage(ctx, firstID, om.Type, om.Content, u, true)
		}
		if err == nil && resp.MessageID == "" {
			err = fmt.Errorf("飞书返回了空的消息 ID")
		}
		if err != nil {
			if i == 0 {
				return nil, err
			}
			for _, p := range om.Parts {
				failed = append(failed, p.StageI)
			}
			warns = append(warns, fmt.Sprintf("「%s」未发送：%s", partNames(om.Parts), truncate(err.Error(), 160)))
			var fe *FeishuError
			if errors.As(err, &fe) && scopeKind(fe.Code) != "" {
				break
			}
			continue
		}
		if i == 0 {
			firstID = resp.MessageID
		}
		s.fillSent(resp, om)
		sent = append(sent, *resp)
		sentOut = append(sentOut, om)
		// A committed message must never be reported as unsent or retried after
		// a readback failure. Surface the privacy failure distinctly to the user.
		for _, p := range om.Parts {
			if p.Kind != "link" || p.Staged == nil || p.Staged.DriveBotMember == nil {
				continue
			}
			perms, e := s.api.DrivePermissions(ctx, p.Key, "file")
			bot := p.Staged.DriveBotMember
			if e == nil {
				e = verifyPrivatePermissions(perms, p.Staged.DriveOwner, bot.MemberType+":"+bot.ID, p.Staged.DriveShareChat)
			}
			if e != nil {
				what := "私密权限"
				if p.Staged.DriveShareChat != "" {
					what = "群阅读权限"
				}
				warns = append(warns, fmt.Sprintf("消息已发送，但「%s」发送后的%s复查未通过：%s。请检查飞书协作者；不要重复发送。", p.Name, what, truncate(e.Error(), 180)))
			}
		}
	}
	// 3. keep the bytes of what we sent so previews / downloads work locally
	for i, om := range sentOut {
		for _, p := range om.Parts {
			if p.Kind == "image" || p.Kind == "file" {
				s.cacheSentResource(sent[i].MessageID, p)
			} else if p.Kind == "link" && p.Key != "" {
				s.cacheSentResource(sent[i].MessageID, &sendPart{
					Kind:   "drive",
					Key:    p.Key,
					Name:   p.Name,
					Data:   p.Data,
					Staged: p.Staged,
				})
			}
			s.Unstage(p.StageI)
		}
	}
	// 4. caches and result
	res := &ComposeResult{Warnings: warns, Failed: failed}
	threadID := tgt.threadID
	for _, m := range sent {
		if m.ThreadID != "" {
			threadID = m.ThreadID
		}
	}
	s.invalidateChat(tgt.chatID, threadID, tgt.threadID)
	r := s.render(ctx, sent)
	res.Messages = r.msgs
	res.ThreadID = threadID
	res.raws = sent
	return res, nil
}

func partNames(ps []*sendPart) string {
	n := make([]string, 0, len(ps))
	for _, p := range ps {
		n = append(n, p.Name)
	}
	return strings.Join(n, "、")
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// fillSent completes a send/reply response the way the list API would.
func (s *Service) fillSent(resp *RawMessage, om outMsg) {
	if resp.CreateTime == "" {
		resp.CreateTime = strconv.FormatInt(time.Now().UnixMilli(), 10)
	}
	if resp.Sender.ID == "" {
		resp.Sender.ID, resp.Sender.IDType, resp.Sender.SenderType = s.auth.UserID(), "open_id", "user"
	}
	if resp.MsgType == "" {
		resp.MsgType = om.Type
	}
	if resp.Body.Content == "" {
		resp.Body.Content = om.Content
	}
}

// invalidateChat drops the cached views a new message makes stale.
func (s *Service) invalidateChat(chatID string, threadIDs ...string) {
	for _, t := range threadIDs {
		if t != "" {
			s.cache.Delete(s.k("thread:" + t))
		}
	}
	if chatID != "" {
		s.cache.Delete(s.k("threadrefs:" + chatID))
	}
}

// resBase mirrors the file layout of the resource proxy cache.
func resBase(dataDir, messageID, fileKey, typ string) string {
	sum := sha256.Sum256([]byte(messageID + "|" + fileKey + "|" + typ))
	return filepath.Join(dataDir, "cache", "res", hex.EncodeToString(sum[:]))
}

// cacheSentResource stores the uploaded bytes in the resource cache, because
// the message-resource API cannot be used (with a user token) to read back the
// user's own image / file.
func (s *Service) cacheSentResource(messageID string, p *sendPart) {
	base := resBase(s.cfg.DataDir, messageID, p.Key, p.Kind)
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		return
	}
	if err := writeFileAtomic(base+".bin", p.Data, 0o600); err != nil {
		return
	}
	mb, _ := json.Marshal(resMeta{ContentType: p.Staged.ContentType, Filename: p.Name})
	_ = writeFileAtomic(base+".json", mb, 0o600)
}

// badRequest is a client error reported as HTTP 400.
type badRequest string

func (e badRequest) Error() string { return string(e) }
