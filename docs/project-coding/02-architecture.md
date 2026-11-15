# Architecture

Keep the pinned upstream gRPC schema and the existing web frontend.

- `ProjectService` owns native directory selection and bounded, read-only local
  file/Git inspection. It shares the chat operation lock for project changes.
- The existing host config persists `tools_workspace`; opening a project enables
  `run_command`, creates a conversation, and asks the supervisor to restart.
- The supervisor remains alive after startup failures and accepts explicit
  restarts. Missing config is not passed as an existing file.
- Rust owns execution. Its tool gateway supplies the canonical working directory
  and bounded root AGENTS.md/CLAUDE.md instructions as model system context.
- UI displays the actual active workspace, files, branch, status, and diff.

Source context: [T3 Code](https://github.com/pingdotgg/t3code) is a control plane
for existing coding agents, while Marlin currently runs its own model/tool loop.
See [Claude Code overview](https://code.claude.com/docs/en/overview) for the
broader coding harness, memory, permissions, MCP, and parallel-agent workflows.
