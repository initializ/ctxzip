package crush

import (
	"fmt"
	"strings"
	"testing"

	"github.com/initializ/ctxzip/ccr"
)

// bigHunk builds one @@ hunk with lead context lines, a change, interior
// context of the given size, another change, and trailing context.
func bigHunk(interior int) string {
	var b strings.Builder
	b.WriteString("@@ -1,50 +1,50 @@ func Handler()\n")
	for i := 0; i < 3; i++ {
		fmt.Fprintf(&b, " context lead %d\n", i)
	}
	b.WriteString("-old line one\n")
	b.WriteString("+new line one\n")
	for i := 0; i < interior; i++ {
		fmt.Fprintf(&b, " unchanged interior %d\n", i)
	}
	b.WriteString("-old line two\n")
	b.WriteString("+new line two\n")
	for i := 0; i < 3; i++ {
		fmt.Fprintf(&b, " context tail %d\n", i)
	}
	return b.String()
}

func fileHeader(path string) string {
	return fmt.Sprintf("diff --git a/%s b/%s\nindex 111..222 100644\n--- a/%s\n+++ b/%s\n", path, path, path, path)
}

func TestDiffCrusher_TrimsContextAndIsReversible(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewDiffCrusher()

	in := fileHeader("server.go") + bigHunk(40)
	res, err := c.Compress(Request{Content: in, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if res.Compressed == in {
		t.Fatal("expected context trimming, got passthrough")
	}
	// Changes are never dropped.
	for _, want := range []string{"-old line one", "+new line one", "-old line two", "+new line two"} {
		if !strings.Contains(res.Compressed, want) {
			t.Fatalf("change line %q was dropped", want)
		}
	}
	// The file/hunk headers survive.
	if !strings.Contains(res.Compressed, "diff --git a/server.go b/server.go") {
		t.Fatal("file header dropped")
	}
	if !strings.Contains(res.Compressed, "@@ -1,50 +1,50 @@") {
		t.Fatal("hunk header dropped")
	}
	// The interior context was offloaded and is retrievable.
	if len(res.Markers) == 0 {
		t.Fatal("no markers emitted")
	}
	if !strings.Contains(res.Compressed, "context_lines_offloaded") {
		t.Fatalf("expected a context-lines marker note, got:\n%s", res.Compressed)
	}
	for _, h := range res.Markers {
		if _, ok := store.Get(h); !ok {
			t.Fatalf("offloaded blob %s not retrievable", h)
		}
	}
}

func TestDiffCrusher_KeepsErrorHunkPastCap(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewDiffCrusher()

	var b strings.Builder
	b.WriteString(fileHeader("app.go"))
	// 15 low-churn hunks (> MaxHunksPerFile=10); hunk 7 carries an error.
	for i := 0; i < 15; i++ {
		fmt.Fprintf(&b, "@@ -%d,3 +%d,3 @@\n context a\n", i*10+1, i*10+1)
		if i == 7 {
			b.WriteString("-panic: nil pointer dereference\n")
			b.WriteString("+return fmt.Errorf(\"guard\")\n")
		} else {
			fmt.Fprintf(&b, "-old %d\n+new %d\n", i, i)
		}
		b.WriteString(" context b\n")
	}
	in := b.String()

	res, err := c.Compress(Request{Content: in, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if res.Compressed == in {
		t.Fatal("expected hunk-cap compression, got passthrough")
	}
	if !strings.Contains(res.Compressed, "panic: nil pointer dereference") {
		t.Fatal("error hunk was dropped by the hunk cap")
	}
	if !strings.Contains(res.Compressed, "hunks_offloaded") {
		t.Fatalf("expected a hunks_offloaded marker, got:\n%s", res.Compressed)
	}
}

func TestDiffCrusher_FileCapDropsLowChurnKeepsNames(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewDiffCrusher()

	var b strings.Builder
	// 25 files (> MaxFiles=20), each a tiny change; one carries an error so it
	// must survive the cap.
	for i := 0; i < 25; i++ {
		path := fmt.Sprintf("pkg/file%02d.go", i)
		b.WriteString(fileHeader(path))
		b.WriteString("@@ -1,2 +1,2 @@\n")
		if i == 24 {
			b.WriteString("-old\n+FATAL boom\n")
		} else {
			b.WriteString("-old\n+new\n")
		}
	}
	in := b.String()

	res, err := c.Compress(Request{Content: in, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Compressed, "files_offloaded") {
		t.Fatalf("expected a files_offloaded marker, got:\n%s", res.Compressed)
	}
	if !strings.Contains(res.Compressed, "[dropped:") {
		t.Fatal("dropped-file names not listed in the marker note")
	}
	// The error-bearing file is protected and kept.
	if !strings.Contains(res.Compressed, "pkg/file24.go") || !strings.Contains(res.Compressed, "FATAL boom") {
		t.Fatal("protected (error) file was dropped by the file cap")
	}
}

func TestDiffCrusher_PlainDiffNoGitHeader(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewDiffCrusher()

	// Two files via plain `diff -u`, no "diff --git" guard.
	in := "--- a/one.txt\n+++ b/one.txt\n" + bigHunk(30) +
		"--- a/two.txt\n+++ b/two.txt\n" + bigHunk(30)

	files, ok := parseDiff(in)
	if !ok {
		t.Fatal("plain diff did not parse")
	}
	if len(files) != 2 {
		t.Fatalf("want 2 files, got %d", len(files))
	}
	res, err := c.Compress(Request{Content: in, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if res.Compressed == in {
		t.Fatal("expected compression of a plain multi-file diff")
	}
}

func TestDiffCrusher_Deterministic(t *testing.T) {
	c := NewDiffCrusher()
	in := fileHeader("server.go") + bigHunk(40)

	res1, _ := c.Compress(Request{Content: in, Store: ccr.NewMemoryStore(ccr.MemoryConfig{})})
	res2, _ := c.Compress(Request{Content: in, Store: ccr.NewMemoryStore(ccr.MemoryConfig{})})
	if res1.Compressed != res2.Compressed {
		t.Fatal("compression is not deterministic (output differs)")
	}
	if strings.Join(res1.Markers, ",") != strings.Join(res2.Markers, ",") {
		t.Fatal("compression is not deterministic (markers differ)")
	}
}

func TestDiffCrusher_SmallDiff_Passthrough(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewDiffCrusher()
	in := fileHeader("x.go") + "@@ -1,2 +1,2 @@\n-old\n+new\n context\n"
	res, _ := c.Compress(Request{Content: in, Store: store})
	if res.Compressed != in {
		t.Fatalf("small diff should pass through, got:\n%s", res.Compressed)
	}
}

func TestDiffCrusher_NonDiff_Passthrough(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewDiffCrusher()
	in := strings.Repeat("just some prose that is not a diff at all.\n", 20)
	res, _ := c.Compress(Request{Content: in, Store: store})
	if res.Compressed != in {
		t.Fatal("non-diff content should pass through")
	}
}

func TestDiffCrusher_NilStore_Passthrough(t *testing.T) {
	c := NewDiffCrusher()
	in := fileHeader("server.go") + bigHunk(40)
	res, _ := c.Compress(Request{Content: in, Store: nil})
	if res.Compressed != in {
		t.Fatal("nil store must force lossless passthrough")
	}
}

func TestDiffCrusher_NeverEmptyOutput(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewDiffCrusher()
	in := fileHeader("server.go") + bigHunk(200)
	res, _ := c.Compress(Request{Content: in, Store: store})
	if strings.TrimSpace(res.Compressed) == "" {
		t.Fatal("non-empty input compressed to empty output")
	}
}
