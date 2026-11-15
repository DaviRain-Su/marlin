# Implementation log

- Added native project selection, fresh conversation creation, canonical
  workspace persistence, and serialized project/config/startup operations.
- Added bounded file listing, rooted UTF-8 preview, Git status/branch and tracked
  diff, including unborn repositories. Files render as text, never HTML.
- The project panel reports the running host directory rather than unapplied
  settings. Explicit refresh updates status and changes.
- Added per-turn root instruction context and canonical shell working directory.
  Provider EOF and exhausted round budgets now fail rather than claim completion.
- Supervisor accepts restart requests after repeated startup failures; a missing
  config is no longer passed as an existing Rust-host config.
- Replaced the ad-hoc settings parser with TOML; preserved host-only keys,
  removed empty overrides, cleared provider-specific credentials on switching.
- Initial tests reproduced absent interfaces, settings path roundtrip failure,
  false completion, and a real Git-output cap bypass (680,483 bytes). Fixed the
  cap by avoiding bytes.Buffer's promoted ReaderFrom method.
- An initial Rust test run exposed temporary-directory collisions under parallel
  execution; added an atomic sequence to the fixture directory identity.

Final checks and isolated native evidence are recorded in `07-review-report.md`.
