package governance

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"
)

// SignedIn, followed by a username, is the source of a fix suggested by
// someone signed in.
const SignedIn = "person:"

// ErrOwnSuggestion means the approver suggested the fix, and the install
// needs someone else to approve it.
var ErrOwnSuggestion = errors.New("a fix needs an approver other than the person who suggested it")

// Approve approves the action awaiting approval with digest, the one the
// approver was shown, and hands it to the executor. A person may approve a
// fix they suggested, and the timeline says so, unless the install requires
// another approver.
func Approve(ctx context.Context, s *store.Store, actionID, digest, approver string, now time.Time, anotherApprover bool) (store.Action, error) {
	var act store.Action
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		own, err := suggestedBy(ctx, s, tx, actionID, approver)
		if err != nil {
			return err
		}
		if own && anotherApprover {
			return ErrOwnSuggestion
		}
		act, err = s.Approve(ctx, tx, actionID, digest, approver, now)
		if err != nil {
			return err
		}
		why := "Approved by " + approver + "."
		if own {
			why = "Suggested and approved by " + approver + "."
		}
		err = s.Transition(ctx, tx, store.TransitionArgs{
			ID: act.IncidentID, From: incident.AwaitingApproval, To: incident.Authorized,
			Reason: why, Actor: approver,
		})
		if err != nil {
			return err
		}
		store.AfterCommit(tx, approvalsTotal.WithLabelValues(resultApproved).Inc)
		return s.EnqueueTx(ctx, tx, ExecuteArgs{ActionID: act.ID, IncidentID: act.IncidentID})
	})
	if err != nil {
		countRefusal(err)
	}
	return act, err
}

// suggestedBy reports whether person, signed in, suggested the fix that
// awaits approval as the action. One no longer awaiting approval is
// Approve's to refuse.
func suggestedBy(ctx context.Context, s *store.Store, tx pgx.Tx, actionID, person string) (bool, error) {
	act, err := s.Action(ctx, tx, actionID)
	if err != nil || act.State != store.ActionAwaitingApproval || person == "" {
		return false, err
	}
	p, err := s.Proposal(ctx, tx, act.ProposalID)
	if err != nil {
		return false, err
	}
	return p.LeaseID == "" && p.Provenance["source"] == SignedIn+person, nil
}

// Deny denies the action awaiting approval, keeping the approver's reason,
// if any, exactly as written. Its incident waits for another fix: one
// suggested, or what investigating it again finds.
func Deny(ctx context.Context, s *store.Store, actionID, approver, reason string, now time.Time) error {
	return deny(ctx, s, actionID, approver, reason, now, incident.AwaitingApproval, incident.AwaitingSuggestion)
}

// DenyAndClose denies the action awaiting approval as Deny does, and closes
// its incident with no change.
func DenyAndClose(ctx context.Context, s *store.Store, actionID, approver, reason string, now time.Time) error {
	return deny(ctx, s, actionID, approver, reason, now, incident.AwaitingApproval, incident.Recommended, incident.ClosedNoAction)
}

func deny(ctx context.Context, s *store.Store, actionID, approver, reason string, now time.Time, path ...incident.State) error {
	reason = strings.TrimSpace(reason)
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		act, err := s.Deny(ctx, tx, actionID, approver, reason, now)
		if err != nil {
			return err
		}
		if err := s.TransitionPath(ctx, tx, act.IncidentID, denied(approver, reason), approver, path...); err != nil {
			return err
		}
		store.AfterCommit(tx, approvalsTotal.WithLabelValues(resultDenied).Inc)
		return nil
	})
	if err != nil {
		countRefusal(err)
	}
	return err
}

// denied says who denied a fix and why, their reason as they wrote it:
// "Denied by alice. Reason: not during the sale."
func denied(approver, reason string) string {
	if reason == "" {
		return "Denied by " + approver + "."
	}
	// A reason that stops mid-sentence gets the full stop the sentence needs.
	if last, _ := utf8.DecodeLastRuneInString(reason); unicode.IsLetter(last) || unicode.IsDigit(last) {
		reason += "."
	}
	return "Denied by " + approver + ". Reason: " + reason
}

// ExpireArgs sweep approvals whose window passed.
type ExpireArgs struct{}

// Kind is persisted with each job and must never change.
func (ExpireArgs) Kind() string { return "expire_approvals" }

// ExpireWorker escalates incidents whose approval nobody gave in time.
type ExpireWorker struct {
	river.WorkerDefaults[ExpireArgs]

	Store *store.Store
	Now   func() time.Time
}

func (w *ExpireWorker) Work(ctx context.Context, _ *river.Job[ExpireArgs]) error {
	return w.Expire(ctx)
}

func (w *ExpireWorker) Expire(ctx context.Context) error {
	now := time.Now()
	if w.Now != nil {
		now = w.Now()
	}
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		expired, err := w.Store.ExpireApprovals(ctx, tx, now)
		if err != nil {
			return err
		}
		for _, a := range expired {
			store.AfterCommit(tx, approvalsTotal.WithLabelValues(resultExpired).Inc)
			err := w.Store.Transition(ctx, tx, store.TransitionArgs{
				ID: a.IncidentID, From: incident.AwaitingApproval, To: incident.Escalated,
				Reason: "Approval expired with no decision. Referred to a person.", Actor: "approval",
			})
			// An incident that already moved on keeps its state; the action
			// still expires.
			if err != nil && !errors.Is(err, store.ErrStateConflict) {
				return err
			}
		}
		return nil
	})
}
