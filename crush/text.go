package crush

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/initializ/ctxzip/ccr"
)

// TextCrusher compresses prose extractively: it keeps a subset of the original
// sentences verbatim and offloads the rest. It never paraphrases, so anything
// it keeps is exactly what was there — and anything it drops is reversible via
// the CCR store, like every other crusher.
//
// Two modes:
//   - With a query, sentences are ranked by BM25 relevance; the most relevant
//     ones (plus lead/tail and any fragile sentence) are kept up to a target.
//   - Without a query, it only removes near-duplicate sentences, so compression
//     is conservative when there is no signal about what matters.
type TextCrusher struct {
	// MinChars / MinSentences are the floors below which prose is left alone.
	MinChars     int
	MinSentences int
	// LeadKeep / TailKeep are sentences always kept at the start/end.
	LeadKeep int
	TailKeep int
	// TargetKeepRatio is roughly the fraction of sentences kept in query mode.
	TargetKeepRatio float64
	// DupThreshold is the Jaccard similarity above which a sentence is a near
	// duplicate (no-query mode).
	DupThreshold float64
}

// NewTextCrusher returns a TextCrusher with sensible defaults.
func NewTextCrusher() *TextCrusher {
	return &TextCrusher{
		MinChars:        400,
		MinSentences:    8,
		LeadKeep:        2,
		TailKeep:        1,
		TargetKeepRatio: 0.5,
		DupThreshold:    0.8,
	}
}

// Name implements Compressor.
func (c *TextCrusher) Name() string { return "text_extractive" }

// Compress implements Compressor.
func (c *TextCrusher) Compress(req Request) (Result, error) {
	if len(req.Content) < c.MinChars || req.Store == nil {
		return passthrough(c.Name(), req.Content), nil
	}
	sents, sep := splitSentences(req.Content)
	if len(sents) < c.MinSentences {
		return passthrough(c.Name(), req.Content), nil
	}

	lineMode := sep == "\n"
	keep := make([]bool, len(sents))
	kept := 0
	mark := func(i int) {
		if !keep[i] {
			keep[i] = true
			kept++
		}
	}

	// Forced keeps: lead, tail, and error-like sentences. The fragile-token
	// floor applies to prose only — in line mode (grep/log output) nearly
	// every line contains a path or identifier, so the floor would pin
	// everything and defeat compression; reversibility plus the error floor
	// carry the fidelity guarantee there instead.
	for i, s := range sents {
		if i < c.LeadKeep || i >= len(sents)-c.TailKeep {
			mark(i)
			continue
		}
		lower := strings.ToLower(s)
		if looksError(lower) || mustKeep(lower, req.MustKeep) {
			mark(i)
			continue
		}
		if !lineMode && looksFragile(s) {
			mark(i)
		}
	}

	terms := queryTerms(req.Query)
	switch {
	case lineMode:
		// Line-oriented content dedups by numeric-insensitive signature —
		// "pods.json:5: status: Running" and "pods.json:14: status: Running"
		// are the same information; the count survives in the marker note.
		// Query-matching lines are exact anchors and never dropped.
		c.selectByLineDedup(sents, terms, keep, mark)
	case len(terms) > 0:
		c.selectByRelevance(sents, terms, keep, mark, &kept)
	default:
		c.selectByDedup(sents, keep, mark)
	}

	var keptSents, dropped []string
	for i, s := range sents {
		if keep[i] {
			keptSents = append(keptSents, s)
		} else {
			dropped = append(dropped, s)
		}
	}
	if len(dropped) == 0 {
		return passthrough(c.Name(), req.Content), nil
	}

	// Join with the same separator the content was split on, so both the
	// stored original and the kept rendering preserve the source layout —
	// line-oriented content (logs, grep output) keeps its newlines.
	blob := []byte(strings.Join(dropped, sep))
	hash := ccr.Hash(blob)
	if err := req.Store.Put(hash, blob, ccr.Meta{
		ToolName:     req.ToolName,
		Query:        req.Query,
		ItemCount:    len(dropped),
		OriginalKind: "text",
	}); err != nil {
		return passthrough(c.Name(), req.Content), nil
	}

	unit := "sentences"
	if lineMode {
		unit = "lines"
	}
	marker := ccr.Marker(hash, fmt.Sprintf("%d_%s_offloaded", len(dropped), unit))
	out := strings.Join(keptSents, sep) + sep + marker
	// In line mode, a categorical trailer after the marker tells the model
	// WHAT was offloaded ("118 running, 20 completed"), letting it answer
	// count questions — and decide against expanding — without retrieval.
	if lineMode {
		if trailer := summarizeDroppedLines(dropped); trailer != "" {
			out += " " + trailer
		}
	}
	return Result{Compressed: out, Strategy: c.Name(), Markers: []string{hash}}, nil
}

