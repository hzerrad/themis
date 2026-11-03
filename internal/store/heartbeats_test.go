package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/store"
)

func TestHeartbeat_KeepsTheLatest(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.LastHeartbeat(t.Context(), store.HeartbeatInvestigator); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("LastHeartbeat before any: %v, want ErrNotFound", err)
	}
	for _, at := range []time.Time{t0.Add(time.Minute), t0} {
		if err := s.Beat(t.Context(), store.HeartbeatInvestigator, "worker-1", at); err != nil {
			t.Fatal(err)
		}
	}
	h, err := s.LastHeartbeat(t.Context(), store.HeartbeatInvestigator)
	if err != nil || !h.SeenAt.Equal(t0.Add(time.Minute)) || h.Detail != "worker-1" {
		t.Errorf("LastHeartbeat = %+v, %v; want the later time", h, err)
	}
}

func TestLastInvestigationOutcome(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.LastInvestigationOutcome(t.Context()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("before any: %v, want ErrNotFound", err)
	}
	investigating(t, s)
	inv, _ := lease(t, s, t0)
	err := s.InTx(t.Context(), func(tx pgx.Tx) error {
		_, err := s.FailInvestigation(t.Context(), tx, inv.LeaseID, "invalid_model_output", "The model's answers weren't in the required format", "no JSON", t0.Add(time.Minute))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	o, err := s.LastInvestigationOutcome(t.Context())
	if err != nil || o.Completed || o.FailureKind != "invalid_model_output" || o.Failure == "" {
		t.Fatalf("after a failed attempt = %+v, %v", o, err)
	}
	// The retry succeeds, and that's what counts now.
	inv, _ = lease(t, s, t0.Add(2*time.Minute))
	err = s.InTx(t.Context(), func(tx pgx.Tx) error {
		return s.CompleteInvestigation(t.Context(), tx, inv.LeaseID, store.InvestigationResult{
			Conclusion: "proposed", Summary: "s", Hypotheses: []byte(`[]`), Usage: []byte(`{}`), Provenance: []byte(`{"model":"qwen3"}`),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if o, err = s.LastInvestigationOutcome(t.Context()); err != nil || !o.Completed || o.Model != "qwen3" {
		t.Errorf("after the retry completed = %+v, %v", o, err)
	}
}
