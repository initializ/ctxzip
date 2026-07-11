package crush

import (
	"fmt"
	"strings"

	"github.com/initializ/ctxzip/ccr"
)

// YAMLCrusher compresses YAML-ish field-per-line content — `kubectl get -o
// yaml`, `kubectl describe`, config dumps. Line dedup barely touches this
// shape (every line is a distinct field), but its bulk lives in deep,
// boring subtrees: managedFields, env lists, volume mounts, tolerations,
// giant last-applied-configuration values.
//
// Strategy: build an indentation tree (no YAML parser — dependency-free and
// tolerant of near-YAML like kubectl describe), then fold every subtree
// whose block is large and contains nothing protected: the key line stays,
// the children are offloaded behind an indented marker. Oversized scalar
// values on a single line fold the same way. Each fold gets its OWN marker,
// so the model expands exactly the subtree it needs.
//
// Fidelity comes from the shared floor: a subtree containing error
// vocabulary, a MustKeep pattern, or a query match is never folded — a
// healthy pod's Containers section folds, a crashing pod's contains
// "Reason: CrashLoopBackOff" and survives whole, with zero YAML-specific
// domain knowledge.
type YAMLCrusher struct {
	// MinLines is the content size below which nothing is attempted.
	MinLines int
	// FoldMinChars is the subtree block size worth folding.
	FoldMinChars int
	// LongValueChars folds a single line's scalar value at this length.
	LongValueChars int
}

// NewYAMLCrusher returns a YAMLCrusher with sensible defaults.
func NewYAMLCrusher() *YAMLCrusher {
	return &YAMLCrusher{MinLines: 15, FoldMinChars: 500, LongValueChars: 400}
}

// Name implements Compressor.
func (c *YAMLCrusher) Name() string { return "yaml_crusher" }

// yamlNode is one line plus everything indented under it.
type yamlNode struct {
	line     string
	indent   int
	children []*yamlNode
}

// Compress implements Compressor.
func (c *YAMLCrusher) Compress(req Request) (Result, error) {
	lines := strings.Split(req.Content, "\n")
	if len(lines) < c.MinLines || req.Store == nil {
		return passthrough(c.Name(), req.Content), nil
	}

	root := parseIndentTree(lines)
	terms := queryTerms(req.Query)

	f := &folder{c: c, req: req, terms: terms}
	f.walk(root)
	if f.folds == 0 {
		return passthrough(c.Name(), req.Content), nil
	}

	var b strings.Builder
	renderTree(root, &b)
	out := strings.TrimSuffix(b.String(), "\n")
	if strings.TrimSpace(out) == "" {
		return passthrough(c.Name(), req.Content), nil
	}
	return Result{Compressed: out, Strategy: c.Name(), Markers: f.markers}, nil
}

// parseIndentTree builds the indentation tree; a virtual root (indent -1)
// holds the top-level lines. Blank lines attach to the current branch so
// they fold with their section.
func parseIndentTree(lines []string) *yamlNode {
	root := &yamlNode{indent: -1}
	stack := []*yamlNode{root}
	for _, ln := range lines {
		ind := leadingSpaces(ln)
		if strings.TrimSpace(ln) == "" {
			// Attach blanks to the deepest open branch.
			parent := stack[len(stack)-1]
			parent.children = append(parent.children, &yamlNode{line: ln, indent: parent.indent + 1})
			continue
		}
		for len(stack) > 1 && stack[len(stack)-1].indent >= ind {
			stack = stack[:len(stack)-1]
		}
		n := &yamlNode{line: ln, indent: ind}
		parent := stack[len(stack)-1]
		parent.children = append(parent.children, n)
		stack = append(stack, n)
	}
	return root
}

func leadingSpaces(s string) int {
	n := 0
	for _, r := range s {
		if r == ' ' {
			n++
		} else if r == '\t' {
			n += 4
		} else {
			break
		}
	}
	return n
}

