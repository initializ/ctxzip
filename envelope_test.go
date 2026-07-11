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

// kubectlJSONEnvelope simulates `kubectl get pods -A -o json` through a
// runner envelope: {"stdout": "<pretty-printed {\"items\": [pod objects]}>"}.
func kubectlJSONEnvelope(t *testing.T, pods int) string {
	t.Helper()
	items := make([]map[string]any, 0, pods+1)
	for i := 0; i < pods; i++ {
		items = append(items, map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{
				"name":      fmt.Sprintf("pod-%04d", i),
				"namespace": fmt.Sprintf("ns-%d", i%7),
				"labels":    map[string]any{"app": fmt.Sprintf("app-%d", i%9)},
			},
			"status": map[string]any{"phase": "Running", "restartCount": 0},
		})
	}
	items = append(items, map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": "payments-api-7d9f", "namespace": "prod"},
		"status": map[string]any{
			"phase": "Running",
			"containerStatuses": []map[string]any{{
				"state":        map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}},
				"restartCount": 41,
				"lastState":    map[string]any{"terminated": map[string]any{"reason": "OOMKilled"}},
			}},
		},
	})
	innerObj, err := json.MarshalIndent(map[string]any{"apiVersion": "v1", "items": items, "kind": "List"}, "", "    ")
	if err != nil {
		t.Fatal(err)
	}
	env, err := json.Marshal(map[string]any{"stdout": string(innerObj), "stderr": "", "exit_code": 0, "truncated": false})
	if err != nil {
		t.Fatal(err)
	}
	return string(env)
}

// Item-1 of the post-merge roadmap: kubectl -o json is an OBJECT wrapping an
// array — previously it fell through to line-mode text dedup, scattering
// records. The object walker must route the items[] array through the JSON
// crusher so anomalous records survive WHOLE and the rest offload as
// retrievable complete records.
func TestCompress_KubectlJSONItems(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	env := kubectlJSONEnvelope(t, 120)

	msgs := []Message{
		{Role: RoleUser, Content: "crashing pods?"},
		{Role: RoleTool, Name: "cli_execute", Content: env},
		{Role: RoleAssistant, Content: "x"},
		{Role: RoleUser, Content: "y"},
	}
	opts := DefaultOptions()
	opts.Store = store
	res, err := Compress(msgs, opts)
	if err != nil {
		t.Fatal(err)
	}

	compressed := res.Messages[1].Content
	if res.Ratio() < 0.5 {
		t.Fatalf("items[] shape should crush hard, got %.0f%% (before=%d after=%d)",
			res.Ratio()*100, res.TokensBefore, res.TokensAfter)
	}
	if len(res.Transforms) != 1 || res.Transforms[0].Strategy != "envelope:json_crusher" {
		t.Fatalf("expected envelope:json_crusher, got %+v", res.Transforms[0].Strategy)
	}

	// Whole-structure validity: envelope -> stdout string -> inner object ->
	// items array must all still parse.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(compressed), &obj); err != nil {
		t.Fatalf("envelope invalid: %v", err)
	}
	var stdout string
	if err := json.Unmarshal(obj["stdout"], &stdout); err != nil {
		t.Fatal(err)
	}
	var innerObj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &innerObj); err != nil {
		t.Fatalf("inner kubectl object invalid: %v\n%.300s", err, stdout)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(innerObj["items"], &items); err != nil {
		t.Fatalf("items array invalid: %v", err)
	}
	if string(innerObj["kind"]) != `"List"` {
		t.Fatalf("sibling field corrupted: %s", innerObj["kind"])
	}

	// The crashing pod survives as a WHOLE record (not scattered lines).
	if !strings.Contains(stdout, "CrashLoopBackOff") || !strings.Contains(stdout, "payments-api-7d9f") {
		t.Fatal("anomalous pod record dropped from compressed view")
	}

	// Offloaded records round-trip as a valid JSON array of complete pods.
	hashes := ccr.ExtractHashes(stdout)
	if len(hashes) != 1 {
		t.Fatalf("want 1 marker, got %d", len(hashes))
	}
	orig, ok := Unzip(store, hashes[0])
	if !ok {
		t.Fatal("offloaded records not retrievable")
	}
	var dropped []map[string]any
	if err := json.Unmarshal(orig, &dropped); err != nil || len(dropped) == 0 {
		t.Fatalf("offloaded content is not a valid array of records: err=%v n=%d", err, len(dropped))
	}
	if _, hasMeta := dropped[0]["metadata"]; !hasMeta {
		t.Fatal("offloaded records are not complete pod objects")
	}

	// Determinism.
	res2, _ := Compress(msgs, opts)
	if res2.Messages[1].Content != compressed {
		t.Fatal("items[] compression not deterministic")
	}
}

