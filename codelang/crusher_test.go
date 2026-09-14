package codelang

import (
	"regexp"
	"strings"
	"testing"

	"github.com/initializ/ctxzip/ccr"
	"github.com/initializ/ctxzip/crush"
)

var markerRe = regexp.MustCompile(`<<ctxzip:([0-9a-f]{12,64})(?:[ ,][^>]*)?>>`)

func expand(t *testing.T, compressed string, store ccr.Store) string {
	t.Helper()
	return markerRe.ReplaceAllStringFunc(compressed, func(m string) string {
		h := markerRe.FindStringSubmatch(m)[1]
		e, ok := store.Get(h)
		if !ok {
			t.Fatalf("marker %s not retrievable", h)
		}
		return string(e.Original)
	})
}

// Each sample is a realistically-sized file (> MinLines) with at least one
// function whose body is worth eliding (> MinBodyLines) and a distinctive
// innard, plus enough structure to detect the language.
var samples = map[string]struct {
	lang   string // expected strategy language suffix
	src    string
	innard string // a body line that must be offloaded (gone from output)
	keep   string // a structural line that must survive
}{
	"python": {"python", `# widget builder module
import os
import sys


class Widget:
    def build(self, name, size):
        if not name:
            raise KeyError("required")
        total = size * 2
        label = name.upper()
        parts = [label, str(total)]
        joined = "-".join(parts)
        return joined

    def rename(self, new):
        old = self.name
        self.name = new
        self.dirty = True
        return old
`, "total = size * 2", "def build(self, name, size):"},

	"javascript": {"javascript", `import fs from 'fs';
import path from 'path';


function build(name, size) {
    if (!name) {
        return null;
    }
    const total = size * 2;
    const label = name.toUpperCase();
    const parts = [label, total];
    return parts.join('-');
}

function rename(obj, next) {
    const old = obj.name;
    obj.name = next;
    obj.dirty = true;
    return old;
}
`, "const total = size * 2", "function build(name, size) {"},

	"typescript": {"typescript", `interface Config {
    name: string;
    size: number;
}

function build(cfg: Config): string {
    if (!cfg.name) {
        return '';
    }
    const total: number = cfg.size * 2;
    const label = cfg.name.toUpperCase();
    const parts = [label, String(total)];
    return parts.join('-');
}

function rename(cfg: Config, next: string): string {
    const old = cfg.name;
    cfg.name = next;
    return old;
}
`, "const total: number", "function build(cfg: Config): string {"},

	"java": {"java", `package widget;

import java.util.List;

public class Builder {
    private String name;

    public String build(String name, int size) {
        if (name.isEmpty()) {
            return "";
        }
        int total = size * 2;
        String label = name.toUpperCase();
        String joined = label + "-" + total;
        return joined;
    }

    public String rename(String next) {
        String old = this.name;
        this.name = next;
        return old;
    }
}
`, "int total = size * 2", "public String build(String name, int size) {"},

	"c": {"c", `#include <stdio.h>
#include <stdlib.h>

int build(const char* name, int size) {
    if (name == 0) {
        return -1;
    }
    int total = size * 2;
    int scaled = total + 1;
    printf("%s %d\n", name, scaled);
    return scaled;
}

int copy_name(char* dst, const char* src) {
    int n = 0;
    while (src[n]) {
        dst[n] = src[n];
        n = n + 1;
    }
    return n;
}
`, "int total = size * 2", "int build(const char* name, int size) {"},

	"cpp": {"cpp", `#include <string>
#include <vector>

class Builder {
public:
    std::string build(const std::string& name, int size) {
        if (name.empty()) {
            return "";
        }
        int total = size * 2;
        std::vector<std::string> parts;
        parts.push_back(name);
        return name + std::to_string(total);
    }

    std::string rename(const std::string& next) {
        std::string old = this->name_;
        this->name_ = next;
        return old;
    }

private:
    std::string name_;
};
`, "int total = size * 2", "std::string build(const std::string& name, int size) {"},
}

