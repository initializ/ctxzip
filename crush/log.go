package crush

import (
	"fmt"
	"strings"

	"github.com/initializ/ctxzip/ccr"
)

// LogCrusher compresses line-oriented output (build logs, command output). It
// keeps error/warning lines, a head and tail window, and drops near-duplicate
// noise in the middle, offloading the dropped lines to the CCR store.
type LogCrusher struct {
	HeadKeep int
	TailKeep int
	MinLines int
}

// NewLogCrusher returns a LogCrusher with sensible defaults.
func NewLogCrusher() *LogCrusher {
	return &LogCrusher{HeadKeep: 10, TailKeep: 10, MinLines: 40}
}

// Name implements Compressor.
func (c *LogCrusher) Name() string { return "log_crusher" }

// Compress implements Compressor.
func (c *LogCrusher) Compress(req Request) (Result, error) {
	lines := strings.Split(req.Content, "\n")
	if len(lines) < c.MinLines || req.Store == nil {
		return passthrough(c.Name(), req.Content), nil
	}

	terms := queryTerms(req.Query)
	keep := make([]bool, len(lines))
	seen := make(map[string]bool, len(lines))

	for i, ln := range lines {
		if i < c.HeadKeep || i >= len(lines)-c.TailKeep {
			keep[i] = true
			continue
		}
		lower := strings.ToLower(ln)
		if looksError(lower) || mustKeep(lower, req.MustKeep) || matchesAny(lower, terms) {
			keep[i] = true
			continue
		}
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || seen[trimmed] {
			continue // drop blank/duplicate noise
		}
		seen[trimmed] = true
	}

	var kept, dropped []string
	for i, ln := range lines {
		if keep[i] {
			kept = append(kept, ln)
		} else {
			dropped = append(dropped, ln)
		}
	}
	if len(dropped) == 0 {
		return passthrough(c.Name(), req.Content), nil
	}

	droppedBlob := []byte(strings.Join(dropped, "\n"))
	hash := ccr.Hash(droppedBlob)
	if err := req.Store.Put(hash, droppedBlob, ccr.Meta{
		ToolName:     req.ToolName,
		Query:        req.Query,
		ItemCount:    len(dropped),
		OriginalKind: "log",
	}); err != nil {
		return passthrough(c.Name(), req.Content), nil
	}

	marker := ccr.Marker(hash, fmt.Sprintf("%d_lines_offloaded", len(dropped)))
	// Splice the marker where the dropped middle was: after the head window.
	head := kept[:min(c.HeadKeep, len(kept))]
	tail := kept[min(c.HeadKeep, len(kept)):]
	out := strings.Join(head, "\n") + "\n" + marker + "\n" + strings.Join(tail, "\n")
	return Result{Compressed: out, Strategy: c.Name(), Markers: []string{hash}}, nil
}
