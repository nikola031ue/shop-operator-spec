#!/usr/bin/env bash
# Compiles MockUSDT.sol with the pinned solc image and writes build/MockUSDT.{abi,bin}
# (spec 005 research R8). CI runs it and fails if the committed files differ.
# Needs Docker and `npm ci` in this directory first (for @openzeppelin/contracts).
set -euo pipefail

SOLC_IMAGE="ethereum/solc:0.8.28"
here="$(cd "$(dirname "$0")" && pwd)"
# Inside this directory (not mktemp): Docker Desktop can't mount Git Bash's /tmp.
out="$here/.build-tmp"
rm -rf "$out"; mkdir -p "$out"
trap 'rm -rf "$out"' EXIT

[[ -d "$here/node_modules/@openzeppelin/contracts" ]] || { echo "run 'npm ci' in contracts/ first" >&2; exit 1; }

# MSYS_NO_PATHCONV keeps Git Bash on Windows from rewriting the container paths.
MSYS_NO_PATHCONV=1 docker run --rm \
  -v "$here:/src:ro" -v "$out:/out" \
  "$SOLC_IMAGE" \
  --optimize --optimize-runs 200 --evm-version cancun \
  --base-path /src --include-path /src/node_modules \
  --abi --bin --overwrite -o /out /src/MockUSDT.sol

mkdir -p "$here/build"
cp "$out/MockUSDT.abi" "$here/build/MockUSDT.abi"
cp "$out/MockUSDT.bin" "$here/build/MockUSDT.bin"
echo "built: build/MockUSDT.abi, build/MockUSDT.bin ($(wc -c < "$here/build/MockUSDT.bin") hex chars)"
