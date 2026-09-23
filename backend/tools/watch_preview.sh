#!/usr/bin/env bash
# Re-runs `go run ./cmd/preview` whenever render/layout source or a candidate font atlas changes,
# for a fast font/layout iteration loop. `go run` recompiles from source each time, so this also
# picks up freshly re-baked atlases at $MD_ATLAS_DIR (see tools/bakefont/try_font.sh) — a new
# process re-reads them, unlike an in-process watch which can't reload the package's sync.Once'd
# atlas cache.
#
# Usage (from backend/):
#   tools/watch_preview.sh                              # watch source only
#   MD_ATLAS_DIR=/tmp/candidate-font tools/watch_preview.sh -dither=false
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
backend_dir="$(dirname "$script_dir")"
cd "$backend_dir"

watch_paths=(internal/render internal/calendar cmd/preview)
[[ -n "${MD_ATLAS_DIR:-}" ]] && watch_paths+=("$MD_ATLAS_DIR")

checksum() {
    find "${watch_paths[@]}" -type f \( -name '*.go' -o -name '*.fnt' \) -exec stat -f '%m %N' {} + 2>/dev/null \
        | sort
}

last=""
echo "watching: ${watch_paths[*]}" >&2
while true; do
    cur="$(checksum)"
    if [[ "$cur" != "$last" ]]; then
        last="$cur"
        echo "--- $(date '+%H:%M:%S') rendering ---"
        go run ./cmd/preview "$@" || true
    fi
    sleep 0.5
done
