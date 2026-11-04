package core

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// The checks behind Themis's probes. Readiness needs only the database;
// liveness checks nothing external.

const databaseTimeout = 2 * time.Second

type pinger interface {
	Ping(ctx context.Context) error
}

// databaseReady fails when the database does not answer within
// databaseTimeout.
func databaseReady(db pinger) healthz.Checker {
	return func(req *http.Request) error {
		ctx, cancel := context.WithTimeout(req.Context(), databaseTimeout)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			return fmt.Errorf("database unreachable: %w", err)
		}
		return nil
	}
}
