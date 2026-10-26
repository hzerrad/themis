// Package alertmanager is the intake source for Alertmanager webhooks.
package alertmanager

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/alertmanager/template"

	"github.com/hzerrad/themis/internal/intake"
)

const Source = "alertmanager"

// PayloadVersion is the only webhook payload version accepted.
const PayloadVersion = "4"

// Reasons an Alertmanager webhook payload is refused.
var (
	ErrUnsupportedVersion = errors.New("unsupported Alertmanager payload version")
	ErrParseFailed        = errors.New("failed to parse Alertmanager payload")
)

// Payload mirrors Alertmanager's webhook.Message.
type Payload struct {
	template.Data

	Version string `json:"version"`

	// TruncatedAlerts were dropped by Alertmanager beyond max_alerts.
	TruncatedAlerts uint64 `json:"truncatedAlerts"`
}

// ParsePayload decodes a webhook body; the caller bounds its size.
func ParsePayload(b []byte) (*Payload, error) {
	var payload Payload
	if err := json.Unmarshal(b, &payload); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrParseFailed, err)
	}
	if payload.Version != PayloadVersion {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedVersion, payload.Version)
	}
	return &payload, nil
}

// toAlerts translates each alert. An untranslatable one is returned as
// invalid, so it does not cost the others: Alertmanager never retries a 4xx.
func (p *Payload) toAlerts(receivedAt time.Time) ([]intake.Alert, []intake.InvalidAlert, error) {
	alerts := make([]intake.Alert, 0, len(p.Alerts))
	var invalid []intake.InvalidAlert

	for i, a := range p.Alerts {
		payload, err := json.Marshal(a)
		if err != nil {
			return nil, nil, fmt.Errorf("encode alert %d: %w", i, err)
		}

		name := a.Labels["alertname"]
		status, err := parseStatus(a.Status)
		if name == "" {
			err = intake.ErrMissingAlertName
		}
		if err != nil {
			invalid = append(invalid, intake.InvalidAlert{
				Source:     Source,
				Name:       name,
				Labels:     a.Labels,
				Err:        fmt.Errorf("%w: alert %d: %w", intake.ErrInvalidAlert, i, err),
				ReceivedAt: receivedAt,
				Payload:    payload,
			})
			continue
		}

		alerts = append(alerts, intake.Alert{
			Source:     Source,
			SourceID:   a.Fingerprint,
			Name:       name,
			Status:     status,
			Labels:     a.Labels,
			StartsAt:   a.StartsAt,
			EndsAt:     a.EndsAt,
			ReceivedAt: receivedAt,
			Payload:    payload,
		})
	}

	return alerts, invalid, nil
}

func parseStatus(s string) (intake.AlertStatus, error) {
	switch s {
	case "firing":
		return intake.AlertFiring, nil
	case "resolved":
		return intake.AlertResolved, nil
	default:
		return "", fmt.Errorf("%w: %q", intake.ErrUnsupportedAlertStatus, s)
	}
}
