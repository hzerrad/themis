package investigation_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/investigation"
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
	return &env{
		worker: &investigation.PrepareWorker{
			Config:   config.Config{EnrolledNamespaces: []string{ns}, Profiles: []config.Profile{profile()}},
			Store:    s,
			Reader:   k8s,
			Registry: evidence.NewRegistry(evidence.NewRolloutHistory(), evidence.NewPodStates(), evidence.NewTargetEvents()),
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

	if err := e.worker.Prepare(t.Context(), id); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := e.state(t, id); got != incident.Investigating {
		t.Errorf("state = %s, want investigating", got)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM evidence_items WHERE incident_id = $1 AND lease_id IS NULL`, id); n != 3 {
		t.Errorf("%d initial items, want 3", n)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM predicate_results WHERE incident_id = $1`, id); n != 5 {
		t.Errorf("%d predicates, want 5", n)
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

// River delivers at least once: a second run must change nothing.
func TestPrepare_Idempotent(t *testing.T) {
	e := newEnv(t)
	id := e.newIncident(t, "rollout-regression", e.target)
	for range 2 {
		if err := e.worker.Prepare(t.Context(), id); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM evidence_items WHERE incident_id = $1`, id); n != 3 {
		t.Errorf("%d items after two runs, want 3", n)
	}
}

func TestPrepare_TargetGone_Rejects(t *testing.T) {
	e := newEnv(t)
	gone := e.target
	gone.UID = "uid-of-a-deleted-deployment"
	id := e.newIncident(t, "rollout-regression", gone)

	if err := e.worker.Prepare(t.Context(), id); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := e.state(t, id); got != incident.Rejected {
		t.Errorf("state = %s, want rejected", got)
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
