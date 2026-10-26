package health_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/health"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

type pinger func(ctx context.Context) error

func (p pinger) Ping(ctx context.Context) error { return p(ctx) }

func check(db health.Pinger) error {
	return health.Database(db)(httptest.NewRequest("GET", "/readyz", nil))
}

func TestDatabase_Ready(t *testing.T) {
	s, _ := pgtest.NewStore(t)

	if err := check(s); err != nil {
		t.Errorf("check = %v, want ready", err)
	}
}

func TestDatabase_Unreachable(t *testing.T) {
	err := check(pinger(func(context.Context) error { return errors.New("connection refused") }))

	if err == nil {
		t.Error("check passed, want it to fail")
	}
}

// A hung database fails the probe within the timeout instead of hanging it.
func TestDatabase_HungDatabaseTimesOut(t *testing.T) {
	start := time.Now()

	err := check(pinger(func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}))

	if err == nil {
		t.Error("check passed, want it to fail")
	}
	if elapsed := time.Since(start); elapsed > health.DatabaseTimeout+time.Second {
		t.Errorf("check took %v, want at most about %v", elapsed, health.DatabaseTimeout)
	}
}
