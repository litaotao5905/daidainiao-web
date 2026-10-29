package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ttlChats    = 10 * time.Minute
	ttlRefs     = 10 * time.Second
	ttlThread   = 2 * time.Minute
	ttlUser     = 24 * time.Hour
	userMissTL  = 30 * time.Minute
	userFailTL  = 10 * time.Minute
	ttlForward  = 24 * time.Hour // merged-forward contents never change
	fwFailTL    = 2 * time.Minute
	maxFwdDepth = 4
)

// Service holds the business logic on top of an API implementation.
type Service struct {
	api   API
	cache *Cache
	cfg   Config
	auth  *Auth

	fileMu          sync.Mutex
	fileJobs        map[string]*topicFileRecord
	materialNameSem chan struct{}
	locks           sync.Map // key -> *sync.Mutex

	uMu       sync.Mutex
	uFailTill time.Time
	uFailMsg  string
	uMiss     map[string]time.Time

	fwMu   sync.Mutex
	fwFail map[string]fwFailure

	// dl holds the local resource catalogue and explicit downloads (nil when disabled).
	dl *DownloadManager

	qOnce sync.Once
	q     *queueStore

	notify *NotifyManager
	readState *ReadStateManager
}

// registerDownloads registers all downloadable files of the conversation and
// preserves downloaded copies without any automatic cloud scan.
func (s *Service) registerDownloads(ctx context.Context, tr *ThreadResp) *ThreadResp {
	return s.registerTopicFiles(ctx, tr)
}

type fwFailure struct {
	at  time.Time
	err error
}

func NewService(api API, cache *Cache, cfg Config, auth *Auth) *Service {
	s := &Service{api: api, cache: cache, cfg: cfg, auth: auth, uMiss: map[string]time.Time{}, fwFail: map[string]fwFailure{}}
	s.notify = newNotifyManager(s)
	s.readState = newReadStateManager(cfg.DataDir)
	return s
}

// ReadState returns the current read state.
func (s *Service) ReadState() ReadState {
	if s.readState == nil {
		return ReadState{Threads: map[string]int64{}}
	}
	return s.readState.Get()
}

// MarkTopicRead marks a thread as read at the given timestamp.
func (s *Service) MarkTopicRead(threadID string, readTime int64) ReadState {
	if s.readState == nil {
		return ReadState{Threads: map[string]int64{}}
	}
	return s.readState.MarkRead(threadID, readTime)
}

// UpdateReadState updates multiple threads or initialization state.
func (s *Service) UpdateReadState(rs ReadState) ReadState {
	if s.readState == nil {
		return ReadState{Threads: map[string]int64{}}
	}
	return s.readState.Update(rs)
}

// Close stops background workers and releases resources.
func (s *Service) Close() {
	if s.notify != nil {
		s.notify.Stop()
	}
	if s.q != nil {
		s.q.Stop()
	}
}

// WatchTask registers interest in a topic so that when Daidainiao finishes its task,
// a notification is triggered.
func (s *Service) WatchTask(threadID, chatID, chatName, topicTitle, userOpenID string) {
	if s.notify != nil {
		s.notify.Watch(threadID, chatID, chatName, topicTitle, userOpenID)
	}
}

// NotifySettings returns user notification preferences.
func (s *Service) NotifySettings() NotificationSettings {
	if s.notify != nil {
		return s.notify.Settings()
	}
	return NotificationSettings{FeishuEnabled: true, BrowserEnabled: true, SoundEnabled: true}
}

// UpdateNotifySettings updates and persists notification preferences.
func (s *Service) UpdateNotifySettings(ns NotificationSettings) {
	if s.notify != nil {
		s.notify.UpdateSettings(ns)
	}
}

// TestFeishuNotice sends an immediate test notification card to userOpenID.
func (s *Service) TestFeishuNotice(ctx context.Context, userOpenID, webBaseURL string) error {
	if s.notify != nil {
		return s.notify.SendTestNotice(ctx, userOpenID, webBaseURL)
	}
	return nil
}

// NotifyTaskComplete sends task complete notification across configured channels.
func (s *Service) NotifyTaskComplete(ctx context.Context, threadID, chatName, topicTitle, userOpenID, snippet, webBaseURL string) (bool, error) {
	if s.notify != nil {
		return s.notify.NotifyTaskComplete(ctx, threadID, chatName, topicTitle, userOpenID, snippet, webBaseURL)
	}
	return false, nil
}

// k scopes a cache key to the logged-in user: chats and threads visible to
// one user must never be served to another.
func (s *Service) k(base string) string { return base + "@" + s.auth.UserID() }

