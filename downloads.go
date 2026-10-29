package main

import (
	"context"
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
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Download states of one file.
const (
	dlNotDownloaded = "not_downloaded"
	dlWaiting       = "waiting"
	dlRunning       = "downloading"
	dlDone          = "done"
	dlFailed        = "failed"
	// dlMissing is only a view state: the index says "done" but the file was
	// removed on disk (only an explicit download click fetches it again).
	dlMissing = "missing"
)

// DLEntry is one downloadable file of a conversation, persisted in the index.
type DLEntry struct {
	CloudName       string `json:"cloud_name,omitempty"`
	ResourceType    string `json:"resource_type,omitempty"`
	DownloadedBytes int64  `json:"downloaded_bytes,omitempty"`
	TotalBytes      int64  `json:"total_bytes,omitempty"`
	SourceFolder    string `json:"source_folder,omitempty"`
	SourcePath      string `json:"source_path,omitempty"`
	LastChecked     int64  `json:"last_checked,omitempty"`
	SyncStatus      string `json:"sync_status,omitempty"`
	SyncError       string `json:"sync_error,omitempty"`
	ContentHash     string `json:"content_hash,omitempty"`
	BackupRel       string `json:"backup_rel,omitempty"`
	RemoteMissing   bool   `json:"remote_missing,omitempty"`
	ID              string `json:"id"`
	Key             string `json:"key"`
	Kind            string `json:"kind"` // image|file|audio|video|drive
	MessageID       string `json:"message_id"`
	FileKey         string `json:"file_key"`
	ResType         string `json:"res_type"` // image|file|drive
	ChatID          string `json:"chat_id"`
	ChatName        string `json:"chat_name,omitempty"`
	TopicKey        string `json:"topic_key"`
	TopicTitle      string `json:"topic_title,omitempty"`
	NameHint        string `json:"name_hint,omitempty"`
	Name            string `json:"name"`
	Rel             string `json:"rel,omitempty"` // slash-separated, relative to the download dir
	Size            int64  `json:"size"`
	Mime            string `json:"mime,omitempty"`
	Status          string `json:"status"`
	Reason          string `json:"reason,omitempty"` // short Chinese failure reason
	Err             string `json:"err,omitempty"`
	Code            int    `json:"code,omitempty"`
	Tries           int    `json:"tries,omitempty"`
	NextRetry       int64  `json:"next_retry,omitempty"`     // unix ms
	Updated         int64  `json:"updated"`                  // unix ms of the last state change
	MsgTime         int64  `json:"msg_time,omitempty"`       // time of the message (ms)
	CloudModTime    int64  `json:"cloud_mod_time,omitempty"` // cloud modification time (ms)
	// Path is filled in by views only (absolute, host path when mapped).
	Path string `json:"path,omitempty"`
}

type dlIndex struct {
	Version   int                 `json:"version"`
	Base      string              `json:"base"`
	Entries   map[string]*DLEntry `json:"entries"`
	TopicDirs map[string]string   `json:"topic_dirs"`
	ChatDirs  map[string]string   `json:"chat_dirs"`
}

// TopicCtx names the conversation a file belongs to.
type TopicCtx struct {
	ChatID, ChatName, TopicKey, TopicTitle string
}

// dlFetched is a fetched file waiting in a local temp/cache location.
type dlFetched struct {
	ResourceType string
	Path         string // readable local file with the bytes
	Name         string // name reported by Feishu (may be empty)
	Mime         string
	ModTime      int64 // cloud modification time if available (ms)
	Cleanup      func(ok bool)
}

// DLFetcher fetches the bytes of an entry. force bypasses any local cache.
type DLFetcher func(ctx context.Context, e DLEntry, force bool) (*dlFetched, error)

type dlFlight struct{ done chan struct{} }

// DownloadManager downloads every file of the conversations the user opens
// into <base>/<group>/<topic>/<original name>, once. State lives in an index
// file; concurrent requests for one file share a single download.
type DownloadManager struct {
	mu        sync.Mutex
	base      string
	hostBase  string
	idxPath   string
	entries   map[string]*DLEntry
	aliases   map[string]string
	topicDirs map[string]string
	chatDirs  map[string]string
	rels      map[string]string // lower(rel) -> key
	flights   map[string]*dlFlight
	sem       chan struct{}
	fetch     DLFetcher
	now       func() time.Time
	dirty     bool
}

// dlConcurrency bounds simultaneous downloads.
const dlConcurrency = 3

func NewDownloadManager(base, hostBase, idxPath string, fetch DLFetcher) *DownloadManager {
	m := &DownloadManager{
		base: base, hostBase: hostBase, idxPath: idxPath, fetch: fetch,
		entries: map[string]*DLEntry{}, aliases: map[string]string{}, topicDirs: map[string]string{}, chatDirs: map[string]string{},
		rels: map[string]string{}, flights: map[string]*dlFlight{},
		sem: make(chan struct{}, dlConcurrency), now: time.Now,
	}
	m.load()
	return m
}

// Base is the download directory.
func (m *DownloadManager) Base() string { return m.base }

func (m *DownloadManager) load() {
	b, err := os.ReadFile(m.idxPath)
	if err != nil {
		return
	}
	var ix dlIndex
	if json.Unmarshal(b, &ix) != nil {
		return
	}
	for k, e := range ix.Entries {
		if e == nil || e.Key != k {
			continue
		}
		if e.Status != dlDone && e.Status != dlFailed {
			e.Status = dlNotDownloaded
		}
		m.entries[k] = e
		if e.Rel != "" {
			m.rels[strings.ToLower(e.Rel)] = k
		}
	}
	for k, v := range ix.TopicDirs {
		m.topicDirs[k] = v
	}
	for k, v := range ix.ChatDirs {
		m.chatDirs[k] = v
	}
	if ix.Base != "" && filepath.Clean(ix.Base) != filepath.Clean(m.base) {
		// the folder was changed: old copies are not trusted, directories are re-derived
		for _, e := range m.entries {
			if e.Status == dlDone {
				e.Status, e.Rel = dlWaiting, ""
			}
		}
		m.rels, m.topicDirs, m.chatDirs = map[string]string{}, map[string]string{}, map[string]string{}
	} else {
		// Preserve distinct remote resources even when bytes and names match.
	}
}

func filesIdentical(p1, p2 string) bool {
	if filepath.Clean(p1) == filepath.Clean(p2) {
		return true
	}
	s1, err1 := os.Stat(p1)
	s2, err2 := os.Stat(p2)
	if err1 != nil || err2 != nil || !s1.Mode().IsRegular() || !s2.Mode().IsRegular() {
		return false
	}
	if s1.Size() != s2.Size() {
		return false
	}
	f1, err := os.Open(p1)
	if err != nil {
		return false
	}
	defer f1.Close()
	f2, err := os.Open(p2)
	if err != nil {
		return false
	}
	defer f2.Close()
	h1, h2 := sha256.New(), sha256.New()
	if _, err := io.Copy(h1, f1); err != nil {
		return false
	}
	if _, err := io.Copy(h2, f2); err != nil {
		return false
	}
	return string(h1.Sum(nil)) == string(h2.Sum(nil))
}

// TriggerUpdate begins an in-place forced re-download for an updated cloud file.
func (m *DownloadManager) TriggerUpdate(key string, cloudModTime int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[key]
	if e == nil {
		return
	}
	e.SyncStatus = "updating"
	m.maybeStartLocked(e, true)
}

func (m *DownloadManager) saveLocked() {
	ix := dlIndex{Version: 1, Base: m.base, Entries: m.entries, TopicDirs: m.topicDirs, ChatDirs: m.chatDirs}
	b, err := json.MarshalIndent(ix, "", " ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(m.idxPath), 0o755); err == nil {
		_ = writeFileAtomic(m.idxPath, b, 0o600)
	}
	m.dirty = false
}

func dlKey(resType, messageID, fileKey string) string {
	return resType + "|" + messageID + "|" + fileKey
}

func dlID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:10])
}

