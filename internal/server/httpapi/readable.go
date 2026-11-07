package httpapi

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/hzerrad/themis/internal/catalog/evidence"
)

// kinds name each evidence template as people call it.
var kinds = map[string]string{
	evidence.RolloutHistory:    "Rollout history",
	evidence.PodStates:         "Pod states",
	evidence.TargetEvents:      "Events",
	evidence.ContainerLogsTail: "Container logs",
	evidence.ReferencedConfig:  "Referenced config",
	evidence.ContainerMemory:   "Container memory",
	evidence.ContainerCPU:      "Container CPU",
	evidence.RevisionHealth:    "Revision health",
	evidence.ThemisFix:         "Themis's earlier fix",
}

// kindOf names a template; one Themis doesn't know reads as its name in
// words.
func kindOf(template string) string {
	if k, ok := kinds[template]; ok {
		return k
	}
	r := []rune(strings.ReplaceAll(template, "_", " "))
	if len(r) > 0 {
		r[0] = unicode.ToUpper(r[0])
	}
	return string(r)
}

// readable is a piece of evidence laid out by its kind, for people: one of
// the views, with notes under it. A template Themis has no view for shows
// its fields.
type readable struct {
	Pods      *podsView      `json:"pods,omitempty"`
	Events    *eventsView    `json:"events,omitempty"`
	Logs      *logsView      `json:"logs,omitempty"`
	Gauges    []gauge        `json:"gauges,omitempty"`
	Revisions []revisionView `json:"revisions,omitempty"`
	Config    []configRow    `json:"config,omitempty"`
	Fix       *fixRecordView `json:"fix,omitempty"`
	Health    []phrase       `json:"health,omitempty"`
	Fields    []fieldRow     `json:"fields,omitempty"`
	Notes     []phrase       `json:"notes,omitempty"`
}

type podsView struct {
	Rows []podRow `json:"rows"`
}

// podRow is one pod: how its containers last ended, from the container
// that ended last.
type podRow struct {
	Name     string `json:"name"`
	Revision string `json:"revision,omitempty"`
	Ready    bool   `json:"ready"`
	// Waiting is why a container waits to run, such as CrashLoopBackOff.
	Waiting  string `json:"waiting,omitempty"`
	Restarts int32  `json:"restarts"`
	// Ended says how the last run ended, such as "OOMKilled, exit 137", and
	// Failed marks an end that isn't a clean exit.
	Ended   string    `json:"ended,omitempty"`
	EndedAt time.Time `json:"endedAt,omitzero"`
	Failed  bool      `json:"failed,omitempty"`
}

type eventsView struct {
	Rows []eventRow `json:"rows"`
}

type eventRow struct {
	LastSeen time.Time `json:"lastSeen,omitzero"`
	Type     string    `json:"type"`
	Warning  bool      `json:"warning,omitempty"`
	Reason   string    `json:"reason"`
	Object   string    `json:"object"`
	Message  string    `json:"message,omitempty"`
	Count    int32     `json:"count"`
}

// logsView is a container's logs, each run the incident holds of it, this
// item's first.
type logsView struct {
	Pod       string   `json:"pod"`
	Container string   `json:"container"`
	Runs      []logRun `json:"runs"`
}

type logRun struct {
	Previous bool `json:"previous"`
	// N is the item the run is, in the list.
	N     int       `json:"n"`
	Lines []logLine `json:"lines"`
	// Ended says how the run ended, from the pod states, such as
	// "OOMKilled, exit 137".
	Ended   string    `json:"ended,omitempty"`
	EndedAt time.Time `json:"endedAt,omitzero"`
	// Note says why there are no lines, as the logs say it.
	Note string `json:"note,omitempty"`
}

type logLine struct {
	At   time.Time `json:"at,omitzero"`
	Text string    `json:"text"`
	// Flagged marks suspicious text.
	Flagged bool `json:"flagged,omitempty"`
}

// gauge is a figure against its scale, labelled where it is: the fill's
// end above, a tick and the track's end below.
type gauge struct {
	Label string `json:"label"`
	// Fill is how far the fill reaches, in percent of the track.
	Fill     float64 `json:"fill"`
	FillText string  `json:"fillText"`
	// Tick marks a request, where there is one.
	Tick *tick `json:"tick,omitempty"`
	// End says what the track's end is, such as "limit 60Mi".
	End string `json:"end"`
	// Aria says it all in one sentence, for screen readers.
	Aria string `json:"aria"`
}

type tick struct {
	At   float64 `json:"at"`
	Text string  `json:"text"`
}

