# ctxzip

**Context compression for AI agents — lossy on the wire, lossless end-to-end.**

`ctxzip` shrinks the bulky content an agent sends to an LLM — tool outputs,
logs, JSON, search results — before it reaches the provider, while guaranteeing
nothing is permanently lost. Every chunk it drops is hashed, cached locally, and
replaced inline by a retrievable marker:

```
<<ctxzip:386f010090c15e05dc536b7c 51_rows_offloaded>>
```

If the model later needs the original bytes, it retrieves them by hash. So
compression is **reversible**: lossy on the wire, lossless end-to-end.

It is a pure-Go, zero-dependency library that operates on its own `Message`
type, so it embeds into [forge](https://github.com/initializ/forge) or any Go
agent runtime via a thin adapter.

## Install

```bash
go get github.com/initializ/ctxzip
```

## Use

```go
import (
	"github.com/initializ/ctxzip"
	"github.com/initializ/ctxzip/ccr"
)

store := ccr.NewMemoryStore(ccr.MemoryConfig{})
opts := ctxzip.DefaultOptions()
opts.Store = store

res, _ := ctxzip.Compress(msgs, opts)   // msgs []ctxzip.Message
// res.Messages   — compressed, same length/order as input
// res.SavedTokens(), res.Ratio()

// Retrieve an offloaded original by the hash in a marker:
hashes := ccr.ExtractHashes(res.Messages[2].Content)
original, ok := ctxzip.Unzip(store, hashes[0])
```

## How it works

```
Compress(msgs)
  └─ for each message in the LIVE ZONE (not the frozen prefix / recent turns):
       detect content type  → JSON | log | diff | search | code | text
       route to compressor  → JSONCrusher | LogCrusher | TextCrusher(passthrough)
       keep what matters     → head+tail, error rows, query-relevant items
       drop the rest         → hash → CCR store → inline <<ctxzip:HASH>> marker
       inflation guard        → never grow a message
```

### The cardinal rule: passthrough is sacred

A provider prompt-cache hit needs a **byte-identical prefix** across turns.
Rewriting the system prompt, tool defs, or old turns destroys that and costs more
than it saves. So `ctxzip` freezes a prefix (`FreezePrefix`, default the system
message) and protects recent turns (`ProtectRecent`, default 2) — only the live
zone is compressed, and compression is deterministic so the frozen bytes stay
stable.

### Reversibility (CCR = Compress-Cache-Retrieve)

Dropped content is never deleted — it is moved to the `ccr.Store`, keyed by the
same hash embedded in the marker. Two backends ship:

- **`MemoryStore`** — in-process, 30-minute TTL. Zero setup, lost on exit.
- **`BoltStore`** — durable, on-disk (bbolt). Originals survive process restarts,
  so a marker handed back in a later run still resolves. TTL-swept and capped.

```go
store, _ := ccr.NewBoltStore(ccr.BoltConfig{Path: ".forge/ctxzip.db"})
defer store.Close()
opts.Store = store
```

Both satisfy `ccr.Store`, so they are drop-in interchangeable; a host can also
supply its own (Redis, SQLite) by implementing the three-method interface.

## Package layout

| Package    | Responsibility                                             |
|------------|------------------------------------------------------------|
| `ctxzip`   | `Compress` entrypoint, `Message`/`Result`/`Options`, `Unzip` |
| `detect`   | content-type detection (heuristic cascade)                 |
| `router`   | content type → compressor                                  |
| `crush`    | the compressors (`JSONCrusher`, `LogCrusher`, `TextCrusher`)|
| `ccr`      | Compress-Cache-Retrieve store, markers, hashing            |
| `tokenize` | approximate token counting                                 |

## Status & roadmap

v1 covers the structure-aware crushers where most agent tokens actually live
(tool outputs). Planned:

- [x] extractive **text** crusher (BM25 sentence selection) replacing the passthrough
- [x] durable `ccr.Store` backend (bbolt) — `BoltStore`, survives restarts
- [ ] dedicated **diff / search / code (AST)** crushers (code via tree-sitter, build-tagged)
- [ ] optional **ML prose** path (ModernBERT/ONNX) behind the same `Compressor` interface
- [ ] forge adapter: `llm.Client` wrapper + `AfterToolExec` hook + retrieve tool + provider cache hints

## License

Apache-2.0 (to match the forge ecosystem). Add a `LICENSE` file before publishing.
