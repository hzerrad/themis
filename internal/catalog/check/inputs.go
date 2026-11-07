package check

import (
	"encoding/json/v2"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/hzerrad/themis/internal/catalog/evidence"
)

// What a check's expressions see, besides each evidence template's latest
// answer under the template's name. Variables marked as read from rollout
// history are missing with it.
const (
	varNow = "now" // when the checks run

	// varIDs maps each template to the ID of the item its variable holds.
	varIDs = "ids"

	// Read from rollout history.
	varCurrent   = "current"    // the current revision, or null
	varPrior     = "prior"      // the newest revision older than it, or null
	varRolloutAt = "rollout_at" // when the current revision rolled out, or null

	// varCurrentRevision is the current revision's number: from rollout
	// history, or without it the newest revision among the pods.
	varCurrentRevision = "current_revision"

	// varFailures are the signs of trouble, in order: warning events about
	// pods, then containers that died. Each has at (null when unknown), what
	// and id, the item it was read from.
	varFailures = "failures"
	// varFirstFailure is the earliest failure, dated by the pods' status and
	// by the events it agrees with, or null when there is none or one can't
	// be dated and so could be earlier still.
	varFirstFailure = "first_failure"

	// varSuspicious is text in any evidence that looks written for the
	// investigator, in collection order. Each has id and template, the item
	// it is in, reason and at.
	varSuspicious = "suspicious_evidence"

	// varThemisFix is Themis's own fix that left the target as it is, from
	// its record (evidence.ThemisFixView), with id, the item it was read
	// from; null when the target runs no fix of Themis's. Its absence is an
	// answer, not missing evidence, so a check that reads it needs nothing.
	varThemisFix = "themis_fix"

	// varWorkload is the name of the workload the evidence is about, for
	// the texts people read.
	varWorkload = "workload"
)

// fromHistory are the variables read from rollout history.
var fromHistory = []string{varCurrent, varPrior, varRolloutAt}

// views are the types the built-in templates answer with. The expressions see
// them as their JSON, except that times are timestamps, and a time or object
// that isn't known is null rather than left out.
var views = map[string]func() any{
	evidence.RolloutHistory:    func() any { return new(evidence.RolloutHistoryView) },
	evidence.PodStates:         func() any { return new(evidence.PodStatesView) },
	evidence.TargetEvents:      func() any { return new(evidence.TargetEventsView) },
	evidence.ReferencedConfig:  func() any { return new(evidence.ReferencedConfigView) },
	evidence.ContainerLogsTail: func() any { return new(evidence.LogsView) },
	evidence.ContainerMemory:   func() any { return new(evidence.ContainerMemoryView) },
	evidence.ContainerCPU:      func() any { return new(evidence.ContainerCPUView) },
	evidence.RevisionHealth:    func() any { return new(evidence.RevisionHealthView) },
	evidence.ThemisFix:         func() any { return new(evidence.ThemisFixView) },
}

// inputs are the variables every check of one evaluation sees.
type inputs struct {
	vars map[string]any
	// ids are the items the template variables hold, by template.
	ids map[string]string
}

// missing says whether there is no evidence from template.
func (in inputs) missing(template string) bool {
	_, ok := in.ids[template]
	return !ok
}

