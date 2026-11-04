package executor_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/check"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/executor"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/governance"
	"github.com/hzerrad/themis/internal/incident/verification"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/telemetry"
	"github.com/hzerrad/themis/internal/testutil/metrictest"
	"github.com/hzerrad/themis/internal/testutil/pgtest"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

const scenario1 = "test/fixtures/scenario1/cluster.yaml"

var now = time.Date(2026, 10, 27, 9, 30, 0, 0, time.UTC)

type env struct {
	store  *store.Store
	pool   *pgxpool.Pool
	target incident.TargetRef
}

func profile() config.Profile {
	return config.Profile{
		Name: "rollout-regression", Priority: 100,
		Evidence:     config.Evidence{Allowed: []string{evidence.PodStates}},
		Capabilities: []string{"deployment.rollback-to-revision"},
	}
}

// approved runs the scenario 1 incident through governance and approval, and
// returns the authorized action.
func approved(t *testing.T) (*env, store.Action) {
	t.Helper()
	ctx := t.Context()
	s, pool := pgtest.NewStore(t)
	ns := k8s.NewNamespace(t)
	k8s.Load(t, ns, scenario1)
	d := deployment(t, ns)
	e := &env{store: s, pool: pool, target: incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: ns, Name: "checkout", UID: string(d.UID)}}

	var incidentID, proposalID string
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		incidentID, _, err = s.CreateOrJoinIncident(ctx, tx, store.IncidentArgs{Fingerprint: "fp", Profile: "rollout-regression", Target: e.target, Payload: []byte(`{}`)})
		if err != nil {
			return err
		}
		for _, step := range [][2]incident.State{{incident.Received, incident.Investigating}, {incident.Investigating, incident.Diagnosed}} {
			if err := s.Transition(ctx, tx, store.TransitionArgs{ID: incidentID, From: step[0], To: step[1], Reason: "test", Actor: "test"}); err != nil {
				return err
			}
		}
		if _, err := s.CreateInvestigation(ctx, tx, incidentID, nil, []check.Result{{Name: check.NameGitOpsManaged, Version: "1", Value: check.False}}); err != nil {
			return err
		}
		p, _, err := s.InsertProposal(ctx, tx, store.Proposal{
			IncidentID: incidentID, LeaseID: "0192f0c1-0000-7000-8000-000000000001", ClientProposalID: "p",
			Capability: "deployment.rollback-to-revision", CapabilityVersion: "1", Target: e.target,
			Params: map[string]any{"revision": float64(1)}, Status: store.ProposalAccepted,
		})
		proposalID = p.ID
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	gov := &governance.Worker{Config: config.Config{Profiles: []config.Profile{profile()}}, Store: s, Reader: k8s, Fixes: catalog.Fixes()}
	gov.Now = func() time.Time { return now }
	if err := gov.Govern(ctx, proposalID); err != nil {
		t.Fatalf("Govern: %v", err)
	}
	actions, err := s.IncidentActions(ctx, s.DB(), incidentID)
	if err != nil || len(actions) != 1 {
		t.Fatalf("actions = %v, %v", actions, err)
	}
	a := actions[0]
	err = s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.Approve(ctx, tx, a.ID, a.Digest, "admin", now); err != nil {
			return err
		}
		return s.Transition(ctx, tx, store.TransitionArgs{ID: incidentID, From: incident.AwaitingApproval, To: incident.Authorized, Reason: "test", Actor: "test"})
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	return e, a
}

func deployment(t *testing.T, ns string) *appsv1.Deployment {
	t.Helper()
	var d appsv1.Deployment
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "checkout"}, &d); err != nil {
		t.Fatal(err)
	}
	return &d
}

func (e *env) executor(c client.Client) *executor.Executor {
	x := executor.New(e.store, c, catalog.Fixes(), nil)
	x.Now = func() time.Time { return now.Add(time.Minute) }
	return x
}

func (e *env) image(t *testing.T) string {
	t.Helper()
	return deployment(t, e.target.Namespace).Spec.Template.Spec.Containers[0].Image
}

