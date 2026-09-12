package crush

// Scorer ranks documents against a query. A document is a token list (e.g. one
// tokenized sentence); the scorer sees the whole corpus at once so corpus
// statistics — IDF, average length — are available. Higher is more relevant.
//
// It is the relevance seam the extractive TextCrusher (and, later, the search
// crusher) selects on. Two implementations ship: BM25Scorer (raw keyword
// matching) and HybridScorer (the default — BM25 plus a matched-term boost).
// A future build-tagged embedding scorer implements the same interface and
// slots in as HybridScorer.Embed.
type Scorer interface {
	// Scores returns one score per document, aligned with docs. It must be
	// deterministic and must not mutate docs.
	Scores(terms []string, docs [][]string) []float64
}

// BM25Scorer is raw Okapi BM25 — the keyword-matching baseline.
type BM25Scorer struct{}

// Scores implements Scorer.
func (BM25Scorer) Scores(terms []string, docs [][]string) []float64 {
	out := make([]float64, len(docs))
	if len(terms) == 0 {
		return out
	}
	m := newBM25(docs)
	for i := range docs {
		out[i] = m.score(terms, i)
	}
	return out
}

// Hybrid boost defaults, ported from headroom's hybrid scorer's graceful-BM25
// fallback. Raw BM25 badly under-scores short single-term matches (BM25 of
// "alice" against {"name","alice"} is ~0.07). That hurts any consumer selecting
// by an absolute score THRESHOLD — the upcoming search crusher — where a real
// match can fall below the cutoff; the floor lifts every match clear of it. (In
// the current extractive TextCrusher, which keeps by budget then original order
// rather than a threshold, the floor is largely inert: there the only
// observable effect is multi-term matches being promoted under budget pressure.)
// So: a matched document is floored, and a document matching several distinct
// terms is nudged further up.
const (
	defaultMatchFloor      = 0.3
	defaultMultiMatchBonus = 0.2
	defaultScoreCap        = 1.0
)

// HybridScorer is the default relevance scorer: a Base scorer (BM25) plus a
// matched-term boost. The boost only ever RAISES a document that contains query
// terms — it never lowers a score and never touches an unmatched document, so
// it can only improve the ranking of relevant items, never promote noise:
//
//   - a document containing any query term scores at least MatchFloor;
//   - a document containing two or more DISTINCT query terms gets
//     MultiMatchBonus added, capped at ScoreCap.
//
// Embed is the extension seam for semantic relevance: when a build-tagged
// embedding Scorer is supplied, its score is fused with the base (that path is
// not yet wired — Embed is nil in the default build, and this is BM25+boost).
//
// Every field is optional: a zero Base defaults to BM25, and a zero MatchFloor /
// MultiMatchBonus / ScoreCap each falls back to its tuned default (see Scores),
// so a directly-constructed HybridScorer{} behaves exactly like
// NewHybridScorer() rather than silently degenerating to plain BM25.
type HybridScorer struct {
	Base            Scorer
	Embed           Scorer // nil until the embedding path lands; reserved
	MatchFloor      float64
	MultiMatchBonus float64
	ScoreCap        float64
}

// NewHybridScorer returns the default hybrid scorer (BM25 base, tuned boost).
func NewHybridScorer() HybridScorer {
	return HybridScorer{
		Base:            BM25Scorer{},
		MatchFloor:      defaultMatchFloor,
		MultiMatchBonus: defaultMultiMatchBonus,
		ScoreCap:        defaultScoreCap,
	}
}

// Scores implements Scorer.
func (h HybridScorer) Scores(terms []string, docs [][]string) []float64 {
	base := h.Base
	if base == nil {
		base = BM25Scorer{}
	}
	scores := base.Scores(terms, docs)
	if len(terms) == 0 {
		return scores
	}
	// Resolve tuned defaults for any zero field, so a directly-constructed
	// HybridScorer{} does not degenerate to plain BM25 (a zero ScoreCap would
	// otherwise clamp the bonus away, and a zero MatchFloor would disable it).
	matchFloor, multiBonus, scoreCap := h.MatchFloor, h.MultiMatchBonus, h.ScoreCap
	if matchFloor == 0 {
		matchFloor = defaultMatchFloor
	}
	if multiBonus == 0 {
		multiBonus = defaultMultiMatchBonus
	}
	if scoreCap == 0 {
		scoreCap = defaultScoreCap
	}

	termSet := make(map[string]struct{}, len(terms))
	for _, t := range terms {
		termSet[t] = struct{}{}
	}
	for i, doc := range docs {
		matched := distinctMatches(doc, termSet)
		if matched == 0 {
			continue // never touch an unmatched document
		}
		raw := scores[i]
		s := raw
		if s < matchFloor {
			s = matchFloor
		}
		if matched >= 2 {
			if s += multiBonus; s > scoreCap {
				s = scoreCap
			}
		}
		// Monotonic guard: the boost only ever raises. Without it, the cap
		// would LOWER a strong match whose raw BM25 already exceeds scoreCap.
		if s < raw {
			s = raw
		}
		scores[i] = s
	}
	return scores
}

// distinctMatches counts how many distinct query terms appear as tokens in doc.
func distinctMatches(doc []string, termSet map[string]struct{}) int {
	seen := make(map[string]struct{}, len(termSet))
	for _, tok := range doc {
		if _, ok := termSet[tok]; ok {
			seen[tok] = struct{}{}
		}
	}
	return len(seen)
}
