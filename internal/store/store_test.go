package store

import (
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

func tmpPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "conv.json")
}

func TestSetGet(t *testing.T) {
	s := New(tmpPath(t), time.Hour)
	if err := s.Set("k", "conv-1", 3); err != nil {
		t.Fatal(err)
	}
	e, ok := s.Get("k")
	if !ok || e.ConversationID != "conv-1" || e.NextIndex != 3 {
		t.Fatalf("entry = %+v ok=%v", e, ok)
	}
}

func TestMissingKey(t *testing.T) {
	s := New(tmpPath(t), time.Hour)
	if _, ok := s.Get("nope"); ok {
		t.Fatal("expected no entry")
	}
}

func TestTTLExpiry(t *testing.T) {
	clock := time.UnixMilli(1_000_000)
	s := NewWithClock(tmpPath(t), time.Second, func() time.Time { return clock })
	if err := s.Set("k", "conv-1", 1); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(500 * time.Millisecond)
	if _, ok := s.Get("k"); !ok {
		t.Fatal("entry should still be live at 500ms")
	}
	clock = clock.Add(600 * time.Millisecond) // now 1100ms later
	if _, ok := s.Get("k"); ok {
		t.Fatal("entry should have expired past the 1s TTL")
	}
}

// A lost-update bug would keep only one of these; the mutex keeps all three.
func TestConcurrentDistinctKeys(t *testing.T) {
	s := New(tmpPath(t), time.Hour)
	var wg sync.WaitGroup
	for _, kv := range [][2]string{{"a", "conv-a"}, {"b", "conv-b"}, {"c", "conv-c"}} {
		wg.Add(1)
		go func(k, v string) {
			defer wg.Done()
			_ = s.Set(k, v, 1)
		}(kv[0], kv[1])
	}
	wg.Wait()

	keys := s.Keys()
	sort.Strings(keys)
	if len(keys) != 3 || keys[0] != "a" || keys[1] != "b" || keys[2] != "c" {
		t.Fatalf("keys = %v, want [a b c]", keys)
	}
	for _, kv := range [][2]string{{"a", "conv-a"}, {"b", "conv-b"}, {"c", "conv-c"}} {
		e, ok := s.Get(kv[0])
		if !ok || e.ConversationID != kv[1] {
			t.Fatalf("%s = %+v", kv[0], e)
		}
	}
}

func TestClearOnlyNamed(t *testing.T) {
	s := New(tmpPath(t), time.Hour)
	_ = s.Set("a", "conv-a", 1)
	_ = s.Set("b", "conv-b", 1)
	if err := s.Clear("a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("a"); ok {
		t.Fatal("a should be gone")
	}
	if _, ok := s.Get("b"); !ok {
		t.Fatal("b should remain")
	}
}

func TestCorruptFile(t *testing.T) {
	p := tmpPath(t)
	if err := os.WriteFile(p, []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(p, time.Hour)
	if _, ok := s.Get("k"); ok {
		t.Fatal("corrupt file should read as empty")
	}
	if err := s.Set("k", "conv-1", 1); err != nil {
		t.Fatal(err)
	}
	if e, ok := s.Get("k"); !ok || e.ConversationID != "conv-1" {
		t.Fatalf("entry after repair = %+v", e)
	}
}

func TestMissingFile(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "nope", "c.json"), time.Hour)
	if keys := s.Keys(); len(keys) != 0 {
		t.Fatalf("keys = %v", keys)
	}
}
