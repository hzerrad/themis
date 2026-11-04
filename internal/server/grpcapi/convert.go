package grpcapi

import (
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

func openIncident(in store.Incident) *themisv1.OpenIncident {
	return &themisv1.OpenIncident{Id: in.ID, Target: targetToProto(in.Target), State: string(in.State)}
}

func targetToProto(t incident.TargetRef) *themisv1.TargetRef {
	return &themisv1.TargetRef{Group: t.Group, Kind: t.Kind, Namespace: t.Namespace, Name: t.Name, Uid: t.UID}
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

var truths = map[check.Truth]themisv1.Truth{
	check.True:    themisv1.Truth_TRUTH_TRUE,
	check.False:   themisv1.Truth_TRUTH_FALSE,
	check.Unknown: themisv1.Truth_TRUTH_UNKNOWN,
}

func (s *Server) predicateToProto(r check.Result) *themisv1.PredicateResult {
	p := &themisv1.PredicateResult{
		Name: r.Name, Value: truths[r.Value], Reason: r.Reason, EvidenceIds: r.EvidenceIDs, PredicateVersion: r.Version,
	}
	if c, ok := s.Checks.Get(r.Name); ok {
		p.Description = c.Description
	}
	return p
}
