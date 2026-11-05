package governance

import (
	"context"
	"encoding/json"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/governance/policy"
	"github.com/hzerrad/themis/internal/store"
)

// ManagedCheck is the check governance overrides with what it reads live:
// whether GitOps owns the target. A scenario pack's check of this name is
// overridden.
const ManagedCheck = "gitops_managed"

// Fix is a fix to decide for an incident's target.
type Fix struct {
	Incident string
	Target   incident.TargetRef
	// Profile's policy decides.
	Profile config.Profile
	// Checks are the incident's check results, as stored.
	Checks []check.Result

	Capability capability.Capability
	Params     map[string]any

	// By is who suggested the fix; zero for a restore.
	By Suggester
	// Restores is the action this fix puts back; empty for a fix of its own.
	Restores string
}

// Suggester is who suggested a fix, as policy sees it.
type Suggester struct {
	Source        string
	Authenticated bool
	// External is set for a suggestion from outside Themis's investigator.
	External bool
}

// Decided is a fix decided: policy's decision, and the action it binds.
type Decided struct {
	Decision policy.Decision
	// Action is the change worked out against the live cluster, with its
	// digest and the decision. What happens next to it, its state and who
	// suggested it, is the caller's.
	Action store.Action
	// ManagedBy names the GitOps tool that owns the target, if any.
	ManagedBy string
}

// Decider decides fixes by their profile's policy, the same way for every
// caller. The zero value is ready to use.
type Decider struct {
	// policies holds compiled policies by their source: profiles can change
	// while Themis runs.
	policies sync.Map
}

// Decide plans fix against what r reads live, and decides it by policy. A
// fix that can't go ahead returns its Plan's error: a *capability.Refusal,
// or capability.ErrInvalidParams. So does one the cluster can't take, for a
// fix that can tell.
func (d *Decider) Decide(ctx context.Context, r client.Reader, fix Fix) (Decided, error) {
	plan, err := fix.Capability.Plan(ctx, r, fix.Target, fix.Params)
	if err != nil {
		return Decided{}, err
	}
	if f, ok := fix.Capability.(capability.Feasibility); ok {
		if err := f.Feasible(ctx, r, plan); err != nil {
			return Decided{}, err
		}
	}
	pol, err := d.policy(fix.Profile)
	if err != nil {
		return Decided{}, err
	}
	predicates := map[string]string{}
	for _, c := range fix.Checks {
		predicates[c.Name] = c.Value.String()
	}
	// Live ownership wins over what the evidence said.
	if plan.ManagedBy != "" {
		predicates[ManagedCheck] = check.True.String()
	}
	decision := pol.Evaluate(policy.Input{
		Capability: fix.Capability.Name(), Risk: plan.Risk, Namespace: fix.Target.Namespace,
		ManagedBy: plan.ManagedBy, Predicates: predicates, Change: plan.Facts,
		Source: fix.By.Source, Authenticated: fix.By.Authenticated, External: fix.By.External,
		Restore: fix.Restores != "",
	})

	fields := capability.Fields(fix.Incident, fix.Capability, plan, fix.Restores, decision.PolicyID, decision.PolicyDigest)
	dg, err := digest.Compute(fields)
	if err != nil {
		return Decided{}, err
	}
	return Decided{
		Decision: decision,
		Action: store.Action{
			IncidentID: fix.Incident,
			Capability: fields.Capability, CapabilityVersion: fields.CapabilityVersion,
			Target: fields.Target, Params: fields.Params, Constraints: fields.Constraints,
			PreState: plan.Before, Desired: plan.After,
			BlastRadius: plan.BlastRadius,
			Risk:        plan.Risk,
			Decision:    string(decision.Outcome), DecisionReason: decision.Reason,
			PolicyID: decision.PolicyID, PolicyDigest: decision.PolicyDigest,
			Digest: dg, Mark: plan.Mark,
		},
		ManagedBy: plan.ManagedBy,
	}, nil
}

// policy returns p's compiled policy.
func (d *Decider) policy(p config.Profile) (*policy.Policy, error) {
	pol := p.EffectivePolicy()
	key, err := json.Marshal(pol)
	if err != nil {
		return nil, err
	}
	if c, ok := d.policies.Load(string(key)); ok {
		return c.(*policy.Policy), nil //nolint:forcetypeassert // only *policy.Policy is stored
	}
	compiled, err := policy.Compile(pol)
	if err != nil {
		return nil, err
	}
	d.policies.Store(string(key), compiled)
	return compiled, nil
}
