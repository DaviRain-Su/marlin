# Test specification

- Happy: inspect Git/non-Git roots; preview UTF-8; show staged/unstaged diff;
  list untracked files; restore saved workspace; clear model/URL defaults.
- Boundary: file count and text/output limits, unborn/detached HEAD, spaces and
  Unicode in names, empty workspace, instruction truncation.
- Error: outside-root paths/symlinks, binary/oversized preview, missing root,
  busy project changes, startup config absent, provider switch without new key.
- Rust: verify actual provider request includes root instructions; no false
  success on bare EOF or round exhaustion; existing cancellation regression.
- Real entry points: packaged app + native picker/project panel, real model
  edits a disposable code file and runs its test, inspect resulting Git diff.

Tests are written before implementation. Static web imports cannot substitute
for native MyGo IPC validation.
