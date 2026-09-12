package crush

import "testing"

// corpus: one single-term match, one two-term match, one unrelated doc.
func scorerCorpus() [][]string {
	return [][]string{
		{"the", "user", "alice", "logged", "in"},         // 0: matches "alice"
		{"alice", "opened", "the", "payment", "invoice"}, // 1: matches "alice","payment"
		{"a", "sunny", "afternoon", "in", "the", "park"}, // 2: matches nothing
	}
}

// TestHybrid_FloorsSingleTermMatchAboveUnmatched is the core fix: raw BM25
// under-scores a lone term match, so a matched sentence can rank at/near an
// unmatched one. The hybrid floor lifts any match clear of the zero-score noise.
func TestHybrid_FloorsSingleTermMatchAboveUnmatched(t *testing.T) {
	docs := scorerCorpus()
	terms := []string{"alice"}

	raw := BM25Scorer{}.Scores(terms, docs)
	hyb := NewHybridScorer().Scores(terms, docs)

	// Raw BM25 for a single common-ish term is small; the floor guarantees >=0.3.
	if hyb[0] < 0.3 {
		t.Fatalf("hybrid floor not applied: doc0 = %v (raw %v)", hyb[0], raw[0])
	}
	// Unmatched doc stays at zero under both.
	if hyb[2] != 0 {
		t.Fatalf("unmatched doc should score 0, got %v", hyb[2])
	}
	// The matched doc now clearly outranks the unmatched one.
	if hyb[0] <= hyb[2] {
		t.Fatal("matched doc did not outrank unmatched after boost")
	}
}

func TestHybrid_MultiTermMatchGetsBonus(t *testing.T) {
	docs := scorerCorpus()
	terms := []string{"alice", "payment"}

	hyb := NewHybridScorer().Scores(terms, docs)
	// doc1 matches both terms → floor + bonus; doc0 matches one → floor only.
	if hyb[1] <= hyb[0] {
		t.Fatalf("two-term match (%v) should outscore one-term match (%v)", hyb[1], hyb[0])
	}
	if hyb[1] < 0.3+0.2 {
		t.Fatalf("multi-match bonus not applied: %v", hyb[1])
	}
}

func TestHybrid_NeverLowersStrongBM25(t *testing.T) {
	// A rare, repeated term yields a raw BM25 well above the 1.0 cap; the boost
	// must not drag a strong two-term match down to the cap.
	docs := [][]string{
		{"zephyr", "zephyr", "quasar", "quasar"}, // strong on both rare terms
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

func TestHybrid_EmptyTermsAllZero(t *testing.T) {
	docs := scorerCorpus()
	for i, s := range NewHybridScorer().Scores(nil, docs) {
		if s != 0 {
			t.Fatalf("empty query should score 0, doc%d = %v", i, s)
		}
	}
}

func TestHybrid_Deterministic(t *testing.T) {
	docs := scorerCorpus()
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
	docs := scorerCorpus()
	before := docs[0][0]
	NewHybridScorer().Scores([]string{"alice"}, docs)
	if docs[0][0] != before {
		t.Fatal("scorer mutated the input docs")
	}
}
