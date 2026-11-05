package grpcapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/investigation"
	"github.com/hzerrad/themis/internal/incident/suggestion"
	"github.com/hzerrad/themis/internal/platform/telemetry"
	"github.com/hzerrad/themis/internal/platform/token"
	"github.com/hzerrad/themis/internal/server/grpcapi"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/pgtest"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

const scenario1 = "test/fixtures/scenario1/cluster.yaml"

// syncBuffer is a buffer the server's goroutines may log to.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// investigatorToken is the investigators' bearer token in tests.
const investigatorToken = "investigator-s3cret"

type env struct {
	server *grpcapi.Server
	lis    *bufconn.Listener
	logs   *syncBuffer
	store  *store.Store
	pool   *pgxpool.Pool
	target incident.TargetRef
	// now is the time the server sees; a test moves it forward.
	now time.Time

	investigations themisv1.InvestigationServiceClient
	evidence       themisv1.EvidenceServiceClient
	proposals      themisv1.ProposalServiceClient
}

func profile() config.Profile {
	return config.Profile{
		Name: "rollout-regression", Priority: 100,
		Evidence: config.Evidence{
			Initial: []string{evidence.RolloutHistory, evidence.PodStates},
			Allowed: []string{evidence.RolloutHistory, evidence.PodStates, evidence.TargetEvents, evidence.ContainerLogsTail},
		},
		Capabilities:  []string{"deployment.rollback-to-revision"},
		Guidance:      "Roll back only what the rollout broke.",
		Investigation: config.Investigation{MaxEvidenceRequests: 2},
	}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, profile())
}

// rolloutChecks are the checks the rollout-regression pack's profile runs,
// whatever other packs are there.
func rolloutChecks(t *testing.T, cat catalog.Catalog) check.List {
	t.Helper()
	p, ok := cat.Profile("rollout-regression")
	if !ok {
		t.Fatal("no rollout-regression pack")
	}
	return cat.Checks.For(p.Checks)
}

func newEnvWith(t *testing.T, p config.Profile) *env {
	t.Helper()
	s, pool := pgtest.NewStore(t)
	ns := k8s.NewNamespace(t)
	var target incident.TargetRef
	for _, obj := range k8s.Load(t, ns, scenario1) {
		if obj.GetKind() == "Deployment" {
			gvk := obj.GroupVersionKind()
			target = incident.TargetRef{Group: gvk.Group, Kind: gvk.Kind, Namespace: ns, Name: obj.GetName(), UID: string(obj.GetUID())}
		}
	}
	cfg := config.Config{EnrolledNamespaces: []string{ns}, Profiles: []config.Profile{p}}
	cat, err := catalog.Themis(nil)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{now: time.Date(2026, 10, 27, 9, 20, 0, 0, time.UTC), logs: &syncBuffer{}, store: s, pool: pool, target: target}
	now := func() time.Time { return e.now }
	gs := &grpcapi.Server{
		Store: s, Config: cfg, Evidence: cat.Evidence, Checks: cat.Checks, Fixes: cat.Fixes, RootCauses: cat.RootCauses, Now: now,
		Logger: telemetry.NewLogger(e.logs, "core", slog.LevelDebug),
	}
	gs.Suggestions = &suggestion.Inbox{Store: s, Config: cfg, Fixes: cat.Fixes, Checks: cat.Checks, Logger: gs.Logger, Now: now}
	gs.Investigations = &investigation.Leases{Store: s, Config: cfg, Evidence: cat.Evidence, Reader: k8s, Suggestions: gs.Suggestions, Logger: gs.Logger, Now: now}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(investigatorToken), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.ChainUnaryInterceptor(gs.Interceptor(tokenFile)))
	gs.Register(srv)
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn := dial(t, lis, grpc.WithPerRPCCredentials(token.GRPCCredentials(investigatorToken)))

	e.lis, e.server = lis, gs
	e.investigations = themisv1.NewInvestigationServiceClient(conn)
	e.evidence = themisv1.NewEvidenceServiceClient(conn)
	e.proposals = themisv1.NewProposalServiceClient(conn)
	prepare := &investigation.PrepareWorker{Config: cfg, Store: s, Reader: k8s, Catalog: catalog.Catalog{Evidence: cat.Evidence, Checks: rolloutChecks(t, cat)}, Now: now}
	if err := prepare.Prepare(t.Context(), e.newIncident(t)); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return e
}

