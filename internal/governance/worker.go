package governance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/predicate"
	"github.com/hzerrad/themis/internal/rollback"
	"github.com/hzerrad/themis/internal/store"
)

const actor = "governance"

// Worker works govern jobs: it derives an action from live state, decides it
// by policy, and stores it with the incident's next state.
type Worker struct {
	river.WorkerDefaults[GovernArgs]

	Config   config.Config
	Store    *store.Store
	Reader   client.Reader
	Now      func() time.Time
	policies map[string]*Policy
}

// NewWorker compiles every profile's policy, so an invalid rule stops the
// core from starting.
func NewWorker(cfg config.Config, s *store.Store, r client.Reader) (*Worker, error) {
	w := &Worker{Config: cfg, Store: s, Reader: r, policies: map[string]*Policy{}}
	var errs []error
	for _, p := range cfg.Profiles {
		compiled, err := CompilePolicy(p.EffectivePolicy())
		if err != nil {
			errs = append(errs, fmt.Errorf("profile %s: %w", p.Name, err))
			continue
		}
		w.policies[p.Name] = compiled
	}
	return w, errors.Join(errs...)
}

func (w *Worker) Work(ctx context.Context, job *river.Job[GovernArgs]) error {
	return w.Govern(ctx, job.Args.ProposalID)
}

// Govern acts only on a diagnosed incident, so a repeated job does nothing.
func (w *Worker) Govern(ctx context.Context, proposalID string) error {
	p, err := w.Store.Proposal(ctx, w.Store.DB(), proposalID)
	if err != nil {
		return err
	}
	in, err := w.Store.Incident(ctx, p.IncidentID)
	if err != nil {
		return err
	}
	if in.State != incident.Diagnosed || p.Status != store.ProposalAccepted {
		return nil
	}
	profile, ok := w.Config.Profile(in.Profile)
	if !ok {
		return w.close(ctx, in.ID, fmt.Sprintf("The %s profile was removed from the configuration, so Themis stopped working on this incident.", in.Profile))
	}
	revision, ok := revisionOf(p.Params)
	if !ok || p.Capability != rollback.Name {
		return w.close(ctx, in.ID, "The suggested fix isn't one Themis can carry out.")
	}

	live, err := rollback.Read(ctx, w.Reader, in.Target, revision)
	if errors.Is(err, rollback.ErrStale) || errors.Is(err, rollback.ErrNoSuchRevision) || errors.Is(err, rollback.ErrAlreadyCurrent) {
		return w.close(ctx, in.ID, rollback.Explain(err, revision))
	}
	if err != nil {
		return err
	}

	results, err := w.Store.Predicates(ctx, in.ID)
	if err != nil {
		return err
	}
	predicates := map[string]string{}
	for _, r := range results {
		predicates[r.Name] = r.Value.String()
	}
	// Live ownership wins over what the evidence said.
	if live.ManagedBy != "" {
		predicates[predicate.NameGitOpsManaged] = predicate.True.String()
	}
	decision := w.policies[profile.Name].Evaluate(PolicyInput{
		Capability: rollback.Name, Risk: "medium", Namespace: in.Target.Namespace,
		ManagedBy: live.ManagedBy, Predicates: predicates,
	})

	a, err := w.action(in, p, live, revision, decision, profile.EffectivePolicy())
	if err != nil {
		return err
	}
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		a.ID, err = w.Store.InsertAction(ctx, tx, a)
		if errors.Is(err, store.ErrActiveAction) {
			if err := w.Store.SetProposalStatus(ctx, tx, p.ID, store.ProposalDeferred, "Another fix is already in progress on this workload."); err != nil {
				return err
			}
			return w.advance(ctx, tx, in.ID, "Another fix is already in progress on this workload.", incident.Diagnosed, incident.Recommended, incident.ClosedNoAction)
		}
		if err != nil {
			return err
		}
		store.AfterCommit(tx, policyDecisions.WithLabelValues(decision.label()).Inc)
		if decision.Outcome == Deny && live.ManagedBy != "" {
			store.AfterCommit(tx, gitOpsRefusals.Inc)
		}
		if decision.Outcome == Deny {
			return w.advance(ctx, tx, in.ID, decision.Reason, incident.Diagnosed, incident.Proposed, incident.Recommended, incident.ClosedNoAction)
		}
		return w.advance(ctx, tx, in.ID, decision.Reason, incident.Diagnosed, incident.Proposed, incident.AwaitingApproval)
	})
}

func (w *Worker) action(in store.Incident, p store.Proposal, live rollback.Live, revision int64, d Decision, pol config.Policy) (store.Action, error) {
	fields := rollback.Fields(in.ID, live, revision, d.PolicyID, d.PolicyDigest)
	dg, err := digest.Compute(fields)
	if err != nil {
		return store.Action{}, err
	}
	pre, err := json.Marshal(live.PreState)
	if err != nil {
		return store.Action{}, err
	}
	desired, err := json.Marshal(live.Desired)
	if err != nil {
		return store.Action{}, err
	}
	a := store.Action{
		IncidentID: in.ID, ProposalID: p.ID,
		Capability: fields.Capability, CapabilityVersion: fields.CapabilityVersion,
		Target: fields.Target, Params: fields.Params, Constraints: fields.Constraints,
		PreState: pre, Desired: desired,
		BlastRadius: map[string]any{"replicas": int64(live.Replicas)},
		Risk:        "medium",
		Decision:    string(d.Outcome), DecisionReason: d.Reason, PolicyID: d.PolicyID, PolicyDigest: d.PolicyDigest,
		Digest: dg, State: store.ActionDenied,
	}
	if d.Outcome != Deny {
		expires := w.now().Add(pol.ApprovalTimeout.Duration)
		a.State, a.Decision, a.ExpiresAt = store.ActionAwaitingApproval, string(RequireApproval), &expires
	}
	return a, nil
}

func (w *Worker) close(ctx context.Context, incidentID, reason string) error {
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		return w.advance(ctx, tx, incidentID, reason, incident.Diagnosed, incident.Recommended, incident.ClosedNoAction)
	})
}

func (w *Worker) advance(ctx context.Context, tx pgx.Tx, incidentID, reason string, path ...incident.State) error {
	for i := 1; i < len(path); i++ {
		err := w.Store.Transition(ctx, tx, store.TransitionArgs{ID: incidentID, From: path[i-1], To: path[i], Reason: reason, Actor: actor})
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func revisionOf(params map[string]any) (int64, bool) {
	switch n := params["revision"].(type) {
	case int64:
		return n, n > 0
	case float64:
		return int64(n), n > 0 && n == float64(int64(n))
	}
	return 0, false
}