// dlSpec turns an attachment into (resType, id parts); ok=false when it
// cannot be downloaded to disk (documents, cards, plain links).
func dlSpec(a Attachment) (resType, messageID, fileKey string, ok bool) {
	if a.NoDownload {
		return
	}
	if a.ResourceToken != "" && (exportExtension(a.ResourceType) != "" || a.ResourceType == "wiki") {
		return "export:" + a.ResourceType, a.MessageID, a.ResourceToken, true
	}
	if a.ResType == "drive" && a.FileKey != "" {
		return "drive", a.MessageID, a.FileKey, true
	}
	if (a.ResType == "image" || a.ResType == "file") && a.FileKey != "" && a.MessageID != "" {
		return a.ResType, a.MessageID, a.FileKey, true
	}
	if a.URL != "" {
		if r, ok := driveRef(a.URL); ok && r.Type == "file" {
			return "drive", a.MessageID, r.Token, true
		}
	}
	return
}

// View returns a copy with the absolute path filled in and the file state
// checked against the disk.
func (m *DownloadManager) viewLocked(e *DLEntry) DLEntry {
	v := *e
	v.Err = ""
	if v.Rel != "" {
		abs := filepath.Join(m.base, filepath.FromSlash(v.Rel))
		if m.hostBase != "" {
			abs = filepath.Join(m.hostBase, filepath.FromSlash(v.Rel))
		}
		v.Path = abs
	}
	if v.Status == dlDone && !m.localExistsLocked(e) {
		v.Status = dlMissing
		if _, err := m.safePath(e.Rel); errors.Is(err, ErrOutsideDir) {
			v.Status = dlDone
			return v
		}
		v.Reason = "文件已被移动或删除"
	}
	return v
}

