package evidence

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/hzerrad/themis/internal/workload"
)

// TemplateChangeView is what the current revision's pod template changed
// from the revision before it, which is what a rollback to that revision
// would undo. App is what changed in what the pods run: their containers,
// volumes and other pod settings. Metadata is what changed in the template's
// labels and annotations only, such as the annotation kubectl rollout restart
// writes: putting it back changes nothing the pods run. A value that may hold
// a secret, such as an environment variable's, a command or arguments, is
// named, never shown. Neither says what a ConfigMap or Secret holds: their
// content is no part of a revision, so a rollback doesn't put it back.
type TemplateChangeView struct {
	// From is the revision before the current one, and To the current one.
	From string `json:"from"`
	To   string `json:"to"`
	// App and Metadata each say one change in plain words, in a stable
	// order: what changed in each container, then in each init container,
	// then in the volumes, then in the other pod settings.
	App      []string `json:"app"`
	Metadata []string `json:"metadata"`
}

// maxChanges bounds each list of a TemplateChangeView.
const maxChanges = 20

// templateChange is what after changed from before, each change as the
// workload says it.
func templateChange(before, after corev1.PodTemplateSpec) (app, metadata []string) {
	a, m := workload.Changes(before, after)
	return bound(a), bound(m)
}

// bound keeps the first maxChanges changes, each cut to a message's length:
// anyone who may write the Deployment chooses its names and images.
func bound(changes []workload.Change) []string {
	out := make([]string, 0, min(len(changes), maxChanges))
	for _, c := range changes[:min(len(changes), maxChanges)] {
		out = append(out, bounded(c.Says, maxMessageBytes))
	}
	return out
}