// folder carries the fold pass state.
type folder struct {
	c       *YAMLCrusher
	req     Request
	terms   []string
	markers []string
	folds   int
}

// walk folds subtrees DEEPEST-FIRST: inner bulky blocks (env lists,
// managedFields, long scalar values) fold before their parents are
// considered, and a parent's residue only folds if it is STILL bulky after
// inner folds — which it never is when it holds identity scalars plus
// markers, because marker-bearing blocks count as protected. Net effect:
// homogeneous boring sections (Volumes:) fold whole even at top level,
// while mixed sections (metadata: name + managedFields) keep their identity
// lines next to the folded bulk.
func (f *folder) walk(n *yamlNode) {
	for _, child := range n.children {
		if len(child.children) > 0 {
			f.walk(child)
			block := renderChildren(child)
			if len(block) >= f.c.FoldMinChars && !f.protected(block) {
				f.fold(child, block)
			}
			continue
		}
		// Oversized scalar value on one line (e.g. last-applied-configuration
		// annotations): fold the value, keep the key.
		if len(child.line) >= f.c.LongValueChars {
			if key, val, ok := splitKeyValue(child.line); ok &&
				len(val) >= f.c.LongValueChars && !f.protected(val) {
				hash := ccr.Hash([]byte(val))
				if f.req.Store.Put(hash, []byte(val), ccr.Meta{
					ToolName:     f.req.ToolName,
					Query:        f.req.Query,
					OriginalKind: "yaml_value",
				}) == nil {
					child.line = key + " " + ccr.Marker(hash, fmt.Sprintf("%d_chars_offloaded", len(val)))
					f.markers = append(f.markers, hash)
					f.folds++
				}
			}
		}
	}
}

// fold offloads a node's children and replaces them with one marker line.
func (f *folder) fold(n *yamlNode, block string) bool {
	lineCount := strings.Count(block, "\n") + 1
	hash := ccr.Hash([]byte(block))
	if f.req.Store.Put(hash, []byte(block), ccr.Meta{
		ToolName:     f.req.ToolName,
		Query:        f.req.Query,
		ItemCount:    lineCount,
		OriginalKind: "yaml_subtree",
	}) != nil {
		return false
	}
	marker := ccr.Marker(hash, fmt.Sprintf("%d_lines_offloaded", lineCount))
	n.children = []*yamlNode{{
		line:   strings.Repeat(" ", n.indent+2) + marker,
		indent: n.indent + 2,
	}}
	f.markers = append(f.markers, hash)
	f.folds++
	return true
}

// protected reports whether text contains anything the fidelity floor keeps:
// error vocabulary, caller MustKeep patterns, query anchors — or an existing
// marker, so already-folded residue (identity lines + markers) never
// re-folds into nested markers.
func (f *folder) protected(text string) bool {
	if strings.Contains(text, ccr.MarkerPrefix) {
		return true
	}
	lower := strings.ToLower(text)
	if looksError(lower) {
		return true
	}
	if mustKeep(lower, f.req.MustKeep) {
		return true
	}
	return len(f.terms) > 0 && matchesAny(lower, discriminativeTerms(strings.Split(text, "\n"), f.terms))
}

// splitKeyValue splits "  key: very long value" into the key part (with
// colon) and the value. ok=false for lines without a key-colon shape.
func splitKeyValue(line string) (key, val string, ok bool) {
	i := strings.Index(line, ": ")
	if i <= 0 {
		return "", "", false
	}
	return line[:i+1], line[i+2:], true
}

// renderChildren renders a node's descendant lines verbatim.
func renderChildren(n *yamlNode) string {
	var b strings.Builder
	for _, c := range n.children {
		renderTree(c, &b)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// renderTree emits a node's line (if any) and its descendants.
func renderTree(n *yamlNode, b *strings.Builder) {
	if n.indent >= 0 {
		b.WriteString(n.line)
		b.WriteByte('\n')
	}
	for _, c := range n.children {
		renderTree(c, b)
	}
}
