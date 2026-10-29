package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Cache is a small in-memory + on-disk JSON cache. Callers decide freshness
// from the returned age, which lets them fall back to stale data on errors.
type Cache struct {
	dir string
	mu  sync.RWMutex
	mem map[string]cacheEntry
}

type cacheEntry struct {
	Key  string          `json:"key"`
	At   time.Time       `json:"at"`
	Data json.RawMessage `json:"data"`
}

func NewCache(dir string) *Cache {
	if dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	return &Cache{dir: dir, mem: map[string]cacheEntry{}}
}

func (c *Cache) file(key string) string {
	h := sha1.Sum([]byte(key))
	return filepath.Join(c.dir, hex.EncodeToString(h[:])+".json")
}

// Get decodes the entry for key into out and returns its age.
func (c *Cache) Get(key string, out any) (time.Duration, bool) {
	c.mu.RLock()
	e, ok := c.mem[key]
	c.mu.RUnlock()
	if !ok && c.dir != "" {
		b, err := os.ReadFile(c.file(key))
		if err == nil && json.Unmarshal(b, &e) == nil && e.Key == key {
			ok = true
			c.mu.Lock()
			c.mem[key] = e
			c.mu.Unlock()
		}
	}
	if !ok {
		return 0, false
	}
	if err := json.Unmarshal(e.Data, out); err != nil {
		return 0, false
	}
	return time.Since(e.At), true
}

// Set stores v under key (memory + disk; disk errors are ignored).
func (c *Cache) Set(key string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	e := cacheEntry{Key: key, At: time.Now(), Data: data}
	c.mu.Lock()
	c.mem[key] = e
	c.mu.Unlock()
	if c.dir != "" {
		if b, err := json.Marshal(e); err == nil {
			_ = writeFileAtomic(c.file(key), b, 0o600)
		}
	}
}

// Delete drops key from memory and disk.
func (c *Cache) Delete(key string) {
	c.mu.Lock()
	delete(c.mem, key)
	c.mu.Unlock()
	if c.dir != "" {
		_ = os.Remove(c.file(key))
	}
}
