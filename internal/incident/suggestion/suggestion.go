// Package suggestion takes the fixes suggested for an incident, from the
// investigator, a caller outside Themis or a person: it judges each once,
// keeps it with why it was refused, if it was, and hands an accepted one to
// governance when the incident is waiting for it. The gRPC and HTTP APIs only
// say who is suggesting and translate.
package suggestion

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"golang.org/x/time/rate"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/governance"
	"github.com/hzerrad/themis/internal/platform/telemetry"
	"github.com/hzerrad/themis/internal/store"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

// Suggestion is a fix put forward for an incident.
type Suggestion struct {
	// Lease is the investigation lease the suggestion is made under; only the
	// investigator has one. Incident is the incident a suggester outside
	// Themis names.
	Lease    string
	Incident string

	// ClientID makes a retry store nothing new.
	ClientID string
	// Fix is the capability's name, and Params its parameters.
	Fix    string
	Params map[string]any
	Target incident.TargetRef

	ExpectedEffect string
	Rationale      string
	// Evidence and Checks are what it cites: evidence IDs and check names.
	Evidence   []string
	Checks     []string
	Provenance map[string]string
	// Notes are what the suggester looked at, for people only.
	Notes string

	// malformed says what the request lacks; invalid, why the parameters of
	// the older rollback form are wrong.
	malformed string
	invalid   string
}

const (
	// PersonSource names a person using the admin token. It is one shared
	// credential, so it proves the token, not which person.
	PersonSource = "admin-token"

	// SignedInSource, followed by a username, names someone signed in.
	SignedInSource = governance.SignedIn

	personPerMinute = 60
)

// Suggester is who suggests a fix, as Themis proved it.
type Suggester struct {
	// Source names a suggester outside Themis, from its proven identity. It
	// is empty for the investigator, which the suggestion's lease proves.
	Source string
	// Namespaces and Fixes are what an outside source may suggest for, and
	// suggest; All lifts both, for a person.
	Namespaces []string
	Fixes      []string
	All        bool
	// PerMinute bounds how often an outside source suggests.
	PerMinute int
}

// Investigator is the investigator, proven by the lease it suggests under.
var Investigator = Suggester{}

// Person is someone signed in as username, or the holder of the admin token
// when username is empty.
func Person(username string) Suggester {
	source := PersonSource
	if username != "" {
		source = SignedInSource + username
	}
	return Suggester{Source: source, PerMinute: personPerMinute, All: true}
}

func (s Suggester) outside() bool { return s.Source != "" }

// InScope reports whether the suggester may see incidents in namespace.
func (s Suggester) InScope(namespace string) bool {
	return s.All || slices.Contains(s.Namespaces, namespace)
}

// Kind is what is wrong with a suggestion that isn't kept.
type Kind int

const (
	// Invalid is a request that isn't well formed.
	Invalid Kind = iota + 1
	// Unauthenticated is the investigator naming an incident.
	Unauthenticated
	// Forbidden is a suggester outside Themis naming a lease.
	Forbidden
	// NotFound is an incident that doesn't exist, or is out of scope.
	NotFound
	// TooMany is a source suggesting too often, or too much.
	TooMany
	// Gone is an investigation whose profile is no longer configured.
	Gone
)

// Error is a suggestion turned away without being kept.
type Error struct {
	Kind    Kind
	Message string
}

func (e *Error) Error() string { return e.Message }

func refuse(k Kind, format string, args ...any) error {
	return &Error{Kind: k, Message: fmt.Sprintf(format, args...)}
}

// Inbox takes suggestions. Its zero value is not ready: set Store, Config
// and Fixes.
type Inbox struct {
	Store  *store.Store
	Config config.Provider
	// Fixes are what a suggestion may name.
	Fixes  capability.Registry
	Logger *slog.Logger
	Now    func() time.Time

	limiters sync.Map
	// decider says which fixes no policy forbids.
	decider governance.Decider
}

const (
	// actor is who moves an incident along when a suggestion is handed on.
	actor = "investigator"

	// maxExternalProposals bounds what one source can pile on an incident.
	maxExternalProposals = 20
	maxExternalText      = 4 << 10
	maxExternalCitations = 50
)

