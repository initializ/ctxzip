// Package detect classifies a blob of content so the router can pick the
// right compressor. Detection is a fast, dependency-free heuristic cascade in
// priority order, mirroring headroom's content_detector. It never parses the
// whole input where a prefix will do.
package detect

import (
	"encoding/json"
	"regexp"
	"strings"
)

// ContentType is the detected shape of a piece of content.
type ContentType string

const (
	JSONArray     ContentType = "json_array"
	GitDiff       ContentType = "git_diff"
	SearchResults ContentType = "search_results"
	YAMLLike      ContentType = "yaml_like"
	BuildLog      ContentType = "log"
	SourceCode    ContentType = "source_code"
	PlainText     ContentType = "plain_text"
)

// Detection is the result of classifying content.
type Detection struct {
	Type       ContentType
	Confidence float64 // 0..1
}

var (
	diffHeaderRe = regexp.MustCompile(`(?m)^(diff --git |--- a/|\+\+\+ b/|@@ )`)
	// searchRe matches grep-style "path:line:content". The prefix must look like
	// a file path — a no-space, no-colon token containing a "." "/" or "\" — so
	// that timestamped log lines like "2026-06-30T10:00:01 ..." (whose "10:00:"
	// also satisfies ":digits:") are NOT misread as search results and instead
	// fall through to the log rule.
	searchRe = regexp.MustCompile(`(?m)^[^\s:]*[./\\][^\s:]*:\d+:`)
	// yamlKeyRe matches YAML-ish "key:" lines — including kubectl describe's
	// near-YAML (Name:, Node:) and list items ("- name:"). Quoted keys
	// (pretty-printed JSON) are deliberately excluded by the leading
	// [A-Za-z_].
	yamlKeyRe = regexp.MustCompile(`^\s*(- )?[A-Za-z_][\w.\-/]*:(\s|$)`)
	logRe     = regexp.MustCompile(`(?i)\b(error|warn|warning|traceback|exception|fatal|panic)\b|^\s*at\s+\S+\(|^\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}`)
	codeRe    = regexp.MustCompile(`(?m)^\s*(def |func |class |import |from \S+ import |const |let |var |function |package |type \w+ struct|public |private )`)
)

// Detect classifies content. The cascade is ordered most-specific first; the
// first rule that clears its confidence floor wins, otherwise PlainText.
func Detect(content string) Detection {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return Detection{Type: PlainText, Confidence: 1}
	}

	if d, ok := detectJSONArray(trimmed); ok {
		return d
	}
	if diffHeaderRe.MatchString(firstLines(trimmed, 500)) {
		return Detection{Type: GitDiff, Confidence: 0.8}
	}
	if frac := lineMatchFraction(trimmed, searchRe); frac >= 0.3 {
		return Detection{Type: SearchResults, Confidence: frac}
	}
	// YAML before log: an events-heavy `kubectl describe` has enough Warning
	// lines to trip the log fraction, but it is structurally field-per-line
	// and folds far better as YAML.
	if strings.Count(trimmed, "\n") >= 14 {
		if frac := lineMatchFraction(trimmed, yamlKeyRe); frac >= 0.4 {
			return Detection{Type: YAMLLike, Confidence: frac}
		}
	}
	if frac := lineMatchFraction(trimmed, logRe); frac >= 0.1 {
		return Detection{Type: BuildLog, Confidence: 0.5 + frac/2}
	}
	if countMatches(trimmed, codeRe) >= 3 {
		return Detection{Type: SourceCode, Confidence: 0.6}
	}
	return Detection{Type: PlainText, Confidence: 1}
}

func detectJSONArray(s string) (Detection, bool) {
	if !strings.HasPrefix(s, "[") {
		return Detection{}, false
	}
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(s), &arr); err != nil {
		return Detection{}, false
	}
	return Detection{Type: JSONArray, Confidence: 1}, true
}

func firstLines(s string, n int) string {
	idx, count := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			count++
			if count >= n {
				idx = i
				break
			}
		}
	}
	if idx == 0 {
		return s
	}
	return s[:idx]
}

func lineMatchFraction(s string, re *regexp.Regexp) float64 {
	lines := strings.Split(s, "\n")
	if len(lines) == 0 {
		return 0
	}
	matched := 0
	for _, ln := range lines {
		if re.MatchString(ln) {
			matched++
		}
	}
	return float64(matched) / float64(len(lines))
}

func countMatches(s string, re *regexp.Regexp) int {
	return len(re.FindAllString(s, -1))
}
