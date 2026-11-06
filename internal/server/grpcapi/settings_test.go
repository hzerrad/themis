package grpcapi_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/hzerrad/themis/internal/config/settings"
	"github.com/hzerrad/themis/internal/server/modeltest"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

type fixedSettings settings.Model

func (f fixedSettings) Model(context.Context) (settings.Model, error) { return settings.Model(f), nil }

// The investigator gets setup's model settings, key included. Nobody else
// can ask: the call needs the investigator's own token.
func TestGetModelSettings(t *testing.T) {
	e := newEnv(t)
	strict := false
	e.server.Settings = fixedSettings{
		Provider: "openai", BaseURL: "https://api.openai.com/v1", Name: "gpt-5", APIKey: "sk-secret", StrictOutput: &strict, SchemaDialect: "openai-strict",
	}
	got, err := e.investigations.GetModelSettings(t.Context(), &themisv1.GetModelSettingsRequest{})
	if err != nil || got.GetBaseUrl() != "https://api.openai.com/v1" || got.GetModel() != "gpt-5" || got.GetApiKey() != "sk-secret" || got.StrictOutput == nil || got.GetStrictOutput() ||
		got.GetProvider() != "openai" || got.GetSchemaDialect() != "openai-strict" {
		t.Fatalf("GetModelSettings = %v, %v", got, err)
	}
	anonymous := themisv1.NewInvestigationServiceClient(dial(t, e.lis))
	if _, err := anonymous.GetModelSettings(t.Context(), &themisv1.GetModelSettingsRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("without a token: %v, want Unauthenticated", err)
	}
}

// Without setup's settings, the answer is empty, and the investigator keeps
// its own.
func TestGetModelSettings_None(t *testing.T) {
	e := newEnv(t)
	got, err := e.investigations.GetModelSettings(t.Context(), &themisv1.GetModelSettingsRequest{})
	if err != nil || got.GetBaseUrl() != "" || got.StrictOutput != nil {
		t.Errorf("GetModelSettings = %v, %v", got, err)
	}
}

// The investigator takes a test of the model's connection with the settings
// under test, and its report reaches the page waiting for it.
func TestModelTest_TakeAndReport(t *testing.T) {
	e := newEnv(t)
	if _, err := e.investigations.TakeModelTest(t.Context(), &themisv1.TakeModelTestRequest{}); status.Code(err) != codes.Unimplemented {
		t.Errorf("without tests: %v, want Unimplemented", err)
	}
	tests := modeltest.New()
	e.server.ModelTests = tests
	done := make(chan modeltest.Result, 1)
	go func() {
		r, _ := tests.Run(context.Background(), settings.Model{Provider: "anthropic", Name: "claude-haiku-5-5", APIKey: "sk-ant-typed"})
		done <- r
	}()
	took, err := e.investigations.TakeModelTest(t.Context(), &themisv1.TakeModelTestRequest{})
	test := took.GetTest()
	if err != nil || test.GetSettings().GetApiKey() != "sk-ant-typed" || test.GetSettings().GetProvider() != "anthropic" || test.GetTimeout().AsDuration() != 20*time.Second {
		t.Fatalf("TakeModelTest = %v, %v", took, err)
	}
	if _, err := e.investigations.ReportModelTest(t.Context(), &themisv1.ReportModelTestRequest{Id: "another", Outcome: themisv1.ModelTestOutcome_MODEL_TEST_OUTCOME_CONNECTED}); status.Code(err) != codes.NotFound {
		t.Errorf("a report for no test: %v, want NotFound", err)
	}
	_, err = e.investigations.ReportModelTest(t.Context(), &themisv1.ReportModelTestRequest{
		Id: test.GetId(), Outcome: themisv1.ModelTestOutcome_MODEL_TEST_OUTCOME_REFUSED, Provider: "anthropic", Model: "claude-haiku-5-5", Status: 401,
		Latency: durationpb.New(300 * time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Outcome != modeltest.Refused || r.Status != 401 || r.Provider != "anthropic" || r.Latency != 300*time.Millisecond {
		t.Errorf("result = %+v", r)
	}
}
