package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/egoist/mygo"
	"marlin-desktop/sidecar"
)

const (
	projectFileLimit    = 500
	projectPreviewLimit = 128 * 1024
	projectGitLimit     = 256 * 1024
	projectGitTimeout   = 5 * time.Second
)

type ProjectSnapshot struct {
	Root      string   `json:"root"`
	Name      string   `json:"name"`
	Branch    string   `json:"branch"`
	Status    string   `json:"status"`
	Files     []string `json:"files"`
	Truncated bool     `json:"truncated"`
}

type ProjectSelection struct {
	SessionID string `json:"sessionId"`
}

// ProjectService inspects the single active workspace. Execution stays in
// the Rust host; this directory is a working directory, not a sandbox.
type ProjectService struct {
	configPath string
	chat       *ChatService
}

func NewProjectService(configPath string, chat *ChatService) *ProjectService {
	return &ProjectService{configPath: configPath, chat: chat}
}

func projectRoot(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("project directory must be an absolute path")
	}
	root, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("project directory: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("project path is not a directory")
	}
	return root, nil
}

func (p *ProjectService) root() (string, error) {
	currentHost.Lock()
	running, workspace := currentHost.h != nil, currentHost.workspace
	currentHost.Unlock()
	if running {
		if workspace == "" {
			return "", nil
		}
		return projectRoot(workspace)
	}
	cfg, err := sidecar.LoadConfig(p.configPath)
	if err != nil || cfg.ToolsWorkspace == "" {
		return "", err
	}
	return projectRoot(cfg.ToolsWorkspace)
}

func (p *ProjectService) Choose(ctx context.Context) (ProjectSelection, error) {
	root, _ := p.root()
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
		Parent: mygo.CallerWindow(ctx), Title: "打开项目", Directory: true,
		DefaultPath: root, ButtonLabel: "打开并启用编码",
		Message: "Agent 可执行本机命令。所有会话共享当前项目，目录不是沙箱。请仅打开信任的项目。",
	})
	if err != nil || len(paths) == 0 {
		return ProjectSelection{}, err
	}
	return p.Open(ctx, paths[0])
}

func (p *ProjectService) Open(ctx context.Context, path string) (ProjectSelection, error) {
	root, err := projectRoot(path)
	if err != nil {
		return ProjectSelection{}, err
	}
	if p.chat == nil {
		return ProjectSelection{}, fmt.Errorf("chat service is unavailable")
	}
	if !p.chat.operation.TryLock() {
		return ProjectSelection{}, fmt.Errorf("another session operation is running")
	}
	defer p.chat.operation.Unlock()
	if p.chat.busy() {
		return ProjectSelection{}, fmt.Errorf("wait for the current turn before changing projects")
	}
	if err := ctx.Err(); err != nil {
		return ProjectSelection{}, err
	}
	currentHost.Lock()
	supervised := currentHost.restart != nil
	currentHost.Unlock()
	if !supervised {
		return ProjectSelection{}, fmt.Errorf("host supervisor is unavailable")
	}
	previous, err := sidecar.LoadConfig(p.configPath)
	if err != nil {
		return ProjectSelection{}, err
	}
	cfg := previous
	cfg.ToolsWorkspace, cfg.ToolsAllow = root, "run_command"
	if err := sidecar.SaveConfig(p.configPath, cfg); err != nil {
		return ProjectSelection{}, err
	}
	meta, err := p.chat.registry.NewSession()
	if err != nil {
		if rollback := sidecar.SaveConfig(p.configPath, previous); rollback != nil {
			return ProjectSelection{}, fmt.Errorf("new session: %v; config rollback: %w", err, rollback)
		}
		return ProjectSelection{}, err
	}
	// Prevent new admissions on the old workspace while its host shuts down.
	p.chat.setManager(nil)
	if err := p.chat.store.BindSession(meta.ID); err != nil {
		return ProjectSelection{}, err
	}
	if err := RestartHost(); err != nil {
		return ProjectSelection{}, err
	}
	return ProjectSelection{SessionID: meta.ID}, nil
}

// boundedOutput keeps memory bounded even if Git produces a huge listing.
type boundedOutput struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	n := len(data)
	room := b.limit - b.buffer.Len()
	if len(data) > room {
		data = data[:room]
		b.truncated = true
	}
	_, _ = b.buffer.Write(data)
	return n, nil
}

func (b *boundedOutput) String() string { return b.buffer.String() }

