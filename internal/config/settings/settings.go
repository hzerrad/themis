// Package settings keeps the settings made in Themis's setup: what the install
// itself left unset. Secret values are encrypted before they reach the
// database, with a key the database never sees.
package settings

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/platform/token"
	"github.com/hzerrad/themis/internal/store"
)

// The model settings setup can make.
const (
	ModelProvider      = "model.provider"
	ModelBaseURL       = "model.baseURL"
	ModelName          = "model.name"
	ModelAPIKey        = "model.apiKey"
	ModelStrictOutput  = "model.strictOutput"
	ModelSchemaDialect = "model.schemaDialect"
)

var modelSettings = []string{ModelProvider, ModelBaseURL, ModelName, ModelAPIKey, ModelStrictOutput, ModelSchemaDialect}

// ErrSetByInstall means a setting the install made was to be changed in setup.
var ErrSetByInstall = errors.New("set by the install")

// Store keeps settings.
type Store interface {
	Settings(ctx context.Context, names []string) (map[string]store.Setting, error)
	PutSettings(ctx context.Context, set []store.Setting, clear []string) error
}

// Settings reads and writes setup's settings.
type Settings struct {
	Store Store
	// KeyFile holds the secret the encryption key is derived from.
	KeyFile string
	// Install is what the install set, which setup leaves alone.
	Install config.Model
	Now     func() time.Time
}

// Model is the investigator's model, as setup set it. Empty values were not
// set.
type Model struct {
	Provider string
	BaseURL  string
	Name     string
	APIKey   string
	// StrictOutput is nil when setup didn't set it.
	StrictOutput  *bool
	SchemaDialect string
}

// ModelView is the model settings for the setup page: what the investigator
// uses, and what the install set. The key itself is never shown.
type ModelView struct {
	// Provider is the API the investigator speaks: openai or anthropic.
	Provider      string `json:"provider"`
	BaseURL       string `json:"baseURL"`
	Name          string `json:"name"`
	APIKeySet     bool   `json:"apiKeySet"`
	StrictOutput  bool   `json:"strictOutput"`
	SchemaDialect string `json:"schemaDialect"`
	// ByInstall lists the settings the install made, which setup can't change.
	ByInstall []string `json:"byInstall"`
	// Complete says the model can be used: it has a name, and an address
	// unless its provider has its own.
	Complete bool `json:"complete"`
}

// Missing lists what the model needs before it can be used: its address,
// unless its provider has its own, and its name.
func (v ModelView) Missing() []string {
	var missing []string
	if v.BaseURL == "" && !v.ownAddress() {
		missing = append(missing, ModelBaseURL)
	}
	if v.Name == "" {
		missing = append(missing, ModelName)
	}
	return missing
}

func (v ModelView) ownAddress() bool { return config.Model{Provider: v.Provider}.OwnAddress() }

// installSet lists the settings the install made. The provider, the address,
// strict output and the schema dialect describe one endpoint: an address set
// at install brings the others with it, and a provider with its own address
// leaves none of them to setup.
func (s *Settings) installSet() []string {
	in := s.Install
	var set []string
	if in.Provider != "" || in.BaseURL != "" {
		set = append(set, ModelProvider)
	}
	if in.BaseURL != "" || in.OwnAddress() {
		set = append(set, ModelBaseURL, ModelStrictOutput)
	}
	if in.SchemaDialect != "" || in.BaseURL != "" || in.OwnAddress() {
		set = append(set, ModelSchemaDialect)
	}
	if in.Name != "" {
		set = append(set, ModelName)
	}
	if in.APIKey {
		set = append(set, ModelAPIKey)
	}
	return set
}

func (s *Settings) byInstall(name string) bool {
	return slices.Contains(s.installSet(), name)
}

