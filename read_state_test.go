package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadStateManagerPersistence(t *testing.T) {
	dir := t.TempDir()
	rm := newReadStateManager(dir)

	st := rm.Get()
	if st.Initialized || len(st.Threads) != 0 {
		t.Fatalf("expected empty uninitialized state, got %+v", st)
	}

	// Mark a single thread read
	rm.MarkRead("omt_123", 1000)
	st = rm.Get()
	if !st.Initialized {
		t.Errorf("expected initialized=true")
	}
	if st.Threads["omt_123"] != 1000 {
		t.Errorf("expected omt_123=1000, got %d", st.Threads["omt_123"])
	}

	// Monotonic time only
	rm.MarkRead("omt_123", 500)
	if rm.Get().Threads["omt_123"] != 1000 {
		t.Errorf("expected omt_123 to remain 1000")
	}

	// Update multiple threads
	rm.Update(ReadState{
		Initialized: true,
		Threads: map[string]int64{
			"omt_123": 1200,
			"omt_456": 2000,
		},
	})
	st = rm.Get()
	if st.Threads["omt_123"] != 1200 || st.Threads["omt_456"] != 2000 {
		t.Errorf("threads mismatch: %+v", st.Threads)
	}

	// Reload from new manager instance to verify disk persistence
	rm2 := newReadStateManager(dir)
	st2 := rm2.Get()
	if !st2.Initialized || st2.Threads["omt_123"] != 1200 || st2.Threads["omt_456"] != 2000 {
		t.Errorf("reloaded state mismatch: %+v", st2)
	}
}

func TestReadStateEndpoints(t *testing.T) {
	dir := t.TempDir()
	srv, _ := newTestServer(t)
	srv.svc.readState = newReadStateManager(dir)
	h := srv.Handler()

	// GET initial
	req := httptest.NewRequest(http.MethodGet, "/api/read_state", nil)
	req.Host = "localhost:8080"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET /api/read_state: %d", rec.Code)
	}
	var rs ReadState
	if err := json.Unmarshal(rec.Body.Bytes(), &rs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rs.Initialized {
		t.Errorf("expected initialized=false initially")
	}

	// POST mark single thread
	postBody, _ := json.Marshal(map[string]any{
		"thread_id": "omt_abc",
		"read_time": 9999,
	})
	req = httptest.NewRequest(http.MethodPost, "/api/read_state", bytes.NewReader(postBody))
	req.Host = "localhost:8080"
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("POST single: %d", rec.Code)
	}
	json.Unmarshal(rec.Body.Bytes(), &rs)
	if !rs.Initialized || rs.Threads["omt_abc"] != 9999 {
		t.Errorf("unexpected state after POST single: %+v", rs)
	}

	// POST batch / all
	postBody, _ = json.Marshal(map[string]any{
		"all": true,
		"threads": map[string]int64{
			"omt_abc": 10000,
			"omt_def": 5555,
		},
	})
	req = httptest.NewRequest(http.MethodPost, "/api/read_state", bytes.NewReader(postBody))
	req.Host = "localhost:8080"
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("POST batch: %d", rec.Code)
	}
	json.Unmarshal(rec.Body.Bytes(), &rs)
	if rs.Threads["omt_abc"] != 10000 || rs.Threads["omt_def"] != 5555 {
		t.Errorf("unexpected state after POST batch: %+v", rs)
	}
}
