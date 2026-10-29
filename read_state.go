package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// ReadState stores per-topic read timestamps (milliseconds) for the user.
type ReadState struct {
	Initialized bool             `json:"initialized"`
	Threads     map[string]int64 `json:"threads"` // thread_id -> last_read_time (ms)
}

// ReadStateManager coordinates reading and persisting the user's read state.
type ReadStateManager struct {
	mu   sync.Mutex
	path string
	data ReadState
}

func newReadStateManager(dataDir string) *ReadStateManager {
	var p string
	if dataDir != "" {
		p = filepath.Join(dataDir, "settings", "read_state.json")
	}
	rm := &ReadStateManager{
		path: p,
		data: ReadState{
			Initialized: false,
			Threads:     make(map[string]int64),
		},
	}
	rm.load()
	return rm
}

func (rm *ReadStateManager) load() {
	if rm.path == "" {
		return
	}
	b, err := os.ReadFile(rm.path)
	if err != nil {
		return
	}
	var rs ReadState
	if json.Unmarshal(b, &rs) == nil {
		if rs.Threads == nil {
			rs.Threads = make(map[string]int64)
		}
		rm.data = rs
	}
}

func (rm *ReadStateManager) saveLocked() {
	if rm.path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(rm.path), 0o700)
	b, err := json.MarshalIndent(rm.data, "", "  ")
	if err == nil {
		_ = writeFileAtomic(rm.path, b, 0o600)
	}
}

// Get returns a copy of the current read state.
func (rm *ReadStateManager) Get() ReadState {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	res := ReadState{
		Initialized: rm.data.Initialized,
		Threads:     make(map[string]int64, len(rm.data.Threads)),
	}
	for k, v := range rm.data.Threads {
		res.Threads[k] = v
	}
	return res
}

// MarkRead marks a single thread as read at the given timestamp.
func (rm *ReadStateManager) MarkRead(threadID string, readTime int64) ReadState {
	if threadID == "" {
		return rm.Get()
	}
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if rm.data.Threads == nil {
		rm.data.Threads = make(map[string]int64)
	}
	if readTime > rm.data.Threads[threadID] {
		rm.data.Threads[threadID] = readTime
	}
	rm.data.Initialized = true
	rm.saveLocked()

	res := ReadState{
		Initialized: rm.data.Initialized,
		Threads:     make(map[string]int64, len(rm.data.Threads)),
	}
	for k, v := range rm.data.Threads {
		res.Threads[k] = v
	}
	return res
}

// Update updates multiple threads and/or the initialized flag.
func (rm *ReadStateManager) Update(rs ReadState) ReadState {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if rm.data.Threads == nil {
		rm.data.Threads = make(map[string]int64)
	}
	if rs.Initialized {
		rm.data.Initialized = true
	}
	for k, v := range rs.Threads {
		if v > rm.data.Threads[k] {
			rm.data.Threads[k] = v
		}
	}
	rm.saveLocked()

	res := ReadState{
		Initialized: rm.data.Initialized,
		Threads:     make(map[string]int64, len(rm.data.Threads)),
	}
	for k, v := range rm.data.Threads {
		res.Threads[k] = v
	}
	return res
}