// summarizeDroppedLines groups dropped lines by their dedup signature and
// renders the top groups with counts, labeled by the group's non-identifier
// words: "[offloaded: 118 running, 20 completed, 11 other]". Deterministic
// (count desc, label asc). Empty when no group yields a usable label.
func summarizeDroppedLines(dropped []string) string {
	type group struct {
		label string
		count int
	}
	counts := map[string]int{}
	labels := map[string]string{}
	for _, ln := range dropped {
		sig := lineSig(ln)
		counts[sig]++
		if _, ok := labels[sig]; !ok {
			labels[sig] = labelFromSig(sig)
		}
	}

	groups := make([]group, 0, len(counts))
	other := 0
	for sig, c := range counts {
		if labels[sig] == "" {
			other += c
			continue
		}
		groups = append(groups, group{labels[sig], c})
	}
	sort.Slice(groups, func(a, b int) bool {
		if groups[a].count != groups[b].count {
			return groups[a].count > groups[b].count
		}
		return groups[a].label < groups[b].label
	})
	if len(groups) == 0 {
		return ""
	}
	if len(groups) > 3 {
		for _, g := range groups[3:] {
			other += g.count
		}
		groups = groups[:3]
	}

	parts := make([]string, 0, 4)
	for _, g := range groups {
		parts = append(parts, fmt.Sprintf("%d %s", g.count, g.label))
	}
	if other > 0 {
		parts = append(parts, fmt.Sprintf("%d other", other))
	}
	return "[offloaded: " + strings.Join(parts, ", ") + "]"
}

// labelFromSig extracts a short human label from a dedup signature: the words
// that survived identifier collapsing (the information axis), capped at three
// tokens / 32 chars. "keda # # completed # #" → "keda completed".
func labelFromSig(sig string) string {
	words := make([]string, 0, 3)
	length := 0
	for _, tok := range strings.Fields(sig) {
		if tok == "#" || strings.Contains(tok, "#") {
			continue
		}
		if length+len(tok) > 32 {
			break
		}
		words = append(words, tok)
		length += len(tok) + 1
		if len(words) == 3 {
			break
		}
	}
	return strings.Join(words, " ")
}

// selectByRelevance keeps the highest-BM25 sentences up to the target budget,
// filling any shortfall by original order so it never over-drops.
func (c *TextCrusher) selectByRelevance(sents []string, terms []string, keep []bool, mark func(int), kept *int) {
	docs := make([][]string, len(sents))
	for i, s := range sents {
		docs[i] = tokenizeWords(s)
	}
	m := newBM25(docs)

	type scored struct {
		idx   int
		score float64
	}
	ranked := make([]scored, 0, len(sents))
	for i := range sents {
		if keep[i] {
			continue
		}
		ranked = append(ranked, scored{i, m.score(terms, i)})
	}
	sort.SliceStable(ranked, func(a, b int) bool { return ranked[a].score > ranked[b].score })

	budget := int(float64(len(sents)) * c.TargetKeepRatio)
	if budget < *kept {
		budget = *kept
	}
	// Add relevant (positive-score) sentences first.
	for _, r := range ranked {
		if *kept >= budget || r.score <= 0 {
			break
		}
		mark(r.idx)
	}
	// If still under budget, fill by original order so we keep ~budget total.
	for i := 0; i < len(sents) && *kept < budget; i++ {
		mark(i)
	}
}

// selectByLineDedup keeps the first line of each numeric-insensitive
// signature and every query-matching line, dropping repeats. Suited to
// grep/log output where thousands of lines differ only by line number,
// timestamp, or counter.
//
// Query terms that match most lines are ignored as stop-terms: when the
// query is a tool invocation (grep pattern + file path), terms like the
// file name match every result line and carry no selection signal — honoring
// them would pin everything and disable compression entirely (observed live).
func (c *TextCrusher) selectByLineDedup(sents []string, terms []string, keep []bool, mark func(int)) {
	terms = discriminativeTerms(sents, terms)
	seen := make(map[string]bool, len(sents))
	for i, s := range sents {
		if keep[i] {
			seen[lineSig(s)] = true
			continue
		}
		if len(terms) > 0 && matchesAny(strings.ToLower(s), terms) {
			mark(i)
			seen[lineSig(s)] = true
			continue
		}
		sig := lineSig(s)
		if seen[sig] {
			continue // drop numeric-variant repeat
		}
		seen[sig] = true
		mark(i)
	}
}

