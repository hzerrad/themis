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
)

// actor is recorded on the lifecycle transitions intake makes.
const actor = "intake"

// ProcessAlertWorker turns a received alert into an incident occurrence, the
// end of an incident, or a rejection.
//
// A firing alert is checked for enrollment, resolved to its target workload
// and matched to a profile; it then creates or joins the incident for that
// profile and target. A resolved alert is found by its identity at the source,
// without reading the cluster, because its target may be gone; when none of
// the incident's alerts still fires and no fix is in flight, the incident
// closes as self_resolved.
//
// Each outcome is stored in one transaction together with the job's
// completion, so a crash cannot apply it twice. Errors that are answers become
// rejections; others are returned so that River retries, and on the last
// attempt they become a processing_failed rejection.
type ProcessAlertWorker struct {
	river.WorkerDefaults[ProcessAlertArgs]

	Config config.Config
	Store  *store.Store

	// Reader and Mapper read the cluster to resolve targets.
	Reader client.Reader
	Mapper meta.RESTMapper
}

// Work processes one alert.
func (w *ProcessAlertWorker) Work(ctx context.Context, job *river.Job[ProcessAlertArgs]) error {
	a := job.Args.Alert
	if a.Status == AlertResolved {
		return w.resolve(ctx, job)
	}

	in, rejection, err := w.judge(ctx, a)
	switch {
	case err != nil && job.Attempt < job.MaxAttempts:
		return err
	case err != nil:
		rejection = &rejected{store.RejectionProcessingFailed,
			fmt.Sprintf("gave up after %d attempts: %v", job.Attempt, err)}
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

// judge decides what a firing alert is about. It returns the incident to
// create or join, or why the alert is rejected, or an error worth retrying.
func (w *ProcessAlertWorker) judge(ctx context.Context, a Alert) (store.IncidentArgs, *rejected, error) {
	if ns := a.Labels[LabelNamespace]; ns != "" && !w.Config.IsEnrolled(ns) {
		return store.IncidentArgs{}, &rejected{store.RejectionNotEnrolled,
			fmt.Sprintf("namespace %q is not enrolled", ns)}, nil
	}

	target, err := ResolveTarget(ctx, w.Reader, w.Mapper, a.Labels)
	if err != nil {
		if reason, ok := rejectionReason(err); ok {
			return store.IncidentArgs{}, &rejected{reason, err.Error()}, nil
		}
		return store.IncidentArgs{}, nil, fmt.Errorf("resolve target: %w", err)
	}

	profile, ok := w.Config.MatchProfile(a.Name, a.Labels, target)
	if !ok {
		return store.IncidentArgs{}, &rejected{store.RejectionNoProfile,
			fmt.Sprintf("no profile matches alert %s on %s %s/%s", a.Name, config.TargetKind(target), target.Namespace, target.Name)}, nil
	}

	return store.IncidentArgs{
		Fingerprint: Fingerprint(profile.Name, target),
		Profile:     profile.Name,
		Target:      target,
		Payload:     a.Payload,
	}, nil, nil
}

// rejectionReason classifies a target resolution error. Errors that are an
// answer, which retrying cannot change, are rejections. Anything else, such as
// the API server being unreachable, is not, so the caller retries.
func rejectionReason(err error) (store.AlertRejectionReason, bool) {
	switch {
	case errors.Is(err, ErrMissingLabels):
		return store.RejectionMissingLabels, true
	case errors.Is(err, ErrInvalidOverride):
		return store.RejectionInvalidOverride, true
	case errors.Is(err, ErrUnknownKind):
		return store.RejectionUnknownKind, true
	case errors.Is(err, ErrNotFound):
		return store.RejectionTargetNotFound, true
	case errors.Is(err, ErrOwnerChainTooLong):
		return store.RejectionOwnerChainTooLong, true
	case apierrors.IsForbidden(err):
		// Themis lacks RBAC on the namespace: a misconfiguration that
		// retrying never fixes.
		return store.RejectionForbidden, true
	default:
		return "", false
	}
}

// fire creates or joins the incident, records the alert as firing in it, and
// queues the investigation of a new incident.
func (w *ProcessAlertWorker) fire(ctx context.Context, job *river.Job[ProcessAlertArgs], in store.IncidentArgs) error {
	a := job.Args.Alert
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		id, created, err := w.Store.CreateOrJoinIncident(ctx, tx, in)
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
}

// resolve marks the alert resolved in its open incidents, and closes each
// incident that has no firing alert left and no fix in flight. An alert that
// belongs to no open incident has nothing to resolve.
func (w *ProcessAlertWorker) resolve(ctx context.Context, job *river.Job[ProcessAlertArgs]) error {
	a := job.Args.Alert
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		incidents, err := w.Store.ResolveAlert(ctx, tx, store.ResolvedAlertArgs{
			Source: a.Source, SourceID: a.SourceID, StartsAt: a.StartsAt, EndsAt: a.EndsAt,
		})
		if err != nil {
			return err
		}
		for _, inc := range incidents {
			// A fix in flight cannot move to self_resolved: verification
			// decides its outcome, not the alert.
			if inc.Firing > 0 || !incident.CanTransition(inc.State, incident.SelfResolved) {
				continue
			}
			err := w.Store.Transition(ctx, tx, store.TransitionArgs{
				ID: inc.IncidentID, From: inc.State, To: incident.SelfResolved,
				Reason: "all alerts resolved", Actor: actor,
			})
			if err != nil {
				return err
			}
		}
		return complete(ctx, tx, job)
	})
}

// reject records why the alert did not become an incident.
func (w *ProcessAlertWorker) reject(ctx context.Context, job *river.Job[ProcessAlertArgs], r rejected) error {
	a := job.Args.Alert
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		_, err := w.Store.InsertRejectedAlert(ctx, tx, store.RejectedAlertArgs{
			Source:     a.Source,
			ReceivedAt: a.ReceivedAt,
			Reason:     r.reason,
			Detail:     r.detail,
			AlertName:  a.Name,
			Namespace:  a.Labels[LabelNamespace],
			Payload:    a.Payload,
		})
		if err != nil {
			return err
		}
		return complete(ctx, tx, job)
	})
}

// complete marks the job done in tx, so its outcome and its completion commit
// together.
func complete(ctx context.Context, tx pgx.Tx, job *river.Job[ProcessAlertArgs]) error {
	if _, err := river.JobCompleteTx[*riverpgxv5.Driver](ctx, tx, job); err != nil {
		return fmt.Errorf("complete job %d: %w", job.ID, err)
	}
	return nil
}
