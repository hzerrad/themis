package investigation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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

	// EventRetention is how long the API server keeps events: older ones
	// are gone. DefaultEventRetention when zero.
	EventRetention time.Duration
}

// DefaultEventRetention is the API server's default --event-ttl.
const DefaultEventRetention = time.Hour

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
	var (
		items []evidence.Item
		gaps  []store.EvidenceGap
	)
	for _, name := range profile.Evidence.Initial {
		item, err := w.Registry.Collect(ctx, w.Reader, name, in.Target, nil, now)
		if errors.Is(err, evidence.ErrTargetGone) {
			return w.reject(ctx, in, err.Error())
		}
		if err != nil {
			// Missing evidence makes predicates unknown; it does not stop the
			// investigation, and the incident says why it is missing.
			w.logger().WarnContext(ctx, "cannot collect initial evidence", "template", name, telemetry.FieldError, err)
			gaps = append(gaps, store.EvidenceGap{
				IncidentID: in.ID, Template: name, Cause: evidence.CauseOf(err), Detail: err.Error(), At: now,
			})
			continue
		}
		items = append(items, item)
	}
	expired, err := w.expiredEvents(ctx, in.ID, profile.Evidence.Initial, now)
	if err != nil {
		return err
	}
	gaps = append(gaps, expired...)
	results := predicate.Attribute(predicate.Evaluate(evidence.Bundle{Target: in.Target, Items: items}, now), checkGaps(gaps))
	// The template investigated, so a change outside Themis can be noticed.
	snap, err := rollback.Snapshot(ctx, w.Reader, in.Target)
	if err != nil {
		w.logger().WarnContext(ctx, "cannot snapshot the target's template", telemetry.FieldError, err)
	}

	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		err := w.Store.Transition(ctx, tx, store.TransitionArgs{
			ID: in.ID, From: incident.Received, To: incident.Investigating,
			Reason: evidenceCollected(len(items)), Actor: actor,
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
		for _, g := range gaps {
			if err := w.Store.InsertEvidenceGap(ctx, tx, g); err != nil {
				return err
			}
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

func evidenceCollected(n int) string {
	if n == 1 {
		return "Investigation started with 1 piece of evidence."
	}
	return fmt.Sprintf("Investigation started with %d pieces of evidence.", n)
}

// expiredEvents records that the target's first events are gone when its
// alerts started longer ago than Kubernetes keeps events.
func (w *PrepareWorker) expiredEvents(ctx context.Context, incidentID string, initial []string, now time.Time) ([]store.EvidenceGap, error) {
	if !slices.Contains(initial, evidence.TargetEvents) {
		return nil, nil
	}
	started, ok, err := w.Store.AlertsStarted(ctx, incidentID)
	if err != nil || !ok {
		return nil, err
	}
	retention := w.EventRetention
	if retention == 0 {
		retention = DefaultEventRetention
	}
	if now.Sub(started) <= retention {
		return nil, nil
	}
	return []store.EvidenceGap{{
		IncidentID: incidentID, Template: evidence.TargetEvents, Cause: evidence.GapExpired, At: now,
		Detail: fmt.Sprintf("the alert started %s before, and Kubernetes keeps events for about %s", now.Sub(started).Round(time.Minute), retention),
	}}, nil
}

// checkGaps are the gaps the checks read.
func checkGaps(gaps []store.EvidenceGap) []predicate.Gap {
	out := make([]predicate.Gap, 0, len(gaps))
	for _, g := range gaps {
		out = append(out, predicate.Gap{Template: g.Template, Cause: g.Cause})
	}
	return out
}
