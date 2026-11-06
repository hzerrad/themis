package alertmanager

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/alertmanager/template"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident/intake"
	"github.com/hzerrad/themis/internal/platform/fetch"
	"github.com/hzerrad/themis/internal/platform/telemetry"
	"github.com/hzerrad/themis/internal/server/identity"
	"github.com/hzerrad/themis/internal/store"
)

// RepeatEvery is how often an alert that keeps firing is taken again, as
// Alertmanager repeats its notifications by default. An alert that waited,
// such as one about a namespace enrolled since, gets another chance.
const RepeatEvery = 4 * time.Hour

// ReadArgs read Alertmanager's active alerts once.
type ReadArgs struct{}

// Kind is persisted with each job and must never change.
func (ReadArgs) Kind() string { return "read_alertmanager" }

// InsertOpts: a failed read isn't retried, the next one reads again, and a
// read waiting or running keeps another from being queued.
func (ReadArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 1, UniqueOpts: river.UniqueOpts{ByState: []rivertype.JobState{
		rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateScheduled,
	}}}
}

// ReadWorker reads the alerts the profiles answer from Alertmanager's API and
// feeds the intake what changed: an alert that starts firing, and one that
// ends. Each alert is taken once per episode, and again every RepeatEvery
// while it fires. What was taken is kept in the database, so any core replica
// reads, one at a time.
//
// An alert is taken only while active. One silenced or inhibited later stays
// taken: a silence stops notifications, not the problem. It ends when
// Alertmanager no longer lists it, or its end passes. Alertmanager forgets its
// alerts when it restarts without storage, until Prometheus sends them again,
// so an alert gone from an Alertmanager that started after it was last seen
// ends only once its end passes.
type ReadWorker struct {
	river.WorkerDefaults[ReadArgs]

	Connection *Connection
	// Config gives the profiles, whose alerts are read.
	Config   config.Provider
	Store    *store.Store
	Receiver *intake.Receiver
	// Gate stops reading while no admin account exists. Nil reads always.
	Gate   *identity.Gate
	Logger *slog.Logger
	Now    func() time.Time
}

func (w *ReadWorker) Work(ctx context.Context, _ *river.Job[ReadArgs]) error {
	return w.Read(ctx)
}

// Read reads Alertmanager once. A failure to reach it is recorded, not
// returned: the next read tries again.
func (w *ReadWorker) Read(ctx context.Context) error {
	open, err := w.Gate.Open(ctx)
	if err != nil || !open {
		return err
	}
	cl, view, err := w.Connection.reader(ctx)
	if err != nil {
		return w.failed(ctx, err, w.now())
	}
	if cl == nil {
		return nil
	}
	answered := w.Config.Current().AlertNames()
	matchers := strings.Join(w.Connection.Install.Matchers, ", ")
	// The database keeps microseconds.
	at := w.now().Truncate(time.Microsecond)
	var list []apiAlert
	if len(answered) > 0 {
		if list, err = cl.alerts(ctx, filter(answered, w.Connection.Install.Matchers)); err != nil {
			return w.failed(ctx, err, at)
		}
	}
	// Whether Alertmanager restarted matters only for an alert gone from it.
	var started time.Time
	taken, err := w.Store.ReadAlerts(ctx, w.Store.DB(), Source)
	if err != nil {
		return err
	}
	if anyGone(taken, list) {
		st, err := cl.status(ctx)
		if err != nil {
			return w.failed(ctx, err, at)
		}
		started = st.Uptime
	}

	var c changes
	stale := false
	err = w.Store.InTx(ctx, func(tx pgx.Tx) error {
		last, err := w.Store.LockAlertRead(ctx, tx, Source)
		if err != nil {
			return err
		}
		// Another replica read later and got here first.
		if stale = !at.After(last.ReadAt); stale {
			return nil
		}
		taken, err := w.Store.ReadAlerts(ctx, tx, Source)
		if err != nil {
			return err
		}
		// What another Alertmanager, or other alerts, listed says nothing
		// about these: forget it rather than end it.
		var forget []string
		if last.Address != view.Address || last.Matchers != matchers {
			for _, t := range taken {
				forget = append(forget, t.SourceID)
			}
			taken = nil
		}
		c = compare(taken, list, answered, started, at)
		alerts, _, err := (&Payload{Data: template.Data{Alerts: c.alerts}}).toAlerts(at)
		if err != nil {
			return err
		}
		if _, err := w.Receiver.ReceiveTx(ctx, tx, alerts, nil); err != nil {
			return err
		}
		if err := w.Store.PutReadAlerts(ctx, tx, Source, c.put, append(forget, c.drop...)); err != nil {
			return err
		}
		return w.Store.AlertReadSucceeded(ctx, tx, Source, view.Address, matchers, at)
	})
	if err != nil || stale {
		return err
	}
	intake.ObserveRead(Source, intake.ReadResultOK)
	intake.ObserveTaken(Source, intake.AlertFiring, c.firing)
	intake.ObserveTaken(Source, intake.AlertResolved, c.resolved)
	if c.firing+c.resolved > 0 {
		w.logger().InfoContext(ctx, "took alerts from Alertmanager", "firing", c.firing, "resolved", c.resolved)
	}
	return nil
}

