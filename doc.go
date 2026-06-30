// Package ctxzip is a context-compression engine for AI agents.
//
// It shrinks the bulky content an agent sends to an LLM — tool outputs,
// logs, JSON, search results, files — before it reaches the provider, while
// guaranteeing nothing is permanently lost. Every chunk it drops is hashed,
// cached locally, and replaced inline by a retrievable marker of the form
//
//	<<ctxzip:HASH 480_rows_offloaded>>
//
// If the model later needs the original bytes, it retrieves them by hash from
// the CCR (Compress-Cache-Retrieve) store. Compression is therefore lossy on
// the wire but lossless end-to-end.
//
// # Design
//
// ctxzip is deliberately provider-agnostic and has zero third-party
// dependencies. It operates on its own [Message] type, not on any agent
// runtime's message struct, so it can be embedded in forge or any other Go
// agent runtime via a thin adapter that maps to and from [Message].
//
//	import "github.com/initializ/ctxzip"
//
//	res, _ := ctxzip.Compress(msgs, ctxzip.DefaultOptions())
//	// res.Messages is the compressed slice; res.Store retrieves originals.
//
// The package is organised as:
//
//	detect    — content-type detection (JSON, log, diff, search, code, text)
//	router    — maps a content type to the right compressor
//	crush     — the compressors (JSON crusher, log crusher, text passthrough)
//	ccr       — Compress-Cache-Retrieve store, markers, hashing
//	tokenize  — approximate token counting
//
// # The cardinal rule: passthrough is sacred
//
// Compression must never touch the provider's cache hot zone (system prompt,
// tool definitions, old turns) — rewriting those bytes destroys prompt-cache
// hits and costs far more than it saves. [Options] therefore freezes a prefix
// and protects recent turns by default; only the "live zone" is compressed.
package ctxzip
