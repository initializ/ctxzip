package crush

import "math"

// bm25 ranks short documents (here: sentences) against a query using Okapi
// BM25. It is the relevance signal behind the extractive TextCrusher: keep the
// sentences that actually answer what the user is asking, drop the rest. Pure,
// dependency-free, and deterministic.
type bm25 struct {
	tf     []map[string]int
	docLen []int
	df     map[string]int
	n      int
	avgLen float64
	k1, b  float64
}

func newBM25(docs [][]string) *bm25 {
	m := &bm25{
		n:      len(docs),
		df:     make(map[string]int),
		tf:     make([]map[string]int, len(docs)),
		docLen: make([]int, len(docs)),
		k1:     1.5,
		b:      0.75,
	}
	total := 0
	for i, d := range docs {
		tf := make(map[string]int, len(d))
		for _, t := range d {
			tf[t]++
		}
		m.tf[i] = tf
		m.docLen[i] = len(d)
		total += len(d)
		for t := range tf {
			m.df[t]++
		}
	}
	if m.n > 0 {
		m.avgLen = float64(total) / float64(m.n)
	}
	return m
}

// score returns the BM25 score of document idx against the query terms.
func (m *bm25) score(query []string, idx int) float64 {
	if m.avgLen == 0 {
		return 0
	}
	tf := m.tf[idx]
	dl := float64(m.docLen[idx])
	var s float64
	for _, q := range query {
		f := float64(tf[q])
		if f == 0 {
			continue
		}
		df := float64(m.df[q])
		idf := math.Log(1 + (float64(m.n)-df+0.5)/(df+0.5))
		s += idf * (f * (m.k1 + 1)) / (f + m.k1*(1-m.b+m.b*dl/m.avgLen))
	}
	return s
}
