package grpcserver_test

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

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/grpcserver"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/investigation"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/telemetry"
	"github.com/hzerrad/themis/internal/testutil/pgtest"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

const scenario1 = "test/fixtures/scenario1/cluster.yaml"

// clock is a settable time source shared by the server and the test.
type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

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

// token is the investigators' bearer token in tests.
const token = "investigator-s3cret"

type env struct {
	lis    *bufconn.Listener
	logs   *syncBuffer
	store  *store.Store
	pool   *pgxpool.Pool
	clock  *clock
	target incident.TargetRef

	investigations themisv1.InvestigationServiceClient
	evidence       themisv1.EvidenceServiceClient
	proposals      themisv1.ProposalServiceClient
}

func profile() config.Profile {
	return config.Profile{
		Name: "rollout-regression", Priority: 100,
		Evidence: config.Evidence{
			Initial: []string{evidence.RolloutHistory, evidence.PodStates},
			Allowed: []string{evidence.RolloutHistory, evidence.PodStates, evidence.TargetEvents},
		},
		Capabilities:  []string{"deployment.rollback-to-revision"},
		Investigation: config.Investigation{MaxEvidenceRequests: 2},
	}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, profile())
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
	registry := evidence.NewRegistry(evidence.NewRolloutHistory(), evidence.NewPodStates(), evidence.NewTargetEvents(), evidence.NewContainerLogsTail(nil))
	clk := &clock{now: time.Date(2026, 10, 27, 9, 20, 0, 0, time.UTC)}

	logs := &syncBuffer{}
	gs := &grpcserver.Server{
		Store: s, Config: cfg, Registry: registry, Reader: k8s, Now: clk.Now, LeaseFor: time.Minute,
		Logger: telemetry.NewLogger(logs, "core", slog.LevelDebug),
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.ChainUnaryInterceptor(gs.Interceptor(tokenFile)))
	gs.Register(srv)
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn := dial(t, lis, grpc.WithPerRPCCredentials(grpcserver.BearerToken(token)))

	e := &env{
		lis: lis, logs: logs, store: s, pool: pool, clock: clk, target: target,
		investigations: themisv1.NewInvestigationServiceClient(conn),
		evidence:       themisv1.NewEvidenceServiceClient(conn),
		proposals:      themisv1.NewProposalServiceClient(conn),
	}
	prepare := &investigation.PrepareWorker{Config: cfg, Store: s, Reader: k8s, Registry: registry, Now: clk.Now}
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

func (e *env) state(t *testing.T, id string) incident.State {
	t.Helper()
	in, err := e.store.Incident(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return in.State
}

func code(err error) codes.Code { return status.Code(err) }

func TestLease_ReturnsJobWithInitialEvidenceAndPredicates(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)

	if inv.GetLeaseId() == "" || inv.GetIncidentId() == "" || inv.GetProfile() != "rollout-regression" {
		t.Errorf("investigation = %v", inv)
	}
	if inv.GetTarget().GetUid() != e.target.UID {
		t.Errorf("target = %v, want %v", inv.GetTarget(), e.target)
	}
	if len(inv.GetInitialEvidence()) != 2 || len(inv.GetPredicates()) != 5 {
		t.Errorf("%d items and %d predicates, want 2 and 5", len(inv.GetInitialEvidence()), len(inv.GetPredicates()))
	}
	if len(inv.GetAllowedTemplates()) != 3 || inv.GetAllowedTemplates()[0].GetDescription() == "" {
		t.Errorf("allowed templates = %v", inv.GetAllowedTemplates())
	}
	if len(inv.GetCandidateCapabilities()) != 1 || inv.GetBudget().GetMaxSteps() == 0 {
		t.Errorf("capabilities = %v, budget = %v", inv.GetCandidateCapabilities(), inv.GetBudget())
	}

	again, err := e.investigations.LeaseInvestigation(t.Context(), &themisv1.LeaseInvestigationRequest{WorkerId: "w-2"})
	if err != nil || again.GetInvestigation() != nil {
		t.Errorf("a second lease = %v, %v; want nothing", again, err)
	}
}

