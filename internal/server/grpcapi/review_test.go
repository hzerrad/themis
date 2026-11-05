package grpcapi_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/hzerrad/themis/internal/server/grpcapi"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

// counting counts the reviews it is asked for.
type counting struct {
	grpcapi.TokenReviewer
	n atomic.Int32
}

func (c *counting) Review(ctx context.Context, token string) (string, error) {
	c.n.Add(1)
	return c.TokenReviewer.Review(ctx, token)
}

func (e *env) countReviews() *counting {
	c := &counting{TokenReviewer: e.server.Reviewer}
	e.server.Reviewer = c
	return c
}

func wantReviews(t *testing.T, c *counting, want int32) {
	t.Helper()
	if got := c.n.Load(); got != want {
		t.Errorf("%d reviews, want %d", got, want)
	}
}

func list(t *testing.T, c themisv1.ProposalServiceClient) error {
	t.Helper()
	_, err := c.ListIncidents(t.Context(), &themisv1.ListIncidentsRequest{})
	return err
}

func TestExternal_ReviewsATokenOnceAMinute(t *testing.T) {
	e := newEnv(t)
	agent := e.external(t)
	reviews := e.countReviews()
	for range 3 {
		if err := list(t, agent); err != nil {
			t.Fatal(err)
		}
	}
	wantReviews(t, reviews, 1)
	e.now = e.now.Add(time.Minute)
	if err := list(t, agent); err != nil {
		t.Fatal(err)
	}
	wantReviews(t, reviews, 2)
}

func TestExternal_ReviewsATokenAgainOnceItExpires(t *testing.T) {
	e := newEnv(t)
	e.external(t)
	claims := fmt.Sprintf(`{"exp":%d}`, e.now.Add(10*time.Second).Unix())
	tok := "h." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".s"
	e.server.Reviewer.(reviewer)[tok] = agentAccount //nolint:forcetypeassert // set by external
	reviews := e.countReviews()
	agent := e.clientWith(t, tok)
	for _, wait := range []time.Duration{0, 5 * time.Second, 6 * time.Second} {
		e.now = e.now.Add(wait)
		if err := list(t, agent); err != nil {
			t.Fatal(err)
		}
	}
	wantReviews(t, reviews, 2)
}

// Past its limit, an address is refused before its token is reviewed, but a
// token already accepted still works.
func TestExternal_LimitsAttemptsPerAddressBeforeReview(t *testing.T) {
	e := newEnv(t)
	agent := e.external(t)
	reviews := e.countReviews()
	if err := list(t, agent); err != nil {
		t.Fatal(err)
	}
	for i := range grpcapi.PeerAttemptsPerMinute - 1 {
		wantCode(t, list(t, e.clientWith(t, fmt.Sprintf("forged-%d", i))), codes.Unauthenticated)
	}
	wantReviews(t, reviews, grpcapi.PeerAttemptsPerMinute)
	wantCode(t, list(t, e.clientWith(t, "forged")), codes.ResourceExhausted)
	wantReviews(t, reviews, grpcapi.PeerAttemptsPerMinute)
	if err := list(t, agent); err != nil {
		t.Errorf("accepted token: %v", err)
	}
	e.now = e.now.Add(time.Minute)
	wantCode(t, list(t, e.clientWith(t, "forged")), codes.Unauthenticated)
}

func TestExternal_LimitsReads(t *testing.T) {
	e := newEnv(t)
	agent := e.external(t)
	for range grpcapi.ReadsPerMinute {
		if err := list(t, agent); err != nil {
			t.Fatal(err)
		}
	}
	_, err := agent.GetIncident(t.Context(), &themisv1.GetIncidentRequest{IncidentId: "00000000-0000-7000-8000-000000000000"})
	wantCode(t, err, codes.ResourceExhausted)
	wantCode(t, list(t, agent), codes.ResourceExhausted)
	e.now = e.now.Add(time.Minute)
	if err := list(t, agent); err != nil {
		t.Errorf("after a minute: %v", err)
	}
}

func TestExternal_RemembersARefusalBriefly(t *testing.T) {
	e := newEnv(t)
	e.external(t)
	reviews := e.countReviews()
	forged := e.clientWith(t, "forged")
	for range 3 {
		wantCode(t, list(t, forged), codes.Unauthenticated)
	}
	wantReviews(t, reviews, 1)
	e.now = e.now.Add(5 * time.Second)
	wantCode(t, list(t, forged), codes.Unauthenticated)
	wantReviews(t, reviews, 2)
}
