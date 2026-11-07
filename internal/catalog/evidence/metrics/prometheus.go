package metrics

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/platform/token"
)

// Prometheus is the source named in evidence for a server answering
// Prometheus queries.
const Prometheus = "prometheus"

// RateWindow is what "now" means for a rate, such as CPU use: the rate over
// the last 5 minutes, as kube-prometheus-stack's rules read CPU use and
// throttling. A rate over less says little, so a share of CPU periods
// throttled is never read over less either.
const RateWindow = 5 * time.Minute

const (
	// queryTimeout bounds each query.
	queryTimeout = 10 * time.Second
	// maxResponseBytes bounds a query's answer: one series per container.
	maxResponseBytes = 4 << 20
	// maxErrorBytes bounds the server's words in an error.
	maxErrorBytes = 256
)

// promQL reads the metrics the kubelet's cAdvisor exports, through
// /api/v1/query.
type promQL struct {
	query     *url.URL
	shown     string // the address, with any password hidden
	tokenFile string
	client    *http.Client
}

// NewPrometheus returns a server answering Prometheus queries as a source.
func NewPrometheus(cfg config.Prometheus) (Source, error) {
	base, err := url.Parse(cfg.Address)
	if err != nil {
		return nil, fmt.Errorf("metrics.prometheus.address: %w", err)
	}
	tlsConfig, err := cfg.TLS.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("metrics.prometheus.tls: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	return promQL{
		query:     base.JoinPath("api", "v1", "query"),
		shown:     base.Redacted(),
		tokenFile: cfg.BearerTokenFile,
		client:    &http.Client{Transport: transport},
	}, nil
}

func (promQL) Name() string { return Prometheus }

// The series of each container of the pods, without the pod's own sandbox
// and the pod-level totals, which have no container.
func selector(pods Pods) string {
	names := make([]string, 0, len(pods.Names))
	for _, n := range pods.Names {
		names = append(names, regexp.QuoteMeta(n))
	}
	// A raw string: the regular expression's backslashes go as they are.
	return fmt.Sprintf("namespace=%s, pod=~`%s`, container!=\"\", container!=\"POD\"", strconv.Quote(pods.Namespace), strings.Join(names, "|"))
}

// seconds writes a window as PromQL reads it.
func seconds(d time.Duration) string { return strconv.FormatInt(int64(d.Seconds()), 10) + "s" }

// promDuration writes a whole number of minutes as PromQL reads it: 5m.
func promDuration(d time.Duration) string { return strconv.FormatInt(int64(d.Minutes()), 10) + "m" }

const notScraped = ": check that it scrapes the kubelet's cAdvisor metrics"

func (p promQL) Memory(ctx context.Context, pods Pods) (Reading, error) {
	q := fmt.Sprintf("max by (container) (container_memory_working_set_bytes{%s})", selector(pods))
	return p.read(ctx, q, "Prometheus has no memory figures for these pods"+notScraped)
}

func (p promQL) PeakMemory(ctx context.Context, pods Pods, window time.Duration) (Reading, error) {
	q := fmt.Sprintf("max by (container) (max_over_time(container_memory_working_set_bytes{%s}[%s]))", selector(pods), seconds(window))
	return p.read(ctx, q, "Prometheus has no memory figures for these pods"+notScraped)
}

func (p promQL) CPU(ctx context.Context, pods Pods) (Reading, error) {
	q := fmt.Sprintf("max by (container) (rate(container_cpu_usage_seconds_total{%s}[%s]))", selector(pods), promDuration(RateWindow))
	return p.read(ctx, q, "Prometheus has no CPU figures for these pods"+notScraped)
}

func (p promQL) Throttling(ctx context.Context, pods Pods, window time.Duration) (Reading, error) {
	sel, w := selector(pods), seconds(window)
	q := fmt.Sprintf("max by (container) (sum by (pod, container) (rate(container_cpu_cfs_throttled_periods_total{%s}[%s])) / sum by (pod, container) (rate(container_cpu_cfs_periods_total{%s}[%s])))", sel, w, sel, w)
	return p.read(ctx, q, "Prometheus has no CPU throttling figures for these pods: only containers with a CPU limit have them")
}