func (s *Service) requireLogin() error {
	if !s.auth.LoggedIn() {
		return ErrLoginRequired
	}
	return nil
}

func (s *Service) lock(key string) func() {
	v, _ := s.locks.LoadOrStore(key, &sync.Mutex{})
	m := v.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}

// ---------- chats ----------

func (s *Service) Chats(ctx context.Context, refresh bool) ([]Chat, error) {
	if err := s.requireLogin(); err != nil {
		return nil, err
	}
	key := s.k("chats")
	var cached []Chat
	age, ok := s.cache.Get(key, &cached)
	if ok && !refresh && age < ttlChats {
		return cached, nil
	}
	defer s.lock(key)()
	if age, ok = s.cache.Get(key, &cached); ok && !refresh && age < ttlChats {
		return cached, nil
	}
	var all []Chat
	token := ""
	for page := 0; page < 50; page++ {
		p, err := s.api.ListChats(ctx, token)
		if err != nil {
			if ok {
				return cached, nil // stale fallback
			}
			return nil, err
		}
		all = append(all, p.Items...)
		if !p.HasMore || p.PageToken == "" {
			break
		}
		token = p.PageToken
	}
	if all == nil {
		all = []Chat{}
	}
	s.cache.Set(key, all)
	return all, nil
}

func (s *Service) chatName(ctx context.Context, chatID string) string {
	var cached []Chat
	if _, ok := s.cache.Get(s.k("chats"), &cached); !ok {
		cs, err := s.Chats(ctx, false)
		if err != nil {
			return chatID
		}
		cached = cs
	}
	for _, c := range cached {
		if c.ChatID == chatID {
			return c.Name
		}
	}
	return chatID
}

// ---------- thread discovery ----------

type threadRef struct {
	ThreadID string      `json:"thread_id"`
	Root     *RawMessage `json:"root,omitempty"`
}

func isRoot(m RawMessage) bool {
	return m.RootID == "" || m.RootID == m.MessageID
}

// threadRefs lists the most recent threads of a chat. In a normal group the
// chat container only returns the root message of each thread (the ones that
// carry a thread_id), so we scan recent history for those.
func (s *Service) threadRefs(ctx context.Context, chatID string, refresh bool) ([]threadRef, error) {
	key := s.k("threadrefs:" + chatID)
	var cached []threadRef
	age, ok := s.cache.Get(key, &cached)
	if ok && !refresh && age < ttlRefs {
		return cached, nil
	}
	defer s.lock(key)()
	if age, ok = s.cache.Get(key, &cached); ok && !refresh && age < ttlRefs {
		return cached, nil
	}
	refs := []threadRef{}
	seen := map[string]int{}
	token := ""
	for page := 0; page < s.cfg.HistoryPages && len(refs) < s.cfg.MaxTopicsPerChat; page++ {
		p, err := s.api.ListMessages(ctx, "chat", chatID, token, true)
		if err != nil {
			if ok {
				return cached, nil
			}
			return nil, err
		}
		for _, m := range p.Items {
			if m.ThreadID == "" {
				continue
			}
			idx, dup := seen[m.ThreadID]
			if !dup {
				if len(refs) >= s.cfg.MaxTopicsPerChat {
					continue
				}
				seen[m.ThreadID] = len(refs)
				ref := threadRef{ThreadID: m.ThreadID}
				if isRoot(m) {
					mm := m
					ref.Root = &mm
				}
				refs = append(refs, ref)
			} else if refs[idx].Root == nil && isRoot(m) {
				mm := m
				refs[idx].Root = &mm
			}
		}
		if !p.HasMore || p.PageToken == "" {
			break
		}
		token = p.PageToken
	}
	s.cache.Set(key, refs)
	return refs, nil
}

func msgTime(m RawMessage) int64 {
	n, _ := strconv.ParseInt(m.CreateTime, 10, 64)
	return n
}

func hasCardPlaceholder(ms []RawMessage) bool {
	for _, m := range ms {
		if m.MsgType == "interactive" && isCardPlaceholder(m.Body.Content) {
			return true
		}
	}
	return false
}

