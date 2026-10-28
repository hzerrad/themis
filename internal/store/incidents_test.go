package store_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"
)

func newIncident(fingerprint string) store.IncidentArgs {
	return store.IncidentArgs{
		Fingerprint: fingerprint,
		Profile:     "rollout-regression",
		Target: incident.TargetRef{
			Group:     "apps",
			Kind:      "Deployment",
			Namespace: "shop",
			Name:      "checkout",
			UID:       "6f1c2a9e-3b7d-4e8a-9c0f-1d2e3f4a5b6c",
		},
		Payload: []byte(`{"alertname":"KubePodCrashLooping"}`),
	}
}

func createOrJoin(t *testing.T, s *store.Store, in store.IncidentArgs) (string, bool) {
	t.Helper()
	var (
		id      string
		created bool
	)
	err := s.InTx(t.Context(), func(tx pgx.Tx) error {
		var err error
		id, created, err = s.CreateOrJoinIncident(t.Context(), tx, in)
		return err
	})
	if err != nil {
		t.Fatalf("CreateOrJoinIncident: %v", err)
	}
	return id, created
}

func transition(t *testing.T, s *store.Store, id string, from, to incident.State) error {
	t.Helper()
	return s.InTx(t.Context(), func(tx pgx.Tx) error {
		return s.Transition(t.Context(), tx, store.TransitionArgs{
			ID: id, From: from, To: to, Reason: "test", Actor: "test",
		})
	})
}

// advance walks an incident along path, failing the test on any error.
func advance(t *testing.T, s *store.Store, id string, path ...incident.State) {
	t.Helper()
	for i := 1; i < len(path); i++ {
		if err := transition(t, s, id, path[i-1], path[i]); err != nil {
			t.Fatalf("Transition(%s, %s): %v", path[i-1], path[i], err)
		}
	}
}

func stateOf(t *testing.T, pool *pgxpool.Pool, id string) incident.State {
	t.Helper()
	var st incident.State
	if err := pool.QueryRow(t.Context(), "SELECT state FROM incidents WHERE id = $1", id).Scan(&st); err != nil {
		t.Fatalf("read state: %v", err)
	}
	return st
}

func TestCreateOrJoin_CreatesReceivedIncident(t *testing.T) {
	s, pool := newStore(t)

	id, created := createOrJoin(t, s, newIncident("fp-1"))

	if !created {
		t.Error("created = false, want true")
	}
	u, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("id %q is not a UUID: %v", id, err)
	}
	if u.Version() != 7 {
		t.Errorf("id version = %d, want 7", u.Version())
	}
	if st := stateOf(t, pool, id); st != incident.Received {
		t.Errorf("state = %s, want %s", st, incident.Received)
	}
	if n := count(t, pool, "SELECT occurrence_count FROM incidents WHERE id = $1", id); n != 1 {
		t.Errorf("occurrence_count = %d, want 1", n)
	}
	if n := count(t, pool, "SELECT count(*) FROM occurrences WHERE incident_id = $1", id); n != 1 {
		t.Errorf("occurrence rows = %d, want 1", n)
	}
}

func TestCreateOrJoin_DedupesNonTerminal(t *testing.T) {
	s, pool := newStore(t)

	id1, created1 := createOrJoin(t, s, newIncident("fp-1"))
	id2, created2 := createOrJoin(t, s, newIncident("fp-1"))

	if id1 != id2 {
		t.Errorf("ids differ: %s and %s", id1, id2)
	}
	if !created1 || created2 {
		t.Errorf("created = %v then %v, want true then false", created1, created2)
	}
	if n := count(t, pool, "SELECT occurrence_count FROM incidents WHERE id = $1", id1); n != 2 {
		t.Errorf("occurrence_count = %d, want 2", n)
	}
	if n := count(t, pool, "SELECT count(*) FROM occurrences WHERE incident_id = $1", id1); n != 2 {
		t.Errorf("occurrence rows = %d, want 2", n)
	}
	if n := count(t, pool, "SELECT count(*) FROM incidents"); n != 1 {
		t.Errorf("incidents = %d, want 1", n)
	}
}

