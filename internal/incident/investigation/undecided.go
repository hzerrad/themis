package investigation

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
	"github.com/hzerrad/themis/internal/platform/telemetry"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/workload"
)

// CloseUndecidedArgs sweep incidents nobody suggested a fix for in time.
type CloseUndecidedArgs struct{}

// Kind is persisted with each job and must never change.
func (CloseUndecidedArgs) Kind() string { return "close_undecided_incidents" }

// CloseUndecidedWorker closes incidents Themis couldn't decide once their
// time for a suggestion has passed, unless they still fail: an incident
// waiting for a suggested fix whose symptoms are still true keeps waiting,
// and one whose symptoms cleared closes as recovered.
type CloseUndecidedWorker struct {
	river.WorkerDefaults[CloseUndecidedArgs]

	Config config.Provider
	Store  *store.Store
	Now    func() time.Time
	// Reader reads the targets, and Checks decides their symptoms again.
	// Without Checks, nothing is decided again: the stored checks keep an
	// incident whose failure they found waiting.
	Reader client.Reader
	Checks Rechecker
	Words  check.List
	Logger *slog.Logger
	// PerRun bounds the incidents one run looks at, those it looked at
	// longest ago first, so each run's cluster and metrics reads stay
	// bounded and every incident still comes up in turn. Zero means 100.
	PerRun int
}

// Rechecker decides checks again from what a workload did over a window,
// as catalog.Catalog does.
type Rechecker interface {
	Recheck(ctx context.Context, c client.Reader, w *workload.Workload, names []string, window time.Duration, now time.Time) []check.Result
}

// undecidedPerRun is how many incidents one run looks at, unless
// CloseUndecidedWorker.PerRun says otherwise.
const undecidedPerRun = 100

func (w *CloseUndecidedWorker) Work(ctx context.Context, _ *river.Job[CloseUndecidedArgs]) error {
	return w.Close(ctx)
}

func (w *CloseUndecidedWorker) Close(ctx context.Context) error {
	now := time.Now()
	if w.Now != nil {
		now = w.Now()
	}
	cfg := w.Config.Current()
	window, waiting := cfg.SuggestionWindow(), cfg.AwaitingSuggestionWindow()
	perRun := w.PerRun
	if perRun <= 0 {
		perRun = undecidedPerRun
	}
	undecided, err := w.Store.NextUndecided(ctx, now.Add(-window), now.Add(-waiting), perRun)
	if err != nil {
		return err
	}
	timedOut := map[incident.State]string{
		incident.Inconclusive:       "No fix was suggested, and no time to suggest one is configured.",
		incident.AwaitingSuggestion: "No fix was suggested within " + shortDuration(waiting) + ".",
	}
	if window > 0 {
		timedOut[incident.Inconclusive] = "No fix was suggested within " + shortDuration(window) + "."
	}
	for _, u := range undecided {
		to, reason := incident.ClosedNoAction, timedOut[u.State]
		if u.State == incident.AwaitingSuggestion {
			var wait bool
			to, reason, wait, err = w.recovered(ctx, cfg, u.ID, reason, now)
			if err != nil {
				w.logger().WarnContext(ctx, "cannot decide whether a waiting incident still fails", telemetry.FieldIncidentID, u.ID, telemetry.FieldError, err)
				continue
			}
			if wait {
				continue
			}
		}
		err := w.Store.InTx(ctx, func(tx pgx.Tx) error {
			return w.Store.Transition(ctx, tx, store.TransitionArgs{
				ID: u.ID, From: u.State, To: to, Reason: reason, Actor: actor,
			})
		})
		if err != nil && !errors.Is(err, store.ErrStateConflict) {
			return err
		}
	}
	return nil
}

// recovered decides how an incident left waiting for a suggested fix past
// its time ends, from its symptoms decided again now: it keeps waiting while
// one is true, or while one its checks found when it opened can't be told
// now; it closes as recovered once those are all false; and, when its checks
// never found its failure, it closes with no change, as timedOut says.
func (w *CloseUndecidedWorker) recovered(ctx context.Context, cfg config.Config, incidentID, timedOut string, now time.Time) (incident.State, string, bool, error) {
	in, err := w.Store.Incident(ctx, incidentID)
	if err != nil {
		return "", "", false, err
	}
	f, err := symptomsNow(ctx, w.Store, w.Reader, w.Checks, cfg, in, now)
	if workload.Lost(err) {
		// Watching the cluster ends an incident whose workload is gone.
		return "", "", true, nil
	}
	switch {
	case err != nil:
		return "", "", false, err
	case f.fails:
		return "", "", true, nil
	case !f.found:
		return incident.ClosedNoAction, timedOut, false, nil
	}
	return incident.SelfResolved, "Recovered before a fix. The checks now find " + that(f.cleared, w.Words) + ".", false, nil
}

// failure is what an incident's symptoms, decided again, find.
type failure struct {
	// fails says Themis can't close the incident as recovered: a symptom is
	// true, or one its checks found when it opened can't be told now.
	fails bool
	// found says its checks found its failure when it opened, and cleared
	// are those symptoms, false now.
	found   bool
	cleared []check.Result
}

// symptomsNow decides the symptoms of the profiles in matched again, from
// evidence read now. Without checks to decide them, the stored ones stand: an
// incident whose failure they found still fails. A target that can't be read
// is an error, which workload.Lost tells apart when it is gone.
func symptomsNow(ctx context.Context, s *store.Store, r client.Reader, checks Rechecker, cfg config.Config, in store.Incident, now time.Time) (failure, error) {
	symptoms := cfg.Matched(in.Profiles()).Symptoms()
	stored, err := s.Predicates(ctx, in.ID)
	if err != nil {
		return failure{}, err
	}
	var failed []string
	for _, r := range stored {
		if r.Value == check.True && slices.Contains(symptoms, r.Name) {
			failed = append(failed, r.Name)
		}
	}
	f := failure{found: len(failed) > 0}
	if checks == nil {
		f.fails = f.found
		return f, nil
	}
	if len(symptoms) == 0 {
		return f, nil
	}
	target, err := workload.Read(ctx, r, in.Target)
	if err != nil {
		return failure{}, err
	}
	for _, res := range checks.Recheck(ctx, r, target, symptoms, 0, now) {
		switch {
		case res.Value == check.True, slices.Contains(failed, res.Name) && res.Value == check.Unknown:
			return failure{fails: true, found: f.found}, nil
		case slices.Contains(failed, res.Name):
			f.cleared = append(f.cleared, res)
		}
	}
	return f, nil
}

// that lists what results found, each as words.Found puts it.
func that(results []check.Result, words check.List) string {
	parts := make([]string, 0, len(results))
	for _, r := range results {
		parts = append(parts, "that "+words.Found(r))
	}
	return strings.Join(parts, " and ")
}

func (w *CloseUndecidedWorker) logger() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}

// shortDuration writes d as people read it: 30m, 1h, 1h30m.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
