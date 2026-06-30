// Package tokenize provides approximate token counting.
//
// ctxzip does not ship an exact tokenizer. Anthropic and Google expose no
// public tokenizer, and pulling in tiktoken would add a heavyweight dependency
// for a number that only needs to be directionally correct for budgeting and
// savings reporting. Estimate deliberately errs slightly high so callers never
// under-count context. A forge adapter that wants exact counts can substitute
// the real provider UsageInfo after the call.
package tokenize

import "unicode"

// Average characters per token for the supported scripts. Latin/code text
// tokenizes at roughly 4 chars/token; dense scripts (CJK, Kana, Hangul)
// tokenize far denser, closer to 1.5 chars/token, so they are priced
// per-character.
const (
	charsPerToken    = 4.0
	denseCharsPerTok = 1.5
)

// Estimate returns an approximate token count for s.
func Estimate(s string) int {
	if s == "" {
		return 0
	}
	var latin, dense int
	for _, r := range s {
		if isDense(r) {
			dense++
		} else {
			latin++
		}
	}
	tokens := float64(latin)/charsPerToken + float64(dense)/denseCharsPerTok
	if tokens < 1 {
		return 1
	}
	return int(tokens + 0.5)
}

func isDense(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}
