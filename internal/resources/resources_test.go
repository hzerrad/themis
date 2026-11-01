package resources_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/resources"
)

// fixture has a platform namespace and two team namespaces, one enrolled.
type fixture struct {
	live                *resources.Live
	platform, team, out string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{platform: k8s.NewNamespace(t), team: k8s.NewNamespace(t), out: k8s.NewNamespace(t)}
	base := config.Config{EnrolledNamespaces: []string{f.team}, Resources: &config.Resources{Namespace: f.platform}}
	f.live = resources.New(base, k8s.Client, k8s.Client, func(p config.Profile) error {
		if p.Policy.Mode == "" && len(p.Policy.Rules) > 0 && p.Policy.Rules[0].When == "bad" {
			return errors.New("rule bad doesn't compile")
		}
		return nil
	}, slog.Default())
	return f
}

func profile(ns, name string, priority int64, mutate ...func(map[string]any)) *unstructured.Unstructured {
	spec := map[string]any{
		"priority": priority,
		"match":    map[string]any{"alertNames": []any{"A"}, "targetKinds": []any{"Deployment.apps"}},
		"evidence": map[string]any{"allowed": []any{"pod_states"}},
	}
	for _, m := range mutate {
		m(spec)
	}
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetGroupVersionKind(resources.IncidentProfileKind)
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

func badRule(spec map[string]any) {
	spec["policy"] = map[string]any{"rules": []any{map[string]any{"name": "r", "when": "bad", "decision": "deny"}}}
}

func apply(t *testing.T, u *unstructured.Unstructured) {
	t.Helper()
	if err := k8s.Client.Create(t.Context(), u); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), u) })
}

func (f *fixture) refresh(t *testing.T) config.Config {
	t.Helper()
	if err := f.live.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return f.live.Current()
}

// accepted reads a resource's Accepted condition.
func accepted(t *testing.T, u *unstructured.Unstructured) (status, reason, message string) {
	t.Helper()
	got := u.DeepCopy()
	if err := k8s.Get(t.Context(), client.ObjectKeyFromObject(u), got); err != nil {
		t.Fatal(err)
	}
	conditions, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
	for _, c := range conditions {
		m := c.(map[string]any) //nolint:forcetypeassert // the CRD's schema
		if m["type"] == "Accepted" {
			return m["status"].(string), m["reason"].(string), m["message"].(string) //nolint:forcetypeassert // the CRD's schema
		}
	}
	t.Fatalf("no Accepted condition on %s", u.GetName())
	return "", "", ""
}

func target(ns string) incident.TargetRef {
	return incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: ns, Name: "x"}
}

func TestPlatformProfileAppliesToEveryEnrolledNamespace(t *testing.T) {
	f := newFixture(t)
	p := profile(f.platform, "shared", 10)
	apply(t, p)
	cfg := f.refresh(t)
	got, ok := cfg.MatchProfile("A", nil, target(f.team))
	if !ok || got.Name != "shared" {
		t.Errorf("match = %+v, %v; want shared", got, ok)
	}
	if status, _, _ := accepted(t, p); status != "True" {
		t.Errorf("Accepted = %s", status)
	}
}

// A team's profile is theirs alone: it can't reach another namespace.
func TestTeamProfileAppliesOnlyToItsOwnNamespace(t *testing.T) {
	f := newFixture(t)
	other := k8s.NewNamespace(t)
	apply(t, profile(f.team, "ours", 10))
	cfg := f.refresh(t)
	got, ok := cfg.MatchProfile("A", nil, target(f.team))
	if !ok || got.Name != f.team+"/ours" {
		t.Errorf("match = %+v, %v; want %s/ours", got, ok, f.team)
	}
	if got, ok := cfg.MatchProfile("A", nil, target(other)); ok {
		t.Errorf("profile %s reached another namespace", got.Name)
	}
	if _, ok := cfg.Profile(f.team + "/ours"); !ok {
		t.Error("the profile can't be found by the name incidents record")
	}
}

