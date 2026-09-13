package crush

import (
	"fmt"
	"strings"
	"testing"

	"github.com/initializ/ctxzip/ccr"
)

func grepLine(path string, line int, content string) string {
	return fmt.Sprintf("%s:%d:%s", path, line, content)
}

// reconstructMatches rebuilds the set of "path:line:content" strings implied by
// a compressed rendering: kept lines under their file header, plus every line
// in an offloaded blob. It is the content-completeness oracle.
func reconstructMatches(t *testing.T, compressed string, store ccr.Store) map[string]bool {
	t.Helper()
	set := map[string]bool{}
	cur := ""
	for _, ln := range strings.Split(compressed, "\n") {
		switch {
		case strings.Contains(ln, ccr.MarkerPrefix):
			for _, h := range ccr.ExtractHashes(ln) {
				e, ok := store.Get(h)
				if !ok {
					t.Fatalf("marker %s not retrievable", h)
				}
				for _, bl := range strings.Split(string(e.Original), "\n") {
					set[bl] = true
				}
			}
		case strings.HasPrefix(ln, "  "):
			set[cur+":"+ln[2:]] = true // "  line:content" -> path:line:content
		case strings.HasSuffix(ln, ":"):
			cur = strings.TrimSuffix(ln, ":")
		}
	}
	return set
}

func inputSet(lines []string) map[string]bool {
	set := make(map[string]bool, len(lines))
	for _, l := range lines {
		set[l] = true
	}
	return set
}

// oneFileGrep builds `n` matches in one file, with an error match at index 25.
func oneFileGrep(path string, n int) []string {
	lines := make([]string, n)
	for i := range lines {
		if i == 25 {
			lines[i] = grepLine(path, i+1, "err := handler(); // ERROR path")
		} else {
			lines[i] = grepLine(path, i+1, fmt.Sprintf("routine call number %d here", i))
		}
	}
	return lines
}

