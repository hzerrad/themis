package evidence

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog/evidence/metrics"
	"github.com/hzerrad/themis/internal/workload"
)

// RevisionHealth is the template that reads how the pods of the target's
// current revision, and of the one before it, ran while each was current,
// and how much of their limits they used, from the install's metrics
// source's history.
const RevisionHealth = "revision_health"

// RevisionHealthView is how each revision ran: the current one first, then
// the one before it, when there is one.
type RevisionHealthView struct {
	Revisions []RevisionRecordView `json:"revisions"`
	// ComparedAge is the time since each revision started over which their
	// use of their limits is compared: the current revision's time so far,
	// or the whole time the one before it ran, when that is shorter. Empty
	// without figures of both.
	ComparedAge string `json:"comparedAge,omitempty"`
}

// RevisionRecordView is how one revision's pods ran, from Since until the
// history's end. A time the record doesn't show is left out: for SeenFrom,
// that the record has none of its pods; for the others, that it didn't
// happen while they ran.
type RevisionRecordView struct {
	Revision   string `json:"revision"`
	ReplicaSet string `json:"replicaSet"`
	Current    bool   `json:"current,omitzero"`
	// Since is where the record starts: the current revision's rollout, or
	// the creation of the earlier revision's ReplicaSet.
	Since time.Time `json:"since"`

	// Source names what the record comes from, such as prometheus.
	// Unavailable says why there is none.
	Source      string `json:"source,omitempty"`
	Unavailable string `json:"unavailable,omitempty"`

	SeenFrom  time.Time `json:"seenFrom,omitzero"`
	SeenUntil time.Time `json:"seenUntil,omitzero"`

	Restarts       int       `json:"restarts"`
	FirstRestartAt time.Time `json:"firstRestartAt,omitzero"`
	OOMKilled      int       `json:"oomKilled"`
	FirstOOMKillAt time.Time `json:"firstOOMKillAt,omitzero"`
	// ThrottledRatio is the largest share of CPU periods a container was
	// throttled in, over an hour; null when none has a CPU limit.
	// FirstThrottledAt is when that share first reached a quarter.
	ThrottledRatio   *float64  `json:"throttledRatio"`
	FirstThrottledAt time.Time `json:"firstThrottledAt,omitzero"`

	// Memory and CPU are how much of its limit each container used on
	// average: memory working set over the memory limit, CPU use over the
	// CPU limit, 1 being the limit. Left out without figures, as for a
	// container with no limit.
	Memory []UseView `json:"memory,omitempty"`
	CPU    []UseView `json:"cpu,omitempty"`
}

// UseView is how much of its limit one container used on average, from 0 to
// 1: First over the first comparedAge since its revision started, and, for
// the revision before the current one, Last over the last comparedAge before
// its pods were gone. Null when the history has no figures for that time.
type UseView struct {
	Container string   `json:"container"`
	First     *float64 `json:"first"`
	Last      *float64 `json:"last,omitempty"`
}

type revisionHealth struct{ source metrics.Source }

// NewRevisionHealth returns the revision_health template, reading source's
// history. Each revision's record names its source, so another one, such as
// what Themis recorded of a change it made, can stand in for a revision the
// metrics history doesn't show.
func NewRevisionHealth(source metrics.Source) Template {
	return revisionHealth{source: orNone(source)}
}

func (revisionHealth) Name() string { return RevisionHealth }

func (revisionHealth) Description() string {
	return "How the pods of the current revision and of the one before it ran, from the metrics source's history: when it first and last has them, their restarts and OOM kills, and their CPU throttling, each with when it first showed; and how much of its memory and CPU limits each container used on average, over the same time since each revision started, and for the one before, over its last that long. A source without history says so."
}

func (revisionHealth) Params() []string { return nil }

// minComparedAge is the least time use is compared over: a step of the
// history.
const minComparedAge = time.Minute

