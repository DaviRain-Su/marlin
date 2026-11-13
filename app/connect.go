package app

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/egoist/mygo"

	appv1 "marlin-desktop/gen/marlin/app/v1"
	"marlin-desktop/client"
	"marlin-desktop/sidecar"
)

// hostBinaryPath resolves the sidecar binary: MARLIN_HOST_BIN first, then
// .mygo/devhost (built by `bun run dev`). Empty means the app runs without a
// host and the page shows the offline banner.
func hostBinaryPath() string {
	if p := os.Getenv("MARLIN_HOST_BIN"); p != "" {
		return p
	}
	p := ".mygo/devhost"
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
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
	for {
		h, err := sidecar.Start(ctx, sidecar.Options{
			Binary:    bin,
			ExtraArgs: []string{"--provider", hostProvider()},
		})
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
		chat.setManager(nil)
		emit(ConnState{State: "reconnecting", Message: "host exited; restarting"})
		time.Sleep(500 * time.Millisecond)
	}
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
