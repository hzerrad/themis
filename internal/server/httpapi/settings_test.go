package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/config/settings"
	"github.com/hzerrad/themis/internal/platform/telemetry"
	"github.com/hzerrad/themis/internal/server/httpapi"
	"github.com/hzerrad/themis/internal/server/modeltest"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

// syncBuffer is a buffer requests may log to while a test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func settingsAPI(t *testing.T, install config.Model) (http.Handler, *settings.Settings) {
	t.Helper()
	s, _ := pgtest.NewStore(t)
	key := filepath.Join(t.TempDir(), "settings")
	if err := os.WriteFile(key, []byte(strings.Repeat("k", 48)), 0o600); err != nil {
		t.Fatal(err)
	}
	ms := &settings.Settings{Store: s, KeyFile: key, Install: install}
	mux := http.NewServeMux()
	(&httpapi.API{Store: s, TokenFile: tokenFile(t), Settings: ms}).Register(mux)
	return mux, ms
}

func send(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&b).Encode(body)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, &b)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestModelSettings(t *testing.T) {
	h, ms := settingsAPI(t, config.Model{Name: "qwen3:8b"})
	rec := send(t, h, http.MethodPut, "/v1/settings/model", map[string]any{"baseURL": "http://ollama:11434/v1", "apiKey": "sk-secret", "strictOutput": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %q", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.Contains(body, "sk-secret") {
		t.Fatalf("the response shows the key: %s", body)
	}
	var v settings.ModelView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || v.BaseURL != "http://ollama:11434/v1" || v.Name != "qwen3:8b" || !v.APIKeySet || v.StrictOutput || !v.Complete {
		t.Errorf("view = %+v, %v", v, err)
	}
	if m, err := ms.Model(t.Context()); err != nil || m.APIKey != "sk-secret" {
		t.Errorf("the investigator's view = %+v, %v", m, err)
	}
	if rec := send(t, h, http.MethodPut, "/v1/settings/model", map[string]any{"name": "other"}); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Helm values") {
		t.Errorf("changing the install's name = %d %q, want 409", rec.Code, rec.Body)
	}
	if rec := send(t, h, http.MethodPut, "/v1/settings/model", map[string]any{"baseURL": "not a url"}); rec.Code != http.StatusBadRequest || !strings.HasPrefix(rec.Body.String(), "The model's address") {
		t.Errorf("a bad address = %d %q, want a 400 sentence", rec.Code, rec.Body)
	}
	if rec := send(t, h, http.MethodGet, "/v1/settings/model", nil); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "sk-secret") {
		t.Errorf("GET = %d %q", rec.Code, rec.Body)
	}
}

// modelTestAPI serves the model settings and their test, to the admin token
// and to a viewer's session.
func modelTestAPI(t *testing.T, install config.Model) (http.Handler, *modeltest.Tests, *syncBuffer) {
	t.Helper()
	s, _ := pgtest.NewStore(t)
	key := filepath.Join(t.TempDir(), "settings")
	if err := os.WriteFile(key, []byte(strings.Repeat("k", 48)), 0o600); err != nil {
		t.Fatal(err)
	}
	se := store.Session{Subject: "v", Username: "viewer@example.com", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.CreateSession(t.Context(), "session-viewer", se); err != nil {
		t.Fatal(err)
	}
	tests := modeltest.New()
	tests.Pickup = 2 * time.Second
	logs := &syncBuffer{}
	mux := http.NewServeMux()
	(&httpapi.API{
		Store: s, TokenFile: tokenFile(t), Sessions: s, OIDC: true, Access: []config.AccessBinding{{Role: config.RoleViewer, Users: []string{"viewer@example.com"}}},
		Settings: &settings.Settings{Store: s, KeyFile: key, Install: install}, ModelTests: tests,
		Logger: telemetry.NewLogger(logs, "core", slog.LevelDebug),
	}).Register(mux)
	return mux, tests, logs
}

type modelTestReply struct {
	OK      bool   `json:"ok"`
	Outcome string `json:"outcome"`
	Text    string `json:"text"`
}

func testModel(t *testing.T, h http.Handler, form map[string]any) modelTestReply {
	t.Helper()
	rec := send(t, h, http.MethodPost, "/v1/settings/model/test", form)
	var got modelTestReply
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("POST test = %d %q", rec.Code, rec.Body)
	}
	return got
}

// The test sends the form's values to the investigator, a key typed but not
// saved included, and says what it found. Nothing is saved, and neither the
// key nor what the model was sent reaches the log.
func TestModelSettings_Test(t *testing.T) {
	h, tests, logs := modelTestAPI(t, config.Model{Provider: "anthropic", Name: "claude-haiku-5-5"})
	took := make(chan modeltest.Test, 1)
	go func() {
		got, _ := tests.Take(context.Background())
		took <- got
		_ = tests.Report(got.ID, modeltest.Result{
			Outcome: modeltest.Connected, Provider: "anthropic", Model: "claude-haiku-5-5", AnsweredBy: "claude-haiku-5-5", Latency: 1200 * time.Millisecond,
		})
	}()
	got := testModel(t, h, map[string]any{"apiKey": "typed-not-saved"})
	want := modelTestReply{OK: true, Outcome: "connected", Text: "Connected. claude-haiku-5-5 answered in 1.2 s in the reply format."}
	if got != want {
		t.Errorf("test = %+v, want %+v", got, want)
	}
	if sent := <-took; sent.Settings.APIKey != "typed-not-saved" {
		t.Errorf("the investigator got %+v", sent.Settings)
	}
	if rec := send(t, h, http.MethodGet, "/v1/settings/model", nil); strings.Contains(rec.Body.String(), `"apiKeySet":true`) {
		t.Errorf("the test saved the key: %s", rec.Body)
	}
	if !strings.Contains(logs.String(), "the model's connection was tested") || strings.Contains(logs.String(), "typed-not") {
		t.Errorf("logs = %s", logs)
	}
}

// Without what the provider needs, nothing is sent; without an investigator,
// the page says so; and only admins may test.
func TestModelSettings_TestRefuses(t *testing.T) {
	h, tests, _ := modelTestAPI(t, config.Model{})
	if got := testModel(t, h, map[string]any{"name": "qwen3:8b"}); got.Text != "Not tested. Enter the address first." || got.OK {
		t.Errorf("without an address: %+v", got)
	}
	if got := testModel(t, h, map[string]any{"provider": "anthropic", "name": "claude-haiku-5-5"}); got.Text != "Not tested. Enter the API key first." {
		t.Errorf("Anthropic without a key: %+v", got)
	}
	tests.Pickup = 20 * time.Millisecond
	if got := testModel(t, h, map[string]any{"baseURL": "http://ollama:11434/v1", "name": "qwen3:8b"}); got.Outcome != "no_investigator" ||
		got.Text != "No investigator is connected. Themis can't reach the model without it." {
		t.Errorf("without an investigator: %+v", got)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/settings/model/test", strings.NewReader(`{}`))
	req.AddCookie(&http.Cookie{Name: "themis_session", Value: "session-viewer", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	req.Header.Set("X-Themis-Request", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a viewer's test = %d %q, want 403", rec.Code, rec.Body)
	}
}
