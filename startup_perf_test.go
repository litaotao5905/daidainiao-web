package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type startupAPI struct {
	*MockAPI
	mu         sync.Mutex
	calls      map[string]int
	beforeScan func(context.Context, string) error
}

func (a *startupAPI) record(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls[key]++
}

func (a *startupAPI) count(key string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[key]
}

func (a *startupAPI) ListChats(ctx context.Context, token string) (*ChatPage, error) {
	a.record("chats:" + token)
	if token == "" && a.beforeScan != nil {
		if err := a.beforeScan(ctx, "chats"); err != nil {
			return nil, err
		}
	}
	return a.MockAPI.ListChats(ctx, token)
}

func (a *startupAPI) ListMessages(ctx context.Context, typ, id, token string, desc bool) (*MsgPage, error) {
	key := typ + ":" + id
	a.record(key + ":" + token)
	if token == "" && a.beforeScan != nil {
		if err := a.beforeScan(ctx, key); err != nil {
			return nil, err
		}
	}
	return a.MockAPI.ListMessages(ctx, typ, id, token, desc)
}

func (a *startupAPI) GetMessage(ctx context.Context, id string) ([]RawMessage, error) {
	a.record("message:" + id)
	return a.MockAPI.GetMessage(ctx, id)
}

func (a *startupAPI) BatchUsers(ctx context.Context, ids []string) ([]User, error) {
	a.record("users")
	return a.MockAPI.BatchUsers(ctx, ids)
}

func trackStartupCalls(svc *Service) *startupAPI {
	a := &startupAPI{MockAPI: svc.api.(*MockAPI), calls: map[string]int{}}
	svc.api = a
	return a
}

func assertNoStartupCalls(t *testing.T, a *startupAPI) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.calls) != 0 {
		t.Fatalf("cached reads called Feishu: %v", a.calls)
	}
}

type cachedListResponse struct {
	Chats     []Chat  `json:"chats"`
	Topics    []Topic `json:"topics"`
	FromCache *bool   `json:"from_cache"`
}

func getStartupList(t *testing.T, srv *Server, path string) cachedListResponse {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Host = "localhost:8080"
	srv.Handler().ServeHTTP(w, r)
	var resp cachedListResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &resp) != nil {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
	}
	return resp
}

func TestStartupCacheMissIsOffline(t *testing.T) {
	srv, svc := newTestServer(t)
	a := trackStartupCalls(svc)
	chats := getStartupList(t, srv, "/api/chats?cached=1")
	if chats.FromCache == nil || *chats.FromCache || chats.Chats == nil || len(chats.Chats) != 0 {
		t.Fatalf("expected empty chat cache miss: %+v", chats)
	}
	for _, path := range []string{"/api/topics/my?cached=1", "/api/topics?chat_id=oc_mock_bot&cached=1"} {
		resp := getStartupList(t, srv, path)
		if resp.FromCache == nil || *resp.FromCache || resp.Topics == nil || len(resp.Topics) != 0 {
			t.Fatalf("expected empty topic cache miss for %s: %+v", path, resp)
		}
	}
	assertNoStartupCalls(t, a)
}

