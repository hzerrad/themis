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

// read runs an instant query whose series each carry a container label. No
// series at all is the empty reason.
func (p promQL) read(ctx context.Context, query, empty string) (Reading, error) {
	series, err := p.run(ctx, query)
	if err != nil {
		return Reading{}, err
	}
	r := Reading{Containers: map[string]float64{}}
	for _, s := range series {
		if len(s.Value) != 2 {
			continue
		}
		text, _ := s.Value[1].(string)
		v, err := strconv.ParseFloat(text, 64)
		// 0/0 is NaN: a container that ran no CPU periods.
		if err != nil || math.IsNaN(v) || s.Metric["container"] == "" {
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
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(qctx, http.MethodPost, p.query.String(), strings.NewReader(url.Values{"query": {query}}.Encode()))
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
