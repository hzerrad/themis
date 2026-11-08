package store

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MigrateTo applies Themis's own migrations up to version, for tests of the
// next one.
func MigrateTo(ctx context.Context, pool *pgxpool.Pool, records Records, version int64) error {
	return migrate(ctx, pool, records, version)
}

// MigrateDownTo takes Themis's own migrations back down to version.
func MigrateDownTo(ctx context.Context, pool *pgxpool.Pool, version int64) error {
	provider, err := migrations(pool, nil)
	if err != nil {
		return err
	}
	defer func() { _ = provider.Close() }()
	_, err = provider.DownTo(ctx, version)
	return err
}