// A bare (non-enveloped) kubectl -o json object gets the same treatment at
// depth 0.
func TestCompress_BareItemsObject(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	env := kubectlJSONEnvelope(t, 120)
	// Extract just the inner object text.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(env), &obj); err != nil {
		t.Fatal(err)
	}
	var inner string
	if err := json.Unmarshal(obj["stdout"], &inner); err != nil {
		t.Fatal(err)
	}

	msgs := []Message{
		{Role: RoleUser, Content: "pods?"},
		{Role: RoleTool, Name: "k8s_api", Content: inner},
		{Role: RoleAssistant, Content: "x"},
		{Role: RoleUser, Content: "y"},
	}
	opts := DefaultOptions()
	opts.Store = store
	res, _ := Compress(msgs, opts)

	if res.Ratio() < 0.5 {
		t.Fatalf("bare items object should crush, got %.0f%%", res.Ratio()*100)
	}
	if !json.Valid([]byte(res.Messages[1].Content)) {
		t.Fatal("compressed bare object is not valid JSON")
	}
	if !strings.Contains(res.Messages[1].Content, "CrashLoopBackOff") {
		t.Fatal("anomaly dropped")
	}
}

// Live-test find (run 004): a runtime size cap cut a 108KB envelope
// mid-string and appended "[OUTPUT TRUNCATED ...]", breaking the JSON — the
// envelope path bailed and the mangled blob passed through uncompressed.
// Salvage must compress the intact prefix, emit VALID JSON, and tell the
// model the tail was destroyed upstream (not offloaded).
func TestCompress_TruncatedEnvelopeSalvage(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	env, _ := kubectlEnvelope(t, 300)

	// Simulate the runtime's cap: cut inside the stdout string (well past the
	// crashing pod's row) and append the truncation notice.
	cut := len(env) * 3 / 4
	truncated := env[:cut] + "\n\n[OUTPUT TRUNCATED -- original length: 108854 chars]"

	msgs := []Message{
		{Role: RoleUser, Content: "crashing pods?"},
		{Role: RoleTool, Name: "cli_execute", Content: truncated},
		{Role: RoleAssistant, Content: "x"},
		{Role: RoleUser, Content: "y"},
	}
	opts := DefaultOptions()
	opts.Store = store
	res, err := Compress(msgs, opts)
	if err != nil {
		t.Fatal(err)
	}

	compressed := res.Messages[1].Content
	if compressed == truncated {
		t.Fatal("truncated envelope was not salvaged (the live-test bug)")
	}
	if len(res.Transforms) != 1 || !strings.HasPrefix(res.Transforms[0].Strategy, "envelope_truncated:") {
		t.Fatalf("expected envelope_truncated strategy, got %+v", res.Transforms)
	}

	// Output is VALID JSON again, with the upstream-truncation note.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(compressed), &obj); err != nil {
		t.Fatalf("salvaged envelope is not valid JSON: %v\n%.200s", err, compressed)
	}
	var note string
	if err := json.Unmarshal(obj["_ctxzip_note"], &note); err != nil || !strings.Contains(note, "destroyed") {
		t.Fatalf("missing/incomplete upstream-truncation note: %q err=%v", note, err)
	}

	// The intact prefix compressed with markers retrievable.
	var stdout string
	if err := json.Unmarshal(obj["stdout"], &stdout); err != nil {
		t.Fatal(err)
	}
	hashes := ccr.ExtractHashes(stdout)
	if len(hashes) == 0 {
		t.Fatalf("no marker in salvaged stdout:\n%.200s", stdout)
	}
	if _, ok := Unzip(store, hashes[0]); !ok {
		t.Fatal("salvaged offload not retrievable")
	}

	// Determinism.
	res2, _ := Compress(msgs, opts)
	if res2.Messages[1].Content != compressed {
		t.Fatal("salvage not deterministic")
	}
}

// A cut OUTSIDE a string value is ambiguous — salvage must bail cleanly.
func TestCompress_TruncatedEnvelope_NonStringCutBails(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	// Big enough to pass the message-size floor, cut mid-number.
	pad := strings.Repeat("x", 3000)
	truncated := `{"stdout":"` + pad + `","exit_co`

	msgs := []Message{
		{Role: RoleUser, Content: "run"},
		{Role: RoleTool, Name: "cli_execute", Content: truncated},
		{Role: RoleAssistant, Content: "x"},
		{Role: RoleUser, Content: "y"},
	}
	opts := DefaultOptions()
	opts.Store = store
	res, _ := Compress(msgs, opts)
	// Falls through to direct routing; whatever happens, no envelope strategy
	// and no invalid-JSON reconstruction claiming to be the envelope.
	for _, tr := range res.Transforms {
		if strings.HasPrefix(tr.Strategy, "envelope") {
			t.Fatalf("ambiguous cut must not use envelope strategy: %+v", tr)
		}
	}
}

// bestEffortUnquote must stop cleanly at partial trailing escapes.
func TestBestEffortUnquote(t *testing.T) {
	cases := map[string]string{
		`line1\nline2`:        "line1\nline2",
		`tab\there`:           "tab\there",
		`quote\"inside`:       `quote"inside`,
		`unicode\u0041end`:    "unicodeAend",
		`cut at escape\`:      "cut at escape",
		`cut at unicode\u00`:  "cut at unicode",
		`terminated"trailing`: "terminated",
	}
	for in, want := range cases {
		if got := bestEffortUnquote(in); got != want {
			t.Errorf("bestEffortUnquote(%q) = %q, want %q", in, got, want)
		}
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
