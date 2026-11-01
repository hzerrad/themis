package intake

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/investigation"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/target"
)

// actor is recorded on the lifecycle transitions intake makes.
const actor = "intake"

// ProcessAlertWorker turns a received alert into an incident occurrence, the
// end of an incident, or a rejection. Each outcome commits with the job's
// completion; transient errors are retried, then rejected as processing_failed.
type ProcessAlertWorker struct {
	river.WorkerDefaults[ProcessAlertArgs]

	Config config.Provider
	Store  *store.Store

	Reader client.Reader
	Mapper meta.RESTMapper
}

func (w *ProcessAlertWorker) Work(ctx context.Context, job *river.Job[ProcessAlertArgs]) error {
	err := w.work(ctx, job)
	if err != nil {
		alertsProcessed.WithLabelValues(outcomeRetried, "").Inc()
	}
	return err
}

func (w *ProcessAlertWorker) work(ctx context.Context, job *river.Job[ProcessAlertArgs]) error {
	a := job.Args.Alert
	if a.Status == AlertResolved {
		return w.resolve(ctx, job)
	}

	in, rejection, err := w.judge(ctx, a)
	switch {
	case err != nil && job.Attempt < job.MaxAttempts:
		return err
	case err != nil:
		rejection = &rejected{
			store.RejectionProcessingFailed,
			fmt.Sprintf("gave up after %d attempts: %v", job.Attempt, err),
		}
	}
	if rejection != nil {
		return w.reject(ctx, job, *rejection)
	}
	return w.fire(ctx, job, in)
}

// rejected is a reason not to act on an alert.
type rejected struct {
	reason store.AlertRejectionReason
	detail string
}

// judge returns the incident a firing alert belongs to, or why it is rejected,
// or an error worth retrying.
func (w *ProcessAlertWorker) judge(ctx context.Context, a Alert) (store.IncidentArgs, *rejected, error) {
	if ns := a.Labels[target.LabelNamespace]; ns != "" && !w.Config.Current().IsEnrolled(ns) {
		return store.IncidentArgs{}, &rejected{
			store.RejectionNotEnrolled,
			fmt.Sprintf("namespace %q is not enrolled", ns),
		}, nil
	}

	ref, err := target.Resolve(ctx, w.Reader, w.Mapper, a.Labels)
	if err != nil {
		if reason, ok := rejectionReason(err); ok {
			return store.IncidentArgs{}, &rejected{reason, err.Error()}, nil
		}
		return store.IncidentArgs{}, nil, fmt.Errorf("resolve target: %w", err)
	}
	// Enforced again on what was resolved, not only on the alert's label.
	if !w.Config.Current().IsEnrolled(ref.Namespace) {
		return store.IncidentArgs{}, &rejected{
			store.RejectionNotEnrolled,
			fmt.Sprintf("target namespace %q is not enrolled", ref.Namespace),
		}, nil
	}

	profile, ok := w.Config.Current().MatchProfile(a.Name, a.Labels, ref)
	if !ok {
		return store.IncidentArgs{}, &rejected{
			store.RejectionNoProfile,
			fmt.Sprintf("no profile matches alert %s on %s %s/%s", a.Name, config.TargetKind(ref), ref.Namespace, ref.Name),
		}, nil
	}

	return store.IncidentArgs{
		Fingerprint: Fingerprint(profile.Name, ref),
		Profile:     profile.Name,
		Target:      ref,
		Payload:     a.Payload,
	}, nil, nil
}

// rejectionReason classifies resolution errors that retrying cannot change.
func rejectionReason(err error) (store.AlertRejectionReason, bool) {
	switch {
	case errors.Is(err, target.ErrMissingLabels):
		return store.RejectionMissingLabels, true
	case errors.Is(err, target.ErrInvalidOverride):
		return store.RejectionInvalidOverride, true
	case errors.Is(err, target.ErrUnknownKind):
		return store.RejectionUnknownKind, true
	case errors.Is(err, target.ErrNotFound):
		return store.RejectionTargetNotFound, true
	case errors.Is(err, target.ErrOwnerChainTooLong):
		return store.RejectionOwnerChainTooLong, true
	case apierrors.IsForbidden(err):
		return store.RejectionForbidden, true
	default:
		return "", false
	}
}

