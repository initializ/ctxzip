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

// Item-2 of the roadmap: the sentinel summary must categorize what was
// offloaded so the model can answer count questions — and decide against
// expanding — without retrieval.
func TestJSONCrusher_CategoricalSummary(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewJSONCrusher()

	items := make([]map[string]any, 40)
	for i := range items {
		status := "Running"
		if i%10 == 0 {
			status = "Succeeded"
		}
		items[i] = map[string]any{"id": fmt.Sprintf("r-%03d", i), "status": status}
	}
	blob, _ := json.Marshal(items)

	res, err := c.Compress(Request{Content: string(blob), Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Compressed, "status:") || !strings.Contains(res.Compressed, "Running") {
		t.Fatalf("summary lacks category counts:\n%s", res.Compressed)
	}
	// Deterministic.
	res2, _ := c.Compress(Request{Content: string(blob), Store: ccr.NewMemoryStore(ccr.MemoryConfig{})})
	if res2.Compressed != res.Compressed {
		t.Fatal("categorical summary not deterministic")
	}
}

// Nested probe: kubectl pod objects carry status as an OBJECT; the summary
// must reach status.phase.
func TestJSONCrusher_NestedCategorySummary(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewJSONCrusher()

	items := make([]map[string]any, 30)
	for i := range items {
		items[i] = map[string]any{
			"metadata": map[string]any{"name": fmt.Sprintf("pod-%d", i)},
			"status":   map[string]any{"phase": "Running", "podIP": "10.0.0.1"},
		}
	}
	blob, _ := json.Marshal(items)

	res, _ := c.Compress(Request{Content: string(blob), Store: store})
	if !strings.Contains(res.Compressed, "status.phase:") {
		t.Fatalf("nested category not surfaced:\n%s", res.Compressed)
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
