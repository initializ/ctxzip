package crush

import (
	"fmt"
	"strings"
	"testing"

	"github.com/initializ/ctxzip/ccr"
)

// bigHunk builds one @@ hunk with 3 lead context lines, a change, `interior`
// context lines, another change, and 3 trailing context lines. The header's
// line counts are computed to match the body exactly, as a real diff's do.
func bigHunk(interior int) string {
	ctx := 6 + interior // lead + tail + interior, counted on both sides
	span := ctx + 2     // + two changed lines per side
	var b strings.Builder
	fmt.Fprintf(&b, "@@ -1,%d +1,%d @@ func Handler()\n", span, span)
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

// manyHunksDiff is one file with 15 low-churn hunks (> MaxHunksPerFile); hunk 7
// carries an error term so it must survive the hunk cap.
func manyHunksDiff() string {
	var b strings.Builder
	b.WriteString(fileHeader("app.go"))
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
	return b.String()
}

// manyFilesDiff is 25 files (> MaxFiles), each a tiny change; file 24 carries an
// error term so it must survive the file cap.
func manyFilesDiff() string {
	var b strings.Builder
	for i := 0; i < 25; i++ {
		b.WriteString(fileHeader(fmt.Sprintf("pkg/file%02d.go", i)))
		b.WriteString("@@ -1,1 +1,1 @@\n")
		if i == 24 {
			b.WriteString("-old\n+FATAL boom\n")
		} else {
			b.WriteString("-old\n+new\n")
		}
	}
	return b.String()
}

// expandMarkers reconstructs the input the way a consumer (forge) would: every
// line carrying a ctxzip marker is replaced by the stored original bytes.
func expandMarkers(t *testing.T, compressed string, store ccr.Store) string {
	t.Helper()
	var out []string
	for _, ln := range strings.Split(compressed, "\n") {
		if !strings.Contains(ln, ccr.MarkerPrefix) {
			out = append(out, ln)
			continue
		}
		hs := ccr.ExtractHashes(ln)
		if len(hs) != 1 {
			t.Fatalf("expected exactly one hash in marker line %q, got %d", ln, len(hs))
		}
		e, ok := store.Get(hs[0])
		if !ok {
			t.Fatalf("marker %s not retrievable from store", hs[0])
		}
		out = append(out, strings.Split(string(e.Original), "\n")...)
	}
	return strings.Join(out, "\n")
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
	for _, want := range []string{"-old line one", "+new line one", "-old line two", "+new line two"} {
		if !strings.Contains(res.Compressed, want) {
			t.Fatalf("change line %q was dropped", want)
		}
	}
	if !strings.Contains(res.Compressed, "diff --git a/server.go b/server.go") {
		t.Fatal("file header dropped")
	}
	if !strings.Contains(res.Compressed, "@@ -1,48 +1,48 @@") {
		t.Fatal("hunk header dropped")
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

// TestDiffCrusher_RoundTripReconstructsExactly is the guarantee the type
// documents: expanding every marker in place restores the original diff
// byte-for-byte, in original order — across context-trim, hunk-cap, and
// file-cap paths (the caps interleave protected and dropped items, which is
// exactly what a grouped, trailing offload would reorder).
func TestDiffCrusher_RoundTripReconstructsExactly(t *testing.T) {
	cases := map[string]string{
		"context-trim": fileHeader("server.go") + bigHunk(60),
		"hunk-cap":     manyHunksDiff(),
		"file-cap":     manyFilesDiff(),
		"no-trailing-newline": strings.TrimRight(
			fileHeader("server.go")+bigHunk(60), "\n"),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			store := ccr.NewMemoryStore(ccr.MemoryConfig{})
			res, err := NewDiffCrusher().Compress(Request{Content: in, Store: store})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Markers) == 0 {
				t.Fatalf("%s: expected offloading, got passthrough", name)
			}
			if got := expandMarkers(t, res.Compressed, store); got != in {
				t.Fatalf("%s: round-trip not byte-exact.\n--- want ---\n%q\n--- got ---\n%q", name, in, got)
			}
		})
	}
}

