package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// EncryptedThread represents a topic bound 1:1 to a Feishu Docx document.
type EncryptedThread struct {
	ThreadID       string    `json:"thread_id"`
	DocToken       string    `json:"doc_token"`
	DocURL         string    `json:"doc_url"`
	FolderToken    string    `json:"folder_token"`
	FolderURL      string    `json:"folder_url"`
	Title          string    `json:"title"`
	OwnerOpenID    string    `json:"owner_openid"`
	ContextLimit   int       `json:"context_limit"` // -1: only this turn, 10: 10 turns, 20: 20 turns, 30: 30 turns, 0: all
	LatestRevision int       `json:"latest_revision"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// EncryptedMessage represents a user prompt, assistant reply, or topic runner card in an encrypted session.
type EncryptedMessage struct {
	ID          string    `json:"id"`
	ThreadID    string    `json:"thread_id"`
	TurnNumber  int       `json:"turn_number"`
	Role        string    `json:"role"`   // "user" | "assistant" | "system"
	Source      string    `json:"source"` // "topic" (话题记录) | "parsed" (文档解析)
	Content     string    `json:"content"`
	Card        *Card     `json:"card,omitempty"`
	RawMsgID    string    `json:"raw_msg_id,omitempty"`
	Attachments []string  `json:"attachments,omitempty"`
	Artifacts   []string  `json:"artifacts,omitempty"`
	IsFrozen    bool      `json:"is_frozen"`
	Status      string    `json:"status"` // "pending" | "running" | "done" | "failed"
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// EncryptedStoreData is the on-disk serializable structure.
type EncryptedStoreData struct {
	Threads  map[string]*EncryptedThread   `json:"threads"`
	Messages map[string][]EncryptedMessage `json:"messages"` // thread_id -> []EncryptedMessage
}

// EncryptedStore provides thread-safe local-first persistence for encrypted sessions.
type EncryptedStore struct {
	mu   sync.RWMutex
	path string
	data EncryptedStoreData
}

// NewEncryptedStore initializes the store at <dataDir>/encrypted/store.json.
func NewEncryptedStore(dataDir string) *EncryptedStore {
	var p string
	if dataDir != "" {
		p = filepath.Join(dataDir, "encrypted", "store.json")
	}
	s := &EncryptedStore{
		path: p,
		data: EncryptedStoreData{
			Threads:  make(map[string]*EncryptedThread),
			Messages: make(map[string][]EncryptedMessage),
		},
	}
	s.load()
	return s
}

func (s *EncryptedStore) load() {
	if s.path == "" {
		return
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var d EncryptedStoreData
	if json.Unmarshal(b, &d) == nil {
		if d.Threads == nil {
			d.Threads = make(map[string]*EncryptedThread)
		}
		if d.Messages == nil {
			d.Messages = make(map[string][]EncryptedMessage)
		}
		for tid, msgs := range d.Messages {
			for i := range msgs {
				if msgs[i].Source == "" {
					msgs[i].Source = "parsed"
				}
			}
			d.Messages[tid] = msgs
		}
		s.data = d
	}
}

func (s *EncryptedStore) saveLocked() {
	if s.path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.path), 0o700)
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err == nil {
		_ = writeFileAtomic(s.path, b, 0o600)
	}
}

// GetThread returns the thread metadata if present.
func (s *EncryptedStore) GetThread(threadID string) (*EncryptedThread, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.data.Threads[threadID]
	if !ok || t == nil {
		return nil, false
	}
	clone := *t
	return &clone, true
}

// SaveThread persists or updates a thread.
func (s *EncryptedStore) SaveThread(thread *EncryptedThread) error {
	if thread == nil || thread.ThreadID == "" {
		return fmt.Errorf("无效的会话数据")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if thread.CreatedAt.IsZero() {
		thread.CreatedAt = time.Now()
	}
	thread.UpdatedAt = time.Now()
	clone := *thread
	s.data.Threads[thread.ThreadID] = &clone
	s.saveLocked()
	return nil
}

// UpdateContextLimit updates the context limit policy for a thread.
func (s *EncryptedStore) UpdateContextLimit(threadID string, limit int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.data.Threads[threadID]
	if !ok {
		s.data.Threads[threadID] = &EncryptedThread{
			ThreadID:     threadID,
			ContextLimit: limit,
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		}
		s.saveLocked()
		return nil
	}
	t.ContextLimit = limit
	t.UpdatedAt = time.Now()
	s.saveLocked()
	return nil
}

// UpdateLatestRevision updates the cached revision ID for the Docx document.
func (s *EncryptedStore) UpdateLatestRevision(threadID string, revision int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.data.Threads[threadID]; ok {
		t.LatestRevision = revision
		t.UpdatedAt = time.Now()
		s.saveLocked()
	}
}

// ListThreads returns all known encrypted threads sorted by updated time descending.
func (s *EncryptedStore) ListThreads() []*EncryptedThread {
	s.mu.RLock()
	defer s.mu.RUnlock()
	res := make([]*EncryptedThread, 0, len(s.data.Threads))
	for _, t := range s.data.Threads {
		clone := *t
		res = append(res, &clone)
	}
	sort.Slice(res, func(i, j int) bool {
		return res[i].UpdatedAt.After(res[j].UpdatedAt)
	})
	return res
}

// GetMessages returns all messages for a thread in chronological order.
func (s *EncryptedStore) GetMessages(threadID string) []EncryptedMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	msgs, ok := s.data.Messages[threadID]
	if !ok {
		return []EncryptedMessage{}
	}
	res := make([]EncryptedMessage, len(msgs))
	copy(res, msgs)
	return res
}

// AddMessage appends a message to the thread message history.
func (s *EncryptedStore) AddMessage(msg EncryptedMessage) error {
	if msg.ThreadID == "" || msg.ID == "" {
		return fmt.Errorf("无效的消息数据")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if msg.Source == "" {
		msg.Source = "parsed"
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	msg.UpdatedAt = time.Now()
	s.data.Messages[msg.ThreadID] = append(s.data.Messages[msg.ThreadID], msg)
	s.saveLocked()
	return nil
}

// UpdateMessage mutates a specific message by its ID.
func (s *EncryptedStore) UpdateMessage(threadID, id string, updater func(m *EncryptedMessage)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs := s.data.Messages[threadID]
	for i := range msgs {
		if msgs[i].ID == id {
			updater(&msgs[i])
			msgs[i].UpdatedAt = time.Now()
			s.saveLocked()
			return true
		}
	}
	return false
}

// UpdateAssistantTurn updates or inserts the assistant message for a turn.
func (s *EncryptedStore) UpdateAssistantTurn(threadID string, turnNumber int, content string, artifacts []string, status string, isFrozen bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs := s.data.Messages[threadID]
	found := false
	for i := range msgs {
		if msgs[i].TurnNumber == turnNumber && msgs[i].Role == "assistant" && (msgs[i].Source == "parsed" || msgs[i].Source == "") {
			msgs[i].Content = content
			msgs[i].Artifacts = artifacts
			msgs[i].Status = status
			msgs[i].IsFrozen = isFrozen
			msgs[i].Source = "parsed"
			msgs[i].UpdatedAt = time.Now()
			found = true
			break
		}
	}
	if !found {
		s.data.Messages[threadID] = append(s.data.Messages[threadID], EncryptedMessage{
			ID:          fmt.Sprintf("msg_%s_turn%d_bot", threadID, turnNumber),
			ThreadID:    threadID,
			TurnNumber:  turnNumber,
			Role:        "assistant",
			Source:      "parsed",
			Content:     content,
			Artifacts:   artifacts,
			IsFrozen:    isFrozen,
			Status:      status,
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		})
	}
	s.saveLocked()
}

// SyncTopicCards merges interactive cards (such as Claude Code status cards) from the topic into the encrypted store.
func (s *EncryptedStore) SyncTopicCards(threadID string, topicMsgs []Message) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing := make(map[string]bool)
	for _, m := range s.data.Messages[threadID] {
		if m.RawMsgID != "" {
			existing[m.RawMsgID] = true
		}
		existing[m.ID] = true
	}

	type turnTime struct {
		turn int
		time time.Time
	}
	var turnTimes []turnTime
	for _, m := range s.data.Messages[threadID] {
		if m.Role == "user" && (m.Source == "parsed" || m.Source == "") {
			turnTimes = append(turnTimes, turnTime{turn: m.TurnNumber, time: m.CreatedAt})
		}
	}
	sort.Slice(turnTimes, func(i, j int) bool {
		return turnTimes[i].turn < turnTimes[j].turn
	})

	for _, tm := range topicMsgs {
		if tm.Card == nil {
			continue
		}
		title := tm.Card.Title
		isRunnerCard := strings.Contains(title, "Claude Code") ||
			strings.Contains(title, "运行结束") ||
			strings.Contains(title, "已停止") ||
			strings.Contains(title, "Model Selection") ||
			strings.Contains(title, "运行") ||
			strings.Contains(title, "Claude") ||
			strings.Contains(title, "Agent") ||
			strings.Contains(title, "执行") ||
			tm.SenderType == "app"

		if !isRunnerCard && title == "" {
			continue
		}

		cardID := "topic_" + tm.ID
		if existing[tm.ID] || existing[cardID] {
			continue
		}

		msgTime := time.UnixMilli(tm.Time)
		cardTurn := 0
		for i := len(turnTimes) - 1; i >= 0; i-- {
			if msgTime.After(turnTimes[i].time.Add(-5 * time.Second)) {
				cardTurn = turnTimes[i].turn
				break
			}
		}

		plainText := tm.Plain
		if plainText == "" {
			plainText = title
		}

		s.data.Messages[threadID] = append(s.data.Messages[threadID], EncryptedMessage{
			ID:         cardID,
			ThreadID:   threadID,
			TurnNumber: cardTurn,
			Role:       "assistant",
			Source:     "topic",
			Content:    plainText,
			Card:       tm.Card,
			RawMsgID:   tm.ID,
			IsFrozen:   true,
			Status:     "done",
			CreatedAt:  msgTime,
			UpdatedAt:  msgTime,
		})
		existing[tm.ID] = true
		existing[cardID] = true
	}

	// Sort messages: by turn_number asc, then user prompt first, then doc reply, then topic card
	msgs := s.data.Messages[threadID]
	sort.SliceStable(msgs, func(i, j int) bool {
		if msgs[i].TurnNumber != msgs[j].TurnNumber {
			return msgs[i].TurnNumber < msgs[j].TurnNumber
		}
		order := func(m EncryptedMessage) int {
			if m.Role == "user" {
				return 1
			}
			if m.Source == "parsed" {
				return 2
			}
			return 3 // topic card
		}
		oi, oj := order(msgs[i]), order(msgs[j])
		if oi != oj {
			return oi < oj
		}
		return msgs[i].CreatedAt.Before(msgs[j].CreatedAt)
	})
	s.data.Messages[threadID] = msgs
	s.saveLocked()
}

// FreezeTurn locks in an assistant message so it is marked immutable.
func (s *EncryptedStore) FreezeTurn(threadID string, turnNumber int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs := s.data.Messages[threadID]
	for i := range msgs {
		if msgs[i].TurnNumber == turnNumber {
			msgs[i].IsFrozen = true
			msgs[i].Status = "done"
			msgs[i].UpdatedAt = time.Now()
		}
	}
	s.saveLocked()
}

// GetLatestTurnNumber returns the maximum turn number recorded for the thread.
func (s *EncryptedStore) GetLatestTurnNumber(threadID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	msgs := s.data.Messages[threadID]
	maxTurn := 0
	for _, m := range msgs {
		if (m.Role == "user" || m.Source == "parsed") && m.TurnNumber > maxTurn {
			maxTurn = m.TurnNumber
		}
	}
	return maxTurn
}
