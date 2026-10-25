#!/usr/bin/env bash
# End-to-end demo, fully offline: builds faultline and the bundled sample
# service, starts both on loopback ports, runs the four bundled experiments
# and checks that each one reaches its expected verdict.
set -euo pipefail

cd "$(dirname "$0")/.."
BIN=${BIN:-bin}
OUT=${OUT:-out/demo}
export FAULTLINE_CONTROL=${FAULTLINE_CONTROL:-127.0.0.1:17070}
export CATALOG_ADDR=${CATALOG_ADDR:-127.0.0.1:17100}
export PRICING_ADDR=${PRICING_ADDR:-127.0.0.1:17101}
export PRICING_PROXY_ADDR=${PRICING_PROXY_ADDR:-127.0.0.1:17102}

go build -o "$BIN/faultline" ./cmd/faultline
go build -o "$BIN/sampleservice" ./cmd/sampleservice
rm -rf "$OUT"
mkdir -p "$OUT"

pids=()
cleanup() {
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

"$BIN/faultline" serve --listen "$FAULTLINE_CONTROL" --seed 42 --max-ttl 5m --log-level warn 2>"$OUT/faultline.log" &
pids+=($!)
# The catalog reaches pricing through the faultline TCP proxy that the
# experiments declare on $PRICING_PROXY_ADDR.
"$BIN/sampleservice" --catalog-listen "$CATALOG_ADDR" --pricing-listen "$PRICING_ADDR" \
  --pricing-addr "$PRICING_PROXY_ADDR" --pricing-timeout 250ms 2>"$OUT/sampleservice.log" &
pids+=($!)
"$BIN/faultline" health --wait 10s
for _ in $(seq 1 100); do
  curl -fsS "http://$CATALOG_ADDR/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done
curl -fsS "http://$CATALOG_ADDR/healthz" >/dev/null || { echo "sample service did not start" >&2; exit 1; }

echo
echo "== validate =="
"$BIN/faultline" validate experiments/*.yaml

expect() {
  case "$1" in
    01-*) echo 0 ;; # passed
    02-*) echo 2 ;; # failed: hypothesis falsified
    03-*) echo 3 ;; # aborted on SLO breach
    04-*) echo 2 ;; # failed: hypothesis falsified
  esac
}

summary=()
status=0
for f in experiments/*.yaml; do
  name=$(basename "$f" .yaml)
  echo
  echo "== run $name =="
  set +e
  "$BIN/faultline" run "$f" --out "$OUT/$name" --quiet
  code=$?
  set -e
  want=$(expect "$name")
  verdict=$(sed -n 's/^  "verdict": "\(.*\)",$/\1/p' "$OUT/$name/report.json")
  if [[ "$code" == "$want" ]]; then mark=ok; else mark=UNEXPECTED; status=1; fi
  summary+=("$(printf '%-32s verdict=%-8s exit=%s expected=%s %s' "$name" "$verdict" "$code" "$want" "$mark")")
done

echo
echo "== abort experiment report (03-pricing-outage-abort) =="
cat "$OUT/03-pricing-outage-abort/report.md"

echo
echo "== control plane after the runs =="
"$BIN/faultline" proxy ls
echo
"$BIN/faultline" audit | tail -n 12

echo
echo "== summary =="
printf '%s\n' "${summary[@]}"
echo "reports: $OUT/<experiment>/report.{md,json}"
exit "$status"