func dial(t *testing.T, lis *bufconn.Listener, opts ...grpc.DialOption) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///bufnet", append(opts,
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func (e *env) newIncident(t *testing.T) string {
	t.Helper()
	var id string
	err := e.store.InTx(t.Context(), func(tx pgx.Tx) error {
		var err error
		id, _, err = e.store.CreateOrJoinIncident(t.Context(), tx, store.IncidentArgs{
			Fingerprint: "rollout-regression/" + e.target.UID, Profile: "rollout-regression", Target: e.target, Payload: []byte(`{}`),
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) lease(t *testing.T) *themisv1.Investigation {
	t.Helper()
	resp, err := e.investigations.LeaseInvestigation(t.Context(), &themisv1.LeaseInvestigationRequest{WorkerId: "w-1"})
	if err != nil {
		t.Fatalf("LeaseInvestigation: %v", err)
	}
	if resp.GetInvestigation() == nil {
		t.Fatal("nothing to lease")
	}
	return resp.GetInvestigation()
}

// logLines decodes the lines logged with msg.
func (e *env) logLines(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(e.logs.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not JSON: %q: %v", l, err)
		}
		if m[slog.MessageKey] == msg {
			out = append(out, m)
		}
	}
	return out
}

func TestGRPC_LeaseRequests_LogIncidentID(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	if _, err := e.investigations.RenewLease(t.Context(), &themisv1.RenewLeaseRequest{LeaseId: inv.GetLeaseId()}); err != nil {
		t.Fatal(err)
	}
	// A lease that expired is still named with its incident.
	e.now = e.now.Add(time.Hour)
	if _, err := e.investigations.RenewLease(t.Context(), &themisv1.RenewLeaseRequest{LeaseId: inv.GetLeaseId()}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("renewing an expired lease: %v", err)
	}

	got := e.logLines(t, "gRPC request served")
	if len(got) != 3 {
		t.Fatalf("got %d request lines, want 3", len(got))
	}
	for i, l := range got {
		if l[telemetry.FieldIncidentID] != inv.GetIncidentId() || l[telemetry.FieldLeaseID] != inv.GetLeaseId() {
			t.Errorf("line %d: %v, want incident_id and lease_id", i, l)
		}
	}
	if got[2]["code"] != codes.FailedPrecondition.String() {
		t.Errorf("code = %v, want FailedPrecondition", got[2]["code"])
	}
}

// The caller chooses the lease ID: only one that is a UUID is logged.
func TestGRPC_MalformedLeaseIDNotLogged(t *testing.T) {
	e := newEnv(t)
	_, _ = e.investigations.RenewLease(t.Context(), &themisv1.RenewLeaseRequest{LeaseId: `x","level":"ERROR`})
	for _, l := range e.logLines(t, "gRPC request served") {
		if _, ok := l[telemetry.FieldLeaseID]; ok {
			t.Errorf("lease_id logged: %v", l)
		}
	}
}

// A rejected request is logged too, without looking its lease up.
func TestGRPC_RejectedRequestIsLogged(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	anonymous := themisv1.NewInvestigationServiceClient(dial(t, e.lis))
	if _, err := anonymous.RenewLease(t.Context(), &themisv1.RenewLeaseRequest{LeaseId: inv.GetLeaseId()}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("RenewLease without a token: %v", err)
	}
	got := e.logLines(t, "gRPC request served")
	last := got[len(got)-1]
	if last["code"] != codes.Unauthenticated.String() {
		t.Errorf("code = %v, want Unauthenticated", last["code"])
	}
	if _, ok := last[telemetry.FieldLeaseID]; ok {
		t.Errorf("a rejected request was resolved to its lease: %v", last)
	}
}
