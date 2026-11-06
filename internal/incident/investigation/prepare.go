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

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/platform/telemetry"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/workload"
)

const actor = "investigation"

// PrepareWorker works investigate jobs: it collects an incident's initial
// evidence and predicates, then queues the investigation for an investigator
// to lease.
type PrepareWorker struct {
	river.WorkerDefaults[InvestigateArgs]

	Config config.Provider
	Store  *store.Store
	Reader client.Reader
	// Catalog diagnoses the target: its evidence templates and checks.
	Catalog catalog.Catalog
	Logger  *slog.Logger
	Now     func() time.Time

	// EventRetention is how long the API server keeps events: older ones
	// are gone. catalog.DefaultEventRetention when zero.
	EventRetention time.Duration
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
	matched := w.Config.Current().Matched(in.Profiles())
	if len(matched) == 0 {
		return w.reject(ctx, in, fmt.Sprintf("Themis no longer has %s this incident matched.", config.Named(in.Profiles())))
	}

	target, err := workload.Read(ctx, w.Reader, in.Target)
	if workload.Lost(err) {
		lost := w.Store.InTx(ctx, func(tx pgx.Tx) error { return TargetLost(ctx, w.Store, tx, in, err, actor) })
		if errors.Is(lost, store.ErrStateConflict) {
			return nil
		}
		return lost
	}
	if err != nil {
		return err
	}
	started, _, err := w.Store.AlertsStarted(ctx, in.ID)
	if err != nil {
		return err
	}
	now := w.now()
	d := w.Catalog.Diagnose(ctx, w.Reader, target, matched, catalog.Moment{Now: now, AlertStarted: started, EventRetention: w.EventRetention})
	gaps := make([]store.EvidenceGap, 0, len(d.Gaps))
	for _, g := range d.Gaps {
		// Missing evidence makes checks unknown; it does not stop the
		// investigation, and the incident says why it is missing.
		w.logger().WarnContext(ctx, "initial evidence is missing", "template", g.Template, "cause", g.Cause, telemetry.FieldError, g.Detail)
		gaps = append(gaps, store.EvidenceGap{IncidentID: in.ID, Template: g.Template, Cause: g.Cause, Detail: g.Detail, At: now})
	}
	// Without an investigator, the incident waits for a person or a tool
	// outside Themis to suggest a fix; the checks still run for the policy.
	noInvestigator := w.Config.Current().Investigators.Disabled
	to, reason := incident.Investigating, evidenceCollected(len(d.Items))
	if noInvestigator {
		to, reason = incident.AwaitingSuggestion, awaitingSuggestion(len(d.Items))
	}
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		err := w.Store.Transition(ctx, tx, store.TransitionArgs{
			ID: in.ID, From: incident.Received, To: to, Reason: reason, Actor: actor,
		})
		if errors.Is(err, store.ErrStateConflict) {
			return nil
		}
		if err != nil {
			return err
		}
		if noInvestigator {
			err = w.Store.RecordInitialEvidence(ctx, tx, in.ID, d.Items, d.Results)
		} else {
			_, err = w.Store.CreateInvestigation(ctx, tx, in.ID, d.Items, d.Results)
		}
		if err != nil {
			return err
		}
		for _, g := range gaps {
			if _, err := recordGap(ctx, w.Store, tx, w.Catalog.Evidence, g); err != nil {
				return err
			}
		}
		// The template investigated, so a change outside Themis can be noticed.
		return w.Store.SetObservedSpec(ctx, tx, in.ID, target.TemplateHash)
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

// awaitingSuggestion says what happened when no investigator is installed.
func awaitingSuggestion(n int) string {
	pieces := fmt.Sprintf("%d pieces", n)
	if n == 1 {
		pieces = "1 piece"
	}
	return "Collected " + pieces + " of evidence and ran the checks. No investigator is installed. Waiting for a suggested fix."
}
