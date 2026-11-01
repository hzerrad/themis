package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/metrictest"
)

func insertGap(t *testing.T, s *store.Store, g store.EvidenceGap) {
	t.Helper()
	if err := s.InTx(t.Context(), func(tx pgx.Tx) error { return s.InsertEvidenceGap(t.Context(), tx, g) }); err != nil {
		t.Fatalf("InsertEvidenceGap: %v", err)
	}
}

func TestEvidenceGaps_RoundTrip(t *testing.T) {
	s, _ := newStore(t)
	id := investigating(t, s)
	inv, _ := lease(t, s, t0)
	at := t0.Add(time.Minute).UTC()

	insertGap(t, s, store.EvidenceGap{
		IncidentID: id, Template: evidence.RolloutHistory, Cause: evidence.GapForbidden,
		Detail: `replicasets.apps is forbidden`, At: at,
	})
	insertGap(t, s, store.EvidenceGap{
		IncidentID: id, LeaseID: inv.LeaseID, Template: evidence.ContainerLogsTail,
		Params: map[string]string{"pod": "checkout-7d9f8b6c4-x2kqp"}, Cause: evidence.GapNotInProfile, At: at.Add(time.Second),
	})

	gaps, err := s.EvidenceGaps(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 2 {
		t.Fatalf("got %d gaps, want 2", len(gaps))
	}
	first, second := gaps[0], gaps[1]
	if first.Template != evidence.RolloutHistory || first.Cause != evidence.GapForbidden || first.LeaseID != "" ||
		first.Detail != "replicasets.apps is forbidden" || !first.At.Equal(at) {
		t.Errorf("initial gap = %+v", first)
	}
	if second.LeaseID != inv.LeaseID || second.Cause != evidence.GapNotInProfile || second.Params["pod"] != "checkout-7d9f8b6c4-x2kqp" {
		t.Errorf("requested gap = %+v", second)
	}
}

// A gap is recorded with the transaction that saw it: a rolled-back one
// leaves none.
func TestEvidenceGaps_RolledBackWithTheTransaction(t *testing.T) {
	s, _ := newStore(t)
	id := investigating(t, s)
	errAbort := errors.New("abort")
	err := s.InTx(t.Context(), func(tx pgx.Tx) error {
		if err := s.InsertEvidenceGap(t.Context(), tx, store.EvidenceGap{IncidentID: id, Template: evidence.PodStates, Cause: evidence.GapTimeout, At: t0}); err != nil {
			return err
		}
		return errAbort
	})
	if !errors.Is(err, errAbort) {
		t.Fatalf("InTx: %v", err)
	}
	if gaps, err := s.EvidenceGaps(t.Context(), id); err != nil || len(gaps) != 0 {
		t.Errorf("gaps = %v, %v; want none", gaps, err)
	}
}

// Only the closed set of causes is stored.
func TestEvidenceGaps_UnknownCauseRefused(t *testing.T) {
	s, _ := newStore(t)
	id := investigating(t, s)
	err := s.InTx(t.Context(), func(tx pgx.Tx) error {
		return s.InsertEvidenceGap(t.Context(), tx, store.EvidenceGap{IncidentID: id, Template: evidence.PodStates, Cause: "gremlins", At: t0})
	})
	if err == nil {
		t.Error("a gap with an unknown cause was stored")
	}
}

// Asking again for what was refused says nothing new, and must not grow the
// table.
func TestEvidenceGaps_OncePerLeaseTemplateAndCause(t *testing.T) {
	s, _ := newStore(t)
	id := investigating(t, s)
	inv, _ := lease(t, s, t0)
	for range 3 {
		insertGap(t, s, store.EvidenceGap{IncidentID: id, LeaseID: inv.LeaseID, Template: evidence.TargetEvents, Cause: evidence.GapBudgetSpent, At: t0})
	}
	insertGap(t, s, store.EvidenceGap{IncidentID: id, LeaseID: inv.LeaseID, Template: evidence.PodStates, Cause: evidence.GapBudgetSpent, At: t0})
	for range 2 {
		insertGap(t, s, store.EvidenceGap{IncidentID: id, Template: evidence.RolloutHistory, Cause: evidence.GapForbidden, At: t0})
	}
	if gaps, err := s.EvidenceGaps(t.Context(), id); err != nil || len(gaps) != 3 {
		t.Errorf("got %d gaps, %v; want one per lease, template and cause: 3", len(gaps), err)
	}
}

// Each thing missed is counted once, when it is first recorded.
func TestMetrics_MissingEvidenceCountedOnce(t *testing.T) {
	s, _ := newStore(t)
	id := investigating(t, s)
	counted := metrictest.Delta(t, "themis_missing_evidence_total", map[string]string{"template": evidence.TargetEvents, "cause": string(evidence.GapExpired)})
	for range 2 {
		insertGap(t, s, store.EvidenceGap{IncidentID: id, Template: evidence.TargetEvents, Cause: evidence.GapExpired, At: t0})
	}
	if got := counted(); got != 1 {
		t.Errorf("counted %v, want 1", got)
	}
}
