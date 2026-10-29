package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ---------- pending topics ----------
//
// A root message sent from this app has no thread_id yet: in an ordinary group
// Feishu creates the thread when somebody replies with reply_in_thread=true.
// The topic list is built from messages that carry a thread_id, so a fresh
// message would be invisible. We remember such messages locally ("pending
// topics") and show them with 0 replies until they show up as real threads.

type pendingTopic struct {
	ChatID    string      `json:"chat_id"`
	MessageID string      `json:"message_id"`
	ThreadID  string      `json:"thread_id,omitempty"` // known once a reply created the thread
	Created   int64       `json:"created"`
	Root      *RawMessage `json:"root,omitempty"`
}

func (s *Service) pendingKey(chatID string) string { return s.k("pending:" + chatID) }

func (s *Service) pendingList(chatID string) []pendingTopic {
	var list []pendingTopic
	if _, ok := s.cache.Get(s.pendingKey(chatID), &list); !ok {
		return nil
	}
	return list
}

func (s *Service) updatePending(chatID string, fn func([]pendingTopic) []pendingTopic) {
	key := s.pendingKey(chatID)
	defer s.lock(key)()
	var list []pendingTopic
	s.cache.Get(key, &list)
	cutoff := time.Now().Add(-pendingTTL).UnixMilli()
	list = fn(list)
	out := list[:0:0]
	for _, p := range list {
		if p.Created >= cutoff {
			out = append(out, p)
		}
	}
	s.cache.Set(key, out)
}

func (s *Service) addPending(p pendingTopic) {
	s.updatePending(p.ChatID, func(l []pendingTopic) []pendingTopic {
		for i := range l {
			if l[i].MessageID == p.MessageID {
				if p.ThreadID != "" {
					l[i].ThreadID = p.ThreadID
				}
				return l
			}
		}
		return append(l, p)
	})
}

func (s *Service) dropPending(chatID string, messageIDs map[string]bool) {
	if len(messageIDs) == 0 {
		return
	}
	s.updatePending(chatID, func(l []pendingTopic) []pendingTopic {
		out := l[:0:0]
		for _, p := range l {
			if !messageIDs[p.MessageID] {
				out = append(out, p)
			}
		}
		return out
	})
}

// pendingTopics returns the pending topics of a chat that the thread scan did
// not (yet) find, and forgets those it did find.
func (s *Service) pendingTopics(ctx context.Context, chatID, chatName string, found []Topic) []Topic {
	list := s.pendingList(chatID)
	if len(list) == 0 {
		return nil
	}
	byThread := map[string]bool{}
	byRoot := map[string]bool{}
	for _, t := range found {
		byThread[t.ThreadID] = true
		byRoot[t.RootMessageID] = true
	}
	done := map[string]bool{}
	var out []Topic
	for _, p := range list {
		if byRoot[p.MessageID] || (p.ThreadID != "" && byThread[p.ThreadID]) {
			done[p.MessageID] = true
			continue
		}
		root := p.Root
		if root == nil {
			items, err := s.api.GetMessage(ctx, p.MessageID)
			if err != nil || len(items) == 0 {
				continue
			}
			root = &items[0]
		}
		tid := p.ThreadID
		if tid == "" {
			tid = pendingThreadID(p.MessageID)
		}
		raws := []RawMessage{*root}
		r := s.render(ctx, raws)
		t := s.buildTopic(chatID, chatName, tid, raws, r)
		t.Pending = p.ThreadID == ""
		out = append(out, t)
	}
	s.dropPending(chatID, done)
	return out
}

// ---------- thread view of a pending topic ----------

