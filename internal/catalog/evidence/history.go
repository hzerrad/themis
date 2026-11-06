package evidence

import (
	"encoding/json/v2"

	"github.com/hzerrad/themis/internal/catalog/evidence/metrics"
)

// sourceNames are the metrics sources as people name them.
var sourceNames = map[string]string{metrics.Prometheus: "Prometheus", metrics.Kubernetes: "the Kubernetes metrics API"}

// History says, for people, which source gave the figures over a window the
// item holds, and over which window: "Prometheus's container CPU history
// over 1h", from the throttling of container_cpu, or the peaks of
// container_memory. It is empty when the item holds none.
func (it Item) History() string {
	var (
		window, what string
		of           ReadingView
	)
	switch it.Template {
	case ContainerCPU:
		var cpu ContainerCPUView
		if json.Unmarshal(it.Data, &cpu) != nil {
			return ""
		}
		window, of, what = cpu.Window, cpu.Throttling, "container CPU"
	case ContainerMemory:
		var memory ContainerMemoryView
		if json.Unmarshal(it.Data, &memory) != nil {
			return ""
		}
		window, of, what = memory.Window, memory.Peak, "container memory"
	default:
		return ""
	}
	if of.Source == "" || of.Unavailable != "" {
		return ""
	}
	source, ok := sourceNames[of.Source]
	if !ok {
		source = of.Source
	}
	out := source + "'s " + what + " history"
	if window != "" {
		out += " over " + window
	}
	return out
}