type revisionView struct {
	Revision   string    `json:"revision"`
	Current    bool      `json:"current,omitempty"`
	CreatedAt  time.Time `json:"createdAt,omitzero"`
	ReplicaSet string    `json:"replicaSet"`
	// Runs says what each container runs.
	Runs []phrase `json:"runs"`
	// Changed says what changed from the revision before it; First is set
	// when none came before it.
	Changed []phrase `json:"changed,omitempty"`
	First   bool     `json:"first,omitempty"`
}

type configRow struct {
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	Optional  bool      `json:"optional,omitempty"`
	Missing   bool      `json:"missing,omitempty"`
	ChangedAt time.Time `json:"changedAt,omitzero"`
}

type fixRecordView struct {
	Incident  string    `json:"incident"`
	Done      string    `json:"done"`
	AppliedAt time.Time `json:"appliedAt"`
	Outcome   string    `json:"outcome,omitempty"`
	Found     []string  `json:"found,omitempty"`
}

type fieldRow struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// shown is a piece of evidence as people read it: a digest in one line, and
// a view by its kind. all is the incident's evidence, numbered from 1 in
// order, which a view may read beside it, such as how a run of logs ended,
// from the pod states.
func shown(it evidence.Item, all []evidence.Item) (phrase, *readable) {
	r := &readable{}
	var digest phrase
	var err error
	switch it.Template {
	case evidence.PodStates:
		digest, err = r.pods(it)
	case evidence.TargetEvents:
		digest, err = r.events(it)
	case evidence.ContainerLogsTail:
		digest, err = r.logs(it, all)
	case evidence.ContainerMemory:
		digest, err = r.memory(it)
	case evidence.ContainerCPU:
		digest, err = r.cpu(it)
	case evidence.RolloutHistory:
		digest, err = r.rollout(it)
	case evidence.ReferencedConfig:
		digest, err = r.config(it)
	case evidence.ThemisFix:
		digest, err = r.fix(it)
	case evidence.RevisionHealth:
		digest, err = r.health(it)
	default:
		err = errNoView
	}
	if err != nil {
		*r = readable{}
		digest = r.fields(it)
	}
	if it.Truncated && it.Template == evidence.ContainerLogsTail {
		r.Notes = append(r.Notes, phrase{}.text("Only the end of the logs is kept."))
	}
	if it.Redactions > 0 {
		r.Notes = append(r.Notes, phrase{}.text(plural(it.Redactions, "value that looks like a secret is", "values that look like secrets are")+" hidden."))
	}
	return digest, r
}

var errNoView = fmt.Errorf("no view")

func (r *readable) pods(it evidence.Item) (phrase, error) {
	var v evidence.PodStatesView
	if err := json.Unmarshal(it.Data, &v); err != nil {
		return nil, err
	}
	var (
		restarts int32
		ready    int
		last     podRow
		waiting  string
	)
	view := &podsView{Rows: []podRow{}}
	for _, p := range v.Pods {
		row := podRow{Name: p.Name, Revision: p.Revision, Ready: p.Ready}
		for _, c := range p.Containers {
			row.Restarts += c.RestartCount
			for _, s := range []*evidence.StateView{c.State, c.LastTermination} {
				if s == nil {
					continue
				}
				if s.Status == "waiting" && s.Reason != "" && row.Waiting == "" {
					row.Waiting = s.Reason
				}
				if s.Status == "terminated" && !s.FinishedAt.Before(row.EndedAt) {
					row.Ended, row.EndedAt, row.Failed = ended(s), s.FinishedAt, s.Reason != "Completed"
				}
			}
		}
		restarts += row.Restarts
		if p.Ready {
			ready++
		}
		if waiting == "" {
			waiting = row.Waiting
		}
		if row.Ended != "" && !row.EndedAt.Before(last.EndedAt) {
			last = row
		}
		view.Rows = append(view.Rows, row)
	}
	r.Pods = view
	if v.TotalPods > len(v.Pods) {
		r.Notes = append(r.Notes, phrase{}.text(fmt.Sprintf("Showing %d of %d pods, those not ready first.", len(v.Pods), v.TotalPods)))
	}
	d := phrase{}.text(plural(v.TotalPods, "pod", "pods"))
	switch {
	case ready == len(v.Pods):
		d = d.text(", all ready")
	case ready == 0:
		d = d.text(", none ready")
	default:
		d = d.text(fmt.Sprintf(", %d ready", ready))
	}
	d = d.text(", " + plural(int(restarts), "restart", "restarts") + ".")
	switch {
	case last.Ended != "":
		reason, exit, _ := strings.Cut(last.Ended, ", ")
		d = d.text(" Last ended ").value(reason)
		if exit != "" {
			d = d.text(", " + exit)
		}
		if !last.EndedAt.IsZero() {
			d = d.text(", at ").at(last.EndedAt)
		}
		d = d.text(".")
	case waiting != "":
		d = d.text(" Waiting with ").value(waiting).text(".")
	}
	return d, nil
}

