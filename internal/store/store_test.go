package store_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"
)

type testJobArgs struct{}

func (testJobArgs) Kind() string { return "test" }

var errRollback = errors.New("rollback")

func TestMigrate_Idempotent(t *testing.T) {
	pool := newTestDB(t)

	for i := range 2 {
		if err := store.Migrate(t.Context(), pool, nil); err != nil {
			t.Fatalf("Migrate run %d: %v", i+1, err)
		}
	}
}

// Everything written in the caller's transaction is rolled back with it:
// incidents, jobs, job batches, evidence gaps and rejected alerts.
func TestEnqueueIsTransactional(t *testing.T) {
	s, pool := newStore(t)

	err := s.InTx(t.Context(), func(tx pgx.Tx) error {
		id, _, err := s.CreateOrJoinIncident(t.Context(), tx, newIncident("fp-1"))
		if err != nil {
			return err
		}
		if err := s.EnqueueTx(t.Context(), tx, testJobArgs{}); err != nil {
			return err
		}
		if err := s.EnqueueManyTx(t.Context(), tx, []river.JobArgs{testJobArgs{}, testJobArgs{}}); err != nil {
			return err
		}
		if err := s.InsertEvidenceGap(t.Context(), tx, store.EvidenceGap{IncidentID: id, Template: evidence.PodStates, Cause: evidence.GapTimeout, At: t0}); err != nil {
			return err
		}
		if _, err := s.InsertRejectedAlert(t.Context(), tx, newRejection(store.RejectionInvalidAlert)); err != nil {
			return err
		}
		return errRollback
	})

	if !errors.Is(err, errRollback) {
		t.Fatalf("InTx err = %v, want the callback's error", err)
	}
	for _, table := range []string{"river_job", "incidents", "evidence_gaps", "rejected_alerts"} {
		if n := count(t, pool, "SELECT count(*) FROM "+table); n != 0 {
			t.Errorf("%s = %d rows, want 0", table, n)
		}
	}
}

type boundedJobArgs struct{}

func (boundedJobArgs) Kind() string { return "bounded" }

func (boundedJobArgs) InsertOpts() river.InsertOpts { return river.InsertOpts{MaxAttempts: 3} }

func TestEnqueueManyTx_CommitsAll(t *testing.T) {
	s, pool := newStore(t)

	err := s.InTx(t.Context(), func(tx pgx.Tx) error {
		return s.EnqueueManyTx(t.Context(), tx, []river.JobArgs{testJobArgs{}, testJobArgs{}, boundedJobArgs{}})
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}

	if n := count(t, pool, "SELECT count(*) FROM river_job"); n != 3 {
		t.Errorf("jobs = %d, want 3", n)
	}
	// Each job keeps its own args' insert options.
	if n := count(t, pool, "SELECT count(*) FROM river_job WHERE kind = 'bounded' AND max_attempts = 3"); n != 1 {
		t.Errorf("bounded jobs with max_attempts 3 = %d, want 1", n)
	}
}

func TestEnqueueManyTx_NoJobs(t *testing.T) {
	s, pool := newStore(t)

	err := s.InTx(t.Context(), func(tx pgx.Tx) error {
		return s.EnqueueManyTx(t.Context(), tx, nil)
	})
	if err != nil {
		t.Fatalf("EnqueueManyTx(nil): %v", err)
	}
	if n := count(t, pool, "SELECT count(*) FROM river_job"); n != 0 {
		t.Errorf("jobs = %d, want 0", n)
	}
}

// The dedup index predicate is written in SQL, but incident.TerminalStates is
// the source of truth. This test fails if the two drift.
func TestDedupIndexMatchesTerminalStates(t *testing.T) {
	_, pool := newStore(t)

	var def string
	err := pool.QueryRow(t.Context(),
		"SELECT indexdef FROM pg_indexes WHERE tablename = 'incidents' AND indexname = 'incidents_open_fingerprint'",
	).Scan(&def)
	if err != nil {
		t.Fatalf("read index incidents_open_fingerprint: %v", err)
	}

	if !strings.Contains(def, "UNIQUE") {
		t.Errorf("index is not unique: %s", def)
	}
	for _, s := range allStates {
		inPredicate := strings.Contains(def, "'"+string(s)+"'")
		if inPredicate != s.Terminal() {
			t.Errorf("state %s: in index predicate = %v, Terminal() = %v\nindex: %s", s, inPredicate, s.Terminal(), def)
		}
	}
}

// The incident_state enum is another copy of the state list in SQL. This test
// fails if it gains, loses or renames a value relative to Go.
func TestStateEnumMatchesGo(t *testing.T) {
	_, pool := newStore(t)

	rows, err := pool.Query(t.Context(), "SELECT unnest(enum_range(NULL::incident_state))::text")
	if err != nil {
		t.Fatalf("read enum incident_state: %v", err)
	}
	values, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("collect enum values: %v", err)
	}

	inEnum := make(map[string]bool, len(values))
	for _, v := range values {
		inEnum[v] = true
	}
	inGo := make(map[string]bool, len(allStates))
	for _, s := range allStates {
		inGo[string(s)] = true
		if !inEnum[string(s)] {
			t.Errorf("state %s is missing from enum incident_state", s)
		}
	}
	for _, v := range values {
		if !inGo[v] {
			t.Errorf("enum incident_state has %q, which is not a Go state", v)
		}
	}
}

