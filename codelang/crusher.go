// Package codelang is the tree-sitter-backed Tier B code crusher for ctxzip:
// signature-keeping, body-eliding compression for Python, TypeScript,
// JavaScript, Java, C, and C++ — the languages ctxzip's stdlib go/ast crusher
// (Tier A, Go only) does not cover.
//
// It lives in its own Go module so ctxzip's core stays dependency-light (bbolt
// only): tree-sitter pulls in CGO and the C grammars, which only hosts that want
// multi-language code compression need. Wire it into the core crusher via
// crush.CodeCrusher.Fallback:
//
//	code := crush.NewCodeCrusher()      // Go via go/ast
//	code.Fallback = codelang.NewCrusher() // Python/TS/JS/Java/C/C++ via tree-sitter
//
// It implements crush.Compressor and honors the same invariants: reversible
// (each elided body's interior is offloaded and replaced in place by a marker,
// so expanding every marker reconstructs the source byte-for-byte),
// deterministic, and fail-open (any parse trouble or unsupported language falls
// back to the extractive text crusher, so nothing is ever lost).
package codelang

import (
	"context"
	"fmt"
	"sort"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"

	"github.com/initializ/ctxzip/ccr"
	"github.com/initializ/ctxzip/crush"
)

// Crusher compresses source code by eliding function bodies. It handles the six
// tree-sitter languages and delegates everything else to an extractive text
// crusher.
type Crusher struct {
	// MinBodyLines is the body line-span below which a function is left whole.
	MinBodyLines int
	// MinLines is the file size (in newlines) below which it is left alone.
	MinLines int
	// maxErrorRatio rejects a parse whose ERROR/MISSING nodes cover more than
	// this fraction of the source — a sign the grammar guess was wrong.
	maxErrorRatio float64
	text          *crush.TextCrusher
}

// NewCrusher returns a Crusher with sensible defaults.
func NewCrusher() *Crusher {
	return &Crusher{MinBodyLines: 3, MinLines: 15, maxErrorRatio: 0.15, text: crush.NewTextCrusher()}
}

// Name implements crush.Compressor.
func (c *Crusher) Name() string { return "code_treesitter" }

// Compress implements crush.Compressor.
func (c *Crusher) Compress(req crush.Request) (crush.Result, error) {
	if req.Store == nil || strings.Count(req.Content, "\n") < c.MinLines {
		return crush.Result{Compressed: req.Content, Strategy: c.Name()}, nil
	}
	// Try each candidate grammar in likelihood order; the first that parses
	// cleanly and finds bodies to elide wins. A wrong-but-clean parse still
	// elides real body nodes, so compression stays valid and reversible.
	for i, lang := range detectCandidates(req.Content) {
		if i >= 3 {
			break // bound the work
		}
		if res, ok := c.compress(req, lang); ok {
			return res, nil
		}
	}
	// Unsupported language or nothing to elide: extractive text, so no regression.
	return c.text.Compress(req)
}

// span is the interior byte range [start,end) of one body to elide.
type span struct {
	start, end, lines int
}

func (c *Crusher) compress(req crush.Request, lang language) (crush.Result, bool) {
	src := []byte(req.Content)
	parser := sitter.NewParser()
	parser.SetLanguage(lang.grammar())
	tree, err := parser.ParseCtx(context.Background(), nil, src)
	if err != nil || tree == nil {
		return crush.Result{}, false
	}
	defer tree.Close()
	root := tree.RootNode()
	if errorByteRatio(root, len(src)) > c.maxErrorRatio {
		return crush.Result{}, false // wrong grammar for this content
	}

	mustKeep := crush.NormalizeMustKeep(req.MustKeep)
	terms := queryWords(req.Query)
	var spans []span
	collectBodies(root, lang, src, mustKeep, terms, c.MinBodyLines, &spans)
	if len(spans) == 0 {
		return crush.Result{}, false
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })

	var b strings.Builder
	var markers []string
	prev := 0
	for _, sp := range spans {
		b.Write(src[prev:sp.start])
		interior := src[sp.start:sp.end]
		hash := ccr.Hash(interior)
		if err := req.Store.Put(hash, interior, ccr.Meta{
			ToolName:     req.ToolName,
			Query:        req.Query,
			ItemCount:    sp.lines,
			OriginalKind: "code",
		}); err != nil {
			b.Write(interior) // fail-open: keep this body verbatim
			prev = sp.end
			continue
		}
		markers = append(markers, hash)
		b.WriteString(ccr.Marker(hash, fmt.Sprintf("%d_lines_offloaded", sp.lines)))
		prev = sp.end
	}
	b.Write(src[prev:])
	if len(markers) == 0 {
		return crush.Result{}, false
	}
	return crush.Result{Compressed: b.String(), Strategy: c.Name() + ":" + lang.name, Markers: markers}, true
}

// collectBodies walks the tree recording the interior span of each function body
// worth eliding. It does not descend into an elided body (its interior is
// offloaded whole), so recorded spans never overlap.
func collectBodies(n *sitter.Node, lang language, src []byte, mustKeep, terms []string, minLines int, out *[]span) {
	for i := 0; i < int(n.NamedChildCount()); i++ {
		child := n.NamedChild(i)
		if lang.funcTypes[child.Type()] {
			if body := child.ChildByFieldName("body"); body != nil && lang.bodyTypes[body.Type()] {
				if sp, ok := bodySpan(body, lang, src, mustKeep, terms, minLines); ok {
					*out = append(*out, sp)
					continue // do not descend into an elided function
				}
			}
		}
		collectBodies(child, lang, src, mustKeep, terms, minLines, out)
	}
}

// bodySpan returns the elidable interior of a body node, or ok=false if it is
// too small or protected by MustKeep / a query term.
func bodySpan(body *sitter.Node, lang language, src []byte, mustKeep, terms []string, minLines int) (span, bool) {
	lines := int(body.EndPoint().Row) - int(body.StartPoint().Row)
	if lines <= minLines {
		return span{}, false
	}
	start, end := int(body.StartByte()), int(body.EndByte())
	if lang.usesBraces {
		// Keep the braces; elide only what's between them.
		if end <= start+1 || src[start] != '{' || src[end-1] != '}' {
			return span{}, false
		}
		start, end = start+1, end-1
	}
	if start >= end {
		return span{}, false
	}
	// Caller/query intent keeps a body whole. The generic error-vocabulary floor
	// is intentionally not applied to code (bodies are full of "error"/"Errorf");
	// bodies are reversible anyway. Mirrors the Go crusher.
	lower := strings.ToLower(string(src[start:end]))
	if containsAny(lower, mustKeep) || (len(terms) > 0 && containsAny(lower, terms)) {
		return span{}, false
	}
	return span{start, end, lines}, true
}

// errorByteRatio is the fraction of source bytes covered by ERROR/MISSING nodes.
func errorByteRatio(root *sitter.Node, total int) float64 {
	if total == 0 {
		return 1
	}
	var errBytes int
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n.IsError() || n.IsMissing() {
			errBytes += int(n.EndByte() - n.StartByte())
			return // don't double-count the subtree
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
	return float64(errBytes) / float64(total)
}

// queryWords splits a query into lowercase words of 3+ chars (symbol-ish tokens).
func queryWords(q string) []string {
	if q == "" {
		return nil
	}
	fields := strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_')
	})
	out := fields[:0]
	for _, f := range fields {
		if len(f) >= 3 {
			out = append(out, f)
		}
	}
	return out
}

func containsAny(s string, terms []string) bool {
	for _, t := range terms {
		if t != "" && strings.Contains(s, t) {
			return true
		}
	}
	return false
}
