package investigation

import (
	"context"
	"errors"
	"strings"
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
	cfg := w.Config.Current()
	window, waiting := cfg.SuggestionWindow(), cfg.AwaitingSuggestionWindow()
	undecided, err := w.Store.UndecidedBefore(ctx, w.Store.DB(), now.Add(-window), now.Add(-waiting), closeUndecidedBatch)
	if err != nil {
		return err
	}
	reasons := map[incident.State]string{
		incident.Inconclusive:       "No fix was suggested, and no time to suggest one is configured.",
		incident.AwaitingSuggestion: "No fix was suggested within " + shortDuration(waiting) + ".",
	}
	if window > 0 {
		reasons[incident.Inconclusive] = "No fix was suggested within " + shortDuration(window) + "."
	}
	for _, u := range undecided {
		err := w.Store.InTx(ctx, func(tx pgx.Tx) error {
			return w.Store.Transition(ctx, tx, store.TransitionArgs{
				ID: u.ID, From: u.State, To: incident.ClosedNoAction,
				Reason: reasons[u.State], Actor: actor,
			})
		})
		if err != nil && !errors.Is(err, store.ErrStateConflict) {
			return err
		}
	}
	return nil
}

// shortDuration writes d as people read it: 30m, 1h, 1h30m.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
