package ccr

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// BoltStore is a durable, on-disk CCR store backed by bbolt. Unlike
// MemoryStore it survives process restarts, so originals offloaded during one
// run are still retrievable in the next — important when a long-lived agent or
// a restarted proxy hands a <<ctxzip:HASH>> marker back to the model later.
//
// Layout: two buckets. "entries" maps hash -> JSON record (original + meta +
// expiry). "byexpiry" maps a big-endian(expiry)+hash key -> hash; because
// bbolt iterates keys in byte order, this bucket is a ready-made expiry queue
// for O(expired) TTL sweeps and O(1) eviction of the soonest-to-expire entry.
//
// The DB file is opened 0600 because offloaded tool output can be sensitive.
type BoltStore struct {
	db         *bolt.DB
	ttl        time.Duration
	maxEntries int
	now        func() time.Time

	mu             sync.Mutex
	count          int
	retrievals     int
	bytes          int64
	putsSincePurge int
}

// BoltConfig configures a BoltStore. Path is required; the rest fall back to
// the package defaults (30-minute TTL, 1000 entries).
type BoltConfig struct {
	Path       string
	TTL        time.Duration
	MaxEntries int
	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time
}

type boltRecord struct {
	Original  []byte `json:"o"`
	Meta      Meta   `json:"m"`
	ExpiresAt int64  `json:"e"` // unix nanoseconds
}

var (
	bucketEntries  = []byte("entries")
	bucketByExpiry = []byte("byexpiry")
)

// purgeEveryNPuts triggers an opportunistic full TTL sweep so expired entries
// that are never Get-ed cannot accumulate forever.
const purgeEveryNPuts = 128

// Compile-time check that BoltStore satisfies Store.
var _ Store = (*BoltStore)(nil)

// NewBoltStore opens (creating if needed) a durable store at cfg.Path.
// Call Close when done.
func NewBoltStore(cfg BoltConfig) (*BoltStore, error) {
	if cfg.Path == "" {
		return nil, errors.New("ccr: BoltConfig.Path is required")
	}
	if cfg.TTL <= 0 {
		cfg.TTL = DefaultTTL
	}
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = DefaultMaxEntries
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	db, err := bolt.Open(cfg.Path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("ccr: open bolt at %s: %w", cfg.Path, err)
	}
	s := &BoltStore{db: db, ttl: cfg.TTL, maxEntries: cfg.MaxEntries, now: cfg.Now}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.Purge(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// init creates buckets and recomputes the in-memory counters from disk.
func (s *BoltStore) init() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		eb, err := tx.CreateBucketIfNotExists(bucketEntries)
		if err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(bucketByExpiry); err != nil {
			return err
		}
		count, bytes := 0, int64(0)
		c := eb.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var rec boltRecord
			if json.Unmarshal(v, &rec) == nil {
				count++
				bytes += int64(len(rec.Original))
			}
		}
		s.count, s.bytes = count, bytes
		return nil
	})
}

