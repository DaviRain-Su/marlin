// Package fakehost implements an in-process Marlin App Server for tests and
// the dev-mode host binary (cmd/devhost). It speaks the real marlin.app.v1
// wire protocol, so the client layer is tested against actual gRPC traffic
// without the Rust host. The Rust sidecar replaces it in production.
package fakehost

import (
	"context"
	"fmt"
	"sync"

	appv1 "marlin-desktop/gen/marlin/app/v1"
)

// Host is a minimal AppServer implementation. Session RPCs return
// Unimplemented until M2 wires the session manager.
type Host struct {
	appv1.UnimplementedAppServerServer

	mu        sync.Mutex
	sessionID string
	maxReq    uint32
	maxMsg    uint32
	maxOut    uint32
}

// New builds a host reporting the given Session binding and limits.
// An empty sessionID means "no Session bound" (clients must treat it as a
// configuration error, design §5.2).
func New(sessionID string) *Host {
	return &Host{sessionID: sessionID, maxReq: 256 * 1024, maxMsg: 128, maxOut: 1024 * 1024}
}

// SetSession binds (or rebinds) the Session ID.
func (h *Host) SetSession(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessionID = id
}

// GetCapabilities implements the handshake RPC.
func (h *Host) GetCapabilities(ctx context.Context, req *appv1.GetCapabilitiesRequest) (*appv1.GetCapabilitiesResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	resp := &appv1.GetCapabilitiesResponse{
		MaxRequestBytes:    h.maxReq,
		MaxMessages:        h.maxMsg,
		MaxOutputTextBytes: h.maxOut,
	}
	if h.sessionID != "" {
		resp.SessionId = &h.sessionID
	}
	return resp, nil
}

// SessionTurn ref placeholder for M2.
func (h *Host) StartSessionTurn(ctx context.Context, req *appv1.StartSessionTurnRequest) (*appv1.StartSessionTurnResponse, error) {
	return nil, fmt.Errorf("fakehost: StartSessionTurn unimplemented until M2")
}