func (e *env) actionState(t *testing.T, id string) store.ActionState {
	t.Helper()
	a, err := e.store.Action(t.Context(), e.store.DB(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a.State
}

func (e *env) incidentState(t *testing.T, id string) incident.State {
	t.Helper()
	in, err := e.store.Incident(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return in.State
}

func TestExecute_RestoresPriorTemplate(t *testing.T) {
	e, a := approved(t)
	res, err := e.executor(k8s.Client).Execute(t.Context(), a.ID)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != executor.Succeeded || res.PostGeneration == 0 {
		t.Errorf("result = %+v", res)
	}
	if got := e.image(t); got != "example/checkout:2.3.1" {
		t.Errorf("image = %s, want revision 1's", got)
	}
	if got := e.actionState(t, a.ID); got != store.ActionSucceeded {
		t.Errorf("action = %s, want succeeded", got)
	}
	if got := e.incidentState(t, a.IncidentID); got != incident.Verifying {
		t.Errorf("incident = %s, want verifying", got)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM river_job WHERE kind = $1 AND args->>'actionID' = $2 AND args->>'incidentID' = $3`, verification.VerifyArgs{}.Kind(), a.ID, a.IncidentID); n != 1 {
		t.Errorf("%d verify jobs for the action and its incident, want 1", n)
	}
}

// A second delivery of the same execution writes nothing more, and reports
// the recorded outcome uncounted.
func TestExecute_Twice_WritesOnce(t *testing.T) {
	e, a := approved(t)
	succeeded := actions(t, "succeeded")
	x := e.executor(k8s.Client)
	first, err := x.Execute(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	gen := deployment(t, e.target.Namespace).Generation
	second, err := x.Execute(t.Context(), a.ID)
	if err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	if second != first || deployment(t, e.target.Namespace).Generation != gen {
		t.Errorf("second execution = %+v, generation moved from %d", second, gen)
	}
	if got := succeeded(); got != 1 {
		t.Errorf("counted %v successes, want 1", got)
	}
}

func TestExecute_DigestTampered_Refuses(t *testing.T) {
	e, a := approved(t)
	if _, err := e.pool.Exec(t.Context(), `UPDATE actions SET params = jsonb_set(params, '{revision}', '2') WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	res, err := e.executor(k8s.Client).Execute(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != executor.Stale || e.image(t) != "example/checkout:2.4.0" {
		t.Errorf("result = %+v, image = %s; want stale and untouched", res, e.image(t))
	}
	if got := e.incidentState(t, a.IncidentID); got != incident.Escalated {
		t.Errorf("incident = %s, want escalated", got)
	}
}

func TestExecute_SpecChangedSinceApproval_Stale(t *testing.T) {
	e, a := approved(t)
	d := deployment(t, e.target.Namespace)
	d.Spec.Template.Annotations = map[string]string{"edited": "by-a-human"}
	if err := k8s.Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	res, err := e.executor(k8s.Client).Execute(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != executor.Stale || e.image(t) != "example/checkout:2.4.0" {
		t.Errorf("result = %+v, image = %s", res, e.image(t))
	}
}

// resourceVersion is not bound: a status update after approval is fine.
func TestExecute_StatusOnlyChange_StillExecutes(t *testing.T) {
	e, a := approved(t)
	d := deployment(t, e.target.Namespace)
	d.Status.ReadyReplicas, d.Status.AvailableReplicas = 2, 2
	if err := k8s.Status().Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if res, err := e.executor(k8s.Client).Execute(t.Context(), a.ID); err != nil || res.Status != executor.Succeeded {
		t.Errorf("Execute = %+v, %v; want succeeded", res, err)
	}
}

// The executor refuses, in terms the core acts on, an action no one approved
// or that doesn't exist, and writes nothing.
func TestExecuteAction_RefusesUnapprovedAndUnknownActions(t *testing.T) {
	e, a := approved(t)
	if _, err := e.pool.Exec(t.Context(), `UPDATE actions SET state = 'awaiting_approval' WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	s := &executor.Server{Executor: e.executor(k8s.Client)}
	for id, want := range map[string]codes.Code{a.ID: codes.FailedPrecondition, "0192f0c1-0000-7000-8000-0000000000ff": codes.NotFound} {
		if _, err := s.ExecuteAction(t.Context(), &themisv1.ExecuteActionRequest{ActionId: id}); status.Code(err) != want {
			t.Errorf("ExecuteAction(%s) = %v, want %s", id, err, want)
		}
	}
	if got := e.image(t); got != "example/checkout:2.4.0" {
		t.Errorf("image = %s, want untouched", got)
	}
}

// failing fails dry-run patches, or real ones, with err.
func failing(t *testing.T, dryRun bool, err error) client.Client {
	t.Helper()
	c, newErr := client.NewWithWatch(k8s.Config, client.Options{})
	if newErr != nil {
		t.Fatal(newErr)
	}
	return interceptor.NewClient(c, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			po := &client.PatchOptions{}
			po.ApplyOptions(opts)
			if (len(po.DryRun) > 0) == dryRun {
				return err
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
}

func TestExecute_DryRunFailure_NoWrite(t *testing.T) {
	e, a := approved(t)
	dryRun, failed := actions(t, "dry_run_failed"), actions(t, "failed")
	res, err := e.executor(failing(t, true, errors.New("admission webhook denied the request"))).Execute(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != executor.Failed || e.image(t) != "example/checkout:2.4.0" {
		t.Errorf("result = %+v, image = %s", res, e.image(t))
	}
	if got := dryRun(); got != 1 {
		t.Errorf("counted %v dry-run failures, want 1", got)
	}
	if got := failed(); got != 0 {
		t.Errorf("counted %v other failures, want 0", got)
	}
}

// How the real write fails decides what the incident says: a timeout after
// sending leaves the write unknown and is never retried blindly, while an
// answer from the API server means nothing was written.
func TestExecute_WriteFailure(t *testing.T) {
	deployments := schema.GroupResource{Group: "apps", Resource: "deployments"}
	tests := map[string]struct {
		err    error
		status executor.Status
		state  store.ActionState
	}{
		"timeout after send": {context.DeadlineExceeded, executor.Ambiguous, store.ActionAmbiguous},
		"changed meanwhile":  {apierrors.NewConflict(deployments, "checkout", errors.New("modified")), executor.Stale, store.ActionStale},
		"rejected":           {apierrors.NewForbidden(deployments, "checkout", errors.New("no")), executor.Failed, store.ActionFailed},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			e, a := approved(t)
			res, err := e.executor(failing(t, false, tt.err)).Execute(t.Context(), a.ID)
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != tt.status {
				t.Errorf("result = %+v, want %s", res, tt.status)
			}
			if got := e.actionState(t, a.ID); got != tt.state {
				t.Errorf("action = %s, want %s", got, tt.state)
			}
		})
	}
}

func TestExecute_InterruptedClaim_Ambiguous(t *testing.T) {
	e, a := approved(t)
	err := e.store.InTx(t.Context(), func(tx pgx.Tx) error {
		locked, err := e.store.Action(t.Context(), tx, a.ID)
		if err != nil {
			return err
		}
		if err := e.store.ClaimExecution(t.Context(), tx, locked, now); err != nil {
			return err
		}
		return e.store.Transition(t.Context(), tx, store.TransitionArgs{ID: a.IncidentID, From: incident.Authorized, To: incident.Executing, Reason: "test", Actor: "test"})
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.executor(k8s.Client).Execute(t.Context(), a.ID)
	if err != nil || res.Status != executor.Ambiguous {
		t.Errorf("Execute = %+v, %v; want ambiguous", res, err)
	}
	if got := e.incidentState(t, a.IncidentID); got != incident.Escalated {
		t.Errorf("incident = %s, want escalated", got)
	}
}

// The executor logs the incident of the action it loaded, whatever the
// caller sent.
func TestExecutor_LogsCarryTheRecordsIncidentID(t *testing.T) {
	e, a := approved(t)
	if _, err := e.pool.Exec(t.Context(), `UPDATE actions SET params = jsonb_set(params, '{revision}', '2') WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	x := e.executor(k8s.Client)
	x.Logger = telemetry.NewLogger(&buf, "executor", slog.LevelInfo)
	ctx := telemetry.WithAttrs(t.Context(), slog.String(telemetry.FieldIncidentID, "01a18d24-9ab1-7c3c-b65f-1ee86ac42758"))
	if _, err := x.Execute(ctx, a.ID); err != nil {
		t.Fatal(err)
	}

	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line); err != nil {
		t.Fatalf("want one JSON line: %q", buf.String())
	}
	if line["msg"] != "action not executed" || line[telemetry.FieldIncidentID] != a.IncidentID {
		t.Errorf("got %v, want the refusal with incident_id %s", line, a.IncidentID)
	}
	if n := strings.Count(buf.String(), `"incident_id"`); n != 1 {
		t.Errorf("incident_id logged %d times", n)
	}
}

func actions(t *testing.T, result string) func() float64 {
	t.Helper()
	return metrictest.Delta(t, "themis_actions_total", map[string]string{"capability": "deployment.rollback-to-revision", "result": result})
}

// restorePlanned applies a, which doesn't help, and has governance plan
// putting the workload back. It returns the restore.
func restorePlanned(t *testing.T, e *env, a store.Action) store.Action {
	t.Helper()
	ctx := t.Context()
	if _, err := e.executor(k8s.Client).Execute(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	// The Deployment controller would number the restored template anew.
	d := deployment(t, e.target.Namespace)
	d.Annotations["deployment.kubernetes.io/revision"] = "3"
	if err := k8s.Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		return e.store.Transition(ctx, tx, store.TransitionArgs{ID: a.IncidentID, From: incident.Verifying, To: incident.Restoring, Reason: "test", Actor: "test"})
	}); err != nil {
		t.Fatal(err)
	}
	gov := &governance.Worker{Config: config.Config{Profiles: []config.Profile{profile()}}, Store: e.store, Reader: k8s, Fixes: catalog.Fixes()}
	if err := gov.Restore(ctx, a.ID); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	actions, err := e.store.IncidentActions(ctx, e.store.DB(), a.IncidentID)
	if err != nil || len(actions) != 2 {
		t.Fatalf("actions = %v, %v; want the fix and its restore", actions, err)
	}
	return actions[1]
}

// A restore runs with the same checks as a fix, without an approval to
// consume, and leaves the incident restoring until it is checked.
func TestExecute_RestorePutsTheTemplateBack(t *testing.T) {
	e, a := approved(t)
	r := restorePlanned(t, e, a)
	if r.State != store.ActionAuthorized {
		t.Fatalf("restore = %s, want authorized", r.State)
	}

	res, err := e.executor(k8s.Client).Execute(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != executor.Succeeded || e.image(t) != "example/checkout:2.4.0" {
		t.Errorf("result = %+v, image = %s; want the template from before the fix", res, e.image(t))
	}
	if got := e.incidentState(t, a.IncidentID); got != incident.Restoring {
		t.Errorf("incident = %s, want restoring until the restore is checked", got)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM river_job WHERE kind = $1 AND args->>'actionID' = $2`, verification.VerifyArgs{}.Kind(), r.ID); n != 1 {
		t.Errorf("%d checks of the restore, want 1", n)
	}
}

// A restore whose workload changed since it was planned writes nothing, and
// the incident goes to a person.
func TestExecute_StaleRestoreEscalates(t *testing.T) {
	e, a := approved(t)
	r := restorePlanned(t, e, a)
	d := deployment(t, e.target.Namespace)
	d.Spec.Template.Annotations = map[string]string{"edited": "by-a-human"}
	if err := k8s.Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}

	res, err := e.executor(k8s.Client).Execute(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != executor.Stale {
		t.Errorf("result = %+v, want stale", res)
	}
	if got := e.incidentState(t, a.IncidentID); got != incident.Escalated {
		t.Errorf("incident = %s, want escalated", got)
	}
}