func (m *DownloadManager) localExistsLocked(e *DLEntry) bool {
	if e.Rel == "" {
		return false
	}
	p, err := m.safePath(e.Rel)
	if err != nil {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}
func (m *DownloadManager) validLocalLocked(e *DLEntry) bool {
	return m.localExistsLocked(e)

}

// RegisterLatest retains its legacy signature; all attachments are registered.
// Identity is topic + resource type + remote token, never the filename.
func (m *DownloadManager) RegisterLatest(tc TopicCtx, latestAtts []Attachment, allAtts []Attachment) []DLEntry {
	return m.register(tc, allAtts, true)
}

// Catalog records available resources without starting or retrying downloads.
func (m *DownloadManager) Catalog(tc TopicCtx, atts []Attachment) []DLEntry {
	return m.register(tc, atts, false)
}

func (m *DownloadManager) register(tc TopicCtx, allAtts []Attachment, autoStart bool) []DLEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UnixMilli()
	seen := map[string]bool{}
	var out []DLEntry
	for _, a := range allAtts {
		rt, mid, fk, ok := dlSpec(a)
		if !ok {
			continue
		}
		key := dlKey(rt, tc.TopicKey, fk)
		m.aliases[dlKey(rt, mid, fk)] = key
		e := m.entries[key]
		if e == nil {
			// Adopt the old index entry for this topic/resource, keeping its path.
			for oldKey, old := range m.entries {
				if old.TopicKey == tc.TopicKey && old.ResType == rt && old.FileKey == fk && m.flights[oldKey] == nil {
					e = old
					delete(m.entries, oldKey)
					m.aliases[oldKey] = key
					e.Key = key
					break
				}
			}
			if e == nil {
				e = &DLEntry{ID: dlID(key), Key: key, Kind: a.Kind, MessageID: mid, FileKey: fk, ResType: rt, NameHint: a.Name, Status: dlNotDownloaded, Updated: now, MsgTime: a.Time}
			}
			m.entries[key] = e
			if e.Rel != "" {
				m.rels[strings.ToLower(e.Rel)] = key
			}
		}
		if e.ResourceType == "" {
			e.ResourceType = a.ResourceType
		}
		e.ChatID, e.ChatName, e.TopicKey, e.TopicTitle = tc.ChatID, tc.ChatName, tc.TopicKey, tc.TopicTitle
		if a.Time > e.MsgTime {
			e.MsgTime = a.Time
		}
		e.SourceFolder, e.SourcePath = a.SourceFolder, a.SourcePath
		if a.Name != "" && a.Name != a.URL {
			e.NameHint = a.Name
		}
		if rt == "drive" {
			e.Kind = "drive"
		}
		if !seen[key] {
			if autoStart && !e.RemoteMissing {
				m.maybeStartLocked(e, false)
			}
			out = append(out, m.viewLocked(e))
			seen[key] = true
		}
	}
	m.saveLocked()
	sortDLEntries(out)
	return out
}