func TestCreateOrJoin_DifferentFingerprintsAreSeparate(t *testing.T) {
	s, _ := newStore(t)

	id1, _ := createOrJoin(t, s, newIncident("fp-1"))
	id2, created := createOrJoin(t, s, newIncident("fp-2"))

	if id1 == id2 || !created {
		t.Errorf("fp-2 joined fp-1's incident")
	}
}

func TestCreateOrJoin_NewAfterTerminal(t *testing.T) {
	s, pool := newStore(t)

	id1, _ := createOrJoin(t, s, newIncident("fp-1"))
	advance(t, s, id1, incident.Received, incident.Rejected)

	id2, created := createOrJoin(t, s, newIncident("fp-1"))

	if id1 == id2 || !created {
		t.Fatal("a terminal incident was joined instead of a new one being created")
	}
	if n := count(t, pool, "SELECT occurrence_count FROM incidents WHERE id = $1", id1); n != 1 {
		t.Errorf("terminal incident occurrence_count = %d, want 1 (unchanged)", n)
	}
}

func TestCreateOrJoin_ConcurrentSameFingerprint(t *testing.T) {
	s, pool := newStore(t)
	const workers = 10

	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		ids     = make([]string, workers)
		created = make([]bool, workers)
		errs    = make([]error, workers)
	)
	for i := range workers {
		wg.Go(func() {
			<-start
			errs[i] = s.InTx(t.Context(), func(tx pgx.Tx) error {
				var err error
				ids[i], created[i], err = s.CreateOrJoinIncident(t.Context(), tx, newIncident("fp-1"))
				return err
			})
		})
	}
	close(start)
	wg.Wait()

	nCreated := 0
	for i := range workers {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Errorf("worker %d got id %s, want %s", i, ids[i], ids[0])
		}
		if created[i] {
			nCreated++
		}
	}
	if nCreated != 1 {
		t.Errorf("created = true for %d workers, want exactly 1", nCreated)
	}
	if n := count(t, pool, "SELECT occurrence_count FROM incidents WHERE id = $1", ids[0]); n != workers {
		t.Errorf("occurrence_count = %d, want %d", n, workers)
	}
}

func TestTransition_WritesAuditRow(t *testing.T) {
	s, pool := newStore(t)
	id, _ := createOrJoin(t, s, newIncident("fp-1"))
	before := time.Now()

	err := s.InTx(t.Context(), func(tx pgx.Tx) error {
		return s.Transition(t.Context(), tx, store.TransitionArgs{
			ID: id, From: incident.Received, To: incident.Investigating, Reason: "leased", Actor: "worker-1",
		})
	})
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}

	if st := stateOf(t, pool, id); st != incident.Investigating {
		t.Errorf("state = %s, want %s", st, incident.Investigating)
	}
	var (
		from, to      incident.State
		reason, actor string
		at            time.Time
	)
	err = pool.QueryRow(t.Context(),
		"SELECT from_state, to_state, reason, actor, at FROM transitions WHERE incident_id = $1", id,
	).Scan(&from, &to, &reason, &actor, &at)
	if err != nil {
		t.Fatalf("read transition: %v", err)
	}
	if from != incident.Received || to != incident.Investigating {
		t.Errorf("transition = %s -> %s, want %s -> %s", from, to, incident.Received, incident.Investigating)
	}
	if reason != "leased" || actor != "worker-1" {
		t.Errorf("reason, actor = %q, %q, want %q, %q", reason, actor, "leased", "worker-1")
	}
	if at.Before(before.Add(-time.Minute)) || at.After(time.Now().Add(time.Minute)) {
		t.Errorf("at = %v, want close to now", at)
	}
}

