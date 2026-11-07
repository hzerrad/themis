package catalog

import (
	"context"
	"slices"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/workload"
)

// SignalWindow is the least time the workload must run before the checks
// named can be decided again from what it did since: the longest of the
// least windows the evidence they read over a window needs, such as 5
// minutes for a share of CPU periods throttled, which is a rate. Zero when
// they read nothing over a window, such as the pods' states.
func (cat Catalog) SignalWindow(names []string) time.Duration {
	var out time.Duration
	for _, t := range cat.reads(names) {
		if w, ok := cat.Evidence[t].(evidence.Windowed); ok {
			out = max(out, w.MinWindow())
		}
	}
	return out
}

// Recheck decides the checks named again, from evidence read now, so a
// decision rests on how w is doing now rather than on how it was when its
// incident opened. Evidence read over a window, such as CPU throttling, is
// read over window, in whole minutes so the checks' reasons read plainly,
// and never over less than SignalWindow. Evidence it can't read leaves the
// checks that need it unknown, saying why.
func (cat Catalog) Recheck(ctx context.Context, c client.Reader, w *workload.Workload, names []string, window time.Duration, now time.Time) []check.Result {
	_, results := cat.Reread(ctx, c, w, names, max(window.Truncate(time.Minute), cat.SignalWindow(names)), now)
	return results
}

// Reread reads again, now, the evidence the checks named read, and decides
// them from it with own: evidence from Themis's own record rather than the
// cluster, such as its fix the target runs. Evidence read over a window is
// read over window, or, when window is zero, over the window each template
// reads by default, as an investigation reads it. Evidence it can't read
// leaves the checks that need it unknown, saying why. It returns what it
// read, for the record, with the checks' answers.
func (cat Catalog) Reread(ctx context.Context, c client.Reader, w *workload.Workload, names []string, window time.Duration, now time.Time, own ...evidence.Item) ([]evidence.Item, []check.Result) {
	if len(names) == 0 {
		return nil, nil
	}
	var read []evidence.Item
	var gaps []check.Gap
	for _, t := range cat.reads(names) {
		var params map[string]string
		if tmpl, ok := cat.Evidence[t]; ok && window > 0 && slices.Contains(tmpl.Params(), evidence.WindowParam) {
			params = map[string]string{evidence.WindowParam: window.String()}
		}
		item, err := cat.Evidence.Collect(ctx, c, t, w, params, now)
		if err != nil {
			gaps = append(gaps, check.Gap{Template: t, Cause: evidence.CauseOf(err)})
			continue
		}
		read = append(read, item)
	}
	checks := cat.Checks.For(names)
	b := evidence.Bundle{Target: w.Target, Items: append(slices.Clone(own), read...)}
	return read, checks.Attribute(checks.Evaluate(b, now), gaps)
}

// reads are the templates the checks named read, each once.
func (cat Catalog) reads(names []string) []string {
	var out []string
	for _, name := range names {
		ch, ok := cat.Checks.Get(name)
		if !ok {
			continue
		}
		for _, t := range ch.Reads {
			if !slices.Contains(out, t) {
				out = append(out, t)
			}
		}
	}
	return out
}
