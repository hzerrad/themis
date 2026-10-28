package execution_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/execution"
	"github.com/hzerrad/themis/internal/governance"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/pgtest"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

type fakeExecutor struct {
	err   error
	calls int
}

func (f *fakeExecutor) ExecuteAction(context.Context, *themisv1.ExecuteActionRequest, ...grpc.CallOption) (*themisv1.ExecuteActionResponse, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &themisv1.ExecuteActionResponse{Status: themisv1.ExecuteActionStatus_EXECUTE_ACTION_STATUS_SUCCEEDED}, nil
}

// authorized stores an authorized action of an authorized incident.
func authorized(t *testing.T, s *store.Store) store.Action {
	t.Helper()
	ctx := t.Context()
	target := incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", UID: "uid"}
	var a store.Action
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		id, _, err := s.CreateOrJoinIncident(ctx, tx, store.IncidentArgs{Fingerprint: "fp", Profile: "p", Target: target, Payload: []byte(`{}`)})
		if err != nil {
			return err
		}
		path := []incident.State{incident.Received, incident.Investigating, incident.Diagnosed, incident.Proposed, incident.AwaitingApproval, incident.Authorized}
		for i := 1; i < len(path); i++ {
			if err := s.Transition(ctx, tx, store.TransitionArgs{ID: id, From: path[i-1], To: path[i], Reason: "test", Actor: "test"}); err != nil {
				return err
			}
		}
		p, _, err := s.InsertProposal(ctx, tx, store.Proposal{IncidentID: id, LeaseID: "0192f0c1-0000-7000-8000-000000000001", ClientProposalID: "p", Capability: "c", CapabilityVersion: "1", Target: target, Params: map[string]any{}, Status: store.ProposalAccepted})
		if err != nil {
			return err
		}
		a = store.Action{
			IncidentID: id, ProposalID: p.ID, Capability: "c", CapabilityVersion: "1", Target: digest.TargetIdentity{UID: "uid"},
			Params: map[string]any{}, BlastRadius: map[string]any{}, Constraints: map[string]any{}, Decision: "require_approval", State: store.ActionAuthorized, Digest: "sha256:d",
		}
		a.ID, err = s.InsertAction(ctx, tx, a)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestExecute_CallsTheExecutor(t *testing.T) {
	s, _ := pgtest.NewStore(t)
	a := authorized(t, s)
	x := &fakeExecutor{}
	if err := (&execution.Worker{Store: s, Executor: x}).Execute(t.Context(), a.ID); err != nil || x.calls != 1 {
		t.Errorf("Execute = %v after %d calls", err, x.calls)
	}
}

// A lost reply is retried only when the executor has no record of the
// execution.
func TestExecute_LostReply(t *testing.T) {
	s, _ := pgtest.NewStore(t)
	a := authorized(t, s)
	w := &execution.Worker{Store: s, Executor: &fakeExecutor{err: status.Error(codes.Unavailable, "connection reset")}}
	if err := w.Execute(t.Context(), a.ID); err == nil {
		t.Error("a lost reply without a record was not retried")
	}

	err := s.InTx(t.Context(), func(tx pgx.Tx) error {
		return s.RecordExecution(t.Context(), tx, store.Execution{ActionID: a.ID, Status: "succeeded", StartedAt: time.Now(), FinishedAt: time.Now()})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Execute(t.Context(), a.ID); err != nil {
		t.Errorf("a lost reply with a record: err = %v, want nil", err)
	}
}

// A claimed action is the executor's: its record decides, not the core.
func TestExecute_AlreadyClaimed(t *testing.T) {
	s, pool := pgtest.NewStore(t)
	a := authorized(t, s)
	if _, err := pool.Exec(t.Context(), `UPDATE actions SET state = 'executing' WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	w := &execution.Worker{Store: s, Executor: &fakeExecutor{err: status.Error(codes.FailedPrecondition, "not authorized")}}
	if err := w.Execute(t.Context(), a.ID); err != nil {
		t.Errorf("err = %v, want nil: retrying cannot help", err)
	}
	if got, _ := s.Action(t.Context(), s.DB(), a.ID); got.State != store.ActionExecuting {
		t.Errorf("action = %s, want executing", got.State)
	}
}

// An approval the executor cannot use leaves nothing to wait for.
func TestExecute_UnusableApproval_Cancels(t *testing.T) {
	s, _ := pgtest.NewStore(t)
	a := authorized(t, s)
	w := &execution.Worker{Store: s, Executor: &fakeExecutor{err: status.Error(codes.FailedPrecondition, "the approval is consumed or bound to another digest")}}
	if err := w.Execute(t.Context(), a.ID); err != nil {
		t.Fatal(err)
	}
	if in, _ := s.Incident(t.Context(), a.IncidentID); in.State != incident.Canceled {
		t.Errorf("incident = %s, want canceled", in.State)
	}
}

func TestExecute_UnreachableOnTheLastAttempt_Cancels(t *testing.T) {
	s, _ := pgtest.NewStore(t)
	a := authorized(t, s)
	w := &execution.Worker{Store: s, Executor: &fakeExecutor{err: status.Error(codes.Unavailable, "connection refused")}}
	job := &river.Job[governance.ExecuteArgs]{JobRow: &rivertype.JobRow{Attempt: 25, MaxAttempts: 25}, Args: governance.ExecuteArgs{ActionID: a.ID}}
	if err := w.Work(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	if in, _ := s.Incident(t.Context(), a.IncidentID); in.State != incident.Canceled {
		t.Errorf("incident = %s, want canceled", in.State)
	}
}

func TestExecute_NoExecutor_Cancels(t *testing.T) {
	s, _ := pgtest.NewStore(t)
	a := authorized(t, s)
	if err := (&execution.Worker{Store: s}).Execute(t.Context(), a.ID); err != nil {
		t.Fatal(err)
	}
	in, err := s.Incident(t.Context(), a.IncidentID)
	if err != nil || in.State != incident.Canceled {
		t.Errorf("incident = %s (%v), want canceled", in.State, err)
	}
	if got, _ := s.Action(t.Context(), s.DB(), a.ID); got.State != store.ActionCanceled {
		t.Errorf("action = %s, want canceled", got.State)
	}
}
