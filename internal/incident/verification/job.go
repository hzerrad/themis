package verification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/governance"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/workload"
)

// DefaultPoll is how often a verification looks at the target.
const DefaultPoll = 15 * time.Second

// outcomes maps a final outcome to the incident's end. Restoring the
// pre-incident state after a failure comes in a later milestone.
var outcomes = map[Outcome]incident.State{
	Resolved:     incident.Resolved,
	Contained:    incident.ContainedEscalated,
	Unchanged:    incident.Escalated,
	Worsened:     incident.Escalated,
	Inconclusive: incident.Escalated,
}

// Worker works verify jobs, snoozing until the outcome is final.
type Worker struct {
	river.WorkerDefaults[VerifyArgs]

	Config config.Provider
	Store  *store.Store
	Reader client.Reader
	Now    func() time.Time
	Poll   time.Duration
}

func (w *Worker) Work(ctx context.Context, job *river.Job[VerifyArgs]) error {
	final, err := w.Verify(ctx, job.Args)
	// Retrying can't make an unsupported target supported.
	if err != nil && (job.Attempt >= job.MaxAttempts || errors.Is(err, workload.ErrUnsupported)) {
		// A write happened: never leave the incident in verifying.
		return w.escalate(ctx, job.Args.ActionID, "Couldn't check whether the fix worked: "+err.Error())
	}
	if err != nil || final {
		return err
	}
	poll := w.Poll
	if poll == 0 {
		poll = DefaultPoll
	}
	return river.JobSnooze(poll)
}

// Verify observes the target once and reports whether the outcome is final.
func (w *Worker) Verify(ctx context.Context, args VerifyArgs) (bool, error) {
	a, err := w.Store.Action(ctx, w.Store.DB(), args.ActionID)
	if err != nil {
		return false, err
	}
	in, err := w.Store.Incident(ctx, a.IncidentID)
	if err != nil {
		return false, err
	}
	if _, ok := a.Params["restores"].(string); ok {
		return w.verifyRestore(ctx, a, in, args)
	}
	if in.State != incident.Verifying {
		return true, nil
	}
	applied, _ := a.Params["templateHash"].(string)
	profile, _ := w.Config.Current().Profile(in.Profile)
	v := profile.EffectiveVerification()
	criteria := Criteria{Window: v.Window.Duration, Stability: v.Stability.Duration}

	now := w.now()
	obs, restarts, err := Observe(ctx, w.Reader, in.Target, args.WindowStart, applied)
	if err != nil {
		return false, err
	}

	final := false
	err = w.Store.InTx(ctx, func(tx pgx.Tx) error {
		rec, err := w.Store.StartVerification(ctx, tx, store.Verification{
			ActionID: a.ID, WindowStart: args.WindowStart, ReadyBefore: obs.ReadyReplicas, RestartsBase: restarts,
		})
		if err != nil {
			return err
		}
		obs.ReadyBefore = rec.ReadyBefore
		obs.RestartDelta = max(restarts-rec.RestartsBase, 0)
		if obs.Healthy() {
			if rec.HealthySince == nil {
				rec.HealthySince = &now
			}
			obs.StableFor = now.Sub(*rec.HealthySince)
		} else {
			rec.HealthySince = nil
		}

		outcome, done := Evaluate(obs, criteria, now.Sub(rec.WindowStart))
		rec.Observation, err = json.Marshal(obs)
		if err != nil {
			return err
		}
		if done {
			rec.Outcome = string(outcome)
		}
		if err := w.Store.UpdateVerification(ctx, tx, rec); err != nil || !done {
			return err
		}
		final = true
		store.AfterCommit(tx, verificationOutcomes.WithLabelValues(string(outcome)).Inc)
		reason := Explain(outcome, obs)
		if outcome == Unchanged || outcome == Worsened {
			// Put back what the fix changed, so people find the workload as
			// they left it (design §6.7).
			if err := w.Store.Transition(ctx, tx, store.TransitionArgs{
				ID: in.ID, From: incident.Verifying, To: incident.Restoring, Reason: reason + " Themis is putting the workload back as it was.", Actor: "verify",
			}); err != nil {
				return err
			}
			return w.Store.EnqueueTx(ctx, tx, governance.RestoreArgs{ActionID: a.ID, IncidentID: in.ID})
		}
		return w.Store.Transition(ctx, tx, store.TransitionArgs{
			ID: in.ID, From: incident.Verifying, To: outcomes[outcome], Reason: reason, Actor: "verify",
		})
	})
	return final, err
}

