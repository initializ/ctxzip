package crush

import (
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

func TestTextCrusher_NeverEmptyOutput(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewTextCrusher()
	in := prose("An unrelated needle about quantum entanglement and spin states.")
	res, _ := c.Compress(Request{Content: in, Query: "something totally absent zzzzz", Store: store})
	if strings.TrimSpace(res.Compressed) == "" {
		t.Fatal("compression produced empty output")
	}
}
