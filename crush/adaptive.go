package crush

// adaptiveKeepCount decides how many items are worth keeping given how redundant
// they are: keep everything when there are few; collapse to the number of
// distinct groups when nearly all are duplicates; otherwise scale between 30%
// and 100% of n by diversity. Result is clamped to [minK, maxK].
//
// It is a simplified port of headroom's compute_optimal_k. It keeps the
// redundancy-adaptive sizing — the dominant behavior, and the reason a fixed cap
// over-keeps on repetitive results and under-keeps on diverse ones — but omits
// headroom's Kneedle knee-detection and zlib-ratio validation refinements, which
// its adaptive_sizer layers on top. Uniqueness is measured with the same
// digit/whitespace-insensitive line signature the text crusher dedups on, so
// "same match modulo identifiers" collapses to one group.
func adaptiveKeepCount(items []string, minK, maxK int) int {
	n := len(items)
	if n <= 8 {
		return n // fast path: too few to bother sizing
	}
	unique := countUniqueSignatures(items)
	if unique <= 3 {
		// Near-total redundancy: keep just the distinct groups.
		return clampInt(max(minK, unique), minK, maxK)
	}
	// diversity in (0,1]: fraction of items that are genuinely distinct.
	diversity := float64(unique) / float64(n)
	keep := max(minK, int(float64(n)*(0.3+0.7*diversity)))
	return clampInt(keep, minK, maxK)
}

// countUniqueSignatures counts distinct near-duplicate groups among items, using
// the shared numeric/whitespace-insensitive line signature.
func countUniqueSignatures(items []string) int {
	seen := make(map[string]struct{}, len(items))
	for _, it := range items {
		seen[lineSig(it)] = struct{}{}
	}
	return len(seen)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		v = lo
	}
	if v > hi {
		v = hi
	}
	return v
}
