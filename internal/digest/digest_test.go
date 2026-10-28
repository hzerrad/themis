package digest_test

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/hzerrad/themis/internal/digest"
)

func action() digest.ActionFields {
	return digest.ActionFields{
		IncidentID:        "0192f0c1-0000-7000-8000-000000000001",
		Capability:        "deployment.rollback-to-revision",
		CapabilityVersion: "1",
		Target: digest.TargetIdentity{
			Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout",
			UID: "5f1c2a7e-0000-4000-8000-000000000001", Generation: 2,
			SpecHash: "sha256:abc", Manager: "kubectl",
		},
		Params:       map[string]any{"revision": int64(1)},
		PolicyID:     "guarded-rollback",
		PolicyDigest: "sha256:def",
		Constraints:  map[string]any{"maxUnavailable": "0"},
	}
}

func mustCompute(t *testing.T, a digest.ActionFields) string {
	t.Helper()
	d, err := digest.Compute(a)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	return d
}

func TestCompute_MapOrderIndependent(t *testing.T) {
	keys := make([]string, 30)
	for i := range keys {
		keys[i] = fmt.Sprintf("k%d", i)
	}
	// params inserts the same keys and values in a random order.
	params := func() map[string]any {
		rand.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] }) //nolint:gosec // test input order
		m := map[string]any{}
		for _, k := range keys {
			m[k] = map[string]any{"key": k, "nested": map[string]any{k: len(k)}}
		}
		return m
	}
	for range 50 {
		a, b := action(), action()
		a.Params, b.Params = params(), params()
		if mustCompute(t, a) != mustCompute(t, b) {
			t.Fatal("equal params in another order give another digest")
		}
	}
}

// Kubernetes objects decode numbers as int64 or float64 depending on the
// decoder; the digest must not depend on which.
func TestCompute_NumberTypesAgree(t *testing.T) {
	a, b := action(), action()
	a.Params = map[string]any{"revision": int64(1)}
	b.Params = map[string]any{"revision": float64(1)}
	if mustCompute(t, a) != mustCompute(t, b) {
		t.Error("int64(1) and float64(1) give different digests")
	}
}

func TestCompute_AnyFieldChangeChangesDigest(t *testing.T) {
	base := mustCompute(t, action())
	mutations := map[string]func(*digest.ActionFields){
		"incident":    func(a *digest.ActionFields) { a.IncidentID += "x" },
		"capability":  func(a *digest.ActionFields) { a.Capability += "x" },
		"version":     func(a *digest.ActionFields) { a.CapabilityVersion = "2" },
		"group":       func(a *digest.ActionFields) { a.Target.Group = "" },
		"kind":        func(a *digest.ActionFields) { a.Target.Kind = "StatefulSet" },
		"namespace":   func(a *digest.ActionFields) { a.Target.Namespace = "kube-system" },
		"name":        func(a *digest.ActionFields) { a.Target.Name = "cart" },
		"uid":         func(a *digest.ActionFields) { a.Target.UID += "x" },
		"generation":  func(a *digest.ActionFields) { a.Target.Generation++ },
		"spec hash":   func(a *digest.ActionFields) { a.Target.SpecHash += "x" },
		"manager":     func(a *digest.ActionFields) { a.Target.Manager = "argocd-controller" },
		"params":      func(a *digest.ActionFields) { a.Params = map[string]any{"revision": int64(2)} },
		"policy":      func(a *digest.ActionFields) { a.PolicyID += "x" },
		"policy hash": func(a *digest.ActionFields) { a.PolicyDigest += "x" },
		"constraints": func(a *digest.ActionFields) { a.Constraints = nil },
	}
	for name, mutate := range mutations {
		a := action()
		mutate(&a)
		if mustCompute(t, a) == base {
			t.Errorf("changing %s leaves the digest unchanged", name)
		}
	}
}

// resourceVersion changes on every status update; binding it would void
// almost every approval.
func TestCompute_NoResourceVersionField(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeFor[digest.TargetIdentity](), reflect.TypeFor[digest.ActionFields]()} {
		for f := range typ.Fields() {
			if strings.Contains(strings.ToLower(f.Name), "resourceversion") {
				t.Errorf("%s has field %s", typ.Name(), f.Name)
			}
		}
	}
}

func TestCompute_KnownVector(t *testing.T) {
	const want = "sha256:c945c870b0d77e1db8daf0a7b3144b2422d3a62e09187ebed59b48fcdebd93aa"
	if got := mustCompute(t, action()); got != want {
		t.Errorf("Compute = %s, want %s: the digest format changed", got, want)
	}
}

func deployment(memory string, readyReplicas int64) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": "checkout", "resourceVersion": "123"},
		"spec": map[string]any{
			"replicas": int64(3),
			"template": map[string]any{"spec": map[string]any{"containers": []any{
				map[string]any{"name": "app", "resources": map[string]any{"limits": map[string]any{"memory": memory}}},
			}}},
		},
		"status": map[string]any{"readyReplicas": readyReplicas},
	}
}

func TestSpecHash_IgnoresStatus(t *testing.T) {
	paths := []string{"spec.template"}
	hash := func(obj map[string]any) string {
		h, err := digest.SpecHash(obj, paths)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	base := hash(deployment("64Mi", 1))
	if hash(deployment("64Mi", 3)) != base {
		t.Error("a status change changes the hash")
	}
	if hash(deployment("256Mi", 1)) == base {
		t.Error("a template change leaves the hash unchanged")
	}
	if !strings.HasPrefix(base, "sha256:") {
		t.Errorf("hash = %s, want sha256:...", base)
	}
}

func TestSpecHash_MissingPathIsStable(t *testing.T) {
	a, err := digest.SpecHash(map[string]any{}, []string{"spec.template"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := digest.SpecHash(map[string]any{"spec": map[string]any{}}, []string{"spec.template"})
	if a != b {
		t.Error("a missing path hashes differently depending on its parent")
	}
}

// Canonical JSON numbers are doubles: integers beyond 2^53 would collide.
func TestCompute_RejectsInexactNumbers(t *testing.T) {
	a := action()
	a.Target.Generation = 1<<53 + 1
	if _, err := digest.Compute(a); err == nil {
		t.Error("Compute accepted an integer beyond 2^53")
	}
}

// A path through a non-object would bind nothing.
func TestSpecHash_PathThroughNonObject(t *testing.T) {
	if _, err := digest.SpecHash(deployment("64Mi", 1), []string{"spec.replicas.template"}); err == nil {
		t.Error("SpecHash accepted a path through a number")
	}
}
