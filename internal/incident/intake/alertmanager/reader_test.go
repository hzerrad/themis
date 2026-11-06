package alertmanager_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/alertmanager/pkg/labels"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/config/settings"
	"github.com/hzerrad/themis/internal/incident/intake"
	"github.com/hzerrad/themis/internal/incident/intake/alertmanager"
	"github.com/hzerrad/themis/internal/server/identity"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/metrictest"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

var t0 = time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)

// fakeAlertmanager serves Alertmanager's API v2: its alerts, filtered as
// Alertmanager filters them, and when it started.
type fakeAlertmanager struct {
	mu      sync.Mutex
	alerts  map[string]fakeAlert
	started time.Time
	// auth is the Authorization header it requires; status, an answer it
	// gives instead.
	auth    string
	status  int
	body    string
	queries []string
}

type fakeAlert struct {
	name             string
	state            string
	startsAt, endsAt time.Time
}

func (f *fakeAlertmanager) set(fp string, a fakeAlert) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a.state == "" {
		a.state = "active"
	}
	if a.endsAt.IsZero() {
		a.endsAt = a.startsAt.Add(24 * time.Hour)
	}
	f.alerts[fp] = a
}

func (f *fakeAlertmanager) remove(fp string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.alerts, fp)
}

func (f *fakeAlertmanager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, r.URL.Path+"?"+r.URL.RawQuery)
	switch {
	case f.auth != "" && r.Header.Get("Authorization") != f.auth:
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	case f.status != 0:
		http.Error(w, "broken", f.status)
	case f.body != "":
		_, _ = w.Write([]byte(f.body))
	case r.URL.Path == "/api/v2/status":
		_ = json.NewEncoder(w).Encode(map[string]any{"uptime": f.started, "versionInfo": map[string]string{"version": "0.34.1"}})
	case r.URL.Path == "/api/v2/alerts":
		out := []map[string]any{}
		for fp, a := range f.alerts {
			ls := map[string]string{"alertname": a.name, "namespace": "shop", "pod": "checkout-1"}
			if !matchesAll(r.URL.Query()["filter"], ls) {
				continue
			}
			out = append(out, map[string]any{
				"fingerprint": fp, "labels": ls, "annotations": map[string]string{"summary": "Pod is crash looping."},
				"startsAt": a.startsAt, "endsAt": a.endsAt, "generatorURL": "http://prometheus/graph",
				"status": map[string]any{"state": a.state},
			})
		}
		_ = json.NewEncoder(w).Encode(out)
	default:
		http.NotFound(w, r)
	}
}

func matchesAll(filter []string, ls map[string]string) bool {
	for _, f := range filter {
		m, err := labels.ParseMatcher(f)
		if err != nil || !m.Matches(ls[m.Name]) {
			return false
		}
	}
	return true
}

type readEnv struct {
	worker *alertmanager.ReadWorker
	am     *fakeAlertmanager
	server *httptest.Server
	store  *store.Store
	pool   *pgxpool.Pool
	now    time.Time
	jobs   int
}

// newRead reads a fake Alertmanager at the address the install sets, with
// install's other settings, for profiles answering KubePodCrashLooping and
// KubePodOOMKilled.
func newRead(t *testing.T, install config.Alertmanager) *readEnv {
	t.Helper()
	s, pool := pgtest.NewStore(t)
	e := &readEnv{am: &fakeAlertmanager{alerts: map[string]fakeAlert{}, started: t0.Add(-time.Hour)}, store: s, pool: pool, now: t0}
	e.server = httptest.NewServer(e.am)
	t.Cleanup(e.server.Close)
	if install.Address == "" {
		install.Address = e.server.URL
	}
	e.worker = e.newWorker(install)
	return e
}

