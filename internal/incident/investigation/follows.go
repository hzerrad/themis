package investigation

import (
	"context"
	"encoding/json/v2"
	"errors"
	"slices"
	"time"

	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/workload"
)

// themisFix is the evidence of Themis's own fix that left target as it is,
// when it runs one: the incident it follows, what the fix did, and what that
// incident's checks found of its failure before it. The fix's rollout then
// brought no new version, and the revision it replaced is known to have
// failed, even once its pods are gone. None when target runs no fix of
// Themis's.
func (w *PrepareWorker) themisFix(ctx context.Context, target *workload.Workload, now time.Time) ([]evidence.Item, error) {
	fix, err := w.Store.AppliedFixLeaving(ctx, target.Target.UID, target.TemplateHash)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	earlier, err := w.Store.Incident(ctx, fix.Action.IncidentID)
	if err != nil {
		return nil, err
	}
	results, err := w.Store.Predicates(ctx, earlier.ID)
	if err != nil {
		return nil, err
	}
	items, err := w.Store.Evidence(ctx, earlier.ID)
	if err != nil {
		return nil, err
	}
	view := evidence.ThemisFixView{
		Incident: earlier.ID, Fix: fix.Action.Capability, Done: fix.Done, AppliedAt: fix.AppliedAt.UTC(),
		Replaced: revisionOf(items), Outcome: fix.Outcome,
	}
	symptoms := w.Config.Current().Matched(earlier.Profiles()).Symptoms()
	for _, r := range results {
		if r.Value == check.True && slices.Contains(symptoms, r.Name) {
			view.Found = append(view.Found, evidence.FoundView{Check: r.Name, Reason: r.Reason})
		}
	}
	item, err := evidence.NewThemisFix(view, target.Target, now)
	if err != nil {
		return nil, err
	}
	return []evidence.Item{item}, nil
}

// revisionOf is the revision an incident was about, by the rollout history
// it read first; empty without one.
func revisionOf(items []evidence.Item) string {
	for _, it := range items {
		var h evidence.RolloutHistoryView
		if it.Template == evidence.RolloutHistory && json.Unmarshal(it.Data, &h) == nil {
			return h.Deployment.Revision
		}
	}
	return ""
}
