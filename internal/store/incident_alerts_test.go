package store_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"
)

var (
	episode1 = time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	episode2 = episode1.Add(time.Hour)
)

func fire(t *testing.T, s *store.Store, incidentID, sourceID string, startsAt time.Time) {
	t.Helper()
	err := s.InTx(t.Context(), func(tx pgx.Tx) error {
		return s.RecordFiringAlert(t.Context(), tx, store.FiringAlertArgs{
			IncidentID: incidentID, Source: "alertmanager", SourceID: sourceID, StartsAt: startsAt,
		})
	})
	if err != nil {
		t.Fatalf("RecordFiringAlert: %v", err)
	}
}

func resolveAlert(t *testing.T, s *store.Store, sourceID string, startsAt time.Time) []store.AlertIncident {
	t.Helper()
	var got []store.AlertIncident
	err := s.InTx(t.Context(), func(tx pgx.Tx) error {
		var err error
		got, err = s.ResolveAlert(t.Context(), tx, store.ResolvedAlertArgs{
			Source: "alertmanager", SourceID: sourceID, StartsAt: startsAt, EndsAt: startsAt.Add(10 * time.Minute),
		})
		return err
	})
	if err != nil {
		t.Fatalf("ResolveAlert: %v", err)
	}
	return got
}

func alertStatus(t *testing.T, pool *pgxpool.Pool, incidentID, sourceID string) string {
	t.Helper()
	var status string
	err := pool.QueryRow(t.Context(),
		`SELECT status::text FROM incident_alerts WHERE incident_id = $1 AND source_id = $2`, incidentID, sourceID,
	).Scan(&status)
	if err != nil {
		t.Fatalf("read alert status: %v", err)
	}
	return status
}

func TestResolveAlert_LastAlertLeavesNoneFiring(t *testing.T) {
	s, pool := newStore(t)
	id, _ := createOrJoin(t, s, newIncident("fp-1"))
	fire(t, s, id, "a", episode1)

	got := resolveAlert(t, s, "a", episode1)

	if len(got) != 1 || got[0].IncidentID != id || got[0].State != incident.Received || got[0].Firing != 0 {
		t.Fatalf("ResolveAlert = %+v, want incident %s in received with 0 firing", got, id)
	}
	if st := alertStatus(t, pool, id, "a"); st != "resolved" {
		t.Errorf("alert status = %s, want resolved", st)
	}
}

// One pod recovering does not leave the incident without firing alerts while
// another pod still fails.
func TestResolveAlert_OtherAlertsStillFiring(t *testing.T) {
	s, _ := newStore(t)
	id, _ := createOrJoin(t, s, newIncident("fp-1"))
	fire(t, s, id, "a", episode1)
	fire(t, s, id, "b", episode1)

	got := resolveAlert(t, s, "a", episode1)

	if len(got) != 1 || got[0].Firing != 1 {
		t.Fatalf("ResolveAlert = %+v, want 1 alert still firing", got)
	}
}

// The resolution of a past episode does not end the current one.
func TestResolveAlert_StaleResolutionIgnored(t *testing.T) {
	s, pool := newStore(t)
	id, _ := createOrJoin(t, s, newIncident("fp-1"))
	fire(t, s, id, "a", episode2)

	got := resolveAlert(t, s, "a", episode1)

	if len(got) != 1 || got[0].Firing != 1 {
		t.Fatalf("ResolveAlert = %+v, want the alert still firing", got)
	}
	if st := alertStatus(t, pool, id, "a"); st != "firing" {
		t.Errorf("alert status = %s, want firing", st)
	}
}

// A firing notification that arrives after the resolution of the same episode
// does not revive it.
func TestRecordFiringAlert_LateFiringIgnored(t *testing.T) {
	s, pool := newStore(t)
	id, _ := createOrJoin(t, s, newIncident("fp-1"))
	fire(t, s, id, "a", episode1)
	resolveAlert(t, s, "a", episode1)

	fire(t, s, id, "a", episode1)

	if st := alertStatus(t, pool, id, "a"); st != "resolved" {
		t.Errorf("alert status = %s, want resolved", st)
	}
}

// A new episode of the same alert fires again.
func TestRecordFiringAlert_NewEpisodeFires(t *testing.T) {
	s, pool := newStore(t)
	id, _ := createOrJoin(t, s, newIncident("fp-1"))
	fire(t, s, id, "a", episode1)
	resolveAlert(t, s, "a", episode1)

	fire(t, s, id, "a", episode2)

	if st := alertStatus(t, pool, id, "a"); st != "firing" {
		t.Errorf("alert status = %s, want firing", st)
	}
}

// Closed incidents are not returned: there is nothing left to resolve.
func TestResolveAlert_IgnoresTerminalIncidents(t *testing.T) {
	s, _ := newStore(t)
	id, _ := createOrJoin(t, s, newIncident("fp-1"))
	fire(t, s, id, "a", episode1)
	advance(t, s, id, incident.Received, incident.Canceled)

	if got := resolveAlert(t, s, "a", episode1); len(got) != 0 {
		t.Errorf("ResolveAlert = %+v, want no incidents", got)
	}
}

func TestResolveAlert_UnknownAlert(t *testing.T) {
	s, _ := newStore(t)

	if got := resolveAlert(t, s, "never-seen", episode1); len(got) != 0 {
		t.Errorf("ResolveAlert = %+v, want no incidents", got)
	}
}