// Register makes sure every downloadable attachment of a conversation is
// queued (or already on disk) and returns their current state.
func (m *DownloadManager) Register(tc TopicCtx, atts []Attachment) []DLEntry {
	return m.RegisterLatest(tc, atts, atts)
}

// maybeStartLocked starts a download unless the file is already on disk, is
// in flight, or failed recently (retry with backoff).
func (m *DownloadManager) maybeStartLocked(e *DLEntry, force bool) *dlFlight {
	if fl := m.flights[e.Key]; fl != nil {
		return fl
	}
	if !force {
		if e.Status == dlDone && m.validLocalLocked(e) {
			return nil
		}
		if e.Status == dlFailed && m.now().UnixMilli() < e.NextRetry {
			return nil
		}
	}
	e.Status = dlWaiting
	e.DownloadedBytes, e.TotalBytes = 0, 0
	e.SyncStatus, e.SyncError = "updating", ""
	fl := &dlFlight{done: make(chan struct{})}
	m.flights[e.Key] = fl
	go m.work(fl, e.Key, force)
	return fl
}

func (m *DownloadManager) work(fl *dlFlight, key string, force bool) {
	defer func() {
		m.mu.Lock()
		delete(m.flights, key)
		m.mu.Unlock()
		close(fl.done)
	}()
	m.sem <- struct{}{}
	defer func() { <-m.sem }()
	m.mu.Lock()
	e := m.entries[key]
	snap := *e
	e.Status = dlRunning
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	ctx = context.WithValue(ctx, downloadProgressKey{}, downloadProgress(func(received, total int64) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.flights[key] == fl {
			if e := m.entries[key]; e != nil {
				e.DownloadedBytes, e.TotalBytes = received, total
			}
		}
	}))
	f, err := m.fetch(ctx, snap, force)
	if err == nil {
		err = m.store(key, f)
	}
	if f != nil && f.Cleanup != nil {
		f.Cleanup(err == nil)
	}
	if err != nil {
		m.fail(key, err)
	}
}

// Ensure returns the local copy of a resource that is registered for download,
// waiting for an in-flight download. found=false: the resource is unknown here.
func (m *DownloadManager) Ensure(ctx context.Context, resType, messageID, fileKey string) (v DLEntry, path string, found bool, err error) {
	key := dlKey(resType, messageID, fileKey)
	m.mu.Lock()
	if k := m.aliases[key]; k != "" {
		key = k
	}
	e := m.entries[key]
	if e == nil {
		for k, candidate := range m.entries {
			if candidate.ResType == resType && candidate.FileKey == fileKey && candidate.MessageID == messageID {
				key, e = k, candidate
				break
			}
		}
	}
	if e == nil {
		m.mu.Unlock()
		return v, "", false, nil
	}
	fl := m.maybeStartLocked(e, false)
	if fl == nil {
		v = m.viewLocked(e)
		m.mu.Unlock()
		if v.Status == dlDone {
			p, perr := m.safePath(v.Rel)
			return v, p, true, perr
		}
		return v, "", true, dlError(v)
	}
	m.mu.Unlock()
	select {
	case <-fl.done:
	case <-ctx.Done():
		return v, "", true, ctx.Err()
	}
	m.mu.Lock()
	v = m.viewLocked(m.entries[key])
	m.mu.Unlock()
	if v.Status == dlDone {
		p, perr := m.safePath(v.Rel)
		return v, p, true, perr
	}
	return v, "", true, dlError(v)
}

func dlError(v DLEntry) error {
	if v.Code != 0 {
		return &FeishuError{Code: v.Code, Msg: firstNonEmpty(v.Err, v.Reason), HTTPStatus: http.StatusBadGateway}
	}
	if v.Status == dlFailed {
		return errors.New(firstNonEmpty(v.Reason, "下载失败"))
	}
	return errors.New("文件尚未下载完成")
}

