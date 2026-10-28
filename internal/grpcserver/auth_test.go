package grpcserver_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"

	"github.com/hzerrad/themis/internal/grpcserver"
)

func call(t *testing.T, tokenFile string, md ...string) error {
	t.Helper()
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(md...))
	_, err := grpcserver.RequireToken(tokenFile)(ctx, nil, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) { return "ok", nil })
	return err
}

func TestRequireToken(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := call(t, file, "authorization", "Bearer s3cret"); err != nil {
		t.Errorf("the right token: %v", err)
	}
	for name, md := range map[string][]string{
		"no token":    nil,
		"wrong token": {"authorization", "Bearer guess"},
		"basic":       {"authorization", "Basic s3cret"},
	} {
		if err := call(t, file, md...); code(err) != codes.Unauthenticated {
			t.Errorf("%s: err = %v, want Unauthenticated", name, err)
		}
	}
}

// An empty or missing token file must not let an empty token in.
func TestRequireToken_NoTokenFile(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{empty, filepath.Join(t.TempDir(), "missing")} {
		if err := call(t, file, "authorization", "Bearer "); code(err) != codes.Unavailable {
			t.Errorf("%s: err = %v, want Unavailable", file, err)
		}
	}
}
