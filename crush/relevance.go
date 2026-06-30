package crush

import (
	"regexp"
	"strings"
)

// fragileRe flags tokens that must never be paraphrased or dropped silently:
// hex addresses, ALLCAPS identifiers (ERROR, EOF), dotted paths
// (libsystem.dylib), unix paths, CLI flags, and CamelCase symbols
// (IndexError). A sentence containing any of these is kept verbatim, mirroring
// headroom's must-keep set. Bare numbers are intentionally excluded — "in 2024"
// should not pin a whole sentence.
var fragileRe = regexp.MustCompile(`0x[0-9a-fA-F]+|\b[A-Z]{2,}\b|\b[\w-]+\.[\w./-]+\b|(?:^|\s)/[\w./-]+|(?:^|\s)--?[a-zA-Z][\w-]+|[a-z][A-Z]`)

// looksFragile reports whether s contains a token that should be kept verbatim.
func looksFragile(s string) bool {
	return fragileRe.MatchString(s)
}

// errorMarkers are substrings that flag an item as worth keeping verbatim.
// Dropping an error the user is about to ask about is the catastrophic failure
// mode for a compressor, so this list is deliberately generous.
var errorMarkers = []string{
	"error", "fail", "exception", "critical", "fatal",
	"panic", "timeout", "denied", "rejected", "invalid", "traceback",
}

// looksError reports whether s mentions an error-like term. Case-insensitive.
func looksError(s string) bool {
	lower := strings.ToLower(s)
	for _, m := range errorMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// queryTerms splits a user query into lowercase terms worth matching on,
// dropping very short tokens that would match everything.
func queryTerms(query string) []string {
	if query == "" {
		return nil
	}
	fields := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-' || r == '/')
	})
	out := fields[:0]
	for _, f := range fields {
		// Keep terms of 3+ chars, plus short tokens that carry a digit
		// (version/id-like, e.g. "v2", "id7") which are high-signal for matching.
		if len(f) >= 3 || hasDigit(f) {
			out = append(out, f)
		}
	}
	return out
}

func hasDigit(s string) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}

func matchesAny(lower string, terms []string) bool {
	for _, t := range terms {
		if strings.Contains(lower, t) {
			return true
		}
	}
	return false
}
