package grpcapi_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hzerrad/themis/internal/config/settings"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

type fixedSettings settings.Model

func (f fixedSettings) Model(context.Context) (settings.Model, error) { return settings.Model(f), nil }

// The investigator gets setup's model settings, key included. Nobody else
// can ask: the call needs the investigator's own token.
func TestGetModelSettings(t *testing.T) {
	e := newEnv(t)
	strict := false
	e.server.Settings = fixedSettings{BaseURL: "https://api.openai.com/v1", Name: "gpt-5", APIKey: "sk-secret", StrictOutput: &strict}
	got, err := e.investigations.GetModelSettings(t.Context(), &themisv1.GetModelSettingsRequest{})
	if err != nil || got.GetBaseUrl() != "https://api.openai.com/v1" || got.GetModel() != "gpt-5" || got.GetApiKey() != "sk-secret" || got.StrictOutput == nil || got.GetStrictOutput() {
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
