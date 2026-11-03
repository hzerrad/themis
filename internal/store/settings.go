package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Setting is one setting made in Themis's setup. An encrypted value is
// ciphertext; the store never sees it in the clear.
type Setting struct {
	Name      string
	Value     string
	Encrypted bool
	UpdatedAt time.Time
	UpdatedBy string
}

// Settings returns the settings with the given names that have been made.
func (s *Store) Settings(ctx context.Context, names []string) (map[string]Setting, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, value, encrypted, updated_at, updated_by FROM settings WHERE name = ANY($1)`, names)
	if err != nil {
		return nil, fmt.Errorf("read settings: %w", err)
	}
	all, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Setting])
	if err != nil {
		return nil, fmt.Errorf("read settings: %w", err)
	}
	out := make(map[string]Setting, len(all))
	for _, st := range all {
		out[st.Name] = st
	}
	return out, nil
}

// PutSettings writes set and removes the settings named in clear, together.
func (s *Store) PutSettings(ctx context.Context, set []Setting, clear []string) error {
	return s.InTx(ctx, func(tx pgx.Tx) error {
		for _, st := range set {
			if _, err := tx.Exec(ctx, `
	INSERT INTO settings (name, value, encrypted, updated_at, updated_by) VALUES ($1, $2, $3, $4, $5)
	ON CONFLICT (name) DO UPDATE SET value = excluded.value, encrypted = excluded.encrypted, updated_at = excluded.updated_at, updated_by = excluded.updated_by
`, st.Name, st.Value, st.Encrypted, st.UpdatedAt, st.UpdatedBy); err != nil {
				return fmt.Errorf("write setting %s: %w", st.Name, err)
			}
		}
		if len(clear) > 0 {
			if _, err := tx.Exec(ctx, `DELETE FROM settings WHERE name = ANY($1)`, clear); err != nil {
				return fmt.Errorf("clear settings: %w", err)
			}
		}
		return nil
	})
}
