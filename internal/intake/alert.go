package intake

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/riverqueue/river"
)

// AlertStatus is whether an alert's problem is happening or has stopped.
// Sources with more states map them onto these two.
type AlertStatus string

const (
	AlertFiring   AlertStatus = "firing"
	AlertResolved AlertStatus = "resolved"
)

// Alert is one alert from any source, in the shape the processing job uses.
// Each source's adapter translates its own format into this, so processing
// does not depend on who sent the alert.
type Alert struct {
	// Source names the sender, for example alertmanager.
	Source string `json:"source"`

	// SourceID identifies the alert at its source, for example Alertmanager's
	// fingerprint. It stays the same between the firing and the resolved
	// notification of one alert, so they can be paired.
	SourceID string `json:"sourceID"`

	// Name identifies the kind of alert, for example KubePodCrashLooping. It is
	// what profiles match in alertNames.
	Name string `json:"name"`

	Status AlertStatus `json:"status"`

	// Labels describe what the alert is about. They are used to resolve the
	// target and to match profiles. Keys use Themis's label names, which are
	// those of kube-state-metrics and the kubelet: namespace, pod, deployment
	// and the others in ObjectLabels. An adapter for a source with other names
	// renames them, for example kube_deployment to deployment.
	Labels map[string]string `json:"labels"`

	// StartsAt and EndsAt are the sender's times for the problem. EndsAt is the
	// zero time while the alert is firing. They order a firing and a resolved
	// notification that are processed out of order.
	StartsAt time.Time `json:"startsAt"`
	EndsAt   time.Time `json:"endsAt"`

	// ReceivedAt is when Themis received the alert. Processing may happen
	// later, after retries.
	ReceivedAt time.Time `json:"receivedAt"`

	// Payload is the alert as the source sent it, re-encoded from its parsed
	// form. It is stored with the occurrence or the rejection the alert becomes.
	Payload json.RawMessage `json:"payload"`
}

// Reasons an alert cannot be translated. Each wraps ErrInvalidAlert.
var (
	// ErrInvalidAlert an alert in a payload cannot become an Alert. The rest of
	// the payload is still accepted.
	ErrInvalidAlert = errors.New("invalid alert")

	// ErrMissingAlertName the alert has no name, so no profile can match it.
	ErrMissingAlertName = errors.New("alert has no name")

	// ErrUnsupportedAlertStatus the alert's status is neither firing nor
	// resolved.
	ErrUnsupportedAlertStatus = errors.New("unsupported alert status")
)

// InvalidAlert is an alert from a payload that could not be translated. It is
// recorded as a rejection with reason invalid_alert instead of being dropped.
type InvalidAlert struct {
	Source string

	// Name is the alert's name if it had one.
	Name string

	// Labels are the alert's labels as sent, possibly nil.
	Labels map[string]string

	// Err says why, and names the alert's position in the payload. It wraps
	// ErrInvalidAlert and a specific reason such as ErrMissingAlertName.
	Err error

	ReceivedAt time.Time
	Payload    json.RawMessage
}

// processAlertMaxAttempts bounds retries of a process_alert job. River's
// backoff is attempt^4 seconds, so 10 attempts give up after about 4 hours.
// By then the alert is stale, and Alertmanager's default repeat_interval of
// 4 hours has sent it again if it is still firing.
const processAlertMaxAttempts = 10

// ProcessAlertArgs are the arguments of the job that turns a received alert
// into an incident occurrence or a rejection. The job itself is the inbox: the
// alert is stored durably in it until processing succeeds or is given up.
//
// River stores job arguments with encoding/json (v1), so this file uses v1.
type ProcessAlertArgs struct {
	Alert Alert `json:"alert"`
}

// Kind is the job's type name. It is persisted with every job, so it must
// never change once released: queued jobs with the old name would never run.
func (ProcessAlertArgs) Kind() string { return "process_alert" }

// InsertOpts are the defaults for every process_alert job.
func (ProcessAlertArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: processAlertMaxAttempts}
}
