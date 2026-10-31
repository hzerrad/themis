package telemetry_test

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	"github.com/hzerrad/themis/internal/telemetry"
)

// loggingHealth logs one line per check with the request's context.
type loggingHealth struct {
	healthpb.UnimplementedHealthServer
	log *slog.Logger
}

func (h loggingHealth) Check(ctx context.Context, _ *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	h.log.InfoContext(ctx, "checked")
	return &healthpb.HealthCheckResponse{}, nil
}

// serve returns a client of a server using the interceptors, and its log.
func serve(t *testing.T) (healthpb.HealthClient, *bytes.Buffer) {
	t.Helper()
	return serveWith(t, nil, nil)
}

// serveTraced is serve, with tracing on both sides.
func serveTraced(t *testing.T, untraced ...string) (healthpb.HealthClient, *bytes.Buffer) {
	t.Helper()
	return serveWith(t, []grpc.ServerOption{telemetry.GRPCServerTracing(untraced...)}, []grpc.DialOption{telemetry.GRPCClientTracing()})
}

func serveWith(t *testing.T, srvOpts []grpc.ServerOption, dialOpts []grpc.DialOption) (healthpb.HealthClient, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(append(srvOpts, grpc.ChainUnaryInterceptor(telemetry.UnaryServerFields()))...)
	healthpb.RegisterHealthServer(srv, loggingHealth{log: telemetry.NewLogger(&buf, "executor", slog.LevelInfo)})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet", append(dialOpts,
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(telemetry.UnaryClientFields()))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return healthpb.NewHealthClient(conn), &buf
}

func TestGRPC_ServerLogsCarryTheCallersIDs(t *testing.T) {
	client, buf := serve(t)
	ctx := telemetry.WithAttrs(t.Context(),
		slog.String(telemetry.FieldIncidentID, "01a18d24-9ab1-7c3c-b65f-1ee86ac42758"),
		slog.String(telemetry.FieldActionID, "act-1"))
	if _, err := client.Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatal(err)
	}
	line := lines(t, buf)[0]
	if line[telemetry.FieldIncidentID] != "01a18d24-9ab1-7c3c-b65f-1ee86ac42758" {
		t.Errorf("got %v, want the caller's incident_id", line)
	}
	// The server takes the action ID from the request, never the caller.
	if _, ok := line[telemetry.FieldActionID]; ok {
		t.Error("action_id reached the server")
	}
}

// The values come from the caller: malformed ones are dropped, not logged.
func TestGRPC_MalformedIDsAreDropped(t *testing.T) {
	for name, id := range map[string]string{
		"injection": `inc","level":"ERROR`,
		"too long":  strings.Repeat("a", 65),
	} {
		t.Run(name, func(t *testing.T) {
			client, buf := serve(t)
			ctx := metadata.AppendToOutgoingContext(t.Context(), "themis-incident-id", id)
			if _, err := client.Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
				t.Fatal(err)
			}
			if v, ok := lines(t, buf)[0][telemetry.FieldIncidentID]; ok {
				t.Errorf("incident_id = %q, want it dropped", v)
			}
		})
	}
}
