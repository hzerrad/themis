package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// WorkerStopTimeout bounds a graceful stop; cancelled jobs are retried.
const WorkerStopTimeout = 25 * time.Second

// WorkerRunner works queued jobs. It has its own River client because the
// workers are built on the store.
type WorkerRunner struct {
	client *river.Client[pgx.Tx]
}

func (s *Store) NewWorkerRunner(workers *river.Workers, maxWorkers int, logger *slog.Logger) (*WorkerRunner, error) {
	if logger == nil {
		logger = slog.Default()
	}
	client, err := river.NewClient(riverpgxv5.New(s.pool), &river.Config{
		Queues:  map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: maxWorkers}},
		Workers: workers,
		Logger:  logger,
	})
	if err != nil {
		return nil, fmt.Errorf("create river worker client: %w", err)
	}
	return &WorkerRunner{client: client}, nil
}

// Start works jobs until ctx is done, then stops gracefully.
func (r *WorkerRunner) Start(ctx context.Context) error {
	// Jobs outlive ctx, which only triggers the graceful stop.
	if err := r.client.Start(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("start river workers: %w", err)
	}
	<-ctx.Done()

	stopCtx, cancel := context.WithTimeout(context.Background(), WorkerStopTimeout)
	defer cancel()
	if err := r.client.Stop(stopCtx); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			hardCtx, hardCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer hardCancel()
			return r.client.StopAndCancel(hardCtx)
		}
		return fmt.Errorf("stop river workers: %w", err)
	}
	return nil
}

func (r *WorkerRunner) NeedLeaderElection() bool { return false }
