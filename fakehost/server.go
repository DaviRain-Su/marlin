package fakehost

import (
	"context"
	"net"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	appv1 "marlin-desktop/gen/marlin/app/v1"
)

// NewServer builds a gRPC server serving h on a real HTTP/2 channel.
// When token is non-empty, every RPC must carry metadata
// "authorization: Bearer <token>" or it is rejected as Unauthenticated.
func NewServer(h *Host, token string) *grpc.Server {
	opts := []grpc.ServerOption{}
	if token != "" {
		opts = append(opts, grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			if err := verifyToken(ctx, token); err != nil {
				return nil, err
			}
			return handler(ctx, req)
		}), grpc.ChainStreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			if err := verifyToken(ss.Context(), token); err != nil {
				return err
			}
			return handler(srv, ss)
		}))
	}
	s := grpc.NewServer(opts...)
	appv1.RegisterAppServerServer(s, h)
	return s
}

// ServeOn listens on ln and serves s; it returns when s stops or ln closes.
func ServeOn(s *grpc.Server, ln net.Listener) error {
	return s.Serve(ln)
}

// Listen binds an ephemeral loopback port.
func Listen() (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:0")
}

func verifyToken(ctx context.Context, want string) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing metadata")
	}
	vals := md.Get(AuthMetadataKeyClient)
	if len(vals) == 0 {
		return status.Error(codes.Unauthenticated, "missing auth metadata")
	}
	const prefix = "Bearer "
	for _, v := range vals {
		if strings.TrimPrefix(v, prefix) == want {
			return nil
		}
	}
	return status.Error(codes.Unauthenticated, "invalid auth token")
}

// AuthMetadataKeyClient mirrors client.AuthMetadataKey without importing the
// client package (keeps fakehost dependency-free in that direction).
const AuthMetadataKeyClient = "authorization"
