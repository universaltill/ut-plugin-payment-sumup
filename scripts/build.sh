#!/usr/bin/env bash
# Builds the WASI module the till executes in-process (ADR-0001).
# The SumUp affiliate key is a hard-coded constant in src/affiliate.go
# (not a secret, ut-docs#6659): no build-time injection.
set -euo pipefail
cd "$(dirname "$0")/.."
GOOS=wasip1 GOARCH=wasm go build -o bin/plugin.wasm ./src
echo "built bin/plugin.wasm ($(wc -c < bin/plugin.wasm | tr -d ' ') bytes)"
