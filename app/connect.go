package app

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/egoist/mygo"

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
		h, err := sidecar.Start(ctx, sidecar.Options{Binary: bin})
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

		mgr := client.NewSessionManager(h.Conn(), h.Caps.SessionID)
		// A fresh manager inherits the journal's latest committed turn so a
		// host restart (or app reopen) does not resend stale after_turn.
		if err := mgr.SyncFromSummary(ctx); err != nil {
			slog.Warn("summary sync failed", "err", err)
		}
		chat.setManager(mgr)
		emit(ConnState{State: "ready"})

		<-h.Exited // the host process died; the turn state lives in its journal
		chat.setManager(nil)
		emit(ConnState{State: "reconnecting", Message: "host exited; restarting"})
		time.Sleep(500 * time.Millisecond)
	}
}
