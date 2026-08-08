#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
crate_dir=$(cd -- "$script_dir/.." && pwd)
snapshot_dir=${1:-$crate_dir/tests/snapshots}
output=${2:-$crate_dir/target/visual-audit.png}

if ! command -v magick >/dev/null 2>&1; then
  printf 'visual-audit: ImageMagick 7 (`magick`) is required\n' >&2
  exit 1
fi

audit_tmp=$(mktemp -d)
trap 'rm -rf -- "$audit_tmp"' EXIT

mapfile -d '' snapshots < <(
  find "$snapshot_dir" -maxdepth 1 -type f -name '*.png' ! -name '*.diff.png' -print0 \
    | sort -z
)
if ((${#snapshots[@]} == 0)); then
  printf 'visual-audit: no PNG snapshots found in %s\n' "$snapshot_dir" >&2
  exit 1
fi

mkdir -p "$(dirname "$output")"
panels=()
index=0
for snapshot in "${snapshots[@]}"; do
  name=$(basename "$snapshot" .png)
  for mode in original quarter grayscale gestalt; do
    panel=$(printf '%s/%04d-%s.png' "$audit_tmp" "$index" "$mode")
    case "$mode" in
      original)
        magick "$snapshot" -resize '640x400>' -background '#111318' \
          -gravity center -extent 640x400 "$panel"
        ;;
      quarter)
        magick "$snapshot" -resize '25%' -resize '400%' -resize '640x400>' \
          -background '#111318' -gravity center -extent 640x400 "$panel"
        ;;
      grayscale)
        magick "$snapshot" -colorspace Gray -resize '640x400>' \
          -background '#111318' -gravity center -extent 640x400 "$panel"
        ;;
      gestalt)
        magick "$snapshot" -colorspace Gray -blur 0x12 -resize '25%' \
          -resize '400%' -resize '640x400>' -background '#111318' \
          -gravity center -extent 640x400 "$panel"
        ;;
    esac
    magick "$panel" -gravity north -background '#20242a' -fill '#e8eaed' \
      -font DejaVu-Sans -pointsize 16 -splice 0x30 \
      -annotate +0+6 "$name · $mode" "$panel"
    panels+=("$panel")
    index=$((index + 1))
  done
done

magick montage "${panels[@]}" -tile 4x -geometry +8+8 \
  -background '#0d0f12' "$output"
printf '%s\n' "$output"
