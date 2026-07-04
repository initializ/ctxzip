package crush

import (
	"fmt"
	"strings"
	"testing"

	"github.com/initializ/ctxzip/ccr"
)

func TestLogCrusher_KeepsErrorsAndIsReversible(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewLogCrusher()

	var sb strings.Builder
	for i := 0; i < 100; i++ {
		if i == 50 {
			sb.WriteString("ERROR something exploded\n")
			continue
		}
		sb.WriteString(fmt.Sprintf("INFO request %d handled\n", i))
	}
	in := sb.String()

	res, err := c.Compress(Request{Content: in, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if res.Compressed == in {
		t.Fatal("expected log compression, got passthrough")
	}
	if !strings.Contains(res.Compressed, "ERROR something exploded") {
		t.Fatal("error line dropped")
	}
	if len(res.Markers) != 1 {
		t.Fatalf("want 1 marker, got %d", len(res.Markers))
	}
	if !ccr.HasMarker(res.Compressed) {
		t.Fatal("no marker in output")
	}
	if _, ok := store.Get(res.Markers[0]); !ok {
		t.Fatal("dropped lines not retrievable")
	}
}

func TestLogCrusher_ShortLog_Passthrough(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewLogCrusher()
	in := "line one\nline two\nline three"
	res, _ := c.Compress(Request{Content: in, Store: store})
	if res.Compressed != in {
		t.Fatal("short log should pass through")
	}
}
