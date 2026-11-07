package capability

import (
	"encoding/json"
	"strings"
)

// Words name one change in plain words, for the people who decide on it and
// follow it: what it is called, what it does, and what checking it looks for.
type Words struct {
	// Noun names the change, as in "Approve the raise" and "Raise approved.".
	Noun string `json:"noun"`
	// Act says what it does, as a command: "Raise search's CPU limit from
	// 200m to 300m".
	Act string `json:"act"`
	// Doing and Done say the same while it is made, and once it is:
	// "Raising search's CPU limit from 200m to 300m."
	Doing string `json:"doing"`
	Done  string `json:"done"`
	// Verifies is what checking the change looks for, to follow "Then checks
	// that": "the pods become ready and stay ready". Empty when nothing is.
	Verifies string `json:"verifies,omitempty"`
	// Note is what else the approver should know of the change's reach, in
	// a sentence, once the fix is worked out: "Rolling back undoes 3
	// changes, including the image." Empty when there is nothing to add.
	Note string `json:"note,omitempty"`
}

// Verb is a verb in the forms Words use.
type Verb struct{ Base, Doing, Done string }

// The verbs most changes take.
var (
	VerbRaise  = Verb{"Raise", "Raising", "Raised"}
	VerbLower  = Verb{"Lower", "Lowering", "Lowered"}
	VerbChange = Verb{"Change", "Changing", "Changed"}
	VerbSet    = Verb{"Set", "Setting", "Set"}
)

// What checking a rollout of new pods looks for, as ReadRollout judges it.
const (
	PodsStayReady = "the pods become ready and stay ready"
	PodsNoRestart = PodsStayReady + ", with no restart"
)

// Say is Words for a change that does v to what: Say("raise", VerbRaise,
// "search's CPU limit from 200m to 300m", PodsStayReady).
func Say(noun string, v Verb, what, verifies string) Words {
	return Words{Noun: noun, Act: v.Base + " " + what, Doing: v.Doing + " " + what + ".", Done: v.Done + " " + what + ".", Verifies: verifies}
}

// Of names what of the containers named in target's pod template a change
// is to: target's own when the template has only the one container,
// "search's CPU limit", or else the containers', "the CPU limit of search's
// app container".
func Of(target string, containers []string, only bool, what string) string {
	if only {
		return possessive(target) + " " + what
	}
	noun := " container"
	if len(containers) > 1 {
		noun = " containers"
	}
	return "the " + what + " of " + possessive(target) + " " + And(containers) + noun
}

// possessive is name's: search's, and payments' for a name that ends in s.
func possessive(name string) string {
	if strings.HasSuffix(name, "s") {
		return name + "'"
	}
	return name + "'s"
}

// And joins words in a sentence: "a", "a and b", "a, b and c".
func And(words []string) string {
	switch n := len(words); n {
	case 0:
		return ""
	case 1:
		return words[0]
	default:
		return strings.Join(words[:n-1], ", ") + " and " + words[n-1]
	}
}

// Words name the change a fix of the registry makes to target, as the fix
// words it: a restore as one, whatever the fix, and a fix Themis doesn't
// know as a change.
func (r Registry) Words(name, target string, params map[string]any, before, after json.RawMessage) Words {
	if _, ok := params["restores"]; ok {
		return Say("restore", Verb{"Restore", "Restoring", "Restored"}, "the previous state of "+target, "")
	}
	if f, ok := r[name]; ok {
		return f.Words(target, params, before, after)
	}
	return Words{Noun: "change", Act: "Apply this change to " + target, Doing: "Applying the change to " + target + ".", Done: "Changed " + target + "."}
}
