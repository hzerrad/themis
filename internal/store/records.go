package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pressly/goose/v3"
)

// Records works out the record a fix keeps of its change from the pod
// templates before and after it, which an earlier version of Themis stored
// whole, as capability.Registry.Record does.
type Records func(capability string, params map[string]any, before, after json.RawMessage) (json.RawMessage, error)

// recordsVersion numbers the migration that stopped actions keeping pod
// templates. It is written in Go, since a record takes its fix to work out:
// the next SQL migration is 00038.
const recordsVersion = 37

// keepRecords is the migration that replaces the pod templates actions kept,
// before and after each change, with each fix's record of it. A template's
// environment can hold secrets, and Themis needs neither template: the
// executor reads the target and its ReplicaSets again, and checks their
// hashes. An action awaiting approval or execution whose record can't be
// worked out is canceled, with its incident, since nobody could see what
// it changes.
func keepRecords(records Records) *goose.Migration {
	return goose.NewGoMigration(recordsVersion,
		&goose.GoFunc{RunTx: func(ctx context.Context, tx *sql.Tx) error { return upRecords(ctx, tx, records) }},
		&goose.GoFunc{RunTx: downRecords})
}

// stored is an action as an earlier version of Themis stored it.
type stored struct {
	id, incident, capability, state string
	params, before, after           []byte
}

func upRecords(ctx context.Context, tx *sql.Tx, records Records) error {
	if _, err := tx.ExecContext(ctx, `ALTER TABLE actions ADD COLUMN record jsonb NOT NULL DEFAULT '{}'`); err != nil {
		return fmt.Errorf("add records: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id::text, incident_id::text, capability, state::text, params, pre_state, desired_state FROM actions ORDER BY created_at, id`)
	if err != nil {
		return fmt.Errorf("read actions: %w", err)
	}
	var actions []stored
	for rows.Next() {
		var a stored
		if err := rows.Scan(&a.id, &a.incident, &a.capability, &a.state, &a.params, &a.before, &a.after); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read actions: %w", err)
		}
		actions = append(actions, a)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("read actions: %w", err)
	}
	for _, a := range actions {
		record, err := a.record(records)
		if err != nil {
			record = []byte(`{}`)
			if err := a.cancel(ctx, tx); err != nil {
				return err
			}
		}
		// The templates are cleared before the columns go, so no row Themis
		// writes from here on holds them.
		if _, err := tx.ExecContext(ctx, `UPDATE actions SET record = $2, pre_state = '{}', desired_state = '{}' WHERE id = $1`, a.id, record); err != nil {
			return fmt.Errorf("record of action %s: %w", a.id, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE actions DROP COLUMN pre_state, DROP COLUMN desired_state`); err != nil {
		return fmt.Errorf("drop templates: %w", err)
	}
	return nil
}

func downRecords(ctx context.Context, tx *sql.Tx) error {
	// The templates are gone for good: they come back empty.
	_, err := tx.ExecContext(ctx, `
	ALTER TABLE actions ADD COLUMN pre_state jsonb NOT NULL DEFAULT '{}', ADD COLUMN desired_state jsonb NOT NULL DEFAULT '{}';
	ALTER TABLE actions DROP COLUMN record;`)
	return err
}

// errNoTemplates means an action stored no templates to work its record out
// from.
var errNoTemplates = errors.New("no templates")

// record works out a's record with records.
func (a stored) record(records Records) (json.RawMessage, error) {
	for _, t := range [][]byte{a.before, a.after} {
		if t := bytes.TrimSpace(t); len(t) == 0 || bytes.Equal(t, []byte(`{}`)) || bytes.Equal(t, []byte(`null`)) {
			return nil, errNoTemplates
		}
	}
	if records == nil {
		return nil, errors.New("no fixes to work records out with")
	}
	var params map[string]any
	dec := json.NewDecoder(bytes.NewReader(a.params))
	dec.UseNumber()
	if err := dec.Decode(&params); err != nil {
		return nil, err
	}
	return records(a.capability, normalizeNumbers(params), a.before, a.after)
}

// unrecorded is, for each state of an action not yet applied, where its
// incident goes when the action is canceled for want of a record, and why.
// A restore isn't canceled: the executor needs no record to apply it, and
// the incident stays restoring until it does.
var unrecorded = map[string]struct{ from, to, reason string }{
	string(ActionAwaitingApproval): {
		"awaiting_approval", "awaiting_suggestion",
		"Approval canceled. Themis no longer stores pod templates, which can hold secrets, and couldn't record this fix without its template. Suggest the fix again.",
	},
	string(ActionAuthorized): {
		"authorized", "canceled",
		"Not applied. Themis no longer stores pod templates, which can hold secrets, and couldn't record this fix without its template.",
	},
}

// cancel cancels a, when it is a fix not yet applied, with its incident.
func (a stored) cancel(ctx context.Context, tx *sql.Tx) error {
	move, ok := unrecorded[a.state]
	if !ok || bytes.Contains(a.params, []byte(`"restores"`)) {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE actions SET state = 'canceled', updated_at = now() WHERE id = $1`, a.id); err != nil {
		return fmt.Errorf("cancel action %s: %w", a.id, err)
	}
	res, err := tx.ExecContext(ctx, `UPDATE incidents SET state = $3, updated_at = now() WHERE id = $1 AND state = $2`, a.incident, move.from, move.to)
	if err != nil {
		return fmt.Errorf("move incident %s: %w", a.incident, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO transitions (incident_id, from_state, to_state, reason, actor) VALUES ($1, $2, $3, $4, 'execution')`,
		a.incident, move.from, move.to, move.reason); err != nil {
		return fmt.Errorf("move incident %s: %w", a.incident, err)
	}
	return nil
}
