package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/hzerrad/themis/internal/config/settings"
)

// ModelSettings are the model settings made in Themis's setup.
type ModelSettings interface {
	View(ctx context.Context) (settings.ModelView, error)
	SetModel(ctx context.Context, change settings.ModelChange, by string) error
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
	err := a.Settings.SetModel(r.Context(), change, actor(r))
	switch {
	case errors.Is(err, settings.ErrSetByInstall):
		http.Error(w, "That was set when Themis was installed. Change it in the Helm values instead.", http.StatusConflict)
		return
	case err != nil && strings.Contains(err.Error(), "settings key"):
		a.fail(w, r, err)
		return
	case err != nil:
		http.Error(w, sentence(err), http.StatusBadRequest)
		return
	}
	var changed []string
	for _, c := range []struct {
		name string
		set  bool
	}{{"address", change.BaseURL != nil}, {"name", change.Name != nil}, {"key", change.APIKey != nil}, {"strict output", change.StrictOutput != nil}} {
		if c.set {
			changed = append(changed, c.name)
		}
	}
	// What changed, never the values: the key is a secret.
	a.logger().InfoContext(r.Context(), "the model settings were changed", "settings", strings.Join(changed, ", "), "by", actor(r))
	a.modelSettings(w, r)
}