func TestStartupCacheRebuildsFromLegacyDiskEntries(t *testing.T) {
	srv, svc := newTestServer(t)
	a := trackStartupCalls(svc)
	const chatID = "oc_mock_bot"
	chat := Chat{ChatID: chatID, Name: "呆呆鸟自动化"}
	refs := []threadRef{}
	for _, id := range []string{"omt_mock_301", "omt_mock_302"} {
		raws := append([]RawMessage(nil), a.threads[id]...)
		raws[0].Mentions = append([]Mention(nil), raws[0].Mentions...)
		raws[0].Mentions = append(raws[0].Mentions, Mention{ID: svc.auth.UserID()})
		root := raws[0]
		refs = append(refs, threadRef{ThreadID: id, Root: &root})
		svc.cache.Set(svc.k("thread:"+id), raws[1:])
	}
	svc.cache.Set(svc.k("threadrefs:"+chatID), refs)
	svc.cache.Set(svc.k("chats"), []Chat{chat})
	// Make the only chats entry stale on disk before simulating a restart.
	key := svc.k("chats")
	entry := svc.cache.mem[key]
	entry.At = time.Now().Add(-time.Hour)
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(svc.cache.file(key), data, 0o600); err != nil {
		t.Fatal(err)
	}
	svc.cache = NewCache(svc.cache.dir)

	chats := getStartupList(t, srv, "/api/chats?cached=1")
	if chats.FromCache == nil || !*chats.FromCache || len(chats.Chats) != 1 || chats.Chats[0].ChatID != chatID {
		t.Fatalf("expected stale chats from disk: %+v", chats)
	}
	for _, path := range []string{"/api/topics/my?cached=1", "/api/topics?chat_id=" + chatID + "&cached=1"} {
		resp := getStartupList(t, srv, path)
		if resp.FromCache == nil || !*resp.FromCache || len(resp.Topics) != len(refs) {
			t.Fatalf("expected legacy topics for %s: %+v", path, resp)
		}
		first := resp.Topics[0]
		if first.ThreadID != "omt_mock_301" || first.ReplyCount != 6 || first.ChatName != chat.Name || !first.UserInvolved {
			t.Fatalf("cached topic lost its root, count, or user association: %+v", first)
		}
	}
	assertNoStartupCalls(t, a)
}

func TestStartupSnapshotsPersist(t *testing.T) {
	srv, svc := newTestServer(t)
	ctx := context.Background()
	my, err := svc.MyTopics(ctx, false)
	if err != nil || len(my.Topics) == 0 {
		t.Fatalf("generate my topics: %+v, %v", my, err)
	}
	const chatID = "oc_mock_dev"
	var group TopicsResp
	if _, ok := svc.cache.Get(svc.k("topics:"+chatID), &group); !ok || len(group.Topics) == 0 {
		t.Fatal("group scan did not write a snapshot")
	}
	svc.cache.Delete(svc.k("threadrefs:" + chatID))
	svc.cache.Delete(svc.k("chats"))
	svc.cache = NewCache(svc.cache.dir)
	a := trackStartupCalls(svc)
	for path, want := range map[string][]Topic{
		"/api/topics/my?cached=1":                     my.Topics,
		"/api/topics?chat_id=" + chatID + "&cached=1": group.Topics,
	} {
		got := getStartupList(t, srv, path)
		if got.FromCache == nil || !*got.FromCache || len(got.Topics) != len(want) {
			t.Fatalf("expected snapshot for %s: %+v", path, got)
		}
		for i, topic := range want {
			if got.Topics[i].ThreadID != topic.ThreadID || got.Topics[i].LastTime != topic.LastTime {
				t.Fatalf("topic %d from %s: got %+v, want %+v", i, path, got.Topics[i], topic)
			}
		}
	}
	assertNoStartupCalls(t, a)
}

func TestStartupChatsFreshWindowAndExplicitRefresh(t *testing.T) {
	srv, svc := newTestServer(t)
	a := trackStartupCalls(svc)
	key := svc.k("chats")
	svc.cache.Set(key, []Chat{{ChatID: "oc_cached", Name: "Cached group"}})
	svc.cache.mu.Lock()
	entry := svc.cache.mem[key]
	entry.At = time.Now().Add(-15 * time.Minute)
	svc.cache.mem[key] = entry
	svc.cache.mu.Unlock()
	if resp := getStartupList(t, srv, "/api/chats"); len(resp.Chats) != 1 || resp.Chats[0].ChatID != "oc_cached" || resp.FromCache != nil {
		t.Fatalf("expected existing chats during the freshness window: %+v", resp)
	}
	assertNoStartupCalls(t, a)
	if resp := getStartupList(t, srv, "/api/chats?refresh=1"); len(resp.Chats) <= 1 || resp.FromCache != nil {
		t.Fatalf("expected a live group list after explicit refresh: %+v", resp)
	}
	if got := a.count("chats:"); got != 1 {
		t.Fatalf("got %d chat scans on explicit refresh, want 1", got)
	}
}

// Signalled when the request begins waiting for its shared result.
type startupWaiter struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (ctx *startupWaiter) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}