// threadMessages returns all messages of a thread in chronological order.
func (s *Service) threadMessages(ctx context.Context, threadID string, root *RawMessage, refresh bool) ([]RawMessage, error) {
	key := s.k("thread:" + threadID)
	var cached []RawMessage
	age, ok := s.cache.Get(key, &cached)
	if ok && !refresh && age < ttlThread && !hasCardPlaceholder(cached) {
		return s.attachRoot(key, cached, root), nil
	}
	defer s.lock(key)()
	if age, ok = s.cache.Get(key, &cached); ok && !refresh && age < ttlThread && !hasCardPlaceholder(cached) {
		return s.attachRoot(key, cached, root), nil
	}
	var all []RawMessage
	token := ""
	seenPages := map[string]bool{}
	for {
		if seenPages[token] {
			return nil, fmt.Errorf("话题消息分页重复，未能读取完整资料")
		}
		seenPages[token] = true
		p, err := s.api.ListMessages(ctx, "thread", threadID, token, false)
		if err != nil {
			if ok && (!refresh || isTransientErr(ctx, err)) {
				return s.attachRoot(key, cached, root), nil
			}
			return nil, err
		}
		all = append(all, p.Items...)
		if !p.HasMore {
			break
		}
		if p.PageToken == "" {
			return nil, fmt.Errorf("话题消息分页信息缺失，未能读取完整资料")
		}
		token = p.PageToken
	}
	// dedupe, make sure the root is present, sort chronologically.
	// The thread container omits the opening message; callers pass it in root.
	out := mergeRoot(all, root)
	s.cache.Set(key, out)
	return out, nil
}

// isTransientErr returns true when a request timed out or was cancelled while
// we already hold cached data that can keep background polling responsive.
func isTransientErr(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	return strings.Contains(err.Error(), "context deadline exceeded") || strings.Contains(err.Error(), "Client.Timeout")
}

// knownRoot is a root message already stored for this thread. A refresh must
// keep it: the thread container never returns the opening message.
func (s *Service) knownRoot(threadID string) *RawMessage {
	var cached []RawMessage
	if _, ok := s.cache.Get(s.k("thread:"+threadID), &cached); !ok {
		return nil
	}
	for i := range cached {
		if isRoot(cached[i]) {
			m := cached[i]
			return &m
		}
	}
	return nil
}

// attachRoot inserts root when the cached thread listing left it out, and
// writes the repaired list back so the next read does not drop it again.
func (s *Service) attachRoot(key string, msgs []RawMessage, root *RawMessage) []RawMessage {
	if root == nil || root.MessageID == "" {
		return msgs
	}
	for _, m := range msgs {
		if m.MessageID == root.MessageID {
			return msgs
		}
	}
	out := mergeRoot(msgs, root)
	s.cache.Set(key, out)
	return out
}

// ensureThreadRoot loads the opening message when every item in the thread
// listing is a reply (they point at a root_id that is not in the list).
func (s *Service) ensureThreadRoot(ctx context.Context, threadID string, raws []RawMessage) []RawMessage {
	for _, m := range raws {
		if isRoot(m) {
			return raws
		}
	}
	rootID := ""
	for _, m := range raws {
		if m.RootID != "" {
			rootID = m.RootID
			break
		}
	}
	if rootID == "" {
		return raws
	}
	items, err := s.api.GetMessage(ctx, rootID)
	if err != nil {
		return raws
	}
	for i := range items {
		if items[i].MessageID == rootID {
			m := items[i]
			return s.attachRoot(s.k("thread:"+threadID), raws, &m)
		}
	}
	return raws
}

// mergeRoot appends root when it is missing and returns messages in time order.
func mergeRoot(msgs []RawMessage, root *RawMessage) []RawMessage {
	seen := map[string]bool{}
	out := make([]RawMessage, 0, len(msgs)+1)
	for _, m := range msgs {
		if m.MessageID == "" || seen[m.MessageID] {
			continue
		}
		seen[m.MessageID] = true
		out = append(out, m)
	}
	if root != nil && root.MessageID != "" && !seen[root.MessageID] {
		out = append(out, *root)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := msgTime(out[i]), msgTime(out[j])
		if a != b {
			return a < b
		}
		return out[i].MessageID < out[j].MessageID
	})
	return out
}

// ---------- users ----------

func shortID(id string) string {
	if len(id) > 4 {
		return id[len(id)-4:]
	}
	return id
}

