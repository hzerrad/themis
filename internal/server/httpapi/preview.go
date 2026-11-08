package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/incident/governance/policy"
	"github.com/hzerrad/themis/internal/incident/suggestion"
	"github.com/hzerrad/themis/internal/platform/telemetry"
	"github.com/hzerrad/themis/internal/server/identity"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

// Previews say what Themis would do with a fix a person suggests, judging and
// deciding it exactly as a suggestion is, and keeping nothing.
type Previews interface {
	Preview(ctx context.Context, from suggestion.Suggester, s suggestion.Suggestion) (suggestion.Preview, error)
}

// previewView is what Themis would do with a suggested fix, as people read
// it: the verdict, the checks for and against it with their figures, and
// what policy does with it then.
type previewView struct {
	Accepted bool `json:"accepted"`
	// Verdict says it in one sentence: "Themis would accept this raise."
	Verdict string `json:"verdict"`
	// Reason is why Themis would refuse it, in the words of the refusal.
	Reason string `json:"reason,omitempty"`
	// Note is what else the fix says of its reach, once worked out against
	// the cluster: "Keeps the rollout's new image."
	Note    string           `json:"note,omitempty"`
	For     []previewFinding `json:"for,omitempty"`
	Against []previewFinding `json:"against,omitempty"`
	// Then says what policy does with a fix Themis would accept.
	Then string `json:"then,omitempty"`
}

// previewFinding is what a check found, and its reason.
type previewFinding struct {
	Says   string `json:"says"`
	Reason string `json:"reason,omitempty"`
}

// preview takes the same request as suggest, and says what Themis would do
// with it.
func (a *API) preview(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "The request isn't valid: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req themisv1.SubmitProposalRequest
	if err := protojson.Unmarshal(body, &req); err != nil {
		http.Error(w, "The request isn't valid: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.IncidentId = r.PathValue("id")
	person := ""
	if who, _ := identity.From(r.Context()); !who.Token {
		person = who.Username
	}
	pv, err := a.Previews.Preview(r.Context(), suggestion.Person(person), suggestion.Request(&req))
	var refused *suggestion.Error
	switch {
	case errors.As(err, &refused):
		http.Error(w, refused.Message, suggestionStatuses[refused.Kind])
		return
	case err != nil && r.Context().Err() != nil:
		http.Error(w, r.Context().Err().Error(), http.StatusServiceUnavailable)
		return
	case err != nil:
		a.logger().ErrorContext(r.Context(), "a preview failed", telemetry.FieldError, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.reply(w, r, a.viewPreview(pv))
}

// viewPreview words a preview: the same refusals and decisions a suggestion
// gets, and the checks that bore on it, as the incident's page says them.
func (a *API) viewPreview(pv suggestion.Preview) previewView {
	p := pv.Proposal
	// The fix named by what it changes, once worked out against the cluster:
	// a raise, a decrease, a rollback.
	var before, after []byte
	if j := pv.Judgement; j != nil {
		before, after = j.Decided.Action.PreState, j.Decided.Action.Desired
	}
	words := a.Fixes.Words(p.Capability, pv.Incident.Target.Name, p.Params, before, after)
	noun := words.Noun
	if noun == "" {
		noun = "fix"
	}
	v := previewView{Accepted: pv.Refused == "", Note: words.Note}
	switch {
	case pv.Recommended != "":
		v.Verdict = "Themis would recommend this " + noun + "."
	case v.Accepted:
		v.Verdict = "Themis would accept this " + noun + "."
	default:
		v.Verdict = "Themis would refuse this " + noun + "."
		reason := strings.TrimPrefix(strings.TrimPrefix(pv.Refused, "Refused: "), "Refused. ")
		v.Reason = upper(reason)
	}
	j := pv.Judgement
	if j == nil {
		return v
	}
	said := map[string]bool{}
	finding := func(name string) (previewFinding, check.Result, bool) {
		i := slices.IndexFunc(j.Checks, func(r check.Result) bool { return r.Name == name })
		if i < 0 || said[name] {
			return previewFinding{}, check.Result{}, false
		}
		said[name] = true
		r := j.Checks[i]
		return previewFinding{Says: upper(a.CheckList.Says(name, r.Value)), Reason: r.Reason}, r, true
	}
	for _, prof := range pv.Matched.Offering(p.Capability) {
		for _, name := range prof.JustifiedBy[p.Capability] {
			if f, r, ok := finding(name); ok && r.Value == check.True {
				v.For = append(v.For, f)
			} else if ok {
				v.Against = append(v.Against, f)
			}
		}
	}
	for _, name := range j.Against {
		if f, _, ok := finding(name); ok {
			v.Against = append(v.Against, f)
		}
	}
	if !v.Accepted || j.Refused != "" {
		return v
	}
	for _, name := range j.Past {
		if c, ok := a.CheckList.Get(name); ok && c.Availability {
			continue
		}
		if f, _, ok := finding(name); ok {
			v.For = append(v.For, f)
		}
	}
	d := j.Decided.Decision
	switch {
	case pv.Recommended != "":
		v.Then = pv.Recommended
	case d.Outcome == policy.Allow:
		v.Then = "Then " + lower(strings.TrimSuffix(d.Reason, ".")) + ", and a person approves it."
	case d.Outcome == policy.RequireApproval:
		v.Then = "Then " + lower(d.Reason)
	}
	return v
}

// upper and lower put a sentence's first letter in capitals, or not, but for
// a first word in capitals, such as CPU.
func upper(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func lower(s string) string {
	if len(s) > 1 && strings.ToUpper(s[1:2]) == s[1:2] && strings.ToLower(s[1:2]) != s[1:2] {
		return s
	}
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}
