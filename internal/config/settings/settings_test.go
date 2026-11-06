package settings_test

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/config/settings"
	"github.com/hzerrad/themis/internal/store"
)

type memStore map[string]store.Setting

func (m memStore) Settings(_ context.Context, names []string) (map[string]store.Setting, error) {
	out := map[string]store.Setting{}
	for _, n := range names {
		if s, ok := m[n]; ok {
			out[n] = s
		}
	}
	return out, nil
}

func (m memStore) PutSettings(_ context.Context, set []store.Setting, clear []string) error {
	for _, s := range set {
		m[s.Name] = s
	}
	for _, n := range clear {
		delete(m, n)
	}
	return nil
}

func keyFile(t *testing.T, key string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(f, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

const key = "a-settings-key-of-at-least-thirty-two-characters"

func ptr[T any](v T) *T { return &v }

func TestModel_SetAndRead(t *testing.T) {
	m := memStore{}
	s := &settings.Settings{Store: m, KeyFile: keyFile(t, key)}
	err := s.SetModel(t.Context(), settings.ModelChange{
		BaseURL: ptr("https://api.openai.com/v1/"), Name: ptr("gpt-5"), APIKey: ptr("sk-secret"), StrictOutput: ptr(false),
	}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if stored := m[settings.ModelAPIKey]; !stored.Encrypted || strings.Contains(stored.Value, "sk-secret") {
		t.Errorf("the key is stored as %+v, want it encrypted", stored)
	}
	if m[settings.ModelName].UpdatedBy != "alice" {
		t.Errorf("updated by = %q", m[settings.ModelName].UpdatedBy)
	}
	got, err := s.Model(t.Context())
	if err != nil || got.BaseURL != "https://api.openai.com/v1" || got.Name != "gpt-5" || got.APIKey != "sk-secret" || got.StrictOutput == nil || *got.StrictOutput {
		t.Fatalf("Model = %+v, %v", got, err)
	}
	v, err := s.View(t.Context())
	if err != nil || !v.Complete || !v.APIKeySet || v.StrictOutput || len(v.ByInstall) != 0 {
		t.Errorf("View = %+v, %v", v, err)
	}
	// An empty key removes it.
	if err := s.SetModel(t.Context(), settings.ModelChange{APIKey: ptr("")}, "alice"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Model(t.Context()); got.APIKey != "" {
		t.Error("the key is still set after removing it")
	}
}

// What the install set, setup can't change, and the view shows the install's.
func TestModel_InstallWins(t *testing.T) {
	s := &settings.Settings{Store: memStore{}, KeyFile: keyFile(t, key), Install: config.Model{BaseURL: "http://ollama:11434/v1", APIKey: true}}
	for name, change := range map[string]settings.ModelChange{
		"address":        {BaseURL: ptr("https://elsewhere/v1")},
		"strict output":  {StrictOutput: ptr(false)},
		"key":            {APIKey: ptr("sk-x")},
		"provider":       {Provider: ptr("anthropic")},
		"schema dialect": {SchemaDialect: ptr("openai-strict")},
	} {
		if err := s.SetModel(t.Context(), change, "alice"); !errors.Is(err, settings.ErrSetByInstall) {
			t.Errorf("changing the install's %s: %v, want ErrSetByInstall", name, err)
		}
	}
	if err := s.SetModel(t.Context(), settings.ModelChange{Name: ptr("qwen3:8b")}, "alice"); err != nil {
		t.Fatalf("setting what the install left unset: %v", err)
	}
	v, err := s.View(t.Context())
	if err != nil || v.BaseURL != "http://ollama:11434/v1" || v.Name != "qwen3:8b" || !v.APIKeySet || !v.Complete {
		t.Errorf("View = %+v, %v", v, err)
	}
	if strings.Join(v.ByInstall, ",") != "model.provider,model.baseURL,model.strictOutput,model.schemaDialect,model.apiKey" {
		t.Errorf("by install = %v", v.ByInstall)
	}
}

// The model is complete once it has what its provider needs, from the install
// or from setup: a name, and an address unless its provider has its own.
func TestModel_CompleteForItsProvider(t *testing.T) {
	strictOff := false
	for name, c := range map[string]struct {
		install  config.Model
		change   settings.ModelChange
		complete bool
		want     settings.ModelView
	}{
		"Anthropic set at install": {
			install: config.Model{Provider: "anthropic", Name: "claude-haiku-5-5", APIKey: true}, complete: true,
			want: settings.ModelView{Provider: "anthropic", Name: "claude-haiku-5-5", APIKeySet: true, StrictOutput: true, SchemaDialect: "as-is"},
		},
		"OpenAI set at install": {
			install:  config.Model{BaseURL: "https://api.openai.com/v1", Name: "gpt-5", APIKey: true, StrictOutput: &strictOff, SchemaDialect: "openai-strict"},
			complete: true,
			want:     settings.ModelView{Provider: "openai", BaseURL: "https://api.openai.com/v1", Name: "gpt-5", APIKeySet: true, SchemaDialect: "openai-strict"},
		},
		"Anthropic chosen in setup": {
			change: settings.ModelChange{Provider: ptr("anthropic"), BaseURL: ptr("http://ollama:11434/v1"), Name: ptr("claude-haiku-5-5")}, complete: true,
			// Setup's address, made for another provider, doesn't apply.
			want: settings.ModelView{Provider: "anthropic", Name: "claude-haiku-5-5", StrictOutput: true, SchemaDialect: "as-is"},
		},
		"OpenAI-compatible in setup, without an address": {
			change: settings.ModelChange{Name: ptr("qwen3:8b"), SchemaDialect: ptr("openai-strict")},
			want:   settings.ModelView{Provider: "openai", Name: "qwen3:8b", StrictOutput: true, SchemaDialect: "openai-strict"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := &settings.Settings{Store: memStore{}, KeyFile: keyFile(t, key), Install: c.install}
			if err := s.SetModel(t.Context(), c.change, "alice"); err != nil {
				t.Fatal(err)
			}
			v, err := s.View(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if v.Complete != c.complete || len(v.Missing()) == 0 != c.complete {
				t.Errorf("complete = %v, missing %v, want complete %v", v.Complete, v.Missing(), c.complete)
			}
			v.ByInstall, v.Complete = nil, false
			if !reflect.DeepEqual(v, c.want) {
				t.Errorf("View = %+v\nwant   %+v", v, c.want)
			}
		})
	}
}

// A trial is the model with the form's values, saved or not: nothing is
// saved, and a key the form leaves alone is the stored one.
func TestModel_Trial(t *testing.T) {
	m := memStore{}
	s := &settings.Settings{Store: m, KeyFile: keyFile(t, key), Install: config.Model{Name: "qwen3:8b"}}
	if err := s.SetModel(t.Context(), settings.ModelChange{BaseURL: ptr("http://ollama:11434/v1"), APIKey: ptr("sk-stored")}, "alice"); err != nil {
		t.Fatal(err)
	}
	before := maps.Clone(m)
	got, v, err := s.Trial(t.Context(), settings.ModelChange{BaseURL: ptr("https://api.openai.com/v1/"), StrictOutput: ptr(false)})
	if err != nil || got.BaseURL != "https://api.openai.com/v1" || got.APIKey != "sk-stored" || got.StrictOutput == nil || *got.StrictOutput {
		t.Fatalf("Trial = %+v, %v", got, err)
	}
	if !v.Complete || v.Name != "qwen3:8b" {
		t.Errorf("trial view = %+v", v)
	}
	if got, _, _ := s.Trial(t.Context(), settings.ModelChange{APIKey: ptr("sk-typed")}); got.APIKey != "sk-typed" {
		t.Errorf("a typed key: %q", got.APIKey)
	}
	if !reflect.DeepEqual(m, before) {
		t.Error("a trial saved something")
	}
	if _, _, err := s.Trial(t.Context(), settings.ModelChange{Name: ptr("other")}); !errors.Is(err, settings.ErrSetByInstall) {
		t.Errorf("trying the install's name: %v, want ErrSetByInstall", err)
	}
}

func TestModel_Refuses(t *testing.T) {
	s := &settings.Settings{Store: memStore{}, KeyFile: keyFile(t, key)}
	if err := s.SetModel(t.Context(), settings.ModelChange{BaseURL: ptr("ftp://x")}, "a"); err == nil {
		t.Error("an ftp address was accepted")
	}
	short := &settings.Settings{Store: memStore{}, KeyFile: keyFile(t, "short")}
	if err := short.SetModel(t.Context(), settings.ModelChange{APIKey: ptr("sk")}, "a"); err == nil || !strings.Contains(err.Error(), "too short") {
		t.Errorf("a short settings key: %v", err)
	}
}

// A changed settings key makes the stored secret unreadable, and says so.
func TestModel_ChangedKey(t *testing.T) {
	m := memStore{}
	s := &settings.Settings{Store: m, KeyFile: keyFile(t, key)}
	if err := s.SetModel(t.Context(), settings.ModelChange{APIKey: ptr("sk-secret")}, "a"); err != nil {
		t.Fatal(err)
	}
	other := &settings.Settings{Store: m, KeyFile: keyFile(t, key+"-rotated")}
	if _, err := other.Model(t.Context()); err == nil || !strings.Contains(err.Error(), "settings key changed") {
		t.Errorf("reading with another key: %v", err)
	}
}