func TestComplete_AfterLeaseExpiry_Rejected(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	e.clock.now = e.clock.now.Add(2 * time.Minute)

	_, err := e.investigations.CompleteInvestigation(t.Context(), &themisv1.CompleteInvestigationRequest{
		LeaseId: inv.GetLeaseId(), Conclusion: themisv1.InvestigationConclusion_INVESTIGATION_CONCLUSION_INCONCLUSIVE,
	})
	if code(err) != codes.FailedPrecondition {
		t.Errorf("err = %v, want FailedPrecondition", err)
	}
	if got := e.state(t, inv.GetIncidentId()); got != incident.Investigating {
		t.Errorf("state = %s, want investigating", got)
	}
}

func TestRenewLease_KeepsTheLeaseAlive(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	e.clock.now = e.clock.now.Add(50 * time.Second)
	if _, err := e.investigations.RenewLease(t.Context(), &themisv1.RenewLeaseRequest{LeaseId: inv.GetLeaseId()}); err != nil {
		t.Fatalf("RenewLease: %v", err)
	}
	e.clock.now = e.clock.now.Add(50 * time.Second)
	if _, err := e.evidence.CollectEvidence(t.Context(), &themisv1.CollectEvidenceRequest{LeaseId: inv.GetLeaseId(), Template: evidence.PodStates}); err != nil {
		t.Errorf("the renewed lease: %v", err)
	}
}

func TestComplete_Inconclusive_ClosesWithoutAction(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	_, err := e.investigations.CompleteInvestigation(t.Context(), &themisv1.CompleteInvestigationRequest{
		LeaseId: inv.GetLeaseId(), Conclusion: themisv1.InvestigationConclusion_INVESTIGATION_CONCLUSION_INCONCLUSIVE,
		Summary: "logs are missing",
	})
	if err != nil {
		t.Fatalf("CompleteInvestigation: %v", err)
	}
	if got := e.state(t, inv.GetIncidentId()); got != incident.ClosedNoAction {
		t.Errorf("state = %s, want closed_no_action", got)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM transitions WHERE incident_id = $1 AND to_state = 'inconclusive'`, inv.GetIncidentId()); n != 1 {
		t.Error("the inconclusive diagnosis is not on record")
	}
}

func TestFail_LastAttemptEscalates(t *testing.T) {
	e := newEnv(t)
	var incidentID string
	for range store.MaxInvestigationAttempts {
		inv := e.lease(t)
		incidentID = inv.GetIncidentId()
		_, err := e.investigations.FailInvestigation(t.Context(), &themisv1.FailInvestigationRequest{
			LeaseId: inv.GetLeaseId(), Reason: themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_MODEL_UNAVAILABLE,
		})
		if err != nil {
			t.Fatalf("FailInvestigation: %v", err)
		}
		e.clock.now = e.clock.now.Add(10 * time.Minute)
	}
	if got := e.state(t, incidentID); got != incident.Escalated {
		t.Errorf("state = %s, want escalated", got)
	}
}

func TestFail_BacksOffBeforeTheNextLease(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	_, err := e.investigations.FailInvestigation(t.Context(), &themisv1.FailInvestigationRequest{
		LeaseId: inv.GetLeaseId(), Reason: themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_MODEL_UNAVAILABLE,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := e.investigations.LeaseInvestigation(t.Context(), &themisv1.LeaseInvestigationRequest{WorkerId: "w"}); err != nil || resp.GetInvestigation() != nil {
		t.Errorf("leased again at once: %v, %v", resp, err)
	}
	e.clock.now = e.clock.now.Add(time.Minute)
	e.lease(t)
}

func TestLease_AbandonedLastAttemptEscalates(t *testing.T) {
	e := newEnv(t)
	var incidentID string
	for range store.MaxInvestigationAttempts {
		incidentID = e.lease(t).GetIncidentId()
		e.clock.now = e.clock.now.Add(2 * time.Minute)
	}
	if _, err := e.investigations.LeaseInvestigation(t.Context(), &themisv1.LeaseInvestigationRequest{WorkerId: "w"}); err != nil {
		t.Fatal(err)
	}
	if got := e.state(t, incidentID); got != incident.Escalated {
		t.Errorf("state = %s, want escalated", got)
	}
}

func TestEvidence_TemplateNotInProfile_PermissionDenied(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	_, err := e.evidence.CollectEvidence(t.Context(), &themisv1.CollectEvidenceRequest{LeaseId: inv.GetLeaseId(), Template: evidence.ContainerLogsTail})
	if code(err) != codes.PermissionDenied {
		t.Errorf("err = %v, want PermissionDenied", err)
	}
}

func TestEvidence_RecordsProvenance(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	resp, err := e.evidence.CollectEvidence(t.Context(), &themisv1.CollectEvidenceRequest{LeaseId: inv.GetLeaseId(), Template: evidence.TargetEvents})
	if err != nil {
		t.Fatalf("CollectEvidence: %v", err)
	}
	if len(resp.GetItems()) != 1 || resp.GetRemainingRequests() != 1 {
		t.Fatalf("items = %d, remaining = %d; want 1 and 1", len(resp.GetItems()), resp.GetRemainingRequests())
	}
	id := resp.GetItems()[0].GetId()
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM evidence_items WHERE id = $1 AND lease_id = $2 AND template = 'target_events'`, id, inv.GetLeaseId()); n != 1 {
		t.Error("the item was not stored with its lease and template")
	}
}

