package approval_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/approval"
	"github.com/hzerrad/themis/internal/testutil/metrictest"
)

func approvals(t *testing.T, result string) func() float64 {
	t.Helper()
	return metrictest.Delta(t, "themis_approvals_total", map[string]string{"result": result})
}

func TestMetrics_ApprovedOnceThenReplayed(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	approved, replayed := approvals(t, "approved"), approvals(t, "replayed")
	e.approve(t, a, a.Digest)
	e.approve(t, a, a.Digest)
	if got := approved(); got != 1 {
		t.Errorf("counted %v approvals, want 1", got)
	}
	if got := replayed(); got != 1 {
		t.Errorf("counted %v replays, want 1", got)
	}
}

func TestMetrics_DigestMismatch(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	mismatch, approved := approvals(t, "digest_mismatch"), approvals(t, "approved")
	e.approve(t, a, "sha256:what-the-page-did-not-show")
	if got := mismatch(); got != 1 {
		t.Errorf("counted %v digest mismatches, want 1", got)
	}
	if got := approved(); got != 0 {
		t.Errorf("counted %v approvals of a mismatched digest, want 0", got)
	}
}

func TestMetrics_Denied(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	denied := approvals(t, "denied")
	if rec := e.do(t, http.MethodPost, "/v1/actions/"+a.ID+"/deny", token, map[string]string{"reason": "no"}); rec.Code != http.StatusOK {
		t.Fatalf("deny: %d", rec.Code)
	}
	if got := denied(); got != 1 {
		t.Errorf("counted %v denials, want 1", got)
	}
}

func TestMetrics_Expired(t *testing.T) {
	e := newEnv(t)
	e.awaiting(t)
	expired := approvals(t, "expired")
	w := &approval.ExpireWorker{Store: e.store, Now: func() time.Time { return now.Add(time.Hour) }}
	if err := w.Expire(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := w.Expire(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := expired(); got != 1 {
		t.Errorf("counted %v expiries, want 1", got)
	}
}
