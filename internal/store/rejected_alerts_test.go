package store_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/store"
)

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

// The rejection is part of the caller's transaction: rolled back with it.
func TestInsertRejectedAlert_RolledBackWithCaller(t *testing.T) {
	s, pool := newStore(t)
	errLater := errors.New("a later step failed")

	err := s.InTx(t.Context(), func(tx pgx.Tx) error {
		if _, err := s.InsertRejectedAlert(t.Context(), tx, newRejection(store.RejectionInvalidAlert)); err != nil {
			return err
		}
		return errLater
	})

	if !errors.Is(err, errLater) {
		t.Fatalf("InTx error = %v, want %v", err, errLater)
	}
	if n := count(t, pool, `SELECT count(*) FROM rejected_alerts`); n != 0 {
		t.Errorf("%d rows stored after rollback, want 0", n)
	}
}

// One row per rejection: the same alert rejected twice is two rows.
func TestInsertRejectedAlert_OneRowPerRejection(t *testing.T) {
	s, pool := newStore(t)

	first, err1 := reject(t, s, newRejection(store.RejectionNoProfile))
	second, err2 := reject(t, s, newRejection(store.RejectionNoProfile))

	if err1 != nil || err2 != nil {
		t.Fatalf("InsertRejectedAlert: %v, %v", err1, err2)
	}
	if first == second {
		t.Errorf("both rejections got ID %s", first)
	}
	if n := count(t, pool, `SELECT count(*) FROM rejected_alerts`); n != 2 {
		t.Errorf("%d rows, want 2", n)
	}
}