func TestEvidence_BudgetExhausted(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	req := &themisv1.CollectEvidenceRequest{LeaseId: inv.GetLeaseId(), Template: evidence.PodStates}
	for range 2 {
		if _, err := e.evidence.CollectEvidence(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.evidence.CollectEvidence(t.Context(), req); code(err) != codes.ResourceExhausted {
		t.Errorf("err = %v, want ResourceExhausted", err)
	}
}

func TestEvidence_StaleLease(t *testing.T) {
	e := newEnv(t)
	_, err := e.evidence.CollectEvidence(t.Context(), &themisv1.CollectEvidenceRequest{LeaseId: "0192f0c1-0000-7000-8000-0000000000ff", Template: evidence.PodStates})
	if code(err) != codes.FailedPrecondition {
		t.Errorf("err = %v, want FailedPrecondition", err)
	}
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
	e.clock.now = e.clock.now.Add(time.Hour)
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

// The timeline says why the investigation gave up, in words.
func TestFail_EscalationReasonIsPlain(t *testing.T) {
	for reason := range themisv1.InvestigationFailureReason_name {
		t.Run(themisv1.InvestigationFailureReason(reason).String(), func(t *testing.T) {
			e := newEnv(t)
			var incidentID string
			for range store.MaxInvestigationAttempts {
				inv := e.lease(t)
				incidentID = inv.GetIncidentId()
				if _, err := e.investigations.FailInvestigation(t.Context(), &themisv1.FailInvestigationRequest{
					LeaseId: inv.GetLeaseId(), Reason: themisv1.InvestigationFailureReason(reason),
				}); err != nil {
					t.Fatal(err)
				}
				e.clock.now = e.clock.now.Add(10 * time.Minute)
			}
			trail, err := e.store.Transitions(t.Context(), incidentID)
			if err != nil {
				t.Fatal(err)
			}
			got := trail[len(trail)-1].Reason
			if !strings.HasPrefix(got, "The investigation failed 3 times. The last time, ") || strings.Contains(got, "REASON") || strings.Contains(got, "%!") {
				t.Errorf("reason = %q", got)
			}
		})
	}
}

// The investigation's failure is stored as a sentence, shown as it is.
func TestFail_StoredAsASentence(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	if _, err := e.investigations.FailInvestigation(t.Context(), &themisv1.FailInvestigationRequest{
		LeaseId: inv.GetLeaseId(), Reason: themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_MODEL_UNAVAILABLE, Message: "connection refused",
	}); err != nil {
		t.Fatal(err)
	}
	var failure string
	if err := e.pool.QueryRow(t.Context(), `SELECT failure FROM investigations WHERE incident_id = $1`, inv.GetIncidentId()).Scan(&failure); err != nil {
		t.Fatal(err)
	}
	if want := "The investigation failed because the model couldn't be reached: connection refused"; failure != want {
		t.Errorf("failure = %q, want %q", failure, want)
	}
}
