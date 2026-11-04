// Package check decides facts about an incident from its evidence. Each
// result is true, false or unknown, and unknown is never coerced.
//
// A check is a definition, written in YAML with CEL expressions (see
// Parse), compiled once when Themis starts.
package check

import (
	"slices"
	"time"

	"github.com/hzerrad/themis/internal/evidence"
)

// Truth is a three-valued answer. The zero value is Unknown.
type Truth int

const (
	Unknown Truth = iota
	True
	False
)

func (t Truth) String() string {
	switch t {
	case True:
		return "true"
	case False:
		return "false"
	case Unknown:
		return "unknown"
	}
	return "unknown"
}

// ParseTruth is the inverse of Truth.String; anything else is Unknown.
func ParseTruth(s string) Truth {
	switch s {
	case "true":
		return True
	case "false":
		return False
	}
	return Unknown
}

func (t Truth) MarshalText() ([]byte, error) { return []byte(t.String()), nil }

func (t *Truth) UnmarshalText(b []byte) error {
	*t = ParseTruth(string(b))
	return nil
}

// Result is one check's answer and the evidence it rests on.
type Result struct {
	Name        string
	Version     string
	Value       Truth
	Reason      string
	EvidenceIDs []string

	// Gaps are the missing evidence behind an unknown result; Blind is set
	// when one of them means Themis could not see, rather than that the
	// evidence did not settle the question.
	Gaps  []Gap `json:",omitempty"`
	Blind bool  `json:",omitempty"`
}

// NameGitOpsManaged is the check governance overrides with what it reads
// live: whether GitOps owns the target.
const NameGitOpsManaged = "gitops_managed"

// List is the checks an install runs, in the order it reports them.
type List []*Check

// Evaluate runs every check in l on b, as of now.
func (l List) Evaluate(b evidence.Bundle, now time.Time) []Result {
	in := newInputs(b, now)
	out := make([]Result, 0, len(l))
	for _, c := range l {
		out = append(out, c.evaluate(in))
	}
	return out
}

// Get returns the check named name.
func (l List) Get(name string) (*Check, bool) {
	i := slices.IndexFunc(l, func(c *Check) bool { return c.Name == name })
	if i < 0 {
		return nil, false
	}
	return l[i], true
}

// For is the checks a profile runs: those it names, in l's order, or all of
// l when it names none. Names l doesn't have are left out.
func (l List) For(names []string) List {
	if len(names) == 0 {
		return l
	}
	return slices.DeleteFunc(slices.Clone(l), func(c *Check) bool { return !slices.Contains(names, c.Name) })
}
