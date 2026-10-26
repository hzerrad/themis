// Command migrate applies the Themis database migrations (Themis's own and
// River's) to the database at DATABASE_URL, exactly as Themis does at startup.
// It is meant for development databases.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hzerrad/themis/internal/store"
)

func main() {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		fmt.Fprintln(os.Stderr, "migrate: DATABASE_URL is not set, e.g. postgres://user:password@localhost:5432/themis?sslmode=disable")
		os.Exit(2)
	}
	if err := run(url); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(url string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	if err := store.Migrate(ctx, pool); err != nil {
		return err
	}
	fmt.Println("migrations applied")
	return nil
}
