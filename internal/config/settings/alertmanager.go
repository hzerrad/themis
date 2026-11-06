package settings

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/store"
)

// AlertmanagerAddress is the address setup gives Alertmanager.
const AlertmanagerAddress = "alerting.alertmanager.address"

// Where the Alertmanager address in use comes from.
const (
	// FromInstall: the install set it, in Helm or the configuration file.
	FromInstall = "install"
	// FromFound: the chart found it in the cluster at install.
	FromFound = "found"
	// FromSetup: set on the Setup page.
	FromSetup = "setup"
)

// Why setup refuses an Alertmanager address.
var (
	ErrReadingOff = errors.New("reading Alertmanager is turned off in the Helm values")
	ErrNotHTTPURL = errors.New("alertmanager's address must be an http or https URL, such as http://alertmanager-operated.monitoring:9093")
)

// AlertmanagerView is the Alertmanager Themis reads: its address, and where
// that comes from. From is empty when there is no address.
type AlertmanagerView struct {
	Address string `json:"address"`
	From    string `json:"from"`
	// Found is the address the chart found at install, which one set on the
	// Setup page replaces.
	Found string `json:"found,omitempty"`
	// Reading is false when the install turned reading off.
	Reading bool `json:"reading"`
}

// Alertmanager returns the address Themis reads: the install's, then
// setup's, then the one the chart found.
func (s *Settings) Alertmanager(ctx context.Context) (AlertmanagerView, error) {
	install := s.InstallAlertmanager
	v := AlertmanagerView{Reading: !install.Disabled}
	if install.Disabled {
		return v, nil
	}
	if install.Address != "" && !install.Found {
		v.Address, v.From = install.Address, FromInstall
		return v, nil
	}
	if install.Found {
		v.Found = install.Address
	}
	stored, err := s.Store.Settings(ctx, []string{AlertmanagerAddress})
	if err != nil {
		return AlertmanagerView{}, err
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
	install := s.InstallAlertmanager
	if install.Disabled {
		return ErrReadingOff
	}
	if install.Address != "" && !install.Found {
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
