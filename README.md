# ctxzip

**Reversible context compression for AI agents — lossy on the wire, lossless end-to-end.**

`ctxzip` shrinks the bulky content an agent sends to an LLM — tool outputs, logs,
JSON arrays, grep results — before it reaches the provider, while guaranteeing
nothing is permanently lost. Every chunk it drops is hashed, cached locally, and
replaced inline by a retrievable marker:

```
<<ctxzip:ac998fea694b 149_lines_offloaded>>
```

If the model later needs the original bytes, it retrieves them by hash. So
compression is **reversible**: what leaves the wire is small, but the full data
remains one tool call away.

Pure Go. The only third-party dependency is [bbolt](https://github.com/etcd-io/bbolt)
(for the durable store). It operates on its own `Message` type, so it embeds into
[forge](https://github.com/initializ/forge) or any Go agent runtime through a thin
adapter — the engine knows nothing about any runtime's native structs.

---

## Why

Agent tool outputs are dominated by repetition: 149 pods that are `Running` and
one that is `CrashLoopBackOff`; 500 log lines that differ only by timestamp;
JSON list responses where the model needs three rows. Today's runtimes deal with
this by **truncating** — cutting output at N lines or K bytes and destroying
whatever fell past the cut, which is frequently the one row that mattered.

ctxzip inverts the tradeoff:

- **Keep what matters** — errors, anomalies, query-relevant items, boundaries.
- **Offload the rest** — hashed and stored locally, not deleted.
- **Let the model decide** — an inline marker says what was offloaded; the model
  calls a retrieval tool when (and only when) it needs the original.

Measured live against a forge agent over a 150-pod fixture: a 5.6 KB grep result
compressed **1,397 → 51 tokens (96%)** with the crashing pod kept verbatim in the
visible remainder, and the full original retrievable by marker hash.

## Install

```bash
go get github.com/initializ/ctxzip
```

## Quick start

```go
import (
    "github.com/initializ/ctxzip"
    "github.com/initializ/ctxzip/ccr"
)

// Durable store: originals survive process restarts.
store, _ := ccr.NewBoltStore(ccr.BoltConfig{Path: ".agent/ctxzip.db"})
defer store.Close()

opts := ctxzip.DefaultOptions()
opts.Store = store

res, _ := ctxzip.Compress(msgs, opts) // msgs []ctxzip.Message
// res.Messages     — compressed slice, same length/order as input
// res.SavedTokens() / res.Ratio()
// res.Transforms   — which messages changed, strategies, marker hashes

// Later, when the model references a marker:
hashes := ccr.ExtractHashes(res.Messages[2].Content)
original, ok := ctxzip.Unzip(store, hashes[0])
```

### Try it on real data

A demo CLI ships with the repo — pipe anything through it and watch the round trip:

```bash
kubectl get pods -o json | go run ./cmd/ctxzip-demo -q "any crashing pods?"
go run ./cmd/ctxzip-demo -q "why did the build fail" build.log
```

It prints the detected content type, tokens before/after, the compressed output,
then retrieves every offloaded original back from the store to prove reversibility.

## How it works

```
Compress(msgs)
  │
  ├─ for each message in the LIVE ZONE only
  │  (frozen prefix + recent turns are forwarded byte-identical)
  │
  ├─ detect content type        JSON array │ log │ search │ diff │ code │ text
  │
  ├─ route to a crusher
  │    JSONCrusher   — keep head/tail, errors, query matches; dedup; drop the rest
  │    LogCrusher    — keep error lines, head/tail; drop blank/duplicate noise
  │    TextCrusher   — line mode: dedup near-identical lines (numeric-insensitive)
  │                    prose mode: extractive BM25 sentence selection, verbatim only
  │
  ├─ every drop → SHA-256 hash → ccr.Store → inline <<ctxzip:HASH note>> marker
  │
  └─ inflation guard: a "compression" that grows a message is discarded
```

### What is never dropped

Fidelity is defense-in-depth; the layers below are cumulative:

1. **Error floor** — content matching error vocabulary (`error`, `fail`, `panic`,
   `timeout`, `crash`, `backoff`, `oomkilled`, `evicted`, …) is kept verbatim.
   Dropping the error the user is about to ask about is the catastrophic failure
   mode, so the list is deliberately generous.
2. **Caller vocabulary** — `Options.MustKeep` adds your domain's never-drop terms
   (product error codes, state words). Union semantics: it can only add
   protection, never disable the built-in floor.
3. **Query anchors** — items matching the relevance query survive. In line mode,
   query terms that match most lines are ignored as stop-terms (a file name in a
   grep query matches every line and carries no signal).
4. **Structure** — head/tail windows, first exemplar of each near-duplicate
   group, fragile tokens in prose (hex addresses, paths, ALLCAPS, CamelCase).
5. **Reversibility** — everything else is offloaded, not deleted. A drop is a
   move into the store.
6. **Source of truth** — after the store's TTL, the disk or the original command
   still holds the data; retrieval misses say exactly that.

### Reversibility (CCR: Compress-Cache-Retrieve)

Dropped content is stored under the first 12 hex chars of its SHA-256 — the same
hash embedded in the marker, so marker and store key can never drift apart.
(12 hex = 48 bits: ample for a ≤1000-entry, 30-minute cache, and short enough
that an LLM transcribing it into a tool call doesn't mangle it — live testing
showed models fumble 24-hex strings.)

Two stores ship; both satisfy the three-method `ccr.Store` interface:

| Store | Backing | Survives restart | Use |
|---|---|---|---|
| `MemoryStore` | in-process map | no | tests, short-lived processes |
| `BoltStore`   | bbolt file, 0600 | **yes** | agent runtimes |

`BoltStore` keeps an expiry-ordered index bucket, giving O(expired) TTL sweeps
and O(1) eviction of the soonest-to-expire entry. Default TTL 30 minutes,
capacity 1000 entries. Hosts can supply their own backend (Redis, SQLite) by
implementing the interface.

## Options

```go
type Options struct {
    Store         ccr.Store         // receives offloaded originals (required for lossy modes)
    Query         string            // relevance context; "" derives from recent user messages
    FreezePrefix  int               // leading messages never touched (default 1: system prompt)
    ProtectRecent int               // trailing messages never touched (default 2)
    MinTokens     int               // skip messages smaller than this (default 50)
    CompressRoles map[string]bool   // eligible roles (default: tool + assistant)
    SkipNames     map[string]bool   // exempt by message Name — e.g. your expansion tool
    MustKeep      []string          // domain vocabulary that must never be dropped
}
```

Notes that earn their place from live testing:

- **`SkipNames`** — exempt your retrieval tool's own output. Without it, the
  model expands a marker and the compressor crushes the expansion straight back
  into a marker: an expand/compress tail-chase.
- **`MustKeep`** — e.g. `[]string{"CrashLoopBackOff", "PAYMENT_DECLINED"}`.
  Case-insensitive substrings, checked in all three crushers.

## Determinism and prompt-cache discipline

Provider prompt caches key on **byte-identical prefixes**. A compressor that
rewrites history differently each turn silently destroys cache hits and can cost
more than it saves. ctxzip's rules:

- **Passthrough is sacred** — the frozen prefix and recent turns are forwarded
  untouched; only the live zone is compressed.
- **Same input, same bytes** — compression is fully deterministic. Callers
  should also pin `Options.Query` to something session-stable (e.g. the first
  user message) rather than the latest turn, so a historic message compresses
  identically on every request. (The empty-string default derives from *recent*
  messages, which is fine for one-shot use but turn-varying in a loop.)

## Embedding in an agent runtime

Three seams, in the order they earn their keep (the
[forge adapter](https://github.com/initializ/forge) implements all three):

1. **Tool-output hook** — compress each tool result once, at production time,
   before it enters conversation memory. The compressed bytes never change
   afterwards, so history stays cache-stable, and the work is done once instead
   of on every request.
2. **Client wrapper** — decorate your LLM client; compress the live zone of each
   outbound request. Catches whatever the hook didn't (bulk user pastes,
   assistant verbosity) and is the natural place for provider cache hints.
3. **Retrieval tool** — register a `context_expand(hash)`-style tool backed by
   `ctxzip.Unzip`. If your runtime owns the agent loop, no special retrieval
   machinery is needed — the model calls it like any other tool. Tolerate
   imperfect hashes (whole markers pasted, truncated hex → prefix-match against
   recently emitted hashes).

Runtime-owned model guidance matters: tell the model what markers are in the
**system prompt** (constant text, appended once, cache-stable) rather than
requiring every skill/prompt author to document compression.

## Package layout

| Package | Responsibility |
|---|---|
| `ctxzip` | `Compress`, `Unzip`, `Message` / `Options` / `Result` |
| `detect` | content-type detection (heuristic cascade, most-specific first) |
| `router` | content type → crusher |
| `crush` | `JSONCrusher`, `LogCrusher`, `TextCrusher`, relevance/BM25, error floor |
| `ccr` | `Store` interface, `MemoryStore`, `BoltStore`, hashing, marker grammar |
| `tokenize` | approximate token counting (CJK-aware; deliberately estimates high) |
| `cmd/ctxzip-demo` | pipe-anything demo CLI |

Design invariants, enforced by tests: non-empty input never compresses to empty
output; the caller's message slice is never mutated; a failed or unprofitable
compression returns the original (fail-open); marker hash == store key, always.

## Status & roadmap

Shipped:

- [x] JSON / log / text crushers with reversible offload
- [x] line-mode text compression (grep/log layout preserved byte-faithfully)
- [x] durable `BoltStore` (restart-safe originals)
- [x] caller `MustKeep` vocabulary + extended k8s error floor
- [x] `SkipNames` expansion-tool exemption
- [x] forge adapter (hook + client wrapper + `context_expand` + provider
      prompt-cache hints + audit events) — lives in the forge repo

Planned:

- [ ] dedicated diff / search / code (AST, build-tagged tree-sitter) crushers
- [ ] richer categorical drop summaries ("149 Running, 3 error-like") in markers
- [ ] optional ML prose path behind the same `Compressor` interface
- [ ] retrieval-mined `MustKeep` suggestions (every expansion is a signal that
      compression dropped something needed)

## Development

```bash
make test     # go test ./...
make check    # fmt + vet + test
make cover    # coverage report
```

Zero-dependency core; tests use no network and no fixtures outside the repo.

## License

Apache-2.0.
