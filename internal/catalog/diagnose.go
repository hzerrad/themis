package catalog

import (
	"context"
	"fmt"
	"slices"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/workload"
)

// DefaultEventRetention is the API server's default --event-ttl.
const DefaultEventRetention = time.Hour

// Moment is when a workload is diagnosed, and what tells whether its first
// events are still there.
type Moment struct {
	Now time.Time
	// AlertStarted is when the incident's first alert started firing; zero
	// when unknown.
	AlertStarted time.Time
	// EventRetention is how long the API server keeps events: older ones are
	// gone. DefaultEventRetention when zero.
	EventRetention time.Duration
}

// Gap is evidence a diagnosis needed and couldn't have, why, and what
// Kubernetes said.
type Gap struct {
	Template string
	Cause    evidence.GapCause
	Detail   string
}

// Diagnosis is what Themis knows about a workload before anyone investigates:
// the evidence it read, what it couldn't, and its checks' answers.
type Diagnosis struct {
	Items []evidence.Item
	Gaps  []Gap
	// Results are the profiles' checks, in the catalog's order. Each unknown
	// one carries the gaps in the evidence it reads, and is blind when Themis
	// couldn't see.
	Results []check.Result
}

// Diagnose reads w's initial evidence under the profiles m with c, all any of
// them collects first, and decides every check any of them runs from it, as
// of at.Now, so each profile's rules can read its checks. Evidence it can't
// read is a gap with its cause, not a failure: the checks that needed it are
// unknown and say why. So are the target's first events, when its alert
// started longer ago than Kubernetes keeps events.
func (cat Catalog) Diagnose(ctx context.Context, c client.Reader, w *workload.Workload, m config.Matched, at Moment) Diagnosis {
	var d Diagnosis
	initial := m.Evidence().Initial
	for _, name := range initial {
		item, err := cat.Evidence.Collect(ctx, c, name, w, nil, at.Now)
		if err != nil {
			d.Gaps = append(d.Gaps, Gap{Template: name, Cause: evidence.CauseOf(err), Detail: err.Error()})
			continue
		}
		d.Items = append(d.Items, item)
	}
	if g, ok := expiredEvents(initial, at); ok {
		d.Gaps = append(d.Gaps, g)
	}
	checks := cat.Checks.For(m.Checks())
	gaps := make([]check.Gap, 0, len(d.Gaps))
	for _, g := range d.Gaps {
		gaps = append(gaps, check.Gap{Template: g.Template, Cause: g.Cause})
	}
	d.Results = checks.Attribute(checks.Evaluate(evidence.Bundle{Target: w.Target, Items: d.Items}, at.Now), gaps)
	return d
}

// expiredEvents is the gap left when the target's first events are gone.
func expiredEvents(initial []string, at Moment) (Gap, bool) {
	if !slices.Contains(initial, evidence.TargetEvents) || at.AlertStarted.IsZero() {
		return Gap{}, false
	}
	retention := at.EventRetention
	if retention == 0 {
		retention = DefaultEventRetention
	}
	age := at.Now.Sub(at.AlertStarted)
	if age <= retention {
		return Gap{}, false
	}
	return Gap{
		Template: evidence.TargetEvents, Cause: evidence.GapExpired,
		Detail: fmt.Sprintf("the alert started %s before, and Kubernetes keeps events for about %s", age.Round(time.Minute), retention),
	}, true
}
