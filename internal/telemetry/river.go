package telemetry

import (
	"context"
	"log/slog"

	"github.com/riverqueue/river"
)

// IncidentScoped is the args of a job about one incident. Incident is empty
// for a job queued before its args carried the incident.
type IncidentScoped interface {
	river.JobArgs
	Incident() string
}

// AddIncidentWorker registers w. Its jobs log their kind and incident.
func AddIncidentWorker[T IncidentScoped](workers *river.Workers, w river.Worker[T]) {
	river.AddWorker[T](workers, incidentWorker[T]{w})
}

// AddUnscopedWorker registers w, whose jobs are not about one incident. They
// log their kind, and add each incident as they come to it.
func AddUnscopedWorker[T river.JobArgs](workers *river.Workers, w river.Worker[T]) {
	river.AddWorker[T](workers, unscopedWorker[T]{w})
}

type incidentWorker[T IncidentScoped] struct{ river.Worker[T] }

func (w incidentWorker[T]) Work(ctx context.Context, job *river.Job[T]) error {
	attrs := []slog.Attr{slog.String(FieldJob, job.Kind)}
	if id := job.Args.Incident(); id != "" {
		attrs = append(attrs, slog.String(FieldIncidentID, id))
	}
	return w.Worker.Work(WithAttrs(ctx, attrs...), job)
}

type unscopedWorker[T river.JobArgs] struct{ river.Worker[T] }

func (w unscopedWorker[T]) Work(ctx context.Context, job *river.Job[T]) error {
	return w.Worker.Work(WithAttrs(ctx, slog.String(FieldJob, job.Kind)), job)
}
