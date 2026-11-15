package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"marlin-desktop/sidecar"
)

func projectFixture(t *testing.T) (*ProjectService, string) {
	t.Helper()
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	path := filepath.Join(t.TempDir(), "app-sidecar.toml")
	if err := sidecar.SaveConfig(path, sidecar.HostConfig{ToolsWorkspace: root}); err != nil {
		t.Fatal(err)
	}
	return NewProjectService(path, nil), root
}

func TestProjectInspectAndPreview(t *testing.T) {
	svc, root := projectFixture(t)
	os.WriteFile(filepath.Join(root, "hello.go"), []byte("package hello\n// 海\n"), 0o600)
	snap, err := svc.State(context.Background())
	if err != nil || snap.Root != root || len(snap.Files) != 1 || snap.Files[0] != "hello.go" {
		t.Fatalf("snapshot: %+v, %v", snap, err)
	}
	text, err := svc.ReadFile(context.Background(), "hello.go")
	if err != nil || !strings.Contains(text, "海") {
		t.Fatalf("preview: %q, %v", text, err)
	}
	for _, path := range []string{"../outside", "/etc/passwd", ".git/config"} {
		if _, err := svc.ReadFile(context.Background(), path); err == nil {
			t.Fatalf("accepted outside/internal path %q", path)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(outside, []byte("private"), 0o600)
	os.Symlink(outside, filepath.Join(root, "link"))
	if _, err := svc.ReadFile(context.Background(), "link"); err == nil {
		t.Fatal("accepted external symlink")
	}
	os.WriteFile(filepath.Join(root, "binary"), []byte{0, 1, 2}, 0o600)
	if _, err := svc.ReadFile(context.Background(), "binary"); err == nil {
		t.Fatal("accepted binary preview")
	}
	os.WriteFile(filepath.Join(root, "large"), []byte(strings.Repeat("x", projectPreviewLimit+1)), 0o600)
	if _, err := svc.ReadFile(context.Background(), "large"); err == nil {
		t.Fatal("accepted oversized preview")
	}
}

func TestProjectGitStagedUntrackedAndUnborn(t *testing.T) {
	svc, root := projectFixture(t)
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v, %s", args, err, out)
		}
		return string(out)
	}
	git("init", "-q")
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("new content\n"), 0o600)
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored\n"), 0o600)
	os.WriteFile(filepath.Join(root, "ignored"), []byte("hidden"), 0o600)
	git("add", "a.txt")
	diff, err := svc.Diff(context.Background())
	if err != nil || !strings.Contains(diff, "+new content") {
		t.Fatalf("unborn staged diff: %q, %v", diff, err)
	}
	snap, err := svc.State(context.Background())
	if err != nil || strings.Contains(strings.Join(snap.Files, "\n"), "ignored") || snap.Branch == "" {
		t.Fatalf("git state: %+v, %v", snap, err)
	}
	if !strings.Contains(strings.Join(snap.Files, "\n"), ".gitignore") {
		t.Fatal("missing untracked file")
	}
}

func TestProjectEmptyMissingAndBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	svc := NewProjectService(path, nil)
	snap, err := svc.State(context.Background())
	if err != nil || snap.Root != "" {
		t.Fatalf("empty: %+v %v", snap, err)
	}
	if _, err := svc.Open(context.Background(), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("accepted missing directory")
	}
	chat, _ := startFakeChat(t)
	svc = NewProjectService(path, chat)
	chat.operation.Lock()
	defer chat.operation.Unlock()
	if _, err := svc.Open(context.Background(), t.TempDir()); err == nil {
		t.Fatal("accepted project switch during another operation")
	}
}

func TestProjectOpenPersistsAndRequestsRestart(t *testing.T) {
	chat, _ := startFakeChat(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	svc := NewProjectService(path, chat)
	root := filepath.Join(t.TempDir(), "项目 with spaces")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	restart := make(chan struct{}, 1)
	currentHost.Lock()
	previous := currentHost.restart
	currentHost.restart = restart
	currentHost.Unlock()
	defer func() { currentHost.Lock(); currentHost.restart = previous; currentHost.Unlock() }()
	old := chat.SessionID()
	selected, err := svc.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if selected.SessionID == "" || selected.SessionID == old || selected.SessionID != chat.SessionID() {
		t.Fatal("project did not start a fresh conversation")
	}
	cfg, err := sidecar.LoadConfig(path)
	canonical, _ := filepath.EvalSymlinks(root)
	if err != nil || cfg.ToolsWorkspace != canonical || cfg.ToolsAllow != "run_command" {
		t.Fatal("project configuration was not persisted")
	}
	if _, err := chat.manager(); err == nil {
		t.Fatal("old workspace manager remained admissible")
	}
	select {
	case <-restart:
	default:
		t.Fatal("no restart requested")
	}
	// The same path supports recovery even when there is no running host.
	if err := NewSettingsService(path).Apply(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-restart:
	default:
		t.Fatal("failed startup cannot be restarted")
	}
}

func TestProjectLimitsAndCancellation(t *testing.T) {
	svc, root := projectFixture(t)
	for i := 0; i <= projectFileLimit; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("%04d.txt", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := svc.State(context.Background())
	if err != nil || !snap.Truncated || len(snap.Files) != projectFileLimit {
		t.Fatalf("file limit: %+v %v", snap, err)
	}
	exact := strings.Repeat("x", projectPreviewLimit)
	if err := os.WriteFile(filepath.Join(root, "exact"), []byte(exact), 0o600); err != nil {
		t.Fatal(err)
	}
	if text, err := svc.ReadFile(context.Background(), "exact"); err != nil || text != exact {
		t.Fatal("exact preview boundary failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.State(ctx); err == nil {
		t.Fatal("ignored cancellation")
	}
	if _, err := svc.ReadFile(ctx, "exact"); err == nil {
		t.Fatal("preview ignored cancellation")
	}
	out := &boundedOutput{limit: 4}
	n, err := out.Write([]byte("123456"))
	if n != 6 || err != nil || out.String() != "1234" || !out.truncated {
		t.Fatal("output cap failed")
	}
}

func TestProjectGitOutputIsBoundedThroughRealCommand(t *testing.T) {
	_, root := projectFixture(t)
	path := filepath.Join(root, "large.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("large diff line\n", 40000)), 0o600); err != nil {
		t.Fatal(err)
	}
	output, truncated, _ := projectGit(context.Background(), root, "diff", "--no-index", "--no-ext-diff", "--no-textconv", "--", "/dev/null", path)
	if !truncated || len(output) > projectGitLimit {
		t.Fatalf("Git cap bypassed: %d bytes, truncated=%v", len(output), truncated)
	}
}
