package investigation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/catalog/evidence/hint"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/workload"
)

// MaxAsked bounds the evidence people may ask for of one incident: enough
// to look at every pod's logs and each window, few enough to keep its record
// small.
const MaxAsked = 20

// Asked collects the evidence a person asks for, through the same templates
// the investigator reads them through, so it is redacted, flagged and bounded
// the same way: only what the incident's profiles allow, one collection at a
// time per incident. Each piece joins the incident's evidence, and its checks
// are decided again with it, so what they found follows what was read. Its
// zero value is not ready: set Store, Config, Catalog and Reader.
type Asked struct {
	Store   *store.Store
	Config  config.Provider
	Catalog catalog.Catalog
	Reader  client.Reader
	// Remedy says why a reading failed and how to let Themis read it.
	Remedy hint.Writer
	Now    func() time.Time
}

// AskKind is why a person's request for evidence was refused.
type AskKind int

const (
	// AskInvalid is a request whose parameters its template refuses.
	AskInvalid AskKind = iota + 1
	// AskNotAllowed is a template none of the incident's profiles allows.
	AskNotAllowed
	// AskBusy is a request while another collection runs for the incident.
	AskBusy
	// AskClosed is an incident that takes no more evidence now.
	AskClosed
	// AskTooMany is a request past MaxAsked.
	AskTooMany
	// AskFailed is a reading that failed, such as access denied.
	AskFailed
)

// AskError is a request for evidence refused, and why, in plain words.
type AskError struct {
	Kind    AskKind
	Message string
}

func (e *AskError) Error() string { return e.Message }

func refuseAsk(k AskKind, format string, args ...any) error {
	return &AskError{Kind: k, Message: fmt.Sprintf(format, args...)}
}

// TakesEvidence says whether a person may ask for evidence of an incident in
// state: while it waits for a person or Themis investigates it, and once it
// is referred to one. Not while Themis decides or applies a fix, nor once it
// is closed.
func TakesEvidence(state incident.State) bool {
	return slices.Contains([]incident.State{
		incident.Investigating, incident.AwaitingSuggestion, incident.Inconclusive, incident.AwaitingApproval, incident.Recommended,
		incident.Escalated, incident.ContainedEscalated,
	}, state)
}

// Collect reads template with params for the incident, as person asked, and
// keeps it. It returns the item kept, and its number in the incident's
// evidence, from 1.
func (a *Asked) Collect(ctx context.Context, incidentID, person, template string, params map[string]string) (evidence.Item, int, error) {
	in, err := a.Store.Incident(ctx, incidentID)
	if err != nil {
		return evidence.Item{}, 0, err
	}
	if !TakesEvidence(in.State) {
		return evidence.Item{}, 0, refuseAsk(AskClosed, "Refused. %s", closedFor(in.State))
	}
	matched := a.Config.Current().Matched(in.Profiles())
	if !matched.AllowsTemplate(template) {
		return evidence.Item{}, 0, refuseAsk(AskNotAllowed, "Refused. %s", notAllowed(matched, in, template))
	}
	// The incident's evidence so far, which the checks read with the new
	// piece.
	before, err := a.Store.Evidence(ctx, in.ID)
	if err != nil {
		return evidence.Item{}, 0, err
	}
	results, err := a.Store.Predicates(ctx, in.ID)
	if err != nil {
		return evidence.Item{}, 0, err
	}
	var item evidence.Item
	err = a.Store.InTx(ctx, func(tx pgx.Tx) error {
		free, err := a.Store.LockCollection(ctx, tx, in.ID)
		if err != nil {
			return err
		}
		if !free {
			return refuseAsk(AskBusy, "Another collection is running for this incident. Try again once it ends.")
		}
		n, err := a.Store.CountRequestedEvidence(ctx, tx, in.ID)
		if err != nil {
			return err
		}
		if n >= MaxAsked {
			return refuseAsk(AskTooMany, "Refused. People asked for %d pieces of evidence of this incident, the most it keeps.", n)
		}
		if item, err = a.read(ctx, in, template, params); err != nil {
			return err
		}
		item.RequestedBy = person
		if err := a.Store.InsertEvidence(ctx, tx, in.ID, "", params, item); err != nil {
			return err
		}
		for _, r := range a.decideAgain(in, results, append(before, item)) {
			if err := a.Store.UpdatePredicate(ctx, tx, in.ID, r); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return evidence.Item{}, 0, err
	}
	return item, len(before) + 1, nil
}

// read reads the target, then runs the template for it. A reading that fails
// says why and how to let Themis read it.
func (a *Asked) read(ctx context.Context, in store.Incident, template string, params map[string]string) (evidence.Item, error) {
	w, err := workload.Read(ctx, a.Reader, in.Target)
	if err == nil {
		var item evidence.Item
		if item, err = a.Catalog.Evidence.Collect(ctx, a.Reader, template, w, params, a.now()); err == nil {
			return item, nil
		}
	}
	if errors.Is(err, evidence.ErrInvalidParams) {
		msg := strings.TrimPrefix(err.Error(), "collect "+template+": ")
		msg = strings.TrimPrefix(msg, evidence.ErrInvalidParams.Error()+": ")
		return evidence.Item{}, refuseAsk(AskInvalid, "Refused. %s.", upperFirst(strings.TrimSuffix(msg, ".")))
	}
	if ctx.Err() != nil {
		return evidence.Item{}, err
	}
	gap := store.EvidenceGap{IncidentID: in.ID, Template: template, Cause: evidence.CauseOf(err), Detail: err.Error(), At: a.now()}
	return evidence.Item{}, refuseAsk(AskFailed, "%s", a.Remedy.Message(in, gap))
}

// decideAgain decides the incident's checks again over items, as of now, and
// returns those whose answer changed.
func (a *Asked) decideAgain(in store.Incident, results []check.Result, items []evidence.Item) []check.Result {
	names := make([]string, 0, len(results))
	for _, r := range results {
		names = append(names, r.Name)
	}
	if len(names) == 0 {
		return nil
	}
	var changed []check.Result
	for _, r := range a.Catalog.Checks.For(names).Evaluate(evidence.Bundle{Target: in.Target, Items: items}, a.now()) {
		i := slices.IndexFunc(results, func(o check.Result) bool { return o.Name == r.Name })
		if i >= 0 && (r.Value != results[i].Value || r.Reason != results[i].Reason || r.Fact != results[i].Fact ||
			r.Headline != results[i].Headline || !slices.Equal(r.EvidenceIDs, results[i].EvidenceIDs)) {
			changed = append(changed, r)
		}
	}
	return changed
}

// closedFor says why an incident in state takes no more evidence now.
func closedFor(state incident.State) string {
	switch {
	case state == incident.Received:
		return "Themis is still reading the incident's first evidence."
	case state.Terminal():
		return "The incident is closed."
	case state == incident.Diagnosed || state == incident.Proposed:
		return "Themis is deciding a fix. Ask once it is decided."
	}
	return "Themis is applying or checking a fix. Ask once it is done."
}

// notAllowed says that none of the incident's profiles allows template.
func notAllowed(matched config.Matched, in store.Incident, template string) string {
	switch len(matched) {
	case 0:
		return "Themis no longer has " + config.Named(in.Profiles()) + " this incident matched."
	case 1:
		return "The " + matched[0].Name + " profile doesn't allow " + template + "."
	}
	return "None of the incident's profiles (" + strings.Join(matched.Names(), ", ") + ") allows " + template + "."
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (a *Asked) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}
