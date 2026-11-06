package investigation_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/catalog/capability/rollback"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

// applied records that Themis applied a fix for incident id that left the
// target with the template mark names.
func (e *env) applied(t *testing.T, id, mark string, at time.Time) {
	t.Helper()
	err := e.store.InTx(t.Context(), func(tx pgx.Tx) error {
		p, _, err := e.store.InsertProposal(t.Context(), tx, store.Proposal{IncidentID: id, LeaseID: "0192f0c1-0000-7000-8000-000000000001", ClientProposalID: "p", Capability: rollback.Name, CapabilityVersion: "1", Target: e.target, Params: map[string]any{}, Status: store.ProposalAccepted})
		if err != nil {
			return err
		}
		tg := e.target
		actionID, err := e.store.InsertAction(t.Context(), tx, store.Action{
			IncidentID: id, ProposalID: p.ID, Capability: rollback.Name, CapabilityVersion: "1",
			Target: digest.TargetIdentity{Group: tg.Group, Kind: tg.Kind, Namespace: tg.Namespace, Name: tg.Name, UID: tg.UID},
			Params: map[string]any{"revision": int64(1)}, Mark: mark, BlastRadius: map[string]any{}, Constraints: map[string]any{}, State: store.ActionSucceeded, Digest: "sha256:d",
		})
		if err != nil {
			return err
		}
		return e.store.RecordExecution(t.Context(), tx, store.Execution{ActionID: actionID, Status: "succeeded", Message: "Rolled back to revision 1.", StartedAt: at, FinishedAt: at})
	})
	if err != nil {
		t.Fatal(err)
	}
}

// An incident that opens on a workload running Themis's own fix follows the
// incident whose fix it was: its evidence says what Themis did, when, and
// what the earlier incident's checks found of its failure. One that opens on
// a template Themis didn't leave follows nothing.
func TestPrepare_FollowsThemisFix(t *testing.T) {
	e := newEnv(t)
	p := profile()
	p.Symptoms = []string{"oom_killed_new_revision"}
	cfg := e.worker.Config.Current()
	cfg.Profiles = append(cfg.Profiles[:0:0], p)
	e.worker.Config = cfg

	earlier := e.newIncident(t, "rollout-regression", e.target)
	if err := e.worker.Prepare(t.Context(), earlier); err != nil {
		t.Fatal(err)
	}
	in, err := e.store.Incident(t.Context(), earlier)
	if err != nil {
		t.Fatal(err)
	}
	at := e.now.Add(-4 * time.Minute)
	e.applied(t, earlier, in.ObservedSpecHash, at)
	if _, err := e.pool.Exec(t.Context(), `UPDATE incidents SET state = 'resolved' WHERE id = $1`, earlier); err != nil {
		t.Fatal(err)
	}

	next := e.newIncident(t, "rollout-regression", e.target)
	if err := e.worker.Prepare(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	var data []byte
	if err := e.pool.QueryRow(t.Context(), `SELECT data FROM evidence_items WHERE incident_id = $1 AND template = $2`, next, evidence.ThemisFix).Scan(&data); err != nil {
		t.Fatalf("no evidence of Themis's fix: %v", err)
	}
	var got evidence.ThemisFixView
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Incident != earlier || got.Fix != rollback.Name || got.Done != "Rolled back to revision 1." || !got.AppliedAt.Equal(at) || got.Replaced != "2" ||
		len(got.Found) != 1 || got.Found[0].Check != "oom_killed_new_revision" {
		t.Errorf("Themis's fix = %+v", got)
	}

	// The workload changed since: it runs no fix of Themis's any more.
	if _, err := e.pool.Exec(t.Context(), `UPDATE actions SET mark = 'sha256:another' WHERE incident_id = $1`, earlier); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(t.Context(), `UPDATE incidents SET state = 'resolved' WHERE id = $1`, next); err != nil {
		t.Fatal(err)
	}
	unrelated := e.newIncident(t, "rollout-regression", e.target)
	if err := e.worker.Prepare(t.Context(), unrelated); err != nil {
		t.Fatal(err)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM evidence_items WHERE incident_id = $1 AND template = $2`, unrelated, evidence.ThemisFix); n != 0 {
		t.Errorf("%d items of Themis's fix for a template it didn't leave, want none", n)
	}
}
