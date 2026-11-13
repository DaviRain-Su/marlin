// Package client owns the gRPC connection and Session semantics for the
// Marlin App Server host. It holds no UI code; see docs/design.md §6.
package client

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
)

// Config configures a connection to a Marlin App Server host.
//
// The transport is plaintext and must stay on 127.0.0.1 (see design §5.2).
// AuthToken is a one-time secret the host was started with; it travels as
// metadata on every call.
type Config struct {
	// Addr is the host address, e.g. "127.0.0.1:49152".
	Addr string
	// AuthToken authorizes every request. May be empty for hosts without auth.
	AuthToken string

	// KeepAlive interval; zero uses the default (30s ping / 10s timeout).
	KeepAlive time.Duration
	// DialTimeout bounds connection setup; zero uses 10s.
	DialTimeout time.Duration
}

// AuthMetadataKey carries the host token. ASSUMPTION: upstream has not
// pinned the metadata key yet (design gap §12.6-2); "authorization" with a
// Bearer prefix is the placeholder. Changing it is a one-line fix here.
const AuthMetadataKey = "authorization"

// Conn is a connected client session.
type Conn struct {
	cfg Config
	cc  *grpc.ClientConn
}

// Dial connects to the host. It returns quickly; readiness is checked by the
// first RPC (see WaitReady).
func Dial(ctx context.Context, cfg Config) (*Conn, error) {
	if cfg.Addr == "" {
		return nil, fmt.Errorf("client: empty addr")
	}
	dialTimeout := cfg.DialTimeout
	if dialTimeout == 0 {
		dialTimeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	ka := keepalive.ClientParameters{}
	if cfg.KeepAlive > 0 {
		ka.Time = cfg.KeepAlive
		ka.Timeout = cfg.KeepAlive / 3
	}
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(authUnary(cfg.AuthToken)),
		grpc.WithChainStreamInterceptor(authStream(cfg.AuthToken)),
	}
	if ka.Time > 0 {
		opts = append(opts, grpc.WithKeepaliveParams(ka))
	}
	cc, err := grpc.NewClient(cfg.Addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("client: dial %s: %w", cfg.Addr, err)
	}
	return &Conn{cfg: cfg, cc: cc}, nil
}

// GRPC exposes the underlying channel for direct RPCs.
func (c *Conn) GRPC() *grpc.ClientConn { return c.cc }

// Close tears down the channel.
func (c *Conn) Close() error { return c.cc.Close() }

func authUnary(token string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return invoker(authCtx(ctx, token), method, req, reply, cc, opts...)
	}
}

func authStream(token string) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		return streamer(authCtx(ctx, token), desc, cc, method, opts...)
	}
}

func authCtx(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, AuthMetadataKey, "Bearer "+token)
}
