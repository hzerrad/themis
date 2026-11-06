package governance

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"
)

// Approve approves the action awaiting approval with digest, the one the
// approver was shown, and hands it to the executor.
func Approve(ctx context.Context, s *store.Store, actionID, digest, approver string, now time.Time) (store.Action, error) {
	var act store.Action
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		act, err = s.Approve(ctx, tx, actionID, digest, approver, now)
		if err != nil {
			return err
		}
		err = s.Transition(ctx, tx, store.TransitionArgs{
			ID: act.IncidentID, From: incident.AwaitingApproval, To: incident.Authorized,
			Reason: "Approved by " + approver + ".", Actor: approver,
		})
		if err != nil {
			return err
		}
		store.AfterCommit(tx, approvalsTotal.WithLabelValues(resultApproved).Inc)
		return s.EnqueueTx(ctx, tx, ExecuteArgs{ActionID: act.ID, IncidentID: act.IncidentID})
	})
	if err != nil {
		countRefusal(err)
	}
	return act, err
}

// Deny denies the action awaiting approval, and closes its incident with no
// action. reason, if any, is the approver's.
func Deny(ctx context.Context, s *store.Store, actionID, approver, reason string) error {
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		act, err := s.Action(ctx, tx, actionID)
		if err != nil {
			return err
		}
		if err := s.SetActionState(ctx, tx, act.ID, store.ActionAwaitingApproval, store.ActionDenied); err != nil {
			return store.ErrAlreadyDecided
		}
		why := "Denied by " + approver + "."
		if reason != "" {
			why = "Denied by " + approver + ": " + reason
		}
		if err := s.TransitionPath(ctx, tx, act.IncidentID, why, approver, incident.AwaitingApproval, incident.Recommended, incident.ClosedNoAction); err != nil {
			return err
		}
		store.AfterCommit(tx, approvalsTotal.WithLabelValues(resultDenied).Inc)
		return nil
	})
	if err != nil {
		countRefusal(err)
	}
	return err
}

// ExpireArgs sweep approvals whose window passed.
type ExpireArgs struct{}

// Kind is persisted with each job and must never change.
func (ExpireArgs) Kind() string { return "expire_approvals" }

// ExpireWorker escalates incidents whose approval nobody gave in time.
type ExpireWorker struct {
	river.WorkerDefaults[ExpireArgs]

	Store *store.Store
	Now   func() time.Time
}

func (w *ExpireWorker) Work(ctx context.Context, _ *river.Job[ExpireArgs]) error {
	return w.Expire(ctx)
}

func (w *ExpireWorker) Expire(ctx context.Context) error {
	now := time.Now()
	if w.Now != nil {
		now = w.Now()
	}
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		expired, err := w.Store.ExpireApprovals(ctx, tx, now)
		if err != nil {
			return err
		}
		for _, a := range expired {
			store.AfterCommit(tx, approvalsTotal.WithLabelValues(resultExpired).Inc)
			err := w.Store.Transition(ctx, tx, store.TransitionArgs{
				ID: a.IncidentID, From: incident.AwaitingApproval, To: incident.Escalated,
				Reason: "Approval expired: no one approved the fix in time. Referred to a person.", Actor: "approval",
			})
			// An incident that already moved on keeps its state; the action
			// still expires.
			if err != nil && !errors.Is(err, store.ErrStateConflict) {
				return err
			}
		}
		return nil
	})
}
