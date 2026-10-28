package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/predicate"
	"github.com/hzerrad/themis/internal/store"
)

var t0 = time.Date(2026, 10, 27, 9, 20, 0, 0, time.UTC)

const leaseFor = time.Minute

func evidenceItem(id, template string) evidence.Item {
	return evidence.Item{
		ID: id, Template: template, Data: []byte(`{"pods":[]}`), CollectedAt: t0,
		Classification: evidence.ClassificationInternal, OriginalBytes: 11,
	}
}

// investigating creates an incident under investigation, with evidence and
// predicates, and returns the incident's ID.
func investigating(t *testing.T, s *store.Store) string {
	t.Helper()
	id, _ := createOrJoin(t, s, newIncident("fp-"+t.Name()))
	items := []evidence.Item{evidenceItem("0192f0c1-0000-7000-8000-00000000000a", evidence.PodStates)}
	results := []predicate.Result{{Name: "oom_killed_new_revision", Version: "1", Value: predicate.True, Reason: "x", EvidenceIDs: []string{items[0].ID}}}
	err := s.InTx(t.Context(), func(tx pgx.Tx) error {
		if err := s.Transition(t.Context(), tx, store.TransitionArgs{ID: id, From: incident.Received, To: incident.Investigating, Reason: "test", Actor: "test"}); err != nil {
			return err
		}
		_, err := s.CreateInvestigation(t.Context(), tx, id, items, results)
		return err
	})
	if err != nil {
		t.Fatalf("CreateInvestigation: %v", err)
	}
	return id
}

func lease(t *testing.T, s *store.Store, now time.Time) (store.Investigation, bool) {
	t.Helper()
	var (
		inv store.Investigation
		ok  bool
	)
	err := s.InTx(t.Context(), func(tx pgx.Tx) error {
		var err error
		inv, ok, err = s.LeaseInvestigation(t.Context(), tx, "worker-1", now, leaseFor)
		return err
	})
	if err != nil {
		t.Fatalf("LeaseInvestigation: %v", err)
	}
	return inv, ok
}

func leased(t *testing.T, s *store.Store, leaseID string, now time.Time) (store.Investigation, error) {
	t.Helper()
	var inv store.Investigation
	err := s.InTx(t.Context(), func(tx pgx.Tx) error {
		var err error
		inv, err = s.LeasedInvestigation(t.Context(), tx, leaseID, now)
		return err
	})
	return inv, err
}

func TestLeaseInvestigation_LeasesPendingOnce(t *testing.T) {
	s, _ := newStore(t)
	id := investigating(t, s)

	inv, ok := lease(t, s, t0)
	if !ok {
		t.Fatal("no investigation leased")
	}
	if inv.IncidentID != id || inv.LeaseID == "" || !inv.LeaseDeadline.Equal(t0.Add(leaseFor)) || inv.Attempts != 1 {
		t.Errorf("leased %+v", inv)
	}
	if inv.Profile != "rollout-regression" || inv.Target.Name != "checkout" {
		t.Errorf("leased investigation lacks its incident: %+v", inv)
	}
	if _, ok := lease(t, s, t0.Add(time.Second)); ok {
		t.Error("a leased investigation was leased again")
	}
}

func TestLeaseInvestigation_ExpiredLeaseIsLeasedAgain(t *testing.T) {
	s, _ := newStore(t)
	investigating(t, s)
	first, _ := lease(t, s, t0)

	second, ok := lease(t, s, t0.Add(leaseFor+time.Second))
	if !ok {
		t.Fatal("an expired lease was not leased again")
	}
	if second.LeaseID == first.LeaseID || second.Attempts != 2 {
		t.Errorf("second lease = %+v, want a new lease on attempt 2", second)
	}
	if _, err := leased(t, s, first.LeaseID, t0.Add(leaseFor+2*time.Second)); !errors.Is(err, store.ErrLeaseLost) {
		t.Errorf("the superseded lease: err = %v, want ErrLeaseLost", err)
	}
}