// How far back, and how finely, History reads. Each bounds a query's cost,
// and neither decides anything: the lookback is the revision's own lifetime,
// and a revision older than maxHistory is read from maxHistory ago, as if the
// history began there.
const (
	maxHistory = 30 * 24 * time.Hour
	// historySteps is how many times a query looks at the history: each time
	// it reports is within one step, a thousandth of the lookback, of when it
	// happened.
	historySteps = 1000
	// minStep is the finest step, about two scrapes of kube-state-metrics.
	minStep = time.Minute
	// throttlingWindow is what a throttled share is over, unless a step is
	// longer: the windows then cover the whole lookback, step by step.
	throttlingWindow = time.Hour
)

// History reads how the pods rs owns ran, from kube-state-metrics' series
// for its pods, their restarts and OOM kills, and cAdvisor's for their CPU
// throttling. It finds the pods by kube_pod_owner, then reads the rest by
// their names, from since until now.
func (p promQL) History(ctx context.Context, rs ReplicaSet, since time.Time) (Record, error) {
	span := max(time.Since(since), minStep)
	if span > maxHistory {
		span = maxHistory
	}
	step := max((span / historySteps).Round(time.Second), minStep)
	r, pods, err := p.seen(ctx, rs, span, step)
	if err != nil || !r.Answered() {
		return r, err
	}
	names := make([]string, 0, len(pods))
	for _, n := range pods {
		names = append(names, regexp.QuoteMeta(n))
	}
	sel := fmt.Sprintf("namespace=%s, pod=~`%s`", strconv.Quote(rs.Namespace), strings.Join(names, "|"))
	restarts := fmt.Sprintf("kube_pod_container_status_restarts_total{%s}", sel)
	oom := fmt.Sprintf(`kube_pod_container_status_last_terminated_reason{%s, reason="OOMKilled"}`, sel)
	window := seconds(max(step, throttlingWindow))
	throttled := fmt.Sprintf("(sum by (pod, container) (rate(container_cpu_cfs_throttled_periods_total{%[1]s, container!=\"\"}[%[2]s]))"+
		" / sum by (pod, container) (rate(container_cpu_cfs_periods_total{%[1]s, container!=\"\"}[%[2]s])))", sel, window)
	sub := "[" + seconds(span) + ":" + seconds(step) + "]"
	parts := []struct{ name, query string }{
		{"restarts", fmt.Sprintf("sum(max_over_time(%s[%s]))", restarts, seconds(span))},
		{"first_restart", fmt.Sprintf("min(min_over_time(timestamp(last_over_time(%s[%s]) > 0)%s))", restarts, seconds(step), sub)},
		{"oom_killed", fmt.Sprintf("count(max_over_time(%s[%s]) == 1)", oom, seconds(span))},
		{"first_oom_kill", fmt.Sprintf("min(min_over_time(timestamp(last_over_time(%s[%s]) == 1)%s))", oom, seconds(step), sub)},
		{"throttled", fmt.Sprintf("max(max_over_time(%s%s))", throttled, sub)},
		{"first_throttled", fmt.Sprintf("min(min_over_time(timestamp(%s >= %g)%s))", throttled, ThrottledShare, sub)},
	}
	queries := make([]string, 0, len(parts))
	for _, part := range parts {
		queries = append(queries, fmt.Sprintf(`label_replace(%s, "part", %q, "", "")`, part.query, part.name))
	}
	series, err := p.run(ctx, strings.Join(queries, " or "))
	if err != nil {
		return Record{}, err
	}
	for _, s := range series {
		v, ok := number(s)
		if !ok {
			continue
		}
		switch s.Metric["part"] {
		case "restarts":
			r.Restarts = int(v)
		case "first_restart":
			r.FirstRestart = unix(v)
		case "oom_killed":
			r.OOMKilled = int(v)
		case "first_oom_kill":
			r.FirstOOMKill = unix(v)
		case "throttled":
			r.Throttled = &v
		case "first_throttled":
			r.FirstThrottled = unix(v)
		}
	}
	return r, nil
}

