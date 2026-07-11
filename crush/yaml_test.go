package crush

import (
	"fmt"
	"strings"
	"testing"

	"github.com/initializ/ctxzip/ccr"
)

// describePod builds a kubectl-describe-like fixture. When crashing is true,
// the container section carries the CrashLoopBackOff state that must survive
// folding.
func describePod(crashing bool) string {
	var b strings.Builder
	b.WriteString("Name:             payments-api-7d9f\n")
	b.WriteString("Namespace:        prod\n")
	b.WriteString("Node:             node-1/192.168.1.9\n")
	b.WriteString("Labels:           app=payments\n")
	b.WriteString("Status:           Running\n")
	b.WriteString("Containers:\n")
	b.WriteString("  payments:\n")
	b.WriteString("    Image:          ghcr.io/x/payments:v1\n")
	if crashing {
		b.WriteString("    State:          Waiting\n")
		b.WriteString("      Reason:       CrashLoopBackOff\n")
		b.WriteString("    Restart Count:  41\n")
	} else {
		b.WriteString("    State:          Started\n")
		b.WriteString("    Restart Count:  0\n")
	}
	b.WriteString("    Environment:\n")
	for i := 0; i < 25; i++ {
		fmt.Fprintf(&b, "      VAR_NUMBER_%02d:  value-%d\n", i, i)
	}
	b.WriteString("    Mounts:\n")
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, "      /mnt/data-%02d from vol-%d (ro)\n", i, i)
	}
	b.WriteString("Volumes:\n")
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, "  vol-%d:\n    Type:  ConfigMap\n    Path:  /cfg/%d\n", i, i)
	}
	b.WriteString("QoS Class:        Burstable\n")
	return b.String()
}

func TestYAMLCrusher_FoldsBoringSubtrees(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewYAMLCrusher()
	in := describePod(false)

	res, err := c.Compress(Request{Content: in, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if res.Compressed == in {
		t.Fatal("healthy describe should fold")
	}
	// Skeleton survives: top-level keys and identity.
	for _, keep := range []string{"Name:", "payments-api-7d9f", "Containers:", "Volumes:", "QoS Class:"} {
		if !strings.Contains(res.Compressed, keep) {
			t.Fatalf("skeleton line %q lost:\n%s", keep, res.Compressed)
		}
	}
	// The bulky env list folded.
	if strings.Contains(res.Compressed, "VAR_NUMBER_07") {
		t.Fatal("environment list should have folded")
	}
	if len(res.Markers) == 0 {
		t.Fatal("no markers emitted")
	}
	// Each fold round-trips with layout intact.
	for _, h := range res.Markers {
		entry, ok := store.Get(h)
		if !ok {
			t.Fatalf("marker %s not retrievable", h)
		}
		if !strings.Contains(string(entry.Original), "\n") {
			t.Fatalf("folded block lost layout: %.80s", entry.Original)
		}
	}
	// Deterministic.
	res2, _ := c.Compress(Request{Content: in, Store: ccr.NewMemoryStore(ccr.MemoryConfig{})})
	if res2.Compressed != res.Compressed {
		t.Fatal("not deterministic")
	}
}

// The fidelity guarantee: a crashing container's section contains error
// vocabulary, so it must never fold — with zero YAML-specific k8s knowledge.
func TestYAMLCrusher_CrashingSectionSurvives(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewYAMLCrusher()
	res, _ := c.Compress(Request{Content: describePod(true), Store: store})

	for _, keep := range []string{"CrashLoopBackOff", "Restart Count:  41"} {
		if !strings.Contains(res.Compressed, keep) {
			t.Fatalf("crash evidence folded away — floor failed:\n%s", res.Compressed)
		}
	}
	// The boring Volumes section still folds.
	if strings.Contains(res.Compressed, "/cfg/15") {
		t.Fatal("volumes should still fold")
	}
}

// Query anchors protect subtrees the user is asking about.
func TestYAMLCrusher_QueryProtectsSubtree(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewYAMLCrusher()
	res, _ := c.Compress(Request{
		Content: describePod(false),
		Query:   "check the VAR_NUMBER_07 environment value",
		Store:   store,
	})
	if !strings.Contains(res.Compressed, "VAR_NUMBER_07") {
		t.Fatal("query-anchored subtree folded away")
	}
}

// Oversized single-line scalar values (last-applied-configuration style)
// fold to a value marker, keeping the key.
func TestYAMLCrusher_LongValueFolds(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewYAMLCrusher()

	var b strings.Builder
	b.WriteString("metadata:\n")
	b.WriteString("  name: thing\n")
	b.WriteString("  annotations:\n")
	fmt.Fprintf(&b, "    kubectl.kubernetes.io/last-applied-configuration: %s\n",
		strings.Repeat(`{"big":"blob"}`, 60))
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&b, "  label%d: v%d\n", i, i)
	}
	res, _ := c.Compress(Request{Content: b.String(), Store: store})

	if !strings.Contains(res.Compressed, "last-applied-configuration:") {
		t.Fatal("key line lost")
	}
	if strings.Count(res.Compressed, `{"big":"blob"}`) > 0 {
		t.Fatal("long value should have folded")
	}
	if len(res.Markers) == 0 {
		t.Fatal("no marker for folded value")
	}
	orig, ok := store.Get(res.Markers[len(res.Markers)-1])
	if !ok || !strings.Contains(string(orig.Original), `{"big":"blob"}`) {
		t.Fatal("folded value not retrievable")
	}
}

func TestYAMLCrusher_SmallInputPassthrough(t *testing.T) {
	store := ccr.NewMemoryStore(ccr.MemoryConfig{})
	c := NewYAMLCrusher()
	in := "a: 1\nb: 2\nc: 3"
	res, _ := c.Compress(Request{Content: in, Store: store})
	if res.Compressed != in {
		t.Fatal("small yaml should pass through")
	}
}