func projectGit(ctx context.Context, root string, args ...string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, projectGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root, "-c", "core.fsmonitor=false"}, args...)...)
	output := &boundedOutput{limit: projectGitLimit}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return output.String(), output.truncated, err
}

func (p *ProjectService) State(ctx context.Context) (ProjectSnapshot, error) {
	snap := ProjectSnapshot{Files: []string{}}
	root, err := p.root()
	if err != nil || root == "" {
		return snap, err
	}
	snap.Root, snap.Name = root, filepath.Base(root)
	_, _, gitErr := projectGit(ctx, root, "rev-parse", "--git-dir")
	if gitErr == nil {
		branch, _, err := projectGit(ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD")
		if err != nil {
			branch, _, err = projectGit(ctx, root, "rev-parse", "--short", "HEAD")
			if err != nil {
				return snap, err
			}
			branch = "detached " + branch
		}
		snap.Branch = strings.TrimSpace(branch)
		status, truncated, err := projectGit(ctx, root, "status", "--short", "--untracked-files=normal", "--", ".")
		if err != nil {
			return snap, err
		}
		snap.Status, snap.Truncated = status, truncated
		files, truncated, err := projectGit(ctx, root, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", ".")
		if err != nil {
			return snap, err
		}
		seen := map[string]bool{}
		paths := strings.Split(files, "\x00")
		if truncated {
			paths = paths[:len(paths)-1]
		}
		for _, path := range paths {
			if path == "" || seen[path] {
				continue
			}
			if len(snap.Files) == projectFileLimit {
				snap.Truncated = true
				break
			}
			seen[path] = true
			snap.Files = append(snap.Files, path)
		}
		snap.Truncated = snap.Truncated || truncated
	} else {
		if ctx.Err() != nil {
			return snap, ctx.Err()
		}
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if path == root {
				return nil
			}
			if entry.IsDir() {
				switch entry.Name() {
				case ".git", "node_modules", "target", "vendor", "dist", "build", ".venv":
					return filepath.SkipDir
				}
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if len(snap.Files) == projectFileLimit {
				snap.Truncated = true
				return fs.SkipAll
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			snap.Files = append(snap.Files, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			return snap, err
		}
	}
	sort.Strings(snap.Files)
	return snap, nil
}

func (p *ProjectService) ReadFile(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	root, err := p.root()
	if err != nil {
		return "", err
	}
	if root == "" {
		return "", fmt.Errorf("open a project first")
	}
	path = filepath.FromSlash(path)
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("file path must be relative")
	}
	clean := filepath.Clean(path)
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		if part == ".." || part == ".git" {
			return "", fmt.Errorf("file path is outside the project")
		}
	}
	target, err := filepath.EvalSymlinks(filepath.Join(root, clean))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("file symlink is outside the project")
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == ".git" {
			return "", fmt.Errorf("Git internals cannot be previewed")
		}
	}
	base, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer base.Close()
	info, err := base.Stat(rel)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("preview requires a regular file")
	}
	file, err := base.Open(rel)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("preview requires a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, projectPreviewLimit+1))
	if err != nil {
		return "", err
	}
	if len(data) > projectPreviewLimit {
		return "", fmt.Errorf("file exceeds the 128 KiB preview limit")
	}
	if !utf8.Valid(data) || bytes.ContainsRune(data, 0) {
		return "", fmt.Errorf("binary files cannot be previewed")
	}
	return string(data), nil
}

func (p *ProjectService) Diff(ctx context.Context) (string, error) {
	root, err := p.root()
	if err != nil {
		return "", err
	}
	if root == "" {
		return "", fmt.Errorf("open a project first")
	}
	if _, _, err := projectGit(ctx, root, "rev-parse", "--git-dir"); err != nil {
		return "", fmt.Errorf("project is not a Git repository")
	}
	args := []string{"diff", "--no-ext-diff", "--no-textconv", "--relative", "HEAD", "--", "."}
	if _, _, err := projectGit(ctx, root, "rev-parse", "--verify", "HEAD"); err != nil {
		// An unborn repository has no HEAD; its staged diff is still useful.
		args = []string{"diff", "--cached", "--no-ext-diff", "--no-textconv", "--relative", "--", "."}
	}
	diff, truncated, err := projectGit(ctx, root, args...)
	if err != nil {
		return "", err
	}
	if truncated {
		diff = strings.ToValidUTF8(diff, "�") + "\n[Diff truncated at 256 KiB]\n"
	}
	return diff, nil
}
