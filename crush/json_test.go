package crush

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/initializ/ctxzip/ccr"
)

func jsonArray(n int, withError bool) string {
	items := make([]map[string]any, n)
	for i := range items {
		items[i] = map[string]any{"id": fmt.Sprintf("r-%03d", i), "status": "ok"}
	}
	if withError && n > 0 {
		items[n/2]["status"] = "FAILED"
	}
	b, _ := json.Marshal(items)
	return string(b)
}

func TestJSONCrusher_Compresses_And_IsReversible(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewJSONCrusher()
	in := jsonArray(40, true)

	res, err := c.Compress(Request{Content: in, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if res.Compressed == in {
		t.Fatal("expected compression, got passthrough")
	}
	if len(res.Markers) != 1 {
		t.Fatalf("want 1 marker, got %d", len(res.Markers))
	}
	// Valid JSON out.
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(res.Compressed), &arr); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	// Error row survives.
	if !strings.Contains(res.Compressed, "FAILED") {
		t.Fatal("error row dropped")
	}
	// Reversible.
	if _, ok := store.Get(res.Markers[0]); !ok {
		t.Fatal("dropped originals not retrievable")
	}
}

func TestJSONCrusher_SmallArray_Passthrough(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewJSONCrusher()
	in := jsonArray(3, false)
	res, _ := c.Compress(Request{Content: in, Store: store})
	if res.Compressed != in {
		t.Fatal("small array should pass through unchanged")
	}
}

func TestJSONCrusher_NilStore_Passthrough(t *testing.T) {
	c := NewJSONCrusher()
	in := jsonArray(40, false)
	res, _ := c.Compress(Request{Content: in, Store: nil})
	if res.Compressed != in {
		t.Fatal("nil store must force lossless passthrough")
	}
}

func TestJSONCrusher_NonArray_Passthrough(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewJSONCrusher()
	in := `{"not":"an array"}`
	res, _ := c.Compress(Request{Content: in, Store: store})
	if res.Compressed != in {
		t.Fatal("non-array JSON should pass through")
	}
}

func TestJSONCrusher_QueryKeepsRelevantRow(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewJSONCrusher()
	// r-021 sits in the droppable middle; the query should rescue it.
	res, _ := c.Compress(Request{Content: jsonArray(40, false), Query: "what about r-021", Store: store})
	if !strings.Contains(res.Compressed, "r-021") {
		t.Fatalf("query-relevant row dropped:\n%s", res.Compressed)
	}
}