// pendingThread shows a root message that has no thread yet. If a thread
// appeared meanwhile (somebody replied in Feishu) the real thread is returned.
func (s *Service) pendingThread(ctx context.Context, id, mid string, refresh bool) (*ThreadResp, error) {
	items, err := s.api.GetMessage(ctx, mid)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("消息 %s 不存在", mid)
	}
	root := items[0]
	if root.ThreadID != "" {
		return s.Thread(ctx, root.ThreadID, refresh)
	}
	raws := []RawMessage{root}
	r := s.render(ctx, raws)
	topic := s.buildTopic(root.ChatID, s.chatName(ctx, root.ChatID), id, raws, r)
	topic.Pending = true
	for i := range r.msgs {
		r.msgs[i].IsRoot = true
	}
	if r.atts == nil {
		r.atts = []Attachment{}
	}
	return s.registerDownloads(ctx, &ThreadResp{Topic: topic, Messages: r.msgs, Attachments: r.atts, Warnings: r.warns, OpenURL: s.chatURL(root.ChatID)}), nil
}

// ---------- reply ----------

// topicDriveFolder returns the Feishu Drive subfolder name for a conversation:
// "<会话名>_<短ID>", matching the local download directory layout.
func (s *Service) topicDriveFolder(raws []RawMessage, fallbackTitle, key string) string {
	title := ""
	if len(raws) > 0 {
		root := raws[0]
		for i := range raws {
			if isRoot(raws[i]) {
				root = raws[i]
				break
			}
		}
		c := ParseContentOpts(root.MsgType, root.Body.Content, root.Mentions, s.parseOpts())
		title = c.Title
		if title == "" {
			title = firstLine(c.Plain, 60)
		}
	}
	if title == "" {
		title = firstLine(fallbackTitle, 60)
	}
	t := sanitizeName(truncateRunes(dlFirstLine(title), 30), 40)
	if key == "" {
		return t
	}
	short := dlShort(key)
	if t == "_" || t == "" {
		return "会话_" + short
	}
	return t + "_" + short
}

// Reply sends a reply (text and/or attachments) into a topic as the logged-in
// user: im/v1/messages/:root_message_id/reply with reply_in_thread=true. For a
// pending topic this is what creates the thread.
func (s *Service) ReplyFull(ctx context.Context, threadID string, req ReplyRequest) (*ComposeResult, error) {
	if err := s.requireLogin(); err != nil {
		return nil, err
	}
	if s.cfg.DisableReply {
		return nil, ErrReplyDisabled
	}
	text := strings.TrimRight(strings.ReplaceAll(req.Text, "\r\n", "\n"), " \t\n")
	if strings.TrimSpace(text) == "" && len(req.Attachments) == 0 {
		return nil, ErrReplyInvalid
	}
	if len([]rune(text)) > 4000 {
		return nil, fmt.Errorf("%w（最多 4000 字）", ErrReplyInvalid)
	}
	atts, err := s.validateAndLoadAttachments(req.Attachments)
	if err != nil {
		return nil, err
	}

	tgt := composeTarget{threadID: threadID}
	pendingMID, isPending := isPendingThread(threadID)
	var raws []RawMessage
	if isPending {
		items, err := s.api.GetMessage(ctx, pendingMID)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			return nil, fmt.Errorf("找不到消息 %s", pendingMID)
		}
		raws = items
		tgt.rootID, tgt.chatID, tgt.threadID = pendingMID, items[0].ChatID, items[0].ThreadID
		if tgt.threadID != "" {
			if traws, err := s.threadMessages(ctx, tgt.threadID, &items[0], true); err == nil && len(traws) > 0 {
				raws = traws
			}
		}
		tgt.topicFolder = s.topicDriveFolder(raws, req.Text, pendingMID)
	} else {
		var err error
		raws, err = s.threadMessages(ctx, threadID, s.knownRoot(threadID), true)
		if err != nil {
			raws, err = s.threadMessages(ctx, threadID, nil, false)
			if err != nil {
				return nil, err
			}
		}
		raws = s.ensureThreadRoot(ctx, threadID, raws)
		tgt.rootID = rootMessageID(raws)
		if len(raws) > 0 {
			tgt.chatID = raws[0].ChatID
		}
		if tgt.rootID == "" {
			return nil, fmt.Errorf("找不到话题 %s 的根消息", threadID)
		}
		key := tgt.rootID
		if key == "" {
			key = threadID
		}
		tgt.topicFolder = s.topicDriveFolder(raws, req.Text, key)
	}
	tgt.shareWithChat = shareChatRequested(req.Encrypt, len(req.Attachments))

	if !req.Force {
		hasQ := s.hasActiveQueue(threadID)
		r := s.render(ctx, raws)
		busy, status := s.topicBusyState(s.chatName(ctx, tgt.chatID), r.msgs, s.getLastSent(threadID), time.Now())
		if hasQ || busy {
			if status == "" {
				status = "⏳ 前序排队任务等待发送中…"
			}
			item := s.enqueueReply(threadID, tgt, req, atts)
			s.WatchTask(threadID, tgt.chatID, s.chatName(ctx, tgt.chatID), req.Text, s.auth.UserID())
			return &ComposeResult{
				ThreadID:  threadID,
				Queued:    true,
				QueueItem: item,
				Queue:     s.QueueFor(threadID),
				BotBusy:   true,
				BotStatus: status,
			}, nil
		}
	}

	res, err := s.compose(ctx, tgt, req.Text, req.Mentions, req.UUID, req.Attachments)
	if err != nil {
		return nil, err
	}
	if isPending {
		s.addPending(pendingTopic{ChatID: tgt.chatID, MessageID: pendingMID, ThreadID: res.ThreadID, Created: time.Now().UnixMilli()})
		s.invalidateChat(tgt.chatID, tgt.threadID, res.ThreadID)
	}
	s.markTopicSent(threadID, res.ThreadID)
	s.WatchTask(firstNonEmpty(res.ThreadID, threadID), tgt.chatID, s.chatName(ctx, tgt.chatID), req.Text, s.auth.UserID())
	res.Queue = s.QueueFor(firstNonEmpty(res.ThreadID, threadID))
	return res, nil
}

