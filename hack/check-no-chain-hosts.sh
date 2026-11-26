#!/usr/bin/env bash
# Fails if CI or test settings name Sepolia or a hosted RPC provider (constitution Principle IV,
# spec 005 FR-019, SC-008): tests use a local Anvil chain only. shop-operator is the only
# component that reads the chain, so this is where the check lives.
set -euo pipefail
cd "$(dirname "$0")/.."

# Host names and the chain id, not the word: CRD descriptions and comments may say "Sepolia".
pattern='[a-z0-9-]*sepolia[a-z0-9-]*\.[a-z0-9.-]+|infura\.io|alchemy\.com|alchemyapi|quiknode|publicnode|drpc\.org|ankr\.com|chainstack|11155111'
targets=(.github/workflows Makefile test config)
mapfile -t tests < <(git ls-files '*_test.go')

if hits="$(grep -n -i -E "$pattern" -r "${targets[@]}" "${tests[@]}" 2>/dev/null)"; then
  echo "::error::CI or tests name Sepolia or an RPC provider; use the local Anvil chain:" >&2
  echo "$hits" >&2
  exit 1
fi
echo "no Sepolia or provider host in CI or tests (${#tests[@]} test files checked)"
