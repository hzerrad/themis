// Package grpcapi serves the core's gRPC API to investigators: leases,
// evidence by named template, and proposals. It says who is calling and
// translates; the investigation's lifecycle and scope are the investigation
// package's, and suggestions the suggestion package's.
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

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/config/settings"
	"github.com/hzerrad/themis/internal/incident/investigation"
	"github.com/hzerrad/themis/internal/incident/suggestion"
	"github.com/hzerrad/themis/internal/platform/telemetry"
	"github.com/hzerrad/themis/internal/platform/token"
	"github.com/hzerrad/themis/internal/server/identity"
	"github.com/hzerrad/themis/internal/server/modeltest"
	"github.com/hzerrad/themis/internal/server/statuspage"
	"github.com/hzerrad/themis/internal/store"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

type Server struct {
	themisv1.UnimplementedInvestigationServiceServer
	themisv1.UnimplementedEvidenceServiceServer
	themisv1.UnimplementedProposalServiceServer

	Store  *store.Store
	Config config.Provider
	// Investigations hands out leases, reads evidence under them, and ends
	// each investigation.
	Investigations *investigation.Leases
	// Evidence is the templates investigators may ask for, described to them.
	Evidence evidence.Registry
	// Checks are the checks Themis runs, described to suggesters.
	Checks check.List
	Logger *slog.Logger
	Now    func() time.Time

	// Reviewer proves who a caller outside Themis is. Without one, there are
	// no such callers.
	Reviewer TokenReviewer

	// Settings are the model settings made in Themis's setup, for the
	// investigator. Nil means none.
	Settings interface {
		Model(ctx context.Context) (settings.Model, error)
	}
	// ModelTests are the tests of the model's connection asked for on the
	// Setup page, which the investigator makes. Nil means none are.
	ModelTests *modeltest.Tests

	// Fixes are the fixes Themis can carry out, described to suggesters.
	Fixes capability.Registry
	// Suggestions takes the fixes suggested, and hands the next to governance.
	Suggestions *suggestion.Inbox
	// RootCauses are what an investigation's hypotheses may name, by profile.
	RootCauses catalog.RootCauses

	// Heartbeat records that the investigator asked for work. Nil records
	// nothing.
	Heartbeat *statuspage.Heartbeat

	// Gate refuses every call while no admin account exists. Nil means
	// always open.
	Gate *identity.Gate

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
			if err := s.open(ctx); err != nil {
				return nil, err
			}
			ctx = withCaller(ctx, caller)
			resp, err := handler(ctx, req)
			s.logger().DebugContext(ctx, "gRPC request served", "method", info.FullMethod, "code", status.Code(err).String(), "source", caller.Source)
			return resp, err
		}
		logCtx := ctx
		resp, err := authenticate(ctx, req, info, func(ctx context.Context, req any) (any, error) {
			if err := s.open(ctx); err != nil {
				return nil, err
			}
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

// open refuses a call while no admin account exists.
func (s *Server) open(ctx context.Context) error {
	open, err := s.Gate.Open(ctx)
	if err != nil {
		s.logger().ErrorContext(ctx, "cannot check for an admin account", telemetry.FieldError, err)
		return status.Error(codes.Unavailable, "Themis can't check its accounts right now. Try again shortly.")
	}
	if !open {
		return status.Error(codes.FailedPrecondition, identity.ClosedMessage)
	}
	return nil
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

// refusalCodes say in gRPC why a request under a lease was refused.
var refusalCodes = map[investigation.Kind]codes.Code{
	investigation.Gone:        codes.FailedPrecondition,
	investigation.NotAllowed:  codes.PermissionDenied,
	investigation.BudgetSpent: codes.ResourceExhausted,
	investigation.Invalid:     codes.InvalidArgument,
}

// internal says in gRPC why a request was refused, or hides an unexpected
// error from the caller and logs it.
func (s *Server) internal(ctx context.Context, err error) error {
	if errors.Is(err, store.ErrLeaseLost) {
		return status.Error(codes.FailedPrecondition, "the lease is unknown, expired or superseded")
	}
	var refused *investigation.Error
	if errors.As(err, &refused) {
		return status.Error(refusalCodes[refused.Kind], refused.Message)
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