// resolveUsers returns user info for the given open_ids. Missing permission
// or unknown users fall back to hint names (from @mentions), never failing.
func (s *Service) resolveUsers(ctx context.Context, ids []string, hints map[string]string) (map[string]User, []string) {
	res := map[string]User{}
	var missing []string
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		var u User
		if age, ok := s.cache.Get("user:"+id, &u); ok && age < ttlUser && u.Name != "" {
			res[id] = u
			continue
		}
		missing = append(missing, id)
	}
	var warns []string
	if len(missing) > 0 {
		s.uMu.Lock()
		failing := time.Now().Before(s.uFailTill)
		failMsg := s.uFailMsg
		var toFetch []string
		for _, id := range missing {
			if t, ok := s.uMiss[id]; ok && time.Since(t) < userMissTL {
				continue
			}
			toFetch = append(toFetch, id)
		}
		s.uMu.Unlock()
		if failing {
			warns = append(warns, failMsg)
		} else if len(toFetch) > 0 {
			users, err := s.api.BatchUsers(ctx, toFetch)
			got := map[string]bool{}
			for _, u := range users {
				if u.OpenID == "" || u.Name == "" {
					continue
				}
				got[u.OpenID] = true
				res[u.OpenID] = u
				s.cache.Set("user:"+u.OpenID, u)
			}
			s.uMu.Lock()
			if err != nil {
				var fe *FeishuError
				msg := "无法获取用户姓名/头像（可能缺少 contact:user.base:readonly 权限或用户不在应用可见范围），已使用 @ 提及中的名称或 ID 代替"
				if errors.As(err, &fe) && fe.Msg != "" {
					msg += "：" + truncate(fe.Msg, 100)
				}
				s.uFailTill = time.Now().Add(userFailTL)
				s.uFailMsg = msg
				warns = append(warns, msg)
			} else {
				for _, id := range toFetch {
					if !got[id] {
						s.uMiss[id] = time.Now()
					}
				}
			}
			s.uMu.Unlock()
		}
	}
	for _, id := range ids {
		if _, ok := res[id]; ok || id == "" {
			continue
		}
		if n := hints[id]; n != "" {
			res[id] = User{OpenID: id, Name: n}
		}
	}
	return res, warns
}

// ---------- rendering ----------

type rendered struct {
	msgs  []Message
	atts  []Attachment
	warns []string
}

func senderIDs(raws []RawMessage) ([]string, map[string]string) {
	var ids []string
	hints := map[string]string{}
	for _, m := range raws {
		if m.Sender.IDType == "open_id" || (m.Sender.IDType == "" && strings.HasPrefix(m.Sender.ID, "ou_")) {
			ids = append(ids, m.Sender.ID)
		}
		for _, mn := range m.Mentions {
			if mn.ID != "" && mn.Name != "" {
				hints[mn.ID] = mn.Name
			}
		}
	}
	return ids, hints
}

func displayName(m RawMessage, users map[string]User) (name, avatar string) {
	if u, ok := users[m.Sender.ID]; ok && u.Name != "" {
		return u.Name, u.Avatar
	}
	switch m.Sender.SenderType {
	case "app":
		return "机器人/应用", ""
	case "anonymous":
		return "匿名用户", ""
	}
	if m.Sender.ID == "" {
		return "未知用户", ""
	}
	return "用户 " + shortID(m.Sender.ID), ""
}

// appLink is the AppLink host matching the configured Open API domain.
func (s *Service) appLink() string {
	if strings.Contains(s.cfg.BaseURL, "larksuite.com") {
		return "https://applink.larksuite.com"
	}
	return "https://applink.feishu.cn"
}

// chatURL opens a chat in the Feishu client. Feishu publishes no way to build
// a link to one message or topic, so this is the closest public deep link.
func (s *Service) chatURL(chatID string) string {
	if !keyRe.MatchString(chatID) {
		return ""
	}
	return s.appLink() + "/client/chat/open?openChatId=" + url.QueryEscape(chatID)
}

func (s *Service) parseOpts() ParseOpts {
	return ParseOpts{
		AppLink: s.appLink(),
		ChatName: func(id string) string {
			var cached []Chat
			if _, ok := s.cache.Get(s.k("chats"), &cached); ok {
				for _, c := range cached {
					if c.ChatID == id {
						return c.Name
					}
				}
			}
			return ""
		},
		UserName: func(id string) string {
			var u User
			if _, ok := s.cache.Get("user:"+id, &u); ok {
				return u.Name
			}
			return ""
		},
	}
}

// forwardSubs fetches the sub-messages of a merge_forward message.
func (s *Service) forwardSubs(ctx context.Context, m RawMessage) ([]RawMessage, error) {
	key := s.k("fwd:" + m.MessageID)
	var cached []RawMessage
	if age, ok := s.cache.Get(key, &cached); ok && age < ttlForward {
		return cached, nil
	}
	s.fwMu.Lock()
	if f, ok := s.fwFail[m.MessageID]; ok && time.Since(f.at) < fwFailTL {
		s.fwMu.Unlock()
		return nil, f.err
	}
	s.fwMu.Unlock()
	items, err := s.api.GetMessage(ctx, m.MessageID)
	if err != nil {
		s.fwMu.Lock()
		s.fwFail[m.MessageID] = fwFailure{at: time.Now(), err: err}
		s.fwMu.Unlock()
		return nil, err
	}
	subs := make([]RawMessage, 0, len(items))
	for _, it := range items {
		if it.MessageID != m.MessageID && it.MessageID != "" {
			subs = append(subs, it)
		}
	}
	s.cache.Set(key, subs)
	return subs, nil
}

