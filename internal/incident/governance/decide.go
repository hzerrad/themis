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

// GitOpsRule names the decision of Themis's own rule that it never changes a
// workload Argo CD or Flux manages, or may manage: it only recommends the fix.
// Every profile's policy comes after it.
const GitOpsRule = "gitops-owns-the-target"

// UnjustifiedRule names the decision of Themis's own rule that a fix needs
// one of the checks its profile names for it to be true: evidence anyone may
// write into never justifies a fix on its own. Putting a fix back is exempt.
const UnjustifiedRule = "no-justifying-check"

// gitOpsTools are the GitOps tools Themis recognizes, as people call them.
var gitOpsTools = map[string]string{"argocd": "Argo CD", "flux": "Flux"}

// Fix is a fix to decide for an incident's target.
type Fix struct {
	Incident string
	Target   incident.TargetRef
	// Profiles are every profile the incident matched. Those offering the
	// fix govern it: each one's policy decides, mode and all, and the
	// strictest decision wins. A profile's rules govern only its own fixes.
	Profiles config.Matched
	// Checks are the incident's check results, as stored.
	Checks []check.Result

	Capability capability.Capability
	Params     map[string]any

	// By is who suggested the fix; zero for a restore.
	By Suggester
	// Restores is the action this fix puts back; empty for a fix of its own.
	Restores string
	// Undoes is the fix a restore puts back: the profiles offering it govern
	// the restore.
	Undoes string
}

// governs is the fix whose profiles govern this one.
func (f Fix) governs() string {
	if f.Undoes != "" {
		return f.Undoes
	}
	return f.Capability.Name()
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

// Decider decides fixes by their profiles' policies, the same way for every
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
	predicates := map[string]string{}
	for _, c := range fix.Checks {
		predicates[c.Name] = c.Value.String()
	}
	decision, err := d.decide(fix, policy.Input{
		Capability: fix.Capability.Name(), Risk: plan.Risk, Namespace: fix.Target.Namespace,
		ManagedBy: plan.ManagedBy, Predicates: predicates, Change: plan.Facts,
		Source: fix.By.Source, Authenticated: fix.By.Authenticated, External: fix.By.External,
		Restore: fix.Restores != "",
	})
	if err != nil {
		return Decided{}, err
	}

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

// decide is the strictest decision on in of the profiles offering fix, each
// by its whole policy, mode and all. On a tie, the higher-priority profile's
// decision stands.
//
// A fix no profile offers is forbidden. Judging refuses it before it gets
// here; the evaluation, which skips judging, learns from the first profile
// with a rule forbidding it which rule that is.
//
// Before any policy, a fix to a workload GitOps manages, or may manage, is
// forbidden, whatever it changes: Argo CD or Flux would undo it, so Themis
// only recommends it.
func (d *Decider) decide(fix Fix, in policy.Input) (policy.Decision, error) {
	offering := fix.Profiles.Offering(fix.governs())
	if len(offering) == 0 {
		for _, p := range fix.Profiles {
			pol, err := d.policy(p)
			if err != nil {
				return policy.Decision{}, err
			}
			if r, ok := pol.Rule(in); ok && r.Outcome == policy.Deny {
				return r, nil
			}
		}
		return policy.Decision{Outcome: policy.Deny, Reason: "Refused: none of the incident's profiles offers this fix."}, nil
	}
	if fix.Restores == "" {
		if reason, ok := justified(offering, fix.Capability, in.Predicates); !ok {
			return policy.Decision{Outcome: policy.Deny, Reason: reason, PolicyID: UnjustifiedRule}, nil
		}
	}
	if reason, managed := gitOps(in); managed {
		return policy.Decision{Outcome: policy.Deny, Reason: reason, PolicyID: GitOpsRule}, nil
	}
	var (
		out  policy.Decision
		from string
	)
	for _, p := range offering {
		pol, err := d.policy(p)
		if err != nil {
			return policy.Decision{}, err
		}
		if got := pol.Evaluate(in); from == "" || policy.Stricter(got.Outcome, out.Outcome) {
			out, from = got, p.Name
		}
	}
	if len(fix.Profiles) > 1 {
		out.Reason += " It is the " + from + " profile's policy."
	}
	return out, nil
}

// gitOps says why Themis only recommends a fix to in's workload, if it does:
// Argo CD or Flux manages it, as read live or by check.GitOpsManaged, or the
// check couldn't tell, or didn't run.
func gitOps(in policy.Input) (string, bool) {
	const recommend = " Apply the change in Git."
	if in.ManagedBy != "" {
		tool, ok := gitOpsTools[in.ManagedBy]
		if !ok {
			tool = in.ManagedBy
		}
		return "Recommended, not applied: " + tool + " manages this workload." + recommend, true
	}
	switch check.ParseTruth(in.Predicates[check.GitOpsManaged]) {
	case check.False:
		return "", false
	case check.True:
		return "Recommended, not applied: Argo CD or Flux manages this workload." + recommend, true
	case check.Unknown:
	}
	return "Recommended, not applied: Themis couldn't tell whether Argo CD or Flux manages this workload." + recommend, true
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
