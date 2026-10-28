package grpcserver

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/hzerrad/themis/internal/auth"
)

// RequireToken refuses calls without the bearer token held in tokenFile. The
// file is read on every call, so rotation needs no restart.
func RequireToken(tokenFile string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		want, err := auth.ReadToken(tokenFile)
		if err != nil {
			return nil, status.Error(codes.Unavailable, "investigator authentication is unavailable")
		}
		md, _ := metadata.FromIncomingContext(ctx)
		for _, v := range md.Get("authorization") {
			if auth.BearerMatches(v, want) {
				return handler(ctx, req)
			}
		}
		return nil, status.Error(codes.Unauthenticated, "a valid bearer token is required")
	}
}

// BearerToken sends token on every call.
type BearerToken string

func (t BearerToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(t)}, nil
}

// RequireTransportSecurity is false: in the cluster, the NetworkPolicy and
// later mTLS protect the channel.
func (BearerToken) RequireTransportSecurity() bool { return false }

var _ credentials.PerRPCCredentials = BearerToken("")
