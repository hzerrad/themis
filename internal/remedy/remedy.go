// Package remedy tells operators, in plain words, what Themis could not see,
// why, and what would let it see next time. The words are written here from
// the cause and the configuration, never by the model.
package remedy

import (
	"fmt"
	"slices"
	"strings"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/store"
)

// Headlines open a stop without a conclusion: which kind of stop it was.
const (
	HeadlineBlind     = "Themis couldn't decide, because it couldn't see everything it needed:"
	HeadlineAmbiguous = "Themis couldn't decide: the evidence doesn't point to one cause."
)

// Writer writes messages for one configuration.
type Writer struct {
	Config config.Config
}

// noun names a template's evidence, singular or plural, for a sentence.
type noun struct {
	name   string
	plural bool
}

var nouns = map[string]noun{
	evidence.RolloutHistory:    {"The rollout history", false},
	evidence.PodStates:         {"The pod states", true},
	evidence.TargetEvents:      {"The events", true},
	evidence.ContainerLogsTail: {"Container logs", true},
}

func nounOf(template string) noun {
	if n, ok := nouns[template]; ok {
		return n
	}
	return noun{"Evidence " + template, false}
}

// pick returns one or many, by the noun's number.
func (n noun) pick(one, many string) string {
	if n.plural {
		return many
	}
	return one
}

// Message says what Themis could not see about in, why, and the change that
// lets it see next time, when there is one.
func (w Writer) Message(in store.Incident, g store.EvidenceGap) string {
	n := nounOf(g.Template)
	name, it := n.name, n.pick("it", "them")
	ns := in.Target.Namespace
	switch g.Cause {
	case evidence.GapForbidden:
		if slices.Contains(w.Config.EnrolledNamespaces, ns) {
			return fmt.Sprintf("%s couldn't be read: Themis isn't allowed to read %s in %s, though %s is enrolled. Check the Role Themis has in %s.", name, it, ns, ns, ns)
		}
		return fmt.Sprintf("%s couldn't be read: Themis isn't allowed to read %s in %s. To allow it, add %s to enrolledNamespaces.", name, it, ns, ns)
	case evidence.GapNotInProfile:
		return fmt.Sprintf("%s could have helped, but the %s profile doesn't allow %s. To allow %s, add %s to that profile's evidence.allowed.", name, in.Profile, it, it, g.Template)
	case evidence.GapExpired:
		return fmt.Sprintf("%s %s incomplete: Kubernetes keeps events for about an hour, and this problem started earlier, so the first ones were already gone.", name, n.pick("was", "were"))
	case evidence.GapNotFound:
		return fmt.Sprintf("%s couldn't be read: the object %s about no longer exists.", name, n.pick("it's", "they're"))
	case evidence.GapBudgetSpent:
		limit := config.DefaultMaxEvidenceRequests
		if p, ok := w.Config.Profile(in.Profile); ok {
			limit = p.InvestigationBudget().MaxEvidenceRequests
		}
		return fmt.Sprintf("%s %s read: the investigation had used all %d of its evidence requests. To allow more, raise investigation.maxEvidenceRequests in the %s profile.", name, n.pick("wasn't", "weren't"), limit, in.Profile)
	case evidence.GapDataCeiling:
		return fmt.Sprintf("%s %s shown to the model, because it isn't allowed to see data this sensitive. To allow it, set model.ceiling to sensitive, or use a self-hosted model.", name, n.pick("wasn't", "weren't"))
	case evidence.GapTimeout:
		return fmt.Sprintf("%s couldn't be read: Kubernetes took too long to answer.", name)
	case evidence.GapAbsent:
		return fmt.Sprintf("%s held nothing to read.", name)
	case evidence.GapUnavailable:
		return fmt.Sprintf("%s couldn't be read: %s.", name, strings.TrimSuffix(g.Detail, "."))
	default:
		return fmt.Sprintf("%s couldn't be read.", name)
	}
}

// Messages says what each gap kept from Themis, once each.
func (w Writer) Messages(in store.Incident, gaps []store.EvidenceGap) []string {
	var out []string
	for _, g := range gaps {
		if m := w.Message(in, g); !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	return out
}

// Headline says which kind of stop an investigation that couldn't decide was.
func Headline(blind bool) string {
	if blind {
		return HeadlineBlind
	}
	return HeadlineAmbiguous
}
