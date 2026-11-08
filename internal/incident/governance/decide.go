package governance

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
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
// workload Argo CD or Flux manages, or may manage: it only recommends a fix
// policy allows.
const GitOpsRule = "gitops-owns-the-target"

// ExecutorRule names the decision of Themis's own rule that it only
// recommends a fix policy allows that the executor may not apply: none is
// installed, or the install doesn't let it change the workload's namespace,
// or apply that fix.
const ExecutorRule = "executor-may-not-apply"

// Recommends says whether d only recommends its fix, by one of the rules
// above: Themis can't apply it, and a person makes the change.
func Recommends(d policy.Decision) bool {
	return d.PolicyID == GitOpsRule || d.PolicyID == ExecutorRule
}

// Reach is what the install lets Themis change, as the executor's own flags
// say: Executor is nil when no executor is installed.
type Reach struct {
	Executor *config.Executor
}

// Why says why the executor may not apply fix to a workload in namespace,
// or is empty when it may. A nil Reach lets it apply any, as the evaluation
// decides.
func (r *Reach) Why(namespace string, fix capability.Capability) string {
	switch {
	case r == nil:
		return ""
	case r.Executor == nil:
		return "The executor is off."
	case !slices.Contains(r.Executor.Namespaces, namespace):
		return "The executor may not change workloads in " + namespace + ", as executor.namespaces doesn't list it."
	case len(r.Executor.Fixes) > 0 && !slices.Contains(r.Executor.Fixes, fix.Name()):
		return "The executor may not apply " + fix.Noun() + ", as executor.fixes doesn't list " + fix.Name() + "."
	}
	return ""
}

// yourself is where a person makes the change the executor may not.
const yourself = " Apply the change yourself."

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
	// Words say what each check finds, for the reasons people read: a
	// refusal names what a check finds, never the check.
	Words check.List
	// EvidenceHasInstructions is set when some of the incident's evidence,
	// any read for the decision included, holds text that looks written for
	// Themis: policy reads it.
	EvidenceHasInstructions bool

	Capability capability.Capability
	Params     map[string]any

	// By is who suggested the fix; zero for a restore.
	By Suggester
	// Restores is the action this fix puts back; empty for a fix of its own.
	Restores string
	// Undoes is the fix a restore puts back: the profiles offering it govern
	// the restore.
	Undoes string

	// Reach is what the install lets Themis change: a fix policy allows that
	// it can't apply is only recommended. Nil decides as though it could.
	Reach *Reach
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

	// in is what policy read, and from the profile whose policy decided.
	in   policy.Input
	from string
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
	// What only the approver reads: a failure to read it leaves it out,
	// and the page says what it doesn't know.
	if f, ok := fix.Capability.(capability.Describing); ok {
		if more, err := f.Describe(ctx, r, plan); err == nil && len(more) > 0 {
			radius := maps.Clone(plan.BlastRadius)
			if radius == nil {
				radius = map[string]any{}
			}
			maps.Copy(radius, more)
			plan.BlastRadius = radius
		}
	}
	predicates := map[string]string{}
	for _, c := range fix.Checks {
		predicates[c.Name] = c.Value.String()
	}
	in := policy.Input{
		Capability: fix.Capability.Name(), Risk: plan.Risk, Namespace: fix.Target.Namespace,
		ManagedBy: plan.ManagedBy, Predicates: predicates, Change: plan.Facts,
		Source: fix.By.Source, Authenticated: fix.By.Authenticated, External: fix.By.External,
		Restore: fix.Restores != "", EvidenceHasInstructions: fix.EvidenceHasInstructions,
	}
	decision, from, err := d.decide(fix, in)
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
		in:        in, from: from,
	}, nil
}

