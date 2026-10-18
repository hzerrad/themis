package store_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/hzerrad/themis/internal/store"
)

// adminConnString points at the container's default database. Each test
// creates its own database from it, so tests never share rows.
var adminConnString string

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	flag.Parse()
	if testing.Short() {
		return m.Run()
	}

	ctx := context.Background()
	ctr, err := postgres.Run(ctx, "postgres:17-alpine",
		postgres.WithDatabase("themis"),
		postgres.WithUsername("themis"),
		postgres.WithPassword("themis"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start postgres container:", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()

	adminConnString, err = ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintln(os.Stderr, "postgres connection string:", err)
		return 1
	}
	return m.Run()
}

// newTestDB creates an empty database and returns a pool connected to it.
func newTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("needs Docker; skipped in -short mode")
	}
	ctx := t.Context()

	name := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := pgx.Connect(ctx, adminConnString)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create database: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(adminConnString)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newStore returns a migrated store on a fresh database, plus its pool for
// assertions that read tables directly.
func newStore(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	pool := newTestDB(t)
	if err := store.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	s, err := store.New(pool)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, pool
}

func count(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}
