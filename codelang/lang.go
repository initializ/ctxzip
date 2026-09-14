package codelang

import (
	"regexp"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/c"
	"github.com/smacker/go-tree-sitter/cpp"
	"github.com/smacker/go-tree-sitter/java"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/python"
	tsx "github.com/smacker/go-tree-sitter/typescript/typescript"
)

// language couples a tree-sitter grammar with the node types whose function
// bodies this crusher elides. funcTypes are the definition nodes; bodyTypes are
// the valid body node types found via the "body" field. usesBraces is false for
// Python (its body is an indented block with no braces), true for the rest.
type language struct {
	name       string
	grammar    func() *sitter.Language
	funcTypes  map[string]bool
	bodyTypes  map[string]bool
	usesBraces bool
	// signals decides candidacy: does this look like the language at all?
	signals *regexp.Regexp
	// strong matches EXCLUSIVE markers — syntax a superset dialect has but its
	// base does not (TS type annotations, C++ scope resolution). A hit here
	// ranks the dialect ahead of its base, so TS beats JS and C++ beats C only
	// when the dialect's own syntax is actually present, not merely on shared
	// tokens like `class`. nil for the base languages (JS, C).
	strong *regexp.Regexp
}

func set(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// languages is the elision/detection registry.
var languages = []language{
	{
		name: "typescript", grammar: tsx.GetLanguage, usesBraces: true,
		funcTypes: set("function_declaration", "method_definition", "generator_function_declaration", "method_signature"),
		bodyTypes: set("statement_block"),
		// Candidacy requires a TS-specific marker (not plain function/const, which
		// are shared with JS) so a pure-JS file never becomes a TS candidate.
		signals: regexp.MustCompile(`\binterface\s+\w|:\s*(string|number|boolean|void|any)\b|\btype\s+\w+\s*=|\bimplements\b|\benum\s+\w|\bexport\s+(type|interface|enum)\b|\breadonly\s|\bas\s+\w`),
		strong:  regexp.MustCompile(`\binterface\s+\w|:\s*(string|number|boolean|void|any)\b|\btype\s+\w+\s*=|\bimplements\b|\benum\s+\w|\bexport\s+(type|interface|enum)\b|\breadonly\s`),
	},
	{
		name: "javascript", grammar: javascript.GetLanguage, usesBraces: true,
		funcTypes: set("function_declaration", "method_definition", "generator_function_declaration"),
		bodyTypes: set("statement_block"),
		signals:   regexp.MustCompile(`(?m)\bfunction\s|\bconst\s|\blet\s|=>|\brequire\(|\bmodule\.exports\b|\bconsole\.log\b|===`),
		// base language: no exclusive markers (TS is the specific dialect).
	},
	{
		name: "cpp", grammar: cpp.GetLanguage, usesBraces: true,
		funcTypes: set("function_definition"),
		bodyTypes: set("compound_statement"),
		// No bare #include here (shared with C); C++ candidacy needs C++-ish syntax.
		signals: regexp.MustCompile(`\bstd::|::\w|\btemplate\s*<|\bnamespace\s|\bnullptr\b|\bcout\b|\bclass\s+\w`),
		strong:  regexp.MustCompile(`\bstd::|\w+::\w|\btemplate\s*<|\bnullptr\b|\bcout\b|\bcin\b|\bnamespace\s+\w`),
	},
	{
		name: "c", grammar: c.GetLanguage, usesBraces: true,
		funcTypes: set("function_definition"),
		bodyTypes: set("compound_statement"),
		signals:   regexp.MustCompile(`(?m)#include|\bint\s+main\b|\bprintf\b|\bstruct\s+\w|\btypedef\b|\bmalloc\b|->`),
		// base language: C++ is the specific dialect.
	},
	{
		name: "java", grammar: java.GetLanguage, usesBraces: true,
		funcTypes: set("method_declaration", "constructor_declaration"),
		bodyTypes: set("block", "constructor_body"),
		signals:   regexp.MustCompile(`\bpublic\s+class\b|\bpackage\s+\w|\bimport\s+java|@Override\b|\bSystem\.|\bpublic\s+static\s+void\s+main|\bclass\s+\w+\s*\{`),
		strong:    regexp.MustCompile(`\bpublic\s+class\b|\bpackage\s+[\w.]+\s*;|\bimport\s+java|@\w+|\bSystem\.(out|err)\b|\bpublic\s+static\s+void\s+main`),
	},
	{
		name: "python", grammar: python.GetLanguage, usesBraces: false,
		funcTypes: set("function_definition"),
		bodyTypes: set("block"),
		signals:   regexp.MustCompile(`(?m)^\s*def\s+\w+\s*\(|^\s*class\s+\w+.*:|^\s*(from\s+\w+\s+)?import\s+\w|\belif\b|\bself\b`),
		strong:    regexp.MustCompile(`(?m)^\s*def\s+\w+\s*\(.*\)\s*:|^\s*class\s+\w+.*:|\belif\b|\bself\b|\b__\w+__\b`),
	},
}

// detectCandidates returns the languages whose signals fire in content, ordered
// by a score that ranks a specific dialect ahead of its base ONLY when the
// dialect's own exclusive syntax is present: score = strongHits*1000 +
// totalHits. So a TypeScript file (which also matches many JavaScript signals)
// is tried as TS before JS because its `interface`/type-annotation strong hits
// dominate, but a plain Java or C file — whose only cross-signal is a shared
// `class`/`#include` — is not hijacked by C++'s weight. The parse-verify step in
// the crusher then confirms the winner.
func detectCandidates(content string) []language {
	type scored struct {
		lang        language
		strong, all int
	}
	var hits []scored
	for _, l := range languages {
		all := len(l.signals.FindAllStringIndex(content, -1))
		if all == 0 {
			continue
		}
		strong := 0
		if l.strong != nil {
			strong = len(l.strong.FindAllStringIndex(content, -1))
		}
		hits = append(hits, scored{l, strong, all})
	}
	score := func(s scored) int { return s.strong*1000 + s.all }
	for i := 0; i < len(hits); i++ {
		for j := i + 1; j < len(hits); j++ {
			if score(hits[j]) > score(hits[i]) {
				hits[i], hits[j] = hits[j], hits[i]
			}
		}
	}
	out := make([]language, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.lang)
	}
	return out
}
