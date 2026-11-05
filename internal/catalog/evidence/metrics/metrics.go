// Package metrics reads how much memory and CPU a workload's containers use,
// from whichever metrics source the install has: the Kubernetes metrics API,
// or a server that answers Prometheus queries. Evidence templates ask it in
// the same words whatever the source, and a source that can't answer says so
// rather than guess.
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

// Source reads what the containers of a workload's revision use. Each method
// answers for the pods given, and an answer it can't give is a Reading with
// Unavailable set, not an error: errors are for a source that failed.
type Source interface {
	// Name names the source in evidence, such as kubernetes or prometheus.
	Name() string
	// Memory is the memory each container uses now (its working set).
	Memory(ctx context.Context, pods Pods) (Reading, error)
	// PeakMemory is the most memory each container used over the window.
	PeakMemory(ctx context.Context, pods Pods, window time.Duration) (Reading, error)
	// CPU is the CPU each container uses now.
	CPU(ctx context.Context, pods Pods) (Reading, error)
	// Throttling is the share of each container's CPU periods throttled
	// over the window.
	Throttling(ctx context.Context, pods Pods, window time.Duration) (Reading, error)
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

func (s Sources) Memory(ctx context.Context, pods Pods) (Reading, error) {
	return s.ask(func(src Source) (Reading, error) { return src.Memory(ctx, pods) })
}

func (s Sources) PeakMemory(ctx context.Context, pods Pods, window time.Duration) (Reading, error) {
	return s.ask(func(src Source) (Reading, error) { return src.PeakMemory(ctx, pods, window) })
}

func (s Sources) CPU(ctx context.Context, pods Pods) (Reading, error) {
	return s.ask(func(src Source) (Reading, error) { return src.CPU(ctx, pods) })
}

func (s Sources) Throttling(ctx context.Context, pods Pods, window time.Duration) (Reading, error) {
	return s.ask(func(src Source) (Reading, error) { return src.Throttling(ctx, pods, window) })
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
	// History says whether the source answers peaks and throttling, which
	// need history.
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
