package core_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/hzerrad/themis/internal/config/resources"
	"github.com/hzerrad/themis/internal/core"
	"github.com/hzerrad/themis/internal/platform/token"
	"github.com/hzerrad/themis/internal/testutil/kubetest"
	"github.com/hzerrad/themis/internal/testutil/pgtest"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

var k8s *kubetest.Env

func TestMain(m *testing.M) {
	crds, err := themisCRDs()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	env, stop, err := kubetest.Start(crds...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start envtest (run `make envtest` first):", err)
		os.Exit(1)
	}
	k8s = env
	code := m.Run()
	stop()
	os.Exit(code)
}

// themisCRDs reads the chart's custom resource definitions, so the core can
// read its configuration from resources.
func themisCRDs() ([]*apiextensionsv1.CustomResourceDefinition, error) {
	var crds []*apiextensionsv1.CustomResourceDefinition
	for _, f := range []string{"incidentprofiles.yaml", "themisconfigs.yaml"} {
		data, err := os.ReadFile(filepath.Join("../../deploy/helm/themis/crds", f)) //nolint:gosec // fixed paths
		if err != nil {
			return nil, err
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(data, &crd); err != nil {
			return nil, err
		}
		crds = append(crds, &crd)
	}
	return crds, nil
}

const (
	smokeToken         = "smoke-token"
	investigatorSecret = "investigator-token"
	adminSecret        = "admin-token"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // a test file
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeConfig(t *testing.T, ns string) string {
	t.Helper()
	tokenFile := writeFile(t, "token", smokeToken+"\n")
	investigatorToken := writeFile(t, "investigator-token", investigatorSecret+"\n")
	adminToken := writeFile(t, "admin-token", adminSecret+"\n")
	return writeFile(t, "themis.yaml", fmt.Sprintf(`
enrolledNamespaces: [%s]
intake:
  bearerTokenFile: %s
investigators:
  bearerTokenFile: %s
api:
  bearerTokenFile: %s
profiles:
  - name: rollout-regression
    priority: 100
    match:
      alertNames: [KubePodCrashLooping]
      targetKinds: [Deployment.apps]
    evidence:
      initial: [rollout_history, pod_states]
      allowed: [rollout_history, pod_states]
`, ns, tokenFile, investigatorToken, adminToken))
}

// eventually polls cond until it holds or the timeout passes. It stops early if
// themis-core exits.
func eventually(t *testing.T, what string, exited <-chan error, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			t.Fatalf("themis-core exited while waiting for %s: %v", what, err)
		default:
		}
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// start runs themis-core until cancel or the end of the test, and waits until
// it is ready. A port freeAddr found can be taken before the core binds it,
// so start tries again with new ports.
func start(t *testing.T, options func() core.Options) (core.Options, <-chan error, context.CancelFunc) {
	t.Helper()
	for attempt := 1; ; attempt++ {
		o := options()
		ctx, cancelRun := context.WithCancel(context.Background())
		exited, stopped := make(chan error, 1), make(chan struct{})
		go func() { exited <- core.Run(ctx, o); close(stopped) }()
		// Stopping waits for Run to return, so the next core doesn't start
		// while this one still holds its metrics and ports.
		cancel := func() { cancelRun(); <-stopped }
		ready, err := waitReady(o, exited)
		if ready {
			t.Cleanup(cancel)
			return o, exited, cancel
		}
		cancel()
		if !errors.Is(err, syscall.EADDRINUSE) || attempt == 3 {
			t.Fatalf("themis-core did not become ready: %v", err)
		}
	}
}

// waitReady reports whether the core became ready, or why it exited.
func waitReady(o core.Options, exited <-chan error) (bool, error) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			return false, err
		default:
		}
		if code, _ := get("http://" + o.ProbeAddr + "/readyz"); code == http.StatusOK {
			return true, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false, errors.New("timed out")
}

func get(url string) (int, string) {
	resp, err := http.Get(url) //nolint:gosec,noctx // a test request to a local server
	if err != nil {
		return 0, ""
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// A webhook posted to a running themis-core becomes an incident, and the
// probes and metrics are served.
func TestRun_AlertBecomesIncident(t *testing.T) {
	ns := k8s.NewNamespace(t)
	_, pod := k8s.NewRollout(t, ns, "checkout")
	dbURL := pgtest.NewDatabase(t)
	// The executor is configured but not running. People sign in through an
	// identity provider, and the model is set in setup, where none is yet.
	executorAddr := freeAddr(t)
	configPath := writeFile(t, "with-executor.yaml", strings.Replace(readFile(t, writeConfig(t, ns)), "\napi:\n", fmt.Sprintf(`
api:
  oidc:
    issuerURL: https://idp.example
    clientID: themis
    clientSecretFile: %s
    redirectURL: https://themis.example/auth/callback
`, writeFile(t, "oidc-secret", "oidc-secret\n")), 1)+fmt.Sprintf("executor:\n  address: %s\n  bearerTokenFile: %s\nsettings:\n  keyFile: %s\n",
		executorAddr, writeFile(t, "executor-token", "executor-secret\n"), writeFile(t, "settings-key", "settings-key\n")))
	o, exited, cancel := start(t, func() core.Options {
		return core.Options{
			ConfigPath:  configPath,
			DatabaseURL: dbURL,
			Kubernetes:  k8s.Config,
			HTTPAddr:    freeAddr(t),
			SetupAddr:   freeAddr(t),
			ProbeAddr:   freeAddr(t),
			MetricsAddr: freeAddr(t),
			GRPCAddr:    freeAddr(t),
			Workers:     2,
			Logger:      slog.New(slog.DiscardHandler),
		}
	})
	if code, _ := get("http://" + o.ProbeAddr + "/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", code)
	}
	// Before anyone has signed in, kubectl port-forward to the setup port is
	// the way in; the API port asks who you are.
	if code, body := get("http://" + o.SetupAddr + "/v1/me"); code != http.StatusOK || !strings.Contains(body, `"setup":true`) {
		t.Errorf("/v1/me through the setup port = %d %q, want the setup admin", code, body)
	}
	if code, _ := get("http://" + o.HTTPAddr + "/v1/me"); code != http.StatusUnauthorized {
		t.Errorf("/v1/me through the API port = %d, want 401", code)
	}
	// The sign-in page offers the identity provider, and the admin can set
	// the model.
	if code, body := get("http://" + o.HTTPAddr + "/v1/sign-in"); code != http.StatusOK || !strings.Contains(body, `"oidc":true`) {
		t.Errorf("/v1/sign-in = %d %s, want the identity provider offered", code, body)
	}
	if code, _ := get("http://" + o.SetupAddr + "/v1/settings/model"); code != http.StatusOK {
		t.Errorf("/v1/settings/model = %d, want 200", code)
	}

	payload := fmt.Sprintf(`{"version":"4","status":"firing","receiver":"themis","alerts":[{
		"status":"firing","fingerprint":"fp-1",
		"labels":{"alertname":"KubePodCrashLooping","namespace":%q,"pod":%q},
		"startsAt":"2026-10-26T09:00:00Z","endsAt":"0001-01-01T00:00:00Z"}]}`, ns, pod.Name)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		"http://"+o.HTTPAddr+"/v1/intake/alertmanager", bytes.NewBufferString(payload))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+smokeToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post alert: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", resp.StatusCode)
	}
	// The status page now says alerts arrive, through which receiver, that
	// no model is set, and that the configured executor can't be reached.
	if code, body := get("http://" + o.SetupAddr + "/v1/status"); code != http.StatusOK ||
		!strings.Contains(body, `"name":"Database","state":"ok"`) || !strings.Contains(body, `through its \"themis\" receiver`) ||
		!strings.Contains(body, `"name":"Model","state":"problem","summary":"No model is set`) ||
		!strings.Contains(body, "can't reach the executor at "+executorAddr) {
		t.Errorf("/v1/status = %d %s", code, body)
	}

	db, err := pgx.Connect(t.Context(), dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = db.Close(context.Background()) }()
	eventually(t, "the incident", exited, func() bool {
		var n int
		_ = db.QueryRow(t.Context(), `SELECT count(*) FROM incidents WHERE profile = 'rollout-regression'`).Scan(&n)
		return n == 1
	})
	// The investigation is prepared and offered to investigators over gRPC.
	conn, err := grpc.NewClient(o.GRPCAddr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(token.GRPCCredentials(investigatorSecret)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	investigations := themisv1.NewInvestigationServiceClient(conn)
	var leased *themisv1.Investigation
	eventually(t, "a leasable investigation", exited, func() bool {
		resp, err := investigations.LeaseInvestigation(t.Context(), &themisv1.LeaseInvestigationRequest{WorkerId: "smoke"})
		leased = resp.GetInvestigation()
		return err == nil && leased != nil
	})
	if len(leased.GetInitialEvidence()) != 2 {
		t.Errorf("%d initial items, want 2", len(leased.GetInitialEvidence()))
	}

	// The API lists the incident to the admin, and to nobody else.
	listReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+o.HTTPAddr+"/v1/incidents", nil)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := get("http://" + o.HTTPAddr + "/v1/incidents"); code != http.StatusUnauthorized {
		t.Errorf("anonymous GET /v1/incidents = %d, want 401", code)
	}
	listReq.Header.Set("Authorization", "Bearer "+adminSecret)
	listResp, err := http.DefaultClient.Do(listReq)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(listResp.Body)
	_ = listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK || !strings.Contains(string(body), leased.GetIncidentId()) {
		t.Errorf("GET /v1/incidents = %d %s", listResp.StatusCode, body)
	}

	_, metrics := get("http://" + o.MetricsAddr + "/metrics")
	if !strings.Contains(metrics, `themis_intake_alerts_processed_total{outcome="created",reason=""}`) {
		t.Errorf("/metrics does not report the created incident")
	}

	cancel()
	select {
	case err := <-exited:
		if err != nil {
			t.Errorf("Run after shutdown = %v, want nil", err)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("themis-core did not stop")
	}
}

// incidentProfile is an IncidentProfile in ns for alerts named alert,
// allowed the evidence template named evidence.
func incidentProfile(ns, name string, priority int64, alert, evidence string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
		"priority": priority,
		"match":    map[string]any{"alertNames": []any{alert}, "targetKinds": []any{"Deployment.apps"}},
		"evidence": map[string]any{"allowed": []any{evidence}},
	}}}
	u.SetGroupVersionKind(resources.IncidentProfileKind)
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

