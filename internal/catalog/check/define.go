package check

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/ext"
	"sigs.k8s.io/yaml"

	"github.com/hzerrad/themis/internal/catalog/evidence"
)

// Definition is a check as its author writes it, in YAML.
type Definition struct {
	// Name is how policy and investigators refer to the check, such as
	// rollout_recent.
	Name    string `json:"name"`
	Version string `json:"version"`
	// Description says in plain words what the check finds when it is
	// true, for investigators and people.
	Description string `json:"description"`
	// WhenFalse says what the check finds when it is false, for people. When
	// empty, people read the description followed by "no".
	WhenFalse string `json:"whenFalse,omitempty"`
	// Topic names what the check is about in a few words, for people reading
	// that it is unknown, such as "Peak memory use".
	Topic string `json:"topic,omitempty"`
	// Availability marks a check that says only whether figures other
	// checks need are there, such as a metrics source's history. People
	// read it as what the other checks rest on, never as for or against a
	// fix.
	Availability bool `json:"availability,omitempty"`
	// Reads are the evidence templates the check reads. When one an answer
	// needs is missing, the check is unknown, without the author saying so.
	Reads []string `json:"reads"`
	// Let names CEL expressions the answers share.
	Let map[string]string `json:"let,omitempty"`
	// Answers are tried in order; the first whose condition holds decides.
	Answers []AnswerDefinition `json:"answers"`
}

// AnswerDefinition is one possible answer of a check.
type AnswerDefinition struct {
	// When is a CEL condition; the last answer has none and is the
	// otherwise.
	When   string     `json:"when,omitempty"`
	Answer answerText `json:"answer"`
	// Reason says why, in plain words; ${expression} puts in a value.
	Reason string `json:"reason"`
	// Cite are CEL expressions for the IDs of the evidence the answer rests
	// on: each a string or a list of strings.
	Cite []string `json:"cite,omitempty"`
}

// answerText is true, false or unknown, written bare or quoted.
type answerText string

func (a *answerText) UnmarshalJSON(b []byte) error {
	var s string
	switch string(b) {
	case "true", "false":
		s = string(b)
	default:
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
	}
	*a = answerText(s)
	return nil
}

// Parse reads a YAML list of definitions. Unknown keys are errors.
func Parse(data []byte) ([]Definition, error) {
	j, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil, err
	}
	var defs []Definition
	if err := json.UnmarshalRead(bytes.NewReader(j), &defs, json.RejectUnknownMembers(true)); err != nil {
		return nil, err
	}
	return defs, nil
}

// costLimit bounds each expression's evaluation.
const costLimit = 1_000_000

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Check is a compiled definition.
type Check struct {
	Name        string
	Version     string
	Description string
	WhenFalse   string
	Topic       string
	// Availability is the definition's: the check says only whether
	// figures other checks need are there.
	Availability bool
	Reads        []string

	lets    map[string]*expr
	answers []answer
}

type answer struct {
	when   *expr // nil for the last
	value  Truth
	reason []part
	cite   []*expr
	// needs are the templates the answer reads, in Reads' order.
	needs []string
}

// part is a piece of a reason: text, or an expression whose value goes in.
type part struct {
	text string
	expr *expr
}

type expr struct {
	program cel.Program
	// mentions are the variables and lets it names.
	mentions []string
}