func TestCrusher_PerLanguage(t *testing.T) {
	for name, tc := range samples {
		t.Run(name, func(t *testing.T) {
			store := ccr.NewMemoryStore(ccr.MemoryConfig{})
			res, err := NewCrusher().Compress(crush.Request{Content: tc.src, Store: store})
			if err != nil {
				t.Fatal(err)
			}
			if res.Compressed == tc.src {
				t.Fatalf("expected compression, got passthrough:\n%s", tc.src)
			}
			if want := "code_treesitter:" + tc.lang; res.Strategy != want {
				t.Fatalf("strategy = %q, want %q", res.Strategy, want)
			}
			if !strings.Contains(res.Compressed, tc.keep) {
				t.Fatalf("signature dropped: %q not in\n%s", tc.keep, res.Compressed)
			}
			if strings.Contains(res.Compressed, tc.innard) {
				t.Fatalf("body not elided: %q still present", tc.innard)
			}
			if len(res.Markers) == 0 {
				t.Fatal("no markers emitted")
			}
			// Byte-exact reversibility.
			if got := expand(t, res.Compressed, store); got != tc.src {
				t.Fatalf("round-trip not byte-exact:\n--- got ---\n%s", got)
			}
		})
	}
}

func TestCrusher_MustKeepProtectsBody(t *testing.T) {
	src := `# module
import math


def alpha(x):
    a = x + 1
    b = a * 2
    c = b - 3
    d = c + 4
    return d


def beta(y):
    KEEPTOKEN = y
    d = y + 1
    e = d * 2
    f = e - 3
    return f
`
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	res, _ := NewCrusher().Compress(crush.Request{
		Content: src, Store: store, MustKeep: []string{"KEEPTOKEN"},
	})
	if !strings.Contains(res.Compressed, "KEEPTOKEN") {
		t.Fatal("MustKeep body was elided")
	}
	if strings.Contains(res.Compressed, "a = x + 1") {
		t.Fatal("non-protected body should have been elided")
	}
}

func TestCrusher_UnsupportedLanguageFallsBack(t *testing.T) {
	// Ruby is not one of the six grammars; the text crusher should handle it.
	var sb strings.Builder
	sb.WriteString("def handler\n")
	for i := 0; i < 60; i++ {
		sb.WriteString("  puts 'processing the current request'\n")
	}
	sb.WriteString("end\n")
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	res, _ := NewCrusher().Compress(crush.Request{Content: sb.String(), Store: store})
	if strings.HasPrefix(res.Strategy, "code_treesitter:") {
		t.Fatalf("Ruby should not be claimed by a grammar, got %s", res.Strategy)
	}
}

func TestCrusher_Deterministic(t *testing.T) {
	src := samples["python"].src
	r1, _ := NewCrusher().Compress(crush.Request{Content: src, Store: ccr.NewMemoryStore(ccr.MemoryConfig{})})
	r2, _ := NewCrusher().Compress(crush.Request{Content: src, Store: ccr.NewMemoryStore(ccr.MemoryConfig{})})
	if r1.Compressed != r2.Compressed || strings.Join(r1.Markers, ",") != strings.Join(r2.Markers, ",") {
		t.Fatal("compression is not deterministic")
	}
}

func TestCrusher_NilStore_Passthrough(t *testing.T) {
	src := samples["python"].src
	res, _ := NewCrusher().Compress(crush.Request{Content: src, Store: nil})
	if res.Compressed != src {
		t.Fatal("nil store must force lossless passthrough")
	}
}

// TestIntegration_WiredIntoCoreCodeCrusher proves the seam: with the tree-sitter
// crusher injected as the core CodeCrusher's Fallback, Python routes to
// tree-sitter while Go still uses the stdlib AST path.
func TestIntegration_WiredIntoCoreCodeCrusher(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	code := crush.NewCodeCrusher()
	code.Fallback = NewCrusher()

	res, _ := code.Compress(crush.Request{Content: samples["python"].src, Store: store})
	if !strings.HasPrefix(res.Strategy, "code_treesitter:") {
		t.Fatalf("Python should route to the tree-sitter fallback, got %s", res.Strategy)
	}

	goSrc := "package p\n\nimport \"fmt\"\n\n" +
		"func Build(name string) string {\n\tx := 1\n\ty := 2\n\tz := x + y\n\treturn fmt.Sprintf(\"%s-%d\", name, z)\n}\n\n" +
		"func Rename(a, b string) string {\n\told := a\n\ta = b\n\tb = old\n\treturn a + b\n}\n"
	resGo, _ := code.Compress(crush.Request{Content: goSrc, Store: store})
	if resGo.Strategy != "code_crusher" {
		t.Fatalf("Go should use the stdlib AST path, got %s", resGo.Strategy)
	}
}
