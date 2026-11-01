package grpcserver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"golang.org/x/time/rate"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/hzerrad/themis/internal/auth"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/governance"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

const (
	// Audience is who a caller's token must be issued for, so a token meant
	// for another service can't be replayed here.
	Audience = "themis"

	// PersonSource names a person using the admin token. It is one shared
	// credential, so it proves the token, not which person.
	PersonSource = "admin-token"

	personPerMinute = 60

	// maxExternalProposals bounds what one source can pile on an incident.
	maxExternalProposals = 20
	maxExternalText      = 4 << 10
	maxExternalCitations = 50
	maxOpenIncidents     = 100
)

// Caller is someone outside Themis, as proven by their credential.
type Caller struct {
	Source       string
	Namespaces   []string
	Capabilities []string
	PerMinute    int
	Ceiling      evidence.Classification

	// All means any namespace and capability the profile allows, for a person.
	All bool
}

type callerKey struct{}

func withCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

func callerFrom(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}

// TokenReviewer returns the ServiceAccount, as namespace/name, a token proves.
type TokenReviewer interface {
	Review(ctx context.Context, token string) (string, error)
}

// KubeTokenReviewer asks the Kubernetes API server.
type KubeTokenReviewer struct {
	Clientset kubernetes.Interface
}

// ErrNotAServiceAccount means a token proves nothing Themis can use.
var ErrNotAServiceAccount = errors.New("the token doesn't prove a ServiceAccount")

func (k KubeTokenReviewer) Review(ctx context.Context, token string) (string, error) {
	r, err := k.Clientset.AuthenticationV1().TokenReviews().Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: []string{Audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("review token: %w", err)
	}
	if !r.Status.Authenticated || !slices.Contains(r.Status.Audiences, Audience) {
		return "", ErrNotAServiceAccount
	}
	name, ok := strings.CutPrefix(r.Status.User.Username, "system:serviceaccount:")
	if !ok {
		return "", ErrNotAServiceAccount
	}
	ns, sa, ok := strings.Cut(name, ":")
	if !ok {
		return "", ErrNotAServiceAccount
	}
	return ns + "/" + sa, nil
}

// externalMethods are all a caller outside Themis may call.
var externalMethods = []string{
	themisv1.ProposalService_SubmitProposal_FullMethodName,
	themisv1.ProposalService_ListIncidents_FullMethodName,
	themisv1.ProposalService_GetIncident_FullMethodName,
}

// externalCaller proves who is calling, unless it is the investigator or the
// method isn't for outside callers, which leaves the usual check to decide.
func (s *Server) externalCaller(ctx context.Context, tokenFile, method string) (Caller, bool, error) {
	if s.Reviewer == nil || !slices.Contains(externalMethods, method) {
		return Caller{}, false, nil
	}
	token := bearer(ctx)
	if token == "" {
		return Caller{}, false, nil
	}
	if want, err := auth.ReadToken(tokenFile); err == nil && auth.BearerMatches("Bearer "+token, want) {
		return Caller{}, false, nil
	}
	account, err := s.Reviewer.Review(ctx, token)
	if err != nil {
		if !errors.Is(err, ErrNotAServiceAccount) {
			s.logger().WarnContext(ctx, "cannot review a caller's token", "error", err)
			return Caller{}, false, status.Error(codes.Unavailable, "the caller's identity can't be checked now")
		}
		return Caller{}, false, status.Error(codes.Unauthenticated, "a valid token issued for "+Audience+" is required")
	}
	source, ok := s.Config.Current().ExternalProposals.Source(account)
	if !ok {
		return Caller{}, false, status.Error(codes.PermissionDenied, account+" isn't allowed to suggest fixes")
	}
	return callerOf(source), true, nil
}

func callerOf(p config.ProposalSource) Caller {
	c := Caller{
		Source: "serviceaccount:" + p.ServiceAccount, Namespaces: p.Namespaces, Capabilities: p.Capabilities,
		PerMinute: p.PerMinute, Ceiling: evidence.ClassificationInternal,
	}
	if c.PerMinute == 0 {
		c.PerMinute = config.DefaultPerMinute
	}
	if p.Ceiling == string(evidence.ClassificationSensitive) {
		c.Ceiling = evidence.ClassificationSensitive
	}
	return c
}

func bearer(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	for _, v := range md.Get("authorization") {
		if scheme, token, ok := strings.Cut(v, " "); ok && strings.EqualFold(scheme, "Bearer") {
			return strings.TrimSpace(token)
		}
	}
	return ""
}

