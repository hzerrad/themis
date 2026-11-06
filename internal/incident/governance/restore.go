package governance

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/governance/policy"
	"github.com/hzerrad/themis/internal/store"
)

// RestoreWorker works restore jobs.
type RestoreWorker struct {
	river.WorkerDefaults[RestoreArgs]

	Governor *Worker
}

func (w *RestoreWorker) Work(ctx context.Context, job *river.Job[RestoreArgs]) error {
	err := w.Governor.Restore(ctx, job.Args.ActionID)
	if err != nil && job.Attempt >= job.MaxAttempts {
		// The fix is still applied: never leave the incident in restoring.
		return w.Governor.escalateAction(ctx, job.Args.ActionID, "Not restored: "+err.Error()+". The fix stays in place.")
	}
	return err
}

// Restore puts back what action changed, so people investigate the workload
// as it was before Themis acted (design §6.7). It is a governed action of its
// own: a rollback to the revision the fix replaced, decided by policy. When
// policy forbids it or asks a person, Themis leaves its change in place and
// escalates. It acts only on an incident in restoring, so a repeated job does
// nothing.
func (w *Worker) Restore(ctx context.Context, actionID string) error {
	applied, err := w.Store.Action(ctx, w.Store.DB(), actionID)
	if err != nil {
		return err
	}
	in, err := w.Store.Incident(ctx, applied.IncidentID)
	if err != nil {
		return err
	}
	if in.State != incident.Restoring {
		return nil
	}
	// A job runs at least once: if this fix's restore already exists, it is
	// under way, and the one-active-action guard must not read it as another
	// fix.
	actions, err := w.Store.IncidentActions(ctx, w.Store.DB(), in.ID)
	if err != nil {
		return err
	}
	for _, a := range actions {
		if undone, ok := a.Restores(); ok && undone == applied.ID {
			return nil
		}
	}
	matched := w.Config.Current().Matched(in.Profiles())
	if len(matched) == 0 {
		return w.escalate(ctx, in.ID, fmt.Sprintf("Not restored: Themis no longer has %s this incident matched. The fix stays in place.", config.Named(in.Profiles())))
	}
	done, ok := w.Fixes[applied.Capability]
	if !ok {
		return w.escalate(ctx, in.ID, "Not restored: Themis has no way to undo this fix. The fix stays in place.")
	}
	name, params, err := done.Undo(ctx, w.Reader, in.Target, applied.PreState)
	if refusal, ok := errors.AsType[*capability.Refusal](err); ok {
		return w.escalate(ctx, in.ID, refusal.Reason)
	}
	if err != nil {
		return err
	}
	undo, ok := w.Fixes[name]
	if !ok {
		return w.escalate(ctx, in.ID, "Not restored: Themis has no way to undo this fix. The fix stays in place.")
	}
	checks, err := w.Store.Predicates(ctx, in.ID)
	if err != nil {
		return err
	}
	d, err := w.decider.Decide(ctx, w.Reader, Fix{
		Incident: in.ID, Target: in.Target, Profiles: matched, Checks: checks,
		Capability: undo, Params: params, Restores: applied.ID, Undoes: applied.Capability,
	})
	if refusal, ok := errors.AsType[*capability.Refusal](err); ok {
		if refusal.NoOp {
			return w.escalate(ctx, in.ID, "Nothing to restore: the workload is already as it was before Themis acted.")
		}
		return w.escalate(ctx, in.ID, "Not restored. "+refusal.Reason)
	}
	if err != nil {
		return err
	}

	// Anything else is someone's later change, not Themis's to undo. From here
	// the digest binds this state, so the executor refuses any later one.
	live, err := done.Mark(ctx, w.Reader, in.Target)
	if err != nil {
		return err
	}
	if live != applied.Mark {
		return w.escalate(ctx, in.ID, "Not restored: the workload changed after Themis's fix. It stays as it is.")
	}

	switch d.Decision.Outcome {
	case policy.Deny:
		return w.escalateDecided(ctx, in.ID, d.Decision, "Not restored. "+d.Decision.Reason+" The fix stays in place.")
	case policy.RequireApproval:
		return w.escalateDecided(ctx, in.ID, d.Decision, "Not restored. "+d.Decision.Reason+" The fix stays in place.")
	case policy.Allow:
	}

	// Policy alone authorizes it: it needs no approval.
	a := d.Action
	a.ProposalID, a.State = applied.ProposalID, store.ActionAuthorized
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		a.ID, err = w.Store.InsertAction(ctx, tx, a)
		if errors.Is(err, store.ErrActiveAction) {
			return w.escalateTx(ctx, tx, in.ID, "Not restored: another fix is already in progress on this workload. The fix stays in place.")
		}
		if err != nil {
			return err
		}
		store.AfterCommit(tx, policyDecisions.WithLabelValues(decisionLabel(d.Decision)).Inc)
		return w.Store.EnqueueTx(ctx, tx, ExecuteArgs{ActionID: a.ID, IncidentID: in.ID})
	})
}

func (w *Worker) escalateAction(ctx context.Context, actionID, reason string) error {
	applied, err := w.Store.Action(ctx, w.Store.DB(), actionID)
	if err != nil {
		return err
	}
	return w.escalate(ctx, applied.IncidentID, reason)
}

func (w *Worker) escalateDecided(ctx context.Context, incidentID string, d policy.Decision, reason string) error {
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		store.AfterCommit(tx, policyDecisions.WithLabelValues(decisionLabel(d)).Inc)
		return w.escalateTx(ctx, tx, incidentID, reason)
	})
}

func (w *Worker) escalate(ctx context.Context, incidentID, reason string) error {
	return w.Store.InTx(ctx, func(tx pgx.Tx) error { return w.escalateTx(ctx, tx, incidentID, reason) })
}

// escalateTx refers the incident to a person, saying why.
func (w *Worker) escalateTx(ctx context.Context, tx pgx.Tx, incidentID, reason string) error {
	err := w.Store.Transition(ctx, tx, store.TransitionArgs{ID: incidentID, From: incident.Restoring, To: incident.Escalated, Reason: reason + " Referred to a person.", Actor: actor})
	if errors.Is(err, store.ErrStateConflict) {
		return nil
	}
	return err
}