func TestDiffCrusher_KeepsErrorHunkPastCap(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	in := manyHunksDiff()

	res, err := NewDiffCrusher().Compress(Request{Content: in, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if res.Compressed == in {
		t.Fatal("expected hunk-cap compression, got passthrough")
	}
	if !strings.Contains(res.Compressed, "panic: nil pointer dereference") {
		t.Fatal("error hunk was dropped by the hunk cap")
	}
	if !strings.Contains(res.Compressed, "hunk_offloaded") {
		t.Fatalf("expected a hunk_offloaded marker, got:\n%s", res.Compressed)
	}
}

func TestDiffCrusher_FileCapDropsLowChurnKeepsNames(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	in := manyFilesDiff()

	res, err := NewDiffCrusher().Compress(Request{Content: in, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Compressed, "file_offloaded:") {
		t.Fatalf("expected a file_offloaded marker naming the file, got:\n%s", res.Compressed)
	}
	// The error-bearing file is protected and kept verbatim.
	if !strings.Contains(res.Compressed, "pkg/file24.go") || !strings.Contains(res.Compressed, "FATAL boom") {
		t.Fatal("protected (error) file was dropped by the file cap")
	}
}

func TestDiffCrusher_PlainDiffNoGitHeader(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})

	// Two files via plain `diff -u`, no "diff --git" guard.
	in := "--- a/one.txt\n+++ b/one.txt\n" + bigHunk(30) +
		"--- a/two.txt\n+++ b/two.txt\n" + bigHunk(30)

	pd, ok := parseDiff(in)
	if !ok {
		t.Fatal("plain diff did not parse")
	}
	if len(pd.files) != 2 {
		t.Fatalf("want 2 files, got %d", len(pd.files))
	}
	res, err := NewDiffCrusher().Compress(Request{Content: in, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if res.Compressed == in {
		t.Fatal("expected compression of a plain multi-file diff")
	}
	if got := expandMarkers(t, res.Compressed, store); got != in {
		t.Fatal("plain multi-file diff did not round-trip exactly")
	}
}

// TestDiffCrusher_NestedDiffInBodyIsOneFile guards the parser fix: a hunk body
// containing lines that read "--- a/x" and "+++ b/x" (an embedded/edited diff)
// must not be mistaken for a second file. Line-count tracking keeps the body
// intact.
func TestDiffCrusher_NestedDiffInBodyIsOneFile(t *testing.T) {
	// One file, one hunk of 6 old / 6 new lines whose removed+added content is
	// itself a diff header pair.
	in := "diff --git a/change.patch b/change.patch\n" +
		"--- a/change.patch\n+++ b/change.patch\n" +
		"@@ -1,4 +1,4 @@\n" +
		" leading context\n" +
		"---- a/embedded\n" + // a context-ish line, not a boundary
		"-old body line\n" +
		"+new body line\n" +
		"+++ b/embedded is added text\n" +
		" trailing context\n"
	pd, ok := parseDiff(in)
	if !ok {
		t.Fatal("nested-diff content did not parse")
	}
	if len(pd.files) != 1 {
		t.Fatalf("nested diff in body split into %d files, want 1", len(pd.files))
	}
	if n := len(pd.files[0].hunks); n != 1 {
		t.Fatalf("want 1 hunk, got %d", n)
	}
}

func TestDiffCrusher_Deterministic(t *testing.T) {
	in := fileHeader("server.go") + bigHunk(40)

	res1, _ := NewDiffCrusher().Compress(Request{Content: in, Store: ccr.NewMemoryStore(ccr.MemoryConfig{})})
	res2, _ := NewDiffCrusher().Compress(Request{Content: in, Store: ccr.NewMemoryStore(ccr.MemoryConfig{})})
	if res1.Compressed != res2.Compressed {
		t.Fatal("compression is not deterministic (output differs)")
	}
	if strings.Join(res1.Markers, ",") != strings.Join(res2.Markers, ",") {
		t.Fatal("compression is not deterministic (markers differ)")
	}
}

func TestDiffCrusher_SmallDiff_Passthrough(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	in := fileHeader("x.go") + "@@ -1,2 +1,2 @@\n-old\n+new\n context\n"
	res, _ := NewDiffCrusher().Compress(Request{Content: in, Store: store})
	if res.Compressed != in {
		t.Fatalf("small diff should pass through, got:\n%s", res.Compressed)
	}
}

func TestDiffCrusher_NonDiff_Passthrough(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	in := strings.Repeat("just some prose that is not a diff at all.\n", 20)
	res, _ := NewDiffCrusher().Compress(Request{Content: in, Store: store})
	if res.Compressed != in {
		t.Fatal("non-diff content should pass through")
	}
}

func TestDiffCrusher_NilStore_Passthrough(t *testing.T) {
	in := fileHeader("server.go") + bigHunk(40)
	res, _ := NewDiffCrusher().Compress(Request{Content: in, Store: nil})
	if res.Compressed != in {
		t.Fatal("nil store must force lossless passthrough")
	}
}

func TestDiffCrusher_NeverEmptyOutput(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	in := fileHeader("server.go") + bigHunk(200)
	res, _ := NewDiffCrusher().Compress(Request{Content: in, Store: store})
	if strings.TrimSpace(res.Compressed) == "" {
		t.Fatal("non-empty input compressed to empty output")
	}
}
