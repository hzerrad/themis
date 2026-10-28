package governance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/cel-go/cel"

	"github.com/hzerrad/themis/internal/config"
)

type Outcome string

const (
	Allow           Outcome = "allow"
	Deny            Outcome = "deny"
	RequireApproval Outcome = "require_approval"
)

// Decision is a policy's verdict on an action, and which rule gave it.
type Decision struct {
	Outcome      Outcome
	Reason       string
	PolicyID     string
	PolicyDigest string
}

// PolicyInput is what rules see.
type PolicyInput struct {
	Capability string
	Risk       string
	Namespace  string
	ManagedBy  string
	Predicates map[string]string
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
		"action":     map[string]string{"capability": in.Capability, "risk": in.Risk},
		"target":     map[string]string{"namespace": in.Namespace, "managed_by": in.ManagedBy},
		"predicates": in.Predicates,
		"mode":       p.mode,
	}
	d := Decision{Outcome: RequireApproval, Reason: "no rule matched", PolicyDigest: p.digest}
	for _, r := range p.rules {
		val, _, err := r.program.Eval(vars)
		if err != nil {
			return Decision{Outcome: Deny, Reason: fmt.Sprintf("rule %s failed: %v", r.Name, err), PolicyID: r.Name, PolicyDigest: p.digest}
		}
		if matched, ok := val.Value().(bool); ok && matched {
			d = Decision{Outcome: Outcome(r.Decision), Reason: "rule " + r.Name + " matched", PolicyID: r.Name, PolicyDigest: p.digest}
			break
		}
	}
	switch {
	case p.mode == config.ModeObserve && d.Outcome != Deny:
		d.Outcome, d.Reason = Deny, d.Reason+"; observe mode never acts"
	case p.mode == config.ModeGuarded && d.Outcome == Allow:
		d.Outcome, d.Reason = RequireApproval, d.Reason+"; guarded mode requires approval"
	}
	return d
}
