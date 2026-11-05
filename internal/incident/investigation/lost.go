package investigation

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/workload"
)

// TargetLost ends an incident whose target was lost before Themis changed
// anything, saying exactly what happened: a deleted workload cancels it, one
// replaced under the same name supersedes it with an incident for the new
// object, and an unsupported kind rejects it. Any other err is returned as is.
// A move from a state the incident has left fails with store.ErrStateConflict.
func TargetLost(ctx context.Context, s *store.Store, tx pgx.Tx, in store.Incident, err error, actor string) error {
	var lost *workload.Error
	if !errors.As(err, &lost) {
		return err
	}
	reason := lost.Error() + "."
	switch {
	case errors.Is(lost, workload.ErrReplaced):
		now := in.Target
		now.UID = lost.Found
		return Supersede(ctx, s, tx, in, now, "Replaced by a new incident for the new object: "+reason, actor)
	case errors.Is(lost, workload.ErrUnsupported) && incident.CanTransition(in.State, incident.Rejected):
		return end(ctx, s, tx, in, incident.Rejected, reason, actor)
	default:
		return end(ctx, s, tx, in, incident.Canceled, reason, actor)
	}
}

// Supersede cancels the incident and opens another for target, to be
// investigated from scratch, carrying over the alerts still firing.
func Supersede(ctx context.Context, s *store.Store, tx pgx.Tx, in store.Incident, target incident.TargetRef, reason, actor string) error {
	if err := end(ctx, s, tx, in, incident.Canceled, reason, actor); err != nil {
		return err
	}
	id, created, err := s.CreateOrJoinIncident(ctx, tx, store.IncidentArgs{
		Fingerprint: incident.Fingerprint(in.Profile, target), Profile: in.Profile, AlsoMatched: in.AlsoMatched, Target: target,
		Payload: fmt.Appendf(nil, `{"supersedes":%q}`, in.ID),
	})
	if err != nil {
		return err
	}
	if created {
		if err := s.EnqueueTx(ctx, tx, InvestigateArgs{IncidentID: id}); err != nil {
			return err
		}
	}
	return s.Supersede(ctx, tx, in.ID, id)
}

func end(ctx context.Context, s *store.Store, tx pgx.Tx, in store.Incident, to incident.State, reason, actor string) error {
	if err := s.Transition(ctx, tx, store.TransitionArgs{ID: in.ID, From: in.State, To: to, Reason: reason, Actor: actor}); err != nil {
		return err
	}
	return s.CancelPendingActions(ctx, tx, in.ID)
}
