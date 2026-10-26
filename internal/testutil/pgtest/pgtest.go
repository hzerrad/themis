// Package pgtest gives tests a migrated Themis store on a real PostgreSQL. One
// container is started per test binary, on first use, and each test gets its
// own empty database in it. The container is removed by the testcontainers
// reaper when the test binary exits.
package pgtest

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/hzerrad/themis/internal/store"
)

// Image is the PostgreSQL image tests run against.
const Image = "postgres:17-alpine"

var (
	once     sync.Once
	connStr  string
	startErr error
)

func start() {
	ctx := context.Background()
	ctr, err := postgres.Run(ctx, Image,
		postgres.WithDatabase("themis"),
		postgres.WithUsername("themis"),
		postgres.WithPassword("themis"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		startErr = err
		return
	}
	connStr, startErr = ctr.ConnectionString(ctx, "sslmode=disable")
}

// NewStore returns a migrated store on a fresh database, plus its pool for
// assertions that read tables directly. It skips the test in -short mode,
// because it needs Docker.
func NewStore(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	if testing.Short() {
		t.Skip("needs Docker; skipped in -short mode")
	}
	once.Do(start)
	if startErr != nil {
		t.Fatalf("start postgres container: %v", startErr)
	}

	ctx := t.Context()
	name := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := pgx.Connect(ctx, connStr)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create database: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	s, err := store.New(pool)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, pool
}

// Count runs a count query and returns the result.
func Count(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}
