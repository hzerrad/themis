package probes

import (
	"encoding/json"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/incident"
)

var _ capability.Commanding = Capability{}

// Commands patch the probe settings the fix changes, by container name,
// which leaves the rest of each probe as it is.
func (Capability) Commands(t incident.TargetRef, params map[string]any, _ json.RawMessage) []string {
	req, err := parse(params, false)
	if err != nil {
		return nil
	}
	container := map[string]any{"name": req.container}
	for kind, values := range req.set {
		container[kind+"Probe"] = values
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{container}}}}})
	if err != nil {
		return nil
	}
	return []string{capability.Kubectl(t, "patch", "-p", capability.Quote(string(patch)))}
}
