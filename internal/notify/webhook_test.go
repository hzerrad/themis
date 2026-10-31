package notify_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/notify"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

var at = time.Date(2026, 10, 28, 17, 0, 0, 0, time.UTC)

// escalated creates an escalated incident and returns its notice.
func escalated(t *testing.T, s *store.Store) store.EscalationArgs {
	t.Helper()
	ctx := t.Context()
	target := incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", UID: "uid-1"}
	var id string
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		id, _, err = s.CreateOrJoinIncident(ctx, tx, store.IncidentArgs{Fingerprint: "fp", Profile: "rollout-regression", Target: target, Payload: []byte(`{}`)})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return store.EscalationArgs{IncidentID: id, From: incident.Verifying, To: incident.Escalated, Reason: "The fix didn't help: 1 of 3 pods are ready.", At: at}
}

type received struct {
	auth string
	body map[string]any
}

func receiver(t *testing.T, code int) (*httptest.Server, <-chan received) {
	t.Helper()
	got := make(chan received, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		got <- received{auth: r.Header.Get("Authorization"), body: body}
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func tokenFile(t *testing.T) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(f, []byte("hook-s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestWebhook_PostsTheEscalation(t *testing.T) {
	s, _ := pgtest.NewStore(t)
	notice := escalated(t, s)
	srv, got := receiver(t, http.StatusNoContent)
	n := &notify.Worker{Store: s, Config: config.Notifications{
		Webhook:     &config.Webhook{URL: srv.URL, BearerTokenFile: tokenFile(t)},
		ExternalURL: "https://themis.example.com",
	}}
	if err := n.Notify(t.Context(), notice); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	r := <-got
	if r.auth != "Bearer hook-s3cret" {
		t.Errorf("Authorization = %q", r.auth)
	}
	b := r.body
	inc, _ := b["incident"].(map[string]any)
	if b["event"] != "incident.escalated" || inc["id"] != notice.IncidentID || inc["from"] != "verifying" || inc["reason"] != "The fix didn't help: 1 of 3 pods are ready." {
		t.Errorf("payload = %v", b)
	}
	if inc["url"] != "https://themis.example.com/#/incidents/"+notice.IncidentID {
		t.Errorf("url = %v", inc["url"])
	}
	if target, _ := inc["target"].(map[string]any); target["name"] != "checkout" || target["namespace"] != "shop" {
		t.Errorf("target = %v", inc["target"])
	}
}

// A failing receiver is retried; River backs off between attempts.
func TestWebhook_ReceiverErrorIsRetried(t *testing.T) {
	s, _ := pgtest.NewStore(t)
	srv, _ := receiver(t, http.StatusBadGateway)
	n := &notify.Worker{Store: s, Config: config.Notifications{Webhook: &config.Webhook{URL: srv.URL}}}
	if err := n.Notify(t.Context(), escalated(t, s)); err == nil {
		t.Error("a 502 was taken as delivered")
	}
}

func TestWebhook_NoneConfigured(t *testing.T) {
	s, _ := pgtest.NewStore(t)
	if err := (&notify.Worker{Store: s}).Notify(t.Context(), escalated(t, s)); err != nil {
		t.Errorf("Notify without a webhook = %v, want nil", err)
	}
}

func TestWebhook_ContainedIsItsOwnEvent(t *testing.T) {
	s, _ := pgtest.NewStore(t)
	notice := escalated(t, s)
	notice.To = incident.ContainedEscalated
	srv, got := receiver(t, http.StatusOK)
	n := &notify.Worker{Store: s, Config: config.Notifications{Webhook: &config.Webhook{URL: srv.URL}}}
	if err := n.Notify(t.Context(), notice); err != nil {
		t.Fatal(err)
	}
	if r := <-got; r.body["event"] != "incident.contained_escalated" || r.auth != "" {
		t.Errorf("event = %v, auth = %q", r.body["event"], r.auth)
	}
}

// A URL holding a key comes from a file, as a mounted Secret.
func TestWebhook_URLFromFile(t *testing.T) {
	s, _ := pgtest.NewStore(t)
	srv, got := receiver(t, http.StatusOK)
	f := filepath.Join(t.TempDir(), "url")
	if err := os.WriteFile(f, []byte(srv.URL+"/hooks/T000/B000/XXXX\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n := &notify.Worker{Store: s, Config: config.Notifications{Webhook: &config.Webhook{URLFile: f}}}
	if err := n.Notify(t.Context(), escalated(t, s)); err != nil {
		t.Fatal(err)
	}
	<-got
}

// River stores job errors and the core logs them: the URL, which may hold a
// key, must not be in them.
func TestWebhook_ErrorsDoNotRevealTheURL(t *testing.T) {
	s, _ := pgtest.NewStore(t)
	n := &notify.Worker{Store: s, Config: config.Notifications{Webhook: &config.Webhook{URL: "http://127.0.0.1:1/hooks/T000/B000/s3cretkey"}}}
	err := n.Notify(t.Context(), escalated(t, s))
	if err == nil || strings.Contains(err.Error(), "s3cretkey") {
		t.Errorf("err = %v", err)
	}
}
