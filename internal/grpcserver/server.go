// Package grpcserver serves the core's gRPC API to investigators: leases,
// evidence by named template, and proposals. It is the single point that
// enforces an investigation's scope.
package grpcserver

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/predicate"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/telemetry"

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

	Store    *store.Store
	Config   config.Provider
	Registry evidence.Registry
	Reader   client.Reader
	Logger   *slog.Logger
	Now      func() time.Time
	LeaseFor time.Duration

	// Reviewer proves who a caller outside Themis is. Without one, there are
	// no such callers.
	Reviewer TokenReviewer

	limiters sync.Map
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
	authenticate := RequireToken(tokenFile)
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

func (s *Server) leaseFor() time.Duration {
	if s.LeaseFor > 0 {
		return s.LeaseFor
	}
	return DefaultLeaseFor
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

func targetToProto(t incident.TargetRef) *themisv1.TargetRef {
	return &themisv1.TargetRef{Group: t.Group, Kind: t.Kind, Namespace: t.Namespace, Name: t.Name, Uid: t.UID}
}

func targetFromProto(t *themisv1.TargetRef) incident.TargetRef {
	return incident.TargetRef{Group: t.GetGroup(), Kind: t.GetKind(), Namespace: t.GetNamespace(), Name: t.GetName(), UID: t.GetUid()}
}

var classifications = map[evidence.Classification]themisv1.DataClassification{
	evidence.ClassificationInternal:  themisv1.DataClassification_DATA_CLASSIFICATION_INTERNAL,
	evidence.ClassificationSensitive: themisv1.DataClassification_DATA_CLASSIFICATION_SENSITIVE,
}

func itemToProto(it evidence.Item) *themisv1.EvidenceItem {
	c, ok := classifications[it.Classification]
	if !ok {
		c = themisv1.DataClassification_DATA_CLASSIFICATION_SENSITIVE
	}
	return &themisv1.EvidenceItem{
		Id:             it.ID,
		Template:       it.Template,
		Target:         targetToProto(it.Target),
		ContentType:    "application/json",
		Data:           it.Data,
		Truncated:      it.Truncated,
		OriginalBytes:  int64(it.OriginalBytes),
		Redactions:     int32(it.Redactions), //nolint:gosec // bounded by the item size
		CollectedAt:    timestamppb.New(it.CollectedAt),
		Classification: c,
	}
}

var truths = map[predicate.Truth]themisv1.Truth{
	predicate.True:    themisv1.Truth_TRUTH_TRUE,
	predicate.False:   themisv1.Truth_TRUTH_FALSE,
	predicate.Unknown: themisv1.Truth_TRUTH_UNKNOWN,
}

func predicateToProto(r predicate.Result) *themisv1.PredicateResult {
	return &themisv1.PredicateResult{
		Name: r.Name, Value: truths[r.Value], Reason: r.Reason, EvidenceIds: r.EvidenceIDs, PredicateVersion: r.Version,
	}
}

// templates describe the evidence templates to investigators.
var templates = map[string]*themisv1.EvidenceTemplate{
	evidence.RolloutHistory: {
		Description: "The Deployment's rollout progress, conditions, GitOps ownership markers, and each ReplicaSet revision with its images and resource limits.",
	},
	evidence.PodStates: {
		Description: "Each pod of the Deployment: its revision, readiness, and each container's state and last termination (where OOMKilled shows).",
	},
	evidence.TargetEvents: {
		Description: "Recent events of the Deployment, its ReplicaSets and its pods, newest first.",
	},
	evidence.ReferencedConfig: {
		Description: "The ConfigMaps and Secrets the pod template refers to, and when each ConfigMap last changed. Names and times only, never values.",
	},
	evidence.ContainerLogsTail: {
		Description: "The end of one container's logs. Defaults to the pod needing attention most, from the run that last ended.",
		Params:      []string{"pod", "container", "previous"},
	},
}
