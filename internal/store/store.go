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

// Querier runs queries: a transaction or the store's pool.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// DB queries outside a transaction.
func (s *Store) DB() Querier {
	return s.pool
}

// Ping reports whether the database answers.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
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

// EnqueueManyTx enqueues jobs in one statement, in the caller's transaction:
// they become visible together when it commits, or not at all. Each job uses
// its args' InsertOpts, if any.
func (s *Store) EnqueueManyTx(ctx context.Context, tx pgx.Tx, args []river.JobArgs) error {
	if len(args) == 0 {
		return nil
	}
	params := make([]river.InsertManyParams, len(args))
	for i, a := range args {
		params[i] = river.InsertManyParams{Args: a}
	}
	if _, err := s.river.InsertManyTx(ctx, tx, params); err != nil {
		return fmt.Errorf("enqueue %d jobs: %w", len(args), err)
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
