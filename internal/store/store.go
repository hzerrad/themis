package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type Store struct {
	pool  *pgxpool.Pool
	river *river.Client[pgx.Tx]
}

func New(pool *pgxpool.Pool) (*Store, error) {
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		return nil, fmt.Errorf("create river client: %w", err)
	}

	return &Store{
		pool:  pool,
		river: client,
	}, nil
}

func (s *Store) InTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, fn)
}

func (s *Store) EnqueueTx(ctx context.Context, tx pgx.Tx, args river.JobArgs) error {
	if _, err := s.river.InsertTx(ctx, tx, args, nil); err != nil {
		return fmt.Errorf("enqueue %s: %w", args.Kind(), err)
	}
	return nil
}

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("open migrations: %w", err)
	}

	// A table lock to holds through transaction-mode connection poolers
	locker, err := lock.NewPostgresTableLocker()
	if err != nil {
		return fmt.Errorf("create migration locker: %w", err)
	}

	db := stdlib.OpenDBFromPool(pool)

	provider, err := goose.NewProvider(goose.DialectPostgres, db, sub, goose.WithLocker(locker))
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("create goose provider: %w", err)
	}
	defer func() { _ = provider.Close() }()

	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}

	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("create river migrator: %w", err)
	}

	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("river migrate: %w", err)
	}
	return nil
}