// ----- failure handling -----

// dlReason maps an error to a short Chinese reason; slow=true for refusals
// that will not go away by retrying soon.
func dlReason(err error) (reason string, code int, slow bool) {
	if errors.Is(err, ErrLoginRequired) {
		return "未登录或登录已过期", 0, false
	}
	if errors.Is(err, ErrNotConfigured) {
		return "未配置 App ID / Secret", 0, true
	}
	var fe *FeishuError
	if errors.As(err, &fe) {
		code = fe.Code
		switch fe.Code {
		case 234043:
			return "卡片 / 转发内的文件，飞书不允许下载", code, true
		case 234004, 230002:
			return "机器人不在该群，无法下载（应用身份）", code, true
		case 234040, 230073:
			return "机器人看不到这条消息，无法下载", code, true
		case 234038:
			return "受限模式下不允许下载", code, true
		case 99991679, 230027, 91204:
			return "缺少权限，请重新登录授权", code, true
		case 1061045, 1061004:
			return "请求过于频繁或无权限，请稍后手动重试", code, false
		}
		switch fe.HTTPStatus {
		case http.StatusForbidden:
			return "没有下载权限", code, true
		case http.StatusNotFound:
			return "文件不存在或已被删除", code, true
		}
		return "下载失败：" + truncate(fe.Msg, 60), code, false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "下载超时", 0, false
	}
	return "下载失败：" + truncate(err.Error(), 60), 0, false
}

func dlBackoff(tries int, slow bool) time.Duration {
	d := 20 * time.Second
	for i := 1; i < tries && d < 15*time.Minute; i++ {
		d *= 2
	}
	if d > 15*time.Minute {
		d = 15 * time.Minute
	}
	if slow && d < 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

func (m *DownloadManager) fail(key string, err error) {
	reason, code, slow := dlReason(err)
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[key]
	if e == nil {
		return
	}
	e.Status, e.Reason, e.Code, e.Err = dlFailed, reason, code, truncate(err.Error(), 300)
	e.SyncStatus, e.SyncError = "failed", reason
	if m.localExistsLocked(e) {
		e.Status = dlDone
	}
	if errors.Is(err, ErrLoginRequired) {
		e.NextRetry = m.now().Add(5 * time.Second).UnixMilli()
	} else {
		e.Tries++
		e.NextRetry = m.now().Add(dlBackoff(e.Tries, slow)).UnixMilli()
	}
	e.Updated = m.now().UnixMilli()
	m.saveLocked()
}

// ----- storing -----

func extForMime(ct string) string {
	switch strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0])) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	case "image/svg+xml":
		return ".svg"
	case "image/heic":
		return ".heic"
	case "image/x-icon", "image/vnd.microsoft.icon":
		return ".ico"
	case "application/pdf":
		return ".pdf"
	case "video/mp4":
		return ".mp4"
	case "audio/mpeg":
		return ".mp3"
	case "audio/ogg":
		return ".ogg"
	}
	return ""
}

func genericName(n, fileKey string) bool {
	n = strings.TrimSpace(n)
	switch n {
	case "", "图片", "视频", "语音", "文件", "image", "video", "audio", "file", "download":
		return true
	}
	stem := strings.TrimSuffix(n, filepath.Ext(n))
	return stem == fileKey || strings.HasPrefix(n, "img_v") || strings.HasPrefix(n, "http://") || strings.HasPrefix(n, "https://")
}

func dlShort(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return unicode.ToLower(r)
		}
		return -1
	}, s)
	if len(s) > 6 {
		s = s[len(s)-6:]
	}
	if s == "" {
		s = "x"
	}
	return s
}

