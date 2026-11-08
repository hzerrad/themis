// Package hint tells operators, in plain words, what evidence Themis was
// missing, and what would let it see that evidence next time. The words are
// written here from the cause and the configuration, never by the model, and
// never reach it.
package hint

import (
	"fmt"
	"slices"
	"strings"

	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/catalog/evidence/metrics"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/store"
)

// Headlines open a stop without a conclusion: which kind of stop it was.
const (
	HeadlineBlind     = "No fix proposed. Some evidence was missing."
	HeadlineAmbiguous = "No fix proposed. The evidence doesn't point to one cause."
)

// Writer writes messages for one configuration.
type Writer struct {
	Config config.Provider
}

func (w Writer) config() config.Config {
	if w.Config == nil {
		return config.Config{}
	}
	return w.Config.Current()
}

// names name each template's evidence as the incident page does.
var names = map[string]string{
	evidence.RolloutHistory:    "Rollout history",
	evidence.PodStates:         "Pod states",
	evidence.TargetEvents:      "Events",
	evidence.ContainerLogsTail: "Container logs",
	evidence.ReferencedConfig:  "Referenced config",
	evidence.ContainerMemory:   "Container memory",
	evidence.ContainerCPU:      "Container CPU",
	evidence.RevisionHealth:    "Revision health",
}

func nameOf(template string) string {
	if n, ok := names[template]; ok {
		return n
	}
	return "Evidence " + template
}

// Remedies for a cluster without a metrics source.
const (
	installMetrics = "Install metrics-server, or set metrics.prometheus.address."
	enableMetrics  = "Set metrics.prometheus.address, or turn the Kubernetes metrics API back on with metrics.kubernetes."
)

// missing is what one gap kept from Themis: the finding, and the change that
// lets Themis see it next time, if there is one.
type missing struct {
	finding, remedy string
}

// missed says what g kept from Themis about in.
func (w Writer) missed(in store.Incident, g store.EvidenceGap) missing {
	ns := in.Target.Namespace
	switch g.Cause {
	case evidence.GapForbidden:
		if slices.Contains(w.config().EnrolledNamespaces, ns) {
			return missing{"access denied in " + ns, "Grant read access in the Role Themis has in " + ns + "."}
		}
		return missing{"access denied in " + ns + ", which isn't enrolled", "Add " + ns + " to enrolledNamespaces."}
	case evidence.GapNotInProfile:
		if len(in.AlsoMatched) > 0 {
			return missing{
				"not allowed by the incident's profiles (" + strings.Join(in.Profiles(), ", ") + ")",
				"Add " + g.Template + " to the evidence.allowed of one of them.",
			}
		}
		return missing{"not allowed by the " + in.Profile + " profile", "Add " + g.Template + " to its evidence.allowed."}
	case evidence.GapExpired:
		return missing{"incomplete. Kubernetes keeps events for about 1 hour, and the earliest had expired", ""}
	case evidence.GapNotFound:
		return missing{"the object no longer exists", ""}
	case evidence.GapBudgetSpent:
		limit := w.config().Matched(in.Profiles()).InvestigationBudget().MaxEvidenceRequests
		where := "the " + in.Profile + " profile"
		if len(in.AlsoMatched) > 0 {
			where = "one of the incident's profiles (" + strings.Join(in.Profiles(), ", ") + ")"
		}
		return missing{
			fmt.Sprintf("not read. The investigation used all %d of its evidence requests", limit),
			"Raise investigation.maxEvidenceRequests in " + where + ".",
		}
	case evidence.GapDataCeiling:
		return missing{"not sent to the model, which may not see sensitive data", "Set investigator.model.ceiling to sensitive, or use a self-hosted model."}
	case evidence.GapTimeout:
		return missing{"Kubernetes didn't answer in time", ""}
	case evidence.GapAbsent:
		return missing{"nothing to read", ""}
	case evidence.GapUnavailable:
		return unavailable(g)
	default:
		return missing{"not read", ""}
	}
}

// unavailable says what a reading that failed for another reason kept from
// Themis. Metrics read without a source, or without history, say which.
func unavailable(g store.EvidenceGap) missing {
	detail := g.Detail
	switch {
	case strings.Contains(detail, metrics.NoSource):
		return missing{"no metrics source", enableMetrics}
	case strings.Contains(detail, metrics.ErrNotServed.Error()):
		return missing{"no metrics source", installMetrics}
	case strings.Contains(detail, metrics.NoHistory), strings.Contains(detail, metrics.NoRevisionHistory):
		return missing{"no history in the Kubernetes metrics API", "Set metrics.prometheus.address."}
	}
	// Themis's own wrapping goes; what the reader said, which the
	// investigator may have reported, stays as it was said.
	detail = strings.TrimPrefix(detail, "collect "+g.Template+": ")
	detail = strings.TrimPrefix(detail, evidence.ErrNoMetrics.Error()+": ")
	if strings.TrimSpace(detail) == "" {
		return missing{"not read", ""}
	}
	return missing{"not read (" + strings.TrimSuffix(strings.TrimSpace(detail), ".") + ")", ""}
}

// Message says in one line what g kept from Themis about in, and the change
// that lets it see next time: "Events: access denied in shop. Grant read
// access in the Role Themis has in shop."
func (w Writer) Message(in store.Incident, g store.EvidenceGap) string {
	return line([]string{nameOf(g.Template)}, w.missed(in, g), true)
}

// Messages say what the gaps kept from Themis, one line per finding: evidence
// missing for the same finding and remedy shares a line, and a remedy is said
// once.
func (w Writer) Messages(in store.Incident, gaps []store.EvidenceGap) []string {
	var (
		order []missing
		by    = map[missing][]string{}
	)
	for _, g := range gaps {
		m := w.missed(in, g)
		if _, ok := by[m]; !ok {
			order = append(order, m)
		}
		if name := nameOf(g.Template); !slices.Contains(by[m], name) {
			by[m] = append(by[m], name)
		}
	}
	out := make([]string, 0, len(order))
	var said []string
	for _, m := range order {
		out = append(out, line(by[m], m, !slices.Contains(said, m.remedy)))
		said = append(said, m.remedy)
	}
	return out
}

// line writes what is missing of the evidence named, then the remedy, when
// it is to be said: "Container memory and container CPU: no metrics source.
// Install metrics-server, or set metrics.prometheus.address."
func line(named []string, m missing, remedy bool) string {
	what := named[0]
	for i, n := range named[1:] {
		sep := ", "
		if i == len(named)-2 {
			sep = " and "
		}
		what += sep + strings.ToLower(n[:1]) + n[1:]
	}
	out := what + ": " + m.finding
	if !strings.HasSuffix(out, ".") {
		out += "."
	}
	if remedy && m.remedy != "" {
		out += " " + m.remedy
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
