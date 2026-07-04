package ccr

import (
	"sync"
	"time"
)

// DefaultTTL matches headroom's CCR TTL: long enough that the model can
// retrieve within a working session, short enough that originals do not
// accumulate. Past the TTL the disk/command is treated as the source of truth.
const DefaultTTL = 30 * time.Minute

// DefaultMaxEntries caps the in-memory store before LRU-ish eviction.
const DefaultMaxEntries = 1000

// MemoryConfig configures a MemoryStore. Zero values fall back to defaults.
type MemoryConfig struct {
	TTL        time.Duration
	MaxEntries int
	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time
}

// MemoryStore is the default in-process CCR store: a TTL map guarded by a
// mutex. It is intentionally simple — a forge adapter swaps in a durable
// backend by implementing Store.
type MemoryStore struct {
	mu         sync.Mutex
	entries    map[string]storedEntry
	ttl        time.Duration
	maxEntries int
	now        func() time.Time
	retrievals int
	bytes      int64
}

type storedEntry struct {
	entry   Entry
	expires time.Time
}

// NewMemoryStore builds an in-memory store.
func NewMemoryStore(cfg MemoryConfig) *MemoryStore {
	if cfg.TTL <= 0 {
		cfg.TTL = DefaultTTL
	}
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = DefaultMaxEntries
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &MemoryStore{
		entries:    make(map[string]storedEntry),
		ttl:        cfg.TTL,
		maxEntries: cfg.MaxEntries,
		now:        cfg.Now,
	}
}

// Put implements Store.
func (s *MemoryStore) Put(hash string, original []byte, meta Meta) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = s.now()
	}
	if _, exists := s.entries[hash]; !exists {
		s.evictIfNeeded()
		s.bytes += int64(len(original))
	}
	s.entries[hash] = storedEntry{
		entry:   Entry{Hash: hash, Original: original, Meta: meta},
		expires: s.now().Add(s.ttl),
	}
	return nil
}

// Get implements Store.
func (s *MemoryStore) Get(hash string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	se, ok := s.entries[hash]
	if !ok {
		return Entry{}, false
	}
	if s.now().After(se.expires) {
		delete(s.entries, hash)
		s.bytes -= int64(len(se.entry.Original))
		return Entry{}, false
	}
	s.retrievals++
	return se.entry, true
}

// Stats implements Store.
func (s *MemoryStore) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		Entries:       len(s.entries),
		Retrievals:    s.retrievals,
		OriginalBytes: s.bytes,
	}
}

// evictIfNeeded drops the soonest-to-expire entry when at capacity. Caller
// must hold the lock. This is an approximation of LRU keyed on expiry, which
// is good enough for a short-TTL cache.
func (s *MemoryStore) evictIfNeeded() {
	if len(s.entries) < s.maxEntries {
		return
	}
	var oldestKey string
	var oldest time.Time
	for k, v := range s.entries {
		if oldestKey == "" || v.expires.Before(oldest) {
			oldestKey, oldest = k, v.expires
		}
	}
	if oldestKey != "" {
		s.bytes -= int64(len(s.entries[oldestKey].entry.Original))
		delete(s.entries, oldestKey)
	}
}
