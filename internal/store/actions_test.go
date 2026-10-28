package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/store"
)

// awaitingAction stores an action awaiting approval for a new incident.
func awaitingAction(t *testing.T, s *store.Store) store.Action {
	t.Helper()
	id := investigating(t, s)
	inv, _ := lease(t, s, t0)
	p, _ := insertProposal(t, s, proposal(id, inv.LeaseID, "p-1"))
	expires := t0.Add(30 * time.Minute)
	a := store.Action{
		IncidentID: id, ProposalID: p.ID, Capability: "deployment.rollback-to-revision", CapabilityVersion: "1",
		Target: digest.TargetIdentity{
			Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout",
			UID: newIncident("").Target.UID, Generation: 2, SpecHash: "sha256:spec", Manager: "kubectl",
		},
		Params:      map[string]any{"revision": int64(1), "templateHash": "sha256:t"},
		PreState:    []byte(`{"spec":{}}`),
		Desired:     []byte(`{"spec":{}}`),
		BlastRadius: map[string]any{"replicas": int64(3)},
		Risk:        "medium", Decision: "require_approval", DecisionReason: "rule matched",
		PolicyID: "approve", PolicyDigest: "sha256:p", Constraints: map[string]any{},
		State: store.ActionAwaitingApproval, ExpiresAt: &expires,
	}
	dg, err := digest.Compute(a.Fields())
	if err != nil {
		t.Fatal(err)
	}
	a.Digest = dg
	err = s.InTx(t.Context(), func(tx pgx.Tx) error {
		a.ID, err = s.InsertAction(t.Context(), tx, a)
		return err
	})
	if err != nil {
		t.Fatalf("InsertAction: %v", err)
	}
	return a
}

func approve(t *testing.T, s *store.Store, a store.Action, dg string, now time.Time) error {
	t.Helper()
	return s.InTx(t.Context(), func(tx pgx.Tx) error {
		_, err := s.Approve(t.Context(), tx, a.ID, dg, "admin", now)
		return err
	})
}

func claim(t *testing.T, s *store.Store, id string) error {
	t.Helper()
	return s.InTx(t.Context(), func(tx pgx.Tx) error {
		a, err := s.Action(t.Context(), tx, id)
		if err != nil {
			return err
		}
		return s.ClaimExecution(t.Context(), tx, a, t0)
	})
}

// The digest of an action read back equals the one computed on write.
func TestAction_DigestSurvivesTheRoundTrip(t *testing.T) {
	s, _ := newStore(t)
	a := awaitingAction(t, s)
	read, err := s.Action(t.Context(), s.DB(), a.ID)
	if err != nil {
		t.Fatalf("Action: %v", err)
	}
	dg, err := digest.Compute(read.Fields())
	if err != nil {
		t.Fatal(err)
	}
	if dg != a.Digest || read.Digest != a.Digest {
		t.Errorf("digest after reading = %s, stored %s, computed %s", dg, read.Digest, a.Digest)
	}
}

func TestInsertAction_OneActivePerTarget(t *testing.T) {
	s, _ := newStore(t)
	a := awaitingAction(t, s)
	second := a
	err := s.InTx(t.Context(), func(tx pgx.Tx) error {
		_, err := s.InsertAction(t.Context(), tx, second)
		return err
	})
	if !errors.Is(err, store.ErrActiveAction) {
		t.Errorf("err = %v, want ErrActiveAction", err)
	}
}

func TestApprove_BindsDigest(t *testing.T) {
	s, _ := newStore(t)
	a := awaitingAction(t, s)
	if err := approve(t, s, a, "sha256:another", t0); !errors.Is(err, store.ErrDigestMismatch) {
		t.Errorf("err = %v, want ErrDigestMismatch", err)
	}
	if err := approve(t, s, a, a.Digest, t0); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := approve(t, s, a, a.Digest, t0); !errors.Is(err, store.ErrAlreadyDecided) {
		t.Errorf("second approval: err = %v, want ErrAlreadyDecided", err)
	}
}

func TestApprove_Expired(t *testing.T) {
	s, _ := newStore(t)
	a := awaitingAction(t, s)
	if err := approve(t, s, a, a.Digest, t0.Add(time.Hour)); !errors.Is(err, store.ErrExpired) {
		t.Errorf("err = %v, want ErrExpired", err)
	}
}

func TestClaimExecution_SingleUse(t *testing.T) {
	s, _ := newStore(t)
	a := awaitingAction(t, s)
	if err := claim(t, s, a.ID); !errors.Is(err, store.ErrApprovalUnusable) {
		t.Errorf("claim before approval: err = %v, want ErrApprovalUnusable", err)
	}
	if err := approve(t, s, a, a.Digest, t0); err != nil {
		t.Fatal(err)
	}
	if err := claim(t, s, a.ID); err != nil {
		t.Fatalf("ClaimExecution: %v", err)
	}
	if err := claim(t, s, a.ID); err == nil {
		t.Error("a second claim succeeded")
	}
}

// An approval bound to another digest cannot authorize what was changed
// after it.
func TestClaimExecution_DigestTamperedAfterApproval(t *testing.T) {
	s, pool := newStore(t)
	a := awaitingAction(t, s)
	if err := approve(t, s, a, a.Digest, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE actions SET digest = 'sha256:tampered' WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := claim(t, s, a.ID); !errors.Is(err, store.ErrApprovalUnusable) {
		t.Errorf("err = %v, want ErrApprovalUnusable", err)
	}
}

func TestExpireApprovals(t *testing.T) {
	s, _ := newStore(t)
	a := awaitingAction(t, s)
	var expired []store.Action
	err := s.InTx(t.Context(), func(tx pgx.Tx) error {
		var err error
		expired, err = s.ExpireApprovals(t.Context(), tx, t0.Add(time.Hour))
		return err
	})
	if err != nil || len(expired) != 1 || expired[0].ID != a.ID {
		t.Fatalf("ExpireApprovals = %v, %v", expired, err)
	}
	// The target is free for a new action.
	b := a
	if err := s.InTx(t.Context(), func(tx pgx.Tx) error { _, err := s.InsertAction(t.Context(), tx, b); return err }); err != nil {
		t.Errorf("a new action after expiry: %v", err)
	}
}
