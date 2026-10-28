package predicate

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hzerrad/themis/internal/evidence"
)

const (
	NameRolloutRecent            = "rollout_recent"
	NameFailureOnsetAfterRollout = "failure_onset_after_rollout"
	NamePriorRevisionWasHealthy  = "prior_revision_was_healthy"
	NameOOMKilledNewRevision     = "oom_killed_new_revision"
	NameGitOpsManaged            = "gitops_managed"
)

// gitOpsManagers are field managers of GitOps controllers.
var gitOpsManagers = []string{"argocd-controller", "argocd-application-controller", "kustomize-controller", "helm-controller"}

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

// RolloutRecent: the current revision was created within window of now.
func RolloutRecent(b evidence.Bundle, window time.Duration, now time.Time) Result {
	var h evidence.RolloutHistoryView
	id, ok := latest(b, evidence.RolloutHistory, &h)
	if !ok {
		return result(NameRolloutRecent, Unknown, "no rollout history")
	}
	cur, ok := currentRevision(h)
	at := rolloutAt(h, cur)
	if !ok || at.IsZero() {
		return result(NameRolloutRecent, Unknown, "the current revision or its rollout time is unknown")
	}
	age := now.Sub(at)
	if age <= window {
		return result(NameRolloutRecent, True, fmt.Sprintf("revision %s rolled out %s ago", cur.Revision, age.Round(time.Second)), id)
	}
	return result(NameRolloutRecent, False, fmt.Sprintf("revision %s rolled out %s ago, beyond %s", cur.Revision, age.Round(time.Second), window), id)
}

// failure is a sign of trouble: a warning event or a container that died.
type failure struct {
	at   time.Time
	what string
	id   string
}

func failures(b evidence.Bundle) []failure {
	var fs []failure
	var ev evidence.TargetEventsView
	if id, ok := latest(b, evidence.TargetEvents, &ev); ok {
		for _, e := range ev.Events {
			if e.Type == "Warning" && strings.HasPrefix(e.Object, "Pod/") {
				fs = append(fs, failure{e.FirstSeen, e.Reason + " on " + e.Object, id})
			}
		}
	}
	var ps evidence.PodStatesView
	if id, ok := latest(b, evidence.PodStates, &ps); ok {
		for _, p := range ps.Pods {
			for _, c := range p.Containers {
				if t := c.LastTermination; t != nil && t.Reason != "Completed" {
					fs = append(fs, failure{t.FinishedAt, t.Reason + " of " + p.Name + "/" + c.Name, id})
				}
			}
		}
	}
	return fs
}

// FailureOnsetAfterRollout: the first failure came after the current
// revision was created. A failure without a time could be earlier.
func FailureOnsetAfterRollout(b evidence.Bundle) Result {
	var h evidence.RolloutHistoryView
	hid, ok := latest(b, evidence.RolloutHistory, &h)
	if !ok {
		return result(NameFailureOnsetAfterRollout, Unknown, "no rollout history")
	}
	cur, ok := currentRevision(h)
	at := rolloutAt(h, cur)
	if !ok || at.IsZero() {
		return result(NameFailureOnsetAfterRollout, Unknown, "the rollout time is unknown")
	}
	fs := failures(b)
	if len(fs) == 0 {
		return result(NameFailureOnsetAfterRollout, Unknown, "no failure observed")
	}
	ids := []string{hid}
	undated := 0
	for _, f := range fs {
		if !slices.Contains(ids, f.id) {
			ids = append(ids, f.id)
		}
		if f.at.IsZero() {
			undated++
			continue
		}
		if f.at.Before(at) {
			return result(NameFailureOnsetAfterRollout, False,
				fmt.Sprintf("%s at %s predates revision %s at %s", f.what, f.at.Format(time.RFC3339), cur.Revision, at.Format(time.RFC3339)), hid, f.id)
		}
	}
	if undated > 0 {
		return result(NameFailureOnsetAfterRollout, Unknown, fmt.Sprintf("%d failures have no time", undated))
	}
	return result(NameFailureOnsetAfterRollout, True,
		fmt.Sprintf("all %d failures follow revision %s at %s", len(fs), cur.Revision, at.Format(time.RFC3339)), ids...)
}

func failed(c evidence.ContainerView) bool {
	return c.LastTermination != nil && c.LastTermination.Reason != "Completed"
}

