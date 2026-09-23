#!/usr/bin/env bash
# Bakes a candidate TTF at every size render.go's call sites need, into a scratch directory, then
# prints the env var to preview it — without touching the committed atlases in
# internal/render/fonts/atlas/ (those are go:embed'd into every binary, including the broker).
#
# Usage:
#   tools/bakefont/try_font.sh <bold-ttf> <regular-ttf> [out-dir]
#   MD_ATLAS_DIR=$(tools/bakefont/try_font.sh fonts-candidate/Foo-Bold.ttf fonts-candidate/Foo-Regular.ttf) \
#       go run ./cmd/preview
#
# Sizes baked must match bitmapfont.go's boldSizes/regularSizes — update both lists together if
# render.go ever asks for a new size.
set -euo pipefail

BOLD_SIZES=(53 45 37 35 32 27)
REGULAR_SIZES=(24 29 32)

if [[ $# -lt 2 ]]; then
    echo "usage: $0 <bold-ttf> <regular-ttf> [out-dir]" >&2
    exit 1
fi

bold_ttf=$1
regular_ttf=$2
out_dir=${3:-$(mktemp -d)}

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
mkdir -p "$out_dir"

for sz in "${BOLD_SIZES[@]}"; do
    python3 "$script_dir/bake_font.py" "$bold_ttf" "$sz" "$out_dir/bold_${sz}.fnt" >&2
done
for sz in "${REGULAR_SIZES[@]}"; do
    python3 "$script_dir/bake_font.py" "$regular_ttf" "$sz" "$out_dir/regular_${sz}.fnt" >&2
done

echo "$out_dir"
