#!/bin/sh
# Re-vendor app_server.proto from a pinned marlin-agent-core commit.
# Changing the pin requires reviewing docs/design.md §4 semantics table.
set -euo pipefail
cd "$(dirname "$0")/.."
PIN="$(cut -d' ' -f1 scripts/PROTO_PIN)"
BRANCH="$(cut -d' ' -f2 scripts/PROTO_PIN)"
SRC="https://raw.githubusercontent.com/tao3k/marlin-agent-core/${PIN}/crates/marlin-agent-app-server/proto/marlin/app/v1/app_server.proto"
curl -fsSL "$SRC" -o proto/marlin/app/v1/app_server.proto
echo "vendored proto from ${BRANCH} @ ${PIN}"
