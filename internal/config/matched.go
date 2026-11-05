package config

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/hzerrad/themis/internal/incident"
)

// Matched is every profile an incident matched, highest priority first: the
// first is its primary profile, the one it is shown under. An incident gets
// what all of them bring, and each of their rules applies to it.
type Matched []Profile

// MatchProfiles returns every profile in profiles matching the alert, highest
// priority first; on a tie, a team's own profile comes before one for every
// namespace. Intake and the evaluation both match with it.
func MatchProfiles(profiles []Profile, alertName string, lbls map[string]string, target incident.TargetRef) Matched {
	var out Matched
	for _, p := range profiles {
		if (p.Scope == "" || p.Scope == target.Namespace) &&
			slices.Contains(p.Match.AlertNames, alertName) &&
			slices.Contains(p.Match.TargetKinds, target.KindGroup()) &&
			labels.SelectorFromSet(p.Match.Labels).Matches(labels.Set(lbls)) {
			out = append(out, p)
		}
	}
	slices.SortStableFunc(out, func(a, b Profile) int {
		if c := cmp.Compare(b.Priority, a.Priority); c != 0 {
			return c
		}
		// On a tie, the team's own profile is the more specific.
		return cmp.Compare(scoped(b), scoped(a))
	})
	return out
}

// Match returns every configured profile matching the alert, highest
// priority first; none when no profile matches.
func (c Config) Match(alertName string, lbls map[string]string, target incident.TargetRef) Matched {
	return MatchProfiles(c.Profiles, alertName, lbls, target)
}

// Matched returns the profiles named that are still configured, in the order
// named. It is empty when none is.
func (c Config) Matched(names []string) Matched {
	var out Matched
	for _, name := range names {
		if p, ok := c.Profile(name); ok {
			out = append(out, p)
		}
	}
	return out
}

// Primary is the highest-priority profile.
func (m Matched) Primary() Profile {
	if len(m) == 0 {
		return Profile{}
	}
	return m[0]
}

// Names are the profiles' names, in order.
func (m Matched) Names() []string {
	out := make([]string, 0, len(m))
	for _, p := range m {
		out = append(out, p.Name)
	}
	return out
}

// Checks are the checks any of the profiles runs, each once. None means
// every check: so does a profile that names none.
func (m Matched) Checks() []string {
	var out []string
	for _, p := range m {
		if len(p.Checks) == 0 {
			return nil
		}
		out = union(out, p.Checks)
	}
	return out
}

// Evidence is the templates any of the profiles collects first, and any may
// ask for.
func (m Matched) Evidence() Evidence {
	var out Evidence
	for _, p := range m {
		out.Initial = union(out.Initial, p.Evidence.Initial)
		out.Allowed = union(out.Allowed, p.Evidence.Allowed)
	}
	return out
}

// AllowsTemplate reports whether any of the profiles allows template.
func (m Matched) AllowsTemplate(template string) bool {
	return slices.ContainsFunc(m, func(p Profile) bool { return p.AllowsTemplate(template) })
}

// Capabilities are the fixes any of the profiles offers, each once.
func (m Matched) Capabilities() []string {
	var out []string
	for _, p := range m {
		out = union(out, p.Capabilities)
	}
	return out
}

// Offering is the profiles that offer fix: they govern it.
func (m Matched) Offering(fix string) Matched {
	var out Matched
	for _, p := range m {
		if slices.Contains(p.Capabilities, fix) {
			out = append(out, p)
		}
	}
	return out
}

// Guidance is what the investigator is told. With one profile, it is that
// profile's guidance; with several, each one's, introduced by its name and
// the fixes it offers, so the investigator knows which part is about which
// fix.
func (m Matched) Guidance() string {
	if len(m) == 1 {
		return m[0].Guidance
	}
	var parts []string
	for _, p := range m {
		if p.Guidance == "" {
			continue
		}
		offers := "which offers no fix"
		if len(p.Capabilities) > 0 {
			offers = "which offers " + strings.Join(p.Capabilities, ", ")
		}
		parts = append(parts, fmt.Sprintf("From the %s profile, %s:\n%s", p.Name, offers, strings.TrimSpace(p.Guidance)))
	}
	return strings.Join(parts, "\n\n")
}

// InvestigationBudget is the largest of the profiles' bounds, with defaults
// filled in: an incident with more to weigh gets the most room any of them
// gives.
func (m Matched) InvestigationBudget() Investigation {
	var out Investigation
	for _, p := range m {
		b := p.InvestigationBudget()
		out.MaxSteps = max(out.MaxSteps, b.MaxSteps)
		out.MaxEvidenceRequests = max(out.MaxEvidenceRequests, b.MaxEvidenceRequests)
		out.Timeout.Duration = max(out.Timeout.Duration, b.Timeout.Duration)
	}
	if len(m) == 0 {
		return Profile{}.InvestigationBudget()
	}
	return out
}

// Verification is the strictest of the profiles' verifications, with
// defaults filled in: the longest window, and the longest the target must
// stay healthy in it.
func (m Matched) Verification() Verification {
	if len(m) == 0 {
		return Profile{}.EffectiveVerification()
	}
	var out Verification
	for _, p := range m {
		v := p.EffectiveVerification()
		out.Window.Duration = max(out.Window.Duration, v.Window.Duration)
		out.Stability.Duration = max(out.Stability.Duration, v.Stability.Duration)
	}
	return out
}

// ApprovalTimeout is the shortest of the profiles' approval timeouts: an
// approval may wait no longer than the strictest of them allows.
func (m Matched) ApprovalTimeout() time.Duration {
	if len(m) == 0 {
		return DefaultApprovalTimeout
	}
	out := m[0].EffectivePolicy().ApprovalTimeout.Duration
	for _, p := range m[1:] {
		out = min(out, p.EffectivePolicy().ApprovalTimeout.Duration)
	}
	return out
}

// Named says which profiles names are, in plain words: "the a profile", "the
// a and b profiles".
func Named(names []string) string {
	switch len(names) {
	case 0:
		return "no profile"
	case 1:
		return "the " + names[0] + " profile"
	default:
		return "the " + strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1] + " profiles"
	}
}

// scoped is 1 for a team's own profile, 0 for one for every namespace.
func scoped(p Profile) int {
	if p.Scope != "" {
		return 1
	}
	return 0
}

// union is a with each of b it doesn't have, in order.
func union(a, b []string) []string {
	for _, s := range b {
		if !slices.Contains(a, s) {
			a = append(a, s)
		}
	}
	return a
}