// SubmitFromPerson takes a suggestion from a person using the API.
func (s *Server) SubmitFromPerson(ctx context.Context, req *themisv1.SubmitProposalRequest) (*themisv1.SubmitProposalResponse, error) {
	return s.submitExternal(ctx, Caller{Source: PersonSource, PerMinute: personPerMinute, All: true}, req)
}

func (s *Server) allow(c Caller) bool {
	key := fmt.Sprintf("%s/%d", c.Source, c.PerMinute)
	l, _ := s.limiters.LoadOrStore(key, rate.NewLimiter(rate.Limit(float64(c.PerMinute)/60), c.PerMinute))
	return l.(*rate.Limiter).Allow() //nolint:forcetypeassert // only limiters are stored
}

func (c Caller) inScope(namespace string) bool {
	return c.All || slices.Contains(c.Namespaces, namespace)
}

// submitExternal stores a suggestion from outside and, if the incident is
// waiting for one, sends it on to governance. It passes the same checks as
// the investigator's. What is out of scope or refused is kept, with why.
func (s *Server) submitExternal(ctx context.Context, caller Caller, req *themisv1.SubmitProposalRequest) (*themisv1.SubmitProposalResponse, error) {
	if req.GetLeaseId() != "" {
		return nil, status.Error(codes.PermissionDenied, "a lease belongs to the investigator")
	}
	if req.GetIncidentId() == "" {
		return nil, status.Error(codes.InvalidArgument, "incident_id is required")
	}
	if len(req.GetRationale()) > maxExternalText || len(req.GetExpectedEffect()) > maxExternalText ||
		len(req.GetClientProposalId()) > 256 ||
		len(req.GetCitedEvidenceIds()) > maxExternalCitations || len(req.GetCitedPredicates()) > maxExternalCitations {
		return nil, status.Error(codes.InvalidArgument, "the suggestion is too long")
	}
	capability, params, invalid, err := parseProposal(req)
	if err != nil {
		return nil, err
	}
	if !s.allow(caller) {
		return nil, status.Error(codes.ResourceExhausted, "too many suggestions: wait a minute and try again")
	}
	in, err := s.Store.Incident(ctx, req.GetIncidentId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "no such incident")
	}
	if err != nil {
		return nil, s.internal(ctx, err)
	}

	prov := provenance(req.GetProvenance())
	prov["claimedSource"] = prov["source"]
	prov["source"] = caller.Source

	var stored store.Proposal
	err = s.Store.InTx(ctx, func(tx pgx.Tx) error {
		state, err := s.Store.LockIncident(ctx, tx, in.ID)
		if err != nil {
			return err
		}
		n, err := s.Store.CountExternalProposals(ctx, tx, in.ID, caller.Source)
		if err != nil {
			return err
		}
		if n >= maxExternalProposals {
			return status.Error(codes.ResourceExhausted, "this source has suggested too many fixes for this incident")
		}
		p := store.Proposal{
			IncidentID: in.ID, ClientProposalID: req.GetClientProposalId(),
			Capability: capability, CapabilityVersion: CapabilityRollbackVersion,
			Target: targetFromProto(req.GetTarget()), Params: params,
			ExpectedEffect: req.GetExpectedEffect(), Rationale: req.GetRationale(),
			CitedEvidenceIDs: req.GetCitedEvidenceIds(), CitedPredicates: req.GetCitedPredicates(),
			Provenance: prov, Authenticated: true, Status: store.ProposalAccepted,
		}
		r, err := s.judgeExternal(ctx, tx, caller, in, state, p, invalid)
		if err != nil {
			return err
		}
		if r != nil {
			p.Status, p.RejectionReason, p.Message = store.ProposalRejected, r.reason.String(), r.message
		}
		var created bool
		stored, created, err = s.Store.InsertProposal(ctx, tx, p)
		if err != nil || !created {
			return err
		}
		countProposal(tx, r)
		store.AfterCommit(tx, externalProposalsTotal.WithLabelValues(externalResult(r)).Inc)
		if r != nil {
			if r.reason != themisv1.RejectionReason_REJECTION_REASON_TARGET_MISMATCH {
				return nil
			}
			return s.Store.RecordSafetyEvent(ctx, tx, store.SafetyEvent{
				IncidentID: in.ID, ProposalID: stored.ID, Kind: store.SafetyTargetMismatch, Detail: r.message,
			})
		}
		// While the investigation runs, it picks this up when it finishes.
		if state != incident.Inconclusive {
			return nil
		}
		if err := s.advance(ctx, tx, in.ID, "A fix was suggested by "+caller.Source+".", incident.Inconclusive, incident.Diagnosed); err != nil {
			return err
		}
		return s.Store.EnqueueTx(ctx, tx, governance.GovernArgs{ProposalID: stored.ID, IncidentID: in.ID})
	})
	if err != nil {
		return nil, s.internal(ctx, err)
	}
	return proposalResponse(stored), nil
}

