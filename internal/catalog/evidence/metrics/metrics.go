// Package metrics reads how much memory and CPU a workload's containers use,
// and how the pods of its earlier revisions ran and used their limits, from
// whichever metrics source the install has: the Kubernetes metrics API, or a
// server that answers Prometheus queries. Evidence templates ask it in the same words
// whatever the source, and a source that can't answer says so rather than
// guess.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/hzerrad/themis/internal/config"
)

// Pods are the pods of one revision of a workload, whose containers a
// source reads.
type Pods struct {
	Namespace string
	Names     []string
}

// Reading is one quantity for each container of a revision: the highest
// value among its pods, by container name. Memory is in bytes, CPU in cores,
// and throttling is the share of CPU periods throttled, from 0 to 1.
type Reading struct {
	// Source names the source that answered, or that was asked last.
	Source string
	// Unavailable says why the source couldn't answer, such as a source that
	// keeps no history asked for a peak. Empty when it answered.
	Unavailable string
	Containers  map[string]float64
}

// Answered reports whether the reading holds an answer.
func (r Reading) Answered() bool { return r.Unavailable == "" }

// ReplicaSet names the pods of one revision of a workload by the ReplicaSet
// that owns them, which outlives them: a source with history reads how they
// ran after they are gone.
type ReplicaSet struct {
	Namespace string
	Name      string
}

// Record is how the pods of one ReplicaSet ran, from a source's history, up to
// now. A zero time is one the history doesn't show: for SeenFrom, that it has
// none of its pods; for the others, that it never happened while they ran.
type Record struct {
	// Source names the source that answered, or that was asked last.
	Source string
	// Unavailable says why the source couldn't answer, such as one that
	// keeps no history. Empty when it answered.
	Unavailable string

	// SeenFrom and SeenUntil are the first and last time the history has
	// any of its pods.
	SeenFrom, SeenUntil time.Time
	// Restarts counts its containers' restarts, and FirstRestart is when the
	// history first shows one.
	Restarts     int
	FirstRestart time.Time
	// OOMKilled counts its containers whose last end was an OOM kill at some
	// time, and FirstOOMKill is when the history first shows one.
	OOMKilled    int
	FirstOOMKill time.Time
	// Throttled is the largest share of CPU periods a container of it was
	// throttled in, over an hour, from 0 to 1; nil when none has a CPU limit.
	// FirstThrottled is when that share first reached a quarter, the share at
	// which kube-prometheus-stack's CPUThrottlingHigh fires.
	Throttled      *float64
	FirstThrottled time.Time
}

// Answered reports whether the record holds an answer.
func (r Record) Answered() bool { return r.Unavailable == "" }

// Use is how much of their limits the containers of one ReplicaSet's pods
// used on average over a span, from a source's history: memory working set
// over the memory limit, and CPU over the CPU limit, as shares, 1 being the
// limit. Each container's is that of the pod where it used the most. A
// container with no limit has none.
type Use struct {
	// Source names the source that answered, or that was asked last.
	Source string
	// Unavailable says why the source couldn't answer, such as one that
	// keeps no history. Empty when it answered.
	Unavailable string
	// Memory and CPU are each container's share, by container name.
	Memory, CPU map[string]float64
}

// Answered reports whether the use holds an answer.
func (u Use) Answered() bool { return u.Unavailable == "" }

// ThrottledShare is the share of an hour's CPU periods throttled at which a
// container counts as throttled at its limit in a record.
const ThrottledShare = 0.25

// Source reads what the containers of a workload's revision use. Each method
// answers for the pods given, and an answer it can't give is a Reading with
// Unavailable set, not an error: errors are for a source that failed.
type Source interface {
	// Name names the source in evidence, such as kubernetes or prometheus.
	Name() string
	// Memory is the memory each container uses now (its working set). A
	// source with history that has no figure that recent, as just after a
	// gap in its data, gives the latest it has within the window.
	Memory(ctx context.Context, pods Pods, window time.Duration) (Reading, error)
	// PeakMemory is the most memory each container used over the window.
	PeakMemory(ctx context.Context, pods Pods, window time.Duration) (Reading, error)
	// CPU is the CPU each container uses now, or, as for Memory, the
	// latest within the window.
	CPU(ctx context.Context, pods Pods, window time.Duration) (Reading, error)
	// Throttling is the share of each container's CPU periods throttled
	// over the window.
	Throttling(ctx context.Context, pods Pods, window time.Duration) (Reading, error)
	// History is how the pods rs owns ran from since until now.
	History(ctx context.Context, rs ReplicaSet, since time.Time) (Record, error)
	// Use is how much of their limits the containers of the pods rs owns
	// used on average from from to to.
	Use(ctx context.Context, rs ReplicaSet, from, to time.Time) (Use, error)
}

