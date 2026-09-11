#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
extension_dir="$repo_root/extension"
output_dir="${1:-$repo_root/dist/web-store}"

version="$(sed -nE 's/^[[:space:]]*"version":[[:space:]]*"([0-9]+\.[0-9]+\.[0-9]+)",?[[:space:]]*$/\1/p' "$extension_dir/manifest.json")"
if [[ -z "$version" ]]; then
  echo "could not read a three-part version from extension/manifest.json" >&2
  exit 1
fi

stage="$(mktemp -d "${TMPDIR:-/tmp}/brw-web-store.XXXXXX")"
trap 'rm -rf -- "$stage"' EXIT

runtime_files=(
  manifest.json
  service_worker.js
  offscreen.html
  offscreen.js
  options.html
  options.css
  options.js
  popup.html
  popup.css
  popup.js
)

for file in "${runtime_files[@]}"; do
  if [[ ! -f "$extension_dir/$file" ]]; then
    echo "missing required extension runtime file: $file" >&2
    exit 1
  fi
  cp "$extension_dir/$file" "$stage/$file"
done
cp -R "$extension_dir/icons" "$stage/icons"

mkdir -p "$output_dir"
output_dir="$(cd -- "$output_dir" && pwd)"
archive="$output_dir/brw-extension-$version.zip"
rm -f -- "$archive"

(
  cd -- "$stage"
  zip -X -q -r "$archive" .
)

entries="$(unzip -Z1 "$archive")"
if ! grep -qx 'manifest.json' <<<"$entries"; then
  echo "package validation failed: manifest.json is not at the ZIP root" >&2
  exit 1
fi
if grep -Eq '(^|/)(README|tab_resolution_test|bridge-defaults|\.)' <<<"$entries"; then
  echo "package validation failed: development or local files were included" >&2
  exit 1
fi

echo "$archive"
