package evidence

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/incident"
)

// TargetEvents lists the recent events of the target, its ReplicaSets and its
// pods.
const TargetEvents = "target_events"

// maxEvents bounds an item; the most recent events are kept.
const maxEvents = 100

type TargetEventsView struct {
	Events      []EventView `json:"events"`
	TotalEvents int         `json:"totalEvents"`
}

type EventView struct {
	Object  string `json:"object"` // Kind/name
	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
	Count   int32  `json:"count,omitzero"`

	// FirstSeen and LastSeen are left out when the event carries no time.
	FirstSeen time.Time `json:"firstSeen,omitzero"`
	LastSeen  time.Time `json:"lastSeen,omitzero"`
}

type targetEvents struct{}

// NewTargetEvents returns the target_events template.
func NewTargetEvents() Template { return targetEvents{} }

func (targetEvents) Name() string { return TargetEvents }

func (targetEvents) Collect(ctx context.Context, c client.Reader, t incident.TargetRef, _ map[string]string) (Item, error) {
	d, err := getDeployment(ctx, c, t)
	if err != nil {
		return Item{}, err
	}
	replicaSets, err := ownedReplicaSets(ctx, c, d)
	if err != nil {
		return Item{}, err
	}
	pods, err := ownedPods(ctx, c, d, replicaSets)
	if err != nil {
		return Item{}, err
	}
	about := map[types.UID]bool{d.UID: true}
	for _, rs := range replicaSets {
		about[rs.UID] = true
	}
	for _, p := range pods {
		about[p.UID] = true
	}

	var list corev1.EventList
	if err := c.List(ctx, &list, client.InNamespace(t.Namespace)); err != nil {
		return Item{}, fmt.Errorf("list events in %s: %w", t.Namespace, err)
	}
	var view TargetEventsView
	for i := range list.Items {
		ev := &list.Items[i]
		if !about[ev.InvolvedObject.UID] {
			continue
		}
		view.Events = append(view.Events, viewEvent(ev))
	}

	view.TotalEvents = len(view.Events)
	slices.SortFunc(view.Events, func(a, b EventView) int {
		return cmp.Or(b.LastSeen.Compare(a.LastSeen), cmp.Compare(a.Reason, b.Reason))
	})
	truncated := len(view.Events) > maxEvents
	if truncated {
		view.Events = view.Events[:maxEvents]
	}

	data, err := json.Marshal(view)
	if err != nil {
		return Item{}, err
	}
	return Item{Data: data, Truncated: truncated}, nil
}

// viewEvent takes each time from whichever field the writer set: legacy
// writers set the timestamps, events.k8s.io/v1 writers set eventTime and the
// series.
func viewEvent(ev *corev1.Event) EventView {
	v := EventView{
		Object:  ev.InvolvedObject.Kind + "/" + ev.InvolvedObject.Name,
		Type:    bounded(ev.Type, maxReasonBytes),
		Reason:  bounded(ev.Reason, maxReasonBytes),
		Message: bounded(ev.Message, maxMessageBytes),
		Count:   ev.Count,
	}
	eventTime := ev.EventTime.Time
	v.FirstSeen = firstNonZero(ev.FirstTimestamp.Time, eventTime)
	var seriesLast time.Time
	if ev.Series != nil {
		seriesLast = ev.Series.LastObservedTime.Time
		v.Count = max(v.Count, ev.Series.Count)
	}
	v.LastSeen = firstNonZero(ev.LastTimestamp.Time, seriesLast, eventTime, v.FirstSeen)
	return v
}

func firstNonZero(times ...time.Time) time.Time {
	for _, t := range times {
		if !t.IsZero() {
			return t.UTC()
		}
	}
	return time.Time{}
}
