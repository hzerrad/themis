package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/google/cel-go/cel"
	celast "github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/operators"

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

// Input is what rules see.
type Input struct {
	Capability string
	Risk       string
	Namespace  string
	ManagedBy  string
	Predicates map[string]string
	// Change is what the fix says about this change, beyond its name and
	// risk, such as which probes it makes more lenient. Rules see it as
	// change.<key>; each fix documents its keys.
	Change map[string]string
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
	reads   []string
}

// Policy is a compiled profile policy.
type Policy struct {
	mode   string
	rules  []compiledRule
	digest string
}

// Compile compiles every rule once; a rule that does not compile to a
// boolean is an error.
func Compile(p config.Policy) (*Policy, error) {
	if p.Mode == "" {
		p.Mode = config.ModeGuarded
	}
	env, err := cel.NewEnv(
		cel.Variable("action", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("target", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("predicates", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("change", cel.MapType(cel.StringType, cel.StringType)),
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
		out.rules = append(out.rules, compiledRule{PolicyRule: r, program: program, reads: checksRead(ast)})
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}

// Read is a check a rule reads.
type Read struct{ Rule, Check string }

// Reads lists, in rule order, the checks each rule reads by name. A rule
// reading a check that isn't run can't be evaluated, and so denies.
func (p *Policy) Reads() []Read {
	var out []Read
	for _, r := range p.rules {
		for _, c := range r.reads {
			out = append(out, Read{Rule: r.Name, Check: c})
		}
	}
	return out
}

// checksRead finds predicates.name and predicates["name"] in a rule.
func checksRead(a *cel.Ast) []string {
	var names []string
	add := func(n string) {
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	isPredicates := func(e celast.Expr) bool {
		return e.Kind() == celast.IdentKind && e.AsIdent() == "predicates"
	}
	celast.PreOrderVisit(a.NativeRep().Expr(), celast.NewExprVisitor(func(e celast.Expr) {
		if e.Kind() == celast.SelectKind {
			if sel := e.AsSelect(); isPredicates(sel.Operand()) {
				add(sel.FieldName())
			}
			return
		}
		if e.Kind() != celast.CallKind {
			return
		}
		call := e.AsCall()
		if call.FunctionName() != operators.Index || len(call.Args()) != 2 || !isPredicates(call.Args()[0]) {
			return
		}
		if key := call.Args()[1]; key.Kind() == celast.LiteralKind {
			if name, ok := key.AsLiteral().Value().(string); ok {
				add(name)
			}
		}
	}))
	return names
}

// Evaluate decides in rule order. Errors deny; observe mode denies; guarded
// mode never allows without approval.
func (p *Policy) Evaluate(in Input) Decision {
	d, matched := p.Rule(in)
	if !matched {
		d = Decision{Outcome: RequireApproval, Reason: "No policy rule matched, so a person must approve this fix.", PolicyDigest: p.digest}
		if in.Restore {
			// Putting back what Themis changed needs no approval unless a
			// rule asks for one: it returns the workload to how a person
			// left it.
			d = Decision{Outcome: Allow, Reason: "No policy rule matched, so Themis may put the workload back as it was.", PolicyDigest: p.digest}
		}
	}
	if d.Failed {
		return d
	}
	switch {
	case p.mode == config.ModeObserve && d.Outcome != Deny:
		d.Outcome, d.Reason = Deny, d.Reason+" Themis is in observe mode, so it never makes changes."
	case p.mode == config.ModeGuarded && d.Outcome == Allow && !in.Restore:
		d.Outcome, d.Reason = RequireApproval, d.Reason+" In guarded mode, a person must approve every fix."
	}
	return d
}

// Rule is the decision of the first rule that matches in, or of the first
// that can't be evaluated, which denies; false when none matches. Neither the
// mode nor what happens when no rule matches plays a part.
func (p *Policy) Rule(in Input) (Decision, bool) {
	change := in.Change
	if change == nil {
		change = map[string]string{}
	}
	vars := map[string]any{
		"action":     map[string]string{"capability": in.Capability, "risk": in.Risk, "restore": strconv.FormatBool(in.Restore)},
		"target":     map[string]string{"namespace": in.Namespace, "managed_by": in.ManagedBy},
		"predicates": in.Predicates,
		"change":     change,
		"proposal":   map[string]string{"source": in.Source, "authenticated": strconv.FormatBool(in.Authenticated), "external": strconv.FormatBool(in.External)},
		"mode":       p.mode,
	}
	for _, r := range p.rules {
		val, _, err := r.program.Eval(vars)
		if err != nil {
			return Decision{
				Outcome: Deny, PolicyID: r.Name, PolicyDigest: p.digest, Failed: true,
				Reason: fmt.Sprintf("Policy rule %s couldn't be evaluated (%v), so the fix is forbidden.", r.Name, err),
			}, true
		}
		if matched, ok := val.Value().(bool); ok && matched {
			return Decision{Outcome: Outcome(r.Decision), Reason: fmt.Sprintf("Policy rule %s %s.", r.Name, ruleVerbs[Outcome(r.Decision)]), PolicyID: r.Name, PolicyDigest: p.digest}, true
		}
	}
	return Decision{}, false
}

// Stricter reports whether a holds back more than b: deny holds back more
// than require_approval, which holds back more than allow.
func Stricter(a, b Outcome) bool {
	return strictness[a] > strictness[b]
}

var strictness = map[Outcome]int{Allow: 0, RequireApproval: 1, Deny: 2}
