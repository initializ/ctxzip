package crush

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/initializ/ctxzip/ccr"
)

// JSONCrusher compresses a JSON array of objects — the single most common
// bulky tool output (list APIs, query results, log arrays). Its strategy,
// ported from headroom's SmartCrusher, keeps the items that matter and
// offloads the rest:
//
//   - keep a head and tail slice (callers usually care about boundaries),
//   - keep any item that looks like an error,
//   - keep any item matching the user's query,
//   - drop exact duplicates,
//   - offload everything dropped to the CCR store as one blob, replaced by a
//     single sentinel object carrying the retrieval marker and a summary.
//
// The output is always valid JSON.
type JSONCrusher struct {
	// HeadKeep / TailKeep are how many leading/trailing items to always keep.
	HeadKeep int
	TailKeep int
	// MinItems is the array size below which compression is not worth it.
	MinItems int
}

// NewJSONCrusher returns a JSONCrusher with sensible defaults.
func NewJSONCrusher() *JSONCrusher {
	return &JSONCrusher{HeadKeep: 5, TailKeep: 3, MinItems: 8}
}

// Name implements Compressor.
func (c *JSONCrusher) Name() string { return "json_crusher" }

// Compress implements Compressor.
func (c *JSONCrusher) Compress(req Request) (Result, error) {
	var items []json.RawMessage
	if err := json.Unmarshal([]byte(req.Content), &items); err != nil {
		// Not a JSON array after all — leave it for another strategy.
		return passthrough(c.Name(), req.Content), nil
	}
	if len(items) < c.MinItems || req.Store == nil {
		return passthrough(c.Name(), req.Content), nil
	}

	terms := queryTerms(req.Query)
	keep := make([]bool, len(items))
	seen := make(map[string]bool, len(items))

	for i, raw := range items {
		// Always keep head and tail.
		if i < c.HeadKeep || i >= len(items)-c.TailKeep {
			keep[i] = true
			continue
		}
		s := strings.ToLower(string(raw))
		// Drop exact duplicates of content already kept or seen.
		if seen[s] {
			continue
		}
		seen[s] = true
		// Must-keep: error-like, builder-flagged, or query-relevant items.
		if looksError(s) || mustKeep(s, req.MustKeep) || matchesAny(s, terms) {
			keep[i] = true
		}
	}

	kept := make([]json.RawMessage, 0, len(items))
	dropped := make([]json.RawMessage, 0, len(items))
	for i, raw := range items {
		if keep[i] {
			kept = append(kept, raw)
		} else {
			dropped = append(dropped, raw)
		}
	}
	if len(dropped) == 0 {
		return passthrough(c.Name(), req.Content), nil
	}

	droppedBytes, err := json.Marshal(dropped)
	if err != nil {
		return passthrough(c.Name(), req.Content), nil
	}
	hash := ccr.Hash(droppedBytes)
	if err := req.Store.Put(hash, droppedBytes, ccr.Meta{
		ToolName:     req.ToolName,
		Query:        req.Query,
		ItemCount:    len(dropped),
		OriginalKind: "json_array",
	}); err != nil {
		return passthrough(c.Name(), req.Content), nil
	}

	marker := ccr.Marker(hash, fmt.Sprintf("%d_rows_offloaded", len(dropped)))
	sentinel := map[string]string{
		"_ctxzip_dropped": marker,
		"_summary":        summarize(dropped),
	}
	out, err := renderArray(kept, sentinel)
	if err != nil {
		return passthrough(c.Name(), req.Content), nil
	}
	return Result{Compressed: out, Strategy: c.Name(), Markers: []string{hash}}, nil
}