func TestLeasedInvestigation_ExpiredLease(t *testing.T) {
	s, _ := newStore(t)
	investigating(t, s)
	inv, _ := lease(t, s, t0)

	if _, err := leased(t, s, inv.LeaseID, t0.Add(leaseFor/2)); err != nil {
		t.Errorf("a live lease: err = %v", err)
	}
	if _, err := leased(t, s, inv.LeaseID, t0.Add(leaseFor+time.Second)); !errors.Is(err, store.ErrLeaseLost) {
		t.Errorf("an expired lease: err = %v, want ErrLeaseLost", err)
	}
	if _, err := leased(t, s, "0192f0c1-0000-7000-8000-0000000000ff", t0); !errors.Is(err, store.ErrLeaseLost) {
		t.Errorf("an unknown lease: err = %v, want ErrLeaseLost", err)
	}
	if _, err := leased(t, s, "not-a-uuid", t0); !errors.Is(err, store.ErrLeaseLost) {
		t.Errorf("a malformed lease: err = %v, want ErrLeaseLost", err)
	}
}

func TestRenewLease(t *testing.T) {
	s, _ := newStore(t)
	investigating(t, s)
	inv, _ := lease(t, s, t0)

	var deadline time.Time
	err := s.InTx(t.Context(), func(tx pgx.Tx) error {
		var err error
		deadline, err = s.RenewLease(t.Context(), tx, inv.LeaseID, t0.Add(30*time.Second), leaseFor)
		return err
	})
	if err != nil || !deadline.Equal(t0.Add(30*time.Second+leaseFor)) {
		t.Errorf("RenewLease = %v, %v", deadline, err)
	}
	if _, err := leased(t, s, inv.LeaseID, t0.Add(leaseFor+time.Second)); err != nil {
		t.Errorf("the renewed lease expired: %v", err)
	}
}

