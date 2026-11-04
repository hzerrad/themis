package core

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

type pingFunc func(ctx context.Context) error

func (p pingFunc) Ping(ctx context.Context) error { return p(ctx) }

func readyz(db pinger) error {
	return databaseReady(db)(httptest.NewRequestWithContext(context.Background(), "GET", "/readyz", nil))
}

func TestDatabase_Ready(t *testing.T) {
	s, _ := pgtest.NewStore(t)

	if err := readyz(s); err != nil {
		t.Errorf("check = %v, want ready", err)
	}
}

// A hung database fails the probe within the timeout instead of hanging it.
func TestDatabase_HungDatabaseTimesOut(t *testing.T) {
	start := time.Now()

	err := readyz(pingFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}))

	if err == nil {
		t.Error("check passed, want it to fail")
	}
	if elapsed := time.Since(start); elapsed > databaseTimeout+time.Second {
		t.Errorf("check took %v, want at most about %v", elapsed, databaseTimeout)
	}
}
