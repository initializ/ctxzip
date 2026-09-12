package crush

import (
	"fmt"
	"sort"
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
//   - file cap — past MaxFiles, the lowest-churn files are offloaded whole,
//     their names kept in the marker note.
//
// The error floor overrides every cap: a hunk (or context line) mentioning
// error vocabulary, a MustKeep term, or a query term is never dropped.
//
// The output is for READING, not `git apply` — dropped context and inline
// markers make it no longer a well-formed patch. Reversibility (retrieve every
// marker) restores the original bytes exactly.
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
	files, ok := parseDiff(req.Content)
	if !ok || len(files) == 0 {
		return passthrough(c.Name(), req.Content), nil
	}

	terms := queryTerms(req.Query)
	var markers []string
	var out strings.Builder

	keepFile := c.selectFiles(files, req, terms)
	var droppedFiles []*diffFile

	for fi, f := range files {
		if !keepFile[fi] {
			droppedFiles = append(droppedFiles, f)
			continue
		}
		writeLines(&out, f.preamble)

		keepHunk := c.selectHunks(f, req, terms)
		var droppedHunks []*diffHunk
		for hi, h := range f.hunks {
			if !keepHunk[hi] {
				droppedHunks = append(droppedHunks, h)
				continue
			}
			out.WriteString(h.header)
			out.WriteByte('\n')
			writeLines(&out, c.renderHunk(req, h, terms, &markers))
		}
		if len(droppedHunks) > 0 {
			if m, ok := c.offloadHunks(req, droppedHunks, &markers); ok {
				out.WriteString(m)
				out.WriteByte('\n')
			} else {
				// Fail-open: could not store, so keep the hunks verbatim.
				for _, h := range droppedHunks {
					out.WriteString(h.header)
					out.WriteByte('\n')
					writeLines(&out, h.lines)
				}
			}
		}
	}

	if len(droppedFiles) > 0 {
		if m, ok := c.offloadFiles(req, droppedFiles, &markers); ok {
			out.WriteString(m)
			out.WriteByte('\n')
		} else {
			for _, f := range droppedFiles {
				out.WriteString(fileText(f))
				out.WriteByte('\n')
			}
		}
	}

	if len(markers) == 0 {
		return passthrough(c.Name(), req.Content), nil
	}
	return Result{
		Compressed: strings.TrimRight(out.String(), "\n"),
		Strategy:   c.Name(),
		Markers:    markers,
	}, nil
}

// renderHunk returns the hunk body with over-long context runs replaced by
// markers. Change lines (+/-), the no-newline marker (\), and error/query
// context are always kept; unchanged context beyond MaxContextLines of a change
// is dropped in runs of at least MinDropRun.
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
		} else if m, ok := c.offload(req, strings.Join(run, "\n"), "diff", len(run),
			fmt.Sprintf("%d_context_lines_offloaded", len(run)), markers); ok {
			body = append(body, m)
		} else {
			body = append(body, run...) // fail-open
		}
		i = j
	}
	return body
}

// offloadHunks stores the wholly-dropped hunks of one file as a single blob and
// returns the marker line to splice after the file's kept hunks.
func (c *DiffCrusher) offloadHunks(req Request, hunks []*diffHunk, markers *[]string) (string, bool) {
	var b strings.Builder
	for _, h := range hunks {
		b.WriteString(h.header)
		b.WriteByte('\n')
		writeLines(&b, h.lines)
	}
	blob := strings.TrimRight(b.String(), "\n")
	return c.offload(req, blob, "diff", len(hunks),
		fmt.Sprintf("%d_hunks_offloaded", len(hunks)), markers)
}

// offloadFiles stores the wholly-dropped files as a single blob and returns a
// marker line naming them, so the model knows which files were set aside.
func (c *DiffCrusher) offloadFiles(req Request, files []*diffFile, markers *[]string) (string, bool) {
	parts := make([]string, len(files))
	names := make([]string, len(files))
	for i, f := range files {
		parts[i] = fileText(f)
		names[i] = f.path
	}
	blob := strings.Join(parts, "\n")
	m, ok := c.offload(req, blob, "diff", len(files),
		fmt.Sprintf("%d_files_offloaded", len(files)), markers)
	if !ok {
		return "", false
	}
	return m + " [dropped: " + strings.Join(names, ", ") + "]", true
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
	churn := make([]int, len(files))
	type cand struct{ idx, churn int }
	var rest []cand
	budget := c.MaxFiles
	for i, f := range files {
		ch, prot := fileStats(f, req, terms)
		churn[i] = ch
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
	if headerProtected := looksError(strings.ToLower(h.header)); headerProtected {
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

// parseDiff splits a unified diff into files and hunks. ok is false when the
// content yields no hunks (nothing this crusher can shrink), so the caller can
// pass it through.
//
// A new file begins at a "diff --git" line, or at a "--- " line immediately
// followed by "+++ " (plain `diff -u`, no git header). The latter lookahead is
// what keeps a removed line that happens to read "--- foo" from being mistaken
// for a file boundary; git diffs additionally carry the unambiguous
// "diff --git" guard.
func parseDiff(content string) ([]*diffFile, bool) {
	lines := strings.Split(content, "\n")
	var files []*diffFile
	var cur *diffFile
	var curHunk *diffHunk
	inHunk := false

	newFile := func() {
		cur = &diffFile{}
		files = append(files, cur)
		curHunk = nil
		inHunk = false
	}

	total := 0
	for i, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "diff --git "):
			newFile()
			cur.preamble = append(cur.preamble, ln)
			cur.path = gitPath(ln)
		case strings.HasPrefix(ln, "--- ") && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "+++ "):
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
			inHunk = true
			total++
		case inHunk && curHunk != nil && isHunkBodyLine(ln):
			curHunk.lines = append(curHunk.lines, ln)
		default:
			if cur == nil {
				continue // leading noise before the first file header
			}
			if inHunk {
				inHunk = false // a non-body line ends the current hunk
			}
			cur.preamble = append(cur.preamble, ln)
			if strings.HasPrefix(ln, "+++ ") && cur.path == "" {
				cur.path = plusPath(ln)
			}
		}
	}
	if total == 0 {
		return nil, false
	}
	return files, true
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

// fileText reconstructs a file section's original text (preamble + all hunks).
func fileText(f *diffFile) string {
	var b strings.Builder
	writeLines(&b, f.preamble)
	for _, h := range f.hunks {
		b.WriteString(h.header)
		b.WriteByte('\n')
		writeLines(&b, h.lines)
	}
	return strings.TrimRight(b.String(), "\n")
}

// writeLines writes each line followed by a newline to b.
func writeLines(b *strings.Builder, lines []string) {
	for _, ln := range lines {
		b.WriteString(ln)
		b.WriteByte('\n')
	}
}
