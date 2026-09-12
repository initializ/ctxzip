package crush

import (
	"math"
	"testing"
)

// fixedScorer is a stub Base that returns controlled scores, so the hybrid
// boost math can be asserted exactly without depending on BM25's numerics
// (which, on a tiny corpus, run high enough to make the floor/bonus inert).
type fixedScorer struct{ vals []float64 }

func (f fixedScorer) Scores(_ []string, _ [][]string) []float64 {
	return append([]float64(nil), f.vals...)
}

func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// boostDocs are crafted so, for terms {"alice","payment"}: doc0 matches one
// term, doc1 and doc2 match two, doc3 matches none.
func boostDocs() [][]string {
	return [][]string{
		{"alice", "logged", "in"},    // 0: 1 match
		{"alice", "payment", "sent"}, // 1: 2 matches
		{"payment", "to", "alice"},   // 2: 2 matches
		{"a", "sunny", "day"},        // 3: 0 matches
	}
}

// TestHybrid_BoostMathExact pins every branch of the boost: floor, bonus,
// cap+monotonic-guard, and unmatched-untouched.
func TestHybrid_BoostMathExact(t *testing.T) {
	docs := boostDocs()
	terms := []string{"alice", "payment"}
	// raw: doc0 below floor, doc1 mid-range, doc2 already above the cap, doc3 any.
	base := fixedScorer{vals: []float64{0.05, 0.4, 2.0, 0.9}}
	got := HybridScorer{Base: base}.Scores(terms, docs)

	want := []float64{
		0.3, // doc0: 1 match, 0.05 < floor -> floored to 0.3
		0.6, // doc1: 2 matches, 0.4 -> +0.2 = 0.6 (< cap)
		2.0, // doc2: 2 matches, 2.0 -> +0.2 = 2.2 > cap 1.0 -> 1.0, guard restores 2.0
		0.9, // doc3: 0 matches -> untouched
	}
	for i := range want {
		if !approxEq(got[i], want[i]) {
			t.Fatalf("doc%d: got %v, want %v", i, got[i], want[i])
		}
	}
}

// TestHybrid_ZeroValueAppliesDefaults guards the footgun: a HybridScorer with
// all boost fields zero (as the search crusher will construct it) must apply
// the tuned defaults, not degenerate to plain BM25.
func TestHybrid_ZeroValueAppliesDefaults(t *testing.T) {
	docs := boostDocs()
	terms := []string{"alice", "payment"}
	base := fixedScorer{vals: []float64{0.05, 0.4, 2.0, 0.9}}

	zero := HybridScorer{Base: base}.Scores(terms, docs)
	tuned := HybridScorer{Base: base, MatchFloor: 0.3, MultiMatchBonus: 0.2, ScoreCap: 1.0}.Scores(terms, docs)
	raw := base.Scores(terms, docs)

	for i := range zero {
		if !approxEq(zero[i], tuned[i]) {
			t.Fatalf("zero-value diverges from tuned at doc%d: %v vs %v", i, zero[i], tuned[i])
		}
	}
	// It must genuinely boost — doc0 (floored) and doc1 (bonus) differ from raw.
	if approxEq(zero[0], raw[0]) || approxEq(zero[1], raw[1]) {
		t.Fatal("zero-value HybridScorer degenerated to plain BM25")
	}
}

// TestHybrid_NeverLowersStrongBM25 exercises the guard on real BM25 numbers: a
// rare, repeated two-term match scores above the cap and must not be dragged
// down to it.
func TestHybrid_NeverLowersStrongBM25(t *testing.T) {
	docs := [][]string{
		{"zephyr", "zephyr", "quasar", "quasar"},
		{"ordinary", "filler", "words", "here"},
	}
	terms := []string{"zephyr", "quasar"}
	raw := BM25Scorer{}.Scores(terms, docs)
	hyb := NewHybridScorer().Scores(terms, docs)
	if raw[0] <= 1.0 {
		t.Skipf("test assumes raw BM25 > cap; got %v", raw[0])
	}
	if hyb[0] < raw[0] {
		t.Fatalf("boost lowered a strong match: raw %v -> hybrid %v", raw[0], hyb[0])
	}
}

// TestHybrid_RanksMatchedOverUnmatched is an end-to-end sanity check on real
// BM25: a matched sentence outranks an unmatched one, and a two-term match
// outranks a one-term match.
func TestHybrid_RanksMatchedOverUnmatched(t *testing.T) {
	docs := [][]string{
		{"the", "user", "alice", "logged", "in"},
		{"alice", "opened", "the", "payment", "invoice"},
		{"a", "sunny", "afternoon", "in", "the", "park"},
	}
	s := NewHybridScorer().Scores([]string{"alice", "payment"}, docs)
	if s[0] <= s[2] {
		t.Fatal("matched doc did not outrank unmatched")
	}
	if s[1] <= s[0] {
		t.Fatal("two-term match did not outrank one-term match")
	}
	if s[2] != 0 {
		t.Fatalf("unmatched doc should score 0, got %v", s[2])
	}
}

func TestHybrid_EmptyTermsAllZero(t *testing.T) {
	docs := boostDocs()
	for i, s := range NewHybridScorer().Scores(nil, docs) {
		if s != 0 {
			t.Fatalf("empty query should score 0, doc%d = %v", i, s)
		}
	}
}

func TestHybrid_Deterministic(t *testing.T) {
	docs := boostDocs()
	terms := []string{"alice", "payment"}
	a := NewHybridScorer().Scores(terms, docs)
	b := NewHybridScorer().Scores(terms, docs)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("non-deterministic score at doc%d: %v vs %v", i, a[i], b[i])
		}
	}
}

// TestHybrid_DoesNotMutateInput guards the Scorer contract.
func TestHybrid_DoesNotMutateInput(t *testing.T) {
	docs := boostDocs()
	before := docs[0][0]
	NewHybridScorer().Scores([]string{"alice"}, docs)
	if docs[0][0] != before {
		t.Fatal("scorer mutated the input docs")
	}
}