func TestCompleteInvestigation(t *testing.T) {
	s, pool := newStore(t)
	investigating(t, s)
	inv, _ := lease(t, s, t0)

	err := s.InTx(t.Context(), func(tx pgx.Tx) error {
		return s.CompleteInvestigation(t.Context(), tx, inv.LeaseID, store.InvestigationResult{
			Conclusion: "proposed", Summary: "revision 2 is OOMKilled",
			Hypotheses: []byte(`[{"rootCauseClass":"memory_limit_regression"}]`),
			Usage:      []byte(`{"stepsUsed":3}`), Provenance: []byte(`{"model":"qwen3"}`),
		})
	})
	if err != nil {
		t.Fatalf("CompleteInvestigation: %v", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM investigations WHERE state = 'completed' AND summary = 'revision 2 is OOMKilled'`); n != 1 {
		t.Errorf("%d completed investigations, want 1", n)
	}
	if _, ok := lease(t, s, t0.Add(leaseFor*2)); ok {
		t.Error("a completed investigation was leased")
	}
}

func TestFailInvestigation_RetriesThenGivesUp(t *testing.T) {
	s, _ := newStore(t)
	investigating(t, s)

	for attempt := 1; attempt <= store.MaxInvestigationAttempts; attempt++ {
		inv, ok := lease(t, s, t0)
		if !ok {
			t.Fatalf("attempt %d: nothing to lease", attempt)
		}
		var exhausted bool
		err := s.InTx(t.Context(), func(tx pgx.Tx) error {
			var err error
			exhausted, err = s.FailInvestigation(t.Context(), tx, inv.LeaseID, "model_unavailable", "connection refused")
			return err
		})
		if err != nil {
			t.Fatalf("FailInvestigation: %v", err)
		}
		if want := attempt == store.MaxInvestigationAttempts; exhausted != want {
			t.Errorf("attempt %d: exhausted = %v, want %v", attempt, exhausted, want)
		}
	}
	if _, ok := lease(t, s, t0); ok {
		t.Error("a failed investigation was leased")
	}
}

// An abandoned lease that used the last attempt is given up on, so its
// incident can be escalated.
func TestExpireInvestigations(t *testing.T) {
	s, _ := newStore(t)
	id := investigating(t, s)
	now := t0
	for range store.MaxInvestigationAttempts {
		if _, ok := lease(t, s, now); !ok {
			t.Fatal("nothing to lease")
		}
		now = now.Add(leaseFor + time.Second)
	}

	var expired []string
	err := s.InTx(t.Context(), func(tx pgx.Tx) error {
		var err error
		expired, err = s.ExpireInvestigations(t.Context(), tx, now)
		return err
	})
	if err != nil {
		t.Fatalf("ExpireInvestigations: %v", err)
	}
	if len(expired) != 1 || expired[0] != id {
		t.Errorf("expired = %v, want [%s]", expired, id)
	}
	if _, ok := lease(t, s, now); ok {
		t.Error("an exhausted investigation was leased")
	}
}

func TestEvidence_RecordsProvenance(t *testing.T) {
	s, pool := newStore(t)
	id := investigating(t, s)
	inv, _ := lease(t, s, t0)
	item := evidenceItem("0192f0c1-0000-7000-8000-00000000000b", evidence.ContainerLogsTail)
	item.Classification = evidence.ClassificationSensitive

	err := s.InTx(t.Context(), func(tx pgx.Tx) error {
		return s.InsertEvidence(t.Context(), tx, id, inv.LeaseID, map[string]string{"pod": "checkout-1"}, item)
	})
	if err != nil {
		t.Fatalf("InsertEvidence: %v", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM evidence_items WHERE id = $1 AND lease_id = $2 AND template = 'container_logs_tail' AND params->>'pod' = 'checkout-1'`, item.ID, inv.LeaseID); n != 1 {
		t.Error("the item was not stored with its lease, template and params")
	}

	items, err := s.Evidence(t.Context(), id)
	if err != nil {
		t.Fatalf("Evidence: %v", err)
	}
	if len(items) != 2 || items[1].ID != item.ID || items[1].Classification != evidence.ClassificationSensitive {
		t.Errorf("Evidence = %+v", items)
	}

	var requests int
	_ = s.InTx(t.Context(), func(tx pgx.Tx) error {
		requests, err = s.CountLeaseEvidence(t.Context(), tx, inv.LeaseID)
		return err
	})
	if requests != 1 {
		t.Errorf("CountLeaseEvidence = %d, want 1", requests)
	}
}

func TestMissingEvidence(t *testing.T) {
	s, _ := newStore(t)
	id := investigating(t, s)

	var missing []string
	err := s.InTx(t.Context(), func(tx pgx.Tx) error {
		var err error
		missing, err = s.MissingEvidence(t.Context(), tx, id, []string{
			"0192f0c1-0000-7000-8000-00000000000a", "0192f0c1-0000-7000-8000-0000000000ee", "not-a-uuid",
		})
		return err
	})
	if err != nil {
		t.Fatalf("MissingEvidence: %v", err)
	}
	if len(missing) != 2 {
		t.Errorf("missing = %v, want the unknown and the malformed IDs", missing)
	}
}

func TestPredicates_Stored(t *testing.T) {
	s, _ := newStore(t)
	id := investigating(t, s)

	results, err := s.Predicates(t.Context(), id)
	if err != nil {
		t.Fatalf("Predicates: %v", err)
	}
	if len(results) != 1 || results[0].Value != predicate.True || len(results[0].EvidenceIDs) != 1 {
		t.Errorf("Predicates = %+v", results)
	}
}

func TestIncidentInvestigation(t *testing.T) {
	s, _ := newStore(t)
	id := investigating(t, s)
	r, err := s.IncidentInvestigation(t.Context(), id)
	if err != nil || r.State != "pending" {
		t.Errorf("IncidentInvestigation = %+v, %v", r, err)
	}
}
