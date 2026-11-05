package investigation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/suggestion"
	"github.com/hzerrad/themis/internal/platform/telemetry"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/workload"
)

// DefaultLeaseFor is how long a lease lasts without renewal.
const DefaultLeaseFor = time.Minute

// investigator is who moves an incident along while it is investigated.
const investigator = "investigator"

// Leases hands prepared investigations to an investigator, one lease at a
// time; reads the evidence it asks for within its profile and budget; and
// ends each with the state and job its outcome leads to. A lease that
// expired on the last attempt is given up on the next time one is handed
// out. Its zero value is not ready: set Store, Config, Evidence, Reader and
// Suggestions.
type Leases struct {
	Store  *store.Store
	Config config.Provider
	// Evidence is the templates an investigator may ask for.
	Evidence evidence.Registry
	Reader   client.Reader
	// Suggestions takes the fixes suggested, and hands the next to governance.
	Suggestions *suggestion.Inbox
	Logger      *slog.Logger
	Now         func() time.Time
	// For is how long a lease lasts without renewal; DefaultLeaseFor when zero.
	For time.Duration
}

// Kind is why a request under a lease was refused.
type Kind int

const (
	// Gone is an investigation whose profile is no longer configured.
	Gone Kind = iota + 1
	// NotAllowed is evidence the investigation's profile doesn't allow.
	NotAllowed
	// BudgetSpent is evidence asked for once the budget is spent.
	BudgetSpent
	// Invalid is evidence asked for with parameters its template refuses.
	Invalid
)

// Error is a request under a lease that was refused, and why. A lease that is
// unknown, expired or superseded is store.ErrLeaseLost instead.
type Error struct {
	Kind    Kind
	Message string
}

func (e *Error) Error() string { return e.Message }

func refuse(k Kind, format string, args ...any) error {
	return &Error{Kind: k, Message: fmt.Sprintf(format, args...)}
}

// Leased is an investigation handed to an investigator, with what it starts
// from.
type Leased struct {
	store.Investigation
	// Scope is the incident's profile: what the investigator may read and
	// suggest, and its budget.
	Scope config.Profile
	// Evidence and Results are the initial evidence and the checks' answers.
	Evidence []evidence.Item
	Results  []check.Result
	// Deadline is when the investigation's time runs out.
	Deadline time.Time
}

// Lease hands the next investigation to worker, first giving up on those
// whose last lease expired. It reports false when there is none.
func (l *Leases) Lease(ctx context.Context, worker string) (Leased, bool, error) {
	now := l.now()
	var (
		inv store.Investigation
		ok  bool
	)
	err := l.Store.InTx(ctx, func(tx pgx.Tx) error {
		expired, err := l.Store.ExpireInvestigations(ctx, tx, now)
		if err != nil {
			return err
		}
		for _, id := range expired {
			if err := l.abandon(ctx, tx, id, "The investigator stopped responding during its last attempt."); err != nil {
				return err
			}
		}
		inv, ok, err = l.Store.LeaseInvestigation(ctx, tx, worker, now, l.leaseFor())
		return err
	})
	if err != nil || !ok {
		return Leased{}, false, err
	}
	profile, err := l.profile(inv)
	if err != nil {
		return Leased{}, false, err
	}
	items, err := l.Store.Evidence(ctx, inv.IncidentID)
	if err != nil {
		return Leased{}, false, err
	}
	results, err := l.Store.Predicates(ctx, inv.IncidentID)
	if err != nil {
		return Leased{}, false, err
	}
	return Leased{
		Investigation: inv, Scope: profile, Evidence: items, Results: results,
		Deadline: now.Add(profile.InvestigationBudget().Timeout.Duration),
	}, true, nil
}

// Renew extends a live lease, and returns its new deadline.
func (l *Leases) Renew(ctx context.Context, leaseID string) (time.Time, error) {
	var deadline time.Time
	err := l.Store.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		deadline, err = l.Store.RenewLease(ctx, tx, leaseID, l.now(), l.leaseFor())
		return err
	})
	return deadline, err
}

// Collected is one more piece of evidence read under a lease, or why it
// couldn't be read.
type Collected struct {
	Item evidence.Item
	// Failed says why the read failed; Item is empty then.
	Failed error
	// Target is what the evidence is about.
	Target incident.TargetRef
	// Remaining is how many more pieces the investigation may ask for.
	Remaining int
}

