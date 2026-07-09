#!/usr/bin/env bash
# Simulate a display device against a running broker — no hardware needed. Useful for testing the
# /display payload path and the low-battery alert (incl. hysteresis/dedupe).
#
#   BROKER=http://localhost:8080 DEVICE=rt-lobby-a1 TOKEN=<raw-token> ./fake_device.sh <cmd>
#
# Commands:
#   gen                 Generate a token + its SHA-256 (paste the hash into config rooms[].token_sha256)
#   display             GET the current framebuffer; print status, ETag, X-Next-Wake, byte count
#   telemetry <pct>     POST one telemetry report at <pct>% battery (default 40)
#   battery-demo        POST 40% twice then 60% then 40% — demonstrates fire-once / re-arm hysteresis
set -euo pipefail

BROKER="${BROKER:-http://localhost:8080}"
DEVICE="${DEVICE:-rt-lobby-a1}"
TOKEN="${TOKEN:-}"
cmd="${1:-display}"

need_token() { [ -n "$TOKEN" ] || { echo "set TOKEN=<raw device token> (use './fake_device.sh gen')"; exit 1; }; }

mv_for_pct() { echo $(( 3300 + ($1 * (4150 - 3300)) / 100 )); }  # mirror firmware linear map

post_telemetry() {
  local pct="$1" mv; mv="$(mv_for_pct "$pct")"
  local body
  body=$(printf '{"fw":"1.0.0","batt_mv":%d,"batt_pct":%d,"heap_free":120000,"heap_min":90000,"rssi":-58,"wake":"timer","wake_ms":1450,"rendered":false,"boot":42,"temp_c":22.4,"rh":47}' "$mv" "$pct")
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
    -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
    --data "$body" "$BROKER/api/v1/telemetry/$DEVICE")
  echo "telemetry pct=$pct% mv=$mv -> HTTP $code"
}

case "$cmd" in
  gen)
    tok="$(openssl rand -base64 32 | tr -d '=+/' | cut -c1-43)"
    sha="$(printf '%s' "$tok" | openssl dgst -sha256 -hex | awk '{print $2}')"
    echo "device token (export TOKEN=...):  $tok"
    echo "token_sha256 (paste into config): $sha"
    ;;
  display)
    need_token
    echo "GET $BROKER/api/v1/display/$DEVICE"
    curl -s -D - -o /dev/null -H "Authorization: Bearer $TOKEN" "$BROKER/api/v1/display/$DEVICE" \
      | grep -iE '^(HTTP/|ETag:|X-Next-Wake:|Content-Length:|Retry-After:)'
    ;;
  telemetry)
    need_token; post_telemetry "${2:-40}"
    ;;
  battery-demo)
    need_token
    echo "1) cross below 45% -> should FIRE (check broker logs / Slack):"; post_telemetry 40
    echo "2) still low        -> should be SILENT (within renotify window):"; post_telemetry 40
    echo "3) charged > 55%    -> re-arms, no alert:";                          post_telemetry 60
    echo "4) cross below again-> should FIRE again:";                          post_telemetry 40
    ;;
  *) echo "unknown command: $cmd"; exit 1 ;;
esac
