package evidence

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog/evidence/metrics"
	"github.com/hzerrad/themis/internal/workload"
)

// The templates that read how much memory and CPU the containers of the
// target's current revision use, from the install's metrics source.
const (
	ContainerMemory = "container_memory"
	ContainerCPU    = "container_cpu"
)

// ErrNoMetrics means no metrics source could say what the containers use.
var ErrNoMetrics = errors.New("no figures")

// The window peaks and throttling are read over, unless the window
// parameter sets another, from minWindow to maxWindow.
const (
	defaultWindow = time.Hour
	minWindow     = time.Minute
	maxWindow     = 7 * 24 * time.Hour
)

// ReadingView says which source answered a quantity, or why none could.
type ReadingView struct {
	Source      string `json:"source,omitempty"`
	Unavailable string `json:"unavailable,omitempty"`
}

// ContainerMemoryView is each container's memory against its request and
// limit. A figure no source could give is null.
type ContainerMemoryView struct {
	// Window is what the peak is over.
	Window     string          `json:"window"`
	Usage      ReadingView     `json:"usage"`
	Peak       ReadingView     `json:"peak"`
	Containers []MemoryUseView `json:"containers"`
}

type MemoryUseView struct {
	Name         string `json:"name"`
	RequestBytes *int64 `json:"requestBytes"`
	LimitBytes   *int64 `json:"limitBytes"`
	// UsageBytes and PeakBytes are the highest among the revision's pods.
	UsageBytes *int64 `json:"usageBytes"`
	PeakBytes  *int64 `json:"peakBytes"`
	// UsageOfLimit and PeakOfLimit are shares of the limit: 1 is all of it.
	UsageOfLimit *float64 `json:"usageOfLimit"`
	PeakOfLimit  *float64 `json:"peakOfLimit"`
}

// ContainerCPUView is each container's CPU against its request and limit,
// in cores. A figure no source could give is null.
type ContainerCPUView struct {
	// Window is what throttling is over.
	Window     string       `json:"window"`
	Usage      ReadingView  `json:"usage"`
	Throttling ReadingView  `json:"throttling"`
	Containers []CPUUseView `json:"containers"`
}

type CPUUseView struct {
	Name         string   `json:"name"`
	RequestCores *float64 `json:"requestCores"`
	LimitCores   *float64 `json:"limitCores"`
	// UsageCores is the highest among the revision's pods.
	UsageCores   *float64 `json:"usageCores"`
	UsageOfLimit *float64 `json:"usageOfLimit"`
	// ThrottledRatio is the share of CPU periods the container was held
	// back in, from 0 to 1: the highest among the pods.
	ThrottledRatio *float64 `json:"throttledRatio"`
}

type containerMemory struct{ source metrics.Source }

// NewContainerMemory returns the container_memory template, reading source.
// Its optional parameter is window, such as 30m: what the peak is over.
func NewContainerMemory(source metrics.Source) Template {
	return containerMemory{source: orNone(source)}
}

func (containerMemory) Name() string { return ContainerMemory }

func (containerMemory) Description() string {
	return "Memory each container of the current revision uses now and at its peak over a window (default 1h), against its request and limit, and which metrics source said so. A figure the source can't give is null, with why."
}

func (containerMemory) Params() []string { return []string{WindowParam} }

// MinWindow is the least a peak is read over.
func (containerMemory) MinWindow() time.Duration { return minWindow }

func (m containerMemory) Collect(ctx context.Context, _ client.Reader, w *workload.Workload, params map[string]string) (Item, error) {
	window, err := windowParam(params)
	if err != nil {
		return Item{}, err
	}
	pods, err := currentPods(ctx, w)
	if err != nil {
		return Item{}, err
	}
	usage, uerr := m.source.Memory(ctx, pods)
	peak, perr := m.source.PeakMemory(ctx, pods, window)
	if err := neither(usage, uerr, peak, perr); err != nil {
		return Item{}, err
	}
	view := ContainerMemoryView{Window: windowText(window), Usage: viewReading(usage, uerr), Peak: viewReading(peak, perr)}
	for _, c := range w.Template.Spec.Containers {
		v := MemoryUseView{
			Name:         c.Name,
			RequestBytes: bytesOf(c.Resources.Requests, corev1.ResourceMemory),
			LimitBytes:   bytesOf(c.Resources.Limits, corev1.ResourceMemory),
			UsageBytes:   wholeBytes(figure(usage, uerr, c.Name)),
			PeakBytes:    wholeBytes(figure(peak, perr, c.Name)),
		}
		v.UsageOfLimit = share(figure(usage, uerr, c.Name), asFloat(v.LimitBytes))
		v.PeakOfLimit = share(figure(peak, perr, c.Name), asFloat(v.LimitBytes))
		view.Containers = append(view.Containers, v)
	}
	data, err := json.Marshal(view, json.Deterministic(true))
	if err != nil {
		return Item{}, err
	}
	return Item{Data: data}, nil
}

type containerCPU struct{ source metrics.Source }

// NewContainerCPU returns the container_cpu template, reading source. Its
// optional parameter is window, such as 30m: what throttling is over.
func NewContainerCPU(source metrics.Source) Template { return containerCPU{source: orNone(source)} }

func (containerCPU) Name() string { return ContainerCPU }