func TestTeamProfileWinsATie(t *testing.T) {
	f := newFixture(t)
	apply(t, profile(f.platform, "shared", 10))
	apply(t, profile(f.team, "ours", 10))
	got, _ := f.refresh(t).MatchProfile("A", nil, target(f.team))
	if got.Name != f.team+"/ours" {
		t.Errorf("match = %s", got.Name)
	}
}

func TestProfileInANamespaceNotEnrolledIsReportedAndIgnored(t *testing.T) {
	f := newFixture(t)
	p := profile(f.out, "theirs", 10)
	apply(t, p)
	if cfg := f.refresh(t); len(cfg.Profiles) != 0 {
		t.Errorf("profiles = %+v, want none", cfg.Profiles)
	}
	if status, reason, message := accepted(t, p); status != "False" || reason != "NotEnrolled" || !strings.Contains(message, "isn't enrolled") {
		t.Errorf("Accepted = %s %s %q", status, reason, message)
	}
}

func TestInvalidProfileIsRefusedAndNeverApplied(t *testing.T) {
	f := newFixture(t)
	p := profile(f.platform, "broken", 10, badRule)
	apply(t, p)
	if cfg := f.refresh(t); len(cfg.Profiles) != 0 {
		t.Errorf("profiles = %+v, want none", cfg.Profiles)
	}
	if status, reason, message := accepted(t, p); status != "False" || reason != "Invalid" || !strings.Contains(message, "doesn't compile") {
		t.Errorf("Accepted = %s %s %q", status, reason, message)
	}
}

func TestPreviousVersionKeepsApplyingWhenAnEditIsInvalid(t *testing.T) {
	f := newFixture(t)
	p := profile(f.platform, "shared", 10)
	apply(t, p)
	f.refresh(t)

	edited := p.DeepCopy()
	if err := k8s.Get(t.Context(), client.ObjectKeyFromObject(p), edited); err != nil {
		t.Fatal(err)
	}
	spec := edited.Object["spec"].(map[string]any) //nolint:forcetypeassert // set above
	badRule(spec)
	if err := k8s.Update(t.Context(), edited); err != nil {
		t.Fatal(err)
	}
	cfg := f.refresh(t)
	got, ok := cfg.Profile("shared")
	if !ok || len(got.Policy.Rules) != 0 {
		t.Errorf("profile = %+v, %v; want the previous version", got, ok)
	}
	if status, _, message := accepted(t, p); status != "False" || !strings.Contains(message, "previous version keeps applying") {
		t.Errorf("Accepted = %s %q", status, message)
	}
}

// The older resource keeps a contested priority, so a new one can't displace
// what's working.
func TestSamePriorityInAScopeRefusesTheNewer(t *testing.T) {
	f := newFixture(t)
	older := profile(f.platform, "older", 10)
	apply(t, older)
	time.Sleep(1100 * time.Millisecond) // creation times have a resolution of one second
	newer := profile(f.platform, "newer", 10)
	apply(t, newer)
	cfg := f.refresh(t)
	if len(cfg.Profiles) != 1 || cfg.Profiles[0].Name != "older" {
		t.Errorf("profiles = %+v, want older alone", cfg.Profiles)
	}
	if status, _, message := accepted(t, newer); status != "False" || !strings.Contains(message, "already used by profile older") {
		t.Errorf("Accepted = %s %q", status, message)
	}
}

func themisConfig(spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetGroupVersionKind(resources.ThemisConfigKind)
	u.SetName(resources.ThemisConfigName)
	return u
}

func TestAutonomyCeilingHoldsEveryProfileToObserve(t *testing.T) {
	f := newFixture(t)
	apply(t, profile(f.team, "ours", 10, func(spec map[string]any) { spec["policy"] = map[string]any{"mode": "guarded"} }))
	apply(t, themisConfig(map[string]any{"autonomyCeiling": "observe"}))
	cfg := f.refresh(t)
	got, ok := cfg.Profile(f.team + "/ours")
	if !ok || got.EffectivePolicy().Mode != config.ModeObserve {
		t.Errorf("profile = %+v, %v; want observe", got, ok)
	}
}

