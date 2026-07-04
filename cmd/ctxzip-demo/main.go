// Command ctxzip-demo compresses a blob of content the way an agent runtime
// would compress a tool result, then proves the drop is reversible by
// retrieving the originals back out of the durable store.
//
// Usage:
//
//	go run ./cmd/ctxzip-demo [-q "user query"] [-db path] [file]
//
// Reads from the file argument, or stdin if none is given. Examples:
//
//	kubectl get pods -o json | go run ./cmd/ctxzip-demo -q "any crashing pods?"
//	go run ./cmd/ctxzip-demo -q "why did the build fail" build.log
//	cat big.json | go run ./cmd/ctxzip-demo
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/initializ/ctxzip"
	"github.com/initializ/ctxzip/ccr"
	"github.com/initializ/ctxzip/detect"
)

func main() {
	query := flag.String("q", "", "user query used for relevance scoring")
	dbPath := flag.String("db", "ctxzip-demo.db", "durable CCR store path (bbolt)")
	flag.Parse()

	content, err := readInput(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "read input:", err)
		os.Exit(1)
	}

	store, err := ccr.NewBoltStore(ccr.BoltConfig{Path: *dbPath})
	if err != nil {
		fmt.Fprintln(os.Stderr, "open store:", err)
		os.Exit(1)
	}
	defer store.Close()

	// Build a realistic conversation. The tool result sits in the "live zone"
	// (not the frozen prefix, not the protected recent turns) so it is eligible
	// for compression.
	msgs := []ctxzip.Message{
		{Role: ctxzip.RoleSystem, Content: "You are a helpful operations assistant."},
		{Role: ctxzip.RoleUser, Content: orDefault(*query, "look at this output")},
		{Role: ctxzip.RoleTool, Name: "tool_output", Content: content},
		{Role: ctxzip.RoleAssistant, Content: "Working on it."},
		{Role: ctxzip.RoleUser, Content: "ok"},
	}

	opts := ctxzip.DefaultOptions()
	opts.Store = store

	res, err := ctxzip.Compress(msgs, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "compress:", err)
		os.Exit(1)
	}

	det := detect.Detect(content)
	fmt.Printf("detected content type : %s (confidence %.2f)\n", det.Type, det.Confidence)
	fmt.Printf("tokens before         : %d\n", res.TokensBefore)
	fmt.Printf("tokens after          : %d\n", res.TokensAfter)
	fmt.Printf("saved                 : %d tokens (%.1f%%)\n", res.SavedTokens(), res.Ratio()*100)

	if len(res.Transforms) == 0 {
		fmt.Println("\n(no message was compressed — input too small, or losslessly left alone)")
		return
	}

	compressed := res.Messages[2].Content
	fmt.Printf("\n--- compressed tool output (%d bytes, was %d) ---\n%s\n",
		len(compressed), len(content), truncate(compressed, 1200))

	// Reversibility: pull every offloaded original back by its marker hash.
	hashes := ccr.ExtractHashes(compressed)
	fmt.Printf("\n--- reversibility: %d marker(s) ---\n", len(hashes))
	for _, h := range hashes {
		orig, ok := ctxzip.Unzip(store, h)
		fmt.Printf("  %s -> retrieved=%v, original=%d bytes\n", h, ok, len(orig))
	}
	fmt.Printf("\nstore now holds %d entr(ies) at %s — re-run to confirm they persist.\n",
		store.Stats().Entries, *dbPath)
}

func readInput(path string) (string, error) {
	if path == "" {
		b, err := io.ReadAll(os.Stdin)
		return string(b), err
	}
	b, err := os.ReadFile(path)
	return string(b), err
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…(truncated)"
}
