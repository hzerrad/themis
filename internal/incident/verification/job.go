package verification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/check"
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
	// Fixes are the fixes Themis can carry out, which read how a target is
	// doing after them.
	Fixes capability.Registry
	// Checks decides the profiles' verification checks again from what the
	// workload did since the change, and Words describes them. Without
	// Checks, a fix is judged by its rollout alone.
	Checks Rechecker
	Words  check.List
}

// Rechecker decides checks again from what a workload did over a window,
// as catalog.Catalog does.
type Rechecker interface {
	Recheck(ctx context.Context, c client.Reader, w *workload.Workload, names []string, window time.Duration, now time.Time) []check.Result
	SignalWindow(names []string) time.Duration
}

func (w *Worker) Work(ctx context.Context, job *river.Job[VerifyArgs]) error {
	final, err := w.Verify(ctx, job.Args)
	// Retrying can't make an unsupported target supported.
	if err != nil && (job.Attempt >= job.MaxAttempts || errors.Is(err, workload.ErrUnsupported)) {
		// A write happened: never leave the incident in verifying.
		return w.escalate(ctx, job.Args.ActionID, "Not verified. Last error: "+err.Error()+". Referred to a person.")
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

// Verify reads the target once through the fix and reports whether the
// outcome is final.
func (w *Worker) Verify(ctx context.Context, args VerifyArgs) (bool, error) {
	a, err := w.Store.Action(ctx, w.Store.DB(), args.ActionID)
	if err != nil {
		return false, err
	}
	in, err := w.Store.Incident(ctx, a.IncidentID)
	if err != nil {
		return false, err
	}
	if _, ok := a.Restores(); ok {
		return w.verifyRestore(ctx, a, in, args)
	}
	if in.State != incident.Verifying {
		return true, nil
	}
	fix, err := w.fixOf(a)
	if err != nil {
		return false, err
	}
	// The strictest of the profiles offering the fix sets how it is watched.
	v := w.Config.Current().Matched(in.Profiles()).Offering(a.Capability).Verification()
	if w.Checks == nil {
		v.Checks = nil
	}
	criteria := Criteria{Window: v.Window.Duration, Stability: v.Stability.Duration}
	if len(v.Checks) > 0 {
		criteria.Signal = w.Checks.SignalWindow(v.Checks)
	}

	var from json.RawMessage
	switch rec, err := w.Store.Verification(ctx, w.Store.DB(), a.ID); {
	case err == nil:
		from = rec.Start
	case !errors.Is(err, store.ErrNotFound):
		return false, err
	}
	now := w.now()
	reading, err := fix.Check(ctx, w.Reader, in.Target, a.Mark, from, args.WindowStart)
	if err != nil {
		return false, err
	}

	var rec store.Verification
	err = w.Store.InTx(ctx, func(tx pgx.Tx) error {
		rec, err = w.Store.StartVerification(ctx, tx, store.Verification{ActionID: a.ID, WindowStart: args.WindowStart, Start: reading.Start})
		return err
	})
	if err != nil {
		return false, err
	}
	var stableFor time.Duration
	if reading.Health == capability.Healthy {
		if rec.HealthySince == nil {
			rec.HealthySince = &now
		}
		stableFor = now.Sub(*rec.HealthySince)
	} else {
		rec.HealthySince = nil
	}
	outcome, done := Evaluate(reading, stableFor, criteria, now.Sub(rec.WindowStart))
	if rec.Observation, err = json.Marshal(observed{Health: reading.Health, Facts: reading.Facts}); err != nil {
		return false, err
	}
	verdict := Rollout(outcome, reading)
	if done && len(v.Checks) > 0 {
		if verdict, err = w.judge(ctx, in, v.Checks, outcome, reading, now.Sub(rec.WindowStart), now); err != nil {
			return false, err
		}
	}
	if done {
		rec.Outcome = string(verdict.Outcome)
	}

	err = w.Store.InTx(ctx, func(tx pgx.Tx) error {
		if err := w.Store.UpdateVerification(ctx, tx, rec); err != nil || !done {
			return err
		}
		store.AfterCommit(tx, verificationOutcomes.WithLabelValues(string(verdict.Outcome)).Inc)
		if verdict.Restore {
			// Put back what the fix changed, so people find the workload as
			// they left it.
			if err := w.Store.Transition(ctx, tx, store.TransitionArgs{
				ID: in.ID, From: incident.Verifying, To: incident.Restoring, Reason: verdict.Reason + " Restoring the previous state.", Actor: "verify",
			}); err != nil {
				return err
			}
			return w.Store.EnqueueTx(ctx, tx, governance.RestoreArgs{ActionID: a.ID, IncidentID: in.ID})
		}
		return w.Store.Transition(ctx, tx, store.TransitionArgs{
			ID: in.ID, From: incident.Verifying, To: outcomes[verdict.Outcome], Reason: verdict.Reason, Actor: "verify",
		})
	})
	return done && err == nil, err
}

// judge decides the profiles' verification checks named again from what the
// workload did since the change, elapsed ago, and turns the rollout's
// outcome into the verdict they give.
func (w *Worker) judge(ctx context.Context, in store.Incident, names []string, outcome Outcome, r capability.Reading, elapsed time.Duration, now time.Time) (Verdict, error) {
	if outcome != Resolved && outcome != Unchanged {
		return Rollout(outcome, r), nil
	}
	target, err := workload.Read(ctx, w.Reader, in.Target)
	if err != nil {
		return Verdict{}, err
	}
	before, err := w.Store.Predicates(ctx, in.ID)
	if err != nil {
		return Verdict{}, err
	}
	found := w.Checks.Recheck(ctx, w.Reader, target, names, elapsed, now)
	return Judge(outcome, r, found, before, w.Words), nil
}

// observed is what a verification shows of its latest reading.
type observed struct {
	Health capability.Health
	Facts  string
}

// fixOf is the fix that carried out a.
func (w *Worker) fixOf(a store.Action) (capability.Capability, error) {
	fix, ok := w.Fixes[a.Capability]
	if !ok {
		return nil, fmt.Errorf("the fix %s isn't one Themis knows", a.Capability)
	}
	return fix, nil
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
	fix, err := w.fixOf(a)
	if err != nil {
		return false, err
	}
	reading, err := fix.Check(ctx, w.Reader, in.Target, a.Mark, nil, args.WindowStart)
	if err != nil {
		return false, err
	}
	window := w.Config.Current().Matched(in.Profiles()).Offering(a.Capability).Verification().Window.Duration
	var reason string
	switch {
	case reading.Changed:
		reason = "Restored, but the workload was then changed outside Themis. Referred to a person."
	case reading.Applied:
		reason = "Restored to the state before Themis acted. " + reading.Facts + " Referred to a person."
	case w.now().Sub(args.WindowStart) >= window:
		reason = "Restore incomplete. Kubernetes didn't finish the rollout within " + minutes(window) + ". " + reading.Facts + " Referred to a person."
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

// minutes writes a window as people read it: 10m, 1h, 1h30m.
func minutes(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
