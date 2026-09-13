package crush

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/initializ/ctxzip/ccr"
)

// SearchCrusher compresses grep-style search output — lines of the form
// "path:line:content" (grep -rn, rg --no-heading -n). Its strategy mirrors
// headroom's search_compressor: group matches by file, keep the ones that
// matter, and offload the rest.
//
// The extractive text crusher already dedups such lines, but a dedicated
// crusher earns three wins line-dedup cannot: it strips the repeated path
// prefix by grouping under one per-file header; it always keeps each file's
// first and last match as anchors; and it selects by relevance SCORE (via the
// shared Scorer — the threshold-based consumer that scorer was built for) plus
// the error floor, rather than by dedup alone.
//
// Reversibility is content-complete, not positional. Grouping by file is the
// whole point (it removes per-line path repetition), so the compressed form is
// deliberately NOT a line-for-line image of the input: kept matches move under
// their file header, and dropped matches are offloaded. Every dropped match is
// retrievable in full "path:line:content" form, and every kept match is shown
// verbatim under its header — so no match is lost, but line order across files
// is regrouped.
type SearchCrusher struct {
	// MaxMatchesPerFile caps matches kept per file (anchors + protected always
	// survive, so a file can exceed this when many matches are protected).
	MaxMatchesPerFile int
	// MaxTotalMatches caps matches kept across all files.
	MaxTotalMatches int
	// MaxFiles caps files kept; files with a protected match are always kept.
	MaxFiles int
	// AlwaysKeepFirst / AlwaysKeepLast keep each file's boundary matches.
	AlwaysKeepFirst bool
	AlwaysKeepLast  bool
	// MinMatches is the match count below which the input is left alone.
	MinMatches int
	// Scorer ranks matches by query relevance. Defaults to HybridScorer.
	Scorer Scorer
}

// NewSearchCrusher returns a SearchCrusher with sensible defaults.
func NewSearchCrusher() *SearchCrusher {
	return &SearchCrusher{
		MaxMatchesPerFile: 5,
		MaxTotalMatches:   30,
		MaxFiles:          15,
		AlwaysKeepFirst:   true,
		AlwaysKeepLast:    true,
		MinMatches:        8,
		Scorer:            NewHybridScorer(),
	}
}

// Name implements Compressor.
func (c *SearchCrusher) Name() string { return "search_crusher" }

func (c *SearchCrusher) scorer() Scorer {
	if c.Scorer == nil {
		return NewHybridScorer()
	}
	return c.Scorer
}

// Compress implements Compressor.
func (c *SearchCrusher) Compress(req Request) (Result, error) {
	if req.Store == nil {
		return passthrough(c.Name(), req.Content), nil
	}
	files, total, ok := parseSearch(req.Content)
	if !ok || total < c.MinMatches {
		return passthrough(c.Name(), req.Content), nil
	}

	terms := queryTerms(req.Query)
	c.score(files, req, terms)
	c.selectMatches(files)

	var markers []string
	var out []string
	var droppedFiles []*searchFile

	// Output files alphabetically for determinism and readability.
	sort.Slice(files, func(a, b int) bool { return files[a].path < files[b].path })
	for _, f := range files {
		if !f.keep {
			droppedFiles = append(droppedFiles, f)
			continue
		}
		out = append(out, f.path+":")
		var dropped []*searchMatch
		for _, m := range f.matches {
			if m.keep {
				// "  <line>:<content>" — no space after the colon, so content is
				// byte-preserved and the full line reconstructs exactly.
				out = append(out, "  "+strconv.Itoa(m.line)+":"+m.content)
			} else {
				dropped = append(dropped, m)
			}
		}
		if len(dropped) > 0 {
			out = c.emitOffload(req, out, f.path, dropped, &markers)
		}
	}
	if len(droppedFiles) > 0 {
		out = c.emitDroppedFiles(req, out, droppedFiles, &markers)
	}

	if len(markers) == 0 {
		return passthrough(c.Name(), req.Content), nil
	}
	return Result{Compressed: strings.Join(out, "\n"), Strategy: c.Name(), Markers: markers}, nil
}

// score assigns each match a relevance score and marks the protected ones. The
// query component comes from the shared Scorer over all match contents; the
// error floor (and MustKeep) both boost the score and force-keep the match.
func (c *SearchCrusher) score(files []*searchFile, req Request, terms []string) {
	var all []*searchMatch
	for _, f := range files {
		all = append(all, f.matches...)
	}
	docs := make([][]string, len(all))
	for i, m := range all {
		docs[i] = tokenizeWords(m.content)
	}
	scores := c.scorer().Scores(terms, docs)
	for i, m := range all {
		m.score = scores[i]
		lower := strings.ToLower(m.content)
		if looksError(lower) || mustKeep(lower, req.MustKeep) {
			m.protected = true
			m.score += 1.0 // rank protected matches (and their files) to the top
		}
		if looksFragile(m.content) {
			m.score += 0.3
		}
	}
}

