// Package reconciliation notices targets changed outside Themis before Themis
// acts on them: fixed by hand or through GitOps, deleted, or changed and
// still failing.
package reconciliation

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/investigation"
	"github.com/hzerrad/themis/internal/platform/telemetry"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/workload"
)

const actor = "reconcile"

// Settle is how long a change must stand before it is judged, so a rollout
// still in progress is not mistaken for its outcome.
const Settle = time.Minute

// perRun is how many incidents one run looks at, unless Worker.PerRun says
// otherwise.
const perRun = 100

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
	// Config names each incident's profiles, whose verification checks and
	// symptoms judge a change made outside Themis. Checks decides them again
	// from what the new pods did since the change, and Words describes them.
	// Without Config or Checks, a change is judged by its rollout alone.
	Config config.Provider
	Checks Rechecker
	Words  check.List
	// PerRun bounds the incidents one run looks at, those it looked at
	// longest ago first, so every incident still comes up in turn. Zero
	// means 100.
	PerRun int
}

// Rechecker decides checks again from what a workload did over a window,
// as catalog.Catalog does, and says how long the workload must run before
// they can be.
type Rechecker interface {
	Recheck(ctx context.Context, c client.Reader, w *workload.Workload, names []string, window time.Duration, now time.Time) []check.Result
	SignalWindow(names []string) time.Duration
}

func (w *Worker) Work(ctx context.Context, _ *river.Job[Args]) error {
	return w.Reconcile(ctx)
}

// Reconcile looks at open incidents not yet executing, those it looked at
// longest ago first. One target that cannot be read does not stop the
// others.
func (w *Worker) Reconcile(ctx context.Context) error {
	n := w.PerRun
	if n <= 0 {
		n = perRun
	}
	incidents, err := w.Store.NextReconcilable(ctx, n)
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

// one judges a change made outside Themis by what it did to the workload. A
// rollout that isn't complete, ready and free of OOM kills since the change
// still fails. One that is recovered only once the profiles' verification
// checks and symptoms, decided again from what the new pods did since the
// change, find no failure, and every verification check can be told: Themis
// never claims a recovery it can't see, such as throttling without a metrics
// source. Anything short of recovered supersedes the incident, saying why.
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
	now := w.now()
	if target.TemplateHash == in.ObservedSpecHash || now.Sub(target.WrittenAt) < Settle {
		return nil
	}

	by := target.Writer
	if by == "" {
		by = "an unknown writer"
	}
	changed := "The workload was changed outside Themis, by " + by
	state, err := target.State(ctx, target.WrittenAt)
	if err != nil {
		return err
	}
	if !state.RolloutComplete || state.Ready < state.Desired || state.OOMKills > 0 {
		return w.supersede(ctx, in, changed+", and still fails.")
	}
	found, wait, err := w.recheck(ctx, in, target, now)
	if err != nil || wait {
		return err
	}
	switch {
	case len(found.failing) > 0:
		return w.supersede(ctx, in, changed+", and still fails. "+since(found.failing, w.Words))
	case len(found.unknown) > 0:
		return w.supersede(ctx, in, changed+". Unknown whether it recovered. "+since(found.unknown, w.Words))
	}
	reason := "Recovered after a change outside Themis, by " + by + "."
	if len(found.cleared) > 0 {
		reason += " " + since(found.cleared, w.Words)
	}
	to := incident.SelfResolved
	if !incident.CanTransition(in.State, to) {
		to = incident.Canceled
	}
	return w.close(ctx, in, to, reason)
}

// findings are what the checks found since a change made outside Themis:
// those that find the failure still, the verification checks that can't be
// told, and those that no longer find it, of the verification checks and
// the symptoms the incident's checks found when it opened.
type findings struct {
	failing, unknown, cleared []check.Result
}

// recheck decides the incident's verification checks and symptoms again
// from what the new pods did since the change, read over the time since. It
// waits until the pods have run as long as the evidence those checks read
// over a window needs, such as 5 minutes for CPU throttling, so the answer
// rests on the new pods alone. A symptom that can't be told doesn't hold a
// recovery back: once every pod is ready, when the failures started can't
// be, for one. The rollout and the verification checks decide then.
func (w *Worker) recheck(ctx context.Context, in store.Incident, target *workload.Workload, now time.Time) (findings, bool, error) {
	if w.Config == nil || w.Checks == nil {
		return findings{}, false, nil
	}
	m := w.Config.Current().Matched(in.Profiles())
	verification := m.Verification().Checks
	names := slices.Clone(verification)
	for _, s := range m.Symptoms() {
		if !slices.Contains(names, s) {
			names = append(names, s)
		}
	}
	if len(names) == 0 {
		return findings{}, false, nil
	}
	elapsed := now.Sub(target.WrittenAt)
	if elapsed < w.Checks.SignalWindow(names) {
		return findings{}, true, nil
	}
	before, err := w.Store.Predicates(ctx, in.ID)
	if err != nil {
		return findings{}, false, err
	}
	found := func(name string) bool {
		return slices.ContainsFunc(before, func(r check.Result) bool { return r.Name == name && r.Value == check.True })
	}
	var out findings
	for _, r := range w.Checks.Recheck(ctx, w.Reader, target, names, elapsed, now) {
		judged := slices.Contains(verification, r.Name)
		switch {
		case r.Value == check.True:
			out.failing = append(out.failing, r)
		case r.Value == check.Unknown && judged:
			out.unknown = append(out.unknown, r)
		case r.Value == check.False && (judged || found(r.Name)):
			out.cleared = append(out.cleared, r)
		}
	}
	return out, false, nil
}

// since says what the checks found since the change: "Since the change, the
// checks found that a container is throttled at its CPU limit (…)."
func since(results []check.Result, words check.List) string {
	parts := make([]string, 0, len(results))
	for _, r := range results {
		parts = append(parts, "that "+words.Found(r))
	}
	return "Since the change, the checks found " + strings.Join(parts, " and ") + "."
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
// now, to be investigated from scratch, saying why.
func (w *Worker) supersede(ctx context.Context, in store.Incident, why string) error {
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		return movedOn(investigation.Supersede(ctx, w.Store, tx, in, in.Target, "Replaced by a new incident. "+why, actor))
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
