package crush

import (
	"fmt"
	"strings"
	"testing"

	"github.com/initializ/ctxzip/ccr"
)

// The bug: every row contains the word "error" (a Go return type), so the old
// looksError substring keep pinned all 400 rows → 0% compression.
func TestJSONCrusher_CodeSignaturesCompress(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < 400; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"file":"src/pkg/module_%d.go","line":%d,"match":"func Handle%d(ctx context.Context) error"}`, i%20, i, i)
	}
	b.WriteString("]")
	in := b.String()
	res, _ := NewJSONCrusher().Compress(Request{Content: in, Store: ccr.NewMemoryStore(ccr.MemoryConfig{})})
	if len(res.Markers) == 0 {
		t.Fatalf("expected compression, got passthrough (%d bytes)", len(res.Compressed))
	}
	if len(res.Compressed) >= len(in) {
		t.Fatalf("did not shrink: %d -> %d", len(in), len(res.Compressed))
	}
	t.Logf("compressed %d -> %d bytes", len(in), len(res.Compressed))
}

// A genuine structural error (FATAL level) must still survive verbatim.
func TestJSONCrusher_RealErrorRowKept(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < 50; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		if i == 30 {
			b.WriteString(`{"level":"FATAL","msg":"db pool exhausted"}`)
			continue
		}
		fmt.Fprintf(&b, `{"level":"INFO","msg":"ok %d"}`, i)
	}
	b.WriteString("]")
	res, _ := NewJSONCrusher().Compress(Request{Content: b.String(), Store: ccr.NewMemoryStore(ccr.MemoryConfig{})})
	if !strings.Contains(res.Compressed, "FATAL") {
		t.Fatalf("FATAL row was dropped: %s", res.Compressed)
	}
	if len(res.Markers) == 0 {
		t.Fatalf("expected the 49 INFO rows to collapse")
	}
}
