package crush

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/initializ/ctxzip/ccr"
)

// DiffCrusher compresses unified diffs (git and plain `diff -u`). It keeps the
// change itself — every +/- line and every file/hunk header — verbatim, and
// offloads the bulk that dominates large diffs: long runs of unchanged context,
// low-signal hunks in churny files, and whole files past a cap. Its strategy
// mirrors headroom's diff_compressor.
//
// Three cumulative reductions, each reversible via the CCR store:
//
//   - context trimming — context lines further than MaxContextLines from any
//     change are dropped (contiguous runs of at least MinDropRun),
//   - per-file hunk cap — a file with more than MaxHunksPerFile hunks keeps the
//     highest-churn ones and offloads the rest as whole hunks,
//   - file cap — past MaxFiles, the lowest-churn files are offloaded whole.
//
// The error floor overrides every cap: a hunk (or context line) mentioning
// error vocabulary, a MustKeep term, or a query term is never dropped.
//
// Reversibility is position-preserving. Every drop — a context run, a capped
// hunk, or a capped file — leaves a marker exactly where the dropped bytes
// were, so expanding all markers in place (replacing each <<ctxzip:HASH>> with
// its stored bytes) reconstructs the original diff byte-for-byte: same files
// and hunks, in their original order. The compressed form itself is for
// READING, not `git apply` — the inline markers and trimmed context make it not
// a well-formed patch until expanded.
type DiffCrusher struct {
	// MaxContextLines is how many unchanged lines to keep on each side of a
	// change before trimming.
	MaxContextLines int
	// MaxHunksPerFile caps hunks kept per file; excess low-churn hunks offload.
	MaxHunksPerFile int
	// MaxFiles caps files kept; excess low-churn files offload whole.
	MaxFiles int
	// MinDropRun is the shortest contiguous context run worth offloading — a
	// marker costs ~1 line, so dropping one or two lines is not worth it.
	MinDropRun int
	// MinLines is the diff size (in newlines) below which it is left alone.
	MinLines int
}

// NewDiffCrusher returns a DiffCrusher with sensible defaults.
func NewDiffCrusher() *DiffCrusher {
	return &DiffCrusher{
		MaxContextLines: 3,
		MaxHunksPerFile: 10,
		MaxFiles:        20,
		MinDropRun:      3,
		MinLines:        12,
	}
}

// Name implements Compressor.
func (c *DiffCrusher) Name() string { return "diff_crusher" }

// Compress implements Compressor.
func (c *DiffCrusher) Compress(req Request) (Result, error) {
	if req.Store == nil || strings.Count(req.Content, "\n") < c.MinLines {
		return passthrough(c.Name(), req.Content), nil
	}
	pd, ok := parseDiff(req.Content)
	if !ok || len(pd.files) == 0 {
		return passthrough(c.Name(), req.Content), nil
	}

	terms := queryTerms(req.Query)
	var markers []string
	// out accumulates the compressed diff line by line. Every original line is
	// either copied verbatim or replaced, in place, by a single marker line —
	// which is what makes expansion an exact, order-preserving inverse.
	out := append([]string(nil), pd.leading...)

	keepFile := c.selectFiles(pd.files, req, terms)
	for fi, f := range pd.files {
		if !keepFile[fi] {
			out = c.emitOffload(req, out, fileText(f), "diff", len(f.hunks),
				"file_offloaded: "+f.path, &markers)
			continue
		}
		out = append(out, f.preamble...)

		keepHunk := c.selectHunks(f, req, terms)
		for hi, h := range f.hunks {
			if !keepHunk[hi] {
				out = c.emitOffload(req, out, hunkText(h), "diff", len(h.lines),
					"hunk_offloaded", &markers)
				continue
			}
			out = append(out, h.header)
			out = append(out, c.renderHunk(req, h, terms, &markers)...)
		}
	}

	if len(markers) == 0 {
		return passthrough(c.Name(), req.Content), nil
	}
	result := strings.Join(out, "\n")
	if pd.trailingNewline {
		result += "\n"
	}
	return Result{Compressed: result, Strategy: c.Name(), Markers: markers}, nil
}