func TestTransition_StateConflict(t *testing.T) {
	s, pool := newStore(t)
	id, _ := createOrJoin(t, s, newIncident("fp-1"))

	err := transition(t, s, id, incident.Investigating, incident.Diagnosed)

	if !errors.Is(err, store.ErrStateConflict) {
		t.Fatalf("err = %v, want ErrStateConflict", err)
	}
	if st := stateOf(t, pool, id); st != incident.Received {
		t.Errorf("state = %s, want %s (unchanged)", st, incident.Received)
	}
	if n := count(t, pool, "SELECT count(*) FROM transitions WHERE incident_id = $1", id); n != 0 {
		t.Errorf("transition rows = %d, want 0", n)
	}
}

func TestTransition_IllegalTransition(t *testing.T) {
	s, pool := newStore(t)
	id, _ := createOrJoin(t, s, newIncident("fp-1"))

	err := transition(t, s, id, incident.Received, incident.Executing)

	if !errors.Is(err, store.ErrIllegalTransition) {
		t.Fatalf("err = %v, want ErrIllegalTransition", err)
	}
	if st := stateOf(t, pool, id); st != incident.Received {
		t.Errorf("state = %s, want %s (unchanged)", st, incident.Received)
	}
	if n := count(t, pool, "SELECT count(*) FROM transitions WHERE incident_id = $1", id); n != 0 {
		t.Errorf("transition rows = %d, want 0", n)
	}
}

// An executor claim and a human cancel race on the same authorized incident.
// Exactly one must win; the other sees a state conflict.
func TestTransition_ConcurrentClaimAndCancel(t *testing.T) {
	s, pool := newStore(t)
	id, _ := createOrJoin(t, s, newIncident("fp-1"))
	advance(t, s, id,
		incident.Received, incident.Investigating, incident.Diagnosed,
		incident.Proposed, incident.Authorized)

	targets := []incident.State{incident.Executing, incident.Canceled}
	errs := make([]error, len(targets))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, to := range targets {
		wg.Go(func() {
			<-start
			errs[i] = transition(t, s, id, incident.Authorized, to)
		})
	}
	close(start)
	wg.Wait()

	wins := 0
	for i, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, store.ErrStateConflict):
		default:
			t.Errorf("Transition(authorized, %s): unexpected error %v", targets[i], err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d transitions succeeded, want exactly 1", wins)
	}
	if n := count(t, pool, "SELECT count(*) FROM transitions WHERE incident_id = $1 AND from_state = 'authorized'", id); n != 1 {
		t.Errorf("transition rows from authorized = %d, want 1", n)
	}
}

func TestIncident(t *testing.T) {
	s, _ := newStore(t)
	id, _ := createOrJoin(t, s, newIncident("fp-get"))

	in, err := s.Incident(t.Context(), id)
	if err != nil {
		t.Fatalf("Incident: %v", err)
	}
	if in.State != incident.Received || in.Profile != "rollout-regression" || in.Target.Name != "checkout" || in.OccurrenceCount != 1 {
		t.Errorf("Incident = %+v", in)
	}
	if _, err := s.Incident(t.Context(), uuid.NewString()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown ID: err = %v, want ErrNotFound", err)
	}
	if _, err := s.Incident(t.Context(), "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("malformed ID: err = %v, want ErrNotFound", err)
	}
}

func TestListIncidentsAndTransitions(t *testing.T) {
	s, _ := newStore(t)
	first, _ := createOrJoin(t, s, newIncident("fp-a"))
	second, _ := createOrJoin(t, s, newIncident("fp-b"))
	advance(t, s, first, incident.Received, incident.Investigating)

	list, err := s.ListIncidents(t.Context(), 10)
	if err != nil || len(list) != 2 || list[0].ID != second {
		t.Fatalf("ListIncidents = %v, %v; want newest first", list, err)
	}
	trail, err := s.Transitions(t.Context(), first)
	if err != nil || len(trail) != 1 || trail[0].To != incident.Investigating {
		t.Errorf("Transitions = %v, %v", trail, err)
	}
}
