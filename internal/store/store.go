package store

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
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
	"github.com/riverqueue/river/rivertype"
	"github.com/riverqueue/rivercontrib/otelriver"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type Store struct {
	pool  *pgxpool.Pool
	river *river.Client[pgx.Tx]
}

func New(pool *pgxpool.Pool) (*Store, error) {
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Middleware: riverMiddleware()})
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

// riverMiddleware traces jobs. A job's metadata, never its args, carries the
// traceparent of the span that queued it; the span working it links there,
// since a job may run long after.
func riverMiddleware() []rivertype.Middleware {
	return []rivertype.Middleware{otelriver.NewMiddleware(&otelriver.MiddlewareConfig{
		EnableTracePropagation:      true,
		EnableWorkSpanJobKindSuffix: true,
	})}
}

// InTx runs fn in a transaction, and then the functions given to AfterCommit
// if it committed.
func (s *Store) InTx(ctx context.Context, fn func(pgx.Tx) error) error {
	h := &hookedTx{}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		h.Tx = tx
		return fn(h)
	})
	if err != nil {
		return err
	}
	for _, f := range h.after {
		f()
	}
	return nil
}

type hookedTx struct {
	pgx.Tx
	after []func()
}

// AfterCommit runs f once tx commits, and never if it rolls back. Only a
// transaction of InTx has the hook; f is dropped for any other.
func AfterCommit(tx pgx.Tx, f func()) {
	if h, ok := tx.(*hookedTx); ok {
		h.after = append(h.after, f)
	}
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

// Migrate brings the database up to date. records works out the record each
// fix keeps of a change, for actions an earlier version of Themis stored with
// whole pod templates; nil will do for a database that has none.
func Migrate(ctx context.Context, pool *pgxpool.Pool, records Records) error {
	if err := migrate(ctx, pool, records, goose.MaxVersion); err != nil {
		return err
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

// migrate applies Themis's own migrations up to version.
func migrate(ctx context.Context, pool *pgxpool.Pool, records Records, version int64) error {
	provider, err := migrations(pool, records)
	if err != nil {
		return err
	}
	defer func() { _ = provider.Close() }()

	if _, err := provider.UpTo(ctx, version); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}

// migrations are Themis's own migrations, on the database of pool: the SQL
// ones, and the one written in Go, which works records out with records.
func migrations(pool *pgxpool.Pool, records Records) (*goose.Provider, error) {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("open migrations: %w", err)
	}

	// A table lock to holds through transaction-mode connection poolers
	locker, err := lock.NewPostgresTableLocker()
	if err != nil {
		return nil, fmt.Errorf("create migration locker: %w", err)
	}

	db := stdlib.OpenDBFromPool(pool)

	provider, err := goose.NewProvider(goose.DialectPostgres, db, sub, goose.WithLocker(locker), goose.WithGoMigrations(keepRecords(records)))
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create goose provider: %w", err)
	}
	return provider, nil
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// normalizeNumbers turns JSON numbers into int64 when integral, else float64,
// so values read back hash as they were written.
func normalizeNumbers(m map[string]any) map[string]any {
	for k, v := range m {
		switch n := v.(type) {
		case json.Number:
			if i, err := n.Int64(); err == nil {
				m[k] = i
			} else if f, err := n.Float64(); err == nil {
				m[k] = f
			}
		case map[string]any:
			m[k] = normalizeNumbers(n)
		}
	}
	return m
}
