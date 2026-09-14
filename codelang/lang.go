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
	// signals scores content for detection; higher means more likely.
	signals *regexp.Regexp
	// weight is a tie-break priority when two languages score equally, favoring
	// the more specific dialect (TS over JS, C++ over C).
	weight int
}

func set(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// languages is the detection/elision registry, most-specific dialects first.
var languages = []language{
	{
		name: "typescript", grammar: tsx.GetLanguage, usesBraces: true, weight: 2,
		funcTypes: set("function_declaration", "method_definition", "generator_function_declaration", "method_signature"),
		bodyTypes: set("statement_block"),
		signals:   regexp.MustCompile(`(?m)\binterface\s+\w|\btype\s+\w+\s*=|:\s*(string|number|boolean)\b|\bimplements\b|\bexport\s+(type|interface)\b|\bas\s+\w`),
	},
	{
		name: "javascript", grammar: javascript.GetLanguage, usesBraces: true, weight: 1,
		funcTypes: set("function_declaration", "method_definition", "generator_function_declaration"),
		bodyTypes: set("statement_block"),
		signals:   regexp.MustCompile(`(?m)\bfunction\s|\bconst\s|\blet\s|=>|\brequire\(|\bmodule\.exports\b|\bconsole\.log\b|===`),
	},
	{
		name: "cpp", grammar: cpp.GetLanguage, usesBraces: true, weight: 2,
		funcTypes: set("function_definition"),
		bodyTypes: set("compound_statement"),
		signals:   regexp.MustCompile(`\bstd::|::\w|\btemplate\s*<|\bnamespace\s|\bnullptr\b|\bcout\b|\bclass\s+\w`),
	},
	{
		name: "c", grammar: c.GetLanguage, usesBraces: true, weight: 1,
		funcTypes: set("function_definition"),
		bodyTypes: set("compound_statement"),
		signals:   regexp.MustCompile(`(?m)#include|\bint\s+main\b|\bprintf\b|\bstruct\s+\w|\btypedef\b|\bmalloc\b|->`),
	},
	{
		name: "java", grammar: java.GetLanguage, usesBraces: true, weight: 1,
		funcTypes: set("method_declaration", "constructor_declaration"),
		bodyTypes: set("block", "constructor_body"),
		signals:   regexp.MustCompile(`\bpublic\s+class\b|\bpackage\s+\w|\bimport\s+java|@Override\b|\bSystem\.|public\s+static\s+void\s+main`),
	},
	{
		name: "python", grammar: python.GetLanguage, usesBraces: false, weight: 1,
		funcTypes: set("function_definition"),
		bodyTypes: set("block"),
		signals:   regexp.MustCompile(`(?m)^\s*def\s+\w+\s*\(|^\s*class\s+\w+.*:|^\s*(from\s+\w+\s+)?import\s+\w|\belif\b|\bself\b`),
	},
}

// detectCandidates returns the languages whose signals fire in content, ordered
// by (match count, specificity weight) descending — the parse-verify step in the
// crusher confirms the winner, so an ambiguous guess self-corrects.
func detectCandidates(content string) []language {
	type scored struct {
		lang  language
		count int
	}
	var hits []scored
	for _, l := range languages {
		if n := len(l.signals.FindAllStringIndex(content, -1)); n > 0 {
			hits = append(hits, scored{l, n})
		}
	}
	// Stable sort by count desc, then weight desc (specific dialect first).
	for i := 0; i < len(hits); i++ {
		for j := i + 1; j < len(hits); j++ {
			a, b := hits[i], hits[j]
			if b.count > a.count || (b.count == a.count && b.lang.weight > a.lang.weight) {
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
