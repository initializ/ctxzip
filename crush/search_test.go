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

// triple is the separator-agnostic identity of a match, so a context line
// stored as "f.go-3-x" and shown as "  3:x" compare equal.
func triple(path string, line int, content string) string {
	return fmt.Sprintf("%s\x00%d\x00%s", path, line, content)
}

// reconstructMatches rebuilds the set of match triples implied by a compressed
// rendering: kept lines under their file header, plus every line in an offloaded
// blob. It is the content-completeness oracle.
func reconstructMatches(t *testing.T, compressed string, store ccr.Store) map[string]bool {
	t.Helper()
	set := map[string]bool{}
	cur := ""
	add := func(line string) {
		if p, n, c, ok := parseMatchLine(line); ok {
			set[triple(p, n, c)] = true
		}
	}
	for _, ln := range strings.Split(compressed, "\n") {
		switch {
		case strings.Contains(ln, ccr.MarkerPrefix):
			for _, h := range ccr.ExtractHashes(ln) {
				e, ok := store.Get(h)
				if !ok {
					t.Fatalf("marker %s not retrievable", h)
				}
				for _, bl := range strings.Split(string(e.Original), "\n") {
					add(bl)
				}
			}
		case strings.HasPrefix(ln, "  "):
			add(cur + ":" + ln[2:]) // "  line:content" -> path:line:content
		case strings.HasSuffix(ln, ":"):
			cur = strings.TrimSuffix(ln, ":")
		}
	}
	return set
}

