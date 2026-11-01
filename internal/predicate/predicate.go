// Package predicate decides facts about an incident from its evidence. Each
// result is true, false or unknown, and unknown is never coerced.
package predicate

import (
	"encoding/json/v2"
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

// Result is one predicate's answer and the evidence it rests on.
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

// RolloutWindow is how recent a rollout must be to be suspected.
const RolloutWindow = 6 * time.Hour

// Evaluate runs every predicate on b.
func Evaluate(b evidence.Bundle, now time.Time) []Result {
	return []Result{
		RolloutRecent(b, RolloutWindow, now),
		FailureOnsetAfterRollout(b),
		PriorRevisionWasHealthy(b),
		OOMKilledNewRevision(b),
		GitOpsManaged(b),
	}
}

// latest decodes the last item of template in b into v, returning its ID.
func latest(b evidence.Bundle, template string, v any) (string, bool) {
	items := b.ByTemplate(template)
	for i := len(items) - 1; i >= 0; i-- {
		if json.Unmarshal(items[i].Data, v) == nil {
			return items[i].ID, true
		}
	}
	return "", false
}

func result(name string, value Truth, reason string, ids ...string) Result {
	return Result{Name: name, Version: "1", Value: value, Reason: reason, EvidenceIDs: ids}
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
