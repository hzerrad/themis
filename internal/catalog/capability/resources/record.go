package resources

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/hzerrad/themis/internal/catalog/capability"
)

// record is what Themis keeps of a change to resources: each value it
// changes, as it was and as the fix sets it, and the hashes of the pod
// template before and after. Memory and CPU values are never secret, and
// nothing else of the template is kept.
type record struct {
	Values []kept `json:"values"`
	// Only says the pod template has one container.
	Only bool   `json:"only,omitempty"`
	From string `json:"from"`
	To   string `json:"to"`
}

// kept is one value a change to resources sets, as its record keeps it.
// An empty Before or After means not set.
type kept struct {
	Container string `json:"container"`
	Kind      string `json:"kind"`
	Resource  string `json:"resource"`
	Before    string `json:"before,omitempty"`
	After     string `json:"after,omitempty"`
}

// newRecord is the record of ds, the values a change from the template
// hashed from to the one hashed to sets, in a template of containers.
func newRecord(ds []diff, containers int, from, to string) record {
	values := make([]kept, 0, len(ds))
	for _, d := range ds {
		k := kept{Container: d.container, Kind: d.field.kind, Resource: string(d.field.resource)}
		if d.before != nil {
			k.Before = d.before.String()
		}
		if d.after != nil {
			k.After = d.after.String()
		}
		values = append(values, k)
	}
	return record{Values: values, Only: containers == 1, From: from, To: to}
}

// readRecord reads the record of a change to resources: ErrNoRecord when it
// has no hash of the template before.
func readRecord(raw json.RawMessage) (record, error) {
	var r record
	if err := json.Unmarshal(raw, &r); err != nil {
		return record{}, fmt.Errorf("%w: %w", capability.ErrNoRecord, err)
	}
	if r.From == "" {
		return record{}, capability.ErrNoRecord
	}
	return r, nil
}

// diffs are the values r keeps, as the fix compares them.
func (r record) diffs() ([]diff, error) {
	out := make([]diff, 0, len(r.Values))
	for _, k := range r.Values {
		d := diff{container: k.Container, field: field{k.Kind, corev1.ResourceName(k.Resource)}}
		var err error
		if d.before, err = unkept(k.Before); err != nil {
			return nil, err
		}
		if d.after, err = unkept(k.After); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// unkept is a value as its record keeps it, s, or nil when s is empty: not
// set.
func unkept(s string) (*resource.Quantity, error) {
	if s == "" {
		return nil, nil
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", capability.ErrNoRecord, err)
	}
	return &q, nil
}
