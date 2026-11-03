package store_test

import (
	"testing"
	"time"
)

func TestSetup_FinishesOnce(t *testing.T) {
	s, pool := newStore(t)
	if finished, err := s.SetupFinished(t.Context()); err != nil || finished {
		t.Fatalf("SetupFinished on a new install = %v, %v; want false", finished, err)
	}
	if err := s.FinishSetup(t.Context(), "alice@example.com", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishSetup(t.Context(), "bob@example.com", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if finished, err := s.SetupFinished(t.Context()); err != nil || !finished {
		t.Fatalf("SetupFinished = %v, %v; want true", finished, err)
	}
	var by string
	if err := pool.QueryRow(t.Context(), `SELECT finished_by FROM setup`).Scan(&by); err != nil || by != "alice@example.com" {
		t.Errorf("finished_by = %q, %v; want the first admin", by, err)
	}
}
