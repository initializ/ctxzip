package ccr

import "time"

// Meta is the bookkeeping stored alongside an original payload. It lets a
// retrieve tool render a useful answer ("here are the 480 rows from the
// `list_pods` call") and lets the store report what it is holding.
type Meta struct {
	ToolName     string
	Query        string
	ItemCount    int    // number of items in the original, if known
	OriginalKind string // e.g. "json_array", "log"
	CreatedAt    time.Time
}

// Entry is a stored original plus its metadata.
type Entry struct {
	Hash     string
	Original []byte
	Meta     Meta
}

// Stats summarises a store's contents.
type Stats struct {
	Entries       int
	Retrievals    int
	OriginalBytes int64
}

// Store is the Compress-Cache-Retrieve backend. Implementations must be safe
// for concurrent use. The default is an in-memory store; a forge adapter can
// supply a bbolt- or SQLite-backed implementation that survives restarts.
type Store interface {
	// Put stores original under hash. Callers compute hash with Hash(original)
	// so that the inline marker and the store key match exactly.
	Put(hash string, original []byte, meta Meta) error
	// Get returns the entry for hash. ok is false if absent or expired.
	Get(hash string) (entry Entry, ok bool)
	// Stats reports current contents.
	Stats() Stats
}
