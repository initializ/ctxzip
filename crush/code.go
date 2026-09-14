package crush

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"

	"github.com/initializ/ctxzip/ccr"
)

// CodeCrusher compresses source code by eliding function bodies while keeping
// the file's shape — package clause, imports, type/const/var declarations, and
// every function signature. The body is the bulk; the signatures and structure
// are what a model usually needs to reason about a file.
//
// Tier A (this crusher) handles Go natively via the standard library's
// go/parser + go/ast — no third-party dependency, and AST-accurate brace
// matching so string/comment braces never fool it. Other languages fall back to
// the extractive text crusher (unchanged from how SourceCode routed before), so
// nothing regresses; a build-tagged tree-sitter path for them is a follow-up.
//
// Reversibility is position-preserving and byte-exact: each elided body's
// interior (the bytes between its braces) is offloaded and replaced in place by
// a marker, so expanding every marker reconstructs the file byte-for-byte. The
// compressed form is for READING, not compiling.
//
// The generic error-vocabulary floor is deliberately NOT applied here. It exists
// to stop a log/tool-output crusher from dropping the error line a user is about
// to ask about; source code is different — Go bodies are saturated with "error"
// / "Errorf", so the floor would pin almost every function and defeat
// compression. Bodies are always reversible, so eliding one that mentions an
// error is not the catastrophic miss the floor guards against. Caller MustKeep
// vocabulary and query terms still protect a body from elision.
type CodeCrusher struct {
	// MinBodyLines is the body line-span below which a function is left whole
	// (a marker isn't worth it for a tiny body).
	MinBodyLines int
	// MinLines is the file size (in newlines) below which it is left alone.
	MinLines int
	// Fallback compresses source the Go AST path does not handle (non-Go, a
	// package-less snippet, or unparseable input). When nil, an extractive text
	// crusher is used — the same strategy SourceCode used before this crusher,
	// so no language regresses. A tree-sitter-backed crusher (the ctxzip/codelang
	// module) can be injected here to give Python/TS/JS/Java/C/C++ the same
	// signature-keeping, body-eliding treatment Go gets. Must implement the same
	// invariants (fail-open, reversible, deterministic).
	Fallback Compressor
	// text is the default fallback.
	text *TextCrusher
}

// NewCodeCrusher returns a CodeCrusher with sensible defaults.
func NewCodeCrusher() *CodeCrusher {
	return &CodeCrusher{MinBodyLines: 3, MinLines: 15, text: NewTextCrusher()}
}

// fallback returns the injected Fallback, or the default text crusher.
func (c *CodeCrusher) fallback() Compressor {
	if c.Fallback != nil {
		return c.Fallback
	}
	return c.text
}

// Name implements Compressor.
func (c *CodeCrusher) Name() string { return "code_crusher" }

// Compress implements Compressor.
func (c *CodeCrusher) Compress(req Request) (Result, error) {
	if req.Store == nil || strings.Count(req.Content, "\n") < c.MinLines {
		return passthrough(c.Name(), req.Content), nil
	}
	if res, ok := c.compressGo(req); ok {
		return res, nil
	}
	// Non-Go, a bare snippet (no package clause), or unparseable: hand off to the
	// fallback (a tree-sitter crusher if injected, else extractive text).
	return c.fallback().Compress(req)
}

// compressGo elides the bodies of top-level Go functions and methods. ok is
// false when the content is not a parseable Go file or nothing is worth eliding,
// so the caller can fall back.
func (c *CodeCrusher) compressGo(req Request) (Result, bool) {
	src := req.Content
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if err != nil {
		return Result{}, false
	}

	terms := queryTerms(req.Query)
	// interior byte range [start,end) of each body to elide, in source order.
	type span struct {
		start, end, lines int
	}
	var spans []span
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue // imports/types/consts, or a body-less signature
		}
		lb := fset.Position(fn.Body.Lbrace)
		rb := fset.Position(fn.Body.Rbrace)
		lines := rb.Line - lb.Line
		if lines <= c.MinBodyLines {
			continue
		}
		start, end := lb.Offset+1, rb.Offset // exclusive of the braces
		if start >= end {
			continue
		}
		// Caller/query intent keeps a body whole (see the type doc on why the
		// generic error floor is not applied to code).
		body := strings.ToLower(src[start:end])
		if mustKeep(body, req.MustKeep) || (len(terms) > 0 && matchesAny(body, terms)) {
			continue
		}
		spans = append(spans, span{start, end, lines})
	}
	if len(spans) == 0 {
		return Result{}, false
	}

	var b strings.Builder
	var markers []string
	prev := 0
	for _, sp := range spans {
		b.WriteString(src[prev:sp.start]) // through the opening brace
		interior := src[sp.start:sp.end]
		hash := ccr.Hash([]byte(interior))
		if err := req.Store.Put(hash, []byte(interior), ccr.Meta{
			ToolName:     req.ToolName,
			Query:        req.Query,
			ItemCount:    sp.lines,
			OriginalKind: "code",
		}); err != nil {
			b.WriteString(interior) // fail-open: keep this body verbatim
			prev = sp.end
			continue
		}
		markers = append(markers, hash)
		b.WriteString(ccr.Marker(hash, fmt.Sprintf("%d_lines_offloaded", sp.lines)))
		prev = sp.end
	}
	b.WriteString(src[prev:])

	if len(markers) == 0 {
		return Result{}, false
	}
	return Result{Compressed: b.String(), Strategy: c.Name(), Markers: markers}, true
}
