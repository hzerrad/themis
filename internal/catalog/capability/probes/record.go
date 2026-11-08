package probes

import (
	"encoding/json"
	"fmt"

	"github.com/hzerrad/themis/internal/catalog/capability"
)

// record is what Themis keeps of a change to probes: each timing field it
// changes, as it was and as the fix sets it, and the hashes of the pod
// template before and after. Probe timings are never secret, and nothing
// else of the template is kept.
type record struct {
	Edits []kept `json:"edits"`
	// Only says the pod template has one container.
	Only bool   `json:"only,omitempty"`
	From string `json:"from"`
	To   string `json:"to"`
}

// kept is one timing field a change to probes sets, as its record keeps it.
type kept struct {
	Container string `json:"container"`
	Probe     string `json:"probe"`
	Field     string `json:"field"`
	Before    int32  `json:"before"`
	After     int32  `json:"after"`
}

// newRecord is the record of edits, a change from the template hashed from
// to the one hashed to, in a template of containers.
func newRecord(edits []edit, containers int, from, to string) record {
	out := make([]kept, 0, len(edits))
	for _, e := range edits {
		out = append(out, kept{Container: e.container, Probe: e.kind, Field: e.field, Before: e.before, After: e.after})
	}
	return record{Edits: out, Only: containers == 1, From: from, To: to}
}

// readRecord reads the record of a change to probes: ErrNoRecord when it has
// no hash of the template before.
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

// edits are the fields r keeps, as the fix compares them.
func (r record) edits() []edit {
	out := make([]edit, 0, len(r.Edits))
	for _, k := range r.Edits {
		out = append(out, edit{container: k.Container, kind: k.Probe, field: k.Field, before: k.Before, after: k.After})
	}
	return out
}