// decide is the strictest decision on in of the profiles offering fix, each
// by its whole policy, mode and all. On a tie, the higher-priority profile's
// decision stands. Its reason names that profile, however many the incident
// matched, so every policy decision reads the same.
//
// A fix no profile offers is forbidden. Judging refuses it before it gets
// here; the evaluation, which skips judging, learns from the first profile
// with a rule forbidding it which rule that is.
//
// A fix policy allows that Themis can't apply is only recommended: to a
// workload GitOps manages, or may manage, whatever it changes, since Argo CD
// or Flux would undo it, or one the executor may not apply. A fix policy
// forbids is never recommended.
//
// It also names the profile whose policy decided, when one did.
func (d *Decider) decide(fix Fix, in policy.Input) (policy.Decision, string, error) {
	offering := fix.Profiles.Offering(fix.governs())
	if len(offering) == 0 {
		for _, p := range fix.Profiles {
			pol, err := d.policy(p)
			if err != nil {
				return policy.Decision{}, "", err
			}
			if r, ok := pol.Rule(in); ok && r.Outcome == policy.Deny {
				return r.Of(p.Name), p.Name, nil
			}
		}
		return policy.Decision{Outcome: policy.Deny, Reason: "Refused. None of the incident's profiles offers this fix."}, "", nil
	}
	if fix.Restores == "" {
		if reason, ok := justified(offering, fix.Capability, fix.Checks, fix.Words); !ok {
			return policy.Decision{Outcome: policy.Deny, Reason: reason, PolicyID: UnjustifiedRule}, "", nil
		}
	}
	var (
		out  policy.Decision
		from string
	)
	for _, p := range offering {
		pol, err := d.policy(p)
		if err != nil {
			return policy.Decision{}, "", err
		}
		if got := pol.Evaluate(in); from == "" || policy.Stricter(got.Outcome, out.Outcome) {
			out, from = got, p.Name
		}
	}
	if out.Outcome != policy.Deny {
		if reason, rule, ok := recommendation(fix, in); ok {
			return policy.Decision{Outcome: policy.Deny, Reason: reason, PolicyID: rule}, "", nil
		}
	}
	return out.Of(from), from, nil
}

// recommendation says why Themis only recommends fix, and by which of its
// own rules, when it can't apply it to the workload in reads: Argo CD or
// Flux manages it, or may, or the executor may not apply it. The reason ends
// with where a person makes the change, but for a restore, which Themis
// refers to a person with its fix in place.
func recommendation(fix Fix, in policy.Input) (reason, rule string, ok bool) {
	why, where, rule := gitOps(in), " Apply the change in Git.", GitOpsRule
	if why == "" {
		why, where, rule = fix.Reach.Why(in.Namespace, fix.Capability), yourself, ExecutorRule
	}
	switch {
	case why == "":
		return "", "", false
	case fix.Restores != "":
		return why, rule, true
	}
	return why + where, rule, true
}

// Bearing says which checks bore on a decided fix. Against are, for a fix a
// profile's policy rule refused, the checks whose answer alone made that rule
// match: another answer from one of them, and the rule wouldn't. Past are,
// for a fix no rule refused, one only recommended included, the checks the
// refusing rules of the profiles governing it read, which it got past.
func (d *Decider) Bearing(fix Fix, got Decided) (against, past []string, err error) {
	add := func(list []string, name string) []string {
		if slices.Contains(list, name) {
			return list
		}
		return append(list, name)
	}
	for _, p := range fix.Profiles.Offering(fix.governs()) {
		pol, err := d.policy(p)
		if err != nil {
			return nil, nil, err
		}
		refusing := map[string]bool{}
		for _, r := range p.EffectivePolicy().Rules {
			refusing[r.Name] = r.Decision == config.DecisionDeny
		}
		allowed := got.Decision.Outcome != policy.Deny || Recommends(got.Decision)
		for _, r := range pol.Reads() {
			switch {
			case allowed && refusing[r.Rule]:
				past = add(past, r.Check)
			case got.Decision.Outcome == policy.Deny && p.Name == got.from && r.Rule == got.Decision.PolicyID && alone(pol, got.in, r):
				against = add(against, r.Check)
			}
		}
	}
	return against, past, nil
}

// alone says whether another answer from the check r reads, all else the
// same, would keep its rule from deciding in.
func alone(pol *policy.Policy, in policy.Input, r policy.Read) bool {
	for _, answer := range []check.Truth{check.True, check.False, check.Unknown} {
		if in.Predicates[r.Check] == answer.String() {
			continue
		}
		other := in
		other.Predicates = maps.Clone(in.Predicates)
		other.Predicates[r.Check] = answer.String()
		if d, ok := pol.Rule(other); !ok || d.PolicyID != r.Rule {
			return true
		}
	}
	return false
}

// gitOps says why Themis may not change in's workload, if it may not: Argo
// CD or Flux manages it, as read live or by check.GitOpsManaged, or the check
// couldn't tell, or didn't run.
func gitOps(in policy.Input) string {
	if in.ManagedBy != "" {
		tool, ok := gitOpsTools[in.ManagedBy]
		if !ok {
			tool = in.ManagedBy
		}
		return tool + " manages this workload."
	}
	switch check.ParseTruth(in.Predicates[check.GitOpsManaged]) {
	case check.False:
		return ""
	case check.True:
		return "Argo CD or Flux manages this workload."
	case check.Unknown:
	}
	return "Themis couldn't tell whether Argo CD or Flux manages this workload."
}

// Reads are the checks p's policy rules read, in rule order, each once.
func (d *Decider) Reads(p config.Profile) ([]string, error) {
	pol, err := d.policy(p)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range pol.Reads() {
		if !slices.Contains(out, r.Check) {
			out = append(out, r.Check)
		}
	}
	return out, nil
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