// failed records a failed read. A first failure, or another than the last,
// is logged as a warning.
func (w *ReadWorker) failed(ctx context.Context, err error, at time.Time) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	kind, msg := fetch.KindOf(err), err.Error()
	intake.ObserveRead(Source, kind)
	level := slog.LevelWarn
	if last, lerr := w.Store.LastAlertRead(ctx, Source); lerr == nil && last.FailedAt.After(last.ReadAt) && last.Failure == msg {
		level = slog.LevelDebug
	}
	w.logger().Log(ctx, level, "cannot read Alertmanager", telemetry.FieldError, msg)
	return w.Store.AlertReadFailed(ctx, Source, kind, msg, at)
}

// filter selects the answered alerts, and those the install's matchers
// select.
func filter(answered, matchers []string) []string {
	quoted := make([]string, len(answered))
	for i, n := range answered {
		quoted[i] = regexp.QuoteMeta(n)
	}
	return append([]string{fmt.Sprintf(`alertname=~"%s"`, strings.Join(quoted, "|"))}, matchers...)
}

// anyGone reports whether an alert taken is no longer listed.
func anyGone(taken []store.ReadAlert, list []apiAlert) bool {
	listed := make(map[string]bool, len(list))
	for _, a := range list {
		listed[a.Fingerprint] = true
	}
	return slices.ContainsFunc(taken, func(t store.ReadAlert) bool { return !listed[t.SourceID] })
}

// changes are what one read takes, and what it keeps and forgets.
type changes struct {
	// alerts go to the intake: firing or resolved.
	alerts           template.Alerts
	firing, resolved int
	put              []store.ReadAlert
	drop             []string
}

func (c *changes) fire(a apiAlert) {
	c.alerts = append(c.alerts, template.Alert{
		Status: string(intake.AlertFiring), Labels: a.Labels, Annotations: a.Annotations,
		StartsAt: a.StartsAt, GeneratorURL: a.GeneratorURL, Fingerprint: a.Fingerprint,
	})
	c.firing++
}

func (c *changes) resolve(t store.ReadAlert, labels map[string]string, endsAt time.Time) {
	if labels == nil {
		labels = map[string]string{"alertname": t.Name}
	}
	c.alerts = append(c.alerts, template.Alert{
		Status: string(intake.AlertResolved), Labels: labels,
		StartsAt: t.StartsAt, EndsAt: endsAt, Fingerprint: t.SourceID,
	})
	c.resolved++
	c.drop = append(c.drop, t.SourceID)
}

// compare finds what changed between the alerts taken and those listed at
// now, by an Alertmanager that started at started (zero when unknown).
func compare(taken []store.ReadAlert, list []apiAlert, answered []string, started, now time.Time) changes {
	var c changes
	byID := make(map[string]store.ReadAlert, len(taken))
	for _, t := range taken {
		byID[t.SourceID] = t
	}
	listed := map[string]bool{}
	for _, a := range list {
		// The database keeps microseconds.
		a.StartsAt, a.EndsAt = a.StartsAt.Truncate(time.Microsecond), a.EndsAt.Truncate(time.Microsecond)
		name := a.Labels["alertname"]
		if a.Fingerprint == "" || name == "" || listed[a.Fingerprint] {
			continue
		}
		listed[a.Fingerprint] = true
		t, known := byID[a.Fingerprint]
		if known && !t.StartsAt.Equal(a.StartsAt) {
			// It ended and fired again.
			c.resolve(t, a.Labels, a.StartsAt)
			known = false
		}
		active := a.Status.State == stateActive
		switch {
		case !a.EndsAt.IsZero() && !a.EndsAt.After(now):
			if known {
				c.resolve(t, a.Labels, a.EndsAt)
			}
		case known:
			// Written only when something changed: Prometheus moves an
			// alert's end each time it sends it again.
			again := active && now.Sub(t.TakenAt) >= RepeatEvery
			if again {
				c.fire(a)
				t.TakenAt = now
			}
			if again || !t.EndsAt.Equal(a.EndsAt) {
				t.EndsAt, t.SeenAt = a.EndsAt, now
				c.put = append(c.put, t)
			}
		case active:
			c.fire(a)
			c.put = append(c.put, store.ReadAlert{
				SourceID: a.Fingerprint, Name: name, StartsAt: a.StartsAt, EndsAt: a.EndsAt, SeenAt: now, TakenAt: now,
			})
		}
	}
	for _, t := range taken {
		switch {
		case listed[t.SourceID]:
		case !slices.Contains(answered, t.Name):
			// No profile answers it now: nothing waits for its end.
			c.drop = append(c.drop, t.SourceID)
		case !started.IsZero() && !started.After(t.SeenAt):
			c.resolve(t, nil, now)
		case !t.EndsAt.After(now):
			c.resolve(t, nil, t.EndsAt)
		}
		// Otherwise Alertmanager may have restarted since it was seen, and
		// lost it until Prometheus sends it again.
	}
	return c
}

func (w *ReadWorker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *ReadWorker) logger() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}
