package alertmanager_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hzerrad/themis/internal/incident/intake/alertmanager"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "test", "fixtures", "alerts", name)) //nolint:gosec // a fixture name chosen by the test
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

func TestParseAlertmanagerPayload_Errors(t *testing.T) {
	// mention is what the error must also say: the version error names the
	// version received, so an operator can tell which Alertmanager sent it, and
	// a parse error keeps the decoder's cause, so logs show what was wrong.
	tests := []struct {
		name    string
		body    string
		want    error
		mention string
	}{
		{"other version", `{"version":"5","alerts":[]}`, alertmanager.ErrUnsupportedVersion, `"5"`},
		{"missing version", `{"alerts":[]}`, alertmanager.ErrUnsupportedVersion, ""},
		{"version as a number", `{"version":4,"alerts":[]}`, alertmanager.ErrParseFailed, ""},
		{"empty body", ``, alertmanager.ErrParseFailed, ""},
		{"not JSON", `version=4`, alertmanager.ErrParseFailed, ""},
		{"not an object", `[{"version":"4"}]`, alertmanager.ErrParseFailed, ""},
		{"wrong field type", `{"version":"4","alerts":"none"}`, alertmanager.ErrParseFailed, ""},
		{"duplicate key", `{"version":"4","version":"4","alerts":[]}`, alertmanager.ErrParseFailed, "duplicate"},
		{"trailing data", `{"version":"4","alerts":[]} {}`, alertmanager.ErrParseFailed, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := alertmanager.ParsePayload([]byte(tt.body))

			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if p != nil {
				t.Errorf("payload = %+v, want nil", p)
			}
			if !strings.Contains(err.Error(), tt.mention) {
				t.Errorf("err = %v, want it to mention %s", err, tt.mention)
			}
		})
	}
}
