# Review and verification

2026-11-15. Scope: first local-project coding increment, preserving MyGo v0.9.0
and the pinned Marlin/Weaver protocol versions.

## Automated checks

PASS:

- `CGO_ENABLED=0 go test ./...`
- `CGO_ENABLED=0 go vet ./...`
- `go test -race ./app ./client ./fakehost ./sidecar`
- `bunx mygo vet .`
- `cargo test --locked --manifest-path host-bin/Cargo.toml` (4 host tests)
- `bun run build` (typed bindings current, TypeScript, Vite, native build,
  signing and DMG creation)
- `git diff --check`

Tests cover project persistence/restart requests, busy switching, absent config,
UTF-8 preview boundaries and outside-root rejection, Git ignores/unborn HEAD,
the 500-file cap, a real subprocess exceeding the 256 KiB output cap, provider
credential changes including implicit Kimi, TOML escaping and unknown host keys,
instruction refresh/limits, cancellation, and false completion on EOF/budget.

Initial failures and their fixes are recorded in `06-implementation-log.md`.
The Rust temporary-directory collision was a test-fixture identity bug, fixed
with an atomic sequence. Final checks passed against the shipped source.

## Native coding acceptance

macOS arm64, packaged app, real provider, isolated app state and journal,
disposable Git repository. Input used native accessibility controls.

| Step | Result | Evidence |
| --- | --- | --- |
| Correct execution location | PASS | Separate scratch-only instance displayed the canonical temporary project path |
| Read, edit and test through the app | PASS | Agent repaired `sum(values[:-1])` to `sum(values)` through the real shell tool |
| Independent correctness check | PASS | Before: 2 of 3 tests failed. After: all 3 passed using `/usr/bin/python3 -m unittest -v` |
| Preserve existing work | PASS | Existing README edit survived; test file stayed unchanged; tracked diff contained the original README edit and calculator fix |
| Root instructions reach the model | PASS | Final answer included the AGENTS.md-only marker `WORKSPACE-QA-VERIFIED` |
| Review actual changes | PASS | Native panel displayed branch, changed paths and the same diff independently obtained from Git; file preview showed corrected code |
| Choose project and restart | PASS | Native picker defaulted to scratch project; explicit confirmation created a fresh session, entered reconnecting, then reached ready |
| Retain old coding transcript | PASS | Switching back after project restart restored the completed transcript and instruction marker |

Local screenshots (generated, not committed):
`build/qa-project-20261115/project-before.png`,
`build/qa-project-20261115/project-diff.png`.
The driver's deterministic marker predicate returned UNKNOWN/untrusted_source;
it is not counted as a pass. Fresh native snapshots, the persisted projection,
filesystem diff and independent test execution supply the evidence instead.
The scratch-only test instance exited normally after acceptance. An earlier
instance was left alone after its directory changed to a real project; no test
prompt was sent there. Its transition from startup error to ready is an
observation, not a fully controlled failure-recovery acceptance test.

## Review boundaries

This is a usable first increment, not parity with Zeron, T3 Code or Claude Code.
One workspace remains global to all conversations. There is no filesystem
sandbox, tool approval UI, durable tool journal, per-turn diff attribution,
worktree management, background parallel agent, terminal, or external CLI backend.

Git output is capped; untracked files appear in status/listing and previews,
not the tracked diff. File previews are read-only. The default tool round limit
remains eight; exhaustion now reports failure rather than fabricated success.
Root instructions are automatic; nested scope instructions rely on the agent
reading them before editing.

Native acceptance covered macOS arm64. Windows/Linux packaging and clean-machine
installation were not exercised. Existing projection rebuild gaps and draft/
queue persistence gaps from the previous audit remain separate work.

The Zeron study is source-based and separately verified; see
`zeron-design-study.md`. It does not imply that Zeron was installed or tested.
