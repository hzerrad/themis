package check

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/interpreter"

	"github.com/hzerrad/themis/internal/evidence"
)

// evaluate decides c from in.
func (c *Check) evaluate(in inputs) Result {
	act := &activation{vars: in.vars, lets: c.lets, done: map[string]ref.Val{}}
	for _, a := range c.answers {
		for _, t := range a.needs {
			if in.missing(t) {
				return c.result(Unknown, missingReason(t), nil)
			}
		}
		if a.when != nil {
			v, err := act.eval(a.when)
			if err != nil {
				return c.failed(err)
			}
			held, ok := v.Value().(bool)
			if !ok {
				return c.failed(fmt.Errorf("when is a %s, not a bool", v.Type().TypeName()))
			}
			if !held {
				continue
			}
		}
		var reason strings.Builder
		for _, p := range a.reason {
			if p.expr == nil {
				reason.WriteString(p.text)
				continue
			}
			v, err := act.eval(p.expr)
			if err != nil {
				return c.failed(err)
			}
			reason.WriteString(format(v))
		}
		var ids []string
		for _, e := range a.cite {
			v, err := act.eval(e)
			if err != nil {
				return c.failed(err)
			}
			cited, err := strs(v)
			if err != nil {
				return c.failed(err)
			}
			for _, id := range cited {
				if !slices.Contains(ids, id) {
					ids = append(ids, id)
				}
			}
		}
		return c.result(a.value, reason.String(), ids)
	}
	panic("check " + c.Name + " has no otherwise") // Compile requires one
}

func (c *Check) result(v Truth, reason string, ids []string) Result {
	return Result{Name: c.Name, Version: c.Version, Value: v, Reason: reason, EvidenceIDs: ids}
}

// failed is the answer when an expression can't be evaluated: unknown.
func (c *Check) failed(err error) Result {
	return c.result(Unknown, "Themis couldn't evaluate this check: "+err.Error(), nil)
}

// missingReasons say what is missing, for templates whose name doesn't.
var missingReasons = map[string]string{
	evidence.ReferencedConfig: "no list of referenced configuration",
}

func missingReason(template string) string {
	if r, ok := missingReasons[template]; ok {
		return r
	}
	return "no " + strings.ReplaceAll(template, "_", " ")
}

// format writes v into a reason: times in RFC 3339, durations to the second.
func format(v ref.Val) string {
	switch v := v.(type) {
	case types.String:
		return string(v)
	case types.Timestamp:
		return v.Format(time.RFC3339)
	case types.Duration:
		return v.Duration.Round(time.Second).String()
	case types.Int:
		return strconv.FormatInt(int64(v), 10)
	default:
		return fmt.Sprint(v.Value())
	}
}

// strs is a cited ID, or a list of them.
func strs(v ref.Val) ([]string, error) {
	if s, ok := v.(types.String); ok {
		return []string{string(s)}, nil
	}
	native, err := v.ConvertToNative(reflectStrings)
	if err != nil {
		return nil, fmt.Errorf("a citation must be an evidence ID or a list of them, not %s", v.Type().TypeName())
	}
	return native.([]string), nil //nolint:forcetypeassert // converted to it
}

// activation resolves variables, and lets when first used.
type activation struct {
	vars map[string]any
	lets map[string]*expr
	done map[string]ref.Val
}

func (a *activation) ResolveName(name string) (any, bool) {
	if v, ok := a.vars[name]; ok {
		return v, true
	}
	e, ok := a.lets[name]
	if !ok {
		return nil, false
	}
	if v, ok := a.done[name]; ok {
		return v, true
	}
	v, err := a.eval(e)
	if err != nil {
		v = types.WrapErr(err)
	}
	a.done[name] = v
	return v, true
}

func (a *activation) Parent() interpreter.Activation { return nil }

func (a *activation) eval(e *expr) (ref.Val, error) {
	v, _, err := e.program.Eval(a)
	return v, err
}

var reflectStrings = reflect.TypeFor[[]string]()