// renderArray emits kept items plus the sentinel as one valid JSON array.
func renderArray(kept []json.RawMessage, sentinel map[string]string) (string, error) {
	// Encode the sentinel with HTML-escaping off so the "<<ctxzip:…>>" marker
	// survives verbatim — both the retrieval regex and the LLM must read it.
	var sb bytes.Buffer
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(sentinel); err != nil {
		return "", err
	}
	sentinelBytes := bytes.TrimRight(sb.Bytes(), "\n")

	var b strings.Builder
	b.WriteByte('[')
	for i, raw := range kept {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(raw)
	}
	if len(kept) > 0 {
		b.WriteByte(',')
	}
	b.Write(sentinelBytes)
	b.WriteByte(']')
	return b.String(), nil
}

// categoryFields are probed, in order, for a scalar value to categorize
// dropped items by — the fields whose value distribution answers "what was
// offloaded?" without retrieving it. A field whose value is an object gets
// one nested probe (kubectl pods: status is an object, status.phase is the
// signal).
var categoryFields = []string{"status", "state", "phase", "level", "severity", "type", "reason", "result", "kind"}

// summarize renders the sentinel's human/LLM-readable description of what was
// offloaded. When the dropped items share a scalar category field, the value
// distribution is included — "118 Running, 2 Succeeded" lets the model answer
// count questions (and decide whether to expand) without retrieving anything.
func summarize(dropped []json.RawMessage) string {
	errs := 0
	for _, d := range dropped {
		if looksError(strings.ToLower(string(d))) {
			errs++
		}
	}

	field, counts := categorize(dropped)
	parts := make([]string, 0, 2)
	if field != "" {
		parts = append(parts, field+": "+renderCounts(counts, len(dropped)))
	}
	if errs > 0 {
		parts = append(parts, fmt.Sprintf("%d error-like", errs))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("%d items offloaded — retrieve to inspect", len(dropped))
	}
	return fmt.Sprintf("%d items offloaded (%s) — retrieve to inspect", len(dropped), strings.Join(parts, "; "))
}

// categorize finds the first category field with scalar values in at least
// half the dropped items and returns its name (dotted when nested) plus the
// value counts. Returns "" when no field qualifies.
func categorize(dropped []json.RawMessage) (string, map[string]int) {
	for _, f := range categoryFields {
		counts := map[string]int{}
		name := f
		for _, d := range dropped {
			var obj map[string]any
			if json.Unmarshal(d, &obj) != nil {
				continue
			}
			v, ok := obj[f]
			if !ok {
				continue
			}
			// One nested probe: an object-valued category field (kubectl's
			// status) is searched for the same scalar fields inside.
			if inner, isObj := v.(map[string]any); isObj {
				for _, nf := range categoryFields {
					if nv, nok := inner[nf]; nok {
						if s, isStr := nv.(string); isStr {
							counts[s]++
							name = f + "." + nf
						}
						break
					}
				}
				continue
			}
			if s, isStr := v.(string); isStr {
				counts[s]++
			}
		}
		total := 0
		for _, c := range counts {
			total += c
		}
		if total*2 >= len(dropped) {
			return name, counts
		}
	}
	return "", nil
}

// renderCounts renders value counts sorted by count desc (label asc on ties),
// capping at four values plus a remainder.
func renderCounts(counts map[string]int, total int) string {
	type vc struct {
		v string
		c int
	}
	sorted := make([]vc, 0, len(counts))
	shown := 0
	for v, c := range counts {
		sorted = append(sorted, vc{v, c})
	}
	sort.Slice(sorted, func(a, b int) bool {
		if sorted[a].c != sorted[b].c {
			return sorted[a].c > sorted[b].c
		}
		return sorted[a].v < sorted[b].v
	})
	if len(sorted) > 4 {
		sorted = sorted[:4]
	}
	parts := make([]string, 0, len(sorted)+1)
	for _, e := range sorted {
		parts = append(parts, fmt.Sprintf("%d %s", e.c, e.v))
		shown += e.c
	}
	if rest := total - shown; rest > 0 {
		parts = append(parts, fmt.Sprintf("%d other", rest))
	}
	return strings.Join(parts, ", ")
}