// Observe reads the target and observes it. A target deleted or replaced is
// a foreign change; generation is not, since annotation updates bump it.
func Observe(ctx context.Context, c client.Reader, t incident.TargetRef, since time.Time, applied string) (Observation, int32, error) {
	w, err := workload.Read(ctx, c, t)
	if errors.Is(err, workload.ErrGone) || errors.Is(err, workload.ErrReplaced) {
		return Observation{ForeignSpecChange: true}, 0, nil
	}
	if err != nil {
		return Observation{}, 0, err
	}
	return ObserveWorkload(ctx, w, since, applied)
}

// ObserveWorkload reports the workload's state, and the restarts of its
// current revision's containers. A template other than the applied one is a
// foreign change.
func ObserveWorkload(ctx context.Context, w *workload.Workload, since time.Time, applied string) (Observation, int32, error) {
	r := w.Replicas
	o := Observation{
		DesiredReplicas:   r.Desired,
		ReadyReplicas:     r.Ready,
		RolloutComplete:   r.Observed && r.Updated == r.Desired && r.Available == r.Desired && r.Total == r.Desired,
		Updated:           r.Observed && r.Updated == r.Desired,
		ForeignSpecChange: w.TemplateHash != applied,
	}
	pods, err := w.Pods(ctx)
	if err != nil {
		return Observation{}, 0, err
	}
	var restarts int32
	for _, p := range pods {
		if p.Revision != w.Revision {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			restarts += cs.RestartCount
			if term := cs.LastTerminationState.Terminated; term != nil && term.Reason == "OOMKilled" && !term.FinishedAt.Time.Before(since) {
				o.NewOOMKills++
			}
		}
	}
	return o, restarts, nil
}

func (w *Worker) escalate(ctx context.Context, actionID, reason string) error {
	a, err := w.Store.Action(ctx, w.Store.DB(), actionID)
	if err != nil {
		return err
	}
	from := incident.Verifying
	if _, ok := a.Params["restores"].(string); ok {
		from = incident.Restoring
	}
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		err := w.Store.Transition(ctx, tx, store.TransitionArgs{
			ID: a.IncidentID, From: from, To: incident.Escalated, Reason: reason, Actor: "verify",
		})
		if errors.Is(err, store.ErrStateConflict) {
			return nil
		}
		if err == nil {
			store.AfterCommit(tx, verificationOutcomes.WithLabelValues(string(Inconclusive)).Inc)
		}
		return err
	})
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// verifyRestore checks that the workload is back as it was before Themis
// acted, then hands the incident to a person: what was failing then is
// failing again, and the fix didn't help.
func (w *Worker) verifyRestore(ctx context.Context, a store.Action, in store.Incident, args VerifyArgs) (bool, error) {
	if in.State != incident.Restoring {
		return true, nil
	}
	applied, _ := a.Params["templateHash"].(string)
	obs, _, err := Observe(ctx, w.Reader, in.Target, args.WindowStart, applied)
	if err != nil {
		return false, err
	}
	profile, _ := w.Config.Current().Profile(in.Profile)
	window := profile.EffectiveVerification().Window.Duration
	pods := fmt.Sprintf("%d of %d pods are ready", obs.ReadyReplicas, obs.DesiredReplicas)
	var reason string
	switch {
	case obs.ForeignSpecChange:
		reason = "Themis put the workload back, but it was then changed outside Themis. A person needs to take over."
	case obs.Updated:
		reason = "Themis put the workload back as it was before it acted: " + pods + ". A person needs to take over."
	case w.now().Sub(args.WindowStart) >= window:
		reason = "Themis put the workload back, but Kubernetes didn't finish rolling it out in time: " + pods + ". A person needs to take over."
	default:
		return false, nil
	}
	err = w.Store.InTx(ctx, func(tx pgx.Tx) error {
		err := w.Store.Transition(ctx, tx, store.TransitionArgs{ID: in.ID, From: incident.Restoring, To: incident.Escalated, Reason: reason, Actor: "verify"})
		if errors.Is(err, store.ErrStateConflict) {
			return nil
		}
		return err
	})
	return err == nil, err
}
