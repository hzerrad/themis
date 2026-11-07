package evidence

import (
	"encoding/json/v2"
	"slices"
	"strings"
	"time"

	"github.com/hzerrad/themis/internal/incident"
)

// ThemisFix is the evidence Themis keeps of its own fix that left a target as
// it is, read from its own record rather than the cluster: an incident that
// opens on a workload Themis changed follows the incident whose fix it was.
// No investigator asks for it, and an incident on a workload Themis didn't
// change has none.
const ThemisFix = "themis_fix"

// ThemisFixView is Themis's fix that the target runs, as the incident whose
// fix it was recorded it.
type ThemisFixView struct {
	// Incident is the incident whose fix it was.
	Incident string `json:"incident"`
	// Fix names the fix, such as deployment.set-resources, and Done says
	// what it did.
	Fix  string `json:"fix"`
	Done string `json:"done"`
	// AppliedAt is when Themis applied it.
	AppliedAt time.Time `json:"appliedAt"`
	// Replaced is the revision the fix replaced: the one that incident was
	// about.
	Replaced string `json:"replaced,omitempty"`
	// Outcome is how Themis's verification of the fix ended, such as
	// resolved or unchanged; empty when it hasn't.
	Outcome string `json:"outcome,omitempty"`
	// Found is what that incident's checks found of its failure, in the
	// revision the fix replaced: why Themis made the fix.
	Found []FoundView `json:"found,omitempty"`
}

// FoundView is a check that found an incident's failure.
type FoundView struct {
	Check  string `json:"check"`
	Reason string `json:"reason"`
}

// NewThemisFix is v as evidence about target, recorded at now. Themis wrote
// all of it but what the checks found, whose reasons quote evidence anyone
// who can write to the workload chose: only those can hold suspicious text.
func NewThemisFix(v ThemisFixView, target incident.TargetRef, now time.Time) (Item, error) {
	data, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		return Item{}, err
	}
	item, err := finish(Item{Data: data}, ThemisFix, target, now)
	if err != nil {
		return Item{}, err
	}
	item.Suspicious = slices.DeleteFunc(item.Suspicious, func(s Suspicion) bool { return !strings.HasPrefix(s.At, "/found/") })
	if len(item.Suspicious) == 0 {
		item.Suspicious = nil
	}
	return item, nil
}
