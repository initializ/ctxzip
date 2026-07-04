// Package ccr implements Compress-Cache-Retrieve: the reversibility layer that
// makes ctxzip lossless end-to-end. When a compressor drops content it hashes
// the original, Puts it in a Store, and leaves an inline marker pointing back
// to it. The model (or a forge retrieve tool) can fetch the original by hash.
package ccr

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
)

// MarkerPrefix opens every ctxzip marker.
const MarkerPrefix = "<<ctxzip:"

// hashHexLen is how many hex chars of the SHA-256 digest form the store key.
// 12 hex chars = 48 bits — ample for a ≤1000-entry, 30-minute cache, and
// short enough that an LLM transcribing the hash from a marker into a
// retrieval tool call doesn't fumble it (live testing showed models truncate
// or mangle 24-hex hashes).
const hashHexLen = 12

var markerRe = regexp.MustCompile(`<<ctxzip:([0-9a-f]{12,64})(?:[ ,][^>]*)?>>`)

// Hash returns the content-addressed key for b: the first 24 hex chars of its
// SHA-256 digest. The same bytes always produce the same key, which is what
// lets a marker and its store entry stay in sync.
func Hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:hashHexLen]
}

// Marker renders the inline pointer left in place of dropped content, e.g.
//
//	Marker("abc123…", "480_rows_offloaded") => "<<ctxzip:abc123… 480_rows_offloaded>>"
//
// The note is human/LLM-readable and helps the model decide whether to
// retrieve and what to search for. It is not parsed back out.
func Marker(hash, note string) string {
	if note == "" {
		return fmt.Sprintf("%s%s>>", MarkerPrefix, hash)
	}
	return fmt.Sprintf("%s%s %s>>", MarkerPrefix, hash, note)
}

// ExtractHashes returns every ctxzip marker hash found in s, in order.
func ExtractHashes(s string) []string {
	matches := markerRe.FindAllStringSubmatch(s, -1)
	if matches == nil {
		return nil
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out
}

// HasMarker reports whether s contains at least one ctxzip marker.
func HasMarker(s string) bool {
	return markerRe.MatchString(s)
}
