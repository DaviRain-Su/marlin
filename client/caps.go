package client

import (
	"context"
	"fmt"
	"time"

	appv1 "marlin-desktop/gen/marlin/app/v1"
)

// Capabilities reports what the host admits and which Session it binds.
type Capabilities struct {
	MaxRequestBytes   uint32
	MaxMessages       uint32
	MaxOutputTextByte uint32
	// SessionID addresses the host-bound Session. Empty means the host has no
	// Session binding (a configuration error for the GUI, design §5.2).
	SessionID string
}

// GetCapabilities performs the authenticated handshake RPC.
func (c *Conn) GetCapabilities(ctx context.Context) (Capabilities, error) {
	resp, err := appv1.NewAppServerClient(c.cc).GetCapabilities(ctx, &appv1.GetCapabilitiesRequest{})
	if err != nil {
		return Capabilities{}, classify(err, "GetCapabilities")
	}
	return Capabilities{
		MaxRequestBytes:   resp.GetMaxRequestBytes(),
		MaxMessages:       resp.GetMaxMessages(),
		MaxOutputTextByte: resp.GetMaxOutputTextBytes(),
		SessionID:         resp.GetSessionId(),
	}, nil
}

// WaitReady polls GetCapabilities until it answers or the context expires.
// Used by the sidecar manager after spawning the host (design §5.2).
func (c *Conn) WaitReady(ctx context.Context, interval time.Duration) (Capabilities, error) {
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	for {
		caps, err := c.GetCapabilities(ctx)
		if err == nil {
			return caps, nil
		}
		if ctx.Err() != nil {
			return Capabilities{}, fmt.Errorf("client: host not ready: %w", ctx.Err())
		}
		select {
		case <-ctx.Done():
			return Capabilities{}, fmt.Errorf("client: host not ready: %w", ctx.Err())
		case <-time.After(interval):
		}
	}
}