func TestThemisConfigCanNarrowEnrollmentButNeverWiden(t *testing.T) {
	f := newFixture(t)
	tc := themisConfig(map[string]any{"enrolledNamespaces": []any{f.team, f.out}})
	apply(t, tc)
	cfg := f.refresh(t)
	if !cfg.IsEnrolled(f.team) || cfg.IsEnrolled(f.out) {
		t.Errorf("enrolled = %v, want %s alone", cfg.EnrolledNamespaces, f.team)
	}
	if status, reason, _ := accepted(t, tc); status != "True" || reason != "NotAllEnrolled" {
		t.Errorf("Accepted = %s %s", status, reason)
	}
}

func TestThemisConfigNamingOnlyOutsideNamespacesIsRefused(t *testing.T) {
	f := newFixture(t)
	tc := themisConfig(map[string]any{"enrolledNamespaces": []any{f.out}})
	apply(t, tc)
	cfg := f.refresh(t)
	if !cfg.IsEnrolled(f.team) {
		t.Errorf("enrolled = %v, want it unchanged", cfg.EnrolledNamespaces)
	}
	if status, _, _ := accepted(t, tc); status != "False" {
		t.Errorf("Accepted = %s", status)
	}
}

func TestThereIsOneThemisConfig(t *testing.T) {
	u := themisConfig(map[string]any{})
	u.SetName("second")
	if err := k8s.Client.Create(t.Context(), u); err == nil {
		_ = k8s.Delete(t.Context(), u)
		t.Error("a ThemisConfig with another name was accepted")
	}
}

func TestDeletingAProfileRemovesIt(t *testing.T) {
	f := newFixture(t)
	p := profile(f.platform, "shared", 10)
	apply(t, p)
	f.refresh(t)
	if err := k8s.Delete(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if cfg := f.refresh(t); len(cfg.Profiles) != 0 {
		t.Errorf("profiles = %+v, want none", cfg.Profiles)
	}
}

// A change reaches the running configuration with no restart.
func TestWatch_AppliesAChangeWithoutARestart(t *testing.T) {
	f := newFixture(t)
	mgr, err := ctrl.NewManager(k8s.Config, ctrl.Options{Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.live.SetupWatches(mgr); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	go func() { _ = mgr.Start(ctx) }()

	p := profile(f.platform, "shared", 10)
	apply(t, p)
	waitFor(t, "the new profile", func() bool {
		_, ok := f.live.Current().Profile("shared")
		return ok
	})
	if err := k8s.Delete(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the profile to go", func() bool {
		_, ok := f.live.Current().Profile("shared")
		return !ok
	})
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// An agent can be let in, or shut out, with no restart.
func TestThemisConfigNamesWhoMaySuggestFixes(t *testing.T) {
	f := newFixture(t)
	tc := themisConfig(map[string]any{"externalProposals": map[string]any{"sources": []any{map[string]any{
		"serviceAccount": "agents/triage", "namespaces": []any{f.team}, "capabilities": []any{config.KnownCapabilities[0]},
	}}}})
	apply(t, tc)
	if _, ok := f.refresh(t).ExternalProposals.Source("agents/triage"); !ok {
		t.Error("the agent wasn't let in")
	}
	if err := k8s.Delete(t.Context(), tc); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.refresh(t).ExternalProposals.Source("agents/triage"); ok {
		t.Error("the agent is still let in")
	}
}

func TestThemisConfigWithAnInvalidSourceIsRefused(t *testing.T) {
	f := newFixture(t)
	tc := themisConfig(map[string]any{"externalProposals": map[string]any{"sources": []any{map[string]any{
		"serviceAccount": "not-an-account", "namespaces": []any{f.team}, "capabilities": []any{"x"},
	}}}})
	apply(t, tc)
	if cfg := f.refresh(t); len(cfg.ExternalProposals.Sources) != 0 {
		t.Errorf("sources = %+v, want none", cfg.ExternalProposals.Sources)
	}
	if status, _, _ := accepted(t, tc); status != "False" {
		t.Errorf("Accepted = %s", status)
	}
}
