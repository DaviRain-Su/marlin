# Technical specification

`ProjectSnapshot`: root/name/branch/status strings, files string[], truncated bool.
Paths are canonical absolute roots and slash-separated relative file names.
`ProjectSelection`: sessionId string (empty on picker cancellation).

`ProjectService.State(ctx)` reads the running host's workspace, falling back to
the configured workspace while offline. Saving settings without applying them
must not mislabel the directory used by the running host. Empty root gives an
empty snapshot. `Choose(ctx)` opens MyGo's directory picker; `Open(ctx,path)`
validates a local directory, refuses while a chat/session operation owns the
lock, saves the workspace, creates a conversation, invalidates the old manager,
and queues restart. `ReadFile(ctx,path)` returns UTF-8 text only within the root.
`Diff(ctx)` returns a bounded tracked Git diff against HEAD, including staged and
unstaged edits. Untracked files appear in the file list and status, not the diff.
All failures return errors; picker cancellation changes nothing.

Limits: 500 files, 128 KiB file previews, 256 KiB Git output, 5s per Git command.
Git listing uses cached + untracked files with standard ignores. Non-Git listing
skips generated directories and symlinks. Preview paths reject absolute paths,
parent traversal, .git internals, binary data, and symlinks outside the root.
Git commands disable external diffs, textconv, and fsmonitor.

Rust context: read at most 32 KiB per root instruction file, refuse instruction
symlinks outside the root, indicate truncation. Refresh instructions for each
turn. Tool round exhaustion or EOF without a terminal response must be an error,
never a fabricated Completed result.

Restart state transitions: starting -> ready or failed/waiting; explicit restart
wakes failed/waiting; ready -> restarting -> ready/failed; shutdown cancels every
wait and stops the host. Provider changes clear the previous credential and
protocol/auth overrides; empty model/base URL restore defaults.

Edges: empty root, missing directory, regular file as root, relative root,
symlink root, root with spaces, non-Git directory, unborn Git HEAD, detached HEAD,
ignored/untracked/deleted/staged files, traversal, external symlink, binary file,
oversized preview/output, busy chat, missing credentials, canceled picker,
restart after failed startup, and shutdown during restart.
