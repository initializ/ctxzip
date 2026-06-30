package ccr

import (
	"testing"
	"time"
)

func TestHash_StableAndShort(t *testing.T) {
	a := Hash([]byte("hello world"))
	b := Hash([]byte("hello world"))
	if a != b {
		t.Fatalf("Hash not deterministic: %s != %s", a, b)
	}
	if len(a) != hashHexLen {
		t.Fatalf("hash length = %d, want %d", len(a), hashHexLen)
	}
	if Hash([]byte("hello world")) == Hash([]byte("hello worle")) {
		t.Fatalf("distinct inputs collided")
	}
}

func TestMarkerRoundTrip(t *testing.T) {
	h := Hash([]byte("payload"))
	m := Marker(h, "480_rows_offloaded")
	if !HasMarker(m) {
		t.Fatalf("HasMarker false for %q", m)
	}
	got := ExtractHashes("prefix " + m + " suffix")
	if len(got) != 1 || got[0] != h {
		t.Fatalf("ExtractHashes = %v, want [%s]", got, h)
	}
}

func TestMemoryStore_PutGet(t *testing.T) {
	s := NewMemoryStore(MemoryConfig{})
	h := Hash([]byte("data"))
	if err := s.Put(h, []byte("data"), Meta{ToolName: "t"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	e, ok := s.Get(h)
	if !ok || string(e.Original) != "data" {
		t.Fatalf("Get miss or wrong payload: ok=%v e=%q", ok, e.Original)
	}
	if s.Stats().Entries != 1 {
		t.Fatalf("Stats.Entries = %d, want 1", s.Stats().Entries)
	}
}

func TestMemoryStore_TTLExpiry(t *testing.T) {
	now := time.Unix(0, 0)
	s := NewMemoryStore(MemoryConfig{
		TTL: time.Minute,
		Now: func() time.Time { return now },
	})
	h := Hash([]byte("x"))
	_ = s.Put(h, []byte("x"), Meta{})

	now = now.Add(2 * time.Minute) // advance past TTL
	if _, ok := s.Get(h); ok {
		t.Fatalf("expected expired entry to be a miss")
	}
}

func TestMemoryStore_Eviction(t *testing.T) {
	s := NewMemoryStore(MemoryConfig{MaxEntries: 2})
	for i, p := range []string{"a", "b", "c"} {
		_ = s.Put(Hash([]byte(p)), []byte(p), Meta{ItemCount: i})
	}
	if got := s.Stats().Entries; got > 2 {
		t.Fatalf("eviction failed: entries=%d, want <=2", got)
	}
}