// renderCtx carries what renderOne needs.
type renderCtx struct {
	users map[string]User
	opts  ParseOpts
	self  *UserInfo
	seen  map[string]bool
	r     *rendered
}

// renderOne converts one raw message. inForward marks messages that live
// inside a merged-forward message (their resources cannot be downloaded).
func (s *Service) renderOne(rc *renderCtx, m RawMessage, inForward bool, addAtts bool) Message {
	name, avatar := displayName(m, rc.users)
	if avatar == "" && rc.self != nil && m.Sender.ID == rc.self.OpenID {
		avatar = rc.self.AvatarURL
	}
	t := msgTime(m)
	msg := Message{
		ID: m.MessageID, ParentID: m.ParentID, SenderID: m.Sender.ID,
		SenderName: name, SenderAvatar: avatar, SenderType: m.Sender.SenderType,
		Time: t, MsgType: m.MsgType, Deleted: m.Deleted, Edited: m.Updated,
	}
	if m.Deleted {
		msg.Paragraphs = [][]Node{{{Type: "text", Text: "[消息已撤回]", Style: []string{"muted"}}}}
		msg.Plain = "[消息已撤回]"
		return msg
	}
	c := ParseContentOpts(m.MsgType, m.Body.Content, m.Mentions, rc.opts)
	msg.Title, msg.Paragraphs, msg.Plain, msg.Card, msg.Notice = c.Title, c.Paragraphs, c.Plain, c.Card, c.Notice
	if !addAtts {
		return msg
	}
	for _, a := range c.Attachments {
		a.MessageID = m.MessageID
		a.SenderName = name
		a.Time = t
		a.LastReferenced = max(t, cloudMillis(m.UpdateTime))
		if inForward && a.URL == "" {
			a.NoDownload = true
		}
		if a.URL != "" {
			a.ID = "u:" + a.URL
		} else {
			a.ID = a.ResType + ":" + m.MessageID + ":" + a.FileKey
		}
		if rc.seen[a.ID] {
			for i := range rc.r.atts {
				if rc.r.atts[i].ID == a.ID {
					rc.r.atts[i].LastReferenced = max(rc.r.atts[i].LastReferenced, a.LastReferenced)
					break
				}
			}
			continue
		}
		rc.seen[a.ID] = true
		rc.r.atts = append(rc.r.atts, a)
	}
	return msg
}

// renderForward builds the nested message list of a merge_forward message.
func (s *Service) renderForward(rc *renderCtx, parentID string, byUpper map[string][]RawMessage, depth int) []Message {
	var out []Message
	for _, sub := range byUpper[parentID] {
		msg := s.renderOne(rc, sub, true, true)
		if sub.MsgType == "merge_forward" && depth < maxFwdDepth {
			msg.Forwarded = s.renderForward(rc, sub.MessageID, byUpper, depth+1)
		}
		out = append(out, msg)
	}
	return out
}

