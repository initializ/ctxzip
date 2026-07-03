// Package crush holds the content-aware compressors. Each Compressor takes a
// single blob of content and returns a smaller rendering of it, offloading any
// dropped bytes to a ccr.Store and leaving inline markers behind so the drop
// is reversible.
package crush

import "github.com/initializ/ctxzip/ccr"

// Request is the input to a Compressor.
type Request struct {
	// Content is the raw text to compress.
	Content string
	// Query is the user's recent ask, used to keep relevant items. May be empty.
	Query string
	// ToolName labels the source for retrieval metadata. May be empty.
	ToolName string
	// MustKeep is the caller's extra force-keep vocabulary: any item, line, or
	// sentence containing one of these (case-insensitive substrings) is never
	// dropped. Union with the built-in error markers — it adds protection,
	// never removes it. Pass through NormalizeMustKeep first.
	MustKeep []string
	// Store receives originals of anything dropped. If nil, the compressor must
	// fall back to lossless behaviour (it may not drop anything it cannot store).
	Store ccr.Store
}

// Result is the output of a Compressor.
type Result struct {
	// Compressed is the rendered, smaller content. If a compressor cannot
	// improve on the input it returns the input unchanged.
	Compressed string
	// Strategy names the compressor (and any fallback chain) that produced this.
	Strategy string
	// Markers lists the ccr hashes embedded in Compressed, if any.
	Markers []string
}

// Compressor compresses one kind of content.
type Compressor interface {
	// Name is the strategy identifier, e.g. "json_crusher".
	Name() string
	// Compress shrinks req.Content. It must never return empty output for
	// non-empty input, and must be deterministic so a frozen prefix stays
	// byte-stable across turns.
	Compress(req Request) (Result, error)
}

// passthrough is the lossless no-op used as a fallback.
func passthrough(name, content string) Result {
	return Result{Compressed: content, Strategy: name}
}
