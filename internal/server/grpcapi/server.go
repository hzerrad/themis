// Package grpcapi serves the core's gRPC API to investigators: leases,
// evidence by named template, and proposals. It is the single point that
// enforces an investigation's scope.
package grpcapi

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/config/settings"
	"github.com/hzerrad/themis/internal/platform/telemetry"
	"github.com/hzerrad/themis/internal/platform/token"
	"github.com/hzerrad/themis/internal/server/statuspage"
	"github.com/hzerrad/themis/internal/store"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

// DefaultLeaseFor is how long a lease lasts without renewal.
const DefaultLeaseFor = time.Minute

// retryBackoff delays the lease after a failure, doubling with each attempt,
// so a brief model outage does not use up every attempt.
const retryBackoff = 30 * time.Second

const actor = "investigator"

type Server struct {
	themisv1.UnimplementedInvestigationServiceServer
	themisv1.UnimplementedEvidenceServiceServer
	themisv1.UnimplementedProposalServiceServer

	Store  *store.Store
	Config config.Provider
	// Evidence is the templates investigators may ask for.
	Evidence evidence.Registry
	// Checks are the checks Themis runs, which suggestions cite.
	Checks   check.List
	Reader   client.Reader
	Logger   *slog.Logger
	Now      func() time.Time
	LeaseFor time.Duration

	// Reviewer proves who a caller outside Themis is. Without one, there are
	// no such callers.
	Reviewer TokenReviewer

	// Settings are the model settings made in Themis's setup, for the
	// investigator. Nil means none.
	Settings interface {
		Model(ctx context.Context) (settings.Model, error)
	}

	// Fixes are the fixes Themis can carry out, which suggestions name.
	Fixes capability.Registry
	// RootCauses are what an investigation's hypotheses may name, by profile.
	RootCauses catalog.RootCauses

	// Heartbeat records that the investigator asked for work. Nil records
	// nothing.
	Heartbeat *statuspage.Heartbeat

	limiters sync.Map
	reviews  lru[[sha256.Size]byte, review]
	peers    lru[string, *rate.Limiter]
}

// Register serves every service of s on g.
func (s *Server) Register(g *grpc.Server) {
	themisv1.RegisterInvestigationServiceServer(g, s)
	themisv1.RegisterEvidenceServiceServer(g, s)
	themisv1.RegisterProposalServiceServer(g, s)
}

// Interceptor requires the bearer token in tokenFile, adds an authenticated
// request's lease and the incident it was granted for to the context, and
// logs every request at debug level.
func (s *Server) Interceptor(tokenFile string) grpc.UnaryServerInterceptor {
	authenticate := token.RequireGRPC(tokenFile)
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if caller, external, err := s.externalCaller(ctx, tokenFile, info.FullMethod); err != nil {
			return nil, err
		} else if external {
			ctx = withCaller(ctx, caller)
			resp, err := handler(ctx, req)
			s.logger().DebugContext(ctx, "gRPC request served", "method", info.FullMethod, "code", status.Code(err).String(), "source", caller.Source)
			return resp, err
		}
		logCtx := ctx
		resp, err := authenticate(ctx, req, info, func(ctx context.Context, req any) (any, error) {
			if r, ok := req.(interface{ GetLeaseId() string }); ok {
				ctx = s.leaseFields(ctx, r.GetLeaseId())
			}
			resp, err := handler(ctx, req)
			if r, ok := resp.(*themisv1.LeaseInvestigationResponse); ok && r.GetInvestigation() != nil {
				inv := r.GetInvestigation()
				ctx = telemetry.WithAttrs(ctx, slog.String(telemetry.FieldLeaseID, inv.GetLeaseId()), slog.String(telemetry.FieldIncidentID, inv.GetIncidentId()))
			}
			logCtx = ctx
			return resp, err
		})
		s.logger().DebugContext(logCtx, "gRPC request served", "method", info.FullMethod, "code", status.Code(err).String())
		return resp, err
	}
}

func (s *Server) leaseFields(ctx context.Context, leaseID string) context.Context {
	if _, err := uuid.Parse(leaseID); err != nil {
		return ctx
	}
	attrs := []slog.Attr{slog.String(telemetry.FieldLeaseID, leaseID)}
	if id, err := s.Store.LeaseIncident(ctx, leaseID); err == nil {
		attrs = append(attrs, slog.String(telemetry.FieldIncidentID, id))
	}
	return telemetry.WithAttrs(ctx, attrs...)
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// internal hides an unexpected error from the caller and logs it.
func (s *Server) internal(ctx context.Context, err error) error {
	if errors.Is(err, store.ErrLeaseLost) {
		return status.Error(codes.FailedPrecondition, "the lease is unknown, expired or superseded")
	}
	if st, ok := status.FromError(err); ok && st.Code() != codes.Unknown {
		return err
	}
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	s.logger().ErrorContext(ctx, "gRPC request failed", telemetry.FieldError, err)
	return status.Error(codes.Internal, "internal error")
}

// profile returns the configured profile of an investigation.
func (s *Server) profile(inv store.Investigation) (config.Profile, error) {
	p, ok := s.Config.Current().Profile(inv.Profile)
	if !ok {
		return config.Profile{}, status.Errorf(codes.FailedPrecondition, "profile %q is no longer configured", inv.Profile)
	}
	return p, nil
}
