package intake

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/hzerrad/themis/internal/store"
)

// Receiver stores what a source sent, whatever the source. It is the only part
// of intake that a source's HTTP handler calls after translating a request,
// and the only part that writes to the database at request time.
type Receiver struct {
	store *store.Store
}

// NewReceiver returns a Receiver that stores into s.
func NewReceiver(s *store.Store) *Receiver {
	return &Receiver{store: s}
}

// Received counts what Receive stored.
type Received struct {
	// Accepted alerts are queued for processing, one process_alert job each.
	Accepted int

	// Rejected alerts could not be translated and are recorded as rejections
	// with reason invalid_alert.
	Rejected int
}

// Receive stores one request's alerts in one transaction: a process_alert job
// for each valid alert and a rejection for each invalid one. Either everything
// is stored or nothing is, so a sender that retries after an error does not
// leave duplicates behind. An error means nothing was stored; the handler
// answers 503 so that the sender retries.
func (r *Receiver) Receive(ctx context.Context, alerts []Alert, invalid []InvalidAlert) (Received, error) {
	if len(alerts) == 0 && len(invalid) == 0 {
		return Received{}, nil
	}

	jobs := make([]river.JobArgs, len(alerts))
	for i, a := range alerts {
		jobs[i] = ProcessAlertArgs{Alert: a}
	}

	err := r.store.InTx(ctx, func(tx pgx.Tx) error {
		if err := r.store.EnqueueManyTx(ctx, tx, jobs); err != nil {
			return err
		}
		for _, a := range invalid {
			if _, err := r.store.InsertRejectedAlert(ctx, tx, rejectionOf(a)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Received{}, fmt.Errorf("store received alerts: %w", err)
	}
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
		Namespace:  a.Labels[LabelNamespace],
		Payload:    a.Payload,
	}
}
