// Package grpcserver serves the core's gRPC API to investigators: leases,
// evidence by named template, and proposals. It is the single point that
// enforces an investigation's scope.
package grpcserver

import (
	"context"
	"errors"
	"log/slog"
	"time"

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

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

// DefaultLeaseFor is how long a lease lasts without renewal.
const DefaultLeaseFor = time.Minute

const actor = "investigator"

type Server struct {
	themisv1.UnimplementedInvestigationServiceServer
	themisv1.UnimplementedEvidenceServiceServer
	themisv1.UnimplementedProposalServiceServer

	Store    *store.Store
	Config   config.Config
	Registry evidence.Registry
	Reader   client.Reader
	Logger   *slog.Logger
	Now      func() time.Time
	LeaseFor time.Duration
}

// Register serves every service of s on g.
func (s *Server) Register(g *grpc.Server) {
	themisv1.RegisterInvestigationServiceServer(g, s)
	themisv1.RegisterEvidenceServiceServer(g, s)
	themisv1.RegisterProposalServiceServer(g, s)
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
	s.logger().Error("gRPC request failed", "error", err)
	return status.Error(codes.Internal, "internal error")
}

// profile returns the configured profile of an investigation.
func (s *Server) profile(inv store.Investigation) (config.Profile, error) {
	p, ok := s.Config.Profile(inv.Profile)
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
	evidence.ContainerLogsTail: {
		Description: "The end of one container's logs. Defaults to the pod needing attention most, from the run that last ended.",
		Params:      []string{"pod", "container", "previous"},
	},
}