func TestStateColumnsUseEnum(t *testing.T) {
	_, pool := newStore(t)

	columns := [][2]string{
		{"incidents", "state"},
		{"transitions", "from_state"},
		{"transitions", "to_state"},
	}
	for _, c := range columns {
		var udt string
		err := pool.QueryRow(t.Context(),
			"SELECT udt_name FROM information_schema.columns WHERE table_name = $1 AND column_name = $2",
			c[0], c[1],
		).Scan(&udt)
		if err != nil {
			t.Fatalf("read type of %s.%s: %v", c[0], c[1], err)
		}
		if udt != "incident_state" {
			t.Errorf("%s.%s has type %s, want incident_state", c[0], c[1], udt)
		}
	}
}

var allStates = []incident.State{
	incident.Received,
	incident.Investigating,
	incident.AwaitingSuggestion,
	incident.Diagnosed,
	incident.Inconclusive,
	incident.Recommended,
	incident.Proposed,
	incident.AwaitingApproval,
	incident.Authorized,
	incident.Executing,
	incident.Verifying,
	incident.Restoring,
	incident.Resolved,
	incident.ContainedEscalated,
	incident.Escalated,
	incident.ClosedNoAction,
	incident.SelfResolved,
	incident.Rejected,
	incident.Canceled,
}

// Upgrading moves a rollback's template hash into its action's mark, and a
// verification's counts into its start, so checks in flight carry on.
func TestMigrate_MovesMarksAndStarts(t *testing.T) {
	ctx := t.Context()
	s, pool := newStore(t)
	a := awaitingAction(t, s)
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		_, err := s.StartVerification(ctx, tx, store.Verification{ActionID: a.ID, WindowStart: t0, Start: []byte(`{"ready": 2, "restarts": 5}`)})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateDownTo(ctx, pool, 25); err != nil {
		t.Fatalf("down to 25: %v", err)
	}
	if err := store.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("up again: %v", err)
	}

	got, err := s.Action(ctx, s.DB(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mark != "sha256:t" {
		t.Errorf("mark = %q, want the template hash the rollback bound", got.Mark)
	}
	v, err := s.Verification(ctx, s.DB(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	var start map[string]int
	if err := json.Unmarshal(v.Start, &start); err != nil || start["ready"] != 2 || start["restarts"] != 5 {
		t.Errorf("start = %s, %v; want ready 2 and restarts 5", v.Start, err)
	}
}
