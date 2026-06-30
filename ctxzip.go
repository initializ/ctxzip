package ctxzip

import (
	"strings"

	"github.com/initializ/ctxzip/ccr"
	"github.com/initializ/ctxzip/crush"
	"github.com/initializ/ctxzip/detect"
	"github.com/initializ/ctxzip/router"
	"github.com/initializ/ctxzip/tokenize"
)

// Compress shrinks the compressible messages in msgs and returns the result.
//
// It is non-destructive: msgs is not mutated; Result.Messages is a fresh slice.
// Only messages in the "live zone" (outside the frozen prefix and protected
// recent window) and of a compressible role are touched, so the provider's
// cache hot zone stays byte-stable. Every drop is reversible via the store —
// retrieve originals with Unzip.
//
// Compress never fails the request: on any per-message problem it leaves that
// message unchanged. The returned error is reserved for future use and is
// currently always nil.
func Compress(msgs []Message, opts *Options) (*Result, error) {
	if opts == nil {
		opts = DefaultOptions()
	}
	if opts.Store == nil {
		opts.Store = ccr.NewMemoryStore(ccr.MemoryConfig{})
	}
	query := opts.Query
	if query == "" {
		query = deriveQuery(msgs)
	}

	r := router.New()
	out := make([]Message, len(msgs))
	copy(out, msgs)
	res := &Result{}

	for i := range out {
		m := &out[i]
		before := tokenize.Estimate(m.Content)
		res.TokensBefore += before

		if !shouldCompress(i, len(out), m, before, opts) {
			res.TokensAfter += before
			continue
		}

		det := detect.Detect(m.Content)
		comp := r.For(det.Type)
		cr, err := comp.Compress(crush.Request{
			Content:  m.Content,
			Query:    query,
			ToolName: m.Name,
			Store:    opts.Store,
		})
		if err != nil {
			res.TokensAfter += before
			continue
		}

		after := tokenize.Estimate(cr.Compressed)
		// Inflation guard: never let "compression" grow a message.
		if after >= before {
			res.TokensAfter += before
			continue
		}

		m.Content = cr.Compressed
		res.TokensAfter += after
		res.Transforms = append(res.Transforms, Transform{
			Index:        i,
			Role:         m.Role,
			Strategy:     cr.Strategy,
			TokensBefore: before,
			TokensAfter:  after,
			Markers:      cr.Markers,
		})
	}

	res.Messages = out
	return res, nil
}

// Unzip retrieves an original blob previously offloaded during compression.
// It is a thin convenience over the store, for a retrieve tool to call when
// the model references a <<ctxzip:HASH>> marker.
func Unzip(store ccr.Store, hash string) ([]byte, bool) {
	if store == nil {
		return nil, false
	}
	entry, ok := store.Get(hash)
	if !ok {
		return nil, false
	}
	return entry.Original, true
}

// shouldCompress decides whether message i is eligible.
func shouldCompress(i, n int, m *Message, tokens int, opts *Options) bool {
	if i < opts.FreezePrefix {
		return false // cache hot zone
	}
	if i >= n-opts.ProtectRecent {
		return false // latest turns, kept verbatim
	}
	if !opts.compressRole(m.Role) {
		return false
	}
	if tokens < opts.MinTokens {
		return false
	}
	return true
}

// deriveQuery builds the relevance context from the most recent user messages.
func deriveQuery(msgs []Message) string {
	var parts []string
	for i := len(msgs) - 1; i >= 0 && len(parts) < 3; i-- {
		if msgs[i].Role == RoleUser {
			parts = append(parts, msgs[i].Content)
		}
	}
	return strings.Join(parts, " ")
}
