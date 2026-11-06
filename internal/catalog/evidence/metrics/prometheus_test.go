package metrics_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/catalog/evidence/metrics"
	"github.com/hzerrad/themis/internal/config"
)

// promServer answers each query with reply(query), and records the last
// query and the token it came with.
type promServer struct {
	reply        func(query string) (status int, body string)
	query, token string
}

func (p *promServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/prom/api/v1/query" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	p.query, p.token = r.FormValue("query"), r.Header.Get("Authorization")
	status, body := p.reply(p.query)
	w.WriteHeader(status)
	_, _ = fmt.Fprint(w, body)
}

func vector(series ...string) string {
	return `{"status":"success","data":{"resultType":"vector","result":[` + strings.Join(series, ",") + `]}}`
}

func sample(container, value string) string {
	return fmt.Sprintf(`{"metric":{"container":%q},"value":[1730800000,%q]}`, container, value)
}

func newPrometheus(t *testing.T, srv *httptest.Server, edit func(*config.Prometheus)) metrics.Source {
	t.Helper()
	cfg := config.Prometheus{Address: srv.URL + "/prom"}
	if edit != nil {
		edit(&cfg)
	}
	src, err := metrics.NewPrometheus(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

var dotted = metrics.Pods{Namespace: "shop", Names: []string{"checkout-a", "checkout.b"}}

// Each question is one query about the pods' containers, whose series are
// read by container; a series without one, or without a number, is not.
func TestPrometheus_Queries(t *testing.T) {
	fake := &promServer{reply: func(string) (int, string) {
		return http.StatusOK, vector(sample("app", "123.5"), sample("", "999"), sample("idle", "NaN"))
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := newPrometheus(t, srv, func(c *config.Prometheus) { c.BearerTokenFile = tokenFile })

	for name, tc := range map[string]struct {
		read func() (metrics.Reading, error)
		want []string
	}{
		"memory": {
			func() (metrics.Reading, error) { return src.Memory(t.Context(), dotted) },
			[]string{"max by (container) (container_memory_working_set_bytes{"},
		},
		"peak memory": {
			func() (metrics.Reading, error) { return src.PeakMemory(t.Context(), dotted, 2*time.Hour) },
			[]string{"max_over_time(container_memory_working_set_bytes{", "[7200s]"},
		},
		"cpu": {
			func() (metrics.Reading, error) { return src.CPU(t.Context(), dotted) },
			[]string{"rate(container_cpu_usage_seconds_total{", "[5m]"},
		},
		"throttling": {
			func() (metrics.Reading, error) { return src.Throttling(t.Context(), dotted, time.Hour) },
			[]string{"container_cpu_cfs_throttled_periods_total{", "/ sum by (pod, container) (rate(container_cpu_cfs_periods_total{", "[3600s]"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, err := tc.read()
			if err != nil || !r.Answered() || len(r.Containers) != 1 || r.Containers["app"] != 123.5 {
				t.Errorf("reading = %+v, %v, want only app at 123.5", r, err)
			}
			// The pods' names are matched literally, and the pod's sandbox
			// and totals are left out.
			want := append(tc.want, `namespace="shop"`, "pod=~`checkout-a|checkout\\.b`", `container!=""`, `container!="POD"`)
			for _, w := range want {
				if !strings.Contains(fake.query, w) {
					t.Errorf("query %s lacks %s", fake.query, w)
				}
			}
			if fake.token != "Bearer s3cret" {
				t.Errorf("token = %q, want the file's", fake.token)
			}
		})
	}
}

func TestPrometheus_CantAnswer(t *testing.T) {
	for name, tc := range map[string]struct {
		status      int
		body        string
		edit        func(*config.Prometheus)
		unavailable string
		err         string
	}{
		"no series": {status: http.StatusOK, body: vector(), unavailable: "cAdvisor"},
		"a bad query": {
			status: http.StatusBadRequest, body: `{"status":"error","errorType":"bad_data","error":"parse error"}`,
			err: "answered 400 Bad Request: parse error",
		},
		"refused":        {status: http.StatusUnauthorized, body: "no", err: "bearerTokenFile"},
		"failing":        {status: http.StatusBadGateway, body: "<html>", err: "answered 502 Bad Gateway"},
		"not a result":   {status: http.StatusOK, body: "<html>", err: "something other than a query result"},
		"not a vector":   {status: http.StatusOK, body: `{"status":"success","data":{"resultType":"matrix","result":[]}}`, err: `"matrix"`},
		"no token":       {status: http.StatusOK, body: vector(), edit: func(c *config.Prometheus) { c.BearerTokenFile = "/nonexistent" }, err: "token"},
		"a hidden login": {status: http.StatusBadGateway, edit: withPassword, err: "xxxxx"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(&promServer{reply: func(string) (int, string) { return tc.status, tc.body }})
			defer srv.Close()
			r, err := newPrometheus(t, srv, tc.edit).Memory(t.Context(), dotted)
			switch {
			case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err) || strings.Contains(err.Error(), "hunter2")):
				t.Errorf("err = %v, want one saying %q", err, tc.err)
			case tc.err == "" && (err != nil || !strings.Contains(r.Unavailable, tc.unavailable)):
				t.Errorf("reading = %+v, %v, want unavailable saying %q", r, err, tc.unavailable)
			}
		})
	}
}

// labelled is a series with the labels given and a value.
func labelled(value string, labels ...string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(labels); i += 2 {
		m[labels[i]] = labels[i+1]
	}
	data, _ := json.Marshal(m) //nolint:errchkjson // a map of strings
	return fmt.Sprintf(`{"metric":%s,"value":[1730800000,%q]}`, data, value)
}

// History finds a ReplicaSet's pods by their owner, then reads how they ran
// by their names, over the lookback since asks for.
func TestPrometheus_History(t *testing.T) {
	var queries []string
	fake := &promServer{reply: func(q string) (int, string) {
		queries = append(queries, q)
		if strings.Contains(q, "kube_pod_owner") {
			return http.StatusOK, vector(
				labelled("1762200000", "pod", "checkout-5c6a1e2f3-a1b2c", "edge", "first"),
				labelled("1762300000", "pod", "checkout-5c6a1e2f3-a1b2c", "edge", "last"),
				labelled("1762100000", "pod", "checkout-5c6a1e2f3-d.e4f", "edge", "first"),
				labelled("1762250000", "pod", "checkout-5c6a1e2f3-d.e4f", "edge", "last"),
			)
		}
		return http.StatusOK, vector(
			labelled("4", "part", "restarts"),
			labelled("1762150000", "part", "first_restart"),
			labelled("1", "part", "oom_killed"),
			labelled("1762150000", "part", "first_oom_kill"),
			labelled("0.3", "part", "throttled"),
			labelled("1762160000", "part", "first_throttled"),
		)
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	since := time.Now().Add(-48 * time.Hour)
	r, err := newPrometheus(t, srv, nil).History(t.Context(), metrics.ReplicaSet{Namespace: "shop", Name: "checkout-5c6a1e2f3"}, since)
	if err != nil || !r.Answered() {
		t.Fatalf("record = %+v, %v", r, err)
	}
	if !r.SeenFrom.Equal(time.Unix(1762100000, 0)) || !r.SeenUntil.Equal(time.Unix(1762300000, 0)) {
		t.Errorf("seen %v to %v, want the earliest first and latest last of its pods", r.SeenFrom, r.SeenUntil)
	}
	if r.Restarts != 4 || r.OOMKilled != 1 || *r.Throttled != 0.3 || !r.FirstRestart.Equal(time.Unix(1762150000, 0)) || !r.FirstThrottled.Equal(time.Unix(1762160000, 0)) {
		t.Errorf("record = %+v, want each part read", r)
	}
	if len(queries) != 2 {
		t.Fatalf("queries = %q, want the pods, then how they ran", queries)
	}
	// Two days back at a thousand steps is a step of 173 seconds.
	for _, want := range []string{`owner_kind="ReplicaSet"`, `owner_name="checkout-5c6a1e2f3"`, `namespace="shop"`, ":173s]"} {
		if !strings.Contains(queries[0], want) {
			t.Errorf("query %s lacks %s", queries[0], want)
		}
	}
	for _, want := range []string{
		"pod=~`checkout-5c6a1e2f3-a1b2c|checkout-5c6a1e2f3-d\\.e4f`", "kube_pod_container_status_restarts_total{",
		`reason="OOMKilled"`, "container_cpu_cfs_throttled_periods_total{", ">= 0.25", "[3600s]", ":173s]",
	} {
		if !strings.Contains(queries[1], want) {
			t.Errorf("query %s lacks %s", queries[1], want)
		}
	}
}

// A ReplicaSet the history has none of is one it can't say anything about,
// and the lookback never goes past the cost bound.
func TestPrometheus_HistoryHasNone(t *testing.T) {
	fake := &promServer{reply: func(string) (int, string) { return http.StatusOK, vector() }}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	r, err := newPrometheus(t, srv, nil).History(t.Context(), metrics.ReplicaSet{Namespace: "shop", Name: "old"}, time.Now().Add(-400*24*time.Hour))
	if err != nil || r.Answered() || r.Unavailable != metrics.NotInHistory("old") {
		t.Errorf("record = %+v, %v, want it to say Prometheus has none of its pods", r, err)
	}
	if !strings.Contains(fake.query, "[2592000s:2592s]") {
		t.Errorf("query %s, want 30 days back at most", fake.query)
	}
}

// A password in the address is never shown.
func withPassword(c *config.Prometheus) {
	c.Address = strings.Replace(c.Address, "http://", "http://admin:hunter2@", 1)
}

// stub answers every question with one reading or error.
type stub struct {
	name string
	r    metrics.Reading
	h    metrics.Record
	err  error
}

func (s stub) Name() string { return s.name }

func (s stub) Memory(context.Context, metrics.Pods) (metrics.Reading, error) { return s.r, s.err }

func (s stub) PeakMemory(context.Context, metrics.Pods, time.Duration) (metrics.Reading, error) {
	return s.r, s.err
}

func (s stub) CPU(context.Context, metrics.Pods) (metrics.Reading, error) { return s.r, s.err }

func (s stub) Throttling(context.Context, metrics.Pods, time.Duration) (metrics.Reading, error) {
	return s.r, s.err
}

func (s stub) History(context.Context, metrics.ReplicaSet, time.Time) (metrics.Record, error) {
	return s.h, s.err
}