// selectMatches sets keep on the matches to retain: per-file anchors + protected
// + top-scored up to MaxMatchesPerFile, then a file cap, then a global cap.
func (c *SearchCrusher) selectMatches(files []*searchFile) {
	for _, f := range files {
		c.selectInFile(f)
	}
	c.applyFileCap(files)
	c.applyGlobalCap(files)
}

// selectInFile marks a single file's kept matches.
func (c *SearchCrusher) selectInFile(f *searchFile) {
	n := len(f.matches)
	if c.AlwaysKeepFirst {
		f.matches[0].keep, f.matches[0].anchor = true, true
	}
	if c.AlwaysKeepLast {
		f.matches[n-1].keep, f.matches[n-1].anchor = true, true
	}
	kept := 0
	for _, m := range f.matches {
		if m.protected {
			m.keep = true
		}
		if m.keep {
			kept++
		}
	}
	// Fill remaining budget with the highest-scored not-yet-kept matches.
	rest := make([]*searchMatch, 0, n)
	for _, m := range f.matches {
		if !m.keep {
			rest = append(rest, m)
		}
	}
	sortByScore(rest)
	for _, m := range rest {
		if kept >= c.MaxMatchesPerFile {
			break
		}
		m.keep = true
		kept++
	}
}

// applyFileCap keeps files bearing a protected match plus the top MaxFiles by
// total score; the rest are dropped whole.
func (c *SearchCrusher) applyFileCap(files []*searchFile) {
	type cand struct {
		f     *searchFile
		total float64
		prot  bool
	}
	cands := make([]cand, len(files))
	for i, f := range files {
		var sum float64
		prot := false
		for _, m := range f.matches {
			sum += m.score
			prot = prot || m.protected
		}
		cands[i] = cand{f, sum, prot}
	}
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].total != cands[b].total {
			return cands[a].total > cands[b].total
		}
		return cands[a].f.path < cands[b].f.path
	})
	budget := c.MaxFiles
	for _, cd := range cands {
		if cd.prot {
			cd.f.keep = true
			budget--
		}
	}
	for _, cd := range cands {
		if cd.f.keep {
			continue
		}
		if budget > 0 {
			cd.f.keep = true
			budget--
		}
	}
}

// globalMinKeep is the floor the adaptive global cap will not size below.
const globalMinKeep = 5

// applyGlobalCap trims the lowest-scored non-protected, non-anchor matches until
// the kept total is within an ADAPTIVE target: MaxTotalMatches is only the
// ceiling — on highly repetitive results the target shrinks toward the number of
// distinct matches, so redundant grep output is not padded out to 30 near-copies.
func (c *SearchCrusher) applyGlobalCap(files []*searchFile) {
	var kept, trimmable []*searchMatch
	var contents []string
	for _, f := range files {
		if !f.keep {
			continue
		}
		for _, m := range f.matches {
			if !m.keep {
				continue
			}
			kept = append(kept, m)
			contents = append(contents, m.content)
			if !m.protected && !m.anchor {
				trimmable = append(trimmable, m)
			}
		}
	}
	target := adaptiveKeepCount(contents, globalMinKeep, c.MaxTotalMatches)
	over := len(kept) - target
	if over <= 0 {
		return
	}
	// Drop the lowest-scored trimmable matches first (protected and anchors are
	// never dropped, so the kept total may stay above target — the floor wins).
	sortByScore(trimmable) // desc
	for i := 0; i < over && i < len(trimmable); i++ {
		trimmable[len(trimmable)-1-i].keep = false
	}
}

// emitOffload stores a file's dropped matches (full "path:line:content" lines,
// line order) and appends the marker under the file's group.
func (c *SearchCrusher) emitOffload(req Request, out []string, path string, dropped []*searchMatch, markers *[]string) []string {
	blob := rawLines(dropped)
	if m, ok := c.offload(req, blob, len(dropped),
		fmt.Sprintf("%d_matches_offloaded", len(dropped)), markers); ok {
		return append(out, "  "+m)
	}
	// Fail-open: show the dropped matches verbatim rather than lose them.
	for _, sm := range dropped {
		out = append(out, "  "+strconv.Itoa(sm.line)+":"+sm.content)
	}
	return out
}

