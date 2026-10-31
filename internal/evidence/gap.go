package evidence

import (
	"context"
	"errors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// GapCause is why evidence an investigation needed is missing. The set is
// closed: each cause has a remedy and a metric label.
type GapCause string

const (
	// GapForbidden: Themis is not allowed to read it.
	GapForbidden GapCause = "forbidden"
	// GapNotInProfile: the incident's profile does not allow the template.
	GapNotInProfile GapCause = "not_in_profile"
	// GapExpired: Kubernetes no longer keeps it, such as old events.
	GapExpired GapCause = "expired"
	// GapNotFound: the object it is about no longer exists.
	GapNotFound GapCause = "not_found"
	// GapBudgetSpent: the investigation used all its evidence requests.
	GapBudgetSpent GapCause = "budget_spent"
	// GapDataCeiling: the model endpoint may not see data this sensitive.
	GapDataCeiling GapCause = "data_ceiling"
	// GapTimeout: Kubernetes did not answer in time.
	GapTimeout GapCause = "timeout"
	// GapUnavailable: reading it failed for another reason.
	GapUnavailable GapCause = "unavailable"
	// GapAbsent: Themis looked, and there was nothing.
	GapAbsent GapCause = "absent"
)

// GapCauses returns every cause.
func GapCauses() []GapCause {
	return []GapCause{
		GapForbidden, GapNotInProfile, GapExpired, GapNotFound, GapBudgetSpent,
		GapDataCeiling, GapTimeout, GapUnavailable, GapAbsent,
	}
}

// Blind reports whether Themis could not see, as opposed to having looked and
// found nothing. A blind gap has a remedy; an absent one does not.
func (c GapCause) Blind() bool { return c != GapAbsent }

// CauseOf says why a collection failed.
func CauseOf(err error) GapCause {
	switch {
	case errors.Is(err, ErrTargetGone), apierrors.IsNotFound(err):
		return GapNotFound
	case apierrors.IsForbidden(err):
		return GapForbidden
	case errors.Is(err, context.DeadlineExceeded), apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		return GapTimeout
	default:
		return GapUnavailable
	}
}
