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
// from the install's metrics source's history.
const RevisionHealth = "revision_health"

// RevisionHealthView is how each revision ran: the current one first, then
// the one before it, when there is one.
type RevisionHealthView struct {
	Revisions []RevisionRecordView `json:"revisions"`
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
	return "How the pods of the current revision and of the one before it ran, from the metrics source's history: when it first and last has them, their restarts and OOM kills, and their CPU throttling, each with when it first showed. A source without history says so."
}

func (revisionHealth) Params() []string { return nil }

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
		case r.Number < w.Revision && (prior == nil || r.Number > prior.Number):
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
		rec, err := h.source.History(ctx, metrics.ReplicaSet{Namespace: w.Target.Namespace, Name: r.rev.Object.Name}, from)
		if err != nil {
			return Item{}, err
		}
		answered = answered || rec.Answered()
		if why == "" {
			why = rec.Unavailable
		}
		view.Revisions = append(view.Revisions, viewRecord(r.rev, r.rev == current, from, rec))
	}
	// A source with no history at all leaves nothing to read: a gap that
	// says why, as for figures no source has.
	if !answered {
		return Item{}, fmt.Errorf("%w: %s", ErrNoMetrics, why)
	}
	data, err := json.Marshal(view, json.Deterministic(true))
	if err != nil {
		return Item{}, err
	}
	return Item{Data: data}, nil
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