// emitOffload stores blob and appends its marker line to out — in the position
// blob occupied, so expansion restores it there. On a store failure it appends
// the original lines verbatim (fail-open), never losing data.
func (c *DiffCrusher) emitOffload(req Request, out []string, blob, kind string, items int, note string, markers *[]string) []string {
	if m, ok := c.offload(req, blob, kind, items, note, markers); ok {
		return append(out, m)
	}
	return append(out, strings.Split(blob, "\n")...)
}

// renderHunk returns the hunk body with over-long context runs replaced, in
// place, by markers. Change lines (+/-), the no-newline marker (\), and
// error/query context are always kept; unchanged context beyond MaxContextLines
// of a change is dropped in runs of at least MinDropRun.
func (c *DiffCrusher) renderHunk(req Request, h *diffHunk, terms []string, markers *[]string) []string {
	n := len(h.lines)
	keep := make([]bool, n)
	hasChange := false
	for i, ln := range h.lines {
		switch lineKind(ln) {
		case '+', '-':
			keep[i] = true
			hasChange = true
		case '\\':
			keep[i] = true
		default: // context
			lower := strings.ToLower(ln)
			if looksError(lower) || mustKeep(lower, req.MustKeep) || matchesAny(lower, terms) {
				keep[i] = true
			}
		}
	}
	// A hunk with no +/- lines is unusual; keep it whole rather than risk
	// dropping its entire body.
	if !hasChange {
		return h.lines
	}
	// Keep context within MaxContextLines of any change.
	for i, ln := range h.lines {
		if k := lineKind(ln); k == '+' || k == '-' {
			for d := 1; d <= c.MaxContextLines; d++ {
				if i-d >= 0 {
					keep[i-d] = true
				}
				if i+d < n {
					keep[i+d] = true
				}
			}
		}
	}

	body := make([]string, 0, n)
	for i := 0; i < n; {
		if keep[i] {
			body = append(body, h.lines[i])
			i++
			continue
		}
		j := i
		for j < n && !keep[j] {
			j++
		}
		run := h.lines[i:j]
		if len(run) < c.MinDropRun {
			body = append(body, run...) // too small to be worth a marker
		} else {
			body = c.emitOffload(req, body, strings.Join(run, "\n"), "diff", len(run),
				fmt.Sprintf("%d_context_lines_offloaded", len(run)), markers)
		}
		i = j
	}
	return body
}

// offload hashes blob, stores it, records the hash, and returns its marker.
func (c *DiffCrusher) offload(req Request, blob, kind string, items int, note string, markers *[]string) (string, bool) {
	b := []byte(blob)
	hash := ccr.Hash(b)
	if err := req.Store.Put(hash, b, ccr.Meta{
		ToolName:     req.ToolName,
		Query:        req.Query,
		ItemCount:    items,
		OriginalKind: kind,
	}); err != nil {
		return "", false
	}
	*markers = append(*markers, hash)
	return ccr.Marker(hash, note), true
}

// selectFiles decides which files to keep. Under the cap, all are kept.
// Otherwise every protected file (error/query/must-keep) is kept, and the
// remaining budget is filled by churn (number of changed lines), highest first.
func (c *DiffCrusher) selectFiles(files []*diffFile, req Request, terms []string) []bool {
	keep := make([]bool, len(files))
	if len(files) <= c.MaxFiles {
		for i := range keep {
			keep[i] = true
		}
		return keep
	}
	type cand struct{ idx, churn int }
	var rest []cand
	budget := c.MaxFiles
	for i, f := range files {
		ch, prot := fileStats(f, req, terms)
		if prot {
			keep[i] = true
			budget--
		} else {
			rest = append(rest, cand{i, ch})
		}
	}
	sort.SliceStable(rest, func(a, b int) bool { return rest[a].churn > rest[b].churn })
	for _, r := range rest {
		if budget <= 0 {
			break
		}
		keep[r.idx] = true
		budget--
	}
	return keep
}

