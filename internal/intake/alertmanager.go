package intake

import (
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/prometheus/alertmanager/template"
)

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
