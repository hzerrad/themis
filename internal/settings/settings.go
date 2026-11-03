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
	"strconv"
	"strings"
	"time"

	"github.com/hzerrad/themis/internal/auth"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/store"
)

// The model settings setup can make.
const (
	ModelBaseURL      = "model.baseURL"
	ModelName         = "model.name"
	ModelAPIKey       = "model.apiKey"
	ModelStrictOutput = "model.strictOutput"
)

var modelSettings = []string{ModelBaseURL, ModelName, ModelAPIKey, ModelStrictOutput}

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

// Model is the investigator's model, as setup set it.
type Model struct {
	BaseURL string
	Name    string
	APIKey  string
	// StrictOutput is nil when setup didn't set it.
	StrictOutput *bool
}

// ModelView is the model settings for the setup page: what's set, by whom.
// The key itself is never shown.
type ModelView struct {
	BaseURL      string `json:"baseURL"`
	Name         string `json:"name"`
	APIKeySet    bool   `json:"apiKeySet"`
	StrictOutput bool   `json:"strictOutput"`
	// ByInstall lists the settings the install made, which setup can't change.
	ByInstall []string `json:"byInstall"`
	// Complete says the model can be used: it has an address and a name.
	Complete bool `json:"complete"`
}

// installSet lists the settings the install made. The address and strict
// output go together: they describe the same endpoint.
func (s *Settings) installSet() []string {
	var set []string
	if s.Install.BaseURL != "" {
		set = append(set, ModelBaseURL, ModelStrictOutput)
	}
	if s.Install.Name != "" {
		set = append(set, ModelName)
	}
	if s.Install.APIKey {
		set = append(set, ModelAPIKey)
	}
	return set
}

func (s *Settings) byInstall(name string) bool {
	for _, n := range s.installSet() {
		if n == name {
			return true
		}
	}
	return false
}

// Model returns what setup set for the model, with secrets in the clear, for
// the investigator to use where its own environment sets nothing.
func (s *Settings) Model(ctx context.Context) (Model, error) {
	stored, err := s.Store.Settings(ctx, modelSettings)
	if err != nil {
		return Model{}, err
	}
	var m Model
	m.BaseURL = stored[ModelBaseURL].Value
	m.Name = stored[ModelName].Value
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
	v := ModelView{BaseURL: m.BaseURL, Name: m.Name, APIKeySet: m.APIKey != "", StrictOutput: m.StrictOutput == nil || *m.StrictOutput, ByInstall: s.installSet()}
	if v.ByInstall == nil {
		v.ByInstall = []string{}
	}
	if s.Install.BaseURL != "" {
		v.BaseURL = s.Install.BaseURL
	}
	if s.Install.Name != "" {
		v.Name = s.Install.Name
	}
	if s.Install.APIKey {
		v.APIKeySet = true
	}
	v.Complete = v.BaseURL != "" && v.Name != ""
	return v, nil
}

// ModelChange is what setup changes. Nil leaves a setting as it is; an empty
// key removes it.
type ModelChange struct {
	BaseURL      *string `json:"baseURL"`
	Name         *string `json:"name"`
	APIKey       *string `json:"apiKey"`
	StrictOutput *bool   `json:"strictOutput"`
}

// SetModel saves change, made by by. It refuses to change what the install
// set, and checks values before saving any.
func (s *Settings) SetModel(ctx context.Context, change ModelChange, by string) error {
	now := s.now()
	var set []store.Setting
	var clear []string
	put := func(name, value string, encrypted bool) error {
		if s.byInstall(name) {
			return fmt.Errorf("%w: %s", ErrSetByInstall, name)
		}
		if value == "" {
			clear = append(clear, name)
			return nil
		}
		set = append(set, store.Setting{Name: name, Value: value, Encrypted: encrypted, UpdatedAt: now, UpdatedBy: by})
		return nil
	}
	if c := change.BaseURL; c != nil {
		u := strings.TrimSpace(*c)
		if u != "" && !config.IsHTTPURL(u) {
			return errors.New("the model's address must be an http or https URL, such as https://api.openai.com/v1")
		}
		if err := put(ModelBaseURL, strings.TrimRight(u, "/"), false); err != nil {
			return err
		}
	}
	if c := change.Name; c != nil {
		if err := put(ModelName, strings.TrimSpace(*c), false); err != nil {
			return err
		}
	}
	if c := change.APIKey; c != nil {
		value := strings.TrimSpace(*c)
		if value != "" {
			sealed, err := s.seal(value)
			if err != nil {
				return err
			}
			value = sealed
		}
		if err := put(ModelAPIKey, value, true); err != nil {
			return err
		}
	}
	if c := change.StrictOutput; c != nil {
		if err := put(ModelStrictOutput, strconv.FormatBool(*c), false); err != nil {
			return err
		}
	}
	return s.Store.PutSettings(ctx, set, clear)
}

// cipher is AES-256-GCM with a key derived from the key file, read each time
// so a rotated Secret needs no restart.
func (s *Settings) cipher() (cipher.AEAD, error) {
	secret, err := auth.ReadToken(s.KeyFile)
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