func newInputs(b evidence.Bundle, now time.Time) inputs {
	in := inputs{vars: map[string]any{varNow: now, varWorkload: b.Target.Name}, ids: map[string]string{}}
	decoded := map[string]any{}
	for _, it := range b.Items {
		if _, done := decoded[it.Template]; done {
			continue
		}
		if view, id, ok := latest(b, it.Template); ok {
			decoded[it.Template], in.ids[it.Template], in.vars[it.Template] = view, id, value(reflect.ValueOf(view))
		}
	}
	in.vars[varIDs] = in.ids

	// The template's variable is the item's view, as for any template, with
	// its ID beside it.
	if fix, ok := in.vars[evidence.ThemisFix].(map[string]any); ok {
		fix["id"] = in.ids[evidence.ThemisFix]
	} else {
		in.vars[varThemisFix] = nil
	}

	revision := ""
	if h, ok := decoded[evidence.RolloutHistory].(*evidence.RolloutHistoryView); ok {
		in.vars[varCurrent], in.vars[varPrior], in.vars[varRolloutAt] = nil, nil, nil
		if cur, ok := currentRevision(*h); ok {
			revision = cur.Revision
			in.vars[varCurrent] = value(reflect.ValueOf(cur))
			if at := rolloutAt(*h, cur); !at.IsZero() {
				in.vars[varRolloutAt] = at
			}
			if prior, ok := priorRevision(*h, cur); ok {
				in.vars[varPrior] = value(reflect.ValueOf(prior))
			}
		}
	}
	if revision == "" {
		revision = newestPodRevision(decoded[evidence.PodStates])
	}
	in.vars[varCurrentRevision] = revision

	fs := failures(decoded, in.ids)
	list := make([]any, 0, len(fs))
	for _, f := range fs {
		list = append(list, f.value())
	}
	in.vars[varFailures] = list
	in.vars[varFirstFailure] = nil
	if first, ok := firstFailure(decoded, in.ids); ok {
		in.vars[varFirstFailure] = first.value()
	}

	suspicious := []any{}
	for _, it := range b.Items {
		for _, s := range it.Suspicious {
			suspicious = append(suspicious, map[string]any{"id": it.ID, "template": it.Template, "reason": s.Reason, "at": s.At})
		}
	}
	in.vars[varSuspicious] = suspicious
	return in
}

// latest decodes the last item of template in b that decodes, returning its
// ID.
func latest(b evidence.Bundle, template string) (any, string, bool) {
	items := b.ByTemplate(template)
	for i := len(items) - 1; i >= 0; i-- {
		var v any
		if view, ok := views[template]; ok {
			v = view()
		} else {
			v = new(map[string]any)
		}
		if json.Unmarshal(items[i].Data, v) == nil {
			return v, items[i].ID, true
		}
	}
	return nil, "", false
}

// value is v as the expressions see it: structs as maps by their JSON names,
// times as timestamps, and nil pointers and unknown times as null.
func value(v reflect.Value) any {
	switch k := v.Kind(); {
	case k == reflect.Pointer || k == reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return value(v.Elem())
	case k == reflect.Struct:
		if t, ok := v.Interface().(time.Time); ok {
			if t.IsZero() {
				return nil
			}
			return t
		}
		m := map[string]any{}
		for i := range v.NumField() {
			f := v.Type().Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if !f.IsExported() || name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			m[name] = value(v.Field(i))
		}
		return m
	case k == reflect.Slice || k == reflect.Array:
		out := make([]any, v.Len())
		for i := range out {
			out[i] = value(v.Index(i))
		}
		return out
	case k == reflect.Map:
		out := make(map[string]any, v.Len())
		for it := v.MapRange(); it.Next(); {
			out[it.Key().String()] = value(it.Value())
		}
		return out
	case k == reflect.String:
		return v.String()
	case k == reflect.Bool:
		return v.Bool()
	case v.CanInt():
		return v.Int()
	case v.CanUint():
		return v.Uint()
	case v.CanFloat():
		return v.Float()
	default:
		return nil
	}
}

func currentRevision(h evidence.RolloutHistoryView) (evidence.RevisionView, bool) {
	for _, r := range h.Revisions {
		if r.Current {
			return r, true
		}
	}
	return evidence.RevisionView{}, false
}

// rolloutAt is when the current revision rolled out: its ReplicaSet's
// creation, or a later template change that reused it.
func rolloutAt(h evidence.RolloutHistoryView, cur evidence.RevisionView) time.Time {
	if h.Deployment.TemplateChangedAt.After(cur.CreatedAt) {
		return h.Deployment.TemplateChangedAt
	}
	return cur.CreatedAt
}

// priorRevision is the newest revision older than the current one.
func priorRevision(h evidence.RolloutHistoryView, current evidence.RevisionView) (evidence.RevisionView, bool) {
	cur, err := strconv.ParseInt(current.Revision, 10, 64)
	if err != nil {
		return evidence.RevisionView{}, false
	}
	var best evidence.RevisionView
	var bestN int64 = -1
	for _, r := range h.Revisions {
		n, err := strconv.ParseInt(r.Revision, 10, 64)
		if err == nil && n < cur && n > bestN {
			best, bestN = r, n
		}
	}
	return best, bestN >= 0
}

