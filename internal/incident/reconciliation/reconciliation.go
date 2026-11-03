// Package reconciliation notices targets changed outside Themis before Themis
// acts on them: fixed by hand or through GitOps, deleted, or changed and
// still failing.
package reconciliation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/investigation"
	"github.com/hzerrad/themis/internal/incident/verification"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/telemetry"
	"github.com/hzerrad/themis/internal/workload"
)

const actor = "reconcile"

// Settle is how long a change must stand before it is judged, so a rollout
// still in progress is not mistaken for its outcome.
const Settle = time.Minute

// batch bounds the incidents one run looks at.
const batch = 100

// Args run one reconciliation.
type Args struct{}

// Kind is persisted with each job and must never change.
func (Args) Kind() string { return "reconcile" }

type Worker struct {
	river.WorkerDefaults[Args]

	Store  *store.Store
	Reader client.Reader
	Logger *slog.Logger
	Now    func() time.Time
}

func (w *Worker) Work(ctx context.Context, _ *river.Job[Args]) error {
	return w.Reconcile(ctx)
}

// Reconcile looks at every open incident not yet executing. One target that
// cannot be read does not stop the others.
func (w *Worker) Reconcile(ctx context.Context) error {
	incidents, err := w.Store.ReconcilableIncidents(ctx, batch)
	if err != nil {
		return err
	}
	for _, in := range incidents {
		if err := w.one(ctx, in); err != nil && !errors.Is(err, errMovedOn) {
			w.logger().WarnContext(ctx, "cannot reconcile incident", telemetry.FieldIncidentID, in.ID, telemetry.FieldError, err)
		}
	}
	return nil
}

func (w *Worker) one(ctx context.Context, in store.Incident) error {
	target, err := workload.Read(ctx, w.Reader, in.Target)
	if workload.Lost(err) {
		return w.Store.InTx(ctx, func(tx pgx.Tx) error {
			return movedOn(investigation.TargetLost(ctx, w.Store, tx, in, err, actor))
		})
	}
	if err != nil {
		return err
	}
	if target.TemplateHash == in.ObservedSpecHash || w.now().Sub(target.WrittenAt) < Settle {
		return nil
	}

	by := target.Writer
	if by == "" {
		by = "an unknown writer"
	}
	obs, _, err := verification.ObserveWorkload(ctx, target, target.WrittenAt, target.TemplateHash)
	if err != nil {
		return err
	}
	if obs.Healthy() {
		to := incident.SelfResolved
		if !incident.CanTransition(in.State, to) {
			to = incident.Canceled
		}
		return w.close(ctx, in, to, "Fixed outside Themis, by "+by+".")
	}
	return w.supersede(ctx, in, by)
}

func (w *Worker) close(ctx context.Context, in store.Incident, to incident.State, reason string) error {
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		if err := w.transition(ctx, tx, in, to, reason); err != nil {
			return err
		}
		return w.Store.CancelPendingActions(ctx, tx, in.ID)
	})
}

// supersede closes the incident and opens another for the target as it is
// now, to be investigated from scratch.
func (w *Worker) supersede(ctx context.Context, in store.Incident, by string) error {
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		reason := fmt.Sprintf("Replaced by a new incident: the workload was changed outside Themis, by %s, and still fails.", by)
		return movedOn(investigation.Supersede(ctx, w.Store, tx, in, in.Target, reason, actor))
	})
}

// movedOn turns a state conflict into errMovedOn.
func movedOn(err error) error {
	if errors.Is(err, store.ErrStateConflict) {
		return errMovedOn
	}
	return err
}

// transition does nothing if the incident moved on meanwhile.
func (w *Worker) transition(ctx context.Context, tx pgx.Tx, in store.Incident, to incident.State, reason string) error {
	return movedOn(w.Store.Transition(ctx, tx, store.TransitionArgs{ID: in.ID, From: in.State, To: to, Reason: reason, Actor: actor}))
}

// errMovedOn rolls a reconciliation back when the incident changed state
// under it; the next run looks again.
var errMovedOn = errors.New("the incident moved on")

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Worker) logger() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}
