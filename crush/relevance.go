package crush

import (
	"regexp"
	"strings"
)

// fragileRe flags tokens that must never be paraphrased or dropped silently:
// hex addresses, ALLCAPS identifiers (ERROR, EOF), dotted paths
// (libsystem.dylib), multi-segment filesystem paths (/etc/passwd), CLI flags,
// and CamelCase symbols (IndexError). A sentence containing any of these is
// kept verbatim, mirroring headroom's must-keep set.
//
// Two deliberate exclusions keep the floor from pinning everything:
//   - bare numbers ("in 2024") — too common to be a signal.
//   - single-segment URL paths ("/health", "/login") — the path rule requires a
//     second segment, so generic HTTP endpoints in access logs do not pin every
//     line (a real false positive found in testing). Real filesystem paths and
//     multi-segment routes (/api/v1/users) still match.
var fragileRe = regexp.MustCompile(`0x[0-9a-fA-F]+|\b[A-Z]{2,}\b|\b[\w-]+\.[\w./-]+\b|(?:^|\s)/[\w.-]+/[\w./-]+|(?:^|\s)--?[a-zA-Z][\w-]+|[a-z][A-Z]`)

// looksFragile reports whether s contains a token that should be kept verbatim.
func looksFragile(s string) bool {
	return fragileRe.MatchString(s)
}

// errorMarkers are substrings that flag an item as worth keeping verbatim.
// Dropping an error the user is about to ask about is the catastrophic failure
// mode for a compressor, so this list is deliberately generous. The k8s-ish
// entries (crash, backoff, oomkilled, evicted) earn their place from live
// testing: "CrashLoopBackOff" matched nothing in the original list and
// survived only via signature dedup. Builders extend (never replace) this via
// Options.MustKeep — see MustKeepTerms.
var errorMarkers = []string{
	"error", "fail", "exception", "critical", "fatal",
	"panic", "timeout", "denied", "rejected", "invalid", "traceback",
	"crash", "backoff", "oomkilled", "evicted", "unhealthy", "degraded",
	// "warn" covers warn/warning/Warning — kubectl events flag problems as
	// TYPE=Warning, and dropping those from a triage sweep is exactly the
	// catastrophic-miss this floor exists to prevent (found live).
	"warn",
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

// hardErrorRe matches STRUCTURAL error signals — log-level tokens, stack
// traces, structured level fields, compiler error codes, k8s failure states —
// as opposed to the mere mention of the word "error" (a Go `error` return type,
// a line saying "0 errors"). looksError stays deliberately broad and is used as
// a soft ranking boost; looksHardError is the tighter test used where an item
// must be kept ABSOLUTELY, so a uniform array that all merely mentions "error"
// can still compress instead of pinning every row. Case-sensitive by design:
// loggers emit uppercase levels, and callers pass the ORIGINAL (not lowercased)
// text.
var hardErrorRe = regexp.MustCompile(strings.Join([]string{
	// Uppercase log levels + Kubernetes failure states (word-bounded).
	`\b(?:ERROR|FATAL|FAILED|FAILURE|PANIC|CRITICAL|EXCEPTION|SIGSEGV|SIGABRT|OOMKilled|CrashLoopBackOff|ImagePullBackOff|Evicted)\b`,
	// Anchored lowercase: "error:", "panic:", "error[E0433]", "traceback:".
	`(?i:\b(?:error|fatal|panic|exception|traceback|failed|failure|denied|rejected)\b\s*[:\[])`,
	// Line-start error/level keyword.
	`(?im:^\s*(?:error|fatal|panic|exception|traceback)\b)`,
	// Structured level fields: {"level":"error"} / level=fatal.
	`(?i:"level"\s*:\s*"(?:error|fatal|critical|panic)")`,
	`(?i:\blevel=(?:error|fatal|critical|panic)\b)`,
}, "|"))

// looksHardError reports whether s carries a structural error signal (see
// hardErrorRe). Pass the ORIGINAL text, not a lowercased copy — uppercase log
// levels are a deliberate signal.
func looksHardError(s string) bool {
	return hardErrorRe.MatchString(s)
}

// IsErrorLike reports whether s matches the built-in error floor — the terms
// compression never drops. Exported for feedback loops in host runtimes:
// a token already on the floor was KEPT, so it cannot be the reason a model
// retrieved offloaded content and makes a useless keep-pattern suggestion.
func IsErrorLike(s string) bool {
	return looksError(s)
}

// mustKeep reports whether s matches any caller-supplied must-keep term
// (already lowercased by NormalizeMustKeep). These are UNION semantics with
// the built-in errorMarkers — builder terms only ever add protection.
func mustKeep(s string, terms []string) bool {
	if len(terms) == 0 {
		return false
	}
	return matchesAny(strings.ToLower(s), terms)
}

// NormalizeMustKeep lowercases and trims caller-supplied must-keep terms,
// dropping empties, so per-item matching does no repeated work.
func NormalizeMustKeep(terms []string) []string {
	if len(terms) == 0 {
		return nil
	}
	out := make([]string, 0, len(terms))
	for _, t := range terms {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			out = append(out, t)
		}
	}
	return out
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
