// Package selfcheck says, in plain words, whether each part of an install works:
// the database, the investigator and its model, alerts, the namespaces Themis
// watches, the executor and sign-in.
package selfcheck

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/hzerrad/themis/internal/telemetry"
)

// beatEvery is how often a heartbeat is written at most, so a component that
// calls every few seconds costs one write per half minute.
const beatEvery = 30 * time.Second

// Beater records heartbeats.
type Beater interface {
	Beat(ctx context.Context, component, detail string, at time.Time) error
}

// Heartbeat records that a component was heard from.
type Heartbeat struct {
	Store     Beater
	Component string
	Logger    *slog.Logger
	Now       func() time.Time

	last atomic.Int64
}

// Beat records that the component was just heard from, with detail, unless it
// already was within the last half minute. A failure is only logged: a
// heartbeat never fails the call it rides on.
func (h *Heartbeat) Beat(ctx context.Context, detail string) {
	if h == nil {
		return
	}
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	last := h.last.Load()
	if now.UnixNano()-last < int64(beatEvery) || !h.last.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	if err := h.Store.Beat(ctx, h.Component, detail, now); err != nil {
		h.last.Store(0)
		if h.Logger != nil {
			h.Logger.WarnContext(ctx, "cannot record a heartbeat", "component", h.Component, telemetry.FieldError, err)
		}
	}
}
