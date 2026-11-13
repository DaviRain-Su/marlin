package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/egoist/mygo"

	appv1 "marlin-desktop/gen/marlin/app/v1"
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
func Connect(chat *ChatService, win *mygo.Window) {
	ctx := context.Background()
	emit := func(state ConnState) {
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
	opts := hostOptions(bin)
	for {
		h, err := sidecar.Start(ctx, opts)
		if err != nil {
			failures++
			if failures >= 5 {
				emit(ConnState{State: "error", Message: err.Error()})
				return
			}
			emit(ConnState{State: "reconnecting", Message: "host failed: retrying"})
			time.Sleep(time.Second)
			continue
		}
		failures = 0
		setCurrentHost(h)

		// Bind the projection store to this Session (a different binding
		// clears the view; design §6.6).
		if err := chat.store.BindSession(h.Caps.SessionID); err != nil {
			slog.Warn("store bind failed", "err", err)
		}
		mgr := client.NewSessionManager(h.Conn(), h.Caps.SessionID)
		// A fresh manager inherits the journal's latest committed turn so a
		// host restart (or app reopen) does not resend stale after_turn.
		if err := mgr.SyncFromSummary(ctx); err != nil {
			slog.Warn("summary sync failed", "err", err)
		}
		chat.setManager(mgr)
		emit(ConnState{State: "ready"})

		watchUnresolved(ctx, mgr, emit)

		<-h.Exited // the host process died; the turn state lives in its journal
		clearCurrentHost(h)
		chat.setManager(nil)
		emit(ConnState{State: "reconnecting", Message: "host exited; restarting"})
		time.Sleep(500 * time.Millisecond)
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

// watchUnresolved surfaces the recovery states the protocol can leave a
// session in (design §6.5/§6.6): while the outcome is unresolved or a tool
// continuation is pending, new work is blocked; the banner stays until the
// session resolves.
func watchUnresolved(ctx context.Context, mgr *client.SessionManager, emit func(ConnState)) {
	s, err := mgr.Summary(ctx)
	if err != nil {
		return // the ready banner already reflects usable state
	}
	if s.RecoveryState != appv1.SessionRecoveryState_SESSION_RECOVERY_STATE_UNRESOLVED &&
		s.RecoveryState != appv1.SessionRecoveryState_SESSION_RECOVERY_STATE_TOOL_CONTINUATION_PENDING {
		return
	}
	emit(ConnState{State: "unresolved", Message: "上次的结果不确定，等待恢复；已暂停新消息"})
	for {
		time.Sleep(2 * time.Second)
		s, err := mgr.Summary(ctx)
		if err != nil {
			continue
		}
		if s.RecoveryState != appv1.SessionRecoveryState_SESSION_RECOVERY_STATE_UNRESOLVED &&
			s.RecoveryState != appv1.SessionRecoveryState_SESSION_RECOVERY_STATE_TOOL_CONTINUATION_PENDING {
			emit(ConnState{State: "ready"})
			return
		}
	}
}