// Reply is the text-only convenience used by tests and older callers.
func (s *Service) Reply(ctx context.Context, threadID string, req ReplyRequest) (*Message, error) {
	res, err := s.ReplyFull(ctx, threadID, req)
	if err != nil {
		return nil, err
	}
	if len(res.Messages) == 0 {
		return nil, fmt.Errorf("无法解析飞书返回的消息")
	}
	return &res.Messages[0], nil
}

// ---------- new topic ----------

// NewTopicRequest is the body of POST /api/topics/new.
type NewTopicRequest struct {
	ChatID      string         `json:"chat_id"`
	Text        string         `json:"text"`
	Mentions    []ReplyMention `json:"mentions,omitempty"`
	UUID        string         `json:"uuid,omitempty"`
	Attachments []string       `json:"attachments,omitempty"`
	// ReplyTo starts the topic under an existing flat message of the chat
	// (reply_in_thread=true) instead of sending a new root message.
	ReplyTo string `json:"reply_to,omitempty"`
	// Encrypt selects attachment visibility. Nil and true keep the current
	// private grant. False also grants this chat view.
	Encrypt *bool `json:"encrypt,omitempty"`
}

// NewTopic starts a topic as the user. Without ReplyTo it sends a root message
// (im/v1/messages?receive_id_type=chat_id); with ReplyTo it replies in thread to
// that message, which creates the thread immediately.
func (s *Service) NewTopic(ctx context.Context, req NewTopicRequest) (*ComposeResult, error) {
	if err := s.requireLogin(); err != nil {
		return nil, err
	}
	if s.cfg.DisableReply {
		return nil, ErrReplyDisabled
	}
	if !msgIDRe.MatchString(req.ChatID) {
		return nil, badRequest("chat_id 非法")
	}
	tgt := composeTarget{chatID: req.ChatID}
	var rootRaw *RawMessage
	if req.ReplyTo != "" {
		if !msgIDRe.MatchString(req.ReplyTo) {
			return nil, badRequest("reply_to 非法")
		}
		items, err := s.api.GetMessage(ctx, req.ReplyTo)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 || items[0].ChatID != req.ChatID {
			return nil, badRequest("该消息不属于所选群聊")
		}
		rootRaw = &items[0]
		tgt.rootID = req.ReplyTo
		tgt.threadID = rootRaw.ThreadID
		tgt.topicFolder = s.topicDriveFolder(items, req.Text, req.ReplyTo)
	} else {
		tgt.topicFolder = s.topicDriveFolder(nil, req.Text, fmt.Sprintf("t%d", time.Now().Unix()))
	}
	tgt.shareWithChat = shareChatRequested(req.Encrypt, len(req.Attachments))
	res, err := s.compose(ctx, tgt, req.Text, req.Mentions, req.UUID, req.Attachments)
	if err != nil {
		return nil, err
	}
	if rootRaw == nil && len(res.raws) > 0 {
		rootRaw = &res.raws[0]
	}
	if rootRaw == nil {
		return res, nil
	}
	pt := pendingTopic{ChatID: req.ChatID, MessageID: rootRaw.MessageID, ThreadID: res.ThreadID, Created: time.Now().UnixMilli(), Root: rootRaw}
	s.addPending(pt)
	// topic summary for the left column
	tid := res.ThreadID
	if tid == "" {
		tid = pendingThreadID(rootRaw.MessageID)
	}
	s.WatchTask(tid, req.ChatID, s.chatName(ctx, req.ChatID), req.Text, s.auth.UserID())
	raws := []RawMessage{*rootRaw}
	r := s.render(ctx, raws)
	t := s.buildTopic(req.ChatID, s.chatName(ctx, req.ChatID), tid, raws, r)
	replies := len(res.raws)
	if req.ReplyTo == "" {
		replies--
	}
	if replies < 0 {
		replies = 0
	}
	t.ReplyCount = replies
	t.Pending = res.ThreadID == ""
	if n := len(res.Messages); n > 0 {
		t.LastTime = res.Messages[n-1].Time
	}
	res.Topic = &t
	if res.ThreadID == "" {
		res.ThreadID = tid
	}
	s.markTopicSent(res.ThreadID)
	return res, nil
}

