package identity

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Argon2id settings: RFC 9106's second recommendation, 64 MiB and 3 passes.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// MinPasswordLength and MaxPasswordLength bound a password, in characters.
const (
	MinPasswordLength = 12
	MaxPasswordLength = 128
)

// hashing allows two hashes at once, so sign-ins can't use more than 128 MiB.
var hashing = make(chan struct{}, 2)

// HashPassword hashes password with argon2id, in the PHC string format.
func HashPassword(ctx context.Context, password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("make a salt: %w", err)
	}
	key, err := argonKey(ctx, password, salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	if err != nil {
		return "", err
	}
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// CheckPassword reports whether password matches encoded, a hash from
// HashPassword. The settings are read from encoded, so hashes made with
// older settings still check.
func CheckPassword(ctx context.Context, encoded, password string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("not an argon2id hash")
	}
	var version int
	var memory, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, fmt.Errorf("unsupported argon2 version %q", parts[2])
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false, fmt.Errorf("read the hash's settings: %w", err)
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("read the salt: %w", err)
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("read the key: %w", err)
	}
	got, err := argonKey(ctx, password, salt, time, memory, threads, uint32(len(want))) //nolint:gosec // a key is 32 bytes
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func argonKey(ctx context.Context, password string, salt []byte, time, memory uint32, threads uint8, keyLen uint32) ([]byte, error) {
	select {
	case hashing <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-hashing }()
	return argon2.IDKey([]byte(password), salt, time, memory, threads, keyLen), nil
}

// dummyHash is checked when a username is unknown, so a wrong username takes
// as long as a wrong password and doesn't reveal which accounts exist.
var dummyHash = sync.OnceValue(func() string {
	h, _ := HashPassword(context.Background(), "not a real password, only for timing")
	return h
})

var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,63}$`)

// CheckUsername says what's wrong with a new account's username, if anything.
func CheckUsername(username string) error {
	if !usernamePattern.MatchString(username) {
		return errors.New("a username is 2 to 64 lowercase letters, digits, dots, dashes or underscores, starting with a letter or digit")
	}
	if username == TokenUser || username == SetupUser {
		return fmt.Errorf("the username %q is kept for Themis's own use", username)
	}
	return nil
}

// CheckNewPassword says what's wrong with a new password, if anything.
func CheckNewPassword(username, password string) error {
	n := utf8.RuneCountInString(password)
	switch {
	case n < MinPasswordLength:
		return fmt.Errorf("a password needs at least %d characters", MinPasswordLength)
	case n > MaxPasswordLength:
		return fmt.Errorf("a password can't be longer than %d characters", MaxPasswordLength)
	case strings.EqualFold(password, username):
		return errors.New("a password can't be the username")
	}
	return nil
}
