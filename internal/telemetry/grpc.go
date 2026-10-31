package telemetry

import (
	"context"
	"log/slog"
	"path"
	"slices"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"
)

// GRPCServerTracing traces each call a server receives, in the caller's trace,
// but those to the untraced methods: polls that would fill traces with noise.
func GRPCServerTracing(untraced ...string) grpc.ServerOption {
	return grpc.StatsHandler(otelgrpc.NewServerHandler(otelgrpc.WithFilter(func(info *stats.RPCTagInfo) bool {
		return !slices.Contains(untraced, path.Base(info.FullMethodName))
	})))
}

// GRPCClientTracing traces each call a client makes and sends its trace along.
func GRPCClientTracing() grpc.DialOption {
	return grpc.WithStatsHandler(otelgrpc.NewClientHandler())
}

// propagated are the context's fields a gRPC call carries to its server.
// They correlate logs and are never trusted for anything else. An action ID
// is not among them: the executor takes it from the request it verifies.
var propagated = []string{FieldIncidentID}

// maxIDLen bounds a propagated value, which the caller controls.
const maxIDLen = 64

func metadataKey(field string) string {
	return "themis-" + strings.ReplaceAll(field, "_", "-")
}

// UnaryClientFields sends the context's incident ID with each call.
func UnaryClientFields() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		values := map[string]string{}
		for _, a := range contextAttrs(ctx) {
			values[a.Key] = a.Value.String()
		}
		for _, f := range propagated {
			if v, ok := values[f]; ok {
				ctx = metadata.AppendToOutgoingContext(ctx, metadataKey(f), v)
			}
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// UnaryServerFields adds the incident ID a caller sent to the request's
// context.
func UnaryServerFields() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		var attrs []slog.Attr
		for _, f := range propagated {
			if v := md.Get(metadataKey(f)); len(v) > 0 && validID(v[0]) {
				attrs = append(attrs, slog.String(f, v[0]))
			}
		}
		if len(attrs) > 0 {
			ctx = WithAttrs(ctx, attrs...)
		}
		return handler(ctx, req)
	}
}

func validID(s string) bool {
	if s == "" || len(s) > maxIDLen {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}
