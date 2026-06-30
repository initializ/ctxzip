package ctxzip

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/initializ/ctxzip/ccr"
)

// bigJSONArray builds a JSON array of n pod-like records, with one error row.
func bigJSONArray(n int) string {
	items := make([]map[string]any, n)
	for i := range items {
		items[i] = map[string]any{
			"name":   fmt.Sprintf("pod-%03d", i),
			"status": "Running",
			"node":   "node-a",
		}
	}
	items[n/2]["status"] = "CrashLoopBackOff"
	items[n/2]["error"] = "container failed to start"
	b, _ := json.Marshal(items)
	return string(b)
}

func TestCompress_JSONToolOutput_IsReversible(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	msgs := []Message{
		{Role: RoleSystem, Content: "You are a helpful k8s assistant."},
		{Role: RoleUser, Content: "list the pods"},
		{Role: RoleTool, Name: "list_pods", Content: bigJSONArray(60)},
		{Role: RoleAssistant, Content: "Here are the pods."},
		{Role: RoleUser, Content: "thanks"},
	}

	opts := DefaultOptions()
	opts.Store = store
	res, err := Compress(msgs, opts)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}

	if res.SavedTokens() <= 0 {
		t.Fatalf("expected token savings, got before=%d after=%d", res.TokensBefore, res.TokensAfter)
	}
	if len(res.Transforms) != 1 {
		t.Fatalf("expected 1 transform, got %d", len(res.Transforms))
	}

	tool := res.Messages[2].Content
	if !ccr.HasMarker(tool) {
		t.Fatalf("compressed tool output has no ctxzip marker:\n%s", tool)
	}
	// Output must still be valid JSON.
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(tool), &arr); err != nil {
		t.Fatalf("compressed output is not valid JSON: %v\n%s", err, tool)
	}
	// The error row must survive compression verbatim (must-keep floor).
	if !strings.Contains(tool, "CrashLoopBackOff") {
		t.Fatalf("error row was dropped — must-keep floor failed:\n%s", tool)
	}

	// Reversibility: the marker hash retrieves the dropped originals.
	hashes := ccr.ExtractHashes(tool)
	if len(hashes) != 1 {
		t.Fatalf("expected 1 marker hash, got %d", len(hashes))
	}
	orig, ok := Unzip(store, hashes[0])
	if !ok {
		t.Fatalf("Unzip(%s) miss — original not retrievable", hashes[0])
	}
	var dropped []json.RawMessage
	if err := json.Unmarshal(orig, &dropped); err != nil {
		t.Fatalf("retrieved original is not valid JSON: %v", err)
	}
	if len(dropped) == 0 {
		t.Fatalf("retrieved original is empty")
	}
}

func TestCompress_FreezesPrefixAndRecent(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	big := bigJSONArray(60)
	msgs := []Message{
		{Role: RoleTool, Name: "a", Content: big}, // index 0: frozen prefix
		{Role: RoleUser, Content: "go"},
		{Role: RoleTool, Name: "b", Content: big}, // index 2: live zone -> compressed
		{Role: RoleTool, Name: "c", Content: big}, // index 3: protected recent
		{Role: RoleUser, Content: "done"},         // index 4: protected recent
	}
	opts := DefaultOptions() // FreezePrefix=1, ProtectRecent=2
	opts.Store = store
	res, _ := Compress(msgs, opts)

	if res.Messages[0].Content != big {
		t.Errorf("frozen prefix (index 0) was modified")
	}
	if res.Messages[3].Content != big {
		t.Errorf("protected recent (index 3) was modified")
	}
	if res.Messages[2].Content == big {
		t.Errorf("live-zone message (index 2) was NOT compressed")
	}
}

func TestCompress_DoesNotMutateInput(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	original := bigJSONArray(60)
	msgs := []Message{
		{Role: RoleUser, Content: "go"},
		{Role: RoleTool, Name: "a", Content: original},
		{Role: RoleUser, Content: "x"},
		{Role: RoleAssistant, Content: "y"},
	}
	opts := DefaultOptions()
	opts.Store = store
	_, _ = Compress(msgs, opts)

	if msgs[1].Content != original {
		t.Fatalf("Compress mutated the caller's input slice")
	}
}
