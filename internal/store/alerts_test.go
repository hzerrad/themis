package store_test

import (
	"slices"
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

// alertStep fires or resolves an alert of one episode.
type alertStep struct {
	resolve bool
	alert   string
	episode time.Time
}

func firing(alert string, episode time.Time) alertStep {
	return alertStep{alert: alert, episode: episode}
}

func resolving(alert string, episode time.Time) alertStep {
	return alertStep{resolve: true, alert: alert, episode: episode}
}

// An alert's status follows its episodes, and a resolution reports what is
// still firing on its incident.
func TestAlert_Lifecycle(t *testing.T) {
	for _, tc := range []struct {
		name      string
		steps     []alertStep
		incidents int    // incidents the last resolution returns
		firing    int    // alerts still firing on it
		status    string // alert a's status at the end, if it has one
	}{{
		name:      "the last alert resolved leaves none firing",
		steps:     []alertStep{firing("a", episode1), resolving("a", episode1)},
		incidents: 1, firing: 0, status: "resolved",
	}, {
		// One pod recovering does not leave the incident without firing alerts
		// while another pod still fails.
		name:      "other alerts still firing",
		steps:     []alertStep{firing("a", episode1), firing("b", episode1), resolving("a", episode1)},
		incidents: 1, firing: 1, status: "resolved",
	}, {
		// The resolution of a past episode does not end the current one.
		name:      "a stale resolution is ignored",
		steps:     []alertStep{firing("a", episode2), resolving("a", episode1)},
		incidents: 1, firing: 1, status: "firing",
	}, {
		// A firing notification that arrives after the resolution of the same
		// episode does not revive it.
		name:      "a late firing is ignored",
		steps:     []alertStep{firing("a", episode1), resolving("a", episode1), firing("a", episode1)},
		incidents: 1, firing: 0, status: "resolved",
	}, {
		name:      "a new episode fires again",
		steps:     []alertStep{firing("a", episode1), resolving("a", episode1), firing("a", episode2)},
		incidents: 1, firing: 0, status: "firing",
	}, {
		name:  "an unknown alert resolves nothing",
		steps: []alertStep{resolving("never-seen", episode1)},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			s, pool := newStore(t)
			id, _ := createOrJoin(t, s, newIncident("fp-1"))
			var got []store.AlertIncident
			for _, st := range tc.steps {
				if st.resolve {
					got = resolveAlert(t, s, st.alert, st.episode)
				} else {
					fire(t, s, id, st.alert, st.episode)
				}
			}
			if len(got) != tc.incidents {
				t.Fatalf("ResolveAlert = %+v, want %d incidents", got, tc.incidents)
			}
			if tc.incidents == 1 && (got[0].IncidentID != id || got[0].State != incident.Received || got[0].Firing != tc.firing) {
				t.Errorf("ResolveAlert = %+v, want incident %s in received with %d firing", got, id, tc.firing)
			}
			if tc.status != "" {
				if st := alertStatus(t, pool, id, "a"); st != tc.status {
					t.Errorf("alert status = %s, want %s", st, tc.status)
				}
			}
		})
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

var receivedAt = time.Date(2026, 10, 26, 9, 12, 5, 0, time.UTC)

func newRejection(reason store.AlertRejectionReason) store.RejectedAlertArgs {
	return store.RejectedAlertArgs{
		Source:     "alertmanager",
		ReceivedAt: receivedAt,
		Reason:     reason,
		Detail:     "namespace kube-system is not enrolled",
		AlertName:  "KubePodCrashLooping",
		Namespace:  "kube-system",
		Payload:    []byte(`{"status":"firing","labels":{"alertname":"KubePodCrashLooping"}}`),
	}
}

func reject(t *testing.T, s *store.Store, in store.RejectedAlertArgs) (string, error) {
	t.Helper()
	var id string
	err := s.InTx(t.Context(), func(tx pgx.Tx) error {
		var err error
		id, err = s.InsertRejectedAlert(t.Context(), tx, in)
		return err
	})
	return id, err
}

func TestInsertRejectedAlert_StoresEveryField(t *testing.T) {
	s, pool := newStore(t)
	before := time.Now()

	id, err := reject(t, s, newRejection(store.RejectionNotEnrolled))
	if err != nil {
		t.Fatalf("InsertRejectedAlert: %v", err)
	}
	// One row per rejection: the same alert rejected again is a second row.
	again, err := reject(t, s, newRejection(store.RejectionNotEnrolled))
	if err != nil {
		t.Fatalf("InsertRejectedAlert again: %v", err)
	}
	if again == id {
		t.Errorf("both rejections got ID %s", id)
	}
	if n := count(t, pool, `SELECT count(*) FROM rejected_alerts`); n != 2 {
		t.Errorf("%d rows, want 2", n)
	}

	var (
		source, reason, detail, alertName, namespace string
		gotReceived, rejectedAt                      time.Time
		payload                                      map[string]any
	)
	err = pool.QueryRow(t.Context(), `
		SELECT source, received_at, rejected_at, reason::text, detail, alert_name, namespace, payload
		FROM rejected_alerts WHERE id = $1`, id,
	).Scan(&source, &gotReceived, &rejectedAt, &reason, &detail, &alertName, &namespace, &payload)
	if err != nil {
		t.Fatalf("read rejection: %v", err)
	}

	if source != "alertmanager" || reason != "not_enrolled" || alertName != "KubePodCrashLooping" ||
		namespace != "kube-system" || detail != "namespace kube-system is not enrolled" {
		t.Errorf("row = %q %q %q %q %q", source, reason, detail, alertName, namespace)
	}
	if payload["status"] != "firing" {
		t.Errorf("payload = %v, want the alert JSON", payload)
	}
	// received_at is the caller's time, not the insert time.
	if !gotReceived.Equal(receivedAt) {
		t.Errorf("received_at = %v, want %v", gotReceived, receivedAt)
	}
	// rejected_at is set by the database at insert time.
	if rejectedAt.Before(before.Add(-time.Minute)) {
		t.Errorf("rejected_at = %v, want about now", rejectedAt)
	}
}

// Every Go reason is accepted by the database enum, and the enum has no value
// Go does not know: the two lists must match exactly.
func TestAlertRejectionReasons_MatchDatabaseEnum(t *testing.T) {
	s, pool := newStore(t)

	for _, reason := range store.AlertRejectionReasons() {
		if _, err := reject(t, s, newRejection(reason)); err != nil {
			t.Errorf("reason %q refused: %v", reason, err)
		}
	}

	rows, err := pool.Query(t.Context(), `SELECT unnest(enum_range(NULL::alert_rejection_reason))::text`)
	if err != nil {
		t.Fatalf("read enum: %v", err)
	}
	inDB, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read enum: %v", err)
	}
	var inGo []string
	for _, r := range store.AlertRejectionReasons() {
		inGo = append(inGo, string(r))
	}
	slices.Sort(inDB)
	slices.Sort(inGo)
	if !slices.Equal(inDB, inGo) {
		t.Errorf("database enum %v, Go reasons %v; want the same", inDB, inGo)
	}
}

func TestInsertRejectedAlert_UnknownReasonRefused(t *testing.T) {
	s, pool := newStore(t)

	_, err := reject(t, s, newRejection("made_up"))

	if err == nil {
		t.Fatal("InsertRejectedAlert accepted an unknown reason")
	}
	if n := count(t, pool, `SELECT count(*) FROM rejected_alerts`); n != 0 {
		t.Errorf("%d rows stored, want 0", n)
	}
}
