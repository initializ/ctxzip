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