func (s *Service) render(ctx context.Context, raws []RawMessage) rendered {
	// Repair interactive messages that only contain upgrade placeholders
	for i := range raws {
		m := &raws[i]
		if m.MsgType == "interactive" && !m.Deleted && isCardPlaceholder(m.Body.Content) {
			if items, err := s.api.GetMessage(ctx, m.MessageID); err == nil && len(items) > 0 {
				if !isCardPlaceholder(items[0].Body.Content) {
					m.Body.Content = items[0].Body.Content
				}
			}
		}
	}

	// 1. expand merged-forward messages first so their senders are resolved too
	fwd := map[string][]RawMessage{}
	fwdErr := map[string]error{}
	allForSenders := append([]RawMessage{}, raws...)
	for _, m := range raws {
		if m.MsgType != "merge_forward" || m.Deleted {
			continue
		}
		subs, err := s.forwardSubs(ctx, m)
		if err != nil {
			fwdErr[m.MessageID] = err
			continue
		}
		fwd[m.MessageID] = subs
		allForSenders = append(allForSenders, subs...)
	}
	for _, m := range allForSenders {
		if m.MsgType == "share_chat" {
			_, _ = s.Chats(ctx, false) // so shared group cards can show the group name
			break
		}
	}
	ids, hints := senderIDs(allForSenders)
	for _, m := range allForSenders {
		if m.MsgType == "interactive" && !m.Deleted {
			ids = append(ids, cardUserIDs(m.Body.Content)...)
		}
	}
	var self *UserInfo
	if st := s.auth.Status(); st.User != nil && st.User.OpenID != "" {
		self = st.User
		if st.User.Name != "" {
			// we already know the logged-in user's own name from the login
			hints[st.User.OpenID] = st.User.Name
		}
	}
	users, warns := s.resolveUsers(ctx, ids, hints)
	r := rendered{warns: warns}
	rc := &renderCtx{users: users, opts: s.parseOpts(), self: self, seen: map[string]bool{}, r: &r}
	for i, m := range raws {
		msg := s.renderOne(rc, m, false, true)
		msg.IsRoot = isRoot(m) && i == 0
		if m.MsgType == "merge_forward" && !m.Deleted {
			if _, bad := fwdErr[m.MessageID]; bad {
				msg.Notice = "forward_failed"
				msg.Plain = "[合并转发的聊天记录]"
			} else {
				// the root of the sub-message tree is the merge message itself,
				// but some responses omit upper_message_id on direct children
				byUpper := map[string][]RawMessage{}
				for _, sub := range fwd[m.MessageID] {
					up := sub.UpperMessageID
					if up == "" {
						up = m.MessageID
					}
					byUpper[up] = append(byUpper[up], sub)
				}
				msg.Forwarded = s.renderForward(rc, m.MessageID, byUpper, 1)
				if len(msg.Forwarded) > 0 {
					var parts []string
					for _, f := range msg.Forwarded {
						parts = append(parts, f.Plain)
					}
					msg.Plain = strings.TrimSpace("[合并转发的聊天记录]\n" + strings.Join(parts, "\n"))
				}
			}
		}
		r.msgs = append(r.msgs, msg)
	}
	return r
}

func (s *Service) buildTopic(chatID, chatName, threadID string, raws []RawMessage, r rendered) Topic {
	t := Topic{ThreadID: threadID, ChatID: chatID, ChatName: chatName, ReplyCount: -1}
	if len(raws) == 0 {
		return t
	}
	root := raws[0]
	for i := range raws {
		if isRoot(raws[i]) {
			root = raws[i]
			break
		}
	}
	var rm Message
	for _, m := range r.msgs {
		if m.ID == root.MessageID {
			rm = m
		}
	}
	t.RootMessageID = root.MessageID
	t.SenderID, t.SenderName, t.SenderAvatar, t.SenderType = rm.SenderID, rm.SenderName, rm.SenderAvatar, rm.SenderType
	t.CreateTime = msgTime(root)
	t.LastTime = r.msgs[len(r.msgs)-1].Time
	t.ReplyCount = len(raws) - 1
	t.AttachmentCount = 0
	for _, a := range r.atts {
		if a.Kind != "link" {
			t.AttachmentCount++
		}
	}
	title := rm.Title
	if title == "" {
		title = firstLine(rm.Plain, 80)
	}
	if title == "" {
		title = "（无文本内容）"
	}
	t.Title = title
	if rm.Title != "" {
		t.Snippet = firstLine(strings.TrimPrefix(rm.Plain, rm.Title), 120)
	}
	currUserID := s.auth.UserID()
	currUserName := ""
	if st := s.auth.Status(); st.User != nil {
		currUserName = st.User.Name
	}
	var userIDs []string
	seenU := map[string]bool{}
	involved := false
	for _, m := range raws {
		if m.Sender.ID != "" && !seenU[m.Sender.ID] {
			seenU[m.Sender.ID] = true
			userIDs = append(userIDs, m.Sender.ID)
		}
		if currUserID != "" && m.Sender.ID == currUserID {
			involved = true
		}
		if currUserName != "" && m.Sender.SenderType == "user" && (m.Sender.ID == currUserName || strings.EqualFold(m.Sender.ID, currUserName)) {
			involved = true
		}
		for _, mn := range m.Mentions {
			if mn.ID != "" && !seenU[mn.ID] {
				seenU[mn.ID] = true
				userIDs = append(userIDs, mn.ID)
			}
			if currUserID != "" && mn.ID == currUserID {
				involved = true
			}
			if currUserName != "" && mn.Name == currUserName {
				involved = true
			}
		}
		if currUserName != "" && strings.Contains(m.Body.Content, "@"+currUserName) {
			involved = true
		}
	}
	if currUserID != "" && (t.SenderID == currUserID || root.Sender.ID == currUserID) {
		involved = true
	}
	if currUserName != "" && (t.SenderName == currUserName || root.Sender.ID == currUserName) {
		involved = true
	}
	t.UserIDs = userIDs
	t.UserInvolved = involved
	return t
}

