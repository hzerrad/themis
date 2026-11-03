package store_test

import (
	"testing"

	"github.com/hzerrad/themis/internal/store"
)

func TestSettings_PutReadClear(t *testing.T) {
	s, _ := newStore(t)
	put := []store.Setting{
		{Name: "model.name", Value: "qwen3", UpdatedAt: t0, UpdatedBy: "alice"},
		{Name: "model.apiKey", Value: "c2VhbGVk", Encrypted: true, UpdatedAt: t0, UpdatedBy: "alice"},
	}
	if err := s.PutSettings(t.Context(), put, nil); err != nil {
		t.Fatal(err)
	}
	put[0].Value = "gpt-5"
	if err := s.PutSettings(t.Context(), put[:1], []string{"model.apiKey"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Settings(t.Context(), []string{"model.name", "model.apiKey", "model.baseURL"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["model.name"].Value != "gpt-5" || got["model.name"].UpdatedBy != "alice" {
		t.Errorf("settings = %+v, want only the updated name", got)
	}
}