func TestSearchCrusher_GroupsAndIsContentComplete(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewSearchCrusher()
	lines := oneFileGrep("internal/server.go", 60)
	in := strings.Join(lines, "\n")

	res, err := c.Compress(Request{Content: in, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if res.Compressed == in {
		t.Fatal("expected compression, got passthrough")
	}
	// Path prefix appears once as a header, not on every kept line.
	if strings.Count(res.Compressed, "internal/server.go:") != 1 {
		t.Fatalf("path not grouped under a single header:\n%s", res.Compressed)
	}
	// Content-complete: every input match is shown or retrievable.
	got := reconstructMatches(t, res.Compressed, store)
	for want := range inputSet(lines) {
		if !got[want] {
			t.Fatalf("match lost (not shown or offloaded): %q", want)
		}
	}
}

func TestSearchCrusher_KeepsErrorMatch(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewSearchCrusher()
	in := strings.Join(oneFileGrep("app/main.go", 60), "\n")

	res, _ := c.Compress(Request{Content: in, Store: store})
	if !strings.Contains(res.Compressed, "// ERROR path") {
		t.Fatalf("error match dropped by the cap:\n%s", res.Compressed)
	}
}

func TestSearchCrusher_KeepsFirstAndLastAnchors(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewSearchCrusher()
	lines := oneFileGrep("pkg/x.go", 40)
	res, _ := c.Compress(Request{Content: strings.Join(lines, "\n"), Store: store})

	// First match is line 1, last is line 40 — both must be visible (not offloaded).
	if !strings.Contains(res.Compressed, "  1:routine call number 0 here") {
		t.Fatal("first-match anchor dropped")
	}
	if !strings.Contains(res.Compressed, "  40:routine call number 39 here") {
		t.Fatal("last-match anchor dropped")
	}
}

func TestSearchCrusher_QueryRelevantMatchKept(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewSearchCrusher()
	lines := make([]string, 0, 51)
	for i := 0; i < 50; i++ {
		lines = append(lines, grepLine("q.go", i+1, "generic filler line without signal"))
	}
	lines = append(lines, grepLine("q.go", 51, "the special deadlock keyword appears here"))
	in := strings.Join(lines, "\n")

	res, _ := c.Compress(Request{Content: in, Query: "special deadlock keyword", Store: store})
	if !strings.Contains(res.Compressed, "special deadlock keyword") {
		t.Fatalf("query-relevant match was offloaded instead of kept:\n%s", res.Compressed)
	}
}

func TestSearchCrusher_FileCapDropsLowScoreFiles(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewSearchCrusher()
	var lines []string
	// 20 files (> MaxFiles=15), 2 matches each; file 19 carries an error.
	for f := 0; f < 20; f++ {
		path := fmt.Sprintf("pkg/f%02d.go", f)
		if f == 19 {
			lines = append(lines, grepLine(path, 1, "panic: fatal error here"), grepLine(path, 2, "trace"))
		} else {
			lines = append(lines, grepLine(path, 1, "ordinary line one"), grepLine(path, 2, "ordinary line two"))
		}
	}
	in := strings.Join(lines, "\n")
	res, _ := c.Compress(Request{Content: in, Store: store})

	if !strings.Contains(res.Compressed, "_files_offloaded") {
		t.Fatalf("expected a dropped-files marker:\n%s", res.Compressed)
	}
	// The error-bearing file is protected and kept.
	if !strings.Contains(res.Compressed, "pkg/f19.go:") || !strings.Contains(res.Compressed, "panic: fatal error here") {
		t.Fatal("protected (error) file dropped by the file cap")
	}
	// Still content-complete across the file cap.
	got := reconstructMatches(t, res.Compressed, store)
	for want := range inputSet(lines) {
		if !got[want] {
			t.Fatalf("match lost across file cap: %q", want)
		}
	}
}

func TestSearchCrusher_Deterministic(t *testing.T) {
	in := strings.Join(oneFileGrep("d.go", 60), "\n")
	r1, _ := NewSearchCrusher().Compress(Request{Content: in, Store: ccr.NewMemoryStore(ccr.MemoryConfig{})})
	r2, _ := NewSearchCrusher().Compress(Request{Content: in, Store: ccr.NewMemoryStore(ccr.MemoryConfig{})})
	if r1.Compressed != r2.Compressed {
		t.Fatal("compression is not deterministic")
	}
}

func TestSearchCrusher_MixedContent_Passthrough(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewSearchCrusher()
	// A grep -C run with a non-match context line must pass through untouched,
	// so nothing is silently dropped.
	in := strings.Join(oneFileGrep("m.go", 30), "\n") + "\n-- context separator not a match --"
	res, _ := c.Compress(Request{Content: in, Store: store})
	if res.Compressed != in {
		t.Fatal("mixed content should pass through, not be partially crushed")
	}
}

func TestSearchCrusher_FewMatches_Passthrough(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewSearchCrusher()
	in := strings.Join(oneFileGrep("s.go", 4), "\n") // < MinMatches
	res, _ := c.Compress(Request{Content: in, Store: store})
	if res.Compressed != in {
		t.Fatal("below MinMatches should pass through")
	}
}

func TestSearchCrusher_NilStore_Passthrough(t *testing.T) {
	c := NewSearchCrusher()
	in := strings.Join(oneFileGrep("n.go", 60), "\n")
	res, _ := c.Compress(Request{Content: in, Store: nil})
	if res.Compressed != in {
		t.Fatal("nil store must force lossless passthrough")
	}
}

func TestSearchCrusher_NeverEmptyOutput(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewSearchCrusher()
	res, _ := c.Compress(Request{Content: strings.Join(oneFileGrep("e.go", 200), "\n"), Store: store})
	if strings.TrimSpace(res.Compressed) == "" {
		t.Fatal("non-empty input compressed to empty output")
	}
}
