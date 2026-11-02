package main

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
)

type countingIncrementalAPI struct {
	API
	mu       sync.Mutex
	calls    int
	messages []RawMessage
}

func (a *countingIncrementalAPI) ListMessages(ctx context.Context, typ, id, token string, desc bool) (*MsgPage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	var src []RawMessage
	for _, m := range a.messages {
		src = append(src, m)
	}
	if desc {
		// Newest first
		for i, j := 0, len(src)-1; i < j; i, j = i+1, j-1 {
			src[i], src[j] = src[j], src[i]
		}
	}
	off := 0
	if token != "" {
		off, _ = strconv.Atoi(token)
	}
	end := off + 50
	hasMore := false
	nextToken := ""
	if end < len(src) {
		hasMore = true
		nextToken = strconv.Itoa(end)
	} else {
		end = len(src)
	}
	return &MsgPage{Items: src[off:end], HasMore: hasMore, PageToken: nextToken}, nil
}

func TestIncrementalThreadSyncAndCachePreservation(t *testing.T) {
	_, s := newTestServer(t)

	// Build 120 historical messages (spread across 3 pages of 50 items each)
	var initialMsgs []RawMessage
	for i := 1; i <= 120; i++ {
		msg := RawMessage{
			MessageID:  fmt.Sprintf("om_inc_%03d", i),
			ThreadID:   "thread_inc_1",
			ChatID:     "oc_mock_dev",
			CreateTime: fmt.Sprintf("%d", 1000000+i*1000),
			MsgType:    "text",
		}
		msg.Body.Content = fmt.Sprintf(`{"text":"message %d"}`, i)
		initialMsgs = append(initialMsgs, msg)
	}

	api := &countingIncrementalAPI{API: s.api, messages: initialMsgs}
	s.api = api

	// 1. Initial cold load: pulls all 3 pages
	ctx := context.Background()
	msgs, err := s.threadMessages(ctx, "thread_inc_1", nil, true)
	if err != nil {
		t.Fatalf("initial load failed: %v", err)
	}
	if len(msgs) != 120 {
		t.Fatalf("expected 120 messages, got %d", len(msgs))
	}
	if api.calls != 3 {
		t.Fatalf("expected 3 pages for initial load, got %d calls", api.calls)
	}

	// 2. Add 2 new messages in Feishu
	api.mu.Lock()
	api.calls = 0
	m121 := RawMessage{
		MessageID:  "om_inc_121",
		ThreadID:   "thread_inc_1",
		ChatID:     "oc_mock_dev",
		CreateTime: "1121000",
		MsgType:    "text",
	}
	m121.Body.Content = `{"text":"new message 121"}`
	m122 := RawMessage{
		MessageID:  "om_inc_122",
		ThreadID:   "thread_inc_1",
		ChatID:     "oc_mock_dev",
		CreateTime: "1122000",
		MsgType:    "text",
	}
	m122.Body.Content = `{"text":"new message 122"}`
	api.messages = append(api.messages, m121, m122)
	api.mu.Unlock()

	// 3. Refresh should now only pull 1 page (50 items) and incrementally merge with local cache!
	msgs2, err := s.threadMessages(ctx, "thread_inc_1", nil, true)
	if err != nil {
		t.Fatalf("incremental refresh failed: %v", err)
	}
	if len(msgs2) != 122 {
		t.Fatalf("expected 122 merged messages, got %d", len(msgs2))
	}
	if api.calls != 1 {
		t.Fatalf("expected exactly 1 call for incremental update, got %d calls", api.calls)
	}

	// 4. Verify historical messages are preserved intact
	if msgs2[0].MessageID != "om_inc_001" || msgs2[121].MessageID != "om_inc_122" {
		t.Fatalf("message ordering or history lost: first=%s, last=%s", msgs2[0].MessageID, msgs2[121].MessageID)
	}

	// 5. Verify local cache is updated and preserved
	var cached []RawMessage
	_, ok := s.cache.Get(s.k("thread:thread_inc_1"), &cached)
	if !ok || len(cached) != 122 {
		t.Fatalf("cache should contain 122 messages, got ok=%v len=%d", ok, len(cached))
	}
}

func TestReplyFastWithoutFullRefreshAndPreservesCache(t *testing.T) {
	_, s := newTestServer(t)

	// Pre-seed local cache for existing mock topic omt_mock_002
	ctx := context.Background()
	initialMsgs, err := s.threadMessages(ctx, "omt_mock_002", nil, false)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	initialCount := len(initialMsgs)

	api := &countingIncrementalAPI{API: s.api, messages: initialMsgs}
	s.api = api

	// Send a reply
	res, err := s.ReplyFull(ctx, "omt_mock_002", ReplyRequest{Text: "快速回复测试", Force: true})
	if err != nil {
		t.Fatalf("ReplyFull failed: %v", err)
	}
	if len(res.Messages) == 0 {
		t.Fatalf("expected composed message, got none")
	}

	// Sending reply must NOT trigger ListMessages calls when local cache exists!
	if api.calls != 0 {
		t.Fatalf("expected 0 ListMessages calls during ReplyFull with existing cache, got %d", api.calls)
	}

	// The newly sent message should be appended to the local cache immediately
	var cached []RawMessage
	_, ok := s.cache.Get(s.k("thread:omt_mock_002"), &cached)
	if !ok || len(cached) != initialCount+1 {
		t.Fatalf("expected %d messages in cache after reply, got ok=%v len=%d", initialCount+1, ok, len(cached))
	}
}
