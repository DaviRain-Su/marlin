# Project coding, first increment

2026-11-15. Extend the existing MyGo/Marlin architecture in `../design.md`.

Users can choose a local project, ask the existing agent to inspect/edit it and
run checks, and inspect files and Git changes in the app. Opening a project starts
a fresh conversation and persists the active working directory.

This increment has one global active workspace. Existing conversations use that
workspace too; the UI must say so. A working directory is not a sandbox.
Per-project conversation routing, worktrees, approvals, tool cards, integrated
terminals, and external agent backends remain separate work.

Acceptance: a scratch Git project can be opened, modified by the real provider's
shell tool, tested, and inspected in the file/diff panel. Personal data must not
be used for QA.
