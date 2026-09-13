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

// applyGlobalCap trims the lowest-scored non-protected, non-anchor matches in
// kept files until the kept total is within MaxTotalMatches.
func (c *SearchCrusher) applyGlobalCap(files []*searchFile) {
	var kept, trimmable []*searchMatch
	for _, f := range files {
		if !f.keep {
			continue
		}
		for _, m := range f.matches {
			if !m.keep {
				continue
			}
			kept = append(kept, m)
			if !m.protected && !m.anchor {
				trimmable = append(trimmable, m)
			}
		}
	}
	over := len(kept) - c.MaxTotalMatches
	if over <= 0 {
		return
	}
	// Drop the lowest-scored trimmable matches first.
	sortByScore(trimmable) // desc
	for i := 0; i < over && i < len(trimmable); i++ {
		trimmable[len(trimmable)-1-i].keep = false
	}
}

// emitOffload stores a file's dropped matches (full "path:line:content" lines,
// line order) and appends the marker under the file's group.
func (c *SearchCrusher) emitOffload(req Request, out []string, path string, dropped []*searchMatch, markers *[]string) []string {
	blob := fullLines(path, dropped)
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
		lines = append(lines, fullLines(f.path, f.matches))
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

// fullLines renders matches back to their original "path:line:content" form,
// line-number ascending, so an offloaded blob is a faithful slice of the input.
func fullLines(path string, matches []*searchMatch) string {
	sorted := append([]*searchMatch(nil), matches...)
	sort.SliceStable(sorted, func(a, b int) bool { return sorted[a].line < sorted[b].line })
	lines := make([]string, len(sorted))
	for i, m := range sorted {
		lines[i] = path + ":" + strconv.Itoa(m.line) + ":" + m.content
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

// searchMatch is one "path:line:content" hit.
type searchMatch struct {
	line      int
	content   string
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

// parseSearch splits grep-style output into files. ok is false unless every
// non-trailing line is a "path:line:content" match — mixed content (context
// lines, separators, prose) is left for another strategy, so nothing is ever
// silently dropped. total is the match count.
func parseSearch(content string) (files []*searchFile, total int, ok bool) {
	lines := strings.Split(content, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1] // trailing newline
	}
	if len(lines) == 0 {
		return nil, 0, false
	}
	byPath := make(map[string]*searchFile)
	for _, ln := range lines {
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
		f.matches = append(f.matches, &searchMatch{line: num, content: body})
		total++
	}
	return files, total, true
}

// parseMatchLine parses "path:line:content". The path must contain a path
// separator or dot (matching the detector), must not be empty, and the line
// field must be all digits.
func parseMatchLine(ln string) (path string, num int, content string, ok bool) {
	i := strings.IndexByte(ln, ':')
	if i <= 0 {
		return "", 0, "", false
	}
	path = ln[:i]
	if !strings.ContainsAny(path, "./\\") {
		return "", 0, "", false
	}
	rest := ln[i+1:]
	j := strings.IndexByte(rest, ':')
	if j <= 0 {
		return "", 0, "", false
	}
	num, err := strconv.Atoi(rest[:j])
	if err != nil {
		return "", 0, "", false
	}
	return path, num, rest[j+1:], true
}