func (e *readEnv) newWorker(install config.Alertmanager) *alertmanager.ReadWorker {
	return &alertmanager.ReadWorker{
		Connection: &alertmanager.Connection{
			Install: install, Addresses: &settings.Settings{Store: e.store, InstallAlertmanager: install}, Reads: e.store,
		},
		Config: config.Config{Profiles: []config.Profile{
			{Match: config.Match{AlertNames: []string{"KubePodCrashLooping"}}},
			{Match: config.Match{AlertNames: []string{"KubePodOOMKilled"}}},
		}},
		Store: e.store, Receiver: intake.NewReceiver(e.store),
		Now: func() time.Time { return e.now },
	}
}

// read reads once, at the clock moved on by d, and returns what it took:
// "firing a", "resolved b at 10:05".
func (e *readEnv) read(t *testing.T, d time.Duration) []string {
	t.Helper()
	e.now = e.now.Add(d)
	if err := e.worker.Read(t.Context()); err != nil {
		t.Fatalf("Read: %v", err)
	}
	return e.taken(t)
}

// taken returns the alerts queued for the intake since it was last called.
func (e *readEnv) taken(t *testing.T) []string {
	t.Helper()
	rows, err := e.pool.Query(t.Context(), `SELECT args FROM river_job WHERE kind = 'process_alert' ORDER BY id OFFSET $1`, e.jobs)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var args intake.ProcessAlertArgs
		if err := rows.Scan(&args); err != nil {
			t.Fatal(err)
		}
		a := args.Alert
		if a.Source != alertmanager.Source || a.Name == "" || a.StartsAt.IsZero() {
			t.Errorf("queued alert %+v lacks its source, name or start", a)
		}
		line := string(a.Status) + " " + a.SourceID
		if a.Status == intake.AlertResolved {
			line += " at " + a.EndsAt.UTC().Format("15:04")
		}
		out = append(out, line)
		e.jobs++
	}
	return out
}

func want(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("%s took %q, want %q", what, got, want)
	}
}