func inputSet(lines []string) map[string]bool {
	set := make(map[string]bool, len(lines))
	for _, l := range lines {
		if p, n, c, ok := parseMatchLine(l); ok {
			set[triple(p, n, c)] = true
		}
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

// --- multi-tier parsing (grep -C context, Windows, ambiguous paths) ---

func TestParseMatchLine_Tiers(t *testing.T) {
	cases := []struct {
		in      string
		path    string
		line    int
		content string
	}{
		{"src/app.go:42:return err", "src/app.go", 42, "return err"},             // colon
		{"src/app.go-43-next line", "src/app.go", 43, "next line"},               // dash context
		{`C:\Users\x\main.go:12:content`, `C:\Users\x\main.go`, 12, "content"},   // Windows drive
		{"logs/2026-05-03/app.log-12-msg", "logs/2026-05-03/app.log", 12, "msg"}, // date in path
		{"a.py:7:x:y:z", "a.py", 7, "x:y:z"},                                     // colons in content
	}
	for _, tc := range cases {
		p, n, c, ok := parseMatchLine(tc.in)
		if !ok || p != tc.path || n != tc.line || c != tc.content {
			t.Errorf("parseMatchLine(%q) = (%q,%d,%q,%v), want (%q,%d,%q,true)",
				tc.in, p, n, c, ok, tc.path, tc.line, tc.content)
		}
	}
	// Not matches.
	for _, bad := range []string{"just prose here", "-- ", ":5:no path", "src/app.go:-1:neg"} {
		if _, _, _, ok := parseMatchLine(bad); ok {
			t.Errorf("parseMatchLine(%q) parsed but should not", bad)
		}
	}
}

// TestSearchCrusher_GrepContextLines compresses grep -C output (colon matches +
// dash context + "--" separators) and round-trips it content-complete, with the
// original separators preserved byte-for-byte in the offloaded blobs.
func TestSearchCrusher_GrepContextLines(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewSearchCrusher()
	var lines []string // the parseable match/context lines
	var withNoise []string
	for i := 0; i < 40; i++ {
		var ln string
		if i%5 == 0 {
			ln = fmt.Sprintf("svc/handler.go:%d:matched call %d", i+1, i) // match
		} else {
			ln = fmt.Sprintf("svc/handler.go-%d-context around %d", i+1, i) // context
		}
		lines = append(lines, ln)
		withNoise = append(withNoise, ln)
		if i%5 == 4 {
			withNoise = append(withNoise, "--") // grep hunk separator
		}
	}
	in := strings.Join(withNoise, "\n")

	res, err := c.Compress(Request{Content: in, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if res.Compressed == in {
		t.Fatal("expected grep -C output to compress")
	}
	// Content-complete across colon + dash lines.
	got := reconstructMatches(t, res.Compressed, store)
	for want := range inputSet(lines) {
		if !got[want] {
			t.Fatalf("context/match line lost: %v", want)
		}
	}
	// Offloaded blobs preserve the original dash separators byte-for-byte.
	sawDash := false
	for _, h := range res.Markers {
		e, _ := store.Get(h)
		if strings.Contains(string(e.Original), "-context around ") {
			sawDash = true
		}
	}
	if !sawDash {
		t.Fatal("offloaded blob did not preserve original dash-form context lines")
	}
}

// TestSearchCrusher_KeptContextLinePreservesSeparator guards byte-fidelity of
// *kept* grep -C context lines: a dash-separated context line that survives as a
// file anchor must be shown verbatim (dash preserved), not recolored to a colon
// — because a kept line, unlike an offloaded one, is recoverable nowhere else.
func TestSearchCrusher_KeptContextLinePreservesSeparator(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewSearchCrusher()
	lines := []string{"svc/h.go-1-context BEFORE first match"} // first anchor: dash context
	for i := 2; i <= 11; i++ {
		lines = append(lines, fmt.Sprintf("svc/h.go:%d:match number %d", i, i))
	}
	lines = append(lines, "svc/h.go-12-context AFTER last match") // last anchor: dash context
	in := strings.Join(lines, "\n")

	res, _ := c.Compress(Request{Content: in, Store: store})
	if res.Compressed == in {
		t.Fatal("expected compression")
	}
	// Kept dash anchors shown verbatim (dash), never recolored to colon.
	if !strings.Contains(res.Compressed, "  1-context BEFORE first match") {
		t.Fatalf("first dash-context anchor not shown verbatim:\n%s", res.Compressed)
	}
	if !strings.Contains(res.Compressed, "  12-context AFTER last match") {
		t.Fatalf("last dash-context anchor not shown verbatim:\n%s", res.Compressed)
	}
	if strings.Contains(res.Compressed, "  1:context BEFORE") || strings.Contains(res.Compressed, "  12:context AFTER") {
		t.Fatal("dash-context anchor was recolored to a colon separator")
	}
	// And still content-complete.
	got := reconstructMatches(t, res.Compressed, store)
	for want := range inputSet(lines) {
		if !got[want] {
			t.Fatalf("match lost: %v", want)
		}
	}
}

// TestSearchCrusher_AdaptiveCapShrinksOnRedundancy checks the adaptive global
// cap keeps fewer matches when results are near-duplicates than when diverse.
func TestSearchCrusher_AdaptiveCapShrinksOnRedundancy(t *testing.T) {
	visibleMatches := func(compressed string) int {
		n := 0
		for _, ln := range strings.Split(compressed, "\n") {
			if strings.HasPrefix(ln, "  ") && !strings.Contains(ln, ccr.MarkerPrefix) {
				n++
			}
		}
		return n
	}
	// 8 files, 6 matches each. Redundant: identical shape. Diverse: text that
	// varies by WORDS (not digits — the signature is digit-insensitive by
	// design, so number-only variation still reads as redundant).
	words := strings.Fields("alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike november oscar papa quebec romeo sierra tango uniform victor whiskey xray yankee zulu")
	var redundant, diverse []string
	for f := 0; f < 8; f++ {
		p := fmt.Sprintf("pkg/f%d.go", f)
		for i := 0; i < 6; i++ {
			redundant = append(redundant, fmt.Sprintf("%s:%d:the same constant line", p, i+1))
			w := words[(f*6+i)%len(words)]
			diverse = append(diverse, fmt.Sprintf("%s:%d:distinct %s %s clause", p, i+1, w, words[(f+i)%len(words)]))
		}
	}
	rRes, _ := NewSearchCrusher().Compress(Request{Content: strings.Join(redundant, "\n"), Store: ccr.NewMemoryStore(ccr.MemoryConfig{})})
	dRes, _ := NewSearchCrusher().Compress(Request{Content: strings.Join(diverse, "\n"), Store: ccr.NewMemoryStore(ccr.MemoryConfig{})})

	rKept, dKept := visibleMatches(rRes.Compressed), visibleMatches(dRes.Compressed)
	if rKept >= dKept {
		t.Fatalf("adaptive cap did not shrink on redundancy: redundant kept %d, diverse kept %d", rKept, dKept)
	}
}

func TestAdaptiveKeepCount(t *testing.T) {
	small := []string{"a", "b", "c"}
	if got := adaptiveKeepCount(small, 5, 30); got != 3 {
		t.Errorf("n<=8 should keep all: got %d", got)
	}
	// All identical -> near-total redundancy -> floor to minK.
	redundant := make([]string, 40)
	for i := range redundant {
		redundant[i] = "identical line 5"
	}
	if got := adaptiveKeepCount(redundant, 5, 30); got != 5 {
		t.Errorf("fully redundant should collapse to minK=5, got %d", got)
	}
	// All distinct (by words, since the signature ignores digits) -> ceiling.
	distinct := make([]string, 40)
	for i := range distinct {
		distinct[i] = fmt.Sprintf("unique word %c%c here", 'a'+i/26, 'a'+i%26)
	}
	if got := adaptiveKeepCount(distinct, 5, 30); got != 30 {
		t.Errorf("fully diverse should hit ceiling 30, got %d", got)
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
