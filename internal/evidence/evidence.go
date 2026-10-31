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
)

var (
	ErrUnknownTemplate   = errors.New("unknown evidence template")
	ErrUnsupportedTarget = errors.New("target kind not supported by the template")

	// ErrTargetGone includes a target recreated under the same name.
	ErrTargetGone = errors.New("target no longer exists")
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

	CollectedAt    time.Time
	Classification Classification
}

// Template is a named, read-only question about a target. Collect fills in
// the answer; the registry fills in the metadata.
type Template interface {
	Name() string
	Collect(ctx context.Context, c client.Reader, t incident.TargetRef, params map[string]string) (Item, error)
}

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

// Collect runs the named template for t and redacts its answer.
func (r Registry) Collect(ctx context.Context, c client.Reader, name string, t incident.TargetRef, params map[string]string, now time.Time) (Item, error) {
	tmpl, ok := r[name]
	if !ok {
		return Item{}, fmt.Errorf("%w: %q", ErrUnknownTemplate, name)
	}
	item, err := tmpl.Collect(ctx, c, t, params)
	if err != nil {
		return Item{}, fmt.Errorf("collect %s: %w", name, err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Item{}, err
	}
	var n int
	item.Data, n = Redact(item.Data, DefaultRules)
	item.Redactions += n
	item.ID = id.String()
	item.Template = name
	item.Target = t
	item.CollectedAt = now
	if item.Classification == "" {
		item.Classification = ClassificationInternal
	}
	countItem(item)
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