func (h revisionHealth) Collect(ctx context.Context, _ client.Reader, w *workload.Workload, _ map[string]string) (Item, error) {
	revisions, err := w.Revisions(ctx)
	if err != nil {
		return Item{}, err
	}
	var current, prior *workload.Revision
	for i := range revisions {
		r := &revisions[i]
		switch {
		case r.Number == w.Revision && w.Revision > 0:
			current = r
		case r.Number > 0 && r.Number < w.Revision && (prior == nil || r.Number > prior.Number):
			prior = r
		}
	}
	if current == nil {
		return Item{}, ErrNoRevision
	}
	// The current revision rolled out when its ReplicaSet was made, or when
	// a later template change took it up again.
	since := current.Object.CreationTimestamp.UTC()
	if w.WrittenAt.After(since) {
		since = w.WrittenAt
	}
	view := RevisionHealthView{}
	var records []metrics.Record
	answered := false
	why := ""
	for _, r := range []struct {
		rev   *workload.Revision
		since time.Time
	}{{current, since}, {prior, time.Time{}}} {
		if r.rev == nil {
			continue
		}
		from := r.since
		if from.IsZero() {
			from = r.rev.Object.CreationTimestamp.UTC()
		}
		rec, err := h.source.History(ctx, replicaSetOf(w, r.rev), from)
		if err != nil {
			return Item{}, err
		}
		answered = answered || rec.Answered()
		if why == "" {
			why = rec.Unavailable
		}
		records = append(records, rec)
		view.Revisions = append(view.Revisions, viewRecord(r.rev, r.rev == current, from, rec))
	}
	// A source with no history at all leaves nothing to read: a gap that
	// says why, as for figures no source has.
	if !answered {
		return Item{}, fmt.Errorf("%w: %s", ErrNoMetrics, why)
	}
	if err := h.compare(ctx, w, current, prior, records, &view); err != nil {
		return Item{}, err
	}
	data, err := json.Marshal(view, json.Deterministic(true))
	if err != nil {
		return Item{}, err
	}
	return Item{Data: data}, nil
}

// compare reads how much of their limits the containers of both revisions
// used over the same time since each started, and the earlier one over its
// last that long too, once the history has both.
func (h revisionHealth) compare(ctx context.Context, w *workload.Workload, current, prior *workload.Revision, records []metrics.Record, view *RevisionHealthView) error {
	if len(records) < 2 || records[0].SeenFrom.IsZero() || records[1].SeenFrom.IsZero() {
		return nil
	}
	now, before := view.Revisions[0], view.Revisions[1]
	age := min(records[0].SeenUntil.Sub(now.Since), records[1].SeenUntil.Sub(before.Since))
	age = max(age, minComparedAge).Truncate(time.Second)
	read := func(r *workload.Revision, from time.Time) (metrics.Use, error) {
		return h.source.Use(ctx, replicaSetOf(w, r), from, from.Add(age))
	}
	cur, err := read(current, now.Since)
	if err != nil {
		return err
	}
	first, err := read(prior, before.Since)
	if err != nil {
		return err
	}
	last, err := read(prior, records[1].SeenUntil.Add(-age))
	if err != nil {
		return err
	}
	if !cur.Answered() && !first.Answered() && !last.Answered() {
		return nil
	}
	view.ComparedAge = windowText(age)
	view.Revisions[0].Memory, view.Revisions[0].CPU = uses(cur.Memory, nil), uses(cur.CPU, nil)
	view.Revisions[1].Memory, view.Revisions[1].CPU = uses(first.Memory, last.Memory), uses(first.CPU, last.CPU)
	return nil
}

// uses is each container's share over the first and the last span, by
// container name.
func uses(first, last map[string]float64) []UseView {
	var out []UseView
	for _, c := range keys(first, last) {
		u := UseView{Container: c}
		if v, ok := first[c]; ok {
			u.First = rounded(&v)
		}
		if v, ok := last[c]; ok {
			u.Last = rounded(&v)
		}
		out = append(out, u)
	}
	return out
}

func replicaSetOf(w *workload.Workload, r *workload.Revision) metrics.ReplicaSet {
	return metrics.ReplicaSet{Namespace: w.Target.Namespace, Name: r.Object.Name}
}

func viewRecord(r *workload.Revision, current bool, since time.Time, rec metrics.Record) RevisionRecordView {
	return RevisionRecordView{
		Revision: fmt.Sprint(r.Number), ReplicaSet: r.Object.Name, Current: current, Since: since,
		Source: rec.Source, Unavailable: rec.Unavailable,
		SeenFrom: rec.SeenFrom, SeenUntil: rec.SeenUntil,
		Restarts: rec.Restarts, FirstRestartAt: rec.FirstRestart,
		OOMKilled: rec.OOMKilled, FirstOOMKillAt: rec.FirstOOMKill,
		ThrottledRatio: rounded(rec.Throttled), FirstThrottledAt: rec.FirstThrottled,
	}
}
