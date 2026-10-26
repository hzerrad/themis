// Package health provides the checks behind Themis's probes. Readiness needs
// only the database; liveness checks nothing external.
package health

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

const DatabaseTimeout = 2 * time.Second

type Pinger interface {
	Ping(ctx context.Context) error
}

// Database fails when the database does not answer within DatabaseTimeout.
func Database(db Pinger) healthz.Checker {
	return func(req *http.Request) error {
		ctx, cancel := context.WithTimeout(req.Context(), DatabaseTimeout)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			return fmt.Errorf("database unreachable: %w", err)
		}
		return nil
	}
}