// emitDroppedFiles stores every match of the wholly-dropped files as one blob
// and appends a marker naming them.
func (c *SearchCrusher) emitDroppedFiles(req Request, out []string, files []*searchFile, markers *[]string) []string {
	var lines []string
	names := make([]string, len(files))
	count := 0
	for i, f := range files {
		lines = append(lines, rawLines(f.matches))
		names[i] = f.path
		count += len(f.matches)
	}
	blob := strings.Join(lines, "\n")
	note := fmt.Sprintf("%d_matches_in_%d_files_offloaded", count, len(files))
	if m, ok := c.offload(req, blob, count, note, markers); ok {
		return append(out, m+" [dropped: "+strings.Join(names, ", ")+"]")
	}
	for _, f := range files {
		out = append(out, f.path+":")
		for _, sm := range f.matches {
			out = append(out, "  "+strconv.Itoa(sm.line)+":"+sm.content)
		}
	}
	return out
}

// offload hashes blob, stores it, records the hash, and returns its marker.
func (c *SearchCrusher) offload(req Request, blob string, items int, note string, markers *[]string) (string, bool) {
	b := []byte(blob)
	hash := ccr.Hash(b)
	if err := req.Store.Put(hash, b, ccr.Meta{
		ToolName:     req.ToolName,
		Query:        req.Query,
		ItemCount:    items,
		OriginalKind: "search",
	}); err != nil {
		return "", false
	}
	*markers = append(*markers, hash)
	return ccr.Marker(hash, note), true
}

// rawLines joins matches' exact original lines, line-number ascending, so an
// offloaded blob is a byte-faithful slice of the input (original separators and
// all) — retrieval returns the lines exactly as grep emitted them.
func rawLines(matches []*searchMatch) string {
	sorted := append([]*searchMatch(nil), matches...)
	sort.SliceStable(sorted, func(a, b int) bool { return sorted[a].line < sorted[b].line })
	lines := make([]string, len(sorted))
	for i, m := range sorted {
		lines[i] = m.raw
	}
	return strings.Join(lines, "\n")
}

// sortByScore orders matches by score desc, line asc (deterministic).
func sortByScore(ms []*searchMatch) {
	sort.SliceStable(ms, func(a, b int) bool {
		if ms[a].score != ms[b].score {
			return ms[a].score > ms[b].score
		}
		return ms[a].line < ms[b].line
	})
}

// searchMatch is one "path:line:content" hit. raw is the exact original line
// (which may use ':' or '-' separators), kept so an offloaded match is
// retrievable byte-for-byte.
type searchMatch struct {
	line      int
	content   string
	raw       string
	score     float64
	protected bool
	anchor    bool
	keep      bool
}

// searchFile groups the matches under one path.
type searchFile struct {
	path    string
	matches []*searchMatch
	keep    bool
}

// parseSearch splits grep-style output into files. Both "path:line:content"
// (matches) and "path-line-content" (grep -A/-B/-C context lines) are parsed;
// grep hunk separators ("--") and blank lines are elided as noise. ok is false
// if any other line fails to parse — mixed content (prose, "Binary file …
// matches") is left for another strategy, so nothing is silently dropped.
// total is the parsed line count.
func parseSearch(content string) (files []*searchFile, total int, ok bool) {
	lines := strings.Split(content, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1] // trailing newline
	}
	byPath := make(map[string]*searchFile)
	for _, ln := range lines {
		if ln == "" || ln == "--" {
			continue // blank line / grep hunk separator: pure noise
		}
		path, num, body, ok := parseMatchLine(ln)
		if !ok {
			return nil, 0, false
		}
		f := byPath[path]
		if f == nil {
			f = &searchFile{path: path}
			byPath[path] = f
			files = append(files, f)
		}
		f.matches = append(f.matches, &searchMatch{line: num, content: body, raw: ln})
		total++
	}
	if total == 0 {
		return nil, 0, false
	}
	return files, total, true
}

// scanTier is a parse strategy for one match line, tried most-specific first.
type scanTier int

const (
	tierColon      scanTier = iota // path:line:content (grep matches)
	tierDash                       // path-line-content (grep context lines)
	tierPermissive                 // leftmost :line: or -line-
)