// Submit judges a suggestion from a suggester and keeps it, accepted or
// rejected with why. A suggestion from outside an incident waiting for one is
// handed to governance at once; the investigator's waits for its
// investigation to complete. An *Error says why nothing was kept.
func (b *Inbox) Submit(ctx context.Context, from Suggester, s Suggestion) (store.Proposal, error) {
	if from.outside() {
		return b.submitOutside(ctx, from, s)
	}
	if s.Incident != "" {
		return store.Proposal{}, refuse(Unauthenticated, "only a caller outside Themis names an incident")
	}
	if s.malformed != "" {
		return store.Proposal{}, refuse(Invalid, "%s", s.malformed)
	}
	var stored store.Proposal
	err := b.Store.InTx(ctx, func(tx pgx.Tx) error {
		inv, err := b.Store.LeasedInvestigation(ctx, tx, s.Lease, b.now())
		if err != nil {
			return err
		}
		matched := b.Config.Current().Matched(inv.Profiles)
		if len(matched) == 0 {
			return refuse(Gone, "no profile of this incident is configured any more (%s)", strings.Join(inv.Profiles, ", "))
		}
		p, invalid := b.proposal(inv.IncidentID, s)
		p.LeaseID = inv.LeaseID
		r, err := b.judge(ctx, tx, from, subject{incidentID: inv.IncidentID, target: inv.Target, profiles: matched, names: inv.Profiles}, p, invalid)
		if err != nil {
			return err
		}
		stored, _, err = b.keep(ctx, tx, p, r)
		return err
	})
	return stored, err
}

// submitOutside takes a suggestion from outside Themis. An incident outside
// the caller's scope looks missing, so it learns nothing and leaves nothing.
func (b *Inbox) submitOutside(ctx context.Context, from Suggester, s Suggestion) (store.Proposal, error) {
	if s.Lease != "" {
		return store.Proposal{}, refuse(Forbidden, "a lease belongs to the investigator")
	}
	if s.Incident == "" {
		return store.Proposal{}, refuse(Invalid, "incident_id is required")
	}
	if len(s.Rationale) > maxExternalText || len(s.ExpectedEffect) > maxExternalText || len(s.ClientID) > 256 ||
		len(s.Evidence) > maxExternalCitations || len(s.Checks) > maxExternalCitations {
		return store.Proposal{}, refuse(Invalid, "the suggestion is too long")
	}
	if s.malformed != "" {
		return store.Proposal{}, refuse(Invalid, "%s", s.malformed)
	}
	if !b.allow(from) {
		CountLimited("suggestions")
		return store.Proposal{}, refuse(TooMany, "too many suggestions: wait a minute and try again")
	}
	in, err := b.Store.Incident(ctx, s.Incident)
	if err == nil && !from.InScope(in.Target.Namespace) {
		b.logger().WarnContext(ctx, "a caller named an incident outside its scope", "source", from.Source, telemetry.FieldIncidentID, in.ID)
		return store.Proposal{}, refuse(NotFound, "no such incident")
	}
	if errors.Is(err, store.ErrNotFound) {
		return store.Proposal{}, refuse(NotFound, "no such incident")
	}
	if err != nil {
		return store.Proposal{}, err
	}

	s.Provenance = provenance(s.Provenance)
	s.Provenance["claimedSource"] = s.Provenance["source"]
	s.Provenance["source"] = from.Source

	var stored store.Proposal
	err = b.Store.InTx(ctx, func(tx pgx.Tx) error {
		state, err := b.Store.LockIncident(ctx, tx, in.ID)
		if err != nil {
			return err
		}
		n, err := b.Store.CountExternalProposals(ctx, tx, in.ID, from.Source)
		if err != nil {
			return err
		}
		if n >= maxExternalProposals {
			return refuse(TooMany, "this source has suggested too many fixes for this incident")
		}
		matched := b.Config.Current().Matched(in.Profiles())
		p, invalid := b.proposal(in.ID, s)
		r, err := b.judge(ctx, tx, from, subject{incidentID: in.ID, target: in.Target, profiles: matched, names: in.Profiles(), state: state}, p, invalid)
		if err != nil {
			return err
		}
		var created bool
		stored, created, err = b.keep(ctx, tx, p, r)
		if err != nil || !created {
			return err
		}
		store.AfterCommit(tx, externalProposalsTotal.WithLabelValues(result(r)).Inc)
		// While the investigation runs, it picks this up when it finishes.
		if stored.Status != store.ProposalAccepted || (state != incident.Inconclusive && state != incident.AwaitingSuggestion) {
			return nil
		}
		return b.handOver(ctx, tx, in.ID, stored.ID, "A fix was suggested by "+from.Source+".", state)
	})
	return stored, err
}