// accepted is what Themis said in a resource's status about using it.
func accepted(t *testing.T, u *unstructured.Unstructured) (string, string) {
	t.Helper()
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(u.GroupVersionKind())
	if err := k8s.Get(t.Context(), client.ObjectKeyFromObject(u), got); err != nil {
		t.Fatal(err)
	}
	conditions, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
	if len(conditions) != 1 {
		t.Fatalf("%s: conditions %v, want Accepted alone", u.GetName(), conditions)
	}
	c, _ := conditions[0].(map[string]any)
	return fmt.Sprint(c["status"]), fmt.Sprint(c["message"])
}

// With resources configured, profiles come from IncidentProfiles: one naming
// evidence this install doesn't have is refused in its status, and the setup
// steps route only the alerts the profiles in force handle.
func TestRun_ProfilesFromResources(t *testing.T) {
	ns := k8s.NewNamespace(t)
	good := incidentProfile(ns, "crash-loops", 100, "KubePodCrashLooping", "pod_states")
	typo := incidentProfile(ns, "typo", 90, "KubeTypoAlert", "pod_state")
	k8s.Create(t, good)
	k8s.Create(t, typo)
	// The file's settings without its profiles, as the chart writes it.
	base, _, _ := strings.Cut(readFile(t, writeConfig(t, ns)), "\nprofiles:")
	base = strings.Replace(base, "intake:\n", "intake:\n  url: http://themis.example/v1/intake/alertmanager\n  tokenSecret: themis-tokens\n", 1)
	configPath := writeFile(t, "resources.yaml", base+fmt.Sprintf("\nresources:\n  namespace: %s\n", ns))
	o, _, _ := start(t, func() core.Options {
		return core.Options{
			ConfigPath:  configPath,
			DatabaseURL: pgtest.NewDatabase(t),
			Kubernetes:  k8s.Config,
			HTTPAddr:    freeAddr(t),
			ProbeAddr:   freeAddr(t),
			MetricsAddr: "0",
			GRPCAddr:    freeAddr(t),
			Workers:     1,
			Logger:      slog.New(slog.DiscardHandler),
		}
	})

	if status, message := accepted(t, typo); status != "False" || !strings.Contains(message, `unknown evidence template "pod_state"`) {
		t.Errorf("typo: Accepted %s: %q, want refused for the unknown template", status, message)
	}
	if status, message := accepted(t, good); status != "True" {
		t.Errorf("crash-loops: Accepted %s: %q, want in use", status, message)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+o.HTTPAddr+"/v1/setup/alertmanager", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+adminSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "KubePodCrashLooping") || strings.Contains(string(body), "KubeTypoAlert") ||
		!strings.Contains(string(body), "kubectl -n "+ns+" get secret themis-tokens") {
		t.Errorf("GET /v1/setup/alertmanager = %d %s, want steps for KubePodCrashLooping alone, copying the token from %s", resp.StatusCode, body, ns)
	}
}

