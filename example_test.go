package ctxzip_test

import (
	"encoding/json"
	"fmt"

	"github.com/initializ/ctxzip"
	"github.com/initializ/ctxzip/ccr"
)

// Example shows the round trip: compress a bulky tool output, then retrieve the
// offloaded originals by the marker hash.
func Example() {
	// A chatty tool result: 50 near-identical JSON records.
	records := make([]map[string]string, 50)
	for i := range records {
		records[i] = map[string]string{"id": fmt.Sprintf("svc-%02d", i), "state": "healthy"}
	}
	blob, _ := json.Marshal(records)

	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	opts := ctxzip.DefaultOptions()
	opts.Store = store

	msgs := []ctxzip.Message{
		{Role: ctxzip.RoleSystem, Content: "You monitor services."},
		{Role: ctxzip.RoleUser, Content: "list services"},
		{Role: ctxzip.RoleTool, Name: "list_services", Content: string(blob)},
		{Role: ctxzip.RoleAssistant, Content: "All healthy."},
		{Role: ctxzip.RoleUser, Content: "ok"},
	}

	res, _ := ctxzip.Compress(msgs, opts)
	fmt.Printf("saved ~%d tokens (%.0f%%)\n", res.SavedTokens(), res.Ratio()*100)

	// The model can retrieve the dropped rows on demand via the marker hash.
	hashes := ccr.ExtractHashes(res.Messages[2].Content)
	original, ok := ctxzip.Unzip(store, hashes[0])
	fmt.Printf("retrievable: %v, original bytes: %d\n", ok, len(original))
	// Output is non-deterministic in token count across Go versions, so we only
	// assert the shape here in docs; see ctxzip_test.go for exact assertions.
}