// PriorRevisionWasHealthy: the revision before the current one runs ready
// pods that never failed.
func PriorRevisionWasHealthy(b evidence.Bundle) Result {
	var h evidence.RolloutHistoryView
	hid, ok := latest(b, evidence.RolloutHistory, &h)
	if !ok {
		return result(NamePriorRevisionWasHealthy, Unknown, "no rollout history")
	}
	cur, ok := currentRevision(h)
	if !ok {
		return result(NamePriorRevisionWasHealthy, Unknown, "the current revision is unknown")
	}
	prior, ok := priorRevision(h, cur)
	if !ok {
		return result(NamePriorRevisionWasHealthy, Unknown, "there is no prior revision")
	}

	var ps evidence.PodStatesView
	pid, ok := latest(b, evidence.PodStates, &ps)
	if !ok {
		return result(NamePriorRevisionWasHealthy, Unknown, "no pod states")
	}
	seen := 0
	for _, p := range ps.Pods {
		if p.Revision != prior.Revision {
			continue
		}
		seen++
		for _, c := range p.Containers {
			if failed(c) {
				return result(NamePriorRevisionWasHealthy, False,
					fmt.Sprintf("pod %s of revision %s: %s %s", p.Name, prior.Revision, c.Name, c.LastTermination.Reason), hid, pid)
			}
		}
		if !p.Ready {
			return result(NamePriorRevisionWasHealthy, False, fmt.Sprintf("pod %s of revision %s is not ready", p.Name, prior.Revision), hid, pid)
		}
	}
	if seen == 0 {
		return result(NamePriorRevisionWasHealthy, Unknown, fmt.Sprintf("revision %s has no pods left to judge", prior.Revision))
	}
	return result(NamePriorRevisionWasHealthy, True,
		fmt.Sprintf("%d pods of revision %s are ready and never failed", seen, prior.Revision), hid, pid)
}

// OOMKilledNewRevision: a container of the current revision was OOMKilled.
func OOMKilledNewRevision(b evidence.Bundle) Result {
	var ps evidence.PodStatesView
	pid, ok := latest(b, evidence.PodStates, &ps)
	if !ok {
		return result(NameOOMKilledNewRevision, Unknown, "no pod states")
	}
	revision := ""
	var h evidence.RolloutHistoryView
	if _, ok := latest(b, evidence.RolloutHistory, &h); ok {
		if cur, ok := currentRevision(h); ok {
			revision = cur.Revision
		}
	}
	if revision == "" {
		// Without history, the newest revision among the pods is current.
		for _, p := range ps.Pods {
			if n, err := strconv.Atoi(p.Revision); err == nil {
				if m, _ := strconv.Atoi(revision); n > m {
					revision = p.Revision
				}
			}
		}
	}

	seen := 0
	for _, p := range ps.Pods {
		if p.Revision != revision {
			continue
		}
		seen++
		for _, c := range p.Containers {
			for _, s := range []*evidence.StateView{c.State, c.LastTermination} {
				if s != nil && s.Reason == "OOMKilled" {
					return result(NameOOMKilledNewRevision, True, fmt.Sprintf("%s/%s of revision %s was OOMKilled", p.Name, c.Name, revision), pid)
				}
			}
		}
	}
	if seen == 0 {
		return result(NameOOMKilledNewRevision, Unknown, "no pods of the current revision")
	}
	return result(NameOOMKilledNewRevision, False, fmt.Sprintf("none of the %d pods of revision %s was OOMKilled", seen, revision), pid)
}

// GitOpsManaged: the target carries GitOps markers or is written by a GitOps
// controller.
func GitOpsManaged(b evidence.Bundle) Result {
	var h evidence.RolloutHistoryView
	id, ok := latest(b, evidence.RolloutHistory, &h)
	if !ok {
		return result(NameGitOpsManaged, Unknown, "no rollout history")
	}
	for k := range h.Deployment.Markers {
		return result(NameGitOpsManaged, True, "the target carries "+k, id)
	}
	for _, m := range h.Deployment.Managers {
		if slices.Contains(gitOpsManagers, m) {
			return result(NameGitOpsManaged, True, "the target is managed by "+m, id)
		}
	}
	return result(NameGitOpsManaged, False, "no GitOps markers or managers", id)
}
