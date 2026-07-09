#!/usr/bin/env bash
# Discover Google Workspace room resources via GAM7 and grant the broker's service account
# free/busy read access on each — no hardcoded room list. Safe by default: prints a plan
# (dry-run); pass --apply to actually make the ACL changes.
#
# Why GAM7 (not gws): this is admin-level calendar-resource ACL management. GAM7 is the mature,
# admin-focused tool; gws is Google's newer official CLI but pre-v1.0 ("expect breaking changes")
# and aimed at end-user/agent use. Revisit gws after its v1.0.
#
# Prereqs: GAM7 installed + authorized as a Workspace admin (https://github.com/GAM-team/GAM).
#   ROOM_FILTER  optional case-insensitive substring/regex to select rooms by name or email
#   SA           service account to grant (default: the broker SA)
#
# Usage:
#   ./share_rooms.sh                 # discover + show the plan (dry-run) + emit rooms.csv lines
#   ./share_rooms.sh --apply         # actually grant free/busy on the matched rooms
#   ROOM_FILTER='floor-3|aspen' ./share_rooms.sh --apply
set -euo pipefail

SA="${SA:-your-broker-sa@your-gcp-project-id.iam.gserviceaccount.com}"
APPLY=0; [ "${1:-}" = "--apply" ] && APPLY=1
command -v gam >/dev/null || { echo "GAM7 not found on PATH — install + authorize as admin first."; exit 1; }

tmp="$(mktemp)"; trap 'rm -f "$tmp"' EXIT
# Pull all calendar resources as CSV; we locate columns by header name so it's version-robust.
gam print resources fields resourceEmail,resourceName > "$tmp"

# Resolve column indices from the header (GAM7 emits resourceEmail / resourceName).
read -r header < "$tmp"
col() { awk -v h="$1" -F, 'NR==1{for(i=1;i<=NF;i++){g=$i;gsub(/\r/,"",g);if(tolower(g)==tolower(h)){print i;exit}}}' "$tmp"; }
ec="$(col resourceEmail)"; nc="$(col resourceName)"
[ -n "$ec" ] || { echo "could not find resourceEmail column in: $header"; exit 1; }

echo "Service account to grant free/busy: $SA"
[ -n "${ROOM_FILTER:-}" ] && echo "Filter: ${ROOM_FILTER}"
echo "Mode: $([ $APPLY -eq 1 ] && echo APPLY || echo DRY-RUN (use --apply to execute))"
echo "──────────────────────────────────────────────────────────────────────────────"

n=0; csv_lines=""
# Skip header (NR>1); split CSV (room names shouldn't contain commas; GAM quotes if they do).
while IFS=, read -r -a f; do
  email="$(echo "${f[$((ec-1))]}" | tr -d '\r')"
  name="$([ -n "$nc" ] && echo "${f[$((nc-1))]}" | tr -d '\r' || echo "$email")"
  [ -z "$email" ] && continue
  if [ -n "${ROOM_FILTER:-}" ] && ! echo "$name $email" | grep -qiE "$ROOM_FILTER"; then continue; fi

  n=$((n+1))
  echo "• $name  <$email>"
  if [ $APPLY -eq 1 ]; then
    # Idempotent enough: if the ACL already exists GAM reports it; we don't treat that as fatal.
    gam calendar "$email" add freebusy user "$SA" || echo "  (already shared or non-fatal error — continuing)"
  else
    echo "    would run: gam calendar $email add freebusy user $SA"
  fi
  csv_lines+="rt-CHANGEME,${name// /-},${email}"$'\n'
done < <(tail -n +2 "$tmp")

echo "──────────────────────────────────────────────────────────────────────────────"
echo "Matched $n room(s)."
echo
echo "rooms.csv lines (assign real device_ids, then feed to provision_all.sh):"
echo "# device_id,display_name,room_calendar_id"
printf '%s' "$csv_lines"
[ $APPLY -eq 0 ] && echo $'\nNothing changed (dry-run). Re-run with --apply to grant free/busy.'
