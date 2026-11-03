// Package intake turns alerts from any source into incidents.
package intake

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/riverqueue/river"
)

// AlertStatus is whether an alert's problem is happening or has stopped.
type AlertStatus string

const (
	AlertFiring   AlertStatus = "firing"
	AlertResolved AlertStatus = "resolved"
)

// Alert is one alert, translated from its source's format.
type Alert struct {
	Source string `json:"source"`

	// SourceID pairs the firing and resolved notifications of one alert.
	SourceID string `json:"sourceID"`

	Name   string      `json:"name"`
	Status AlertStatus `json:"status"`

	// Labels use kube-state-metrics names (namespace, pod, deployment...).
	Labels map[string]string `json:"labels"`

	// StartsAt and EndsAt are the sender's times; EndsAt is zero while firing.
	StartsAt time.Time `json:"startsAt"`
	EndsAt   time.Time `json:"endsAt"`

	ReceivedAt time.Time       `json:"receivedAt"`
	Payload    json.RawMessage `json:"payload"`
}

// Reasons an alert cannot be translated. Each wraps ErrInvalidAlert.
var (
	ErrInvalidAlert           = errors.New("invalid alert")
	ErrMissingAlertName       = errors.New("alert has no name")
	ErrUnsupportedAlertStatus = errors.New("unsupported alert status")
)

// InvalidAlert is an alert that could not be translated; it is recorded as
// an invalid_alert rejection.
type InvalidAlert struct {
	Source     string
	Name       string
	Labels     map[string]string
	Err        error
	ReceivedAt time.Time
	Payload    json.RawMessage
}

// processAlertMaxAttempts gives up after about 4 hours of River's backoff,
// Alertmanager's default repeat_interval.
const processAlertMaxAttempts = 10

// ProcessAlertArgs turn a received alert into an incident or a rejection.
// The job is the inbox. River encodes args with encoding/json v1.
type ProcessAlertArgs struct {
	Alert Alert `json:"alert"`
}

// Kind is persisted with each job and must never change.
func (ProcessAlertArgs) Kind() string { return "process_alert" }

func (ProcessAlertArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: processAlertMaxAttempts}
}
