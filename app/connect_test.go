package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHostOptionsDoNotPassMissingConfigToRustHost(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MARLIN_DATA_DIR", root)
	if args := hostOptions("/tmp/marlin-host").ExtraArgs; len(args) != 0 {
		t.Fatalf("missing config: %v", args)
	}
	path := filepath.Join(root, "app-sidecar.toml")
	if err := os.WriteFile(path, []byte("provider = 'kimi'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := hostOptions("/tmp/marlin-host").ExtraArgs
	if len(args) != 2 || args[0] != "--config" || args[1] != path {
		t.Fatalf("existing config: %v", args)
	}
}

func TestRestartSignalWakesBackoff(t *testing.T) {
	wake := make(chan struct{}, 1)
	wake <- struct{}{}
	if !waitForRestartSignal(context.Background(), time.Hour, wake) {
		t.Fatal("restart failed to wake")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitForRestartSignal(ctx, time.Hour, nil) {
		t.Fatal("shutdown ignored")
	}
}