// Put implements Store.
func (s *BoltStore) Put(hash string, original []byte, meta Meta) error {
	now := s.now()
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	rec := boltRecord{Original: original, Meta: meta, ExpiresAt: now.Add(s.ttl).UnixNano()}
	val, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("ccr: marshal record: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	err = s.db.Update(func(tx *bolt.Tx) error {
		eb := tx.Bucket(bucketEntries)
		xb := tx.Bucket(bucketByExpiry)

		// Replacing an existing hash: drop its old expiry-index entry first.
		if old := eb.Get([]byte(hash)); old != nil {
			var orec boltRecord
			if json.Unmarshal(old, &orec) == nil {
				_ = xb.Delete(indexKey(orec.ExpiresAt, hash))
				s.count--
				s.bytes -= int64(len(orec.Original))
			}
		}
		if err := eb.Put([]byte(hash), val); err != nil {
			return err
		}
		if err := xb.Put(indexKey(rec.ExpiresAt, hash), []byte(hash)); err != nil {
			return err
		}
		s.count++
		s.bytes += int64(len(original))

		for s.count > s.maxEntries {
			if !s.evictOldest(tx) {
				break
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("ccr: bolt put: %w", err)
	}

	if s.putsSincePurge++; s.putsSincePurge >= purgeEveryNPuts {
		s.putsSincePurge = 0
		_ = s.purgeLocked()
	}
	return nil
}

// Get implements Store.
func (s *BoltStore) Get(hash string) (Entry, bool) {
	var rec boltRecord
	found := false
	_ = s.db.View(func(tx *bolt.Tx) error {
		if v := tx.Bucket(bucketEntries).Get([]byte(hash)); v != nil {
			found = json.Unmarshal(v, &rec) == nil
		}
		return nil
	})
	if !found {
		return Entry{}, false
	}
	if s.now().UnixNano() > rec.ExpiresAt {
		s.deleteExpired(hash, rec)
		return Entry{}, false
	}
	s.mu.Lock()
	s.retrievals++
	s.mu.Unlock()
	return Entry{Hash: hash, Original: rec.Original, Meta: rec.Meta}, true
}

// Stats implements Store.
func (s *BoltStore) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{Entries: s.count, Retrievals: s.retrievals, OriginalBytes: s.bytes}
}

// Close releases the underlying database.
func (s *BoltStore) Close() error {
	return s.db.Close()
}

// Purge removes every expired entry. Called on open and opportunistically, and
// safe to call directly.
func (s *BoltStore) Purge() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.purgeLocked()
}

// purgeLocked sweeps expired entries in expiry order; the caller holds s.mu.
func (s *BoltStore) purgeLocked() error {
	nowNano := s.now().UnixNano()
	return s.db.Update(func(tx *bolt.Tx) error {
		xb := tx.Bucket(bucketByExpiry)
		eb := tx.Bucket(bucketEntries)
		c := xb.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if int64(binary.BigEndian.Uint64(k[:8])) > nowNano {
				break // keys are expiry-ordered; nothing after this is expired
			}
			hash := string(v)
			if rec := eb.Get([]byte(hash)); rec != nil {
				var r boltRecord
				if json.Unmarshal(rec, &r) == nil {
					s.bytes -= int64(len(r.Original))
				}
				_ = eb.Delete([]byte(hash))
			}
			_ = c.Delete()
			s.count--
		}
		return nil
	})
}

// evictOldest removes the soonest-to-expire entry; the caller holds s.mu and a
// write tx. Returns false when the store is empty.
func (s *BoltStore) evictOldest(tx *bolt.Tx) bool {
	xb := tx.Bucket(bucketByExpiry)
	eb := tx.Bucket(bucketEntries)
	k, v := xb.Cursor().First()
	if k == nil {
		return false
	}
	hash := string(v)
	if rec := eb.Get([]byte(hash)); rec != nil {
		var r boltRecord
		if json.Unmarshal(rec, &r) == nil {
			s.bytes -= int64(len(r.Original))
		}
		_ = eb.Delete([]byte(hash))
	}
	_ = xb.Delete(k)
	s.count--
	return true
}

// deleteExpired removes a single entry found expired during Get.
func (s *BoltStore) deleteExpired(hash string, rec boltRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.db.Update(func(tx *bolt.Tx) error {
		eb := tx.Bucket(bucketEntries)
		xb := tx.Bucket(bucketByExpiry)
		if eb.Get([]byte(hash)) != nil {
			_ = eb.Delete([]byte(hash))
			_ = xb.Delete(indexKey(rec.ExpiresAt, hash))
			s.count--
			s.bytes -= int64(len(rec.Original))
		}
		return nil
	})
}

// indexKey builds an expiry-ordered key: 8-byte big-endian expiry, then hash.
func indexKey(expiresAt int64, hash string) []byte {
	b := make([]byte, 8+len(hash))
	binary.BigEndian.PutUint64(b[:8], uint64(expiresAt))
	copy(b[8:], hash)
	return b
}
