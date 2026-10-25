#!/usr/bin/env bash
# Measures statement coverage of the internal packages (the library and CLI
# logic; cmd/ and tools/ mains are thin wrappers) and fails below $1 percent.
set -euo pipefail
cd "$(dirname "$0")/.."
min=${1:-80}
if ! log=$(go test -count=1 -covermode=atomic -coverprofile=coverage.out ./internal/... 2>&1); then
  echo "$log" >&2
  exit 1
fi
# Per-package summary from the profile.
awk -F'[: ,]+' 'NR > 1 {
  file = $1; sub(/\/[^\/]*$/, "", file); sub(/^github.com\/SlateGitOrg\/faultline\//, "", file)
  stmts[file] += $(NF-1); if ($NF > 0) hit[file] += $(NF-1)
  total += $(NF-1); if ($NF > 0) covered += $(NF-1)
} END {
  for (p in stmts) printf "%-22s %6.1f%%  (%d/%d statements)\n", p, 100 * hit[p] / stmts[p], hit[p], stmts[p] | "sort"
  close("sort")
  printf "%-22s %6.1f%%  (%d/%d statements)\n", "TOTAL", 100 * covered / total, covered, total
}' coverage.out
total=$(go tool cover -func=coverage.out | awk '/^total:/ { sub(/%/, "", $3); print $3 }')
if awk -v t="$total" -v m="$min" 'BEGIN { exit !(t + 0 < m + 0) }'; then
  echo "coverage ${total}% is below the ${min}% gate" >&2
  exit 1
fi
echo "coverage gate: ${total}% >= ${min}%"
