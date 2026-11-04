package grpcapi

import (
	"context"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

// GetModelSettings returns the model settings made in Themis's setup. The
// investigator uses them for whatever its own environment leaves unset.
func (s *Server) GetModelSettings(ctx context.Context, _ *themisv1.GetModelSettingsRequest) (*themisv1.GetModelSettingsResponse, error) {
	// An investigator waiting for a model asks this instead of for work: it
	// is still there.
	s.Heartbeat.Beat(ctx, "")
	if s.Settings == nil {
		return &themisv1.GetModelSettingsResponse{}, nil
	}
	m, err := s.Settings.Model(ctx)
	if err != nil {
		return nil, s.internal(ctx, err)
	}
	return &themisv1.GetModelSettingsResponse{BaseUrl: m.BaseURL, Model: m.Name, ApiKey: m.APIKey, StrictOutput: m.StrictOutput}, nil
}
