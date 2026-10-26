package intake

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/alertmanager/template"
)

// AlertmanagerSource is the Source of alerts from Alertmanager.
const AlertmanagerSource = "alertmanager"

// AlertmanagerPayloadVersion is the Alertmanager webhook payload version Themis accepts.
// Payloads of any other version are rejected rather than partly understood.
const AlertmanagerPayloadVersion = "4"

// Reasons an Alertmanager webhook payload is refused. Both wrap the details.
var (
	// ErrUnsupportedAlertmanagerPayloadVersion the payload's version is not
	// AlertmanagerPayloadVersion, including when it is missing.
	ErrUnsupportedAlertmanagerPayloadVersion = errors.New("unsupported Alertmanager payload version")

	// ErrParseFailed the body is not a single, well-formed JSON object of the
	// payload's shape, for example with duplicate keys or trailing data.
	ErrParseFailed = errors.New("failed to parse Alertmanager payload")
)

// AlertmanagerPayload is the body of an Alertmanager webhook request. It
// mirrors webhook.Message from github.com/prometheus/alertmanager/notify/webhook.
type AlertmanagerPayload struct {
	template.Data

	// Version is the payload format version; see AlertmanagerPayloadVersion.
	Version string `json:"version"`

	// TruncatedAlerts is the number of alerts Alertmanager dropped from this
	// payload because the receiver's max_alerts was exceeded. Themis never sees
	// those alerts, so a non-zero value must be surfaced to the operator.
	TruncatedAlerts uint64 `json:"truncatedAlerts"`
}

// ParseAlertmanagerPayload decodes an Alertmanager webhook request body. It
// does not limit the body's size; the caller reads it through a bounded reader.
func ParseAlertmanagerPayload(b []byte) (*AlertmanagerPayload, error) {
	var payload AlertmanagerPayload
	if err := json.Unmarshal(b, &payload); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrParseFailed, err)
	}
	if payload.Version != AlertmanagerPayloadVersion {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedAlertmanagerPayloadVersion, payload.Version)
	}
	return &payload, nil
}

// toAlerts translates each alert in the payload. An alert that cannot be
// translated is returned as an InvalidAlert, and the others are still
// returned: one malformed alert must not cost the valid ones, which a
// rejected request would, because Alertmanager does not retry a 4xx.
// The error is for failures unrelated to any one alert's content.
func (p *AlertmanagerPayload) toAlerts(receivedAt time.Time) ([]Alert, []InvalidAlert, error) {
	alerts := make([]Alert, 0, len(p.Alerts))
	var invalid []InvalidAlert

	for i, a := range p.Alerts {
		payload, err := json.Marshal(a)
		if err != nil {
			return nil, nil, fmt.Errorf("encode alert %d: %w", i, err)
		}

		name := a.Labels["alertname"]
		status, err := alertmanagerStatus(a.Status)
		if name == "" {
			err = ErrMissingAlertName
		}
		if err != nil {
			invalid = append(invalid, InvalidAlert{
				Source:     AlertmanagerSource,
				Name:       name,
				Labels:     a.Labels,
				Err:        fmt.Errorf("%w: alert %d: %w", ErrInvalidAlert, i, err),
				ReceivedAt: receivedAt,
				Payload:    payload,
			})
			continue
		}

		alerts = append(alerts, Alert{
			Source:     AlertmanagerSource,
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

// alertmanagerStatus maps an Alertmanager alert status onto AlertStatus.
func alertmanagerStatus(s string) (AlertStatus, error) {
	switch s {
	case "firing":
		return AlertFiring, nil
	case "resolved":
		return AlertResolved, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnsupportedAlertStatus, s)
	}
}
