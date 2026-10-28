package core_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/hzerrad/themis/internal/core"
	"github.com/hzerrad/themis/internal/grpcserver"
	"github.com/hzerrad/themis/internal/testutil/kubetest"
	"github.com/hzerrad/themis/internal/testutil/pgtest"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

var k8s *kubetest.Env

func TestMain(m *testing.M) {
	env, stop, err := kubetest.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "start envtest (run `make envtest` first):", err)
		os.Exit(1)
	}
	k8s = env
	code := m.Run()
	stop()
	os.Exit(code)
}

const (
	token              = "smoke-token"
	investigatorSecret = "investigator-token"
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
	tokenFile := writeFile(t, "token", token+"\n")
	investigatorToken := writeFile(t, "investigator-token", investigatorSecret+"\n")
	return writeFile(t, "themis.yaml", fmt.Sprintf(`
enrolledNamespaces: [%s]
intake:
  bearerTokenFile: %s
investigators:
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
`, ns, tokenFile, investigatorToken))
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
	o := core.Options{
		ConfigPath:  writeConfig(t, ns),
		DatabaseURL: dbURL,
		Kubernetes:  k8s.Config,
		HTTPAddr:    freeAddr(t),
		ProbeAddr:   freeAddr(t),
		MetricsAddr: freeAddr(t),
		GRPCAddr:    freeAddr(t),
		Workers:     2,
		Logger:      slog.New(slog.DiscardHandler),
	}
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan error, 1)
	go func() { exited <- core.Run(ctx, o) }()
	defer cancel()

	eventually(t, "readiness", exited, func() bool {
		code, _ := get("http://" + o.ProbeAddr + "/readyz")
		return code == http.StatusOK
	})
	if code, _ := get("http://" + o.ProbeAddr + "/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", code)
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
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post alert: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", resp.StatusCode)
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
		grpc.WithPerRPCCredentials(grpcserver.BearerToken(investigatorSecret)))
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

// themis-core refuses to start rather than run half-configured.
func TestRun_RefusesToStart(t *testing.T) {
	ns := k8s.NewNamespace(t)
	tests := map[string]func(o *core.Options){
		"invalid config":  func(o *core.Options) { o.ConfigPath = writeFile(t, "bad.yaml", "enrolledNamespaces: []\n") },
		"no database URL": func(o *core.Options) { o.DatabaseURL = "" },
		"unknown template": func(o *core.Options) {
			o.ConfigPath = writeFile(t, "typo.yaml", strings.ReplaceAll(readFile(t, o.ConfigPath), "pod_states", "pod_state"))
		},
		"unreachable database": func(o *core.Options) {
			o.DatabaseURL = "postgres://themis:themis@" + freeAddr(t) + "/themis?sslmode=disable&connect_timeout=2"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			o := core.Options{
				ConfigPath:  writeConfig(t, ns),
				DatabaseURL: "postgres://unused",
				Kubernetes:  k8s.Config,
				HTTPAddr:    freeAddr(t),
				ProbeAddr:   freeAddr(t),
				MetricsAddr: "0",
				GRPCAddr:    freeAddr(t),
				Workers:     1,
				Logger:      slog.New(slog.DiscardHandler),
			}
			mutate(&o)

			done := make(chan error, 1)
			go func() { done <- core.Run(t.Context(), o) }()
			select {
			case err := <-done:
				if err == nil {
					t.Error("Run returned nil, want an error")
				}
			case <-time.After(20 * time.Second):
				t.Fatal("Run did not refuse to start")
			}
		})
	}
}
