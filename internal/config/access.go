package config

import (
	"slices"
	"time"
)

// API configures the HTTP API.
type API struct {
	// BearerTokenFile holds the admin token. It stays usable when people sign
	// in through OIDC, as a way in if the identity provider is down.
	BearerTokenFile string `json:"bearerTokenFile" jsonschema:"minLength=1"`

	// InitialAdmin lets the first admin sign in with a generated password, to
	// make the first admin account. Set it or AdminAccount, not both.
	InitialAdmin *InitialAdmin `json:"initialAdmin,omitempty"`

	// AdminAccount is an admin account Themis makes at startup when no admin
	// account exists. Set it or InitialAdmin, not both.
	AdminAccount *AdminAccount `json:"adminAccount,omitempty"`

	// OIDC lets people sign in to the UI through an identity provider.
	OIDC *OIDC `json:"oidc,omitempty"`

	// Access gives people roles, by username or group. Someone signed in who
	// matches no binding can't see anything.
	Access []AccessBinding `json:"access,omitempty"`
}

// InitialAdmin is the password the first admin signs in with.
type InitialAdmin struct {
	// PasswordFile holds the password. It is read at each try, and never
	// stored.
	PasswordFile string `json:"passwordFile" jsonschema:"minLength=1"`

	// Secret names the Secret, in Themis's namespace, whose password key
	// holds it, for the command the sign-in page shows. The chart sets it.
	Secret string `json:"secret,omitempty"`
}

// AdminAccount is an admin account the install configures. Themis makes it
// only while no admin account exists, and never changes it after: its
// password changes as any account's does.
type AdminAccount struct {
	// Username names the account.
	Username string `json:"username" jsonschema:"minLength=1"`

	// PasswordHashFile holds the password's hash: argon2id in the PHC string
	// format, or bcrypt.
	PasswordHashFile string `json:"passwordHashFile,omitempty"`

	// PasswordFile holds the password itself, for secrets tools that can't
	// hash it. Themis hashes it, and uses it only when the hash file doesn't
	// exist.
	PasswordFile string `json:"passwordFile,omitempty"`
}

// OIDC is an identity provider people sign in with.
type OIDC struct {
	IssuerURL string `json:"issuerURL" jsonschema:"minLength=1"`
	ClientID  string `json:"clientID" jsonschema:"minLength=1"`
	// ClientSecretFile holds the client secret.
	ClientSecretFile string `json:"clientSecretFile" jsonschema:"minLength=1"`
	// RedirectURL is Themis's /auth/callback as the browser reaches it.
	RedirectURL string `json:"redirectURL" jsonschema:"minLength=1"`

	// UsernameClaim names the ID token claim used as the username (email).
	UsernameClaim string `json:"usernameClaim,omitempty"`
	// GroupsClaim names the ID token claim listing the person's groups (groups).
	GroupsClaim string `json:"groupsClaim,omitempty"`
	// Scopes are asked of the provider; openid is always added. Some
	// providers need groups to send groups, and some refuse it (openid, email,
	// profile).
	Scopes []string `json:"scopes,omitempty"`
	// SessionTTL is how long a sign-in lasts (8h).
	SessionTTL Duration `json:"sessionTTL,omitzero"`
}

// Roles, from least to most trusted. Each can do what the ones before it can.
const (
	// RoleViewer sees incidents, fixes and their outcomes.
	RoleViewer = "viewer"
	// RoleResponder also sees raw evidence, such as logs.
	RoleResponder = "responder"
	// RoleApprover also suggests, approves and denies fixes.
	RoleApprover = "approver"
	// RoleAdmin can do everything.
	RoleAdmin = "admin"
)

// Roles lists every role, least trusted first.
var Roles = []string{RoleViewer, RoleResponder, RoleApprover, RoleAdmin}

// AccessBinding gives a role to people by username and by group.
type AccessBinding struct {
	Role   string   `json:"role" jsonschema:"enum=viewer,enum=responder,enum=approver,enum=admin"`
	Users  []string `json:"users,omitempty"`
	Groups []string `json:"groups,omitempty"`
}

// Defaults for OIDC sign-in.
const (
	DefaultUsernameClaim = "email"
	DefaultGroupsClaim   = "groups"
	DefaultSessionTTL    = 8 * time.Hour
)

// WithDefaults returns o with its defaults filled in.
func (o OIDC) WithDefaults() OIDC {
	if o.UsernameClaim == "" {
		o.UsernameClaim = DefaultUsernameClaim
	}
	if o.GroupsClaim == "" {
		o.GroupsClaim = DefaultGroupsClaim
	}
	if o.SessionTTL.Duration == 0 {
		o.SessionTTL.Duration = DefaultSessionTTL
	}
	if len(o.Scopes) == 0 {
		o.Scopes = []string{"email", "profile"}
	}
	if !slices.Contains(o.Scopes, "openid") {
		o.Scopes = append([]string{"openid"}, o.Scopes...)
	}
	return o
}
