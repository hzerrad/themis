// Package evidence collects facts about an incident's target through named,
// read-only templates. Each answer is an Item with an ID that diagnoses cite.
package evidence

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/platform/redact"
	"github.com/hzerrad/themis/internal/workload"
)

var (
	ErrUnknownTemplate = errors.New("unknown evidence template")

	// ErrNoPods means the workload runs no pods to read from.
	ErrNoPods = errors.New("the workload has no pods")

	// ErrNoRevision means Kubernetes keeps no current revision of the
	// workload to read about.
	ErrNoRevision = errors.New("the workload has no current revision")

	// ErrPodReplaced means a pod was recreated under its name while it was read.
	ErrPodReplaced = errors.New("the pod was replaced")
)

// Classification is how sensitive an item is.
type Classification string

const (
	ClassificationInternal  Classification = "internal"
	ClassificationSensitive Classification = "sensitive"
)

// Item is one piece of evidence: a template's answer for one target.
type Item struct {
	ID       string
	Template string
	Target   incident.TargetRef

	// Data is the template's answer, as JSON.
	Data []byte

	// Truncated is set when the answer was cut to the template's bound.
	Truncated     bool
	OriginalBytes int
	Redactions    int
	// Suspicious is text in Data that looks written for the investigator,
	// such as instructions or forged tool output.
	Suspicious []Suspicion `json:",omitempty"`

	CollectedAt    time.Time
	Classification Classification
}

// Template is a named, read-only question about a workload. Collect fills in
// the answer; the registry fills in the metadata. The workload is already
// read and checked to be the incident's, and c reads anything else. The same
// cluster must give the same answer byte for byte: the investigator's replies
// are recorded against it.
type Template interface {
	Name() string
	// Description says what the template reads, for investigators; Params
	// names the parameters it takes, if any.
	Description() string
	Params() []string
	Collect(ctx context.Context, c client.Reader, w *workload.Workload, params map[string]string) (Item, error)
}

// Windowed is a template that reads over a window, which its window
// parameter sets, such as how often a container was throttled. MinWindow is
// the least that gives an answer worth reading.
type Windowed interface {
	MinWindow() time.Duration
}

// WindowParam is the parameter a Windowed template reads its window from.
const WindowParam = "window"

// Registry holds the templates Themis can run, by name.
type Registry map[string]Template

// NewRegistry returns a registry of the given templates.
func NewRegistry(templates ...Template) Registry {
	r := make(Registry, len(templates))
	for _, t := range templates {
		r[t.Name()] = t
		initMetrics(t.Name())
	}
	return r
}

// Collect runs the named template for w and redacts its answer.
func (r Registry) Collect(ctx context.Context, c client.Reader, name string, w *workload.Workload, params map[string]string, now time.Time) (Item, error) {
	tmpl, ok := r[name]
	if !ok {
		return Item{}, fmt.Errorf("%w: %q", ErrUnknownTemplate, name)
	}
	item, err := tmpl.Collect(ctx, c, w, params)
	if err != nil {
		return Item{}, fmt.Errorf("collect %s: %w", name, err)
	}
	item, err = finish(item, name, w.Target, now)
	if err != nil {
		return Item{}, err
	}
	countItem(item)
	return item, nil
}

// finish makes a template's answer an item about target, read at now: it
// gets its ID, and secrets in it are hidden and suspicious text flagged.
func finish(item Item, template string, target incident.TargetRef, now time.Time) (Item, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Item{}, err
	}
	var n int
	item.Data, n = redact.Apply(item.Data, redact.DefaultRules)
	item.Redactions += n
	item.Suspicious = Suspicions(item.Data)
	item.ID = id.String()
	item.Template = template
	item.Target = target
	item.CollectedAt = now
	if item.Classification == "" {
		item.Classification = ClassificationInternal
	}
	return item, nil
}

// Bundle is the evidence collected for one incident.
type Bundle struct {
	Target incident.TargetRef
	Items  []Item
}

// ByTemplate returns the bundle's items from the named template.
func (b Bundle) ByTemplate(name string) []Item {
	var items []Item
	for _, item := range b.Items {
		if item.Template == name {
			items = append(items, item)
		}
	}
	return items
}

// maxMessageBytes bounds free text that objects carry, such as condition and
// event messages: anyone allowed to write the object chooses it.
const maxMessageBytes = 256

// maxReasonBytes bounds short free text, such as an event's reason.
const maxReasonBytes = 128

// bounded cuts s to at most n bytes on a rune boundary, keeping it valid
// UTF-8.
func bounded(s string, n int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