// discriminativeTerms drops query terms that match more than half the lines —
// they cannot distinguish relevant lines from noise.
func discriminativeTerms(sents []string, terms []string) []string {
	if len(terms) == 0 {
		return terms
	}
	out := make([]string, 0, len(terms))
	half := len(sents) / 2
	for _, t := range terms {
		matches := 0
		for _, s := range sents {
			if strings.Contains(strings.ToLower(s), t) {
				matches++
				if matches > half {
					break
				}
			}
		}
		if matches <= half {
			out = append(out, t)
		}
	}
	return out
}

var (
	// idTokenRe matches whitespace-delimited tokens carrying at least one
	// digit — pod names ("backup-job-29610720-sdmkc"), counters ("0/1"),
	// ages ("381d"), IPs, hashes. In infrastructure output these are the
	// identifier axis; the remaining words are the information axis.
	idTokenRe = regexp.MustCompile(`\S*\d\S*`)
	wsRunRe   = regexp.MustCompile(`\s+`)
)

// lineSig canonicalizes a line for dedup: lowercase, every digit-bearing
// token collapsed to '#', whitespace runs (column alignment) collapsed to
// one space. Two kubectl rows that differ only in pod name, ready count,
// restarts, and age therefore share a signature — "same information modulo
// identifiers" — while any row with a distinct status word (CrashLoopBackOff)
// keeps its own. Digit-only collapsing was not enough in live testing:
// k8s tables barely deduped because every pod NAME is a unique token.
func lineSig(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = idTokenRe.ReplaceAllString(s, "#")
	return wsRunRe.ReplaceAllString(s, " ")
}

// selectByDedup keeps every sentence except near-duplicates of one already kept.
func (c *TextCrusher) selectByDedup(sents []string, keep []bool, mark func(int)) {
	var keptToks [][]string
	for i := range sents {
		toks := tokenizeWords(sents[i])
		if keep[i] {
			keptToks = append(keptToks, toks)
			continue
		}
		if isNearDup(toks, keptToks, c.DupThreshold) {
			continue // drop redundant sentence
		}
		mark(i)
		keptToks = append(keptToks, toks)
	}
}

// splitSentences breaks text into verbatim units and reports the separator to
// rejoin them with.
//
// Line-oriented content (logs, grep output, anything with several newlines)
// splits on newlines ONLY and rejoins with "\n" — splitting such text on dots
// mangles tokens like "pods.json" and destroys the layout the model (and a
// later retrieval) depends on. Prose splits on terminal punctuation, but only
// when followed by whitespace, so "3.14" and "file.go" never break mid-token.
func splitSentences(text string) (sents []string, sep string) {
	if strings.Count(text, "\n") >= 4 {
		for _, ln := range strings.Split(text, "\n") {
			if strings.TrimSpace(ln) != "" {
				sents = append(sents, ln)
			}
		}
		return sents, "\n"
	}

	var b strings.Builder
	flush := func() {
		if s := strings.TrimSpace(b.String()); s != "" {
			sents = append(sents, s)
		}
		b.Reset()
	}
	runes := []rune(text)
	for i, r := range runes {
		b.WriteRune(r)
		switch r {
		case '\n':
			flush()
		case '.', '!', '?':
			// Only a real sentence boundary when followed by whitespace (or
			// end of text) — never split inside pods.json / v1.2 / 3.14.
			if i+1 == len(runes) || runes[i+1] == ' ' || runes[i+1] == '\t' || runes[i+1] == '\n' {
				flush()
			}
		}
	}
	flush()
	return sents, " "
}

func tokenizeWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_')
	})
}

// isNearDup reports whether toks is a near-duplicate (Jaccard ≥ threshold) of
// any document in corpus.
func isNearDup(toks []string, corpus [][]string, threshold float64) bool {
	if len(toks) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(toks))
	for _, t := range toks {
		set[t] = struct{}{}
	}
	for _, other := range corpus {
		if jaccard(set, other) >= threshold {
			return true
		}
	}
	return false
}

func jaccard(set map[string]struct{}, other []string) float64 {
	if len(set) == 0 || len(other) == 0 {
		return 0
	}
	otherSet := make(map[string]struct{}, len(other))
	for _, t := range other {
		otherSet[t] = struct{}{}
	}
	inter := 0
	for t := range set {
		if _, ok := otherSet[t]; ok {
			inter++
		}
	}
	union := len(set) + len(otherSet) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}
