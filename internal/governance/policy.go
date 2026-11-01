package governance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/cel-go/cel"

	"github.com/hzerrad/themis/internal/config"
)

type Outcome string

const (
	Allow           Outcome = "allow"
	Deny            Outcome = "deny"
	RequireApproval Outcome = "require_approval"
)

// ruleVerbs say what a matching rule does to the fix.
var ruleVerbs = map[Outcome]string{
	Allow:           "allows this fix",
	Deny:            "forbids this fix",
	RequireApproval: "requires a person's approval",
}

// Decision is a policy's verdict on an action, and which rule gave it.
type Decision struct {
	Outcome      Outcome
	Reason       string
	PolicyID     string
	PolicyDigest string

	// Failed means a rule could not be evaluated, which denies.
	Failed bool
}

// PolicyInput is what rules see.
type PolicyInput struct {
	Capability string
	Risk       string
	Namespace  string
	ManagedBy  string
	Predicates map[string]string
	// Restore is set when the action puts back what Themis changed, after
	// a fix that didn't help. Rules see it as action.restore.
	Restore bool

	// Source says who suggested the action and whether that is proven: only
	// that, so a source can't argue its way past a rule.
	Source        string
	Authenticated bool
	External      bool
}

// costLimit bounds a rule's evaluation.
const costLimit = 10_000

type compiledRule struct {
	config.PolicyRule
	program cel.Program
}

// Policy is a compiled profile policy.
type Policy struct {
	mode   string
	rules  []compiledRule
	digest string
}

// CompilePolicy compiles every rule once; a rule that does not compile to a
// boolean is an error.
func CompilePolicy(p config.Policy) (*Policy, error) {
	if p.Mode == "" {
		p.Mode = config.ModeGuarded
	}
	env, err := cel.NewEnv(
		cel.Variable("action", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("target", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("predicates", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("proposal", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("mode", cel.StringType),
	)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	out := &Policy{mode: p.Mode, digest: "sha256:" + hex.EncodeToString(sum[:])}
	var errs []error
	for _, r := range p.Rules {
		ast, issues := env.Compile(r.When)
		if issues.Err() != nil {
			errs = append(errs, fmt.Errorf("rule %s: %w", r.Name, issues.Err()))
			continue
		}
		if ast.OutputType() != cel.BoolType {
			errs = append(errs, fmt.Errorf("rule %s: must be a boolean, not %s", r.Name, ast.OutputType()))
			continue
		}
		program, err := env.Program(ast, cel.CostLimit(costLimit))
		if err != nil {
			errs = append(errs, fmt.Errorf("rule %s: %w", r.Name, err))
			continue
		}
		out.rules = append(out.rules, compiledRule{PolicyRule: r, program: program})
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}

// Evaluate decides in rule order. Errors deny; observe mode denies; guarded
// mode never allows without approval.
func (p *Policy) Evaluate(in PolicyInput) Decision {
	vars := map[string]any{
		"action":     map[string]string{"capability": in.Capability, "risk": in.Risk, "restore": strconv.FormatBool(in.Restore)},
		"target":     map[string]string{"namespace": in.Namespace, "managed_by": in.ManagedBy},
		"predicates": in.Predicates,
		"proposal":   map[string]string{"source": in.Source, "authenticated": strconv.FormatBool(in.Authenticated), "external": strconv.FormatBool(in.External)},
		"mode":       p.mode,
	}
	d := Decision{Outcome: RequireApproval, Reason: "No policy rule matched, so a person must approve this fix.", PolicyDigest: p.digest}
	if in.Restore {
		// Putting back what Themis changed needs no approval unless a rule
		// asks for one: it returns the workload to how a person left it.
		d = Decision{Outcome: Allow, Reason: "No policy rule matched, so Themis may put the workload back as it was.", PolicyDigest: p.digest}
	}
	for _, r := range p.rules {
		val, _, err := r.program.Eval(vars)
		if err != nil {
			return Decision{
				Outcome: Deny, PolicyID: r.Name, PolicyDigest: p.digest, Failed: true,
				Reason: fmt.Sprintf("Policy rule %s couldn't be evaluated (%v), so the fix is forbidden.", r.Name, err),
			}
		}
		if matched, ok := val.Value().(bool); ok && matched {
			d = Decision{Outcome: Outcome(r.Decision), Reason: fmt.Sprintf("Policy rule %s %s.", r.Name, ruleVerbs[Outcome(r.Decision)]), PolicyID: r.Name, PolicyDigest: p.digest}
			break
		}
	}
	switch {
	case p.mode == config.ModeObserve && d.Outcome != Deny:
		d.Outcome, d.Reason = Deny, d.Reason+" Themis is in observe mode, so it never makes changes."
	case p.mode == config.ModeGuarded && d.Outcome == Allow && !in.Restore:
		d.Outcome, d.Reason = RequireApproval, d.Reason+" In guarded mode, a person must approve every fix."
	}
	return d
}
