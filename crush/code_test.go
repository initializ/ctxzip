package crush

import (
	"regexp"
	"strings"
	"testing"

	"github.com/initializ/ctxzip/ccr"
)

// goSource is a small but realistic Go file: imports, a type, a const, a
// body-less signature is not valid at top level so we use a small func, and two
// functions with multi-line bodies worth eliding.
const goSource = `package widget

import (
	"fmt"
	"strings"
)

// Config tunes a Widget.
type Config struct {
	Name string
	Size int
}

const DefaultSize = 42

// Build assembles a widget from cfg and returns its label.
func Build(cfg Config) (string, error) {
	if cfg.Name == "" {
		return "", fmt.Errorf("name required")
	}
	size := cfg.Size
	if size == 0 {
		size = DefaultSize
	}
	label := strings.Repeat(cfg.Name, 1)
	return fmt.Sprintf("%s[%d]", label, size), nil
}

// tiny has a short body that is not worth eliding.
func tiny() int {
	return DefaultSize
}

func (c Config) Describe() string {
	parts := []string{c.Name}
	parts = append(parts, "sized")
	parts = append(parts, strings.Repeat("x", c.Size))
	joined := strings.Join(parts, "-")
	return joined
}
`

var testMarkerRe = regexp.MustCompile(`<<ctxzip:([0-9a-f]{12,64})(?:[ ,][^>]*)?>>`)

// expandCode replaces every marker with its stored original bytes.
func expandCode(t *testing.T, compressed string, store ccr.Store) string {
	t.Helper()
	return testMarkerRe.ReplaceAllStringFunc(compressed, func(m string) string {
		h := testMarkerRe.FindStringSubmatch(m)[1]
		e, ok := store.Get(h)
		if !ok {
			t.Fatalf("marker %s not retrievable", h)
		}
		return string(e.Original)
	})
}

func TestCodeCrusher_ElidesBodiesKeepsShape(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewCodeCrusher()

	res, err := c.Compress(Request{Content: goSource, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if res.Strategy != "code_crusher" {
		t.Fatalf("expected code_crusher, got %s", res.Strategy)
	}
	if res.Compressed == goSource {
		t.Fatal("expected body elision, got passthrough")
	}
	// Structure is kept verbatim.
	for _, want := range []string{
		"package widget",
		`import (`,
		"type Config struct {",
		"const DefaultSize = 42",
		"func Build(cfg Config) (string, error) {",
		"func (c Config) Describe() string {",
	} {
		if !strings.Contains(res.Compressed, want) {
			t.Fatalf("structural line dropped: %q", want)
		}
	}
	// The big bodies' innards are gone from the visible output...
	if strings.Contains(res.Compressed, `fmt.Errorf("name required")`) {
		t.Fatal("Build body not elided")
	}
	// ...but the tiny body is kept whole (<= MinBodyLines).
	if !strings.Contains(res.Compressed, "return DefaultSize") {
		t.Fatal("tiny body should not be elided")
	}
	// Two multi-line bodies elided -> two markers, both retrievable.
	if len(res.Markers) != 2 {
		t.Fatalf("want 2 markers, got %d", len(res.Markers))
	}
	for _, h := range res.Markers {
		if _, ok := store.Get(h); !ok {
			t.Fatalf("body %s not retrievable", h)
		}
	}
}

func TestCodeCrusher_RoundTripByteExact(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	res, _ := NewCodeCrusher().Compress(Request{Content: goSource, Store: store})
	if got := expandCode(t, res.Compressed, store); got != goSource {
		t.Fatalf("round-trip not byte-exact:\n--- got ---\n%s", got)
	}
}

func TestCodeCrusher_MustKeepProtectsBody(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewCodeCrusher()
	res, _ := c.Compress(Request{Content: goSource, Store: store, MustKeep: NormalizeMustKeep([]string{"DefaultSize"})})
	// Build's body references DefaultSize, so it must stay whole.
	if !strings.Contains(res.Compressed, "size = DefaultSize") {
		t.Fatal("MustKeep did not protect the body referencing the term")
	}
}

func TestCodeCrusher_NonGoFallsBackToText(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewCodeCrusher()
	// Repetitive non-Go (Python-ish) source: the text fallback should dedup it.
	var sb strings.Builder
	sb.WriteString("def handler(req):\n")
	for i := 0; i < 60; i++ {
		sb.WriteString("    log.info('processing request')\n")
	}
	res, _ := c.Compress(Request{Content: sb.String(), Store: store})
	if res.Strategy == "code_crusher" {
		t.Fatal("non-Go should not use the Go AST path")
	}
	if res.Compressed == sb.String() {
		t.Fatal("expected the text fallback to compress repetitive non-Go source")
	}
}

func TestCodeCrusher_SnippetNoPackageFallsBack(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewCodeCrusher()
	// A bare function snippet (no package clause) is not a parseable Go file.
	snippet := "func orphan() {\n" + strings.Repeat("\tx := compute()\n", 40) + "}\n"
	res, _ := c.Compress(Request{Content: snippet, Store: store})
	if res.Strategy == "code_crusher" {
		t.Fatal("a package-less snippet should fall back, not use the AST path")
	}
}

func TestCodeCrusher_Deterministic(t *testing.T) {
	r1, _ := NewCodeCrusher().Compress(Request{Content: goSource, Store: ccr.NewMemoryStore(ccr.MemoryConfig{})})
	r2, _ := NewCodeCrusher().Compress(Request{Content: goSource, Store: ccr.NewMemoryStore(ccr.MemoryConfig{})})
	if r1.Compressed != r2.Compressed || strings.Join(r1.Markers, ",") != strings.Join(r2.Markers, ",") {
		t.Fatal("compression is not deterministic")
	}
}

func TestCodeCrusher_SmallFile_Passthrough(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewCodeCrusher()
	in := "package p\n\nfunc f() int { return 1 }\n"
	res, _ := c.Compress(Request{Content: in, Store: store})
	if res.Compressed != in {
		t.Fatal("small file should pass through")
	}
}

func TestCodeCrusher_NilStore_Passthrough(t *testing.T) {
	c := NewCodeCrusher()
	res, _ := c.Compress(Request{Content: goSource, Store: nil})
	if res.Compressed != goSource {
		t.Fatal("nil store must force lossless passthrough")
	}
}

func TestCodeCrusher_NeverEmptyOutput(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	res, _ := NewCodeCrusher().Compress(Request{Content: goSource, Store: store})
	if strings.TrimSpace(res.Compressed) == "" {
		t.Fatal("non-empty input compressed to empty output")
	}
}