// pickName chooses the file name: the original name when known, otherwise
// <kind>_<time>_<shortkey>.<ext>.
func pickName(e DLEntry, fname, ct string, now time.Time) string {
	var name string
	if e.Kind == "image" {
		if !genericName(fname, e.FileKey) && filepath.Ext(fname) != "" {
			name = fname
		}
	} else {
		for _, c := range []string{e.NameHint, fname} {
			if strings.HasPrefix(c, "https://") || strings.HasPrefix(c, "http://") {
				continue
			}
			if !genericName(c, e.FileKey) {
				name = c
				break
			}
		}
	}
	ext := extForMime(ct)
	if name == "" {
		prefix := map[string]string{"image": "image", "video": "video", "audio": "audio"}[e.Kind]
		if prefix == "" {
			prefix = "file"
		}
		t := now
		if e.MsgTime > 0 {
			t = time.UnixMilli(e.MsgTime)
		}
		if ext == "" {
			ext = ".bin"
		}
		return fmt.Sprintf("%s_%s_%s%s", prefix, t.Format("20060102-150405"), dlShort(e.FileKey), ext)
	}
	name = sanitizeName(name, 120)
	if filepath.Ext(name) == "" && ext != "" {
		name += ext
	}
	return name
}

var winReserved = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true}

// sanitizeName makes s a safe single path component on Windows, macOS and
// Linux: no separators or reserved characters, no control characters, no
// trailing dots/spaces, no reserved device names, at most max runes (the
// extension is kept).
func sanitizeName(s string, max int) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r == utf8.RuneError, r < 0x20, r == 0x7f, unicode.Is(unicode.Cf, r):
			continue
		case strings.ContainsRune(`<>:"/\|?*`, r):
			sb.WriteRune('_')
		default:
			sb.WriteRune(r)
		}
	}
	out := strings.TrimLeft(strings.TrimRight(sb.String(), " ."), " ")
	if out == "" || out == "." || out == ".." {
		return "_"
	}
	if n := utf8.RuneCountInString(out); n > max {
		ext := filepath.Ext(out)
		if utf8.RuneCountInString(ext) > 16 {
			ext = ""
		}
		stem := []rune(strings.TrimSuffix(out, ext))
		keep := max - utf8.RuneCountInString(ext)
		if keep < 1 {
			keep = 1
		}
		if keep < len(stem) {
			stem = stem[:keep]
		}
		out = strings.TrimRight(string(stem), " .") + ext
	}
	base := strings.ToUpper(strings.SplitN(out, ".", 2)[0])
	if winReserved[strings.TrimRight(base, " ")] {
		out = "_" + out
	}
	return out
}

func (m *DownloadManager) chatDirLocked(e *DLEntry) string {
	if d, ok := m.chatDirs[e.ChatID]; ok {
		return d
	}
	name := sanitizeName(firstNonEmpty(e.ChatName, e.ChatID, "未归类"), 60)
	for other, d := range m.chatDirs {
		if other != e.ChatID && strings.EqualFold(d, name) {
			name += "_" + dlShort(e.ChatID)
			break
		}
	}
	m.chatDirs[e.ChatID] = name
	return name
}

func (m *DownloadManager) topicDirLocked(e *DLEntry) string {
	key := e.TopicKey
	if key == "" {
		key = "_none"
	}
	if d, ok := m.topicDirs[key]; ok {
		return d
	}
	title := dlFirstLine(e.TopicTitle)
	if title == "" {
		title = "话题"
	}
	d := sanitizeName(truncateRunes(title, 30), 40) + "_" + dlShort(key)
	m.topicDirs[key] = d
	return d
}

func dlFirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return s
}