// Collect reads one more piece of evidence for the investigation leaseID
// holds, within its profile and budget, and keeps it. Evidence refused or not
// read is a gap.
func (l *Leases) Collect(ctx context.Context, leaseID, template string, params map[string]string) (Collected, error) {
	var (
		inv       store.Investigation
		remaining int
	)
	// Check before reading the cluster, and again before storing: the lease
	// may expire in between.
	check := func(tx pgx.Tx) error {
		var err error
		inv, err = l.Store.LeasedInvestigation(ctx, tx, leaseID, l.now())
		if err != nil {
			return err
		}
		profile, err := l.profile(inv)
		if err != nil {
			return err
		}
		if !profile.AllowsTemplate(template) {
			return refuse(NotAllowed, "template %q is not allowed for profile %s", template, profile.Name)
		}
		used, err := l.Store.CountLeaseEvidence(ctx, tx, inv.LeaseID)
		if err != nil {
			return err
		}
		remaining = profile.InvestigationBudget().MaxEvidenceRequests - used
		if remaining <= 0 {
			return refuse(BudgetSpent, "the evidence budget is spent")
		}
		return nil
	}
	if err := l.Store.InTx(ctx, check); err != nil {
		l.refused(ctx, inv, template, params, err)
		return Collected{}, err
	}

	item, err := l.read(ctx, template, inv.Target, params)
	if err != nil {
		if errors.Is(err, evidence.ErrInvalidParams) {
			return Collected{}, &Error{Kind: Invalid, Message: err.Error()}
		}
		l.gap(ctx, inv, template, params, evidence.CauseOf(err), err.Error())
		return Collected{Failed: err, Target: inv.Target, Remaining: remaining}, nil
	}

	err = l.Store.InTx(ctx, func(tx pgx.Tx) error {
		if err := check(tx); err != nil {
			return err
		}
		return l.Store.InsertEvidence(ctx, tx, inv.IncidentID, inv.LeaseID, params, item)
	})
	if err != nil {
		l.refused(ctx, inv, template, params, err)
		return Collected{}, err
	}
	return Collected{Item: item, Target: inv.Target, Remaining: remaining - 1}, nil
}

// read reads the target, then runs the template for it.
func (l *Leases) read(ctx context.Context, template string, t incident.TargetRef, params map[string]string) (evidence.Item, error) {
	w, err := workload.Read(ctx, l.Reader, t)
	if err != nil {
		return evidence.Item{}, err
	}
	return l.Evidence.Collect(ctx, l.Reader, template, w, params, l.now())
}

// refusals are the refusals that keep evidence from an investigation.
var refusals = map[Kind]evidence.GapCause{
	NotAllowed:  evidence.GapNotInProfile,
	BudgetSpent: evidence.GapBudgetSpent,
}

// refused records evidence the profile or the budget kept from the
// investigation. The check rolled back with its transaction, so the gap is
// recorded in another.
func (l *Leases) refused(ctx context.Context, inv store.Investigation, template string, params map[string]string, err error) {
	var r *Error
	if !errors.As(err, &r) {
		return
	}
	if cause, ok := refusals[r.Kind]; ok {
		l.gap(ctx, inv, template, params, cause, "")
	}
}

// gap keeps why evidence is missing. Failing to keep it does not change the
// investigator's answer, only what the incident can tell later.
func (l *Leases) gap(ctx context.Context, inv store.Investigation, template string, params map[string]string, cause evidence.GapCause, detail string) {
	// A lost lease has no investigation to attribute the gap to.
	if inv.IncidentID == "" {
		return
	}
	g := store.EvidenceGap{
		IncidentID: inv.IncidentID, LeaseID: inv.LeaseID, Template: template, Params: params,
		Cause: cause, Detail: detail, At: l.now(),
	}
	err := l.Store.InTx(ctx, func(tx pgx.Tx) error {
		_, err := recordGap(ctx, l.Store, tx, l.Evidence, g)
		return err
	})
	if err != nil {
		l.logger().WarnContext(ctx, "cannot record why evidence is missing", "cause", string(cause), telemetry.FieldError, err)
	}
}

// profile returns the configured profile of an investigation.
func (l *Leases) profile(inv store.Investigation) (config.Profile, error) {
	p, ok := l.Config.Current().Profile(inv.Profile)
	if !ok {
		return config.Profile{}, refuse(Gone, "profile %q is no longer configured", inv.Profile)
	}
	return p, nil
}

func (l *Leases) leaseFor() time.Duration {
	if l.For > 0 {
		return l.For
	}
	return DefaultLeaseFor
}

func (l *Leases) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Leases) logger() *slog.Logger {
	if l.Logger != nil {
		return l.Logger
	}
	return slog.Default()
}