// ended says how a container's run ended: its reason and exit code.
func ended(s *evidence.StateView) string {
	out := cmp.Or(s.Reason, "Terminated")
	if s.ExitCode != nil {
		out += ", exit " + strconv.Itoa(int(*s.ExitCode))
	}
	return out
}

func (r *readable) events(it evidence.Item) (phrase, error) {
	var v evidence.TargetEventsView
	if err := json.Unmarshal(it.Data, &v); err != nil {
		return nil, err
	}
	view := &eventsView{Rows: []eventRow{}}
	total := 0
	byReason := map[string]int{}
	for _, e := range v.Events {
		n := max(int(e.Count), 1)
		total += n
		byReason[e.Reason] += n
		view.Rows = append(view.Rows, eventRow{
			LastSeen: e.LastSeen, Type: e.Type, Warning: e.Type == "Warning", Reason: e.Reason,
			Object: e.Object, Message: e.Message, Count: int32(n), //nolint:gosec // a count of events
		})
	}
	r.Events = view
	if v.TotalEvents > len(v.Events) {
		r.Notes = append(r.Notes, phrase{}.text(fmt.Sprintf("Showing the newest %d of %d events.", len(v.Events), v.TotalEvents)))
	}
	if total == 0 {
		return phrase{}.text("No events."), nil
	}
	reasons := slices.SortedFunc(maps.Keys(byReason), func(a, b string) int {
		return cmp.Or(cmp.Compare(byReason[b], byReason[a]), cmp.Compare(a, b))
	})
	d := phrase{}.text(plural(total, "event", "events") + ". ")
	for i, reason := range reasons[:min(3, len(reasons))] {
		if i > 0 {
			d = d.text(", ")
		}
		d = d.value(reason).text(" ×" + strconv.Itoa(byReason[reason]))
	}
	if more := len(reasons) - 3; more > 0 {
		d = d.text(", and " + plural(more, "other reason", "other reasons"))
	}
	return d.text("."), nil
}

