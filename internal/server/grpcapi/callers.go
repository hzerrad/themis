package grpcapi

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident/suggestion"
	"github.com/hzerrad/themis/internal/platform/token"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

// Caller is someone outside Themis, as proven by their credential: what it
// may suggest, and the evidence it may read.
type Caller struct {
	suggestion.Suggester
	Ceiling evidence.Classification
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

const (
	// reviewFor is how long a token's review is trusted, at most until the
	// token expires. A ServiceAccount deleted meanwhile keeps access that long.
	reviewFor = time.Minute
	// refusalFor is shorter, so a token that becomes valid works soon.
	refusalFor = 5 * time.Second
)

// review is what the API server said of a token, until when.
type review struct {
	account string
	until   time.Time
}

var errTooMany = status.Error(codes.ResourceExhausted, "too many requests: wait a moment and try again")

// review asks the Reviewer about a token, or recalls a recent answer. Only
// the token's hash is kept. Unless the token was accepted, the caller's
// address must be within its limit.
func (s *Server) review(ctx context.Context, token string) (string, error) {
	now := s.now()
	key := sha256.Sum256([]byte(token))
	r, cached := s.reviews.get(key)
	cached = cached && now.Before(r.until)
	if cached && r.account != "" {
		tokenReviewCacheHitsTotal.Inc()
		return r.account, nil
	}
	if !s.allowPeer(ctx, now) {
		suggestion.CountLimited("address")
		return "", errTooMany
	}
	if cached {
		tokenReviewCacheHitsTotal.Inc()
		return "", ErrNotAServiceAccount
	}
	account, err := s.Reviewer.Review(ctx, token)
	switch {
	case err == nil:
		tokenReviewsTotal.WithLabelValues("accepted").Inc()
		until := now.Add(reviewFor)
		if exp, ok := expiry(token); ok && exp.Before(until) {
			until = exp
		}
		s.reviews.put(key, review{account, until})
	case errors.Is(err, ErrNotAServiceAccount):
		tokenReviewsTotal.WithLabelValues("refused").Inc()
		s.reviews.put(key, review{"", now.Add(refusalFor)})
	default:
		tokenReviewsTotal.WithLabelValues("failed").Inc()
	}
	return account, err
}

// expiry reads a JWT's exp claim without verifying it, so only use it on a
// token the API server accepted.
func expiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
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
	presented := bearer(ctx)
	if presented == "" {
		return Caller{}, false, nil
	}
	if want, err := token.Read(tokenFile); err == nil && token.Matches("Bearer "+presented, want) {
		return Caller{}, false, nil
	}
	account, err := s.review(ctx, presented)
	if err != nil {
		if errors.Is(err, errTooMany) {
			return Caller{}, false, err
		}
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
		Suggester: suggestion.Suggester{
			Source: "serviceaccount:" + p.ServiceAccount, Namespaces: p.Namespaces, Fixes: p.Capabilities, PerMinute: p.EffectivePerMinute(),
		},
		Ceiling: evidence.ClassificationInternal,
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

func perMinute(n int) *rate.Limiter {
	return rate.NewLimiter(rate.Limit(float64(n)/60), n)
}

// allowPeer counts an attempt from the caller's address, without its port.
func (s *Server) allowPeer(ctx context.Context, now time.Time) bool {
	var addr string
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		addr = p.Addr.String()
		if host, _, err := net.SplitHostPort(addr); err == nil {
			addr = host
		}
	}
	return s.peers.getOrPut(addr, func() *rate.Limiter { return perMinute(PeerAttemptsPerMinute) }).AllowN(now, 1)
}

func (s *Server) allowRead(c Caller) bool {
	l, _ := s.limiters.LoadOrStore("read/"+c.Source, perMinute(ReadsPerMinute))
	return l.(*rate.Limiter).AllowN(s.now(), 1) //nolint:forcetypeassert // only limiters are stored
}

// reader is the caller of a read, within its limit.
func (s *Server) reader(ctx context.Context) (Caller, error) {
	c, ok := callerFrom(ctx)
	if !ok {
		return Caller{}, status.Error(codes.PermissionDenied, "this is for callers outside Themis")
	}
	if !s.allowRead(c) {
		suggestion.CountLimited("reads")
		return Caller{}, errTooMany
	}
	return c, nil
}

// lruSize bounds each lru, so callers can't grow it without limit.
const lruSize = 4096

// lru keeps the most recently used values, up to lruSize. Its zero value is
// ready to use, and it is safe for concurrent use.
type lru[K comparable, V any] struct {
	mu    sync.Mutex
	items map[K]*list.Element
	order list.List // most recent first
}

type lruEntry[K comparable, V any] struct {
	key   K
	value V
}

func (c *lru[K, V]) get(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.lookup(k); ok {
		return e.value, true
	}
	var zero V
	return zero, false
}

func (c *lru[K, V]) put(k K, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.lookup(k); ok {
		e.value = v
		return
	}
	c.insert(k, v)
}

// getOrPut returns the value for k, storing a new one first if there is none.
func (c *lru[K, V]) getOrPut(k K, v func() V) V {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.lookup(k); ok {
		return e.value
	}
	nv := v()
	c.insert(k, nv)
	return nv
}

func (c *lru[K, V]) lookup(k K) (*lruEntry[K, V], bool) {
	el, ok := c.items[k]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*lruEntry[K, V]), true //nolint:forcetypeassert // only entries are stored
}

func (c *lru[K, V]) insert(k K, v V) {
	if c.items == nil {
		c.items = map[K]*list.Element{}
	}
	c.items[k] = c.order.PushFront(&lruEntry[K, V]{k, v})
	if c.order.Len() > lruSize {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*lruEntry[K, V]).key) //nolint:forcetypeassert // only entries are stored
	}
}
