package ccr

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func newBolt(t *testing.T, cfg BoltConfig) *BoltStore {
	t.Helper()
	if cfg.Path == "" {
		cfg.Path = filepath.Join(t.TempDir(), "ccr.db")
	}
	s, err := NewBoltStore(cfg)
	if err != nil {
		t.Fatalf("NewBoltStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestBoltStore_PutGet(t *testing.T) {
	s := newBolt(t, BoltConfig{})
	h := Hash([]byte("data"))
	if err := s.Put(h, []byte("data"), Meta{ToolName: "list_pods", ItemCount: 3}); err != nil {
		t.Fatal(err)
	}
	e, ok := s.Get(h)
	if !ok {
		t.Fatal("Get miss after Put")
	}
	if string(e.Original) != "data" {
		t.Fatalf("original = %q, want %q", e.Original, "data")
	}
	if e.Meta.ToolName != "list_pods" || e.Meta.ItemCount != 3 {
		t.Fatalf("meta not round-tripped: %+v", e.Meta)
	}
	if st := s.Stats(); st.Entries != 1 || st.Retrievals != 1 {
		t.Fatalf("stats = %+v, want Entries=1 Retrievals=1", st)
	}
}

// The whole point of the bolt store: data survives a restart.
func TestBoltStore_SurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ccr.db")
	h := Hash([]byte("persist me"))

	s1, err := NewBoltStore(BoltConfig{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Put(h, []byte("persist me"), Meta{}); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen the same file in a fresh store, as a restarted process would.
	s2, err := NewBoltStore(BoltConfig{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	e, ok := s2.Get(h)
	if !ok {
		t.Fatal("entry did not survive restart")
	}
	if string(e.Original) != "persist me" {
		t.Fatalf("original corrupted across restart: %q", e.Original)
	}
	if s2.Stats().Entries != 1 {
		t.Fatalf("counter not recovered on open: %+v", s2.Stats())
	}
}

func TestBoltStore_TTLExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	s := newBolt(t, BoltConfig{TTL: time.Minute, Now: func() time.Time { return now }})
	h := Hash([]byte("x"))
	if err := s.Put(h, []byte("x"), Meta{}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute) // advance past TTL
	if _, ok := s.Get(h); ok {
		t.Fatal("expected expired entry to be a miss")
	}
	if s.Stats().Entries != 0 {
		t.Fatalf("expired entry not reaped: %+v", s.Stats())
	}
}

func TestBoltStore_PurgeOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ccr.db")
	now := time.Unix(1000, 0)
	clock := func() time.Time { return now }

	s1, _ := NewBoltStore(BoltConfig{Path: path, TTL: time.Minute, Now: clock})
	_ = s1.Put(Hash([]byte("a")), []byte("a"), Meta{})
	_ = s1.Close()

	now = now.Add(2 * time.Minute) // entry now expired
	s2, err := NewBoltStore(BoltConfig{Path: path, TTL: time.Minute, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := s2.Stats().Entries; got != 0 {
		t.Fatalf("expired entry not purged on open: entries=%d", got)
	}
}

func TestBoltStore_Eviction(t *testing.T) {
	s := newBolt(t, BoltConfig{MaxEntries: 3})
	for i := 0; i < 10; i++ {
		p := []byte(fmt.Sprintf("payload-%d", i))
		if err := s.Put(Hash(p), p, Meta{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.Stats().Entries; got > 3 {
		t.Fatalf("eviction failed: entries=%d, want <=3", got)
	}
}

func TestBoltStore_ReplaceSameHash(t *testing.T) {
	s := newBolt(t, BoltConfig{})
	h := Hash([]byte("v1"))
	_ = s.Put(h, []byte("v1"), Meta{})
	_ = s.Put(h, []byte("v1"), Meta{}) // same hash again
	if got := s.Stats().Entries; got != 1 {
		t.Fatalf("re-Put of same hash double-counted: entries=%d, want 1", got)
	}
}

// BoltStore must be drop-in interchangeable with MemoryStore via the interface.
func TestBoltStore_SatisfiesStore(t *testing.T) {
	var _ Store = newBolt(t, BoltConfig{})
}
