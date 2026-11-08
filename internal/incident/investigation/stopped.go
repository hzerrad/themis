package investigation

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/workload"
)

// AlertsStoppedArgs decide whether an incident every alert of which stopped
// firing recovered.
type AlertsStoppedArgs struct {
	IncidentID string `json:"incidentID"`
}

// Kind is persisted with each job and must never change.
func (AlertsStoppedArgs) Kind() string { return "alerts_stopped" }

func (a AlertsStoppedArgs) Incident() string { return a.IncidentID }

// InsertOpts: one decision waits per incident at a time.
func (AlertsStoppedArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
		rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateScheduled,
	}}}
}

// StoppedRecheckEvery is how often Themis decides again the symptoms of an
// incident whose alerts stopped while its checks still found its failure.
const StoppedRecheckEvery = time.Minute

// AlertsStoppedWorker ends an incident whose alerts all stopped firing before
// Themis acted, unless it still fails. Alerts stop for reasons other than a
// recovery: the pods they were about were replaced, or Alertmanager let their
// end pass while Prometheus couldn't reach it. So the incident's symptoms are
// decided again from evidence read now. While one is true, or one its checks
// found when it opened can't be told, the incident stays open, and they are
// decided again every StoppedRecheckEvery until it recovers, an alert fires
// again, or the incident moves on. Once they are false, it closes as
// recovered.
type AlertsStoppedWorker struct {
	river.WorkerDefaults[AlertsStoppedArgs]

	Config config.Provider
	Store  *store.Store
	// Reader reads the targets, and Checks decides their symptoms again.
	// Without Checks, nothing is decided again: the stored checks keep an
	// incident whose failure they found open.
	Reader client.Reader
	Checks Rechecker
	Words  check.List
	Logger *slog.Logger
	Now    func() time.Time
}

func (w *AlertsStoppedWorker) Work(ctx context.Context, job *river.Job[AlertsStoppedArgs]) error {
	open, err := w.Decide(ctx, job.Args.IncidentID)
	if err != nil {
		return err
	}
	if open {
		return river.JobSnooze(StoppedRecheckEvery)
	}
	return nil
}

// Decide ends the incident as recovered when none of its alerts fires and
// its symptoms, decided again now, no longer find its failure. It reports
// whether the incident stays open with no alert firing, to be decided again.
func (w *AlertsStoppedWorker) Decide(ctx context.Context, incidentID string) (bool, error) {
	in, err := w.Store.Incident(ctx, incidentID)
	if err != nil {
		return false, err
	}
	if !incident.CanTransition(in.State, incident.SelfResolved) {
		return false, nil
	}
	if firing, err := w.Store.FiringAlerts(ctx, w.Store.DB(), in.ID); err != nil || firing > 0 {
		return false, err
	}
	f, err := symptomsNow(ctx, w.Store, w.Reader, w.Checks, w.Config.Current(), in, w.now())
	if workload.Lost(err) {
		return false, w.Store.InTx(ctx, func(tx pgx.Tx) error {
			return movedOn(TargetLost(ctx, w.Store, tx, in, err, actorStopped))
		})
	}
	if err != nil {
		return false, err
	}
	if f.fails {
		// The job is about the incident, so its ID is on the line already.
		w.logger().InfoContext(ctx, "the incident's alerts stopped firing, and its checks still find its failure: it stays open")
		return true, nil
	}
	reason := "All of its alerts stopped firing."
	if f.found {
		reason += " The checks now find " + that(f.cleared, w.Words) + "."
	}
	return false, w.Store.InTx(ctx, func(tx pgx.Tx) error {
		// An alert may have fired again, or the incident moved on, since.
		state, err := w.Store.LockIncident(ctx, tx, in.ID)
		if err != nil || state != in.State {
			return err
		}
		if firing, err := w.Store.FiringAlerts(ctx, tx, in.ID); err != nil || firing > 0 {
			return err
		}
		err = w.Store.Transition(ctx, tx, store.TransitionArgs{ID: in.ID, From: in.State, To: incident.SelfResolved, Reason: reason, Actor: actorStopped})
		if err != nil {
			return err
		}
		return w.Store.CancelPendingActions(ctx, tx, in.ID)
	})
}

// actorStopped is recorded on the transitions an incident's alerts stopping
// makes, as when intake closed it.
const actorStopped = "intake"

// movedOn makes a move from a state the incident has left nothing to do.
func movedOn(err error) error {
	if errors.Is(err, store.ErrStateConflict) {
		return nil
	}
	return err
}

func (w *AlertsStoppedWorker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *AlertsStoppedWorker) logger() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}