// stamp finds a time a log line starts with.
var stamp = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:[.,]\d+)?(?:Z|[+-]\d{2}:?\d{2})?)\s+`)

// lineTime is the time a log line starts with, and the rest of it.
func lineTime(line string) (time.Time, string) {
	m := stamp.FindStringSubmatch(line)
	if m == nil {
		return time.Time{}, line
	}
	s := strings.Replace(strings.Replace(m[1], " ", "T", 1), ",", ".", 1)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999Z0700", "2006-01-02T15:04:05.999999999"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, line[len(m[0]):]
		}
	}
	return time.Time{}, line
}

func (r *readable) logs(it evidence.Item, all []evidence.Item) (phrase, error) {
	var v evidence.LogsView
	if err := json.Unmarshal(it.Data, &v); err != nil {
		return nil, err
	}
	view := &logsView{Pod: v.Pod, Container: v.Container}
	own := logRunOf(it, v, all)
	view.Runs = append(view.Runs, own)
	// The other runs of the same container, for switching between them.
	for i, other := range all {
		var o evidence.LogsView
		if other.ID == it.ID || other.Template != evidence.ContainerLogsTail || json.Unmarshal(other.Data, &o) != nil ||
			o.Pod != v.Pod || o.Container != v.Container || o.Previous == v.Previous ||
			slices.ContainsFunc(view.Runs, func(r logRun) bool { return r.Previous == o.Previous }) {
			continue
		}
		run := logRunOf(other, o, all)
		run.N = i + 1
		view.Runs = append(view.Runs, run)
	}
	r.Logs = view
	r.Notes = append(r.Notes, phrase{}.text("Pod ").value(v.Pod).text("."))
	d := phrase{}.value(v.Container).text(", " + runName(v.Previous) + ". ")
	lines := own.Lines
	if len(lines) == 0 {
		return d.text("No lines."), nil
	}
	d = d.text(plural(len(lines), "line", "lines") + ". Last: ")
	last := lines[len(lines)-1].Text
	if r := []rune(last); len(r) > 80 {
		last = string(r[:79]) + "…"
	}
	return d.value(last), nil
}

func runName(previous bool) string {
	if previous {
		return "previous run"
	}
	return "current run"
}

// logRunOf is the run an item of logs holds, with how it ended, from the
// latest pod states, and its suspicious lines marked.
func logRunOf(it evidence.Item, v evidence.LogsView, all []evidence.Item) logRun {
	run := logRun{Previous: v.Previous, Note: v.Note, Lines: []logLine{}}
	for i, other := range all {
		if other.ID == it.ID {
			run.N = i + 1
		}
	}
	var flagged []string
	for _, s := range it.Suspicious {
		if s.At == "/logs" {
			if t := strings.Trim(s.Excerpt, "…"); t != "" {
				flagged = append(flagged, t)
			}
		}
	}
	for _, line := range strings.Split(strings.TrimRight(v.Logs, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		at, text := lineTime(line)
		run.Lines = append(run.Lines, logLine{
			At: at, Text: text,
			Flagged: slices.ContainsFunc(flagged, func(f string) bool { return strings.Contains(line, f) || strings.Contains(f, strings.TrimSpace(text)) }),
		})
	}
	if !v.Previous {
		return run
	}
	for i := len(all) - 1; i >= 0; i-- {
		var pods evidence.PodStatesView
		if all[i].Template != evidence.PodStates || json.Unmarshal(all[i].Data, &pods) != nil {
			continue
		}
		for _, p := range pods.Pods {
			for _, c := range p.Containers {
				if p.Name == v.Pod && c.Name == v.Container && c.LastTermination != nil && c.LastTermination.Status == "terminated" {
					run.Ended, run.EndedAt = ended(c.LastTermination), c.LastTermination.FinishedAt
				}
			}
		}
		break
	}
	return run
}

// mebibytes writes bytes as people write memory: 57Mi, 1.5Gi.
func mebibytes(b int64) string {
	units := []string{"", "Ki", "Mi", "Gi", "Ti"}
	v := float64(b)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if v >= 10 || v == math.Trunc(v) {
		return strconv.FormatFloat(math.Round(v), 'f', -1, 64) + units[i]
	}
	return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64) + units[i]
}

// millicores writes cores as Kubernetes writes CPU: 250m, 2.
func millicores(c float64) string {
	m := math.Round(c * 1000)
	if math.Mod(m, 1000) == 0 {
		return strconv.FormatFloat(m/1000, 'f', -1, 64)
	}
	return strconv.FormatFloat(m, 'f', -1, 64) + "m"
}

func percent(share float64) string { return strconv.Itoa(int(math.Round(share*100))) + "%" }

// sourceName names a metrics source as people do.
func sourceName(s string) string {
	switch s {
	case "prometheus":
		return "Prometheus"
	case "kubernetes", "metrics-server":
		return "the Kubernetes metrics API"
	}
	return s
}

// scaled is v along a track that ends at scale, in percent.
func scaled(v, scale float64) float64 {
	if scale <= 0 {
		return 0
	}
	return math.Round(min(100, v/scale*100)*10) / 10
}

func (r *readable) memory(it evidence.Item) (phrase, error) {
	var v evidence.ContainerMemoryView
	if err := json.Unmarshal(it.Data, &v); err != nil {
		return nil, err
	}
	var top *evidence.MemoryUseView
	for i := range v.Containers {
		c := &v.Containers[i]
		figure, what := c.PeakBytes, "peak"
		if figure == nil {
			figure, what = c.UsageBytes, "now"
		}
		if figure == nil {
			continue
		}
		g := gauge{Label: c.Name, FillText: what + " " + mebibytes(*figure)}
		scale := float64(*figure)
		if c.RequestBytes != nil {
			scale = max(scale, float64(*c.RequestBytes))
		}
		aria := c.Name + ": " + what + " " + mebibytes(*figure)
		if c.LimitBytes != nil {
			scale = max(scale, float64(*c.LimitBytes))
			g.End = "limit " + mebibytes(*c.LimitBytes)
			aria += " of a " + mebibytes(*c.LimitBytes) + " limit"
		} else {
			scale *= 1.25
			g.End = "no limit"
			aria += ", no limit"
		}
		if c.RequestBytes != nil {
			g.Tick = &tick{At: scaled(float64(*c.RequestBytes), scale), Text: "request " + mebibytes(*c.RequestBytes)}
			aria += ", request " + mebibytes(*c.RequestBytes)
		}
		g.Fill, g.Aria = scaled(float64(*figure), scale), aria
		r.Gauges = append(r.Gauges, g)
		if top == nil || share(c.PeakOfLimit, c.UsageOfLimit) > share(top.PeakOfLimit, top.UsageOfLimit) {
			top = c
		}
	}
	if v.Peak.Unavailable != "" {
		r.Notes = append(r.Notes, phrase{}.text("No peak figures: "+strings.TrimSuffix(v.Peak.Unavailable, ".")+"."))
	}
	if top == nil {
		return phrase{}.text("No figures."), nil
	}
	d := phrase{}
	from := v.Usage.Source
	if top.PeakBytes != nil {
		d, from = d.text("Peak ").value(mebibytes(*top.PeakBytes)), v.Peak.Source
	} else {
		d = d.text("Now ").value(mebibytes(*top.UsageBytes))
	}
	if top.LimitBytes != nil {
		d = d.text(" of ").value(mebibytes(*top.LimitBytes))
		if s := share(top.PeakOfLimit, top.UsageOfLimit); s > 0 {
			d = d.text(" (" + percent(s) + ")")
		}
	} else {
		d = d.text(", no limit")
	}
	if top.PeakBytes != nil && v.Window != "" {
		d = d.text(" over " + v.Window)
	}
	if len(v.Containers) > 1 {
		d = d.text(", in ").value(top.Name)
	}
	if from != "" {
		d = d.text(", from " + sourceName(from))
	}
	return d.text("."), nil
}

// share is the first of the shares that is known, or 0.
func share(shares ...*float64) float64 {
	for _, s := range shares {
		if s != nil {
			return *s
		}
	}
	return 0
}

func (r *readable) cpu(it evidence.Item) (phrase, error) {
	var v evidence.ContainerCPUView
	if err := json.Unmarshal(it.Data, &v); err != nil {
		return nil, err
	}
	var top, throttled *evidence.CPUUseView
	for i := range v.Containers {
		c := &v.Containers[i]
		if c.UsageCores != nil {
			g := gauge{Label: c.Name, FillText: "now " + millicores(*c.UsageCores)}
			scale := *c.UsageCores
			if c.RequestCores != nil {
				scale = max(scale, *c.RequestCores)
			}
			aria := c.Name + ": now " + millicores(*c.UsageCores)
			if c.LimitCores != nil {
				scale = max(scale, *c.LimitCores)
				g.End = "limit " + millicores(*c.LimitCores)
				aria += " of a " + millicores(*c.LimitCores) + " limit"
			} else {
				scale *= 1.25
				g.End = "no limit"
				aria += ", no limit"
			}
			if c.RequestCores != nil {
				g.Tick = &tick{At: scaled(*c.RequestCores, scale), Text: "request " + millicores(*c.RequestCores)}
				aria += ", request " + millicores(*c.RequestCores)
			}
			g.Fill, g.Aria = scaled(*c.UsageCores, scale), aria
			r.Gauges = append(r.Gauges, g)
			if top == nil || share(c.UsageOfLimit) > share(top.UsageOfLimit) {
				top = c
			}
		}
		// Throttling is its own figure: a share of all CPU periods.
		if c.ThrottledRatio != nil {
			end := "100% of periods"
			if v.Window != "" {
				end += " over " + v.Window
			}
			r.Gauges = append(r.Gauges, gauge{
				Label: c.Name, Fill: scaled(*c.ThrottledRatio, 1), FillText: "throttled " + percent(*c.ThrottledRatio), End: end,
				Aria: c.Name + ": throttled in " + percent(*c.ThrottledRatio) + " of its CPU periods over " + cmp.Or(v.Window, "the window"),
			})
			if throttled == nil || *c.ThrottledRatio > *throttled.ThrottledRatio {
				throttled = c
			}
		}
	}
	if v.Throttling.Unavailable != "" {
		r.Notes = append(r.Notes, phrase{}.text("No throttling figures: "+strings.TrimSuffix(v.Throttling.Unavailable, ".")+"."))
	}
	d := phrase{}
	if top != nil {
		d = d.text("Uses ").value(millicores(*top.UsageCores))
		if top.LimitCores != nil {
			d = d.text(" of ").value(millicores(*top.LimitCores))
			if top.UsageOfLimit != nil {
				d = d.text(" (" + percent(*top.UsageOfLimit) + ")")
			}
		} else {
			d = d.text(", no limit")
		}
		if len(v.Containers) > 1 {
			d = d.text(", in ").value(top.Name)
		}
		d = d.text(".")
	}
	if throttled != nil {
		if len(d) > 0 {
			d = d.text(" ")
		}
		d = d.text("Throttled in " + percent(*throttled.ThrottledRatio) + " of periods")
		if v.Window != "" {
			d = d.text(" over " + v.Window)
		}
		if src := v.Throttling.Source; src != "" {
			d = d.text(", from " + sourceName(src))
		}
		d = d.text(".")
	} else if top != nil && v.Throttling.Unavailable != "" {
		d = d.text(" No throttling figures.")
	}
	if len(d) == 0 {
		return phrase{}.text("No figures."), nil
	}
	return d, nil
}

func (r *readable) rollout(it evidence.Item) (phrase, error) {
	var v evidence.RolloutHistoryView
	if err := json.Unmarshal(it.Data, &v); err != nil {
		return nil, err
	}
	revs := slices.Clone(v.Revisions)
	slices.SortFunc(revs, func(a, b evidence.RevisionView) int { return cmp.Compare(number(b.Revision), number(a.Revision)) })
	for i, rev := range revs {
		row := revisionView{Revision: rev.Revision, Current: rev.Current, CreatedAt: rev.CreatedAt, ReplicaSet: rev.ReplicaSet}
		for _, c := range rev.Containers {
			row.Runs = append(row.Runs, runs(c, len(rev.Containers) > 1))
		}
		if i+1 < len(revs) {
			row.Changed = revisionChanges(revs[i+1], rev)
		} else {
			row.First = v.TotalRevisions <= len(v.Revisions)
		}
		r.Revisions = append(r.Revisions, row)
	}
	if v.TotalRevisions > len(v.Revisions) {
		r.Notes = append(r.Notes, phrase{}.text(fmt.Sprintf("Showing the newest %d of %d revisions.", len(v.Revisions), v.TotalRevisions)))
	}
	dep := v.Deployment
	r.Notes = append(r.Notes, phrase{}.text(fmt.Sprintf("%d of %d replicas ready.", dep.ReadyReplicas, dep.Replicas)))
	if dep.Paused {
		r.Notes = append(r.Notes, phrase{}.text("The rollout is paused."))
	}
	if len(revs) == 0 {
		return phrase{}.text("No revisions."), nil
	}
	if len(revs) == 1 {
		return phrase{}.text("1 revision. Created ").at(revs[0].CreatedAt).text("."), nil
	}
	newest := r.Revisions[0]
	d := phrase{}.text(fmt.Sprintf("%d revisions. Revision %s replaced %s", v.TotalRevisions, revs[0].Revision, revs[1].Revision))
	if !revs[0].CreatedAt.IsZero() {
		d = d.text(" at ").at(revs[0].CreatedAt)
	}
	d = d.text(".")
	if len(newest.Changed) > 0 {
		d = append(d.text(" "), newest.Changed[0]...)
	}
	return d, nil
}

func number(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// runs says what a container of a revision runs: its image, and its memory
// and CPU.
func runs(c evidence.TemplateContainerView, named bool) phrase {
	p := phrase{}
	if named {
		p = p.value(c.Name).text(": image ")
	} else {
		p = p.text("Image ")
	}
	p = p.value(c.Image).text(".")
	for _, res := range []string{"memory", "cpu"} {
		limit, request := c.Limits[res], c.Requests[res]
		if limit == "" && request == "" {
			continue
		}
		name := "Memory"
		if res == "cpu" {
			name = "CPU"
		}
		p = p.text(" " + name)
		if limit != "" {
			p = p.text(" limit ").value(limit)
			if request != "" {
				p = p.text(",")
			}
		}
		if request != "" {
			p = p.text(" request ").value(request)
		}
		p = p.text(".")
	}
	return p
}

// revisionChanges says what changed from one revision to the next, in the
// fields the rollout history keeps: images, memory and CPU, and probe
// timing. Other fields of the pod template, such as env, it doesn't keep.
func revisionChanges(before, after evidence.RevisionView) []phrase {
	var out []phrase
	was := map[string]evidence.TemplateContainerView{}
	for _, c := range before.Containers {
		was[c.Name] = c
	}
	for _, c := range after.Containers {
		old, ok := was[c.Name]
		if !ok {
			out = append(out, phrase{}.text("Container ").value(c.Name).text(" added."))
			continue
		}
		delete(was, c.Name)
		if old.Image != c.Image {
			out = append(out, phrase{}.text("The image of ").value(c.Name).text(" changed from ").value(old.Image).text(" to ").value(c.Image).text("."))
		}
		for _, kind := range []string{"limit", "request"} {
			olds, news := old.Limits, c.Limits
			if kind == "request" {
				olds, news = old.Requests, c.Requests
			}
			for _, res := range []string{"memory", "cpu"} {
				if olds[res] == news[res] {
					continue
				}
				what := map[string]string{"memory": "memory", "cpu": "CPU"}[res] + " " + kind
				p := phrase{}.text(strings.ToUpper(what[:1]) + what[1:] + " of ").value(c.Name)
				switch {
				case olds[res] == "":
					p = p.text(" set to ").value(news[res])
				case news[res] == "":
					p = p.text(" removed, from ").value(olds[res])
				default:
					p = p.text(" from ").value(olds[res]).text(" to ").value(news[res])
				}
				out = append(out, p.text("."))
			}
		}
		out = append(out, probeChanges(c.Name, old.Probes, c.Probes)...)
	}
	for _, name := range slices.Sorted(maps.Keys(was)) {
		out = append(out, phrase{}.text("Container ").value(name).text(" removed."))
	}
	return out
}

func probeChanges(container string, before, after map[string]evidence.ProbeView) []phrase {
	var out []phrase
	for _, kind := range []string{"liveness", "readiness", "startup"} {
		old, had := before[kind]
		p, has := after[kind]
		switch {
		case !had && has:
			out = append(out, phrase{}.text(cap1(kind)+" probe of ").value(container).text(" added."))
		case had && !has:
			out = append(out, phrase{}.text(cap1(kind)+" probe of ").value(container).text(" removed."))
		case had && has && old != p:
			fields := []struct {
				name       string
				was, after int32
				unit       string
			}{
				{"initial delay", old.InitialDelaySeconds, p.InitialDelaySeconds, "s"},
				{"period", old.PeriodSeconds, p.PeriodSeconds, "s"},
				{"timeout", old.TimeoutSeconds, p.TimeoutSeconds, "s"},
				{"failure threshold", old.FailureThreshold, p.FailureThreshold, ""},
				{"success threshold", old.SuccessThreshold, p.SuccessThreshold, ""},
			}
			for _, f := range fields {
				if f.was != f.after {
					out = append(out, phrase{}.text(cap1(kind)+" probe "+f.name+" of ").value(container).
						text(fmt.Sprintf(" from %d%s to %d%s.", f.was, f.unit, f.after, f.unit)))
				}
			}
		}
	}
	return out
}

func cap1(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (r *readable) config(it evidence.Item) (phrase, error) {
	var v evidence.ReferencedConfigView
	if err := json.Unmarshal(it.Data, &v); err != nil {
		return nil, err
	}
	counts := map[string]int{}
	var missing []string
	var latest configRow
	r.Config = []configRow{}
	for _, ref := range v.Refs {
		row := configRow{Kind: ref.Kind, Name: ref.Name, Optional: ref.Optional, Missing: ref.Missing, ChangedAt: ref.ChangedAt}
		r.Config = append(r.Config, row)
		counts[ref.Kind]++
		if ref.Missing && !ref.Optional {
			missing = append(missing, ref.Name)
		}
		if ref.ChangedAt.After(latest.ChangedAt) {
			latest = row
		}
	}
	if len(v.Refs) == 0 {
		return phrase{}.text("No ConfigMap or Secret referenced."), nil
	}
	var kinds []string
	if n := counts[evidence.ConfigKindConfigMap]; n > 0 {
		kinds = append(kinds, plural(n, "ConfigMap", "ConfigMaps"))
	}
	if n := counts[evidence.ConfigKindSecret]; n > 0 {
		kinds = append(kinds, plural(n, "Secret", "Secrets"))
	}
	d := phrase{}.text(strings.Join(kinds, ", ") + ".")
	switch {
	case len(missing) > 0:
		d = d.text(" ").value(missing[0]).text(" doesn't exist.")
	case !latest.ChangedAt.IsZero():
		d = d.text(" ").value(latest.Name).text(" last changed at ").at(latest.ChangedAt).text(".")
	}
	return d, nil
}

func (r *readable) fix(it evidence.Item) (phrase, error) {
	var v evidence.ThemisFixView
	if err := json.Unmarshal(it.Data, &v); err != nil {
		return nil, err
	}
	f := &fixRecordView{Incident: v.Incident, Done: v.Done, AppliedAt: v.AppliedAt, Outcome: v.Outcome}
	for _, found := range v.Found {
		f.Found = append(f.Found, found.Reason)
	}
	r.Fix = f
	d := phrase{}.text(cmp.Or(strings.TrimSuffix(v.Done, "."), "A fix") + ". Applied by Themis at ").at(v.AppliedAt).text(".")
	return d, nil
}

func (r *readable) health(it evidence.Item) (phrase, error) {
	var v evidence.RevisionHealthView
	if err := json.Unmarshal(it.Data, &v); err != nil {
		return nil, err
	}
	var d phrase
	for _, rec := range v.Revisions {
		line := phrase{}.text("Revision " + rec.Revision)
		if rec.Current {
			line = line.text(", the current one")
		}
		switch {
		case rec.Unavailable != "" && rec.SeenFrom.IsZero():
			line = line.text(". No history: " + strings.TrimSuffix(rec.Unavailable, ".") + ".")
		case rec.SeenFrom.IsZero():
			line = line.text(". The history has none of its pods.")
		default:
			line = line.text(", seen from ").at(rec.SeenFrom).text(" to ").at(rec.SeenUntil).text(".")
			if rec.Restarts > 0 {
				line = line.text(" " + plural(rec.Restarts, "restart", "restarts") + ", first at ").at(rec.FirstRestartAt).text(".")
			}
			if rec.OOMKilled > 0 {
				line = line.text(" " + plural(rec.OOMKilled, "OOM kill", "OOM kills") + ", first at ").at(rec.FirstOOMKillAt).text(".")
			}
			if rec.ThrottledRatio != nil && !rec.FirstThrottledAt.IsZero() {
				line = line.text(" Throttled in up to " + percent(*rec.ThrottledRatio) + " of CPU periods, from ").at(rec.FirstThrottledAt).text(".")
			}
			if rec.Restarts == 0 && rec.OOMKilled == 0 && rec.FirstThrottledAt.IsZero() {
				line = line.text(" No restart or OOM kill.")
			}
		}
		r.Health = append(r.Health, line)
		if len(d) > 0 {
			d = d.text(" ")
		}
		d = append(d, healthDigest(rec)...)
	}
	if src := sourceOf(v); src != "" {
		r.Notes = append(r.Notes, phrase{}.text("From "+sourceName(src)+"'s history."))
	}
	if len(d) == 0 {
		return phrase{}.text("No revisions."), nil
	}
	return d, nil
}

func sourceOf(v evidence.RevisionHealthView) string {
	for _, rec := range v.Revisions {
		if rec.Source != "" {
			return rec.Source
		}
	}
	return ""
}

// healthDigest is one revision's record in brief.
func healthDigest(rec evidence.RevisionRecordView) phrase {
	p := phrase{}.text("Revision " + rec.Revision + ": ")
	switch {
	case rec.SeenFrom.IsZero():
		return p.text("no history.")
	case rec.OOMKilled > 0:
		return p.text(plural(rec.OOMKilled, "OOM kill", "OOM kills") + ", first at ").at(rec.FirstOOMKillAt).text(".")
	case rec.Restarts > 0:
		return p.text(plural(rec.Restarts, "restart", "restarts") + ", first at ").at(rec.FirstRestartAt).text(".")
	case !rec.FirstThrottledAt.IsZero():
		return p.text("throttled from ").at(rec.FirstThrottledAt).text(".")
	}
	return p.text("no restart or OOM kill in " + roughly(rec.SeenUntil.Sub(rec.SeenFrom)) + ".")
}

// roughly writes a duration as people say it: 45s, 12m, 2h, 3d.
func roughly(d time.Duration) string {
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	}
	return strconv.Itoa(int(d.Hours()/24)) + "d"
}

// fields shows evidence Themis has no view for: each top-level field, with
// its value in brief.
func (r *readable) fields(it evidence.Item) phrase {
	var top map[string]jsontext.Value
	if err := json.Unmarshal(it.Data, &top); err != nil {
		r.Fields = []fieldRow{{Name: "Value", Value: brief(jsontext.Value(it.Data))}}
		return phrase{}.text("Not a JSON object.")
	}
	for _, k := range slices.Sorted(maps.Keys(top)) {
		r.Fields = append(r.Fields, fieldRow{Name: kindOf(words(k)), Value: brief(top[k])})
	}
	return phrase{}.text(plural(len(top), "field", "fields") + ".")
}

// words splits a JSON name into words: totalPods reads "total pods".
func words(name string) string {
	var b strings.Builder
	for i, r := range name {
		if i > 0 && unicode.IsUpper(r) {
			b.WriteRune(' ')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return strings.ReplaceAll(b.String(), "_", " ")
}

// brief is a JSON value in a few words: text and numbers as they are, a
// list or object by its size.
func brief(v jsontext.Value) string {
	switch v.Kind() {
	case '"':
		var s string
		if json.Unmarshal(v, &s) == nil {
			if r := []rune(s); len(r) > 120 {
				return string(r[:119]) + "…"
			}
			return s
		}
	case '[':
		var list []jsontext.Value
		if json.Unmarshal(v, &list) == nil {
			return plural(len(list), "item", "items")
		}
	case '{':
		var m map[string]jsontext.Value
		if json.Unmarshal(v, &m) == nil {
			return plural(len(m), "field", "fields")
		}
	case 'n':
		return "—"
	case 't':
		return "Yes"
	case 'f':
		return "No"
	case '0', 0, '}', ']':
	}
	return string(v)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
