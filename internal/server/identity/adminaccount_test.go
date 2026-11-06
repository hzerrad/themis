package identity_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/server/identity"
	"github.com/hzerrad/themis/internal/store"
)

const chartPassword = "a long password of the chart"

// chartBcrypt is chartPassword as `htpasswd -nbBC 10 "" '<password>' | tr -d ':\n'` hashes it.
const chartBcrypt = "$2y$10$QtJ0A2cMK/Jlz9F9bZ0tCec6kO0/K9SYKuO.nfDJCI8Sr3ST6F4Wm"

type accountList struct{ accounts []store.Account }

func (l *accountList) Accounts(context.Context) ([]store.Account, error) { return l.accounts, nil }

func (l *accountList) CreateAccount(_ context.Context, a store.Account) error {
	l.accounts = append(l.accounts, a)
	return nil
}

// The install's admin account is made once, with the password it chose, and
// never changed after; a hash or password Themis can't take stops startup.
func TestEnsureAdminAccount(t *testing.T) {
	argon, err := identity.HashPassword(t.Context(), chartPassword)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		username, hash, password string
		existing                 []store.Account
		created                  bool
		err                      string
	}{
		"from an argon2id hash":                    {hash: argon, created: true},
		"from a bcrypt hash, as htpasswd makes":    {hash: chartBcrypt, created: true},
		"from the password, without a hash":        {password: chartPassword, created: true},
		"once an admin account exists":             {hash: argon, existing: []store.Account{{Username: "root", Role: config.RoleAdmin, PasswordHash: "changed"}}},
		"with the username taken by another role":  {hash: argon, existing: []store.Account{{Username: "root", Role: config.RoleViewer}}},
		"from a hash Themis can't check with":      {hash: "$1$md5$not-ours", err: "the password hash in"},
		"from a bcrypt hash too cheap to guard it": {hash: "$2y$04$luNqaYZ.yNt8KhsZLwQyCOy84kRtqxorY53uwu2ejJLh8bvhJOOk6", err: "the bcrypt cost is 4"},
		"from a password too short":                {password: "short", err: "at least 12 characters"},
		"with nothing to read":                     {err: "read the password"},
		"with a name Themis keeps":                 {username: identity.ChartUser, hash: argon, err: "api.adminAccount.username"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			c := config.AdminAccount{Username: "root", PasswordHashFile: filepath.Join(dir, "password-hash"), PasswordFile: filepath.Join(dir, "password")}
			if tc.username != "" {
				c.Username = tc.username
			}
			for file, content := range map[string]string{c.PasswordHashFile: tc.hash, c.PasswordFile: tc.password} {
				if content != "" {
					if err := os.WriteFile(file, []byte(content+"\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			s := &accountList{accounts: tc.existing}
			err := identity.EnsureAdminAccount(t.Context(), s, c, now, slog.New(slog.DiscardHandler))
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want one saying %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !tc.created {
				if len(s.accounts) != len(tc.existing) || s.accounts[0] != tc.existing[0] {
					t.Errorf("accounts = %+v, want them as they were", s.accounts)
				}
				return
			}
			a := s.accounts[len(s.accounts)-1]
			if a.Username != "root" || a.Role != config.RoleAdmin || a.TemporaryPassword || a.CreatedBy != identity.ChartUser || !a.CreatedAt.Equal(now) {
				t.Errorf("account = %+v, want root, admin, a password of its own, made by the chart", a)
			}
			if ok, err := identity.CheckPassword(t.Context(), a.PasswordHash, chartPassword); err != nil || !ok {
				t.Errorf("the chosen password doesn't sign in: %v, %v", ok, err)
			}
		})
	}
}

func TestCheckHash(t *testing.T) {
	argon, err := identity.HashPassword(t.Context(), chartPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.CheckHash(argon); err != nil {
		t.Errorf("Themis's own hash: %v", err)
	}
	for name, hash := range map[string]string{
		"argon2i":            strings.Replace(argon, "$argon2id$", "$argon2i$", 1),
		"a gigabyte too far": strings.Replace(argon, "m=65536", "m=2097152", 1),
		"no salt":            "$argon2id$v=19$m=65536,t=3,p=4$$" + strings.Split(argon, "$")[5],
		"truncated bcrypt":   chartBcrypt[:20],
	} {
		if err := identity.CheckHash(hash); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