// parseMatchLine parses a grep line into (path, line, content), trying the colon
// tier, then the dash tier, then a permissive fallback. Ported from headroom's
// scan_match_line. The path must be non-empty and whitespace-free (typed tiers);
// the line field is all digits between a matched separator pair.
func parseMatchLine(ln string) (path string, num int, content string, ok bool) {
	for _, t := range []scanTier{tierColon, tierDash, tierPermissive} {
		if p, n, c, ok := scanMatchLine(ln, t); ok {
			return p, n, c, true
		}
	}
	return "", 0, "", false
}

// scanMatchLine finds the <sep><digits><sep> line-number marker for one tier.
func scanMatchLine(ln string, tier scanTier) (string, int, string, bool) {
	b := ln
	n := len(b)
	// Skip a Windows drive prefix ("C:\" / "C:/") so its colon isn't misread
	// as the line-number separator.
	scanStart := 0
	if n >= 3 && isAlpha(b[0]) && b[1] == ':' && (b[2] == '\\' || b[2] == '/') {
		scanStart = 2
	}

	firstSet, chosenSet := false, false
	var fEnd, fDS, fDE, cEnd, cDS, cDE int
	for i := scanStart; i < n; {
		sep := b[i] == ':' || b[i] == '-'
		var tierSep bool
		switch tier {
		case tierColon:
			tierSep = b[i] == ':'
		case tierDash:
			tierSep = b[i] == '-'
		default:
			tierSep = sep
		}
		if !tierSep {
			i++
			continue
		}
		// Collapse adjacent-separator runs ("::", ":-") so "-1" negatives in
		// content aren't read as the marker.
		if i > 0 && (b[i-1] == ':' || b[i-1] == '-') {
			i++
			continue
		}
		// Typed tiers: the path is whitespace-free; bodies routinely aren't.
		if tier != tierPermissive && hasWhitespace(b[:i]) {
			break
		}
		ds := i + 1
		j := ds
		for j < n && isDigit(b[j]) {
			j++
		}
		closes := j > ds && j < n
		if closes {
			if tier == tierPermissive {
				closes = b[j] == ':' || b[j] == '-'
			} else {
				closes = b[j] == b[i]
			}
		}
		if !closes {
			i++
			continue
		}
		if i == 0 {
			return "", 0, "", false // zero-length path
		}
		if !firstSet {
			firstSet, fEnd, fDS, fDE = true, i, ds, j
		}
		if tier != tierDash {
			chosenSet, cEnd, cDS, cDE = true, i, ds, j
		}
		// Advance past this marker only on positive evidence the path runs
		// through it: the path so far ends in an extension (this marker is the
		// boundary), or the tail carries no further path structure.
		if lastSegmentHasExtension(b[:i]) || !pathContinues(b[j+1:]) {
			chosenSet, cEnd, cDS, cDE = true, i, ds, j
			break
		}
		i = j + 1
	}

	end, ds, de := cEnd, cDS, cDE
	if !chosenSet {
		if !firstSet {
			return "", 0, "", false
		}
		end, ds, de = fEnd, fDS, fDE
	}
	num, err := strconv.Atoi(b[ds:de])
	if err != nil {
		return "", 0, "", false
	}
	return b[:end], num, b[de+1:], true
}

func isAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func hasWhitespace(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\r', '\n', '\f', '\v':
			return true
		}
	}
	return false
}

// hasExtensionDot reports whether tok holds a "." followed by 1–8 alphanumerics
// with at least one letter — a file extension. The letter requirement keeps a
// dotted version ("v1.2.3", all-digit runs) from reading as an extension.
func hasExtensionDot(tok string) bool {
	for i := 0; i+1 < len(tok); i++ {
		if tok[i] != '.' {
			continue
		}
		end := i + 1
		for end < len(tok) && (isAlpha(tok[end]) || isDigit(tok[end])) {
			end++
		}
		ext := tok[i+1 : end]
		if n := len(ext); n >= 1 && n <= 8 {
			for k := 0; k < n; k++ {
				if isAlpha(ext[k]) {
					return true
				}
			}
		}
	}
	return false
}

// lastSegmentHasExtension reports whether path's final segment carries an
// extension — evidence the line-number marker follows.
func lastSegmentHasExtension(path string) bool {
	seg := path
	if i := strings.LastIndexAny(path, "/\\"); i >= 0 {
		seg = path[i+1:]
	}
	return hasExtensionDot(seg)
}

// pathContinues reports whether the token after a marker still carries path
// structure (a separator or an extension dot) — evidence the digits were still
// inside the path, not the line-number marker.
func pathContinues(rest string) bool {
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return false
	}
	tok := fields[0]
	return strings.ContainsAny(tok, "/\\") || hasExtensionDot(tok)
}