// selectHunks mirrors selectFiles at hunk granularity within one file.
func (c *DiffCrusher) selectHunks(f *diffFile, req Request, terms []string) []bool {
	keep := make([]bool, len(f.hunks))
	if len(f.hunks) <= c.MaxHunksPerFile {
		for i := range keep {
			keep[i] = true
		}
		return keep
	}
	type cand struct{ idx, churn int }
	var rest []cand
	budget := c.MaxHunksPerFile
	for i, h := range f.hunks {
		ch, prot := hunkStats(h, req, terms)
		if prot {
			keep[i] = true
			budget--
		} else {
			rest = append(rest, cand{i, ch})
		}
	}
	sort.SliceStable(rest, func(a, b int) bool { return rest[a].churn > rest[b].churn })
	for _, r := range rest {
		if budget <= 0 {
			break
		}
		keep[r.idx] = true
		budget--
	}
	return keep
}

// fileStats returns a file's churn (changed-line count) and whether any line in
// it is protected by the error floor, a must-keep term, or the query.
func fileStats(f *diffFile, req Request, terms []string) (churn int, protected bool) {
	for _, h := range f.hunks {
		ch, prot := hunkStats(h, req, terms)
		churn += ch
		protected = protected || prot
	}
	return churn, protected
}

// hunkStats returns a hunk's churn and protection, scanning its body once.
func hunkStats(h *diffHunk, req Request, terms []string) (churn int, protected bool) {
	if looksError(strings.ToLower(h.header)) {
		protected = true
	}
	for _, ln := range h.lines {
		if k := lineKind(ln); k == '+' || k == '-' {
			churn++
		}
		lower := strings.ToLower(ln)
		if looksError(lower) || mustKeep(lower, req.MustKeep) || matchesAny(lower, terms) {
			protected = true
		}
	}
	return churn, protected
}

// parsedDiff is the structured form of a unified diff, retaining enough to
// reconstruct the input byte-for-byte: any content before the first file
// header, the file sections, and whether the input ended with a newline.
type parsedDiff struct {
	leading         []string
	files           []*diffFile
	trailingNewline bool
}

// diffFile is one file section of a unified diff: its header preamble plus hunks.
type diffFile struct {
	preamble []string // diff --git, index, ---, +++, mode/rename/Binary lines
	path     string   // best-effort display path, for the dropped-file note
	hunks    []*diffHunk
}

// diffHunk is one @@ ... @@ block: the header line and its body lines.
type diffHunk struct {
	header string
	lines  []string
}

// hunkHeaderRe captures the optional old/new line counts from a hunk header:
// "@@ -oldStart[,oldCount] +newStart[,newCount] @@ [section]". A missing count
// means 1 (unified-diff convention).
var hunkHeaderRe = regexp.MustCompile(`^@@ -\d+(?:,(\d+))? \+\d+(?:,(\d+))? @@`)

// parseHunkCounts returns how many old-side and new-side lines a hunk spans.
// An unparseable header yields large sentinels so the body is consumed greedily
// (best effort for a malformed diff, which git/diff never emits).
func parseHunkCounts(header string) (oldCount, newCount int) {
	m := hunkHeaderRe.FindStringSubmatch(header)
	if m == nil {
		return 1 << 30, 1 << 30
	}
	oldCount, newCount = 1, 1
	if m[1] != "" {
		oldCount, _ = strconv.Atoi(m[1])
	}
	if m[2] != "" {
		newCount, _ = strconv.Atoi(m[2])
	}
	return oldCount, newCount
}

