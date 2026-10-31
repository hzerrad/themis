package investigation_test

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

// forbidden answers like an API server that denies the core's ServiceAccount.
type forbidden struct{ name string }

func (f forbidden) Name() string { return f.name }

func (forbidden) Collect(context.Context, client.Reader, incident.TargetRef, map[string]string) (evidence.Item, error) {
	return evidence.Item{}, apierrors.NewForbidden(schema.GroupResource{Resource: "events"}, "", errors.New("RBAC: access denied"))
}

// An initial template Themis may not read is a gap the operator can fix, not
// silence: it is recorded with its cause, and the investigation goes on.
func TestPrepare_ForbiddenTemplateRecordsGap(t *testing.T) {
	e := newEnv(t)
	e.worker.Registry = evidence.NewRegistry(evidence.NewRolloutHistory(), evidence.NewPodStates(), forbidden{evidence.TargetEvents})
	id := e.newIncident(t, "rollout-regression", e.target)

	if err := e.worker.Prepare(t.Context(), id); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := e.state(t, id); got != incident.Investigating {
		t.Errorf("state = %s, want investigating", got)
	}
	gaps, err := e.store.EvidenceGaps(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 1 {
		t.Fatalf("got %d gaps, want 1: %+v", len(gaps), gaps)
	}
	g := gaps[0]
	if g.Template != evidence.TargetEvents || g.Cause != evidence.GapForbidden || g.LeaseID != "" || g.Detail == "" {
		t.Errorf("gap = %+v, want target_events forbidden before any lease, with the API server's message", g)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM evidence_items WHERE incident_id = $1`, id); n != 2 {
		t.Errorf("%d items, want the 2 that could be read", n)
	}
}

// Prepare runs at least once: a second run records the gap once.
func TestPrepare_GapRecordedOnce(t *testing.T) {
	e := newEnv(t)
	e.worker.Registry = evidence.NewRegistry(evidence.NewRolloutHistory(), evidence.NewPodStates(), forbidden{evidence.TargetEvents})
	id := e.newIncident(t, "rollout-regression", e.target)
	for range 2 {
		if err := e.worker.Prepare(t.Context(), id); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
	}
	if gaps, _ := e.store.EvidenceGaps(t.Context(), id); len(gaps) != 1 {
		t.Errorf("got %d gaps after two runs, want 1", len(gaps))
	}
}

// Evidence that could be read leaves no gap.
func TestPrepare_NoGapWhenEverythingIsRead(t *testing.T) {
	e := newEnv(t)
	id := e.newIncident(t, "rollout-regression", e.target)
	if err := e.worker.Prepare(t.Context(), id); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if gaps, err := e.store.EvidenceGaps(t.Context(), id); err != nil || len(gaps) != 0 {
		t.Errorf("gaps = %+v, %v; want none", gaps, err)
	}
}
