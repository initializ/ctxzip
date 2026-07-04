package detect

import "testing"

func TestDetect(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    ContentType
	}{
		{"json array", `[{"a":1},{"a":2}]`, JSONArray},
		{"json object is not array", `{"a":1}`, PlainText},
		{"git diff", "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@", GitDiff},
		{"search results", "main.go:10:func main\nmain.go:11:  return\nutil.go:3:var x", SearchResults},
		{"log", "INFO starting\nERROR boom\nWARN careful\nException in thread", BuildLog},
		// Regression: ISO-timestamped logs must NOT be read as search results
		// just because "10:00:" satisfies the ":digits:" shape — they are logs.
		{"timestamped log not search", "2026-06-30T10:00:01 ERROR upstream timeout\n2026-06-30T10:00:02 ERROR retry failed\n2026-06-30T10:00:03 ERROR giving up", BuildLog},
		// A grep-style path prefix still detects as search.
		{"path-prefixed search", "src/app.go:42:return err\nsrc/app.go:43:}\npkg/db.go:8:var conn", SearchResults},
		{"code", "package main\nimport \"fmt\"\nfunc main() {\n  const x = 1\n}", SourceCode},
		{"prose", "The quick brown fox jumped over the lazy dog and ran away.", PlainText},
		{"empty", "", PlainText},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Detect(c.content).Type; got != c.want {
				t.Errorf("Detect(%q) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}
