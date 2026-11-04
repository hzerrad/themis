package catalog_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/workload"
)

var diagnosedAt = time.Date(2026, 10, 27, 9, 20, 0, 0, time.UTC)

// failing is a template whose every read fails with err.
type failing struct {
	name string
	err  error
}

func (f failing) Name() string      { return f.name }
func (failing) Description() string { return "" }
func (failing) Params() []string    { return nil }

func (f failing) Collect(context.Context, client.Reader, *workload.Workload, map[string]string) (evidence.Item, error) {
	return evidence.Item{}, f.err
}

// diagnose diagnoses a Deployment with no pods and no events, with the
// shipped checks and the templates given.
func diagnose(t *testing.T, templates evidence.Registry, p config.Profile, at catalog.Moment) catalog.Diagnosis {
	t.Helper()
	cat, err := catalog.Themis(nil)
	if err != nil {
		t.Fatal(err)
	}
	cat.Evidence = templates
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "checkout", UID: "uid-checkout"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "checkout"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "checkout"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "checkout:2"}}},
			},
		},
	}
	c := fake.NewClientBuilder().WithObjects(d).Build()
	w, err := workload.Read(t.Context(), c, incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", UID: "uid-checkout"})
	if err != nil {
		t.Fatal(err)
	}
	return cat.Diagnose(t.Context(), c, w, p, at)
}

func initial(templates ...string) config.Profile {
	return config.Profile{Evidence: config.Evidence{Initial: templates, Allowed: templates}}
}

// Evidence Themis can't read is a gap with its cause, not a failure: the
// rest is read, and each check that needed it is unknown with that gap. It is
// blind when Themis couldn't see, and ambiguous when it looked and found
// nothing.
func TestDiagnose_GapsAndBlindness(t *testing.T) {
	denied := apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "replicasets"}, "", errors.New("RBAC: access denied"))
	got := diagnose(t, evidence.NewRegistry(
		failing{evidence.RolloutHistory, denied},
		failing{evidence.PodStates, evidence.ErrNoPods},
		evidence.NewTargetEvents(),
	), initial(evidence.RolloutHistory, evidence.PodStates, evidence.TargetEvents), catalog.Moment{Now: diagnosedAt})

	if len(got.Items) != 1 || got.Items[0].Template != evidence.TargetEvents {
		t.Errorf("items = %+v, want target_events only", got.Items)
	}
	if len(got.Gaps) != 2 ||
		got.Gaps[0].Template != evidence.RolloutHistory || got.Gaps[0].Cause != evidence.GapForbidden || !strings.Contains(got.Gaps[0].Detail, "RBAC: access denied") ||
		got.Gaps[1].Template != evidence.PodStates || got.Gaps[1].Cause != evidence.GapAbsent {
		t.Errorf("gaps = %+v, want rollout_history forbidden with the API server's message, then pod_states absent", got.Gaps)
	}
	for name, want := range map[string]struct {
		gap   check.Gap
		blind bool
	}{
		"rollout_recent":          {check.Gap{Template: evidence.RolloutHistory, Cause: evidence.GapForbidden}, true},
		"oom_killed_new_revision": {check.Gap{Template: evidence.PodStates, Cause: evidence.GapAbsent}, false},
	} {
		i := slices.IndexFunc(got.Results, func(r check.Result) bool { return r.Name == name })
		if i < 0 {
			t.Fatalf("%s was not run", name)
		}
		r := got.Results[i]
		if r.Value != check.Unknown || r.Blind != want.blind || !slices.Equal(r.Gaps, []check.Gap{want.gap}) {
			t.Errorf("%s = %+v, want unknown, blind %v, from %+v", name, r, want.blind, want.gap)
		}
	}
}

// Kubernetes deletes events after a while: when the alert started longer ago,
// the target's first events are gone, and that is a gap too.
func TestDiagnose_ExpiredEvents(t *testing.T) {
	events := evidence.NewRegistry(evidence.NewTargetEvents(), evidence.NewPodStates())
	tests := []struct {
		name      string
		profile   config.Profile
		started   time.Duration
		retention time.Duration
		expired   bool
	}{
		{"past the default hour", initial(evidence.TargetEvents), 70 * time.Minute, 0, true},
		{"within it", initial(evidence.TargetEvents), 10 * time.Minute, 0, false},
		{"within a longer retention", initial(evidence.TargetEvents), 70 * time.Minute, 2 * time.Hour, false},
		{"no alert start known", initial(evidence.TargetEvents), 0, 0, false},
		{"events not read", initial(evidence.PodStates), 70 * time.Minute, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at := catalog.Moment{Now: diagnosedAt, EventRetention: tt.retention}
			if tt.started > 0 {
				at.AlertStarted = diagnosedAt.Add(-tt.started)
			}
			got := diagnose(t, events, tt.profile, at)
			var expired []catalog.Gap
			for _, g := range got.Gaps {
				if g.Cause == evidence.GapExpired {
					expired = append(expired, g)
				}
			}
			if !tt.expired {
				if len(expired) != 0 {
					t.Errorf("gaps = %+v, want none expired", got.Gaps)
				}
				return
			}
			want := catalog.Gap{Template: evidence.TargetEvents, Cause: evidence.GapExpired, Detail: "the alert started 1h10m0s before, and Kubernetes keeps events for about 1h0m0s"}
			if !slices.Equal(expired, []catalog.Gap{want}) {
				t.Errorf("gaps = %+v, want %+v", got.Gaps, want)
			}
		})
	}
}

// A profile runs the checks it names, in the catalog's order, or all of them
// when it names none.
func TestDiagnose_RunsTheProfilesChecks(t *testing.T) {
	cat, err := catalog.Themis(nil)
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	for _, c := range cat.Checks {
		all = append(all, c.Name)
	}
	for _, tt := range []struct {
		checks, want []string
	}{
		{[]string{"gitops_managed", "rollout_recent"}, []string{"rollout_recent", "gitops_managed"}},
		{nil, all},
	} {
		p := initial(evidence.TargetEvents)
		p.Checks = tt.checks
		var got []string
		for _, r := range diagnose(t, cat.Evidence, p, catalog.Moment{Now: diagnosedAt}).Results {
			got = append(got, r.Name)
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("checks %v ran %v, want %v", tt.checks, got, tt.want)
		}
	}
}
