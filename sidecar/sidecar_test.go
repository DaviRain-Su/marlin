package sidecar_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"marlin-desktop/client"
	"marlin-desktop/sidecar"
)

var (
	buildOnce sync.Once
	devhost   string
	buildErr  error
)

// buildDevhost compiles cmd/devhost once for the package's tests.
func buildDevhost(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "devhost-build")
		if err != nil {
			buildErr = err
			return
		}
		out := filepath.Join(dir, "devhost")
		cmd := exec.Command("go", "build", "-o", out, "marlin-desktop/cmd/devhost")
		cmd.Dir = projectRoot()
		if out2, err := cmd.CombinedOutput(); err != nil {
			buildErr = err
			t.Logf("go build output: %s", out2)
			return
		}
		devhost = out
	})
	if buildErr != nil {
		t.Fatalf("build devhost: %v", buildErr)
	}
	return devhost
}

// projectRoot walks up from the test's working dir to the module root.
func projectRoot() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "."
		}
		dir = parent
	}
}

func TestStartStopLifecycle(t *testing.T) {
	bin := buildDevhost(t)
	ctx := context.Background()

	h, err := sidecar.Start(ctx, sidecar.Options{Binary: bin, SessionID: "sidecar-test"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if h.Caps.SessionID != "sidecar-test" {
		t.Errorf("Caps.SessionID = %q, want sidecar-test", h.Caps.SessionID)
	}
	// The connection the manager hands back must be usable on its own.
	again, err := h.Conn().GetCapabilities(context.Background())
	if err != nil {
		t.Fatalf("GetCapabilities via returned conn: %v", err)
	}
	if again.SessionID != "sidecar-test" {
		t.Errorf("second read SessionID = %q", again.SessionID)
	}

	h.Stop()
	select {
	case <-h.Exited:
	case <-time.After(10 * time.Second):
		t.Fatal("host did not exit after Stop")
	}
}

func TestSpawnFailsOnMissingBinary(t *testing.T) {
	_, err := sidecar.Start(context.Background(), sidecar.Options{
		Binary: filepath.Join(t.TempDir(), "no-such-host"),
	})
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}

func TestTokenIsPerProcess(t *testing.T) {
	bin := buildDevhost(t)
	h1, err := sidecar.Start(context.Background(), sidecar.Options{Binary: bin})
	if err != nil {
		t.Fatalf("start h1: %v", err)
	}
	defer h1.Stop()
	h2, err := sidecar.Start(context.Background(), sidecar.Options{Binary: bin})
	if err != nil {
		t.Fatalf("start h2: %v", err)
	}
	defer h2.Stop()

	if h1.Addr() == h2.Addr() {
		t.Errorf("two hosts share addr %s; ports must differ", h1.Addr())
	}
	// A foreign token must be rejected by a sidecar-spawned host: auth is
	// enforced end-to-end, not just by cmd/devhost's tests.
	conn, err := client.Dial(context.Background(), client.Config{Addr: h2.Addr(), AuthToken: "foreign-token"})
	if err != nil {
		t.Fatalf("dial h2 with foreign token: %v", err)
	}
	defer conn.Close()
	if _, err := conn.GetCapabilities(context.Background()); err == nil {
		t.Fatal("expected h2 to reject a foreign token")
	}
}
