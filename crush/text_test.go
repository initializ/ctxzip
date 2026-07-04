package crush

import (
	"fmt"
	"strings"
	"testing"

	"github.com/initializ/ctxzip/ccr"
)

// prose builds a long block of filler sentences with one needle sentence.
func prose(needle string) string {
	var sb strings.Builder
	filler := []string{
		"The weather today is mild and pleasant.",
		"Many people enjoy walking in the park.",
		"Coffee is a popular morning beverage.",
		"The library closes at nine in the evening.",
		"Cats often sleep for most of the day.",
		"Autumn leaves turn shades of red and gold.",
		"The train departs from platform two.",
		"Fresh bread smells wonderful in the morning.",
	}
	for _, f := range filler {
		sb.WriteString(f)
		sb.WriteString(" ")
	}
	sb.WriteString(needle)
	sb.WriteString(" ")
	for _, f := range filler {
		sb.WriteString(f)
		sb.WriteString(" ")
	}
	return sb.String()
}

func TestTextCrusher_QueryMode_KeepsNeedle_Reversible(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewTextCrusher()
	needle := "The database migration failed because the schema version was incompatible."
	in := prose(needle)

	res, err := c.Compress(Request{Content: in, Query: "why did the database migration fail", Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if res.Compressed == in {
		t.Fatal("expected extractive compression, got passthrough")
	}
	if !strings.Contains(res.Compressed, "migration failed") {
		t.Fatalf("query-relevant needle dropped:\n%s", res.Compressed)
	}
	if len(res.Markers) != 1 {
		t.Fatalf("want 1 marker, got %d", len(res.Markers))
	}
	if _, ok := store.Get(res.Markers[0]); !ok {
		t.Fatal("dropped sentences not retrievable")
	}
}

func TestTextCrusher_NoQuery_DropsDuplicates(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewTextCrusher()
	// Same sentence repeated many times -> dedup should shrink it.
	one := "This exact sentence repeats over and over again here. "
	in := strings.Repeat(one, 20)

	res, _ := c.Compress(Request{Content: in, Store: store})
	if res.Compressed == in {
		t.Fatal("expected duplicate suppression to compress")
	}
}

func TestTextCrusher_ShortProse_Passthrough(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewTextCrusher()
	in := "Just one short sentence."
	res, _ := c.Compress(Request{Content: in, Store: store})
	if res.Compressed != in {
		t.Fatal("short prose should pass through")
	}
}

// Regression (live-test find): grep/log-shaped text must split on LINES, not
// dots — splitting "pods.json:445:" mid-token mangled both the compressed view
// and the stored original ("pods. pods. json:445:"). Line layout must survive
// the compress→store→retrieve round trip byte-faithfully.
func TestTextCrusher_LineOriented_KeepsLinesIntact(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewTextCrusher()

	var sb strings.Builder
	for i := 0; i < 120; i++ {
		fmt.Fprintf(&sb, "pods.json:%d:  \"status\": \"Running\",\n", i*9+5)
	}
	sb.WriteString("pods.json:701:  \"error\": \"OOMKilled: container exceeded memory limit\",\n")
	in := sb.String()

	res, err := c.Compress(Request{Content: in, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if res.Compressed == in {
		t.Fatal("expected line dedup to compress repetitive grep output")
	}
	if strings.Contains(res.Compressed, "pods. json") {
		t.Fatalf("mid-token dot split mangled output:\n%.200s", res.Compressed)
	}
	if !strings.Contains(res.Compressed, "OOMKilled") {
		t.Fatal("error line dropped")
	}

	// The stored original must be intact lines, newline-separated.
	hashes := ccr.ExtractHashes(res.Compressed)
	if len(hashes) != 1 {
		t.Fatalf("want 1 marker, got %d", len(hashes))
	}
	entry, ok := store.Get(hashes[0])
	if !ok {
		t.Fatal("dropped lines not retrievable")
	}
	blob := string(entry.Original)
	if strings.Contains(blob, "pods. json") {
		t.Fatalf("stored original mangled:\n%.200s", blob)
	}
	for _, ln := range strings.Split(blob, "\n") {
		if ln != "" && !strings.HasPrefix(ln, "pods.json:") {
			t.Fatalf("stored original line lost its layout: %q", ln)
		}
	}
}

// Regression (live-test find): when the query is a tool invocation (grep
// pattern + path), terms like the file name match EVERY result line; honoring
// them pinned everything and disabled compression. Such stop-terms must be
// ignored while rare terms still anchor their lines.
func TestTextCrusher_LineMode_IgnoresStopTerms(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewTextCrusher()

	var sb strings.Builder
	for i := 0; i < 150; i++ {
		fmt.Fprintf(&sb, "pods.json:%d:  \"name\": \"pod-%03d\",\n", i*9+3, i)
	}
	sb.WriteString("pods.json:701:  \"name\": \"special-target\",\n")
	in := sb.String()

	// "pods.json" and "name" match every line (stop-terms); "special-target"
	// matches one line (anchor).
	res, _ := c.Compress(Request{
		Content: in,
		Query:   `{"pattern":"name","path":"pods.json","find":"special-target"}`,
		Store:   store,
	})
	if res.Compressed == in {
		t.Fatal("stop-terms pinned every line — compression disabled")
	}
	if !strings.Contains(res.Compressed, "special-target") {
		t.Fatal("discriminative query anchor was dropped")
	}
}

// Prose with mid-token dots (versions, filenames, decimals) must not split
// inside the token.
func TestSplitSentences_NoMidTokenSplit(t *testing.T) {
	sents, sep := splitSentences("We deployed v1.2 reading pods.json with pi 3.14 today. It worked well.")
	if sep != " " {
		t.Fatalf("prose separator = %q, want space", sep)
	}
	if len(sents) != 2 {
		t.Fatalf("want 2 sentences, got %d: %q", len(sents), sents)
	}
	if !strings.Contains(sents[0], "pods.json") || !strings.Contains(sents[0], "3.14") {
		t.Fatalf("mid-token split occurred: %q", sents[0])
	}
}

func TestTextCrusher_NeverEmptyOutput(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewTextCrusher()
	in := prose("An unrelated needle about quantum entanglement and spin states.")
	res, _ := c.Compress(Request{Content: in, Query: "something totally absent zzzzz", Store: store})
	if strings.TrimSpace(res.Compressed) == "" {
		t.Fatal("compression produced empty output")
	}
}
