package grpcapi

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/hzerrad/themis/internal/config/settings"
	"github.com/hzerrad/themis/internal/server/modeltest"

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
	return modelSettingsToProto(m), nil
}

func modelSettingsToProto(m settings.Model) *themisv1.GetModelSettingsResponse {
	return &themisv1.GetModelSettingsResponse{
		Provider: m.Provider, BaseUrl: m.BaseURL, Model: m.Name, ApiKey: m.APIKey, StrictOutput: m.StrictOutput, SchemaDialect: m.SchemaDialect,
	}
}

// TakeModelTest hands the investigator a test of the model's connection,
// waiting a few seconds for one.
func (s *Server) TakeModelTest(ctx context.Context, _ *themisv1.TakeModelTestRequest) (*themisv1.TakeModelTestResponse, error) {
	if s.ModelTests == nil {
		return nil, status.Error(codes.Unimplemented, "this core doesn't test the model's connection")
	}
	t, ok := s.ModelTests.Take(ctx)
	if !ok {
		return &themisv1.TakeModelTestResponse{}, nil
	}
	return &themisv1.TakeModelTestResponse{Test: &themisv1.ModelTest{
		Id: t.ID, Settings: modelSettingsToProto(t.Settings), Timeout: durationpb.New(t.Timeout),
	}}, nil
}

// outcomes are the outcomes the investigator reports, as the core names them.
var outcomes = map[themisv1.ModelTestOutcome]modeltest.Outcome{
	themisv1.ModelTestOutcome_MODEL_TEST_OUTCOME_CONNECTED:     modeltest.Connected,
	themisv1.ModelTestOutcome_MODEL_TEST_OUTCOME_INCOMPLETE:    modeltest.Incomplete,
	themisv1.ModelTestOutcome_MODEL_TEST_OUTCOME_UNREACHABLE:   modeltest.Unreachable,
	themisv1.ModelTestOutcome_MODEL_TEST_OUTCOME_NO_ANSWER:     modeltest.NoAnswer,
	themisv1.ModelTestOutcome_MODEL_TEST_OUTCOME_REFUSED:       modeltest.Refused,
	themisv1.ModelTestOutcome_MODEL_TEST_OUTCOME_NOT_IN_FORMAT: modeltest.NotInFormat,
	themisv1.ModelTestOutcome_MODEL_TEST_OUTCOME_CUT_OFF:       modeltest.CutOff,
	themisv1.ModelTestOutcome_MODEL_TEST_OUTCOME_DECLINED:      modeltest.Declined,
	themisv1.ModelTestOutcome_MODEL_TEST_OUTCOME_ERROR:         modeltest.Errored,
}

// ReportModelTest gives a test its result.
func (s *Server) ReportModelTest(_ context.Context, req *themisv1.ReportModelTestRequest) (*themisv1.ReportModelTestResponse, error) {
	if s.ModelTests == nil {
		return nil, status.Error(codes.Unimplemented, "this core doesn't test the model's connection")
	}
	outcome, ok := outcomes[req.GetOutcome()]
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "the outcome is unknown")
	}
	err := s.ModelTests.Report(req.GetId(), modeltest.Result{
		Outcome: outcome, Provider: req.GetProvider(), Address: req.GetAddress(), Model: req.GetModel(),
		StrictOutput: req.GetStrictOutput(), SchemaDialect: req.GetSchemaDialect(), AnsweredBy: req.GetAnsweredBy(),
		Status: int(req.GetStatus()), Detail: req.GetDetail(), Latency: req.GetLatency().AsDuration(), OutputLimit: int(req.GetOutputLimit()),
	})
	if errors.Is(err, modeltest.ErrUnknown) {
		return nil, status.Error(codes.NotFound, "no test is waiting with this id: it ended")
	}
	return &themisv1.ReportModelTestResponse{}, err
}