// MyTopics returns all topics across Daidainiao chats that are related to the logged-in user,
// ordered by last activity time descending.
func (s *Service) MyTopics(ctx context.Context, refresh bool) (*TopicsResp, error) {
	if err := s.requireLogin(); err != nil {
		return nil, err
	}
	allChats, err := s.Chats(ctx, refresh)
	if err != nil {
		return nil, err
	}
	var targetChats []Chat
	for _, c := range allChats {
		if strings.Contains(c.Name, "呆呆鸟") && !strings.Contains(c.Name, "总群") {
			targetChats = append(targetChats, c)
		}
	}
	if len(targetChats) == 0 {
		for _, c := range allChats {
			if strings.Contains(c.Name, "呆呆鸟") {
				targetChats = append(targetChats, c)
			}
		}
	}
	if len(targetChats) == 0 {
		targetChats = allChats
	}

	type chatResult struct {
		topics []Topic
		warns  []string
		err    error
	}
	results := make([]chatResult, len(targetChats))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, c := range targetChats {
		wg.Add(1)
		go func(idx int, chatID string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			resp, err := s.Topics(ctx, chatID, refresh)
			if err != nil {
				results[idx].err = err
				return
			}
			results[idx].topics = resp.Topics
			results[idx].warns = resp.Warnings
		}(i, c.ChatID)
	}
	wg.Wait()

	currUserID := s.auth.UserID()
	currUserName := ""
	if st := s.auth.Status(); st.User != nil {
		currUserName = st.User.Name
	}

	var myTopics []Topic
	seenThreads := map[string]bool{}
	warnSet := map[string]bool{}
	for _, r := range results {
		for _, w := range r.warns {
			if !warnSet[w] {
				warnSet[w] = true
			}
		}
		for _, tp := range r.topics {
			if seenThreads[tp.ThreadID] {
				continue
			}
			isRelated := tp.UserInvolved ||
				(currUserID != "" && tp.SenderID == currUserID) ||
				(currUserName != "" && tp.SenderName == currUserName)
			if isRelated {
				seenThreads[tp.ThreadID] = true
				myTopics = append(myTopics, tp)
			}
		}
	}

	sort.SliceStable(myTopics, func(i, j int) bool {
		return myTopics[i].LastTime > myTopics[j].LastTime
	})

	var warns []string
	for w := range warnSet {
		warns = append(warns, w)
	}
	return &TopicsResp{Topics: myTopics, Warnings: warns}, nil
}

// ---------- public operations ----------

func (s *Service) Topics(ctx context.Context, chatID string, refresh bool) (*TopicsResp, error) {
	if err := s.requireLogin(); err != nil {
		return nil, err
	}
	refs, err := s.threadRefs(ctx, chatID, refresh)
	if err != nil {
		return nil, err
	}
	chatName := s.chatName(ctx, chatID)
	type result struct {
		topic Topic
		warns []string
		err   error
	}
	results := make([]result, len(refs))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, ref := range refs {
		wg.Add(1)
		go func(i int, ref threadRef) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			raws, err := s.threadMessages(ctx, ref.ThreadID, ref.Root, refresh)
			if err != nil {
				results[i].err = err
				if ref.Root != nil {
					raws = []RawMessage{*ref.Root}
				} else {
					return
				}
			}
			r := s.render(ctx, raws)
			t := s.buildTopic(chatID, chatName, ref.ThreadID, raws, r)
			if err != nil {
				t.ReplyCount = -1
			}
			results[i].topic = t
			results[i].warns = r.warns
		}(i, ref)
	}
	wg.Wait()
	resp := &TopicsResp{ChatID: chatID, Topics: []Topic{}}
	warnSet := map[string]bool{}
	var firstErr error
	failed := 0
	for _, r := range results {
		if r.err != nil {
			failed++
			if firstErr == nil {
				firstErr = r.err
			}
		}
		if r.topic.ThreadID != "" {
			resp.Topics = append(resp.Topics, r.topic)
		}
		for _, w := range r.warns {
			if !warnSet[w] {
				warnSet[w] = true
				resp.Warnings = append(resp.Warnings, w)
			}
		}
	}
	if failed > 0 && len(resp.Topics) == 0 && firstErr != nil && len(s.pendingList(chatID)) == 0 {
		return nil, firstErr
	}
	if failed > 0 {
		resp.Warnings = append(resp.Warnings, fmt.Sprintf("%d 个话题的回复读取失败（回复数未知）：%s", failed, truncate(firstErr.Error(), 120)))
	}
	resp.Topics = append(resp.Topics, s.pendingTopics(ctx, chatID, chatName, resp.Topics)...)
	sort.SliceStable(resp.Topics, func(i, j int) bool { return resp.Topics[i].LastTime > resp.Topics[j].LastTime })
	return resp, nil
}