// seen finds the pods rs owns in the history, and the first and last time it
// has any of them. A record that couldn't answer says why.
func (p promQL) seen(ctx context.Context, rs ReplicaSet, span, step time.Duration) (Record, []string, error) {
	owned := fmt.Sprintf(`kube_pod_owner{namespace=%s, owner_kind="ReplicaSet", owner_name=%s}`, strconv.Quote(rs.Namespace), strconv.Quote(rs.Name))
	// Each step looks back a whole step, so no pod falls between two.
	at := fmt.Sprintf("timestamp(last_over_time(%s[%s]))[%s:%s]", owned, seconds(step), seconds(span), seconds(step))
	series, err := p.run(ctx, fmt.Sprintf(`label_replace(min by (pod) (min_over_time(%[1]s)), "edge", "first", "", "")`+
		` or label_replace(max by (pod) (max_over_time(%[1]s)), "edge", "last", "", "")`, at))
	if err != nil {
		return Record{}, nil, err
	}
	var r Record
	var pods []string
	for _, s := range series {
		v, ok := number(s)
		pod := s.Metric["pod"]
		if !ok || pod == "" {
			continue
		}
		if !slices.Contains(pods, pod) {
			pods = append(pods, pod)
		}
		switch t := unix(v); s.Metric["edge"] {
		case "first":
			if r.SeenFrom.IsZero() || t.Before(r.SeenFrom) {
				r.SeenFrom = t
			}
		case "last":
			if t.After(r.SeenUntil) {
				r.SeenUntil = t
			}
		}
	}
	if len(pods) == 0 || r.SeenFrom.IsZero() {
		r.Unavailable = NotInHistory(rs.Name)
		return r, nil, nil
	}
	slices.Sort(pods)
	return r, pods, nil
}

// Use reads how much of their limits the containers of the pods rs owns used
// on average from from to to: cAdvisor's working set and CPU use over
// kube-state-metrics' limits, at each step of the span, for the pods
// kube_pod_owner says rs owns then. Each container's average is that of the
// pod where it was highest.
func (p promQL) Use(ctx context.Context, rs ReplicaSet, from, to time.Time) (Use, error) {
	span := max(to.Sub(from), minStep)
	step := max((span / historySteps).Round(time.Second), minStep)
	ns := strconv.Quote(rs.Namespace)
	owned := fmt.Sprintf(`max by (pod) (kube_pod_owner{namespace=%s, owner_kind="ReplicaSet", owner_name=%s})`, ns, strconv.Quote(rs.Name))
	limit := func(resource string) string {
		return fmt.Sprintf(`max by (pod, container) (kube_pod_container_resource_limits{namespace=%s, resource=%q} > 0)`, ns, resource)
	}
	containers := fmt.Sprintf(`namespace=%s, container!="", container!="POD"`, ns)
	shares := map[string]string{
		"memory": fmt.Sprintf("max by (pod, container) (container_memory_working_set_bytes{%s})", containers),
		"cpu":    fmt.Sprintf("max by (pod, container) (rate(container_cpu_usage_seconds_total{%s}[%s]))", containers, promDuration(RateWindow)),
	}
	queries := make([]string, 0, len(shares))
	for _, part := range []string{"memory", "cpu"} {
		share := fmt.Sprintf("%s / on (pod, container) %s * on (pod) group_left() %s", shares[part], limit(part), owned)
		queries = append(queries, fmt.Sprintf(`label_replace(max by (container) (avg_over_time((%s)[%s:%s])), "part", %q, "", "")`,
			share, seconds(span), seconds(step), part))
	}
	series, err := p.runAt(ctx, strings.Join(queries, " or "), from.Add(span))
	if err != nil {
		return Use{}, err
	}
	u := Use{Memory: map[string]float64{}, CPU: map[string]float64{}}
	for _, s := range series {
		v, ok := number(s)
		c := s.Metric["container"]
		if !ok || c == "" {
			continue
		}
		switch s.Metric["part"] {
		case "memory":
			u.Memory[c] = v
		case "cpu":
			u.CPU[c] = v
		}
	}
	if len(u.Memory) == 0 && len(u.CPU) == 0 {
		u.Unavailable = "Prometheus has no memory or CPU figures, against a limit, for the pods of ReplicaSet " + rs.Name +
			" from " + from.UTC().Format(time.RFC3339) + " to " + to.UTC().Format(time.RFC3339) + notScraped + " and kube-state-metrics"
	}
	return u, nil
}