// TestRun_WaitsForTheDatabase starts the core before its database, as a fresh
// install does, and expects it to wait instead of exiting.
func TestRun_WaitsForTheDatabase(t *testing.T) {
	ns := k8s.NewNamespace(t)
	dbURL, err := url.Parse(pgtest.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	late := freeAddr(t)
	target := dbURL.Host
	dbURL.Host = late
	go func() {
		time.Sleep(3 * time.Second)
		relay(t, late, target)
	}()
	start(t, func() core.Options {
		return core.Options{
			ConfigPath:   writeConfig(t, ns),
			DatabaseURL:  dbURL.String(),
			Kubernetes:   k8s.Config,
			HTTPAddr:     freeAddr(t),
			ProbeAddr:    freeAddr(t),
			MetricsAddr:  "0",
			GRPCAddr:     freeAddr(t),
			Workers:      1,
			DatabaseWait: 20 * time.Second,
			Logger:       slog.New(slog.DiscardHandler),
		}
	})
}

// relay forwards connections on addr to target, standing in for a database
// that only starts listening now.
func relay(t *testing.T, addr, target string) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", addr)
	if err != nil {
		t.Errorf("listen on %s: %v", addr, err)
		return
	}
	t.Cleanup(func() { _ = ln.Close() })
	for {
		in, err := ln.Accept()
		if err != nil {
			return
		}
		out, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", target)
		if err != nil {
			_ = in.Close()
			continue
		}
		go func() { _, _ = io.Copy(out, in); _ = out.Close() }()
		go func() { _, _ = io.Copy(in, out); _ = in.Close() }()
	}
}

