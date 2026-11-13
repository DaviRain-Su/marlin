#!/bin/sh
# Generate Go bindings for marlin.app.v1 from the vendored proto.
# Output: gen/marlin/app/v1/*.pb.go (see buf.gen.yaml).
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="$HOME/go/bin:$PATH"

command -v buf >/dev/null 2>&1 || { echo "buf missing: run 'go install github.com/bufbuild/buf/cmd/buf@latest'" >&2; exit 1; }
command -v protoc-gen-go >/dev/null 2>&1 || { echo "protoc-gen-go missing: run 'go install google.golang.org/protobuf/cmd/protoc-gen-go@latest'" >&2; exit 1; }
command -v protoc-gen-go-grpc >/dev/null 2>&1 || { echo "protoc-gen-go-grpc missing: run 'go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest'" >&2; exit 1; }

exec buf generate
