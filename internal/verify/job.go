package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/governance"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/rollback"
	"github.com/hzerrad/themis/internal/store"
)

// DefaultPoll is how often a verification looks at the target.
const DefaultPoll = 15 * time.Second

const revisionAnnotation = "deployment.kubernetes.io/revision"

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

	Config config.Config
	Store  *store.Store
	Reader client.Reader
	Now    func() time.Time
	Poll   time.Duration
}

func (w *Worker) Work(ctx context.Context, job *river.Job[VerifyArgs]) error {
	final, err := w.Verify(ctx, job.Args)
	if err != nil && job.Attempt >= job.MaxAttempts {
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
	profile, _ := w.Config.Profile(in.Profile)
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

// Observe reads the target's state, and the restarts of its current
// revision's containers. A template other than the applied one is a foreign
// change; generation is not, since annotation updates bump it.
func Observe(ctx context.Context, c client.Reader, t incident.TargetRef, since time.Time, applied string) (Observation, int32, error) {
	var d appsv1.Deployment
	err := c.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: t.Name}, &d)
	if apierrors.IsNotFound(err) || (err == nil && string(d.UID) != t.UID) {
		return Observation{ForeignSpecChange: true}, 0, nil
	}
	if err != nil {
		return Observation{}, 0, fmt.Errorf("get Deployment %s: %w", t.Name, err)
	}
	live, err := rollback.TemplateHash(d.Spec.Template)
	if err != nil {
		return Observation{}, 0, err
	}
	desired := int32(1)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	s := d.Status
	o := Observation{
		DesiredReplicas: desired,
		ReadyReplicas:   s.ReadyReplicas,
		RolloutComplete: s.ObservedGeneration >= d.Generation && s.UpdatedReplicas == desired &&
			s.AvailableReplicas == desired && s.Replicas == desired,
		Updated:           s.ObservedGeneration >= d.Generation && s.UpdatedReplicas == desired,
		ForeignSpecChange: live != applied,
	}

	pods, err := currentPods(ctx, c, &d)
	if err != nil {
		return Observation{}, 0, err
	}
	var restarts int32
	for _, p := range pods {
		for _, cs := range p.Status.ContainerStatuses {
			restarts += cs.RestartCount
			if term := cs.LastTerminationState.Terminated; term != nil && term.Reason == "OOMKilled" && !term.FinishedAt.Time.Before(since) {
				o.NewOOMKills++
			}
		}
	}
	return o, restarts, nil
}

// currentPods are the pods of the ReplicaSet at the Deployment's revision.
func currentPods(ctx context.Context, c client.Reader, d *appsv1.Deployment) ([]corev1.Pod, error) {
	selector, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return nil, err
	}
	in := []client.ListOption{client.InNamespace(d.Namespace), client.MatchingLabelsSelector{Selector: selector}}
	var sets appsv1.ReplicaSetList
	if err := c.List(ctx, &sets, in...); err != nil {
		return nil, fmt.Errorf("list ReplicaSets: %w", err)
	}
	var current *appsv1.ReplicaSet
	for i := range sets.Items {
		rs := &sets.Items[i]
		if metav1.IsControlledBy(rs, d) && rs.Annotations[revisionAnnotation] == d.Annotations[revisionAnnotation] {
			current = rs
		}
	}
	if current == nil {
		return nil, nil
	}
	var pods corev1.PodList
	if err := c.List(ctx, &pods, in...); err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	var out []corev1.Pod
	for _, p := range pods.Items {
		if metav1.IsControlledBy(&p, current) {
			out = append(out, p)
		}
	}
	return out, nil
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
	profile, _ := w.Config.Profile(in.Profile)
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