// Model returns what setup set for the model, with secrets in the clear, for
// the investigator to use where its own environment sets nothing.
func (s *Settings) Model(ctx context.Context) (Model, error) {
	stored, err := s.Store.Settings(ctx, modelSettings)
	if err != nil {
		return Model{}, err
	}
	m := Model{
		Provider: stored[ModelProvider].Value, BaseURL: stored[ModelBaseURL].Value, Name: stored[ModelName].Value,
		SchemaDialect: stored[ModelSchemaDialect].Value,
	}
	if k, ok := stored[ModelAPIKey]; ok {
		if m.APIKey, err = s.open(k.Value); err != nil {
			return Model{}, fmt.Errorf("read the model's key: %w", err)
		}
	}
	if v, ok := stored[ModelStrictOutput]; ok {
		b, err := strconv.ParseBool(v.Value)
		if err != nil {
			return Model{}, fmt.Errorf("read %s: %w", ModelStrictOutput, err)
		}
		m.StrictOutput = &b
	}
	return m, nil
}

// View returns the model settings for the setup page: the install's where it
// set them, setup's otherwise.
func (s *Settings) View(ctx context.Context) (ModelView, error) {
	m, err := s.Model(ctx)
	if err != nil {
		return ModelView{}, err
	}
	return s.view(m), nil
}

// view is m as the investigator uses it: the install's settings where it made
// them, the defaults where neither did. Setup's address doesn't apply to a
// provider with its own.
func (s *Settings) view(m Model) ModelView {
	in := s.Install
	v := ModelView{
		Provider: m.Provider, BaseURL: m.BaseURL, Name: m.Name, APIKeySet: m.APIKey != "",
		StrictOutput: m.StrictOutput == nil || *m.StrictOutput, SchemaDialect: m.SchemaDialect, ByInstall: s.installSet(),
	}
	if v.ByInstall == nil {
		v.ByInstall = []string{}
	}
	if in.Provider != "" || in.BaseURL != "" {
		v.Provider = in.Provider
	}
	if v.Provider == "" {
		v.Provider = "openai"
	}
	if in.BaseURL != "" || v.ownAddress() {
		v.BaseURL, v.SchemaDialect = in.BaseURL, in.SchemaDialect
		v.StrictOutput = in.StrictOutput == nil || *in.StrictOutput
	}
	if in.SchemaDialect != "" {
		v.SchemaDialect = in.SchemaDialect
	}
	if v.SchemaDialect == "" {
		v.SchemaDialect = "as-is"
	}
	if in.Name != "" {
		v.Name = in.Name
	}
	if in.APIKey {
		v.APIKeySet = true
	}
	v.Complete = len(v.Missing()) == 0
	return v
}

// ModelChange is what setup changes. Nil leaves a setting as it is; an empty
// value removes it.
type ModelChange struct {
	Provider      *string `json:"provider"`
	BaseURL       *string `json:"baseURL"`
	Name          *string `json:"name"`
	APIKey        *string `json:"apiKey"`
	StrictOutput  *bool   `json:"strictOutput"`
	SchemaDialect *string `json:"schemaDialect"`
}

// normalized checks change, refusing to change what the install set, and
// returns its values as they are kept: trimmed, the address without a
// trailing slash.
func (s *Settings) normalized(change ModelChange) (ModelChange, error) {
	var out ModelChange
	text := func(name string, c *string, check func(string) error) (*string, error) {
		if c == nil {
			return nil, nil
		}
		if s.byInstall(name) {
			return nil, fmt.Errorf("%w: %s", ErrSetByInstall, name)
		}
		v := strings.TrimSpace(*c)
		if v != "" && check != nil {
			if err := check(v); err != nil {
				return nil, err
			}
		}
		return &v, nil
	}
	var err error
	if out.Provider, err = text(ModelProvider, change.Provider, func(v string) error {
		if !slices.Contains(config.ModelProviders, v) {
			return errors.New("the provider must be openai or anthropic")
		}
		return nil
	}); err != nil {
		return ModelChange{}, err
	}
	if out.BaseURL, err = text(ModelBaseURL, change.BaseURL, func(v string) error {
		if !config.IsHTTPURL(v) {
			return errors.New("the model's address must be an http or https URL, such as https://api.openai.com/v1")
		}
		return nil
	}); err != nil {
		return ModelChange{}, err
	}
	if out.BaseURL != nil {
		*out.BaseURL = strings.TrimRight(*out.BaseURL, "/")
	}
	if out.Name, err = text(ModelName, change.Name, nil); err != nil {
		return ModelChange{}, err
	}
	if out.APIKey, err = text(ModelAPIKey, change.APIKey, nil); err != nil {
		return ModelChange{}, err
	}
	if out.SchemaDialect, err = text(ModelSchemaDialect, change.SchemaDialect, func(v string) error {
		if !slices.Contains(config.SchemaDialects, v) {
			return errors.New("the schema dialect must be as-is or openai-strict")
		}
		return nil
	}); err != nil {
		return ModelChange{}, err
	}
	if change.StrictOutput != nil {
		if s.byInstall(ModelStrictOutput) {
			return ModelChange{}, fmt.Errorf("%w: %s", ErrSetByInstall, ModelStrictOutput)
		}
		out.StrictOutput = change.StrictOutput
	}
	return out, nil
}

