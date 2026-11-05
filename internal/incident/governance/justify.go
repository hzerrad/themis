package governance

import (
	"fmt"
	"slices"
	"strings"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident/governance/policy"
)

// justified says why fix isn't justified for an incident whose checks gave
// predicates: every profile offering it must find one of the checks it names
// for fix true. ok is set when they all do.
func justified(offering config.Matched, fix capability.Capability, predicates map[string]string) (reason string, ok bool) {
	for _, p := range offering {
		names := p.JustifiedBy[fix.Name()]
		if len(names) == 0 {
			return fmt.Sprintf("The %s profile names no check that justifies %s, so Themis refuses it.", p.Name, fix.Noun()), false
		}
		if len(supporting(names, predicates)) > 0 {
			continue
		}
		answers := make([]string, 0, len(names))
		for _, name := range names {
			answers = append(answers, answer(name, predicates))
		}
		if len(names) == 1 {
			reason = fmt.Sprintf("The check that justifies %s isn't true: %s.", fix.Noun(), answers[0])
		} else {
			reason = fmt.Sprintf("None of the checks that justify %s is true: %s.", fix.Noun(), strings.Join(answers, ", "))
		}
		if len(offering) > 1 {
			reason += " They are the " + p.Name + " profile's."
		}
		return reason, false
	}
	return "", true
}

// supporting are the checks of names that are true.
func supporting(names []string, predicates map[string]string) []string {
	var out []string
	for _, name := range names {
		if check.ParseTruth(predicates[name]) == check.True {
			out = append(out, name)
		}
	}
	return out
}

// answer says what a check answered, such as "probe_kills is unknown".
func answer(name string, predicates map[string]string) string {
	value, ok := predicates[name]
	if !ok {
		return name + " didn't run"
	}
	return name + " is " + value
}

// Unproposed says which fixes the incident's profiles offer that its checks
// justify and that no policy of theirs forbids whatever the fix would change,
// for an investigation that suggested none: "Themis's checks support a
// rollback (failure_onset_after_rollout is true), but the investigator didn't
// propose it." It is empty when there is none. What a fix would change isn't
// known without one, so a rule that reads the change is passed over, and the
// cluster isn't read: GitOps may still keep Themis from applying it.
func (d *Decider) Unproposed(profiles config.Matched, fixes capability.Registry, checks []check.Result, namespace string) (string, error) {
	predicates := map[string]string{}
	for _, c := range checks {
		predicates[c.Name] = c.Value.String()
	}
	var notes []string
	for _, name := range profiles.Capabilities() {
		fix, known := fixes[name]
		if !known {
			continue
		}
		offering := profiles.Offering(name)
		if _, ok := justified(offering, fix, predicates); !ok {
			continue
		}
		forbidden := false
		var support []string
		for _, p := range offering {
			pol, err := d.policy(p)
			if err != nil {
				return "", err
			}
			// As the investigator would have suggested it.
			forbidden = forbidden || pol.Forbids(policy.Input{Capability: name, Namespace: namespace, Predicates: predicates, Source: "themis-intelligence", Authenticated: true})
			for _, c := range supporting(p.JustifiedBy[name], predicates) {
				if !slices.Contains(support, c+" is true") {
					support = append(support, c+" is true")
				}
			}
		}
		if !forbidden {
			notes = append(notes, fmt.Sprintf("Themis's checks support %s (%s), but the investigator didn't propose it.", fix.Noun(), strings.Join(support, ", ")))
		}
	}
	return strings.Join(notes, " "), nil
}