// proposal is s as stored for an incident, accepted until judged otherwise,
// with its parameters as its fix keeps them. invalid says why they are wrong.
func (b *Inbox) proposal(incidentID string, s Suggestion) (p store.Proposal, invalid string) {
	p = store.Proposal{
		IncidentID: incidentID, ClientProposalID: s.ClientID,
		Capability: s.Fix, Target: s.Target, Params: s.Params,
		ExpectedEffect: s.ExpectedEffect, Rationale: s.Rationale,
		CitedEvidenceIDs: s.Evidence, CitedPredicates: s.Checks,
		Provenance: provenance(s.Provenance), Notes: notes(s.Notes),
		// The lease or the caller's proven identity says who suggested it.
		Authenticated: true,
		Status:        store.ProposalAccepted,
	}
	f, ok := b.Fixes[s.Fix]
	if !ok {
		// Not a fix Themis knows: judging rejects it as not allowed.
		return p, ""
	}
	p.CapabilityVersion = f.Version()
	if s.invalid != "" {
		return p, s.invalid
	}
	normal, err := f.Validate(s.Params)
	if err != nil {
		return p, sentence(err)
	}
	p.Params = normal
	return p, ""
}

// keep stores p, rejected if judging returned why, with its safety event.
// What is new is counted once tx commits.
func (b *Inbox) keep(ctx context.Context, tx pgx.Tx, p store.Proposal, r *rejection) (store.Proposal, bool, error) {
	if r != nil {
		p.Status, p.RejectionReason, p.Message = store.ProposalRejected, r.reason.String(), r.message
	}
	stored, created, err := b.Store.InsertProposal(ctx, tx, p)
	if err != nil || !created {
		return stored, created, err
	}
	count(tx, r)
	if r == nil || r.reason != themisv1.RejectionReason_REJECTION_REASON_TARGET_MISMATCH {
		return stored, true, nil
	}
	return stored, true, b.Store.RecordSafetyEvent(ctx, tx, store.SafetyEvent{
		IncidentID: p.IncidentID, ProposalID: stored.ID, Kind: store.SafetyTargetMismatch, Detail: r.message,
	})
}

func (b *Inbox) allow(s Suggester) bool {
	key := fmt.Sprintf("%s/%d", s.Source, s.PerMinute)
	l, _ := b.limiters.LoadOrStore(key, rate.NewLimiter(rate.Limit(float64(s.PerMinute)/60), s.PerMinute))
	return l.(*rate.Limiter).Allow() //nolint:forcetypeassert // only limiters are stored
}

func (b *Inbox) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func (b *Inbox) logger() *slog.Logger {
	if b.Logger != nil {
		return b.Logger
	}
	return slog.Default()
}

// provenance is a copy of p with every key Themis keeps.
func provenance(p map[string]string) map[string]string {
	out := map[string]string{"source": "", "model": "", "promptVersion": "", "route": ""}
	for k, v := range p {
		out[k] = v
	}
	return out
}

// maxNotes bounds what a suggester may say it looked at.
const maxNotes = 8 << 10

// notes keeps at most maxNotes of valid text, cut at a character.
func notes(s string) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) <= maxNotes {
		return s
	}
	cut := maxNotes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + " …(cut)"
}
