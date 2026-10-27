// Package store persists the conversation cursor.
//
// A caller key maps to the upstream conversation id plus a cursor: how many
// messages of that caller's array the upstream already knows about. Backed by one
// JSON file, written atomically and serialised through a mutex so concurrent
// requests cannot lose updates.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry is one stored conversation.
type Entry struct {
	ConversationID string `json:"conversationId"`
	NextIndex      int    `json:"nextIndex"`
	UpdatedAt      int64  `json:"updatedAt"` // unix milliseconds
}

// Store is the file-backed cursor store.
type Store struct {
	path string
	ttl  time.Duration
	now  func() time.Time

	mu sync.Mutex
}

// New builds a store. ttl <= 0 disables expiry.
func New(path string, ttl time.Duration) *Store {
	return &Store{path: path, ttl: ttl, now: time.Now}
}

// NewWithClock is New with an injectable clock, for tests.
func NewWithClock(path string, ttl time.Duration, now func() time.Time) *Store {
	return &Store{path: path, ttl: ttl, now: now}
}

func (s *Store) ttlMillis() int64 { return s.ttl.Milliseconds() }

// readLocked loads and prunes the table. Callers must hold s.mu.
// A missing or corrupt file yields an empty table rather than an error.
func (s *Store) readLocked() map[string]Entry {
	table := map[string]Entry{}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return table
	}
	if err := json.Unmarshal(raw, &table); err != nil {
		return map[string]Entry{}
	}
	if s.ttl <= 0 {
		return table
	}
	cutoff := s.now().UnixMilli() - s.ttlMillis()
	for k, v := range table {
		if v.UpdatedAt < cutoff {
			delete(table, k)
		}
	}
	return table
}

// writeLocked persists the table atomically. Callers must hold s.mu.
func (s *Store) writeLocked(table map[string]Entry) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	buf, err := json.MarshalIndent(table, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".conv-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// Get returns the entry for key, or false.
func (s *Store) Get(key string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.readLocked()[key]
	return e, ok
}

// Set stores or replaces an entry.
func (s *Store) Set(key, conversationID string, nextIndex int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	table := s.readLocked()
	table[key] = Entry{
		ConversationID: conversationID,
		NextIndex:      nextIndex,
		UpdatedAt:      s.now().UnixMilli(),
	}
	return s.writeLocked(table)
}

// Clear removes key if present.
func (s *Store) Clear(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	table := s.readLocked()
	if _, ok := table[key]; !ok {
		return nil
	}
	delete(table, key)
	return s.writeLocked(table)
}

// Keys returns every live key, sorted by nothing in particular.
func (s *Store) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	table := s.readLocked()
	out := make([]string, 0, len(table))
	for k := range table {
		out = append(out, k)
	}
	return out
}
