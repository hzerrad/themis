package store_test

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/store"
)

func proposal(incidentID, leaseID, clientID string) store.Proposal {
	return store.Proposal{
		IncidentID: incidentID, LeaseID: leaseID, ClientProposalID: clientID,
		Capability: "deployment.rollback-to-revision", CapabilityVersion: "1",
		Target:         newIncident("").Target,
		Params:         map[string]any{"revision": float64(1)},
		ExpectedEffect: "pods stop being OOMKilled", Rationale: "revision 2 lowered the memory limit",
		CitedEvidenceIDs: []string{"0192f0c1-0000-7000-8000-00000000000a"}, CitedPredicates: []string{"oom_killed_new_revision"},
		Provenance: map[string]string{"model": "qwen3"},
		Status:     store.ProposalAccepted,
	}
}

func insertProposal(t *testing.T, s *store.Store, p store.Proposal) (store.Proposal, bool) {
	t.Helper()
	var (
		got     store.Proposal
		created bool
	)
	err := s.InTx(t.Context(), func(tx pgx.Tx) error {
		var err error
		got, created, err = s.InsertProposal(t.Context(), tx, p)
		return err
	})
	if err != nil {
		t.Fatalf("InsertProposal: %v", err)
	}
	return got, created
}

func TestInsertProposal_StoresEveryField(t *testing.T) {
	s, _ := newStore(t)
	id := investigating(t, s)
	inv, _ := lease(t, s, t0)
	want := proposal(id, inv.LeaseID, "p-1")

	got, created := insertProposal(t, s, want)
	if !created || got.ID == "" {
		t.Fatalf("created = %v, ID = %q", created, got.ID)
	}
	read, err := s.Proposal(t.Context(), s.DB(), got.ID)
	if err != nil {
		t.Fatalf("Proposal: %v", err)
	}
	if read.Capability != want.Capability || read.Target != want.Target || read.Params["revision"] != float64(1) ||
		read.Rationale != want.Rationale || len(read.CitedEvidenceIDs) != 1 || read.CitedPredicates[0] != "oom_killed_new_revision" ||
		read.Provenance["model"] != "qwen3" || read.Status != store.ProposalAccepted {
		t.Errorf("Proposal = %+v", read)
	}
}
