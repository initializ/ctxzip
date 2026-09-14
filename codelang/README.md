# ctxzip/codelang

Tree-sitter-backed **Tier B** code crusher for ctxzip: signature-keeping,
body-eliding compression for **Python, TypeScript, JavaScript, Java, C, and
C++** — the languages the core crusher's stdlib `go/ast` path (Tier A, Go only)
doesn't cover.

It's a **separate Go module** on purpose. tree-sitter pulls in CGO and the C
grammars; ctxzip's core stays dependency-light (bbolt only), and only hosts that
want multi-language code compression take on this dependency.

## Use

Wire it into the core crusher via the `Fallback` seam — Go goes through the
stdlib AST path, everything else through tree-sitter:

```go
import (
    "github.com/initializ/ctxzip/crush"
    "github.com/initializ/ctxzip/codelang"
)

code := crush.NewCodeCrusher()        // Go via go/ast (no CGO)
code.Fallback = codelang.NewCrusher() // Python/TS/JS/Java/C/C++ via tree-sitter
// ...use `code` as the SourceCode compressor in your router.
```

`codelang.Crusher` implements `crush.Compressor`, so it can also be used stand-alone.

## How it works

1. **Detect** the language from content signals; candidates are tried in
   likelihood order (specific dialects — TS over JS, C++ over C — first).
2. **Parse** with the candidate grammar and **verify**: a parse whose
   ERROR/MISSING nodes cover more than 15% of the source is rejected and the
   next candidate tried. A wrong-but-clean parse still elides real body nodes, so
   compression stays valid.
3. **Elide** each function/method body worth eliding (`> MinBodyLines`),
   preserving imports, type declarations, class headers, and every signature.
   Braces are kept; only the bytes between them are offloaded (Python's
   brace-less block is offloaded whole after the `:`).

Same invariants as every ctxzip crusher:

- **Reversible & byte-exact** — each body's interior is offloaded and replaced in
  place by a `<<ctxzip:HASH>>` marker, so expanding every marker reconstructs the
  source byte-for-byte (tested per language).
- **Deterministic**, **fail-open** (nil store / unsupported language / parse
  trouble → extractive text fallback, so nothing is lost), never-empty.
- The generic **error-vocabulary floor is not applied to code** (bodies are full
  of `error`/`Errorf`); `MustKeep` and query terms still protect a body.

## Develop

```bash
go test ./...   # requires CGO + a C toolchain (builds the tree-sitter grammars)
```

The `replace github.com/initializ/ctxzip => ../` in `go.mod` builds this module
against the working tree; it's dropped once the core module is tagged.

## License

Apache-2.0.
