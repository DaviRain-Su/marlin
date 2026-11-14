package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/egoist/mygo"

	"marlin-desktop/client"
	"marlin-desktop/sidecar"
)

// hostBinaryPath resolves the sidecar binary, in order: MARLIN_HOST_BIN,
// the packaged app's resources (next to the executable or the Resources
// directory of a macOS bundle), then .mygo/devhost (built by `bun run dev`).
// Empty means the app runs without a host and the page shows the offline
// banner.
func hostBinaryPath() string {
	if p := os.Getenv("MARLIN_HOST_BIN"); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, candidate := range []string{
			filepath.Join(dir, "marlin-host"),
			filepath.Join(dir, "..", "Resources", "marlin-host"),
		} {
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	p := ".mygo/devhost"
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// hostOptions assembles the spawn options for one host start: the binary,
// the model route (config file if present, else the dev env), and the
// session binding.
func hostOptions(bin string) sidecar.Options {
	opts := sidecar.Options{Binary: bin}
	if cfgPath, err := sidecar.DefaultConfigPath(); err == nil {
		if _, err := sidecar.LoadConfig(cfgPath); err == nil {
			// The host applies the config itself; the sidecar only hands
			// over the path and leaves the session binding to the host.
			opts.ExtraArgs = append(opts.ExtraArgs, "--config", cfgPath)
			return opts
		} else {
			slog.Warn("host config unreadable; falling back to env credentials", "path", cfgPath, "err", err)
		}
	}
	// Dev path: no config file — devhost reads provider env/flags itself.
	opts.ExtraArgs = append(opts.ExtraArgs, "--provider", hostProvider())
	return opts
}

// currentHost tracks the running sidecar host so the settings service can
// restart it after a config change. The Connect supervisor owns the
// lifecycle; this only holds a handle.
var currentHost struct {
	sync.Mutex
	h *sidecar.Host
}

func hostRunning() bool {
	currentHost.Lock()
	defer currentHost.Unlock()
	return currentHost.h != nil
}

// RestartHost stops the running host; the Connect supervisor respawns it
// with the current config (design §5.2 restart-on-exit).
func RestartHost() {
	currentHost.Lock()
	h := currentHost.h
	currentHost.Unlock()
	if h != nil {
		h.Stop() // Exited closes → the supervisor loop respawns
	}
}

// hostProvider resolves which model gateway the dev host should use.
// MARLIN_HOST_PROVIDER overrides; the dev default is kimi. This flag belongs
// to devhost's CLI; the real Rust host reads its route config host-side.
func hostProvider() string {
	if p := os.Getenv("MARLIN_HOST_PROVIDER"); p != "" {
		return p
	}
	return "kimi"
}

// Connect spawns the sidecar host, installs the session manager, and
// supervises the process for the app's lifetime. It runs on its own
// goroutine and reports every transition through the Status event
// (design doc §5.2–5.3).
func Connect(ctx context.Context, chat *ChatService, win *mygo.Window) {
	emit := func(state ConnState) {
		if ctx.Err() != nil {
			return
		}
		if err := Status.Emit(win, state); err != nil {
			slog.Info("status event dropped", "err", err)
		}
	}
	bin := hostBinaryPath()
	if bin == "" {
		emit(ConnState{State: "offline", Message: "host binary not found (dev: `bun run dev` builds .mygo/devhost)"})
		return
	}

	emit(ConnState{State: "starting"})
	failures := 0
	for ctx.Err() == nil {
		h, err := sidecar.Start(ctx, hostOptions(bin))
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			failures++
			if failures >= 5 {
				emit(ConnState{State: "error", Message: err.Error()})
				return
			}
			emit(ConnState{State: "reconnecting", Message: "host failed: retrying"})
			if !waitForRestart(ctx, time.Second) {
				return
			}
			continue
		}
		failures = 0
		setCurrentHost(h)

		// Bind the projection store and session manager to the registry's
		// current Session (session identity is client-minted; capabilities
		// of a multi-session host report no single binding, design §6.6).
		sid := chat.SessionID()
		if sid == "" {
			sid = "gui-session"
		}
		if err := chat.store.BindSession(sid); err != nil {
			slog.Warn("store bind failed", "err", err)
		}
		mgr := client.NewSessionManager(h.Conn(), sid)
		// A fresh manager inherits the journal's latest committed turn so a
		// host restart (or app reopen) does not resend stale after_turn.
		if err := mgr.SyncFromSummary(ctx); err != nil {
			slog.Warn("summary sync failed", "err", err)
		}
		chat.setManager(mgr)
		emit(ConnState{State: "ready"})

		select {
		case <-ctx.Done():
			h.Stop()
		case <-h.Exited: // the turn state lives in the host's journal
		}
		clearCurrentHost(h)
		chat.setManager(nil)
		if ctx.Err() != nil {
			return
		}
		emit(ConnState{State: "reconnecting", Message: "host exited; restarting"})
		if !waitForRestart(ctx, 500*time.Millisecond) {
			return
		}
	}
}

func waitForRestart(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func setCurrentHost(h *sidecar.Host) {
	currentHost.Lock()
	currentHost.h = h
	currentHost.Unlock()
}

func clearCurrentHost(h *sidecar.Host) {
	currentHost.Lock()
	if currentHost.h == h {
		currentHost.h = nil
	}
	currentHost.Unlock()
}