// Each alert is taken once when it fires and once when it ends. A silence
// doesn't end it.
func TestRead_FiringAndResolved(t *testing.T) {
	firing := metrictest.Delta(t, "themis_intake_alerts_taken_total", map[string]string{"source": "alertmanager", "status": "firing"})
	resolved := metrictest.Delta(t, "themis_intake_alerts_taken_total", map[string]string{"source": "alertmanager", "status": "resolved"})
	e := newRead(t, config.Alertmanager{})
	e.am.set("a", fakeAlert{name: "KubePodCrashLooping", startsAt: t0})
	e.am.set("b", fakeAlert{name: "KubePodOOMKilled", startsAt: t0, state: "suppressed"})
	e.am.set("other", fakeAlert{name: "TargetDown", startsAt: t0})

	want(t, "the first read", e.read(t, 0), "firing a")
	if q := e.am.queries[0]; !strings.Contains(q, "filter=alertname%3D~%22KubePodCrashLooping%7CKubePodOOMKilled%22") {
		t.Errorf("the read asked %q, want only the answered alerts", q)
	}
	var args intake.ProcessAlertArgs
	if err := e.pool.QueryRow(t.Context(), `SELECT args FROM river_job WHERE kind = 'process_alert'`).Scan(&args); err != nil {
		t.Fatal(err)
	}
	var payload struct{ Annotations map[string]string }
	if err := json.Unmarshal(args.Alert.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if a := args.Alert; a.Labels["pod"] != "checkout-1" || !a.EndsAt.IsZero() || payload.Annotations["summary"] != "Pod is crash looping." {
		t.Errorf("the firing alert = %+v, want its labels, annotations, and no end as a webhook sends it", a)
	}
	want(t, "the same list again", e.read(t, 15*time.Second))

	e.am.set("b", fakeAlert{name: "KubePodOOMKilled", startsAt: t0})
	want(t, "a silence that expired", e.read(t, 15*time.Second), "firing b")
	e.am.set("a", fakeAlert{name: "KubePodCrashLooping", startsAt: t0, state: "suppressed"})
	want(t, "a silence on an alert taken", e.read(t, 15*time.Second))

	e.am.remove("a")
	want(t, "an alert gone", e.read(t, 15*time.Second), "resolved a at 10:01")
	e.am.set("b", fakeAlert{name: "KubePodOOMKilled", startsAt: t0, endsAt: t0.Add(70 * time.Second)})
	want(t, "an alert whose end passed", e.read(t, 15*time.Second), "resolved b at 10:01")
	e.am.set("a", fakeAlert{name: "KubePodCrashLooping", startsAt: t0.Add(2 * time.Minute)})
	want(t, "an alert firing again", e.read(t, 15*time.Second), "firing a")
	e.am.set("a", fakeAlert{name: "KubePodCrashLooping", startsAt: t0.Add(3 * time.Minute)})
	want(t, "an alert that ended and fired again between reads", e.read(t, time.Minute), "resolved a at 10:03", "firing a")
	want(t, "an alert still firing after 4 hours", e.read(t, alertmanager.RepeatEvery), "firing a")

	if got := firing(); got != 5 {
		t.Errorf("firing alerts counted %v, want 5", got)
	}
	if got := resolved(); got != 3 {
		t.Errorf("resolved alerts counted %v, want 3", got)
	}
	last, err := e.store.LastAlertRead(t.Context(), alertmanager.Source)
	if err != nil || last.Address != e.server.URL || !last.ReadAt.Equal(e.now) || !last.FailedAt.IsZero() {
		t.Errorf("the last read = %+v, %v", last, err)
	}
}

// Alertmanager forgets its alerts when it restarts without storage, until
// Prometheus sends them again: an alert it lost ends only when its end passes.
func TestRead_AlertmanagerRestarted(t *testing.T) {
	e := newRead(t, config.Alertmanager{})
	e.am.set("a", fakeAlert{name: "KubePodCrashLooping", startsAt: t0, endsAt: t0.Add(4 * time.Minute)})
	want(t, "the first read", e.read(t, 0), "firing a")

	e.am.remove("a")
	e.am.started = t0.Add(30 * time.Second)
	want(t, "a restarted Alertmanager without it", e.read(t, 30*time.Second))
	e.am.set("a", fakeAlert{name: "KubePodCrashLooping", startsAt: t0, endsAt: t0.Add(6 * time.Minute)})
	want(t, "Prometheus sending it again", e.read(t, 30*time.Second))

	e.am.remove("a")
	e.am.started = t0.Add(2 * time.Minute)
	want(t, "restarted again, before its end", e.read(t, time.Minute))
	want(t, "once its end passed", e.read(t, 5*time.Minute), "resolved a at 10:06")
}

// The address or the matchers changing says nothing about the alerts taken
// before: they are forgotten, never resolved.
func TestRead_AnotherAlertmanager(t *testing.T) {
	e := newRead(t, config.Alertmanager{})
	e.am.set("a", fakeAlert{name: "KubePodCrashLooping", startsAt: t0})
	want(t, "the first read", e.read(t, 0), "firing a")

	other := httptest.NewServer(&fakeAlertmanager{alerts: map[string]fakeAlert{}, started: t0.Add(-time.Hour)})
	t.Cleanup(other.Close)
	e.worker = e.newWorker(config.Alertmanager{Address: e.server.URL, Found: true})
	if err := e.worker.Connection.SetAddress(t.Context(), other.URL, "alice"); err != nil {
		t.Fatal(err)
	}
	want(t, "another Alertmanager, without the alert", e.read(t, 15*time.Second))
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM read_alerts`); n != 0 {
		t.Errorf("%d alerts are still taken, want none", n)
	}
}

// Several core replicas read: each change is taken once, and a read that
// finishes after a later one changes nothing.
func TestRead_SeveralReplicas(t *testing.T) {
	e := newRead(t, config.Alertmanager{})
	e.am.set("a", fakeAlert{name: "KubePodCrashLooping", startsAt: t0})
	replicas := make([]*alertmanager.ReadWorker, 4)
	for i := range replicas {
		replicas[i] = e.newWorker(config.Alertmanager{Address: e.server.URL})
		at := t0.Add(time.Duration(i) * time.Millisecond)
		replicas[i].Now = func() time.Time { return at }
	}
	var wg sync.WaitGroup
	for _, r := range replicas {
		wg.Go(func() {
			if err := r.Read(t.Context()); err != nil {
				t.Errorf("Read: %v", err)
			}
		})
	}
	wg.Wait()
	want(t, "four replicas at once", e.taken(t), "firing a")

	e.am.remove("a")
	late := e.newWorker(config.Alertmanager{Address: e.server.URL})
	late.Now = func() time.Time { return t0.Add(-time.Second) }
	if err := late.Read(t.Context()); err != nil {
		t.Fatal(err)
	}
	want(t, "a read older than the last", e.taken(t))
}

// While no admin account exists, Alertmanager isn't read at all.
func TestRead_NoAdminAccount(t *testing.T) {
	e := newRead(t, config.Alertmanager{})
	e.am.set("a", fakeAlert{name: "KubePodCrashLooping", startsAt: t0})
	e.worker.Gate = &identity.Gate{Store: adminAccounts(false)}
	want(t, "a closed Themis", e.read(t, 0))
	if len(e.am.queries) != 0 {
		t.Errorf("Alertmanager was asked %q", e.am.queries)
	}
	e.worker.Gate = &identity.Gate{Store: adminAccounts(true)}
	want(t, "once an admin account exists", e.read(t, 15*time.Second), "firing a")
}

// The install's credentials go with the address it set.
func TestRead_Credentials(t *testing.T) {
	for name, tc := range map[string]struct {
		install func(dir string) config.Alertmanager
		header  string
	}{
		"a token": {func(dir string) config.Alertmanager {
			return config.Alertmanager{BearerTokenFile: file(t, dir, "token", "am-token\n")}
		}, "Bearer am-token"},
		"basic auth": {func(dir string) config.Alertmanager {
			return config.Alertmanager{BasicAuth: &config.BasicAuth{Username: "themis", PasswordFile: file(t, dir, "password", "s3cret")}}
		}, "Basic dGhlbWlzOnMzY3JldA=="},
	} {
		t.Run(name, func(t *testing.T) {
			e := newRead(t, config.Alertmanager{})
			e.am.auth = tc.header
			e.am.set("a", fakeAlert{name: "KubePodCrashLooping", startsAt: t0})
			install := tc.install(t.TempDir())
			install.Address = e.server.URL
			e.worker = e.newWorker(install)
			want(t, "a read with credentials", e.read(t, 0), "firing a")
		})
	}
}

func file(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A read that fails is recorded with why, takes nothing, and is counted.
func TestRead_Failures(t *testing.T) {
	for name, tc := range map[string]struct {
		breakIt func(*readEnv)
		kind    string
		says    string
	}{
		"unreachable":      {func(e *readEnv) { e.server.Close() }, store.ReadUnreachable, "alertmanager at http://127.0.0.1:%s doesn't answer: dial tcp"},
		"wrong token":      {func(e *readEnv) { e.am.auth = "Bearer other" }, store.ReadRefused, "alertmanager at http://127.0.0.1:%s answered 401 Unauthorized"},
		"broken":           {func(e *readEnv) { e.am.status = http.StatusBadGateway }, store.ReadUnexpected, "alertmanager at http://127.0.0.1:%s answered 502 Bad Gateway"},
		"not Alertmanager": {func(e *readEnv) { e.am.body = "<html>" }, store.ReadUnexpected, "the answer from http://127.0.0.1:%s isn't Alertmanager's"},
	} {
		t.Run(name, func(t *testing.T) {
			counted := metrictest.Delta(t, "themis_intake_reads_total", map[string]string{"source": "alertmanager", "result": tc.kind})
			e := newRead(t, config.Alertmanager{})
			e.am.set("a", fakeAlert{name: "KubePodCrashLooping", startsAt: t0})
			port := e.server.URL[strings.LastIndex(e.server.URL, ":")+1:]
			tc.breakIt(e)
			want(t, "a failed read", e.read(t, 0))
			last, err := e.store.LastAlertRead(t.Context(), alertmanager.Source)
			if err != nil || last.FailureKind != tc.kind || !strings.HasPrefix(last.Failure, fmt.Sprintf(tc.says, port)) || !last.FailedAt.Equal(t0) {
				t.Errorf("the last read = %+v, %v; want %s saying %q", last, err, tc.kind, fmt.Sprintf(tc.says, port))
			}
			if got := counted(); got != 1 {
				t.Errorf("reads counted %v, want 1", got)
			}
		})
	}
}

// The Setup page's test says what Alertmanager lists now, at the address
// given or the one in use.
func TestConnection_Test(t *testing.T) {
	e := newRead(t, config.Alertmanager{})
	e.am.set("a", fakeAlert{name: "KubePodCrashLooping", startsAt: t0})
	e.am.set("b", fakeAlert{name: "KubePodOOMKilled", startsAt: t0, state: "suppressed"})
	e.am.set("other", fakeAlert{name: "TargetDown", startsAt: t0})
	answered := []string{"KubePodCrashLooping", "KubePodOOMKilled"}

	got, err := e.worker.Connection.Test(t.Context(), "", answered)
	if err != nil || got.Address != e.server.URL || got.Version != "0.34.1" || got.Active != 2 ||
		len(got.Firing) != 1 || got.Firing[0] != (alertmanager.Firing{Name: "KubePodCrashLooping", Count: 1}) {
		t.Errorf("Test = %+v, %v; want 2 active, KubePodCrashLooping firing", got, err)
	}
	other := newRead(t, config.Alertmanager{Matchers: []string{`namespace="payments"`}})
	got, err = other.worker.Connection.Test(t.Context(), e.server.URL, answered)
	if err != nil || got.Active != 2 || len(got.Firing) != 0 {
		t.Errorf("Test of another address, for alerts in payments = %+v, %v; want 2 active, none answered", got, err)
	}
	for address, says := range map[string]string{
		"ftp://alertmanager": "alertmanager's address must be an http or https URL",
		"http://127.0.0.1:1": "alertmanager at http://127.0.0.1:1 doesn't answer",
	} {
		if _, err := e.worker.Connection.Test(t.Context(), address, answered); err == nil || !strings.HasPrefix(err.Error(), says) {
			t.Errorf("Test(%s) = %v, want %q", address, err, says)
		}
	}
	none := e.newWorker(config.Alertmanager{Disabled: true})
	if _, err := none.Connection.Test(t.Context(), "", answered); !errors.Is(err, alertmanager.ErrNoAddress) {
		t.Errorf("Test without an address = %v, want ErrNoAddress", err)
	}
}

// A read waiting or running keeps another from being queued, so slow reads
// don't pile up.
func TestReadArgs_OneQueuedAtATime(t *testing.T) {
	s, pool := pgtest.NewStore(t)
	for range 2 {
		if err := s.InTx(t.Context(), func(tx pgx.Tx) error { return s.EnqueueTx(t.Context(), tx, alertmanager.ReadArgs{}) }); err != nil {
			t.Fatal(err)
		}
	}
	if n := pgtest.Count(t, pool, `SELECT count(*) FROM river_job WHERE kind = $1`, alertmanager.ReadArgs{}.Kind()); n != 1 {
		t.Errorf("%d reads queued, want 1", n)
	}
}