func (containerCPU) Description() string {
	return "CPU each container of the current revision uses now, in cores, and how often it was throttled over a window (default 1h), against its request and limit, and which metrics source said so. A figure the source can't give is null, with why."
}

func (containerCPU) Params() []string { return []string{WindowParam} }

// MinWindow is the least throttling is read over: a share of CPU periods
// is a rate, and the metrics source reads a rate over metrics.RateWindow.
func (containerCPU) MinWindow() time.Duration { return max(minWindow, metrics.RateWindow) }

func (m containerCPU) Collect(ctx context.Context, _ client.Reader, w *workload.Workload, params map[string]string) (Item, error) {
	window, err := windowParam(params)
	if err != nil {
		return Item{}, err
	}
	pods, err := currentPods(ctx, w)
	if err != nil {
		return Item{}, err
	}
	usage, uerr := m.source.CPU(ctx, pods)
	throttling, terr := m.source.Throttling(ctx, pods, window)
	if err := neither(usage, uerr, throttling, terr); err != nil {
		return Item{}, err
	}
	view := ContainerCPUView{Window: windowText(window), Usage: viewReading(usage, uerr), Throttling: viewReading(throttling, terr)}
	for _, c := range w.Template.Spec.Containers {
		v := CPUUseView{
			Name:           c.Name,
			RequestCores:   coresOf(c.Resources.Requests),
			LimitCores:     coresOf(c.Resources.Limits),
			UsageCores:     rounded(figure(usage, uerr, c.Name)),
			ThrottledRatio: rounded(figure(throttling, terr, c.Name)),
		}
		v.UsageOfLimit = share(figure(usage, uerr, c.Name), v.LimitCores)
		view.Containers = append(view.Containers, v)
	}
	data, err := json.Marshal(view, json.Deterministic(true))
	if err != nil {
		return Item{}, err
	}
	return Item{Data: data}, nil
}

// orNone makes a missing source one that says none is set up.
func orNone(s metrics.Source) metrics.Source {
	if s == nil {
		return metrics.Sources(nil)
	}
	return s
}

func windowParam(params map[string]string) (time.Duration, error) {
	for k := range params {
		if k != WindowParam {
			return 0, fmt.Errorf("%w: unknown parameter %q", ErrInvalidParams, k)
		}
	}
	s, ok := params[WindowParam]
	if !ok {
		return defaultWindow, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < minWindow || d > maxWindow {
		return 0, fmt.Errorf("%w: window must be a duration from %s to %s, such as 30m", ErrInvalidParams, minWindow, maxWindow)
	}
	return d, nil
}

// currentPods are the pods of the target's current revision.
func currentPods(ctx context.Context, w *workload.Workload) (metrics.Pods, error) {
	all, err := w.Pods(ctx)
	if err != nil {
		return metrics.Pods{}, err
	}
	pods := metrics.Pods{Namespace: w.Target.Namespace}
	for _, p := range all {
		if p.Revision == w.Revision {
			pods.Names = append(pods.Names, p.Name)
		}
	}
	if len(pods.Names) == 0 {
		return metrics.Pods{}, ErrNoPods
	}
	return pods, nil
}

// neither is the error when neither quantity could be read: a source's
// failure, which tells the gap's cause, or else why none could answer.
func neither(a metrics.Reading, aerr error, b metrics.Reading, berr error) error {
	if (aerr == nil && a.Answered()) || (berr == nil && b.Answered()) {
		return nil
	}
	if err := errors.Join(aerr, berr); err != nil {
		return err
	}
	why := []string{a.Unavailable}
	if b.Unavailable != a.Unavailable {
		why = append(why, b.Unavailable)
	}
	return fmt.Errorf("%w: %s", ErrNoMetrics, strings.Join(why, "; "))
}

func viewReading(r metrics.Reading, err error) ReadingView {
	if err != nil {
		return ReadingView{Unavailable: err.Error()}
	}
	return ReadingView{Source: r.Source, Unavailable: r.Unavailable}
}

// figure is a container's value in r, or nil when r has none.
func figure(r metrics.Reading, err error, container string) *float64 {
	v, ok := r.Containers[container]
	if err != nil || !r.Answered() || !ok {
		return nil
	}
	return &v
}

func bytesOf(l corev1.ResourceList, name corev1.ResourceName) *int64 {
	q, ok := l[name]
	if !ok {
		return nil
	}
	v := q.Value()
	return &v
}

func coresOf(l corev1.ResourceList) *float64 {
	q, ok := l[corev1.ResourceCPU]
	if !ok {
		return nil
	}
	v := float64(q.MilliValue()) / 1000
	return &v
}

func wholeBytes(v *float64) *int64 {
	if v == nil {
		return nil
	}
	n := int64(math.Round(*v))
	return &n
}

func asFloat(v *int64) *float64 {
	if v == nil {
		return nil
	}
	f := float64(*v)
	return &f
}

// share is v as a share of limit, or nil without both or without a limit.
func share(v, limit *float64) *float64 {
	if v == nil || limit == nil || *limit == 0 {
		return nil
	}
	return rounded(new(*v / *limit))
}

// rounded keeps three decimals: a thousandth of a core, or of a share.
func rounded(v *float64) *float64 {
	if v == nil {
		return nil
	}
	r := math.Round(*v*1000) / 1000
	return &r
}

// windowText writes a window as people do: 1h, not 1h0m0s.
func windowText(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
