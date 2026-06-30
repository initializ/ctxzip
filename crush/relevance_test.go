package crush

import (
	"reflect"
	"testing"
)

func TestLooksError(t *testing.T) {
	yes := []string{"connection timeout", "ERROR: boom", "request failed", "invalid token"}
	no := []string{"all good", "request handled ok", "status running"}
	for _, s := range yes {
		if !looksError(s) {
			t.Errorf("looksError(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if looksError(s) {
			t.Errorf("looksError(%q) = true, want false", s)
		}
	}
}

func TestLooksFragile(t *testing.T) {
	yes := []string{
		"see 0xdeadbeef for the address",
		"raised an IndexError here",
		"open /etc/passwd now",
		"pass the --verbose flag",
		"in libsystem_kernel.dylib",
		"got an EOF",
	}
	no := []string{"the year was 2024", "a plain english sentence"}
	for _, s := range yes {
		if !looksFragile(s) {
			t.Errorf("looksFragile(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if looksFragile(s) {
			t.Errorf("looksFragile(%q) = true, want false", s)
		}
	}
}

func TestQueryTerms(t *testing.T) {
	// "db" and "at" drop (2 chars, no digit); "v2" survives (digit-bearing id).
	got := queryTerms("Why did the DB migration fail at v2?")
	want := []string{"why", "did", "the", "migration", "fail", "v2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("queryTerms = %v, want %v", got, want)
	}
	if queryTerms("") != nil {
		t.Error("empty query should yield nil terms")
	}
}

func TestBM25_RanksMatchHigher(t *testing.T) {
	docs := [][]string{
		{"the", "cat", "sat"},
		{"database", "migration", "failed"},
		{"a", "sunny", "day"},
	}
	m := newBM25(docs)
	q := []string{"migration", "failed"}
	if m.score(q, 1) <= m.score(q, 0) {
		t.Error("matching doc should outscore non-matching doc")
	}
	if m.score(q, 1) <= m.score(q, 2) {
		t.Error("matching doc should outscore unrelated doc")
	}
}