func (s *Service) Thread(ctx context.Context, threadID string, refresh bool) (*ThreadResp, error) {
	if err := s.requireLogin(); err != nil {
		return nil, err
	}
	if mid, ok := isPendingThread(threadID); ok {
		return s.pendingThread(ctx, threadID, mid, refresh)
	}
	// The thread container omits the opening message. Keep one we already
	// stored, and if the listing is only replies, load the root by its id.
	raws, err := s.threadMessages(ctx, threadID, s.knownRoot(threadID), refresh)
	if err != nil {
		return nil, err
	}
	raws = s.ensureThreadRoot(ctx, threadID, raws)
	if len(raws) == 0 {
		return nil, fmt.Errorf("话题 %s 没有消息", threadID)
	}
	r := s.render(ctx, raws)
	chatID := raws[0].ChatID
	topic := s.buildTopic(chatID, s.chatName(ctx, chatID), threadID, raws, r)
	// mark root properly
	for i := range r.msgs {
		r.msgs[i].IsRoot = r.msgs[i].ID == topic.RootMessageID
	}
	if r.atts == nil {
		r.atts = []Attachment{}
	}
	return s.registerDownloads(ctx, &ThreadResp{Topic: topic, Messages: r.msgs, Attachments: r.atts, Warnings: r.warns, OpenURL: s.chatURL(chatID)}), nil
}

// ---------- replying ----------

// ReplyMention is an @ picked in the composer.
type ReplyMention struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ReplyRequest is the body of POST /api/topics/{thread_id}/reply.
type ReplyRequest struct {
	Text     string         `json:"text"`
	Mentions []ReplyMention `json:"mentions,omitempty"`
	UUID     string         `json:"uuid,omitempty"`
	// Attachments are staged upload ids (POST /api/upload).
	Attachments []string `json:"attachments,omitempty"`
	// Force bypasses the Daidainiao task queue and sends immediately.
	Force bool `json:"force,omitempty"`
	// Encrypt selects attachment visibility. Nil and true keep the file private
	// to the sender and Daidainiao. False also grants the current chat view.
	Encrypt *bool `json:"encrypt,omitempty"`
}

const maxReplyRunes = 10000

// ErrReplyInvalid is returned for an unusable reply body.
var ErrReplyInvalid = errors.New("内容不能为空（可只发附件），文字不能超过 10000 个字")

// ErrReplyDisabled is returned when config.disable_reply is set.
var ErrReplyDisabled = errors.New("已在配置中禁用回复（disable_reply）")

// buildReplyText turns "@Name" typed through the @ picker into Feishu's
// <at user_id="ou_x">Name</at> markup. Only mentions the client explicitly
// picked (with a well-formed id) are converted; everything else stays plain.
func buildReplyText(text string, ms []ReplyMention) string {
	valid := make([]ReplyMention, 0, len(ms))
	for _, m := range ms {
		if keyRe.MatchString(m.ID) && m.Name != "" && !strings.ContainsAny(m.Name, "<>\"\n") {
			valid = append(valid, m)
		}
	}
	// longest names first so "@张伟伟" wins over "@张伟"
	sort.SliceStable(valid, func(i, j int) bool { return len([]rune(valid[i].Name)) > len([]rune(valid[j].Name)) })
	for _, m := range valid {
		text = strings.ReplaceAll(text, "@"+m.Name, `<at user_id="`+m.ID+`">`+m.Name+`</at>`)
	}
	return text
}

var uuidRe = regexp.MustCompile(`^[A-Za-z0-9_\-]{1,50}$`)

// rootMessageID finds the message a thread reply must be attached to.
func rootMessageID(raws []RawMessage) string {
	for _, m := range raws {
		if isRoot(m) {
			return m.MessageID
		}
	}
	if len(raws) > 0 {
		if raws[0].RootID != "" {
			return raws[0].RootID
		}
		return raws[0].MessageID
	}
	return ""
}
