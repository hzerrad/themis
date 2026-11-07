package metrics_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"

	"github.com/hzerrad/themis/internal/catalog/evidence/metrics"
	"github.com/hzerrad/themis/internal/config"
)

// A question goes to each source in turn: the first answer wins; without
// one, a failure wins over a source that couldn't answer.
func TestSources_AskInTurn(t *testing.T) {
	answer := metrics.Reading{Containers: map[string]float64{"app": 1}}
	noHistory := stub{name: "kubernetes", r: metrics.Reading{Unavailable: "no history"}}
	down := stub{name: "prometheus", err: errors.New("connection refused")}
	for name, tc := range map[string]struct {
		sources     metrics.Sources
		source      string
		unavailable string
		err         string
	}{
		"the first answers":       {sources: metrics.Sources{stub{name: "prometheus", r: answer}, noHistory}, source: "prometheus"},
		"the next answers":        {sources: metrics.Sources{down, stub{name: "kubernetes", r: answer}}, source: "kubernetes"},
		"none can":                {sources: metrics.Sources{noHistory, stub{name: "other", r: metrics.Reading{Unavailable: "no data"}}}, source: "other", unavailable: "no history; no data"},
		"one failed, none answer": {sources: metrics.Sources{down, noHistory}, err: "prometheus: connection refused"},
		"none set up":             {unavailable: metrics.NoSource},
	} {
		t.Run(name, func(t *testing.T) {
			r, err := tc.sources.PeakMemory(t.Context(), asked, 0)
			if tc.err != "" {
				if err == nil || err.Error() != tc.err {
					t.Errorf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || r.Source != tc.source || r.Unavailable != tc.unavailable {
				t.Errorf("reading = %+v, %v, want from %q, unavailable %q", r, err, tc.source, tc.unavailable)
			}
		})
	}
}

// History and Use ask each source in turn too; without an answer, the first
// source's reason is the one given, and the metrics API says it keeps none.
func TestSources_History(t *testing.T) {
	rs := metrics.ReplicaSet{Namespace: "shop", Name: "checkout-5c6a1e2f3"}
	none := stub{name: "prometheus", h: metrics.Record{Unavailable: metrics.NotInHistory(rs.Name)}, u: metrics.Use{Unavailable: metrics.NotInHistory(rs.Name)}}
	api := metrics.NewKubernetes(metricsfake.NewSimpleClientset())
	seen := stub{name: "prometheus", h: metrics.Record{Restarts: 2}, u: metrics.Use{Memory: map[string]float64{"app": 0.5}}}
	for name, tc := range map[string]struct {
		sources     metrics.Sources
		source      string
		unavailable string
	}{
		"the history answers":  {sources: metrics.Sources{seen, api}, source: "prometheus"},
		"the history has none": {sources: metrics.Sources{none, api}, source: "prometheus", unavailable: metrics.NotInHistory(rs.Name)},
		"only the metrics API": {sources: metrics.Sources{api}, source: "kubernetes", unavailable: metrics.NoRevisionHistory},
		"no source":            {unavailable: metrics.NoSource},
	} {
		t.Run(name, func(t *testing.T) {
			r, err := tc.sources.History(t.Context(), rs, time.Now())
			if err != nil || r.Source != tc.source || r.Unavailable != tc.unavailable {
				t.Errorf("record = %+v, %v, want from %q, unavailable %q", r, err, tc.source, tc.unavailable)
			}
			u, err := tc.sources.Use(t.Context(), rs, time.Now().Add(-time.Hour), time.Now())
			if err != nil || u.Source != tc.source || u.Unavailable != tc.unavailable {
				t.Errorf("use = %+v, %v, want from %q, unavailable %q", u, err, tc.source, tc.unavailable)
			}
		})
	}
}

// The configuration sets up Prometheus first, when it has an address, then
// the metrics API unless it is off, and each says whether it answers.
func TestFromConfig_Health(t *testing.T) {
	srv := httptest.NewServer(&promServer{reply: func(string) (int, string) { return http.StatusOK, vector(sample("", "1")) }})
	defer srv.Close()
	prom := &config.Prometheus{Address: srv.URL + "/prom"}

	served, err := metrics.FromConfig(config.Metrics{Prometheus: prom}, metricsAPI(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	h := served.Health(t.Context())
	if served.Name() != "prometheus, kubernetes" || len(h) != 2 || h[0].Err != nil || !h[0].History || h[1].Err != nil || h[1].History {
		t.Errorf("%s: health = %+v, want both answering, only Prometheus with history", served.Name(), h)
	}

	notServed, err := metrics.FromConfig(config.Metrics{}, metricsfake.NewSimpleClientset())
	if err != nil {
		t.Fatal(err)
	}
	if h := notServed.Health(t.Context()); len(h) != 1 || h[0].Err == nil || !strings.Contains(h[0].Err.Error(), "metrics-server") {
		t.Errorf("health = %+v, want the metrics API not served", h)
	}

	off, err := metrics.FromConfig(config.Metrics{Kubernetes: config.MetricsAPI{Disabled: true}}, nil)
	if err != nil || len(off) != 0 {
		t.Errorf("sources = %v, %v, want none", off, err)
	}
	if _, err := metrics.FromConfig(config.Metrics{Prometheus: &config.Prometheus{Address: srv.URL, TLS: config.TLS{CAFile: "/nonexistent"}}}, nil); err == nil {
		t.Error("a CA file that can't be read was accepted")
	}
}