// NotInHistory is why Prometheus can't say how the pods of a ReplicaSet ran.
func NotInHistory(replicaSet string) string {
	return "Prometheus has no pods of ReplicaSet " + replicaSet + " in its history: it keeps none that old, or doesn't scrape kube-state-metrics"
}

// number is a series' value, if it is a number.
func number(s series) (float64, bool) {
	if len(s.Value) != 2 {
		return 0, false
	}
	text, _ := s.Value[1].(string)
	v, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// unix is a Prometheus timestamp, in seconds, as a time.
func unix(v float64) time.Time {
	return time.Unix(0, int64(v*float64(time.Second))).UTC().Truncate(time.Second)
}

// read runs an instant query whose series each carry a container label. No
// series at all is the empty reason.
func (p promQL) read(ctx context.Context, query, empty string) (Reading, error) {
	series, err := p.run(ctx, query)
	if err != nil {
		return Reading{}, err
	}
	r := Reading{Containers: map[string]float64{}}
	for _, s := range series {
		// 0/0 is NaN: a container that ran no CPU periods.
		v, ok := number(s)
		if !ok || s.Metric["container"] == "" {
			continue
		}
		r.Containers[s.Metric["container"]] = v
	}
	if len(r.Containers) == 0 {
		r.Unavailable = empty
	}
	return r, nil
}

type series struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value"`
}

type response struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string   `json:"resultType"`
		Result     []series `json:"result"`
	} `json:"data"`
}

func (p promQL) run(ctx context.Context, query string) ([]series, error) {
	return p.runAt(ctx, query, time.Time{})
}

// runAt runs query as of at, or now when at is zero.
func (p promQL) runAt(ctx context.Context, query string, at time.Time) ([]series, error) {
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	form := url.Values{"query": {query}}
	if !at.IsZero() {
		form.Set("time", strconv.FormatInt(at.Unix(), 10))
	}
	req, err := http.NewRequestWithContext(qctx, http.MethodPost, p.query.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if p.tokenFile != "" {
		secret, err := token.Read(p.tokenFile)
		if err != nil {
			return nil, fmt.Errorf("prometheus token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+string(secret))
	}
	res, err := p.client.Do(req)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return nil, fmt.Errorf("prometheus at %s didn't answer within %s", p.shown, queryTimeout)
	}
	if err != nil {
		return nil, fmt.Errorf("query Prometheus at %s: %w", p.shown, err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read prometheus's answer: %w", err)
	}
	var out response
	decodeErr := json.Unmarshal(body, &out)
	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("prometheus at %s refused Themis (%s): check metrics.prometheus.bearerTokenFile", p.shown, res.Status)
	case decodeErr == nil && out.Status == "error":
		return nil, fmt.Errorf("prometheus at %s answered %s: %s", p.shown, res.Status, cut(out.Error))
	case res.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("prometheus at %s answered %s", p.shown, res.Status)
	case decodeErr != nil:
		return nil, fmt.Errorf("prometheus at %s answered with something other than a query result: %w", p.shown, decodeErr)
	case out.Data.ResultType != "vector":
		return nil, fmt.Errorf("prometheus at %s answered a %q, not a vector", p.shown, cut(out.Data.ResultType))
	}
	return out.Data.Result, nil
}

// cut bounds what the server wrote, which Themis shows.
func cut(s string) string {
	if len(s) <= maxErrorBytes {
		return s
	}
	return strings.ToValidUTF8(s[:maxErrorBytes], "")
}

func (p promQL) probe(ctx context.Context) error {
	_, err := p.run(ctx, "vector(1)")
	return err
}

func (promQL) history() bool { return true }
