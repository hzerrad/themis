package investigation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/predicate"
	"github.com/hzerrad/themis/internal/rollback"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/telemetry"
)

const actor = "investigation"

// PrepareWorker works investigate jobs: it collects an incident's initial
// evidence and predicates, then queues the investigation for an investigator
// to lease.
type PrepareWorker struct {
	river.WorkerDefaults[InvestigateArgs]

	Config   config.Config
	Store    *store.Store
	Reader   client.Reader
	Registry evidence.Registry
	Logger   *slog.Logger
	Now      func() time.Time
}

func (w *PrepareWorker) Work(ctx context.Context, job *river.Job[InvestigateArgs]) error {
	return w.Prepare(ctx, job.Args.IncidentID)
}

// Prepare acts only on a received incident, so a repeated job does nothing.
func (w *PrepareWorker) Prepare(ctx context.Context, incidentID string) error {
	in, err := w.Store.Incident(ctx, incidentID)
	if err != nil {
		return err
	}
	if in.State != incident.Received {
		return nil
	}
	profile, ok := w.Config.Profile(in.Profile)
	if !ok {
		return w.reject(ctx, in, fmt.Sprintf("profile %q is no longer configured", in.Profile))
	}

	now := w.now()
	var items []evidence.Item
	for _, name := range profile.Evidence.Initial {
		item, err := w.Registry.Collect(ctx, w.Reader, name, in.Target, nil, now)
		if errors.Is(err, evidence.ErrTargetGone) {
			return w.reject(ctx, in, err.Error())
		}
		if err != nil {
			// Missing evidence makes predicates unknown; it does not stop the
			// investigation.
			w.logger().WarnContext(ctx, "cannot collect initial evidence", telemetry.FieldIncidentID, in.ID, "template", name, telemetry.FieldError, err)
			continue
		}
		items = append(items, item)
	}
	results := predicate.Evaluate(evidence.Bundle{Target: in.Target, Items: items}, now)
	// The template investigated, so a change outside Themis can be noticed.
	snap, err := rollback.Snapshot(ctx, w.Reader, in.Target)
	if err != nil {
		w.logger().WarnContext(ctx, "cannot snapshot the target's template", telemetry.FieldIncidentID, in.ID, telemetry.FieldError, err)
	}

	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		err := w.Store.Transition(ctx, tx, store.TransitionArgs{
			ID: in.ID, From: incident.Received, To: incident.Investigating,
			Reason: fmt.Sprintf("collected %d initial evidence items", len(items)), Actor: actor,
		})
		if errors.Is(err, store.ErrStateConflict) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := w.Store.CreateInvestigation(ctx, tx, in.ID, items, results); err != nil {
			return err
		}
		return w.Store.SetObservedSpec(ctx, tx, in.ID, snap.SpecHash)
	})
}

func (w *PrepareWorker) reject(ctx context.Context, in store.Incident, reason string) error {
	err := w.Store.InTx(ctx, func(tx pgx.Tx) error {
		return w.Store.Transition(ctx, tx, store.TransitionArgs{
			ID: in.ID, From: incident.Received, To: incident.Rejected, Reason: reason, Actor: actor,
		})
	})
	if errors.Is(err, store.ErrStateConflict) {
		return nil
	}
	return err
}

func (w *PrepareWorker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *PrepareWorker) logger() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}
