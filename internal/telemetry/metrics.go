package telemetry

import "slices"

// MetricLabels are the label names Themis metrics may use: closed sets only.
// Never an incident ID, namespace, workload name, alert name or error text.
var MetricLabels = []string{
	"capability", "code", "decision", "from", "kind", "outcome", "reason",
	"result", "source", "stage", "state", "template", "to",
}

// LabelCeiling is the most distinct values a metric label may take.
const LabelCeiling = 20

// Other stands for every value outside a Bounded set.
const Other = "other"

// Bounded is the closed set of values a label may take when they come from
// configuration or code, which a closed name alone does not bound.
type Bounded []string

// Value returns v if it is in b, or Other.
func (b Bounded) Value(v string) string {
	if slices.Contains(b, v) {
		return v
	}
	return Other
}
