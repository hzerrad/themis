package intake

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/hzerrad/themis/internal/incident/intake/target"
	"github.com/hzerrad/themis/internal/store"
)

// Receiver stores what any source sent.
type Receiver struct {
	store *store.Store
}

func NewReceiver(s *store.Store) *Receiver {
	return &Receiver{store: s}
}

// Received counts what Receive stored.
type Received struct {
	Accepted int
	Rejected int
}

// Receive stores one request's alerts in one transaction: a process_alert job
// per valid alert and a rejection per invalid one.
func (r *Receiver) Receive(ctx context.Context, alerts []Alert, invalid []InvalidAlert) (Received, error) {
	if len(alerts) == 0 && len(invalid) == 0 {
		return Received{}, nil
	}
	var got Received
	err := r.store.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		got, err = r.ReceiveTx(ctx, tx, alerts, invalid)
		return err
	})
	if err != nil {
		return Received{}, err
	}
	return got, nil
}

// ReceiveTx stores alerts as Receive does, in the caller's transaction.
func (r *Receiver) ReceiveTx(ctx context.Context, tx pgx.Tx, alerts []Alert, invalid []InvalidAlert) (Received, error) {
	jobs := make([]river.JobArgs, len(alerts))
	for i, a := range alerts {
		jobs[i] = ProcessAlertArgs{Alert: a}
	}
	if err := r.store.EnqueueManyTx(ctx, tx, jobs); err != nil {
		return Received{}, fmt.Errorf("store received alerts: %w", err)
	}
	for _, a := range invalid {
		if _, err := r.store.InsertRejectedAlert(ctx, tx, rejectionOf(a)); err != nil {
			return Received{}, fmt.Errorf("store received alerts: %w", err)
		}
	}
	store.AfterCommit(tx, func() {
		for _, a := range alerts {
			alertsReceived.WithLabelValues(a.Source, "accepted").Inc()
		}
		for _, a := range invalid {
			alertsReceived.WithLabelValues(a.Source, "invalid").Inc()
		}
	})
	return Received{Accepted: len(alerts), Rejected: len(invalid)}, nil
}

func rejectionOf(a InvalidAlert) store.RejectedAlertArgs {
	detail := "invalid alert"
	if a.Err != nil {
		detail = a.Err.Error()
	}
	return store.RejectedAlertArgs{
		Source:     a.Source,
		ReceivedAt: a.ReceivedAt,
		Reason:     store.RejectionInvalidAlert,
		Detail:     detail,
		AlertName:  a.Name,
		Namespace:  a.Labels[target.LabelNamespace],
		Payload:    a.Payload,
	}
}