// ---------- recent flat messages (start a topic under an existing message) ----------

// RecentMsg is a flat (thread-less) message offered in the "start a topic
// under an existing message" picker.
type RecentMsg struct {
	ID         string `json:"id"`
	SenderName string `json:"sender_name"`
	Time       int64  `json:"time"`
	MsgType    string `json:"msg_type"`
	Text       string `json:"text"`
}

// Recent lists the latest messages of a chat that are not part of a thread.
func (s *Service) Recent(ctx context.Context, chatID string) ([]RecentMsg, error) {
	if err := s.requireLogin(); err != nil {
		return nil, err
	}
	var raws []RawMessage
	token := ""
	for page := 0; page < s.cfg.HistoryPages && len(raws) < 25; page++ {
		p, err := s.api.ListMessages(ctx, "chat", chatID, token, true)
		if err != nil {
			if len(raws) > 0 {
				break
			}
			return nil, err
		}
		for _, m := range p.Items {
			if m.ThreadID != "" || m.Deleted || m.MsgType == "system" || m.MessageID == "" {
				continue
			}
			raws = append(raws, m)
			if len(raws) >= 25 {
				break
			}
		}
		if !p.HasMore || p.PageToken == "" {
			break
		}
		token = p.PageToken
	}
	r := s.render(ctx, raws)
	out := make([]RecentMsg, 0, len(r.msgs))
	for _, m := range r.msgs {
		txt := firstLine(m.Plain, 100)
		if txt == "" {
			txt = "[" + m.MsgType + "]"
		}
		if strings.TrimSpace(m.Title) != "" && !strings.Contains(txt, m.Title) {
			txt = m.Title + " " + txt
		}
		out = append(out, RecentMsg{ID: m.ID, SenderName: m.SenderName, Time: m.Time, MsgType: m.MsgType, Text: txt})
	}
	return out, nil
}