// taken holds a port, as another process would, until the test ends.
func taken(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

// themis-core refuses to start rather than run half-configured, and says why.
func TestRun_RefusesToStart(t *testing.T) {
	ns := k8s.NewNamespace(t)
	tests := map[string]struct {
		mutate func(t *testing.T, o *core.Options)
		want   string
	}{
		"invalid config": {func(t *testing.T, o *core.Options) {
			o.ConfigPath = writeFile(t, "bad.yaml", "enrolledNamespaces: []\n")
		}, "invalid config"},
		"no database URL": {func(_ *testing.T, o *core.Options) { o.DatabaseURL = "" }, "no database URL"},
		"malformed database URL": {func(_ *testing.T, o *core.Options) {
			o.DatabaseURL = "postgres://themis@%zz/themis"
		}, "connect to database"},
		// The catalog checks every profile before anything starts.
		"profile naming an unknown template": {func(t *testing.T, o *core.Options) {
			o.ConfigPath = writeFile(t, "typo.yaml", strings.ReplaceAll(readFile(t, o.ConfigPath), "pod_states", "pod_state"))
		}, `unknown evidence template "pod_state"`},
		"setup address others can reach": {func(_ *testing.T, o *core.Options) { o.SetupAddr = "0.0.0.0:8083" }, "must be a loopback address"},
		"setup address without a port":   {func(_ *testing.T, o *core.Options) { o.SetupAddr = "localhost" }, `setup address "localhost"`},
		"unreadable executor token": {func(t *testing.T, o *core.Options) {
			o.DatabaseURL = pgtest.NewDatabase(t)
			o.ConfigPath = writeFile(t, "executor.yaml", readFile(t, o.ConfigPath)+"executor:\n  address: 127.0.0.1:1\n  bearerTokenFile: /nonexistent/executor-token\n")
		}, "executor token"},
		"unreachable database": {func(t *testing.T, o *core.Options) {
			o.DatabaseURL = "postgres://themis:themis@" + freeAddr(t) + "/themis?sslmode=disable&connect_timeout=2"
		}, "database unreachable"},
		// A port Themis can't bind stops it, rather than leaving it running
		// without its API or without investigators.
		"API port taken": {func(t *testing.T, o *core.Options) {
			o.DatabaseURL, o.HTTPAddr = pgtest.NewDatabase(t), taken(t)
		}, "listen on"},
		"investigator port taken": {func(t *testing.T, o *core.Options) {
			o.DatabaseURL, o.GRPCAddr = pgtest.NewDatabase(t), taken(t)
		}, "listen on"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			o := core.Options{
				ConfigPath:   writeConfig(t, ns),
				DatabaseURL:  "postgres://unused",
				Kubernetes:   k8s.Config,
				HTTPAddr:     freeAddr(t),
				ProbeAddr:    freeAddr(t),
				MetricsAddr:  "0",
				GRPCAddr:     freeAddr(t),
				Workers:      1,
				DatabaseWait: 3 * time.Second,
				Logger:       slog.New(slog.DiscardHandler),
			}
			tt.mutate(t, &o)

			done := make(chan error, 1)
			go func() { done <- core.Run(t.Context(), o) }()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Errorf("Run = %v, want an error saying %q", err, tt.want)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("Run did not refuse to start")
			}
		})
	}
}
