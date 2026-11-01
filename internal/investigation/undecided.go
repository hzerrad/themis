package investigation

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"
)

// CloseUndecidedArgs sweep incidents nobody suggested a fix for in time.
type CloseUndecidedArgs struct{}

// Kind is persisted with each job and must never change.
func (CloseUndecidedArgs) Kind() string { return "close_undecided_incidents" }

// CloseUndecidedWorker closes incidents Themis couldn't decide once their
// time for a suggestion has passed.
type CloseUndecidedWorker struct {
	river.WorkerDefaults[CloseUndecidedArgs]

	Config config.Provider
	Store  *store.Store
	Now    func() time.Time
}

const closeUndecidedBatch = 100

func (w *CloseUndecidedWorker) Work(ctx context.Context, _ *river.Job[CloseUndecidedArgs]) error {
	return w.Close(ctx)
}

func (w *CloseUndecidedWorker) Close(ctx context.Context) error {
	now := time.Now()
	if w.Now != nil {
		now = w.Now()
	}
	window := w.Config.Current().ExternalProposals.EffectiveWindow()
	ids, err := w.Store.UndecidedBefore(ctx, w.Store.DB(), now.Add(-window), closeUndecidedBatch)
	if err != nil {
		return err
	}
	for _, id := range ids {
		err := w.Store.InTx(ctx, func(tx pgx.Tx) error {
			return w.Store.Transition(ctx, tx, store.TransitionArgs{
				ID: id, From: incident.Inconclusive, To: incident.ClosedNoAction,
				Reason: "Nobody suggested a fix in time, so Themis closed the incident.", Actor: actor,
			})
		})
		if err != nil && !errors.Is(err, store.ErrStateConflict) {
			return err
		}
	}
	return nil
}
