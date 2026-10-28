package approval

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"
)

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
			err := w.Store.Transition(ctx, tx, store.TransitionArgs{
				ID: a.IncidentID, From: incident.AwaitingApproval, To: incident.Escalated,
				Reason: "nobody approved the action in time", Actor: "approval",
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
