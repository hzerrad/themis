package investigation_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/investigation"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

const scenario1 = "test/fixtures/scenario1/cluster.yaml"

func profile() config.Profile {
	return config.Profile{
		Name: "rollout-regression", Priority: 100,
		Evidence: config.Evidence{
			Initial: []string{evidence.RolloutHistory, evidence.PodStates, evidence.TargetEvents},
			Allowed: []string{evidence.RolloutHistory, evidence.PodStates, evidence.TargetEvents},
		},
	}
}

type env struct {
	worker *investigation.PrepareWorker
	store  *store.Store
	pool   *pgxpool.Pool
	target incident.TargetRef
}

func newEnv(t *testing.T) *env {
	t.Helper()
	s, pool := pgtest.NewStore(t)
	ns := k8s.NewNamespace(t)
	var target incident.TargetRef
	for _, obj := range k8s.Load(t, ns, scenario1) {
		if obj.GetKind() == "Deployment" {
			target = targetOf(obj)
		}
	}
	cat, err := catalog.Themis(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &env{
		worker: &investigation.PrepareWorker{
			Config:   config.Config{EnrolledNamespaces: []string{ns}, Profiles: []config.Profile{profile()}},
			Store:    s,
			Reader:   k8s,
			Evidence: evidence.NewRegistry(evidence.NewRolloutHistory(), evidence.NewPodStates(), evidence.NewTargetEvents()),
			Checks:   cat.Checks,
			Now:      func() time.Time { return time.Date(2026, 10, 27, 9, 20, 0, 0, time.UTC) },
		},
		store: s, pool: pool, target: target,
	}
}

func targetOf(obj *unstructured.Unstructured) incident.TargetRef {
	gvk := obj.GroupVersionKind()
	return incident.TargetRef{Group: gvk.Group, Kind: gvk.Kind, Namespace: obj.GetNamespace(), Name: obj.GetName(), UID: string(obj.GetUID())}
}

func (e *env) newIncident(t *testing.T, profile string, target incident.TargetRef) string {
	t.Helper()
	var id string
	err := e.store.InTx(t.Context(), func(tx pgx.Tx) error {
		var err error
		id, _, err = e.store.CreateOrJoinIncident(t.Context(), tx, store.IncidentArgs{
			Fingerprint: profile + "/" + target.UID, Profile: profile, Target: target, Payload: []byte(`{}`),
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) state(t *testing.T, id string) incident.State {
	t.Helper()
	in, err := e.store.Incident(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return in.State
}

func TestPrepare_CollectsInitialEvidenceAndPredicates(t *testing.T) {
	e := newEnv(t)
	id := e.newIncident(t, "rollout-regression", e.target)

	// River delivers at least once: a second run must change nothing.
	for range 2 {
		if err := e.worker.Prepare(t.Context(), id); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
	}
	if got := e.state(t, id); got != incident.Investigating {
		t.Errorf("state = %s, want investigating", got)
	}
	// Evidence that could be read leaves no gap.
	if gaps, err := e.store.EvidenceGaps(t.Context(), id); err != nil || len(gaps) != 0 {
		t.Errorf("gaps = %+v, %v; want none", gaps, err)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM evidence_items WHERE incident_id = $1`, id); n != 3 {
		t.Errorf("%d initial items, want 3", n)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM predicate_results WHERE incident_id = $1`, id); n != 6 {
		t.Errorf("%d predicates, want 6", n)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM predicate_results WHERE incident_id = $1 AND name = 'oom_killed_new_revision' AND value = 'true'`, id); n != 1 {
		t.Error("oom_killed_new_revision is not true on the scenario 1 cluster")
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM investigations WHERE incident_id = $1 AND state = 'pending'`, id); n != 1 {
		t.Error("no pending investigation")
	}
	if in, _ := e.store.Incident(t.Context(), id); in.ObservedSpecHash == "" {
		t.Error("the investigated template is not recorded")
	}
}

// A profile that names checks runs only those.
func TestPrepare_RunsTheChecksItsProfileNames(t *testing.T) {
	e := newEnv(t)
	p := profile()
	p.Checks = []string{"gitops_managed", "rollout_recent"}
	e.worker.Config = config.Config{EnrolledNamespaces: []string{e.target.Namespace}, Profiles: []config.Profile{p}}
	id := e.newIncident(t, p.Name, e.target)
	if err := e.worker.Prepare(t.Context(), id); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	results, err := e.store.Predicates(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range results {
		names = append(names, r.Name)
	}
	if !slices.Equal(names, []string{"rollout_recent", "gitops_managed"}) {
		t.Errorf("checks run = %v, want rollout_recent and gitops_managed", names)
	}
}

// Without an investigator, the evidence and checks are recorded for the
// policy, and the incident waits for a suggestion instead.
func TestPrepare_WithoutAnInvestigatorWaitsForASuggestion(t *testing.T) {
	e := newEnv(t)
	cfg := e.worker.Config.Current()
	cfg.Investigators.Disabled = true
	e.worker.Config = cfg
	id := e.newIncident(t, "rollout-regression", e.target)
	for range 2 {
		if err := e.worker.Prepare(t.Context(), id); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
	}
	if got := e.state(t, id); got != incident.AwaitingSuggestion {
		t.Errorf("state = %s, want awaiting_suggestion", got)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM evidence_items WHERE incident_id = $1`, id); n != 3 {
		t.Errorf("%d initial items, want 3", n)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM predicate_results WHERE incident_id = $1`, id); n != 6 {
		t.Errorf("%d predicates, want 6", n)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM investigations WHERE incident_id = $1`, id); n != 0 {
		t.Errorf("%d investigations, want none: nothing would ever pick it up", n)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM transitions WHERE incident_id = $1 AND reason LIKE '%No investigator is installed%'`, id); n != 1 {
		t.Error("the timeline doesn't say why the incident waits")
	}
}

// A target lost before Themis acts ends the incident in a way that says what
// happened. A replaced one isn't left unresolved: a new incident follows the
// new object.
func TestPrepare_TargetLost(t *testing.T) {
	e := newEnv(t)
	replaced, deleted, statefulSet := e.target, e.target, e.target
	replaced.UID = "uid-of-the-deployment-before"
	deleted.Name = "deleted"
	statefulSet.Kind = "StatefulSet"

	for name, c := range map[string]struct {
		target incident.TargetRef
		want   incident.State
	}{
		"replaced":         {replaced, incident.Canceled},
		"deleted":          {deleted, incident.Canceled},
		"unsupported kind": {statefulSet, incident.Rejected},
	} {
		id := e.newIncident(t, "rollout-regression", c.target)
		if err := e.worker.Prepare(t.Context(), id); err != nil {
			t.Fatalf("%s: Prepare: %v", name, err)
		}
		in, err := e.store.Incident(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if in.State != c.want {
			t.Errorf("%s: state = %s, want %s", name, in.State, c.want)
		}
		if name != "replaced" {
			if in.SupersededBy != "" {
				t.Errorf("%s: superseded by %s, want no successor", name, in.SupersededBy)
			}
			continue
		}
		next, err := e.store.Incident(t.Context(), in.SupersededBy)
		if err != nil {
			t.Fatalf("replaced: no successor: %v", err)
		}
		if next.Target != e.target || next.State != incident.Received {
			t.Errorf("replaced: successor targets %+v in %s, want the new object, received", next.Target, next.State)
		}
	}
}

// unavailable answers like an API server that is briefly down.
type unavailable struct{ client.Reader }

func (unavailable) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return apierrors.NewServiceUnavailable("the server is shutting down")
}

// A target that can't be read right now is not a lost one: the job fails, to
// be retried, and the incident stays as it was.
func TestPrepare_UnreadableTargetRetries(t *testing.T) {
	e := newEnv(t)
	e.worker.Reader = unavailable{k8s}
	id := e.newIncident(t, "rollout-regression", e.target)

	if err := e.worker.Prepare(t.Context(), id); err == nil {
		t.Error("Prepare = nil, want an error so the job is retried")
	}
	if got := e.state(t, id); got != incident.Received {
		t.Errorf("state = %s, want received", got)
	}
}

func TestPrepare_ProfileRemoved_Rejects(t *testing.T) {
	e := newEnv(t)
	id := e.newIncident(t, "renamed-profile", e.target)

	if err := e.worker.Prepare(t.Context(), id); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := e.state(t, id); got != incident.Rejected {
		t.Errorf("state = %s, want rejected", got)
	}
}
