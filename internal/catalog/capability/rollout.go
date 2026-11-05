package capability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/workload"
)

// rolloutStart is how the workload was when checking began.
type rolloutStart struct {
	Ready    int32 `json:"ready"`
	Restarts int32 `json:"restarts"`
}

// ReadRollout is Check for a fix that rolls out a new pod template: it reads
// the current revision's pods. The fix cured its symptom when the rollout is
// complete, every replica is ready, and no container has restarted or run
// out of memory since.
func ReadRollout(ctx context.Context, r client.Reader, t incident.TargetRef, mark string, from json.RawMessage, since time.Time) (Reading, error) {
	w, err := workload.Read(ctx, r, t)
	if errors.Is(err, workload.ErrGone) || errors.Is(err, workload.ErrReplaced) {
		return Reading{Changed: true, Health: Unchanged, Facts: err.Error() + ".", Start: from}, nil
	}
	if err != nil {
		return Reading{}, err
	}
	s, err := w.State(ctx, since)
	if err != nil {
		return Reading{}, err
	}
	base := rolloutStart{Ready: s.Ready, Restarts: s.Restarts}
	if from == nil {
		if from, err = json.Marshal(base); err != nil {
			return Reading{}, err
		}
	} else if err := json.Unmarshal(from, &base); err != nil {
		return Reading{}, fmt.Errorf("start of the check: %w", err)
	}
	restarts := max(s.Restarts-base.Restarts, 0)
	allReady := s.RolloutComplete && s.Ready >= s.Desired && s.OOMKills == 0
	health := Unchanged
	switch {
	case allReady && restarts == 0:
		health = Healthy
	case s.Ready < base.Ready:
		health = Worse
	case allReady:
		health = Improved
	}
	return Reading{
		Changed: w.TemplateHash != mark, Applied: s.Updated, Health: health,
		Facts: rolloutFacts(s, base, restarts), Start: from,
	}, nil
}

// rolloutFacts says what the pods are doing, in plain words.
func rolloutFacts(s workload.State, base rolloutStart, restarts int32) string {
	msg := fmt.Sprintf("%d of %d pods are ready", s.Ready, s.Desired)
	if s.Ready < base.Ready {
		msg += fmt.Sprintf(", down from %d", base.Ready)
	}
	if restarts > 0 {
		msg += "; containers restarted " + times(int(restarts))
	}
	if s.OOMKills > 0 {
		msg += "; containers ran out of memory " + times(s.OOMKills)
	}
	return msg + "."
}

func times(n int) string {
	if n == 1 {
		return "once"
	}
	return fmt.Sprintf("%d times", n)
}