// parseDiff splits a unified diff into files and hunks. ok is false when the
// content yields no hunks (nothing this crusher can shrink), so the caller can
// pass it through.
//
// Hunk boundaries are tracked by the line counts declared in each "@@" header,
// so a hunk ends exactly where it should. That is what keeps a hunk BODY line
// reading "--- a/x" (an embedded/edited diff in the content) from being
// mistaken for a file boundary: while a hunk still has lines to consume, a body
// line always wins. A new file otherwise begins at "diff --git", or — for plain
// `diff -u` with no git header — at a "--- " line immediately followed by
// "+++ ".
func parseDiff(content string) (*parsedDiff, bool) {
	raw := strings.Split(content, "\n")
	pd := &parsedDiff{}
	// A trailing "" element is the input's final newline; strip it here and
	// restore it on output so reconstruction is byte-exact.
	if n := len(raw); n > 0 && raw[n-1] == "" {
		pd.trailingNewline = true
		raw = raw[:n-1]
	}

	var cur *diffFile
	var curHunk *diffHunk
	oldRem, newRem := 0, 0
	inHunk := false
	total := 0

	newFile := func() {
		cur = &diffFile{}
		pd.files = append(pd.files, cur)
		curHunk = nil
		inHunk = false
	}

	for i, ln := range raw {
		switch {
		case strings.HasPrefix(ln, "diff --git "):
			newFile()
			cur.preamble = append(cur.preamble, ln)
			cur.path = gitPath(ln)
		case inHunk && (oldRem > 0 || newRem > 0) && isHunkBodyLine(ln):
			curHunk.lines = append(curHunk.lines, ln)
			switch lineKind(ln) {
			case '+':
				newRem--
			case '-':
				oldRem--
			case '\\':
				// "\ No newline at end of file" spans neither side.
			default: // context
				oldRem--
				newRem--
			}
			if oldRem <= 0 && newRem <= 0 {
				inHunk = false
			}
		case strings.HasPrefix(ln, "--- ") && i+1 < len(raw) && strings.HasPrefix(raw[i+1], "+++ "):
			if cur == nil || len(cur.hunks) > 0 || inHunk {
				newFile()
			}
			cur.preamble = append(cur.preamble, ln)
			inHunk = false
		case strings.HasPrefix(ln, "@@ "):
			if cur == nil {
				newFile()
			}
			curHunk = &diffHunk{header: ln}
			cur.hunks = append(cur.hunks, curHunk)
			oldRem, newRem = parseHunkCounts(ln)
			inHunk = oldRem > 0 || newRem > 0
			total++
		default:
			if cur == nil {
				pd.leading = append(pd.leading, ln) // content before the first file
				continue
			}
			inHunk = false
			cur.preamble = append(cur.preamble, ln)
			if strings.HasPrefix(ln, "+++ ") && cur.path == "" {
				cur.path = plusPath(ln)
			}
		}
	}
	if total == 0 {
		return nil, false
	}
	return pd, true
}

// isHunkBodyLine reports whether ln is a unified-diff body line: context (space
// or empty), addition (+), removal (-), or the "\ No newline" marker.
func isHunkBodyLine(ln string) bool {
	if ln == "" {
		return true
	}
	switch ln[0] {
	case ' ', '+', '-', '\\':
		return true
	}
	return false
}

// lineKind classifies a hunk body line by its first byte. An empty line is
// treated as context.
func lineKind(ln string) byte {
	if ln == "" {
		return ' '
	}
	switch ln[0] {
	case '+', '-', '\\':
		return ln[0]
	default:
		return ' '
	}
}

// gitPath extracts the b-side path from a "diff --git a/x b/x" line.
func gitPath(ln string) string {
	fields := strings.Fields(ln)
	if len(fields) < 4 {
		return ""
	}
	return strings.TrimPrefix(fields[len(fields)-1], "b/")
}

// plusPath extracts the path from a "+++ b/x" line, dropping a tab-separated
// timestamp if present.
func plusPath(ln string) string {
	p := strings.TrimPrefix(ln, "+++ ")
	if tab := strings.IndexByte(p, '\t'); tab >= 0 {
		p = p[:tab]
	}
	return strings.TrimPrefix(p, "b/")
}

// hunkText reconstructs a hunk's original text (header + body lines).
func hunkText(h *diffHunk) string {
	if len(h.lines) == 0 {
		return h.header
	}
	return h.header + "\n" + strings.Join(h.lines, "\n")
}

// fileText reconstructs a file section's original text (preamble + all hunks).
func fileText(f *diffFile) string {
	parts := make([]string, 0, len(f.preamble)+len(f.hunks))
	parts = append(parts, f.preamble...)
	for _, h := range f.hunks {
		parts = append(parts, h.header)
		parts = append(parts, h.lines...)
	}
	return strings.Join(parts, "\n")
}
