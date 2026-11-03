package httpapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/server/httpapi"
	"github.com/hzerrad/themis/internal/settings"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

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