// fire creates or joins the incident and queues the investigation of a new one.
func (w *ProcessAlertWorker) fire(ctx context.Context, job *river.Job[ProcessAlertArgs], in store.IncidentArgs) error {
	a := job.Args.Alert
	var created, stale bool
	err := w.Store.InTx(ctx, func(tx pgx.Tx) error {
		var (
			id  string
			err error
		)
		// The resolution of this episode was processed first.
		if stale, err = w.Store.Resolved(ctx, tx, a.Source, a.SourceID, a.StartsAt); err != nil || stale {
			if err != nil {
				return err
			}
			return complete(ctx, tx, job)
		}
		id, created, err = w.Store.CreateOrJoinIncident(ctx, tx, in)
		if err != nil {
			return err
		}
		err = w.Store.RecordFiringAlert(ctx, tx, store.FiringAlertArgs{
			IncidentID: id, Source: a.Source, SourceID: a.SourceID, StartsAt: a.StartsAt,
		})
		if err != nil {
			return err
		}
		if created {
			if err := w.Store.EnqueueTx(ctx, tx, investigation.InvestigateArgs{IncidentID: id}); err != nil {
				return err
			}
		}
		return complete(ctx, tx, job)
	})
	if err != nil {
		return err
	}
	switch {
	case stale:
		alertsProcessed.WithLabelValues(outcomeStale, "").Inc()
		return nil
	case created:
		alertsProcessed.WithLabelValues(outcomeCreated, "").Inc()
	default:
		alertsProcessed.WithLabelValues(outcomeJoined, "").Inc()
	}
	return nil
}

// resolve marks the alert resolved and closes incidents with no firing alert
// left and no fix in flight.
func (w *ProcessAlertWorker) resolve(ctx context.Context, job *river.Job[ProcessAlertArgs]) error {
	a := job.Args.Alert
	err := w.Store.InTx(ctx, func(tx pgx.Tx) error {
		resolved := store.ResolvedAlertArgs{Source: a.Source, SourceID: a.SourceID, StartsAt: a.StartsAt, EndsAt: a.EndsAt}
		if err := w.Store.RecordResolution(ctx, tx, resolved); err != nil {
			return err
		}
		incidents, err := w.Store.ResolveAlert(ctx, tx, resolved)
		if err != nil {
			return err
		}
		for _, inc := range incidents {
			if inc.Firing > 0 || !incident.CanTransition(inc.State, incident.SelfResolved) {
				continue
			}
			err := w.Store.Transition(ctx, tx, store.TransitionArgs{
				ID: inc.IncidentID, From: inc.State, To: incident.SelfResolved,
				Reason: "All of its alerts stopped firing.", Actor: actor,
			})
			if err != nil {
				return err
			}
			if err := w.Store.CancelPendingActions(ctx, tx, inc.IncidentID); err != nil {
				return err
			}
		}
		return complete(ctx, tx, job)
	})
	if err != nil {
		return err
	}
	alertsProcessed.WithLabelValues(outcomeResolved, "").Inc()
	return nil
}

// reject records why the alert did not become an incident.
func (w *ProcessAlertWorker) reject(ctx context.Context, job *river.Job[ProcessAlertArgs], r rejected) error {
	a := job.Args.Alert
	err := w.Store.InTx(ctx, func(tx pgx.Tx) error {
		_, err := w.Store.InsertRejectedAlert(ctx, tx, store.RejectedAlertArgs{
			Source:     a.Source,
			ReceivedAt: a.ReceivedAt,
			Reason:     r.reason,
			Detail:     r.detail,
			AlertName:  a.Name,
			Namespace:  a.Labels[target.LabelNamespace],
			Payload:    a.Payload,
		})
		if err != nil {
			return err
		}
		return complete(ctx, tx, job)
	})
	if err != nil {
		return err
	}
	alertsProcessed.WithLabelValues(outcomeRejected, string(r.reason)).Inc()
	return nil
}

// complete marks the job done in tx.
func complete(ctx context.Context, tx pgx.Tx, job *river.Job[ProcessAlertArgs]) error {
	if _, err := river.JobCompleteTx[*riverpgxv5.Driver](ctx, tx, job); err != nil {
		return fmt.Errorf("complete job %d: %w", job.ID, err)
	}
	return nil
}
