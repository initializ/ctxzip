package ctxzip

import "github.com/initializ/ctxzip/ccr"

// Options configures Compress. Use DefaultOptions and adjust fields.
type Options struct {
	// Store receives originals of dropped content for later retrieval. If nil,
	// Compress creates an in-memory store and returns it via the Result's store
	// (use WithStore for an explicit, durable backend).
	Store ccr.Store

	// Query overrides the relevance context. When empty, Compress derives it
	// from the most recent user messages.
	Query string

	// FreezePrefix is the number of leading messages never compressed. This is
	// the cache hot zone (system prompt, early turns) — touching it would break
	// provider prompt caches. Default 1 (the system prompt).
	FreezePrefix int

	// ProtectRecent is the number of trailing messages never compressed, so the
	// model always sees the latest turns verbatim. Default 2.
	ProtectRecent int

	// MinTokens skips messages smaller than this; not worth the markers.
	MinTokens int

	// CompressRoles is the set of roles eligible for compression. Tool outputs
	// dominate token usage, so the default is tool + assistant only. User and
	// system messages are left alone.
	CompressRoles map[string]bool

	// SkipNames excludes messages by Name (for tool messages, the tool that
	// produced them). Hosts use this to exempt their retrieval/expansion tool:
	// compressing content the model just asked to expand recreates the marker
	// it tried to resolve — an expand/compress tail-chase.
	SkipNames map[string]bool
}

// DefaultOptions returns the recommended configuration.
func DefaultOptions() *Options {
	return &Options{
		FreezePrefix:  1,
		ProtectRecent: 2,
		MinTokens:     50,
		CompressRoles: map[string]bool{
			RoleTool:      true,
			RoleAssistant: true,
		},
	}
}

func (o *Options) compressRole(role string) bool {
	if len(o.CompressRoles) == 0 {
		return role == RoleTool || role == RoleAssistant
	}
	return o.CompressRoles[role]
}
