#!/usr/bin/env bash
# Generate per-device bearer tokens for the whole fleet from a simple CSV, and emit:
#   provisioning/device-tokens.txt   (SECRET, chmod 600) — device_id <tab> token, for flashing (Phase 3)
#   provisioning/rooms.snippet.yaml  — the `rooms:` block (with token_sha256) to paste into your config
#
# Input CSV (default: tools/rooms.csv), one line per display, no header:
#   device_id,display_name,room_calendar_id
# e.g.:
#   rt-lobby-a1,Aspen,aspen@example.com
#
# Usage: ./provision_all.sh [rooms.csv]
set -euo pipefail

CSV="${1:-$(dirname "$0")/rooms.csv}"
OUT="$(dirname "$0")/../provisioning"
[ -f "$CSV" ] || { echo "missing CSV: $CSV (see header for format)"; exit 1; }
command -v openssl >/dev/null || { echo "openssl required"; exit 1; }

mkdir -p "$OUT"; chmod 700 "$OUT"
TOKENS="$OUT/device-tokens.txt"; SNIPPET="$OUT/rooms.snippet.yaml"
: > "$TOKENS"; chmod 600 "$TOKENS"
echo "rooms:" > "$SNIPPET"

n=0
while IFS=, read -r dev name room || [ -n "$dev" ]; do
  [ -z "${dev// }" ] && continue          # skip blank lines
  case "$dev" in \#*) continue;; esac      # skip comments
  dev="$(echo "$dev" | xargs)"; name="$(echo "$name" | xargs)"; room="$(echo "$room" | xargs)"

  tok="$(openssl rand -base64 32 | tr -d '=+/' | cut -c1-43)"
  sha="$(printf '%s' "$tok" | openssl dgst -sha256 -hex | awk '{print $2}')"

  printf '%s\t%s\n' "$dev" "$tok" >> "$TOKENS"      # secret → file only, never stdout
  {
    printf '  - device_id: "%s"\n' "$dev"
    printf '    name: "%s"\n' "$name"
    printf '    room: "%s"\n' "$room"
    printf '    token_sha256: "%s"\n' "$sha"
  } >> "$SNIPPET"
  n=$((n+1))
done < "$CSV"

cat <<EOF

Provisioned $n device(s).
  • $SNIPPET        ← paste this rooms: block into config.yaml / the broker ConfigMap
  • $TOKENS  ← SECRET: device_id + token, used when flashing each unit (Phase 3). Do not commit.

Next: Google auth is keyless (WIF) — no SA key. The only secret is the optional alert webhook:
  kubectl -n meeting-displays create secret generic broker-secrets \\
    --from-literal=alert-webhook-url='https://hooks.slack.com/services/REPLACE'
EOF
