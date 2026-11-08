package store_test

import (
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/capability/rollback"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"
)

// Upgrading replaces the pod templates an earlier version stored with each
// fix's record: no row keeps a secret written as a plain value in the
// workload's environment, and the change people see is kept, the secret
// withheld. A fix awaiting approval whose record can't be worked out is
// canceled, saying why, and its incident waits for another suggestion.
func TestMigrate_KeepsRecordsInsteadOfTemplates(t *testing.T) {
	const (
		pre     = `{"spec":{"containers":[{"name":"app","image":"shop/checkout:1.5.0","env":[{"name":"DB_PASSWORD","value":"hunter2"}]}]}}`
		desired = `{"spec":{"containers":[{"name":"app","image":"shop/checkout:1.4.1","env":[{"name":"DB_PASSWORD","value":"hunter1"}]}]}}`
	)
	ctx := t.Context()
	s, pool := newStore(t)
	applied := awaitingAction(t, s)
	pending := applied
	pending.Capability = "deployment.retired-fix"
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE actions SET state = 'succeeded' WHERE id = $1`, applied.ID); err != nil {
			return err
		}
		var err error
		if pending.ID, err = s.InsertAction(ctx, tx, pending); err != nil {
			return err
		}
		return s.TransitionPath(ctx, tx, applied.IncidentID, "test", "test", incident.Investigating, incident.Diagnosed, incident.Proposed, incident.AwaitingApproval)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateDownTo(ctx, pool, 36); err != nil {
		t.Fatalf("down to 36: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE actions SET pre_state = $1, desired_state = $2`, pre, desired); err != nil {
		t.Fatal(err)
	}

	if err := store.Migrate(ctx, pool, catalog.Fixes().Record); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for _, secret := range []string{"hunter1", "hunter2"} {
		for _, table := range tables(t, pool) {
			if n := count(t, pool, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()+` r WHERE strpos(r::text, $1) > 0`, secret); n > 0 {
				t.Errorf("%d rows of %s hold %q", n, table, secret)
			}
		}
	}
	got, err := s.Action(ctx, s.DB(), applied.ID)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := rollback.Capability{}.Changes(got.Record)
	hidden := capability.Change{Container: "app", Field: "env DB_PASSWORD", Before: "hidden (sensitive name)", After: "hidden (sensitive name)", Withheld: capability.Secret}
	if err != nil || got.State != store.ActionSucceeded || !slices.Contains(changes, hidden) ||
		!slices.Contains(changes, capability.Change{Container: "app", Field: "image", Before: "shop/checkout:1.5.0", After: "shop/checkout:1.4.1"}) {
		t.Errorf("the applied fix: %s, changes %+v (%v); want the image and the hidden password", got.State, changes, err)
	}

	canceled, err := s.Action(ctx, s.DB(), pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	trail, err := s.Transitions(ctx, applied.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	last := trail[len(trail)-1]
	const why = "Approval canceled. Themis no longer stores pod templates, which can hold secrets, and couldn't record this fix without its template. Suggest the fix again."
	if canceled.State != store.ActionCanceled || last.To != incident.AwaitingSuggestion || last.Reason != why {
		t.Errorf("the fix awaiting approval: %s, incident moved to %s (%q); want canceled, waiting for a suggestion", canceled.State, last.To, last.Reason)
	}
}

// tables are the tables of Themis's database.
func tables(t *testing.T, q store.Querier) []string {
	t.Helper()
	rows, err := q.Query(t.Context(), `SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_type = 'BASE TABLE'`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}