// judgeExternal adds to judge what only a caller from outside can get wrong.
func (s *Server) judgeExternal(ctx context.Context, tx pgx.Tx, c Caller, in store.Incident, state incident.State, p store.Proposal, invalid string) (*rejection, error) {
	if !c.inScope(in.Target.Namespace) {
		return &rejection{
			themisv1.RejectionReason_REJECTION_REASON_OUT_OF_SCOPE,
			fmt.Sprintf("%s may not suggest fixes in %s.", c.Source, in.Target.Namespace),
		}, nil
	}
	if state != incident.Investigating && state != incident.Inconclusive {
		return &rejection{
			themisv1.RejectionReason_REJECTION_REASON_INCIDENT_CLOSED,
			"This incident is past the point of taking suggestions.",
		}, nil
	}
	if !c.All && !slices.Contains(c.Capabilities, p.Capability) {
		return &rejection{
			themisv1.RejectionReason_REJECTION_REASON_CAPABILITY_NOT_ALLOWED,
			fmt.Sprintf("%s may not suggest this kind of fix (%s).", c.Source, p.Capability),
		}, nil
	}
	profile, ok := s.Config.Current().Profile(in.Profile)
	if !ok {
		return &rejection{
			themisv1.RejectionReason_REJECTION_REASON_INCIDENT_CLOSED,
			fmt.Sprintf("The %s profile is no longer configured.", in.Profile),
		}, nil
	}
	return s.judge(ctx, tx, store.Investigation{IncidentID: in.ID, Target: in.Target}, profile, p, invalid)
}

func externalResult(r *rejection) string {
	if r == nil {
		return "accepted"
	}
	return "rejected"
}

// ListIncidents returns the open incidents the caller may suggest fixes for.
func (s *Server) ListIncidents(ctx context.Context, _ *themisv1.ListIncidentsRequest) (*themisv1.ListIncidentsResponse, error) {
	caller, ok := callerFrom(ctx)
	if !ok {
		return nil, status.Error(codes.PermissionDenied, "this is for callers outside Themis")
	}
	incidents, err := s.Store.OpenIncidents(ctx, caller.Namespaces, maxOpenIncidents)
	if err != nil {
		return nil, s.internal(ctx, err)
	}
	resp := &themisv1.ListIncidentsResponse{}
	for _, in := range incidents {
		resp.Incidents = append(resp.Incidents, openIncident(in))
	}
	return resp, nil
}

func openIncident(in store.Incident) *themisv1.OpenIncident {
	return &themisv1.OpenIncident{Id: in.ID, Target: targetToProto(in.Target), State: string(in.State)}
}

// GetIncident returns what Themis knows about an incident in the caller's
// scope: redacted evidence up to its ceiling, and the checks.
func (s *Server) GetIncident(ctx context.Context, req *themisv1.GetIncidentRequest) (*themisv1.GetIncidentResponse, error) {
	caller, ok := callerFrom(ctx)
	if !ok {
		return nil, status.Error(codes.PermissionDenied, "this is for callers outside Themis")
	}
	in, err := s.Store.Incident(ctx, req.GetIncidentId())
	// One answer for both, so a caller can't learn of an incident out of its scope.
	if errors.Is(err, store.ErrNotFound) || (err == nil && !caller.inScope(in.Target.Namespace)) {
		return nil, status.Error(codes.NotFound, "no such incident")
	}
	if err != nil {
		return nil, s.internal(ctx, err)
	}
	items, err := s.Store.Evidence(ctx, in.ID)
	if err != nil {
		return nil, s.internal(ctx, err)
	}
	results, err := s.Store.Predicates(ctx, in.ID)
	if err != nil {
		return nil, s.internal(ctx, err)
	}
	resp := &themisv1.GetIncidentResponse{Incident: openIncident(in)}
	for _, it := range items {
		if it.Classification == evidence.ClassificationInternal || caller.Ceiling == evidence.ClassificationSensitive {
			resp.Evidence = append(resp.Evidence, itemToProto(it))
		} else {
			resp.Withheld++
		}
	}
	for _, r := range results {
		resp.Predicates = append(resp.Predicates, predicateToProto(r))
	}
	return resp, nil
}