// claimLocked picks a free file name in dirRel ("name (2).ext" on collision)
// and creates it exclusively so it is reserved on disk. If an already-owned
// file in dirRel has identical content to srcPath, it reuses that path.
func (m *DownloadManager) claimLocked(dirRel, name, key, srcPath string) (string, bool, error) {
	initial, err := m.writeDestination(dirRel + "/" + name)
	if err != nil {
		return "", false, err
	}
	dirAbs := filepath.Dir(initial)
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; i < 1000; i++ {
		cand := name
		if i > 1 {
			cand = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		rel := dirRel + "/" + cand
		candAbs := filepath.Join(dirAbs, cand)
		owner, taken := m.rels[strings.ToLower(rel)]
		if taken && owner != key {

			continue
		}
		f, err := os.OpenFile(candAbs, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			if os.IsExist(err) {

				continue
			}
			return "", false, err
		}
		f.Close()
		m.rels[strings.ToLower(rel)] = key
		return rel, false, nil
	}
	return "", false, errors.New("无法为文件找到可用的文件名")
}

func sniffMime(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	return strings.SplitN(http.DetectContentType(buf[:n]), ";", 2)[0]
}

func (m *DownloadManager) store(key string, f *dlFetched) error {
	st, err := os.Stat(f.Path)
	if err != nil {
		return err
	}
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(f.Mime, ";", 2)[0]))
	if ct == "" || ct == "application/octet-stream" || ct == "binary/octet-stream" {
		if s := sniffMime(f.Path); s != "" && s != "application/octet-stream" {
			ct = s
		}
	}
	m.mu.Lock()
	e := m.entries[key]
	snap := *e
	if strings.HasPrefix(e.ResType, "export:") {
		snap.NameHint = f.Name
	}
	var rel string
	var reused bool
	var baseRel string
	fresh := e.Rel == ""
	if e.Rel != "" {
		for otherKey, other := range m.entries {
			if otherKey != key && other.Rel != "" && strings.EqualFold(other.Rel, e.Rel) && (other.TopicKey != e.TopicKey || other.ResType != e.ResType || other.FileKey != e.FileKey) {
				// Old indices sometimes merged different resources by filename.
				// Allocate a separate path on successful refresh, keeping old bytes intact.
				fresh = true
				m.rels[strings.ToLower(e.Rel)] = otherKey
				break
			}
		}
	}
	if fresh {
		name := pickName(snap, f.Name, ct, m.now())
		dirRel := m.chatDirLocked(e) + "/" + m.topicDirLocked(e)
		if e.SourcePath != "" {
			dirRel += "/" + safeSourcePath(e.SourcePath)
		}
		baseRel = dirRel + "/" + name
		rel, reused, err = m.claimLocked(dirRel, name, key, f.Path)
		if err != nil {
			m.mu.Unlock()
			return err
		}
	} else {
		rel = e.Rel
		if _, err := m.writeDestination(rel); err != nil {
			m.mu.Unlock()
			return err
		}
	}
	m.mu.Unlock()

	dst, err := m.writeDestination(rel)
	if err != nil {
		return err
	}
	backup := ""
	if !fresh && !filesIdentical(dst, f.Path) {
		hash, hashErr := hashLocalFile(dst)
		if hashErr == nil && (snap.ContentHash == "" || snap.ContentHash != hash) {
			ext := filepath.Ext(rel)
			backup = strings.TrimSuffix(rel, ext) + ".本地备份-" + m.now().Format("20060102-150405.000000000") + ext
			backupPath, err := m.writeDestination(backup)
			if err != nil {
				return err
			}
			if err := copyFileAtomic(dst, backupPath); err != nil {
				return fmt.Errorf("保留本地修改失败：%w", err)
			}
		}
	}
	if !reused {
		if !filesIdentical(dst, f.Path) {
			if err := copyFileAtomic(f.Path, dst); err != nil {
				if fresh && rel == baseRel {
					_ = os.Remove(dst)
					m.mu.Lock()
					delete(m.rels, strings.ToLower(rel))
					m.mu.Unlock()
				}
				return err
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e.BackupRel = backup
	e.ContentHash, _ = hashLocalFile(dst)
	e.LastChecked = m.now().UnixMilli()
	e.SyncStatus, e.SyncError, e.RemoteMissing = "current", "", false
	e.Rel, e.Name, e.Size, e.Mime = rel, filepath.Base(filepath.FromSlash(rel)), st.Size(), ct
	e.DownloadedBytes, e.TotalBytes = st.Size(), st.Size()
	if f.ResourceType != "" {
		e.ResourceType = f.ResourceType
		if f.ResourceType == "file" {
			e.Kind = "file"
		}
	}
	e.CloudModTime = f.ModTime
	e.Status, e.Reason, e.Err, e.Code, e.Tries, e.NextRetry = dlDone, "", "", 0, 0, 0
	e.Updated = m.now().UnixMilli()
	m.rels[strings.ToLower(rel)] = key
	m.saveLocked()
	return nil
}

func copyFileAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".part-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	_ = os.Chmod(tmp.Name(), 0o644)
	return os.Rename(tmp.Name(), dst)
}

// ----- queries -----

func dlSortTime(e DLEntry) int64 {
	if e.MsgTime > 0 {
		return e.MsgTime
	}
	return e.Updated
}

func sortDLEntries(out []DLEntry) {
	sort.SliceStable(out, func(i, j int) bool {
		ti, tj := dlSortTime(out[i]), dlSortTime(out[j])
		if ti != tj {
			return ti > tj
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
}

func dedupeDLEntriesByRel(in []DLEntry) []DLEntry {
	seenRel := map[string]bool{}
	out := make([]DLEntry, 0, len(in))
	for _, e := range in {
		if e.Rel != "" {
			k := strings.ToLower(e.Rel)
			if seenRel[k] {
				continue
			}
			seenRel[k] = true
		}
		out = append(out, e)
	}
	return out
}

// List returns the entries of one topic (topicKey != "") or of all topics,
// sorted in reverse chronological order (newest first) and deduplicated by local file path.
func (m *DownloadManager) List(topicKey string) []DLEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []DLEntry{}
	for _, e := range m.entries {
		if topicKey != "" && e.TopicKey != topicKey {
			continue
		}
		out = append(out, m.viewLocked(e))
	}
	sortDLEntries(out)
	return dedupeDLEntriesByRel(out)
}

func (m *DownloadManager) Get(id string) (DLEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.entries {
		if e.ID == id {
			return m.viewLocked(e), true
		}
	}
	return DLEntry{}, false
}

// ErrOutsideDir means an index entry resolves outside the download directory.
var ErrOutsideDir = errors.New("文件不在下载目录内")

// safePath resolves a recorded relative path to a real regular file inside the
// download directory (symlinks are resolved and must not leave it).
func (m *DownloadManager) safePath(rel string) (string, error) {
	if rel == "" || strings.ContainsRune(rel, 0) {
		return "", ErrOutsideDir
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || filepath.VolumeName(clean) != "" || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", ErrOutsideDir
	}
	baseReal, err := filepath.EvalSymlinks(m.base)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(filepath.Join(m.base, clean))
	if err != nil {
		return "", err
	}
	r, err := filepath.Rel(baseReal, real)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) || filepath.IsAbs(r) {
		return "", ErrOutsideDir
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", ErrOutsideDir
	}
	return real, nil
}

// Resolve maps an internal file id to a validated local path.
func (m *DownloadManager) Resolve(id string) (DLEntry, string, error) {
	e, ok := m.Get(id)
	if !ok {
		return e, "", errNoSuchFile
	}
	if e.Status != dlDone {
		return e, "", errors.New("文件尚未下载或已丢失，请先重新下载")
	}
	p, err := m.safePath(e.Rel)
	return e, p, err
}

var errNoSuchFile = errors.New("没有这个文件")

// Redownload drops the local copy state and fetches the file again, ignoring
// the cache and any failure backoff.
func (m *DownloadManager) Redownload(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.entries {
		if e.ID != id {
			continue
		}
		e.Tries, e.NextRetry, e.Reason, e.Err, e.Code = 0, 0, "", "", 0
		if m.flights[e.Key] == nil {
			m.maybeStartLocked(e, true)
		}
		return nil
	}
	return errNoSuchFile
}

// ----- serving helpers shared with /api/resource -----

func contentTypeFor(ct, name string, sniff func() string) string {
	ct = strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	if ct == "" || ct == "application/octet-stream" || ct == "binary/octet-stream" {
		if ext := fileExt(name); ext != "" {
			if textExts[ext] {
				ct = "text/plain"
			} else if t := mime.TypeByExtension("." + ext); t != "" {
				ct = strings.SplitN(t, ";", 2)[0]
			}
		}
	}
	if (ct == "" || ct == "application/octet-stream") && sniff != nil {
		ct = sniff()
	}
	if strings.HasPrefix(ct, "text/") {
		ct = "text/plain" // text-like content never executes
	}
	return ct
}