// newestPodRevision is the highest numbered revision among the pods, if any.
func newestPodRevision(view any) string {
	ps, ok := view.(*evidence.PodStatesView)
	if !ok {
		return ""
	}
	revision := ""
	for _, p := range ps.Pods {
		if n, err := strconv.Atoi(p.Revision); err == nil {
			if m, _ := strconv.Atoi(revision); n > m {
				revision = p.Revision
			}
		}
	}
	return revision
}

// failure is a sign of trouble: a warning event or a container that died.
type failure struct {
	at   time.Time
	what string
	id   string
}

func (f failure) value() map[string]any {
	var at any
	if !f.at.IsZero() {
		at = f.at
	}
	return map[string]any{"at": at, "what": f.what, "id": f.id}
}

func failures(decoded map[string]any, ids map[string]string) []failure {
	var fs []failure
	if ev, ok := decoded[evidence.TargetEvents].(*evidence.TargetEventsView); ok {
		for _, e := range ev.Events {
			if e.Type == "Warning" && strings.HasPrefix(e.Object, "Pod/") {
				fs = append(fs, failure{e.FirstSeen, e.Reason + " on " + e.Object, ids[evidence.TargetEvents]})
			}
		}
	}
	if ps, ok := decoded[evidence.PodStates].(*evidence.PodStatesView); ok {
		for _, p := range ps.Pods {
			for _, c := range p.Containers {
				if t := c.LastTermination; t != nil && t.Reason != "Completed" {
					fs = append(fs, failure{t.FinishedAt, t.Reason + " of " + p.Name + "/" + c.Name, ids[evidence.PodStates]})
				}
			}
		}
	}
	return fs
}

// firstFailure is the earliest failure, dated by the pods' status, which only
// Kubernetes' own components write in practice: anyone who may create events
// can date one as they like. A container's last termination dates a failure,
// and its first when it restarted at most once. The status doesn't date the
// first failure of a container that restarted more than once, or of a pod
// that isn't ready for another reason: an event about that pod dates it, when
// the pod started no later than the event. Other events are left out, since
// the status already says when, or that nothing failed. There is none when a
// failure can't be dated, as it could be earlier still.
func firstFailure(decoded map[string]any, ids map[string]string) (failure, bool) {
	ps, ok := decoded[evidence.PodStates].(*evidence.PodStatesView)
	if !ok {
		return failure{}, false
	}
	var fs []failure
	// The pods whose first failure only an event can date, and for those
	// that must have one, the latest it can be.
	open := map[string]evidence.PodView{}
	needed := map[string]time.Time{}
	for _, p := range ps.Pods {
		ended := false
		for _, c := range p.Containers {
			t := c.LastTermination
			if t == nil || t.Reason == "Completed" {
				continue
			}
			if t.FinishedAt.IsZero() {
				return failure{}, false
			}
			ended = true
			fs = append(fs, failure{t.FinishedAt, t.Reason + " of " + p.Name + "/" + c.Name, ids[evidence.PodStates]})
			if c.RestartCount > 1 {
				open["Pod/"+p.Name] = p
				if by, ok := needed[p.Name]; !ok || t.FinishedAt.Before(by) {
					needed[p.Name] = t.FinishedAt
				}
			}
		}
		if !p.Ready && !ended {
			open["Pod/"+p.Name] = p
		}
	}
	if ev, ok := decoded[evidence.TargetEvents].(*evidence.TargetEventsView); ok {
		for _, e := range ev.Events {
			p, ok := open[e.Object]
			if e.Type != "Warning" || !ok || p.StartedAt.IsZero() || (!e.FirstSeen.IsZero() && e.FirstSeen.Before(p.StartedAt)) {
				continue
			}
			if e.FirstSeen.IsZero() {
				return failure{}, false
			}
			fs = append(fs, failure{e.FirstSeen, e.Reason + " on " + e.Object, ids[evidence.TargetEvents]})
			if by, ok := needed[p.Name]; ok && !e.FirstSeen.After(by) {
				delete(needed, p.Name)
			}
		}
	}
	if len(fs) == 0 || len(needed) > 0 {
		return failure{}, false
	}
	first := fs[0]
	for _, f := range fs {
		if f.at.Before(first.at) {
			first = f
		}
	}
	return first, true
}
