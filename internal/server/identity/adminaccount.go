package identity

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"time"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/platform/token"
	"github.com/hzerrad/themis/internal/store"
)

// ChartUser is who the record names as making the admin account the install
// configures, as the Helm chart does.
const ChartUser = "helm-chart"

// AdminAccountStore keeps the accounts the configured admin account joins.
type AdminAccountStore interface {
	Accounts(ctx context.Context) ([]store.Account, error)
	CreateAccount(ctx context.Context, a store.Account) error
}

// EnsureAdminAccount makes the admin account the install configures, with
// the password the install chose, when no admin account exists. It never
// changes an account, so a password changed in Themis stays changed. It
// fails when it must make the account and can't.
func EnsureAdminAccount(ctx context.Context, s AdminAccountStore, c config.AdminAccount, now time.Time, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	if err := CheckUsername(c.Username); err != nil {
		return fmt.Errorf("api.adminAccount.username: %w", err)
	}
	accounts, err := s.Accounts(ctx)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(accounts, func(a store.Account) bool { return a.Role == config.RoleAdmin }) {
		return nil
	}
	if slices.ContainsFunc(accounts, func(a store.Account) bool { return a.Username == c.Username }) {
		log.ErrorContext(ctx, "the configured admin account exists without the admin role, and is left as it is", "username", c.Username)
		return nil
	}
	hash, err := configuredHash(ctx, c)
	if err != nil {
		return err
	}
	err = s.CreateAccount(ctx, store.Account{Username: c.Username, PasswordHash: hash, Role: config.RoleAdmin, CreatedAt: now, CreatedBy: ChartUser})
	if errors.Is(err, store.ErrAccountExists) {
		// Another core made it first.
		return nil
	}
	if err != nil {
		return err
	}
	log.InfoContext(ctx, "the configured admin account was created", "username", c.Username)
	return nil
}

// configuredHash is the configured password's hash: the hash file's, or the
// password file's password, hashed.
func configuredHash(ctx context.Context, c config.AdminAccount) (string, error) {
	if c.PasswordHashFile != "" {
		hash, err := token.Read(c.PasswordHashFile)
		switch {
		case err == nil:
			if err := CheckHash(string(hash)); err != nil {
				return "", fmt.Errorf("api.adminAccount.passwordHashFile: %w", err)
			}
			return string(hash), nil
		case !errors.Is(err, fs.ErrNotExist) || c.PasswordFile == "":
			return "", fmt.Errorf("api.adminAccount.passwordHashFile: %w", err)
		}
	}
	password, err := token.Read(c.PasswordFile)
	if err != nil {
		return "", fmt.Errorf("api.adminAccount.passwordFile: %w", err)
	}
	if err := CheckNewPassword(c.Username, string(password)); err != nil {
		return "", fmt.Errorf("api.adminAccount.passwordFile: %w", err)
	}
	hash, err := HashPassword(ctx, string(password))
	if err != nil {
		return "", fmt.Errorf("api.adminAccount: %w", err)
	}
	return hash, nil
}
