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
// results: every profile offering it must find one of the checks it names
// for fix true. ok is set when they all do. People read the reason, so it
// says what each check finds in its own words, never its name: "Refused.
// The rollout-regression profile allows a rollback only when the failures
// started after the rollout, and the checks found otherwise (…)."
func justified(offering config.Matched, fix capability.Capability, results []check.Result, words check.List) (reason string, ok bool) {
	byName := make(map[string]check.Result, len(results))
	for _, r := range results {
		byName[r.Name] = r
	}
	for _, p := range offering {
		names := p.JustifiedBy[fix.Name()]
		if len(names) == 0 {
			return fmt.Sprintf("Refused. The %s profile names no check that justifies %s.", p.Name, fix.Noun()), false
		}
		if len(supporting(names, results)) > 0 {
			continue
		}
		allows := fmt.Sprintf("Refused. The %s profile allows %s only when ", p.Name, fix.Noun())
		if len(names) > 1 {
			finds := make([]string, 0, len(names))
			for _, name := range names {
				finds = append(finds, words.Says(name, check.True))
			}
			return allows + "the checks find one of these: " + strings.Join(finds, ", or ") + ". They found none of them.", false
		}
		requirement := allows + words.Says(names[0], check.True)
		r, ran := byName[names[0]]
		if !ran {
			return requirement + ", and that check didn't run.", false
		}
		found := ", and the checks found otherwise"
		if r.Value == check.Unknown {
			found = ", and the checks couldn't tell"
		}
		if why := strings.TrimSuffix(r.Reason, "."); why != "" {
			found += " (" + why + ")"
		}
		return requirement + found + ".", false
	}
	return "", true
}

// supporting are the checks of names that are true.
func supporting(names []string, results []check.Result) []string {
	var out []string
	for _, name := range names {
		if slices.ContainsFunc(results, func(r check.Result) bool { return r.Name == name && r.Value == check.True }) {
			out = append(out, name)
		}
	}
	return out
}

// Failing are the incident's checks that find its failure: those true among
// the symptoms of its profiles. While one is, Themis never closes the
// incident with no change on its own.
func Failing(profiles config.Matched, checks []check.Result) []check.Result {
	symptoms := profiles.Symptoms()
	var out []check.Result
	for _, r := range checks {
		if r.Value == check.True && slices.Contains(symptoms, r.Name) {
			out = append(out, r)
		}
	}
	return out
}

// Referral says what a person taking over an incident with no fix applied
// should know, while its checks find its failure or support a fix: the fixes
// they support, as Unproposed says, or else what they found. It is empty
// when they do neither: nothing keeps Themis from closing the incident then.
func (d *Decider) Referral(profiles config.Matched, fixes capability.Registry, checks []check.Result, words check.List, namespace string) (string, error) {
	note, err := d.Unproposed(profiles, fixes, checks, words, namespace)
	if note != "" || err != nil {
		return note, err
	}
	return words.Findings(Failing(profiles, checks)), nil
}

// Unproposed says which fixes the incident's profiles offer that its checks
// justify and that no policy of theirs forbids whatever the fix would change,
// for an investigation that suggested none, in words people read: "The
// checks support a rollback (the failures started after the rollout)." It is
// empty when there is none. What a fix would change isn't known without one,
// so a rule that reads the change is passed over, and the cluster isn't read:
// GitOps may still keep Themis from applying it.
func (d *Decider) Unproposed(profiles config.Matched, fixes capability.Registry, checks []check.Result, words check.List, namespace string) (string, error) {
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
		if _, ok := justified(offering, fix, checks, words); !ok {
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
			for _, c := range supporting(p.JustifiedBy[name], checks) {
				if says := words.Says(c, check.True); !slices.Contains(support, says) {
					support = append(support, says)
				}
			}
		}
		if forbidden {
			continue
		}
		// Fixes the same checks support are one choice: "a rollback or a
		// partial rollback (…)".
		why := " (" + strings.Join(support, ", and ") + ")"
		if i := slices.IndexFunc(notes, func(n string) bool { return strings.HasSuffix(n, why) }); i >= 0 {
			notes[i] = strings.TrimSuffix(notes[i], why) + " or " + fix.Noun() + why
			continue
		}
		notes = append(notes, fix.Noun()+why)
	}
	switch len(notes) {
	case 0:
		return "", nil
	case 1:
		return "The checks support " + notes[0] + ".", nil
	default:
		return "The checks support " + strings.Join(notes[:len(notes)-1], ", ") + " and " + notes[len(notes)-1] + ".", nil
	}
}
