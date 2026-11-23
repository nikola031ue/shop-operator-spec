#!/usr/bin/env bash
# Fetch the upstream database-operator CRDs used by envtest into test/crds/.
# Versions MUST match kube-state/.github/upstream-charts.yaml (research R6).
set -euo pipefail

CNPG_CHART="oci://ghcr.io/cloudnative-pg/charts/cloudnative-pg"
CNPG_VERSION="0.29.2"
REDIS_CHART="oci://ghcr.io/ot-container-kit/charts/redis-operator"
REDIS_VERSION="0.27.0"

cd "$(dirname "${BASH_SOURCE[0]}")/.."
out="test/crds"
mkdir -p "$out"
for t in helm yq; do command -v "$t" >/dev/null || { echo "ERROR: $t not found" >&2; exit 2; }; done

# pick FILE-or-STDIN NAME -> exactly one CRD document with metadata.name == NAME
pick() {
  yq eval-all "[select(.kind == \"CustomResourceDefinition\" and .metadata.name == \"$1\")] | .[0]" -
}

# CNPG ships its CRDs in templates/, so render the chart.
helm template cnpg "$CNPG_CHART" --version "$CNPG_VERSION" \
  | pick clusters.postgresql.cnpg.io > "$out/postgresql.cnpg.io_clusters.yaml"

# OT redis-operator ships its CRDs in crds/.
helm show crds "$REDIS_CHART" --version "$REDIS_VERSION" \
  | pick redis.redis.redis.opstreelabs.in > "$out/redis.redis.opstreelabs.in_redis.yaml"

for f in "$out"/*.yaml; do
  name="$(yq eval '.metadata.name' "$f")"
  [[ -n "$name" && "$name" != null ]] || { echo "ERROR: $f is empty" >&2; exit 1; }
  echo "wrote $f ($name)"
done
