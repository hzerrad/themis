package store

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AlertRejectionReason is why an alert did not become an incident. Values are
// a database enum.
type AlertRejectionReason string

const (
	RejectionInvalidAlert      AlertRejectionReason = "invalid_alert"
	RejectionNotEnrolled       AlertRejectionReason = "not_enrolled"
	RejectionMissingLabels     AlertRejectionReason = "missing_labels"
	RejectionInvalidOverride   AlertRejectionReason = "invalid_override"
	RejectionUnknownKind       AlertRejectionReason = "unknown_kind"
	RejectionTargetNotFound    AlertRejectionReason = "target_not_found"
	RejectionOwnerChainTooLong AlertRejectionReason = "owner_chain_too_long"
	RejectionNoProfile         AlertRejectionReason = "no_profile"
	RejectionProcessingFailed  AlertRejectionReason = "processing_failed"
	RejectionForbidden         AlertRejectionReason = "forbidden"
)

var rejectionReasons = []AlertRejectionReason{
	RejectionInvalidAlert, RejectionNotEnrolled, RejectionMissingLabels,
	RejectionInvalidOverride, RejectionUnknownKind, RejectionTargetNotFound,
	RejectionOwnerChainTooLong, RejectionNoProfile, RejectionProcessingFailed,
	RejectionForbidden,
}

// AlertRejectionReasons returns every reason.
func AlertRejectionReasons() []AlertRejectionReason {
	return slices.Clone(rejectionReasons)
}

// RejectedAlertArgs describe an alert Themis declined.
type RejectedAlertArgs struct {
	Source     string
	ReceivedAt time.Time
	Reason     AlertRejectionReason
	Detail     string
	AlertName  string
	Namespace  string
	Payload    []byte
}

// InsertRejectedAlert records an alert that did not become an incident and
// returns the rejection's ID.
func (s *Store) InsertRejectedAlert(ctx context.Context, tx pgx.Tx, in RejectedAlertArgs) (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("failed to create rejected alert ID: %w", err)
	}

	sql := `
	INSERT INTO rejected_alerts (id, source, received_at, reason, detail, alert_name, namespace, payload)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
`

	_, err = tx.Exec(ctx, sql, id, in.Source, in.ReceivedAt, in.Reason, in.Detail, in.AlertName, in.Namespace, in.Payload)
	if err != nil {
		return "", fmt.Errorf("failed to insert rejected alert: %w", err)
	}

	return id.String(), nil
}
