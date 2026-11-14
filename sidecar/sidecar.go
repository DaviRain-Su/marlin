// Package sidecar owns the lifecycle of the Marlin App Server host process:
// port reservation, one-time token, spawn, readiness, and shutdown
// (docs/design.md §5).
//
// ASSUMPTION (§12.6 gap 2): the Rust host binary's CLI is not pinned upstream
// yet. The default flags (--bind-addr / --session-id) and the token
// environment (MARLIN_HOST_AUTH_TOKEN) match cmd/devhost, our development
// stand-in; the Rust host must adopt the same interface or Options must be
// adapted.
package sidecar

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"

	"marlin-desktop/client"
)

// Options configures one host process.
type Options struct {
	// Binary is the host executable path. Required.
	Binary string
	// ExtraArgs are appended after the standard flags.
	ExtraArgs []string
	// SessionID is passed to the host as --session-id.
	SessionID string
	// ExtraEnv are appended to the child's environment after the token.
	ExtraEnv []string
	// AuthToken overrides the generated one-time token (tests only).
	AuthToken string
	// ReadinessTimeout bounds the startup handshake; zero uses 10s.
	ReadinessTimeout time.Duration
	// StopGrace bounds the SIGTERM wait; zero uses 5s.
	StopGrace time.Duration
	// Stdout/Stderr capture the child's output; nil defaults to os.Stdout/err.
	Stdout io.Writer
	Stderr io.Writer
	// Logger receives lifecycle events; nil discards.
	Logger *slog.Logger
}

// TokenEnv is the environment variable that carries the one-time token to
// the host process. The token never goes into argv: ps and friends would
// expose it to every local user (design §5.2: token 只进子进程环境/argv，
// 我们收紧为只用环境).
const TokenEnv = "MARLIN_HOST_AUTH_TOKEN"

// Host is a running sidecar host plus its verified client connection.
type Host struct {
	opts    Options
	token   string
	addr    string
	cmd     *exec.Cmd
	conn    *client.Conn
	Caps    client.Capabilities
	Exited  chan struct{} // closed when the child process exits
	exitErr error
}

// Start reserves a loopback port, spawns the host, and waits for
// GetCapabilities to answer over real HTTP/2.
func Start(ctx context.Context, opts Options) (*Host, error) {
	port, err := reservePort()
	if err != nil {
		return nil, fmt.Errorf("sidecar: reserve port: %w", err)
	}
	token := opts.AuthToken
	if token == "" {
		token, err = newToken()
		if err != nil {
			return nil, fmt.Errorf("sidecar: mint token: %w", err)
		}
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	if opts.SessionID == "" {
		opts.SessionID = "gui-session"
	}
	readiness := opts.ReadinessTimeout
	if readiness == 0 {
		readiness = 10 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}

	args := []string{
		"--bind-addr", addr,
		"--session-id", opts.SessionID,
	}
	args = append(args, opts.ExtraArgs...)
	cmd := exec.Command(opts.Binary, args...)
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	// The token travels in the child's environment only. The child inherits
	// the app's environment (HOME, PATH — the host resolves user paths) and
	// a clean base keeps secrets out of inherited exports.
	cmd.Env = append(os.Environ(), TokenEnv+"="+token)
	cmd.Env = append(cmd.Env, opts.ExtraEnv...)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("sidecar: spawn %s: %w", opts.Binary, err)
	}

	h := &Host{opts: opts, token: token, addr: addr, cmd: cmd, Exited: make(chan struct{})}
	go func() {
		h.exitErr = cmd.Wait()
		close(h.Exited)
	}()

	conn, err := client.Dial(ctx, client.Config{Addr: addr, AuthToken: token, DialTimeout: readiness})
	if err != nil {
		h.stop(2 * time.Second)
		return nil, fmt.Errorf("sidecar: dial host: %w", err)
	}
	h.conn = conn

	readyCtx, cancel := context.WithTimeout(ctx, readiness)
	defer cancel()
	caps, err := conn.WaitReady(readyCtx, 200*time.Millisecond)
	if err != nil {
		h.stop(2 * time.Second)
		return nil, fmt.Errorf("sidecar: host not ready: %w", err)
	}
	// A multi-session host resolves Sessions on demand and reports no
	// single binding in capabilities; an empty SessionID is expected
	// there. The App binds its registry's current session instead.
	h.Caps = caps
	opts.Logger.Info("sidecar host ready", "addr", addr, "session", caps.SessionID)
	return h, nil
}

// Addr is the host's loopback address.
func (h *Host) Addr() string { return h.addr }

// Conn is the authenticated client connection.
func (h *Host) Conn() *client.Conn { return h.conn }

// Stop terminates the host: SIGTERM, then SIGKILL after the grace period.
func (h *Host) Stop() { h.stop(0) }

func (h *Host) stop(grace time.Duration) {
	if h.conn != nil {
		_ = h.conn.Close()
	}
	if h.cmd == nil || h.cmd.Process == nil {
		return
	}
	_ = h.cmd.Process.Signal(syscall.SIGTERM)
	if grace == 0 {
		grace = 5 * time.Second
		if h.opts.StopGrace > 0 {
			grace = h.opts.StopGrace
		}
	}
	select {
	case <-h.Exited:
	case <-time.After(grace):
		_ = h.cmd.Process.Kill()
		<-h.Exited
	}
}

// ExitErr is the child's exit status, valid once Exited is closed.
func (h *Host) ExitErr() error { return h.exitErr }

// reservePort binds and immediately releases an ephemeral loopback port.
// The release-then-spawn race is acceptable on loopback; the readiness
// handshake is the real gate.
func reservePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// newToken mints a 256-bit one-time secret (never persisted, design §5.2).
func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