// SetModel saves change, made by by. It refuses to change what the install
// set, and checks values before saving any.
func (s *Settings) SetModel(ctx context.Context, change ModelChange, by string) error {
	c, err := s.normalized(change)
	if err != nil {
		return err
	}
	if c.APIKey != nil && *c.APIKey != "" {
		sealed, err := s.seal(*c.APIKey)
		if err != nil {
			return err
		}
		c.APIKey = &sealed
	}
	var strict *string
	if c.StrictOutput != nil {
		v := strconv.FormatBool(*c.StrictOutput)
		strict = &v
	}
	return s.put(ctx, by, c, strict)
}

// put saves c, its key already sealed, and strict, strict output as stored.
func (s *Settings) put(ctx context.Context, by string, c ModelChange, strict *string) error {
	now := s.now()
	var set []store.Setting
	var clear []string
	for _, v := range []struct {
		name      string
		value     *string
		encrypted bool
	}{
		{ModelProvider, c.Provider, false},
		{ModelBaseURL, c.BaseURL, false},
		{ModelName, c.Name, false},
		{ModelAPIKey, c.APIKey, true},
		{ModelStrictOutput, strict, false},
		{ModelSchemaDialect, c.SchemaDialect, false},
	} {
		switch {
		case v.value == nil:
		case *v.value == "":
			clear = append(clear, v.name)
		default:
			set = append(set, store.Setting{Name: v.name, Value: *v.value, Encrypted: v.encrypted, UpdatedAt: now, UpdatedBy: by})
		}
	}
	return s.Store.PutSettings(ctx, set, clear)
}

// Trial returns the model as it would be with change saved, and its view, to
// test its connection. Nothing is saved. A key change leaves alone is the one
// stored.
func (s *Settings) Trial(ctx context.Context, change ModelChange) (Model, ModelView, error) {
	c, err := s.normalized(change)
	if err != nil {
		return Model{}, ModelView{}, err
	}
	m, err := s.Model(ctx)
	if err != nil {
		return Model{}, ModelView{}, err
	}
	for _, v := range []struct {
		to   *string
		from *string
	}{{&m.Provider, c.Provider}, {&m.BaseURL, c.BaseURL}, {&m.Name, c.Name}, {&m.APIKey, c.APIKey}, {&m.SchemaDialect, c.SchemaDialect}} {
		if v.from != nil {
			*v.to = *v.from
		}
	}
	if c.StrictOutput != nil {
		m.StrictOutput = c.StrictOutput
	}
	return m, s.view(m), nil
}

// cipher is AES-256-GCM with a key derived from the key file, read each time
// so a rotated Secret needs no restart.
func (s *Settings) cipher() (cipher.AEAD, error) {
	secret, err := token.Read(s.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("read the settings key: %w", err)
	}
	if len(secret) < 32 {
		return nil, errors.New("the settings key is too short: it needs at least 32 characters")
	}
	key := sha256.Sum256(secret)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (s *Settings) seal(plain string) (string, error) {
	aead, err := s.cipher()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(aead.Seal(nonce, nonce, []byte(plain), nil)), nil
}

func (s *Settings) open(sealed string) (string, error) {
	aead, err := s.cipher()
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < aead.NonceSize() {
		return "", errors.New("the stored value is damaged")
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], nil)
	if err != nil {
		return "", errors.New("the stored value can't be decrypted: the settings key changed since it was saved")
	}
	return string(plain), nil
}

func (s *Settings) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
