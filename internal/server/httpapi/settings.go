package httpapi

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/hzerrad/themis/internal/config/settings"
	"github.com/hzerrad/themis/internal/server/modeltest"
)

// ModelSettings are the model settings made in Themis's setup.
type ModelSettings interface {
	View(ctx context.Context) (settings.ModelView, error)
	SetModel(ctx context.Context, change settings.ModelChange, by string) error
	Trial(ctx context.Context, change settings.ModelChange) (settings.Model, settings.ModelView, error)
}

func (a *API) modelSettings(w http.ResponseWriter, r *http.Request) {
	v, err := a.Settings.View(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.reply(w, r, v)
}

// setModelSettings changes the model settings the install left unset. The
// investigator picks them up before its next investigation.
func (a *API) setModelSettings(w http.ResponseWriter, r *http.Request) {
	var change settings.ModelChange
	if !a.decode(w, r, &change) {
		return
	}
	if !a.settingsDone(w, r, a.Settings.SetModel(r.Context(), change, actor(r))) {
		return
	}
	var changed []string
	for _, c := range []struct {
		name string
		set  bool
	}{
		{"provider", change.Provider != nil}, {"address", change.BaseURL != nil}, {"name", change.Name != nil}, {"key", change.APIKey != nil},
		{"strict output", change.StrictOutput != nil}, {"schema dialect", change.SchemaDialect != nil},
	} {
		if c.set {
			changed = append(changed, c.name)
		}
	}
	// What changed, never the values: the key is a secret.
	a.logger().InfoContext(r.Context(), "the model settings were changed", "settings", strings.Join(changed, ", "), "by", actor(r))
	a.modelSettings(w, r)
}

// settingsDone answers a change of the model settings that err refused, and
// says whether it went through.
func (a *API) settingsDone(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case errors.Is(err, settings.ErrSetByInstall):
		http.Error(w, "That was set when Themis was installed. Change it in the Helm values instead.", http.StatusConflict)
	case err != nil && strings.Contains(err.Error(), "settings key"):
		a.fail(w, r, err)
	case err != nil:
		http.Error(w, sentence(err), http.StatusBadRequest)
	default:
		return true
	}
	return false
}

// modelTestView is what a test of the model's connection found.
type modelTestView struct {
	OK      bool   `json:"ok"`
	Outcome string `json:"outcome"`
	Text    string `json:"text"`
}

// testModel tests the model's connection with the settings in the form,
// saved or not; nothing is saved. Only the investigator may reach the model,
// so it makes the request. A key typed in the form goes to it for this test
// only.
func (a *API) testModel(w http.ResponseWriter, r *http.Request) {
	var change settings.ModelChange
	if !a.decode(w, r, &change) {
		return
	}
	m, view, err := a.Settings.Trial(r.Context(), change)
	if !a.settingsDone(w, r, err) {
		return
	}
	result := modeltest.Result{Outcome: modeltest.Incomplete, Missing: view.Missing()}
	// Anthropic's own API refuses every request without a key.
	if view.Provider == "anthropic" && view.BaseURL == "" && !view.APIKeySet {
		result.Missing = append(result.Missing, settings.ModelAPIKey)
	}
	if len(result.Missing) == 0 {
		result, err = a.ModelTests.Run(r.Context(), m)
		switch {
		case errors.Is(err, modeltest.ErrBusy):
			http.Error(w, "Refused. A test of the connection is already running.", http.StatusConflict)
			return
		case err != nil:
			a.fail(w, r, err)
			return
		}
	}
	// The investigator leaves the key out too: this covers a provider that
	// quotes it back.
	if len(m.APIKey) >= 8 {
		result.Detail = strings.ReplaceAll(result.Detail, m.APIKey, "…")
	}
	if result.Provider == "" {
		result.Provider = view.Provider
	}
	// The outcome, never the key or what the model was sent.
	a.logger().InfoContext(r.Context(), "the model's connection was tested", "outcome", string(result.Outcome), "provider", result.Provider,
		"status", result.Status, "latency", result.Latency, "by", actor(r))
	byInstall := func(name string) bool { return slices.Contains(view.ByInstall, name) }
	a.reply(w, r, modelTestView{OK: result.OK(), Outcome: string(result.Outcome), Text: result.Text(byInstall)})
}
