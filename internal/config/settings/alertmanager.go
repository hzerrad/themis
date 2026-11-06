package settings

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/store"
)

// The alerting settings setup can make.
const (
	// AlertmanagerAddress is the address setup gives Alertmanager.
	AlertmanagerAddress = "alerting.alertmanager.address"
	// AlertsMode is how alerts reach Themis: poll or push.
	AlertsMode = "alerting.mode"
)

// Where the Alertmanager address in use comes from.
const (
	// FromInstall: the install set it, in Helm or the configuration file.
	FromInstall = "install"
	// FromFound: the chart found it in the cluster at install.
	FromFound = "found"
	// FromSetup: set on the Setup page.
	FromSetup = "setup"
)

// Why setup refuses an alerting setting.
var (
	ErrPushByInstall = errors.New("push mode is set in the Helm values, so Themis doesn't read Alertmanager")
	ErrNotHTTPURL    = errors.New("alertmanager's address must be an http or https URL, such as http://alertmanager-operated.monitoring:9093")
	ErrUnknownMode   = errors.New("the mode must be poll or push")
)

// AlertmanagerView is how alerts reach Themis: the mode, and the
// Alertmanager Themis reads in poll mode, with where its address comes from.
// From is empty when there is no address.
type AlertmanagerView struct {
	// Mode is poll or push.
	Mode string `json:"mode"`
	// ModeByInstall says the install fixed the mode, so setup can't switch it.
	ModeByInstall bool `json:"modeByInstall"`
	// ModeSince is when setup last switched the mode; zero when it never did.
	// What was read before it says nothing about the mode in use.
	ModeSince time.Time `json:"modeSince,omitzero"`

	Address string `json:"address"`
	From    string `json:"from"`
	// Found is the address the chart found at install, which one set on the
	// Setup page replaces.
	Found string `json:"found,omitempty"`
}

// Polling says Themis reads Alertmanager.
func (v AlertmanagerView) Polling() bool { return v.Mode == config.PollMode }

// Alertmanager returns how alerts reach Themis: the mode the install fixed,
// else setup's, else poll; and the address Themis reads: the install's, then
// setup's, then the one the chart found.
func (s *Settings) Alertmanager(ctx context.Context) (AlertmanagerView, error) {
	in := s.InstallAlerting
	install := in.Alertmanager
	stored, err := s.Store.Settings(ctx, []string{AlertmanagerAddress, AlertsMode})
	if err != nil {
		return AlertmanagerView{}, err
	}
	v := AlertmanagerView{Mode: in.FixedMode(), ModeByInstall: in.FixedMode() != ""}
	if !v.ModeByInstall {
		v.Mode = config.PollMode
		if m, ok := stored[AlertsMode]; ok {
			v.ModeSince = m.UpdatedAt
			if m.Value == config.PushMode {
				v.Mode = config.PushMode
			}
		}
	}
	if v.ModeByInstall && v.Mode == config.PushMode {
		return v, nil
	}
	if install.Address != "" && !install.Found {
		v.Address, v.From = install.Address, FromInstall
		return v, nil
	}
	if install.Found {
		v.Found = install.Address
	}
	switch set, ok := stored[AlertmanagerAddress]; {
	case ok:
		v.Address, v.From = set.Value, FromSetup
	case install.Found:
		v.Address, v.From = install.Address, FromFound
	}
	return v, nil
}

// SetAlertmanager saves the address setup gives Alertmanager, set by by. An
// empty address removes it, so the one found at install applies again. It
// refuses to change an address the install set.
func (s *Settings) SetAlertmanager(ctx context.Context, address, by string) error {
	in := s.InstallAlerting
	if in.FixedMode() == config.PushMode {
		return ErrPushByInstall
	}
	if in.Alertmanager.Address != "" && !in.Alertmanager.Found {
		return fmt.Errorf("%w: %s", ErrSetByInstall, AlertmanagerAddress)
	}
	address = strings.TrimRight(strings.TrimSpace(address), "/")
	if address == "" {
		return s.Store.PutSettings(ctx, nil, []string{AlertmanagerAddress})
	}
	if !config.IsHTTPURL(address) {
		return ErrNotHTTPURL
	}
	return s.Store.PutSettings(ctx, []store.Setting{{Name: AlertmanagerAddress, Value: address, UpdatedAt: s.now(), UpdatedBy: by}}, nil)
}

// SetMode switches between poll and push mode, set by by. It refuses to
// switch a mode the install fixed. Switching to the mode in use changes
// nothing.
//
// What Themis read before is kept: once reading resumes, an alert that ended
// meanwhile is taken as resolved, and an alert's start keeps that from
// resolving a later firing of it.
func (s *Settings) SetMode(ctx context.Context, mode, by string) error {
	if s.InstallAlerting.FixedMode() != "" {
		return fmt.Errorf("%w: %s", ErrSetByInstall, AlertsMode)
	}
	if mode != config.PollMode && mode != config.PushMode {
		return ErrUnknownMode
	}
	v, err := s.Alertmanager(ctx)
	if err != nil || v.Mode == mode {
		return err
	}
	return s.Store.PutSettings(ctx, []store.Setting{{Name: AlertsMode, Value: mode, UpdatedAt: s.now(), UpdatedBy: by}}, nil)
}