// Compile compiles defs against the evidence templates an install has. It
// reports every problem, each naming its check.
func Compile(defs []Definition, templates []string) (List, error) {
	env, err := newEnv(templates)
	if err != nil {
		return nil, err
	}
	var errs []error
	var out List
	for i, d := range defs {
		c, err := compile(env, d, templates)
		if err != nil {
			name := d.Name
			if name == "" {
				name = fmt.Sprintf("[%d]", i)
			}
			errs = append(errs, fmt.Errorf("check %s: %w", name, err))
			continue
		}
		if _, dup := out.Get(c.Name); dup {
			errs = append(errs, fmt.Errorf("check %s: defined twice", c.Name))
			continue
		}
		out = append(out, c)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}

func newEnv(templates []string) (*cel.Env, error) {
	opts := []cel.EnvOption{
		cel.Variable(varNow, cel.TimestampType),
		cel.Variable(varIDs, cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable(varCurrent, cel.DynType),
		cel.Variable(varPrior, cel.DynType),
		cel.Variable(varRolloutAt, cel.DynType),
		cel.Variable(varCurrentRevision, cel.StringType),
		cel.Variable(varFailures, cel.ListType(cel.DynType)),
		cel.Variable(varFirstFailure, cel.DynType),
		cel.Variable(varSuspicious, cel.ListType(cel.DynType)),
		cel.Variable(varThemisFix, cel.DynType),
		ext.Lists(),
		ext.Strings(),
	}
	for _, t := range templates {
		opts = append(opts, cel.Variable(t, cel.DynType))
	}
	return cel.NewEnv(opts...)
}

func compile(env *cel.Env, d Definition, templates []string) (*Check, error) {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if !namePattern.MatchString(d.Name) {
		add("name must be lowercase letters, digits and underscores, starting with a letter")
	}
	if d.Version == "" {
		add("version is required")
	}
	if d.Description == "" {
		add("description is required")
	}
	if len(d.Reads) == 0 {
		add("reads names no evidence template")
	}
	for _, t := range d.Reads {
		if !slices.Contains(templates, t) {
			add("reads unknown evidence template %q", t)
		}
	}
	if len(d.Answers) == 0 {
		add("has no answers")
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	c := &Check{Name: d.Name, Version: d.Version, Description: d.Description, WhenFalse: d.WhenFalse, Topic: d.Topic, Availability: d.Availability, Reads: d.Reads, lets: map[string]*expr{}}
	letNames := slices.Sorted(maps.Keys(d.Let))
	declared := []cel.EnvOption{}
	for _, n := range letNames {
		if !namePattern.MatchString(n) || isVariable(n, templates) {
			add("let %s: must be a new name of lowercase letters, digits and underscores", n)
			continue
		}
		declared = append(declared, cel.Variable(n, cel.DynType))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	env, err := env.Extend(declared...)
	if err != nil {
		return nil, err
	}
	for _, n := range letNames {
		e, err := compileExpr(env, d.Let[n], nil)
		if err != nil {
			add("let %s: %w", n, err)
			continue
		}
		c.lets[n] = e
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if n, ok := c.cycle(); ok {
		return nil, fmt.Errorf("let %s refers to itself", n)
	}

	for i, a := range d.Answers {
		ans, err := c.compileAnswer(env, a, i == len(d.Answers)-1, templates)
		if err != nil {
			add("answer %d: %w", i+1, err)
			continue
		}
		c.answers = append(c.answers, ans)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

func isVariable(name string, templates []string) bool {
	return slices.Contains(templates, name) || slices.Contains([]string{
		varNow, varIDs, varCurrent, varPrior, varRolloutAt, varCurrentRevision, varFailures, varFirstFailure, varSuspicious, varThemisFix,
	}, name)
}

func (c *Check) compileAnswer(env *cel.Env, a AnswerDefinition, last bool, templates []string) (answer, error) {
	var ans answer
	switch a.Answer {
	case "true":
		ans.value = True
	case "false":
		ans.value = False
	case "unknown":
		ans.value = Unknown
	default:
		return ans, fmt.Errorf("answer must be true, false or unknown, not %q", a.Answer)
	}
	if a.Reason == "" {
		return ans, errors.New("reason is required")
	}
	var all []*expr
	switch {
	case last && a.When != "":
		return ans, errors.New("the last answer is the otherwise: it has no when")
	case !last && a.When == "":
		return ans, errors.New("when is required on every answer but the last")
	case a.When != "":
		e, err := compileExpr(env, a.When, cel.BoolType)
		if err != nil {
			return ans, fmt.Errorf("when: %w", err)
		}
		ans.when = e
		all = append(all, e)
	}
	parts, err := parseReason(a.Reason)
	if err != nil {
		return ans, fmt.Errorf("reason: %w", err)
	}
	for _, p := range parts {
		if p.src == "" {
			ans.reason = append(ans.reason, part{text: p.text})
			continue
		}
		e, err := compileExpr(env, p.src, nil)
		if err != nil {
			return ans, fmt.Errorf("reason: ${%s}: %w", p.src, err)
		}
		ans.reason = append(ans.reason, part{expr: e})
		all = append(all, e)
	}
	for _, src := range a.Cite {
		e, err := compileExpr(env, src, nil)
		if err != nil {
			return ans, fmt.Errorf("cite %s: %w", src, err)
		}
		ans.cite = append(ans.cite, e)
		all = append(all, e)
	}
	var needs []string
	for _, e := range all {
		needs = append(needs, c.needs(e, templates)...)
	}
	for _, t := range needs {
		if !slices.Contains(c.Reads, t) {
			return ans, fmt.Errorf("reads evidence template %s, which reads doesn't list", t)
		}
	}
	ans.needs = slices.DeleteFunc(slices.Clone(c.Reads), func(t string) bool { return !slices.Contains(needs, t) })
	return ans, nil
}

// needs are the templates e reads, through the variables and lets it names.
func (c *Check) needs(e *expr, templates []string) []string {
	var out []string
	for _, m := range e.mentions {
		switch {
		case slices.Contains(templates, m):
			out = append(out, m)
		case strings.HasPrefix(m, varIDs+"."):
			out = append(out, strings.TrimPrefix(m, varIDs+"."))
		case slices.Contains(fromHistory, m):
			out = append(out, evidence.RolloutHistory)
		case c.lets[m] != nil:
			out = append(out, c.needs(c.lets[m], templates)...)
		}
	}
	return out
}

// cycle finds a let that refers to itself, directly or not.
func (c *Check) cycle() (string, bool) {
	var reaches func(from, to string, seen map[string]bool) bool
	reaches = func(from, to string, seen map[string]bool) bool {
		for _, m := range c.lets[from].mentions {
			if m == to {
				return true
			}
			if c.lets[m] != nil && !seen[m] {
				seen[m] = true
				if reaches(m, to, seen) {
					return true
				}
			}
		}
		return false
	}
	for _, n := range slices.Sorted(maps.Keys(c.lets)) {
		if reaches(n, n, map[string]bool{}) {
			return n, true
		}
	}
	return "", false
}

func compileExpr(env *cel.Env, src string, want *cel.Type) (*expr, error) {
	checked, iss := env.Compile(src)
	if iss.Err() != nil {
		return nil, iss.Err()
	}
	if out := checked.OutputType(); want != nil && !out.IsExactType(want) && !out.IsExactType(cel.DynType) {
		return nil, fmt.Errorf("must be a %s, not %s", want, out)
	}
	program, err := env.Program(checked, cel.CostLimit(costLimit))
	if err != nil {
		return nil, err
	}
	return &expr{program: program, mentions: mentions(checked.NativeRep().Expr())}, nil
}

// mentions are the variables and lets e names; ids.<template> counts as its
// own name.
func mentions(e ast.Expr) []string {
	var out []string
	ast.PreOrderVisit(e, ast.NewExprVisitor(func(e ast.Expr) {
		if e.Kind() == ast.IdentKind {
			out = append(out, e.AsIdent())
		}
		if e.Kind() == ast.SelectKind {
			s := e.AsSelect()
			if op := s.Operand(); op.Kind() == ast.IdentKind && op.AsIdent() == varIDs {
				out = append(out, varIDs+"."+s.FieldName())
			}
		}
	}))
	return slices.DeleteFunc(out, func(m string) bool {
		return m == varIDs // its fields are counted
	})
}

type rawPart struct{ text, src string }

// parseReason splits text with ${expression} placeholders.
func parseReason(s string) ([]rawPart, error) {
	var out []rawPart
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			if s != "" {
				out = append(out, rawPart{text: s})
			}
			return out, nil
		}
		if i > 0 {
			out = append(out, rawPart{text: s[:i]})
		}
		depth, end := 0, -1
		for j := i + 2; j < len(s) && end < 0; j++ {
			switch s[j] {
			case '{':
				depth++
			case '}':
				if depth == 0 {
					end = j
				}
				depth--
			}
		}
		if end < 0 {
			return nil, errors.New("a ${ is never closed")
		}
		src := strings.TrimSpace(s[i+2 : end])
		if src == "" {
			return nil, errors.New("${} is empty")
		}
		out = append(out, rawPart{src: src})
		s = s[end+1:]
	}
}
