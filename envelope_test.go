package ctxzip

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/initializ/ctxzip/ccr"
)

// kubectlEnvelope simulates forge cli_execute output: a single-line JSON
// object whose stdout field holds a large kubectl table with one crashing pod.
func kubectlEnvelope(t *testing.T, rows int) (envelope, inner string) {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("NAMESPACE      NAME                                     READY   STATUS             RESTARTS   AGE\n")
	for i := 0; i < rows; i++ {
		fmt.Fprintf(&sb, "ns-%02d          backup-job-%08d-%04x                  1/1     Running            0          38d\n", i%5, i*37, i*997)
	}
	sb.WriteString("prod           payments-api-7d9f                        1/2     CrashLoopBackOff   41         2d\n")
	inner = sb.String()
	env, err := json.Marshal(map[string]any{
		"stdout": inner, "stderr": "", "exit_code": 0, "truncated": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(env), inner
}

// Regression (live-test find): tool runners wrap output in a single-line JSON
// envelope ({"stdout": "...\n...", ...}) whose escaped newlines defeat every
// content detector — 28 KB kubectl outputs sailed through uncompressed.
// Envelope handling must compress the inner text, keep the result valid JSON,
// keep untouched fields byte-identical, preserve the anomaly, and round-trip
// the offloaded lines intact.
func TestCompress_JSONEnvelope(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	env, _ := kubectlEnvelope(t, 300)

	msgs := []Message{
		{Role: RoleSystem, Content: "You are a k8s triage assistant."},
		{Role: RoleUser, Content: "are there crashing pods?"},
		{Role: RoleTool, Name: "cli_execute", Content: env},
		{Role: RoleAssistant, Content: "checking"},
		{Role: RoleUser, Content: "go on"},
	}
	opts := DefaultOptions()
	opts.Store = store
	res, err := Compress(msgs, opts)
	if err != nil {
		t.Fatal(err)
	}

	compressed := res.Messages[2].Content
	if compressed == env {
		t.Fatal("envelope was not compressed (the live-test bug)")
	}
	if res.SavedTokens() <= 0 {
		t.Fatalf("no savings: before=%d after=%d", res.TokensBefore, res.TokensAfter)
	}
	if len(res.Transforms) != 1 || !strings.HasPrefix(res.Transforms[0].Strategy, "envelope:") {
		t.Fatalf("expected envelope strategy, got %+v", res.Transforms)
	}

	// Still a valid JSON object with untouched fields intact.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(compressed), &obj); err != nil {
		t.Fatalf("compressed envelope is not valid JSON: %v\n%.200s", err, compressed)
	}
	if string(obj["exit_code"]) != "0" || string(obj["truncated"]) != "false" {
		t.Fatalf("untouched fields corrupted: exit_code=%s truncated=%s", obj["exit_code"], obj["truncated"])
	}

	// The anomaly survives in the visible stdout; the marker is inside it.
	var stdout string
	if err := json.Unmarshal(obj["stdout"], &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "CrashLoopBackOff") {
		t.Fatalf("crashing pod dropped from compressed view:\n%s", stdout)
	}
	hashes := ccr.ExtractHashes(stdout)
	if len(hashes) != 1 {
		t.Fatalf("want 1 marker inside stdout, got %d", len(hashes))
	}

	// Offloaded lines round-trip with real newlines and intact layout.
	orig, ok := Unzip(store, hashes[0])
	if !ok {
		t.Fatal("offloaded lines not retrievable")
	}
	if !strings.Contains(string(orig), "\n") || !strings.Contains(string(orig), "Running") {
		t.Fatalf("retrieved original mangled:\n%.200s", orig)
	}

	// Determinism: same input, identical bytes (prompt-cache safety).
	res2, _ := Compress(msgs, opts)
	if res2.Messages[2].Content != compressed {
		t.Fatal("envelope compression not deterministic")
	}
}

// Non-envelope and small-field content must fall through untouched.
func TestCompressEnvelope_Fallthrough(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	opts := DefaultOptions()
	opts.Store = store

	small, _ := json.Marshal(map[string]any{"stdout": "ok\n", "exit_code": 0})
	msgs := []Message{
		{Role: RoleUser, Content: "run it"},
		{Role: RoleTool, Name: "cli_execute", Content: string(small)},
		{Role: RoleAssistant, Content: "done"},
		{Role: RoleUser, Content: "thanks"},
	}
	res, _ := Compress(msgs, opts)
	if res.Messages[1].Content != string(small) {
		t.Fatal("small envelope should be untouched")
	}
}

// Live-test find #2: kubectl table rows barely deduped because every pod NAME
// is a unique digit-bearing token. The identifier-collapsing signature must
// crush hundreds of Running rows to exemplars while the anomaly row survives.
func TestCompress_K8sTableCrushes(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	_, inner := kubectlEnvelope(t, 300)

	msgs := []Message{
		{Role: RoleUser, Content: "check pods"},
		{Role: RoleTool, Name: "kubectl", Content: inner},
		{Role: RoleAssistant, Content: "ok"},
		{Role: RoleUser, Content: "and?"},
	}
	opts := DefaultOptions()
	opts.Store = store
	res, _ := Compress(msgs, opts)

	if res.Ratio() < 0.5 {
		t.Fatalf("k8s table should crush hard, got %.0f%% (before=%d after=%d)",
			res.Ratio()*100, res.TokensBefore, res.TokensAfter)
	}
	if !strings.Contains(res.Messages[1].Content, "CrashLoopBackOff") {
		t.Fatal("anomaly row dropped")
	}
}
