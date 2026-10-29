package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	queuePollInterval = 3 * time.Second
	maxBotRunAge      = 2 * time.Hour
	postSendGraceMs   = 15_000
	userMsgGraceMs    = 25_000
)

// QueuedAttachment describes one staged file attached to a queued reply.
type QueuedAttachment struct {
	UploadID string `json:"upload_id"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Kind     string `json:"kind"` // image | file
}

// QueuedItem is a message waiting to be sent once Daidainiao finishes its current task.
type QueuedItem struct {
	ID          string             `json:"id"`
	ThreadID    string             `json:"thread_id"`
	ChatID      string             `json:"chat_id"`
	RootID      string             `json:"root_id,omitempty"`
	TopicFolder string             `json:"topic_folder,omitempty"`
	Text        string             `json:"text"`
	Mentions    []ReplyMention     `json:"mentions,omitempty"`
	UUID        string             `json:"uuid,omitempty"`
	Attachments []QueuedAttachment `json:"attachments,omitempty"`
	// Encrypt nil means private. False grants the destination chat view at send time.
	Encrypt   *bool  `json:"encrypt,omitempty"`
	CreatedAt int64  `json:"created_at"`
	Status    string `json:"status"` // queued | sending | failed
	Error     string `json:"error,omitempty"`
}

type queueStore struct {
	mu         sync.Mutex
	loaded     bool
	byThread   map[string][]*QueuedItem
	lastSentAt map[string]int64
	wasBusy    map[string]bool
	workerRun  bool
	wakeCh     chan struct{}
	stopCh     chan struct{}
	wg         sync.WaitGroup
}

func (s *Service) initQueue() *queueStore {
	s.qOnce.Do(func() {
		s.q = &queueStore{
			byThread:   map[string][]*QueuedItem{},
			lastSentAt: map[string]int64{},
			wasBusy:    map[string]bool{},
			wakeCh:     make(chan struct{}, 1),
			stopCh:     make(chan struct{}),
		}
		s.loadQueueFromDisk()
	})
	return s.q
}

func (q *queueStore) Stop() {
	q.mu.Lock()
	clear(q.byThread)
	select {
	case <-q.stopCh:
	default:
		close(q.stopCh)
	}
	q.mu.Unlock()
	q.wg.Wait()
}

func (s *Service) queueFilePath() string {
	return filepath.Join(s.cfg.DataDir, "cache", "reply_queue.json")
}

func (s *Service) loadQueueFromDisk() {
	b, err := os.ReadFile(s.queueFilePath())
	if err != nil {
		return
	}
	var saved map[string][]*QueuedItem
	if json.Unmarshal(b, &saved) != nil {
		return
	}
	hasRunnable := false
	for tid, list := range saved {
		var clean []*QueuedItem
		for _, it := range list {
			if it == nil || it.ID == "" {
				continue
			}
			if it.Status == "sending" {
				it.Status = "queued"
			}
			if it.Status == "queued" {
				hasRunnable = true
			}
			clean = append(clean, it)
		}
		if len(clean) > 0 {
			s.q.byThread[tid] = clean
		}
	}
	if hasRunnable {
		s.ensureQueueWorker()
	}
}

func (s *Service) saveQueueLocked() {
	if s.cfg.DataDir == "" {
		return
	}
	p := s.queueFilePath()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	b, err := json.MarshalIndent(s.q.byThread, "", "  ")
	if err == nil {
		_ = writeFileAtomic(p, b, 0o600)
	}
}

// isStagedInQueue reports whether uploadID is referenced by any queued item so
// pruneStaged does not delete it while waiting in queue.
func (s *Service) isStagedInQueue(uploadID string) bool {
	q := s.initQueue()
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, list := range q.byThread {
		for _, it := range list {
			for _, a := range it.Attachments {
				if a.UploadID == uploadID {
					return true
				}
			}
		}
	}
	return false
}

func (s *Service) markTopicSent(threadIDs ...string) {
	q := s.initQueue()
	now := time.Now().UnixMilli()
	q.mu.Lock()
	for _, tid := range threadIDs {
		if tid != "" {
			q.lastSentAt[tid] = now
		}
	}
	q.mu.Unlock()
}

func (s *Service) getLastSent(threadID string) int64 {
	q := s.initQueue()
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.lastSentAt[threadID]
}

// QueueFor returns a snapshot of the queued items for threadID.
func (s *Service) QueueFor(threadID string) []QueuedItem {
	q := s.initQueue()
	q.mu.Lock()
	defer q.mu.Unlock()
	list := q.byThread[threadID]
	if len(list) == 0 {
		return nil
	}
	out := make([]QueuedItem, len(list))
	for i, it := range list {
		out[i] = *it
	}
	return out
}

func (s *Service) hasActiveQueue(threadID string) bool {
	q := s.initQueue()
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, it := range q.byThread[threadID] {
		if it.Status == "queued" || it.Status == "sending" {
			return true
		}
	}
	return false
}

// ---------- Daidainiao running task detection ----------

func isClaudeCard(plain string) bool {
	return strings.Contains(plain, "Claude Code ·") ||
		(strings.Contains(plain, "Claude Code") && strings.Contains(plain, "状态 "))
}

func isClaudeCardFinished(plain string) bool {
	return strings.Contains(plain, "本轮已结束") ||
		strings.Contains(plain, "运行结束") ||
		strings.Contains(plain, "运行出错") ||
		strings.Contains(plain, "已停止")
}

func isLegacyFinishedCard(plain string) bool {
	return strings.HasPrefix(plain, "✅ 已完成") ||
		strings.Contains(plain, "✅ 本回合已完成") ||
		strings.HasPrefix(plain, "❌ 错误") ||
		strings.TrimSpace(plain) == "执行已停止。" ||
		strings.TrimSpace(plain) == "执行失败，请查看运行记录。"
}

func extractBotStatusLine(plain string) string {
	lines := strings.Split(plain, "\n")
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "状态 ") {
			return strings.TrimSpace(strings.TrimPrefix(ln, "状态 "))
		}
	}
	if len(lines) > 0 && strings.TrimSpace(lines[0]) != "" {
		return strings.TrimSpace(lines[0])
	}
	return "🤖 呆呆鸟正在运行中…"
}

// topicBusyState inspects the rendered messages of a topic and determines
// whether Daidainiao (or the topic's bot) is currently executing a task.
func (s *Service) topicBusyState(chatName string, msgs []Message, lastSentMs int64, now time.Time) (bool, string) {
	nowMs := now.UnixMilli()
	maxAgeMs := maxBotRunAge.Milliseconds()

	// 1. Check the latest Claude Code status card in the topic
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.SenderType != "app" || m.Deleted {
			continue
		}
		if isClaudeCard(m.Plain) {
			if !isClaudeCardFinished(m.Plain) && (m.Time <= 0 || nowMs-m.Time < maxAgeMs) {
				return true, extractBotStatusLine(m.Plain)
			}
			break
		}
	}

	// 2. Check legacy / companion running cards after the last finished turn
	lastFinishedIdx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.SenderType == "app" && (isClaudeCardFinished(m.Plain) || isLegacyFinishedCard(m.Plain)) {
			lastFinishedIdx = i
			break
		}
	}
	for i := len(msgs) - 1; i > lastFinishedIdx; i-- {
		m := msgs[i]
		if m.SenderType != "app" || m.Deleted {
			continue
		}
		if m.Time > 0 && nowMs-m.Time >= maxAgeMs {
			continue
		}
		p := strings.TrimSpace(m.Plain)
		switch {
		case strings.HasPrefix(p, "⏳ 处理中"):
			return true, firstLine(p, 80)
		case strings.HasPrefix(p, "💭 思考与进展") && !strings.Contains(p, "本回合已完成"):
			return true, "💭 呆呆鸟正在思考与进展中…"
		case strings.HasPrefix(p, "🧵 后台任务") && !strings.Contains(p, "已全部结束"):
			return true, firstLine(p, 80)
		case strings.HasPrefix(p, "🤖 正在思考") || strings.HasPrefix(p, "⚡ 已中断，正在处理新指令") || strings.Contains(p, "正在继续处理…"):
			return true, firstLine(p, 80)
		case strings.HasPrefix(p, "🤖 Claude Code 会话已启动"):
			return true, "🤖 Claude Code 会话已启动，正在处理…"
		}
	}

	// 3. Check post-send startup window in Daidainiao topics (when a user message
	// was just sent seconds ago and Daidainiao hasn't posted its status card yet)
	isBotTopic := strings.Contains(chatName, "呆呆鸟")
	if !isBotTopic {
		for _, m := range msgs {
			if m.SenderType == "app" {
				isBotTopic = true
				break
			}
		}
	}
	if !isBotTopic {
		return false, ""
	}

	if lastSentMs > 0 && nowMs-lastSentMs >= 0 && nowMs-lastSentMs < postSendGraceMs {
		hasAppAfter := false
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].SenderType == "user" && !msgs[i].Deleted {
				break
			}
			if msgs[i].SenderType == "app" && !msgs[i].Deleted {
				hasAppAfter = true
				break
			}
		}
		if !hasAppAfter {
			return true, "⏳ 上一条消息刚发出，等待呆呆鸟启动任务…"
		}
	}

	lastUserIdx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].SenderType == "user" && !msgs[i].Deleted {
			lastUserIdx = i
			break
		}
	}
	if lastUserIdx >= 0 {
		uTime := msgs[lastUserIdx].Time
		if uTime > 0 && nowMs-uTime >= 0 && nowMs-uTime < userMsgGraceMs {
			// Check what app messages appeared after lastUserIdx
			hasTerminalAppAfter := false
			hasAnySubstantiveApp := false
			for i := lastUserIdx + 1; i < len(msgs); i++ {
				if msgs[i].SenderType != "app" || msgs[i].Deleted {
					continue
				}
				p := strings.TrimSpace(msgs[i].Plain)
				if strings.HasPrefix(p, "✅ 本话题使用：") {
					// model selection notice before Claude Code card is created
					continue
				}
				hasAnySubstantiveApp = true
				if isClaudeCardFinished(p) || isLegacyFinishedCard(p) || strings.HasPrefix(p, "📎 已收到文件") || strings.HasPrefix(p, "已保存本话题的") || strings.HasPrefix(p, "⚠️ 你还没有授权飞书") {
					hasTerminalAppAfter = true
				}
			}
			if !hasAnySubstantiveApp && !hasTerminalAppAfter {
				return true, "⏳ 消息刚发出，等待呆呆鸟启动任务…"
			}
		}
	}

	return false, ""
}

// enrichThreadQueue populates BotBusy, BotStatus, and Queue on a ThreadResp.
func (s *Service) enrichThreadQueue(tr *ThreadResp) *ThreadResp {
	if tr == nil {
		return nil
	}
	tid := tr.Topic.ThreadID
	busy, status := s.topicBusyState(tr.Topic.ChatName, tr.Messages, s.getLastSent(tid), time.Now())
	tr.BotBusy = busy
	tr.BotStatus = status
	tr.Queue = s.QueueFor(tid)
	if len(tr.Queue) > 0 {
		s.ensureQueueWorker()
		if !busy {
			s.wakeQueueWorker()
		}
	}
	if busy {
		s.WatchTask(tid, tr.Topic.ChatID, tr.Topic.ChatName, tr.Topic.Title, s.auth.UserID())
	}
	return tr
}

// ---------- Enqueuing & Managing Queued Replies ----------

func (s *Service) validateAndLoadAttachments(attIDs []string) ([]QueuedAttachment, error) {
	if len(attIDs) == 0 {
		return nil, nil
	}
	if !s.cfg.UploadEnabled() {
		return nil, ErrUploadDisabled
	}
	if len(attIDs) > maxAttachments {
		return nil, badRequest(fmt.Sprintf("一次最多发送 %d 个附件", maxAttachments))
	}
	seen := map[string]bool{}
	var out []QueuedAttachment
	for _, id := range attIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		st, _, err := s.loadStaged(id)
		if err != nil {
			return nil, err
		}
		out = append(out, QueuedAttachment{
			UploadID: st.ID,
			Name:     st.Name,
			Size:     st.Size,
			Kind:     st.Kind,
		})
	}
	return out, nil
}

func (s *Service) enqueueReply(threadID string, tgt composeTarget, req ReplyRequest, atts []QueuedAttachment) *QueuedItem {
	q := s.initQueue()
	var idb [8]byte
	_, _ = rand.Read(idb[:])
	item := &QueuedItem{
		ID:          "q_" + hex.EncodeToString(idb[:]),
		ThreadID:    threadID,
		ChatID:      tgt.chatID,
		RootID:      tgt.rootID,
		TopicFolder: tgt.topicFolder,
		Text:        strings.TrimRight(strings.ReplaceAll(req.Text, "\r\n", "\n"), " \t\n"),
		Mentions:    req.Mentions,
		UUID:        req.UUID,
		Attachments: atts,
		Encrypt:     req.Encrypt,
		CreatedAt:   time.Now().UnixMilli(),
		Status:      "queued",
	}
	if strings.TrimSpace(item.Text) == "" {
		item.Text = ""
	}
	q.mu.Lock()
	q.byThread[threadID] = append(q.byThread[threadID], item)
	s.saveQueueLocked()
	q.mu.Unlock()
	s.ensureQueueWorker()
	return item
}

// CancelQueued removes a queued item. When keepStaged is true (user clicked
// "撤回编辑"), staged attachment files are preserved so they can be re-sent.
func (s *Service) CancelQueued(threadID, queueID string, keepStaged bool) (*QueuedItem, []QueuedItem, error) {
	q := s.initQueue()
	q.mu.Lock()
	list := q.byThread[threadID]
	var removed *QueuedItem
	var next []*QueuedItem
	for _, it := range list {
		if it.ID == queueID && removed == nil {
			if it.Status == "sending" {
				q.mu.Unlock()
				return nil, nil, badRequest("该排队消息正在发送中，无法取消")
			}
			removed = it
			continue
		}
		next = append(next, it)
	}
	if removed == nil {
		q.mu.Unlock()
		return nil, nil, badRequest("找不到该排队消息（可能已自动发出）")
	}
	if len(next) == 0 {
		delete(q.byThread, threadID)
	} else {
		q.byThread[threadID] = next
	}
	s.saveQueueLocked()
	q.mu.Unlock()

	if !keepStaged {
		for _, a := range removed.Attachments {
			s.Unstage(a.UploadID)
		}
	}
	return removed, s.QueueFor(threadID), nil
}

// SendQueuedNow immediately sends a specific queued item without waiting for Daidainiao.
func (s *Service) SendQueuedNow(ctx context.Context, threadID, queueID string) (*ComposeResult, error) {
	if err := s.requireLogin(); err != nil {
		return nil, err
	}
	q := s.initQueue()
	q.mu.Lock()
	list := q.byThread[threadID]
	var target *QueuedItem
	for _, it := range list {
		if it.ID == queueID {
			if it.Status == "sending" {
				q.mu.Unlock()
				return nil, badRequest("该排队消息正在发送中")
			}
			it.Status = "sending"
			it.Error = ""
			target = it
			break
		}
	}
	q.mu.Unlock()
	if target == nil {
		return nil, badRequest("找不到该排队消息（可能已自动发出）")
	}
	return s.dispatchQueuedItem(ctx, threadID, target)
}

func (s *Service) dispatchQueuedItem(ctx context.Context, threadID string, item *QueuedItem) (*ComposeResult, error) {
	tgt := composeTarget{
		chatID:      item.ChatID,
		rootID:      item.RootID,
		threadID:    item.ThreadID,
		topicFolder: item.TopicFolder,
	}
	pendingMID, isPending := isPendingThread(threadID)
	if tgt.rootID == "" || tgt.chatID == "" {
		if isPending {
			if items, err := s.api.GetMessage(ctx, pendingMID); err == nil && len(items) > 0 {
				tgt.rootID, tgt.chatID, tgt.threadID = pendingMID, items[0].ChatID, items[0].ThreadID
				if tgt.topicFolder == "" {
					tgt.topicFolder = s.topicDriveFolder(items, item.Text, pendingMID)
				}
			}
		} else if raws, err := s.threadMessages(ctx, threadID, nil, false); err == nil && len(raws) > 0 {
			tgt.rootID = rootMessageID(raws)
			tgt.chatID = raws[0].ChatID
			if tgt.topicFolder == "" {
				tgt.topicFolder = s.topicDriveFolder(raws, item.Text, firstNonEmpty(tgt.rootID, threadID))
			}
		}
	}
	attIDs := make([]string, 0, len(item.Attachments))
	for _, a := range item.Attachments {
		attIDs = append(attIDs, a.UploadID)
	}
	tgt.shareWithChat = shareChatRequested(item.Encrypt, len(attIDs))
	res, err := s.compose(ctx, tgt, item.Text, item.Mentions, item.UUID, attIDs)
	q := s.initQueue()
	q.mu.Lock()
	if err != nil {
		item.Status = "failed"
		item.Error = err.Error()
		s.saveQueueLocked()
		q.mu.Unlock()
		return nil, err
	}
	// Remove sent item from queue
	list := q.byThread[threadID]
	var remain []*QueuedItem
	for _, it := range list {
		if it.ID != item.ID {
			remain = append(remain, it)
		}
	}
	realTID := res.ThreadID
	if realTID != "" && realTID != threadID && !strings.HasPrefix(realTID, pendingPrefix) {
		delete(q.byThread, threadID)
		if len(remain) > 0 {
			for _, it := range remain {
				it.ThreadID = realTID
			}
			q.byThread[realTID] = append(q.byThread[realTID], remain...)
		}
	} else if len(remain) == 0 {
		delete(q.byThread, threadID)
	} else {
		q.byThread[threadID] = remain
	}
	s.saveQueueLocked()
	q.mu.Unlock()

	if isPending {
		s.addPending(pendingTopic{ChatID: tgt.chatID, MessageID: pendingMID, ThreadID: res.ThreadID, Created: time.Now().UnixMilli()})
		s.invalidateChat(tgt.chatID, tgt.threadID, res.ThreadID)
	}
	s.markTopicSent(threadID, res.ThreadID)
	s.WatchTask(firstNonEmpty(res.ThreadID, threadID), tgt.chatID, s.chatName(ctx, tgt.chatID), item.Text, s.auth.UserID())
	res.Queue = s.QueueFor(firstNonEmpty(res.ThreadID, threadID))
	return res, nil
}

// ---------- Background Queue Worker ----------

func (s *Service) wakeQueueWorker() {
	q := s.initQueue()
	select {
	case q.wakeCh <- struct{}{}:
	default:
	}
}

func (s *Service) ensureQueueWorker() {
	q := s.initQueue()
	q.mu.Lock()
	if q.workerRun {
		q.mu.Unlock()
		return
	}
	q.workerRun = true
	q.wg.Add(1)
	q.mu.Unlock()
	go s.runQueueWorker()
}

func (s *Service) runQueueWorker() {
	q := s.initQueue()
	defer func() {
		q.mu.Lock()
		q.workerRun = false
		q.mu.Unlock()
		q.wg.Done()
	}()

	ticker := time.NewTicker(queuePollInterval)
	defer ticker.Stop()

	for {
		if !s.processQueueTick() {
			return
		}
		select {
		case <-q.stopCh:
			return
		case <-ticker.C:
		case <-q.wakeCh:
		}
	}
}

func (s *Service) processQueueTick() bool {
	q := s.initQueue()
	q.mu.Lock()
	var threads []string
	for tid, list := range q.byThread {
		for _, it := range list {
			if it.Status == "queued" {
				threads = append(threads, tid)
				break
			}
		}
	}
	q.mu.Unlock()

	if len(threads) == 0 {
		return false
	}
	if !s.auth.LoggedIn() {
		return true
	}

	for _, tid := range threads {
		s.checkAndDispatchThread(tid)
	}
	return true
}

func (s *Service) checkAndDispatchThread(threadID string) {
	q := s.initQueue()
	q.mu.Lock()
	var nextItem *QueuedItem
	for _, it := range q.byThread[threadID] {
		if it.Status == "sending" {
			q.mu.Unlock()
			return
		}
		if it.Status == "queued" {
			nextItem = it
			break
		}
	}
	q.mu.Unlock()
	if nextItem == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	var raws []RawMessage
	var chatID string
	if pendingMID, ok := isPendingThread(threadID); ok {
		items, err := s.api.GetMessage(ctx, pendingMID)
		if err != nil || len(items) == 0 {
			return
		}
		if items[0].ThreadID != "" {
			raws, _ = s.threadMessages(ctx, items[0].ThreadID, &items[0], true)
		} else {
			raws = items
		}
		chatID = items[0].ChatID
	} else {
		var err error
		raws, err = s.threadMessages(ctx, threadID, s.knownRoot(threadID), true)
		if err != nil {
			return
		}
		raws = s.ensureThreadRoot(ctx, threadID, raws)
		if len(raws) > 0 {
			chatID = raws[0].ChatID
		}
	}
	if chatID == "" {
		chatID = nextItem.ChatID
	}
	r := s.render(ctx, raws)
	busy, _ := s.topicBusyState(s.chatName(ctx, chatID), r.msgs, s.getLastSent(threadID), time.Now())

	q.mu.Lock()
	wasBusy := q.wasBusy[threadID]
	q.wasBusy[threadID] = busy
	q.mu.Unlock()

	if busy {
		return
	}
	// If it just transitioned from busy -> idle, wait 1.5s so any trailing file
	// attachment from Daidainiao lands in the thread before our queued message.
	if wasBusy {
		time.Sleep(1500 * time.Millisecond)
	}

	q.mu.Lock()
	var toSend *QueuedItem
	for _, it := range q.byThread[threadID] {
		if it.Status == "sending" {
			q.mu.Unlock()
			return
		}
		if it.Status == "queued" {
			it.Status = "sending"
			toSend = it
			break
		}
	}
	q.mu.Unlock()
	if toSend == nil {
		return
	}

	if _, err := s.dispatchQueuedItem(ctx, threadID, toSend); err != nil {
		log.Printf("queue dispatch failed for %s (%s): %v", threadID, toSend.ID, err)
	}
}