// NoSource is why nothing answers when no source is set up.
const NoSource = "no metrics source is set up"

// Sources are the sources an install reads, in the order they are asked.
// A question goes to each in turn until one answers, so a source with
// history comes first and the metrics API fills in when it can't answer.
type Sources []Source

// FromConfig sets up the sources cfg turns on: Prometheus first, when it has
// an address, then the Kubernetes metrics API, unless it is turned off.
func FromConfig(cfg config.Metrics, api metricsclient.Interface) (Sources, error) {
	var out Sources
	if p := cfg.Prometheus; p != nil {
		src, err := NewPrometheus(*p)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	if !cfg.Kubernetes.Disabled {
		out = append(out, NewKubernetes(api))
	}
	return out, nil
}

func (s Sources) Name() string {
	names := make([]string, 0, len(s))
	for _, src := range s {
		names = append(names, src.Name())
	}
	return strings.Join(names, ", ")
}

func (s Sources) Memory(ctx context.Context, pods Pods, window time.Duration) (Reading, error) {
	return s.ask(func(src Source) (Reading, error) { return src.Memory(ctx, pods, window) })
}

func (s Sources) PeakMemory(ctx context.Context, pods Pods, window time.Duration) (Reading, error) {
	return s.ask(func(src Source) (Reading, error) { return src.PeakMemory(ctx, pods, window) })
}

func (s Sources) CPU(ctx context.Context, pods Pods, window time.Duration) (Reading, error) {
	return s.ask(func(src Source) (Reading, error) { return src.CPU(ctx, pods, window) })
}

func (s Sources) Throttling(ctx context.Context, pods Pods, window time.Duration) (Reading, error) {
	return s.ask(func(src Source) (Reading, error) { return src.Throttling(ctx, pods, window) })
}

// History asks each source in turn, as for the other questions. Without an
// answer, the first source's reason is the one given: a source with history
// is asked first, and says more than one that keeps none.
func (s Sources) History(ctx context.Context, rs ReplicaSet, since time.Time) (Record, error) {
	if len(s) == 0 {
		return Record{Unavailable: NoSource}, nil
	}
	var errs []error
	var first *Record
	for _, src := range s {
		r, err := src.History(ctx, rs, since)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", src.Name(), err))
		case r.Answered():
			r.Source = src.Name()
			return r, nil
		case first == nil:
			r.Source = src.Name()
			first = &r
		}
	}
	if len(errs) > 0 {
		return Record{}, errors.Join(errs...)
	}
	return *first, nil
}

// Use asks each source in turn, as History does.
func (s Sources) Use(ctx context.Context, rs ReplicaSet, from, to time.Time) (Use, error) {
	if len(s) == 0 {
		return Use{Unavailable: NoSource}, nil
	}
	var errs []error
	var first *Use
	for _, src := range s {
		u, err := src.Use(ctx, rs, from, to)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", src.Name(), err))
		case u.Answered():
			u.Source = src.Name()
			return u, nil
		case first == nil:
			u.Source = src.Name()
			first = &u
		}
	}
	if len(errs) > 0 {
		return Use{}, errors.Join(errs...)
	}
	return *first, nil
}

// ask returns the first answer. Without one, a source that failed wins over
// one that couldn't answer: something is broken, and the error says what.
func (s Sources) ask(read func(Source) (Reading, error)) (Reading, error) {
	if len(s) == 0 {
		return Reading{Unavailable: NoSource}, nil
	}
	var errs []error
	var why []string
	last := ""
	for _, src := range s {
		r, err := read(src)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", src.Name(), err))
		case r.Answered():
			r.Source = src.Name()
			return r, nil
		default:
			why = append(why, r.Unavailable)
		}
		last = src.Name()
	}
	if len(errs) > 0 {
		return Reading{}, errors.Join(errs...)
	}
	return Reading{Source: last, Unavailable: strings.Join(why, "; ")}, nil
}

// Health is whether one source answers, for the status page.
type Health struct {
	Source string
	// History says whether the source answers peaks, throttling and how a
	// revision's pods ran, which need history.
	History bool
	// Err is why the source can't answer anything; nil when it can.
	Err error
}

// prober is a source that can say whether it answers at all.
type prober interface {
	probe(ctx context.Context) error
	history() bool
}

// Health asks each source whether it answers.
func (s Sources) Health(ctx context.Context) []Health {
	out := make([]Health, 0, len(s))
	for _, src := range s {
		h := Health{Source: src.Name()}
		if p, ok := src.(prober); ok {
			h.History, h.Err = p.history(), p.probe(ctx)
		}
		out = append(out, h)
	}
	return out
}
