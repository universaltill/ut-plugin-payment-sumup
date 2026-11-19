#!/usr/bin/env bash
# Builds the WASI module the till executes in-process (ADR-0001).
#
# Universal Till's SumUp affiliate key is built in (ut-docs#6659): set
# SUMUP_AFFILIATE_KEY in the environment and it is linked into the module via
# -X main.builtinAffiliateKey. Unset/empty builds a keyless module (CI, PRs,
# local dev): reader payments then decline `no_affiliate_key`. The value is
# never echoed; only "present"/"absent" is printed. For the sandbox, build
# locally with the sandbox key (README "Build").
set -euo pipefail
set +x
cd "$(dirname "$0")/.."

key="${SUMUP_AFFILIATE_KEY:-}"
# Trim surrounding whitespace (a pasted secret often carries a newline); the
# value is never echoed.
key="${key#"${key%%[![:space:]]*}"}"
key="${key%"${key##*[![:space:]]}"}"
if [ -n "$key" ] && ! [[ "$key" =~ ^[A-Za-z0-9_.-]+$ ]]; then
  # Never print the value: only say what is wrong with it.
  echo "ERROR: SUMUP_AFFILIATE_KEY has characters outside [A-Za-z0-9_.-]" >&2
  exit 1
fi

GOOS=wasip1 GOARCH=wasm go build -ldflags "-X main.builtinAffiliateKey=${key}" -o bin/plugin.wasm ./src
if [ -n "$key" ]; then
  echo "built-in affiliate key: present"
else
  echo "built-in affiliate key: absent (keyless build)"
fi
echo "built bin/plugin.wasm ($(wc -c < bin/plugin.wasm | tr -d ' ') bytes)"