func TestStartupTopicScansCoalesce(t *testing.T) {
	const chatScan = "chat:oc_mock_dev"
	for _, test := range []struct {
		name string
		gate string
		load func(context.Context, *Service) (*TopicsResp, error)
	}{
		{"my topics", "chats", func(ctx context.Context, s *Service) (*TopicsResp, error) { return s.MyTopics(ctx, false) }},
		{"group topics", chatScan, func(ctx context.Context, s *Service) (*TopicsResp, error) {
			return s.Topics(ctx, "oc_mock_dev", true)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, svc := newTestServer(t)
			a := trackStartupCalls(svc)
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := sync.OnceFunc(func() { close(release) })
			t.Cleanup(unblock)
			a.beforeScan = func(ctx context.Context, key string) error {
				if key != test.gate {
					return nil
				}
				once.Do(func() { close(started) })
				select {
				case <-release:
					return ctx.Err()
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			first, cancel := context.WithCancel(ctx)
			defer cancel()
			firstErr := make(chan error, 1)
			go func() { _, err := test.load(first, svc); firstErr <- err }()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("scan did not start")
			}
			type result struct {
				resp *TopicsResp
				err  error
			}
			const waiters = 6
			results := make(chan result, waiters)
			for range waiters {
				waiter := &startupWaiter{Context: ctx, waiting: make(chan struct{})}
				go func() { resp, err := test.load(waiter, svc); results <- result{resp, err} }()
				select {
				case <-waiter.waiting:
				case <-ctx.Done():
					t.Fatal("request did not join the scan")
				}
			}
			cancel()
			if err := <-firstErr; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled caller: %v", err)
			}
			unblock()
			var shared *TopicsResp
			for range waiters {
				got := <-results
				if got.err != nil || got.resp == nil || len(got.resp.Topics) == 0 {
					t.Fatalf("shared scan did not complete: %+v", got)
				}
				if shared != nil && shared != got.resp {
					t.Fatal("concurrent requests returned separate scan results")
				}
				shared = got.resp
			}
			if scans := a.count(chatScan + ":"); scans != 1 {
				t.Fatalf("got %d scans of the same chat, want 1", scans)
			}
			if chats := a.count("chats:"); chats != 1 {
				t.Fatalf("got %d chats requests, want 1", chats)
			}
		})
	}
}

func TestStartupListDoesNotRefetchHistoricalPlaceholder(t *testing.T) {
	_, svc := newTestServer(t)
	a := trackStartupCalls(svc)
	ctx := context.Background()
	const chatID = "oc_mock_bot"
	const threadID = "omt_mock_301"
	const messageKey = "message:om_mock_g06"
	const threadKey = "thread:" + threadID + ":"
	for range 3 {
		if _, err := svc.Topics(ctx, chatID, false); err != nil {
			t.Fatal(err)
		}
	}
	if got := a.count(threadKey); got != 1 || a.count(messageKey) != 0 {
		t.Fatalf("summary fetches = %d, card repairs = %d; want 1 and 0", got, a.count(messageKey))
	}
	if _, err := svc.Thread(ctx, threadID, false); err != nil {
		t.Fatal(err)
	}
	if got := a.count(threadKey); got != 2 || a.count(messageKey) != 1 {
		t.Fatalf("initial detail fetches = %d, card repairs = %d; want 2 and 1", got, a.count(messageKey))
	}
	if _, err := svc.Thread(ctx, threadID, false); err != nil {
		t.Fatal(err)
	}
	if got := a.count(threadKey); got != 2 {
		t.Fatalf("detail bypassed fresh cache during cooldown: %d fetches", got)
	}
	for range 2 {
		if _, err := svc.Topics(ctx, chatID, true); err != nil {
			t.Fatal(err)
		}
	}
	if got := a.count(threadKey); got != 4 || a.count(messageKey) != 1 {
		t.Fatalf("forced fetches = %d, card repairs = %d; want 4 and 1", got, a.count(messageKey))
	}
	svc.cardFailures.Store(svc.k("card:om_mock_g06"), time.Now().Add(-time.Second))
	if _, err := svc.Thread(ctx, threadID, false); err != nil {
		t.Fatal(err)
	}
	if got := a.count(messageKey); got != 2 {
		t.Fatalf("got %d repairs, want the detail view to retry after cooldown", got)
	}
}
