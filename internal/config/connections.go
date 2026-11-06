package config

import (
	"errors"
	"fmt"

	"github.com/hzerrad/themis/internal/platform/token"
)

// Settings configures the settings made in Themis's setup.
type Settings struct {
	// KeyFile holds the secret that encrypts secret settings, such as the
	// model's key. Losing it loses them; changing it makes them unreadable.
	KeyFile string `json:"keyFile" jsonschema:"minLength=1"`
}

// Model says which of the investigator's model settings the install set.
// Those win over setup's.
type Model struct {
	// Provider is the API the investigator speaks: openai, the default, or
	// anthropic, which has its own address, so setup's doesn't apply.
	Provider string `json:"provider,omitempty" jsonschema:"enum=openai,enum=anthropic"`
	BaseURL  string `json:"baseURL,omitempty"`
	Name     string `json:"name,omitempty"`
	// APIKey says the install gives the investigator a key.
	APIKey bool `json:"apiKey,omitempty"`
	// StrictOutput is the install's strict output, which applies with its
	// address. Nil means on.
	StrictOutput *bool `json:"strictOutput,omitempty"`
	// SchemaDialect is how the reply schema is sent to an OpenAI-compatible
	// API: as-is, or openai-strict, the form OpenAI's strict mode accepts.
	// Empty leaves it to setup.
	SchemaDialect string `json:"schemaDialect,omitempty" jsonschema:"enum=as-is,enum=openai-strict"`
}

// ModelProviders are the APIs the investigator speaks.
var ModelProviders = []string{"openai", "anthropic"}

// SchemaDialects are the forms an OpenAI-compatible API can be sent the reply
// schema in.
var SchemaDialects = []string{"as-is", "openai-strict"}

// OwnAddress says the provider's API has a single address of its own, so a
// model there needs only a name.
func (m Model) OwnAddress() bool { return m.Provider == "anthropic" }

// Resources configures reading configuration from custom resources.
type Resources struct {
	// Namespace is Themis's own. An IncidentProfile here applies to every
	// enrolled namespace; one elsewhere applies only to its own namespace.
	Namespace string `json:"namespace" jsonschema:"minLength=1"`
}

// ThemisConfigSpec is the spec of the ThemisConfig resource: settings a
// platform team changes without reinstalling.
type ThemisConfigSpec struct {
	// EnrolledNamespaces narrows the namespaces given at install, which hold
	// Themis's permissions. A namespace outside them is reported and ignored.
	EnrolledNamespaces []string `json:"enrolledNamespaces,omitempty"`

	// AutonomyCeiling is the most any profile may do: guarded, or observe to
	// stop Themis making changes anywhere. Defaults to guarded.
	AutonomyCeiling string `json:"autonomyCeiling,omitempty" jsonschema:"enum=guarded,enum=observe"`

	// ExternalProposals lets callers outside Themis suggest fixes.
	ExternalProposals ExternalProposals `json:"externalProposals,omitzero"`
}

// Intake configures the Alertmanager webhook.
type Intake struct {
	// BearerTokenFile holds the token Alertmanager must send as a bearer token.
	BearerTokenFile string `json:"bearerTokenFile" jsonschema:"minLength=1"`

	// URL is where Alertmanager reaches the intake, for the setup steps
	// Themis shows. The chart sets it to the core's Service.
	URL string `json:"url,omitempty"`

	// TokenSecret names the Secret, in Themis's namespace, whose intake key
	// holds the token, for the setup steps Themis shows. The chart sets it.
	TokenSecret string `json:"tokenSecret,omitempty"`
}

// Investigators configures the investigators' gRPC API.
type Investigators struct {
	// BearerTokenFile holds the token investigators must send as a bearer token.
	BearerTokenFile string `json:"bearerTokenFile" jsonschema:"minLength=1"`

	// Disabled means no investigator is installed: Themis collects evidence
	// and runs its checks, then each incident waits for a fix suggested by a
	// person or a tool outside Themis.
	Disabled bool `json:"disabled,omitempty"`
}

// Executor configures the connection to themis-executor.
type Executor struct {
	Address string `json:"address" jsonschema:"minLength=1"`

	// BearerTokenFile holds the token the executor requires.
	BearerTokenFile string `json:"bearerTokenFile" jsonschema:"minLength=1"`
}

// Notifications configures outbound notices.
type Notifications struct {
	// Webhook receives a JSON POST for every escalation.
	Webhook *Webhook `json:"webhook,omitempty"`

	// ExternalURL is where people reach the UI, for links in notices.
	ExternalURL string `json:"externalURL,omitempty"`
}

// Webhook is an HTTP endpoint for notices: URL, or URLFile when the URL
// embeds a key and comes from a Secret.
type Webhook struct {
	URL     string `json:"url,omitempty"`
	URLFile string `json:"urlFile,omitempty"`

	// BearerTokenFile holds a token sent as a bearer token, if the receiver
	// needs one.
	BearerTokenFile string `json:"bearerTokenFile,omitempty"`
}

// Target is the URL to post notices to: URL, or what URLFile holds, read
// each time so a rotated Secret takes effect without a restart. It must be an
// http or https URL.
func (w Webhook) Target() (string, error) {
	target := w.URL
	if w.URLFile != "" {
		b, err := token.Read(w.URLFile)
		if err != nil {
			return "", fmt.Errorf("webhook URL: %w", err)
		}
		target = string(b)
	}
	if !IsHTTPURL(target) {
		return "", errors.New("the webhook URL is not an http or https URL")
	}
	return target, nil
}
