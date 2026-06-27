#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: scripts/package-macos.sh <version> [out-dir]" >&2
}

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
  usage
  exit 2
fi

version="$1"
out_dir="${2:-dist/release}"

case "$version" in
  v*)
    echo "version must not include the leading v: $version" >&2
    exit 2
    ;;
esac

if ! [[ "$version" =~ ^([0-9]+\.[0-9]+\.[0-9]+)([.-].*)?$ ]]; then
  echo "version must start with x.y.z: $version" >&2
  exit 2
fi
pkg_version="${BASH_REMATCH[1]}"

for tool in pkgbuild lipo codesign; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "$tool is required to build the macOS package" >&2
    exit 127
  fi
done

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_dir="$repo_root/dist/package/macos"
root_dir="$work_dir/root"
case "$out_dir" in
  /*) out_abs="$out_dir" ;;
  *) out_abs="$repo_root/$out_dir" ;;
esac

rm -rf "$work_dir"
mkdir -p "$root_dir/usr/local/bin" "$root_dir/usr/local/share/brw/doc" "$out_abs"
cd "$repo_root"
export COPYFILE_DISABLE=1

for cmd in brwd brwctl brwcheck brw-devtools-mcp; do
  for arch in amd64 arm64; do
    mkdir -p "$work_dir/build/$arch"
    CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" \
      go build -trimpath -ldflags="-s -w" -o "$work_dir/build/$arch/$cmd" "./cmd/$cmd"
  done
  lipo -create \
    "$work_dir/build/amd64/$cmd" \
    "$work_dir/build/arm64/$cmd" \
    -output "$root_dir/usr/local/bin/$cmd"
  chmod 0755 "$root_dir/usr/local/bin/$cmd"
  codesign --force --sign - "$root_dir/usr/local/bin/$cmd" >/dev/null
done

cp -R "$repo_root/extension" "$root_dir/usr/local/share/brw/extension"
cp -R "$repo_root/tests" "$root_dir/usr/local/share/brw/tests"
cp "$repo_root/LICENSE" "$root_dir/usr/local/share/brw/doc/LICENSE"
cp "$repo_root/README.md" "$root_dir/usr/local/share/brw/doc/README.md"
find "$root_dir" -name '._*' -delete
if command -v xattr >/dev/null 2>&1; then
  xattr -cr "$root_dir" || true
fi

pkgbuild \
  --root "$root_dir" \
  --identifier "co.donworks.brw" \
  --version "$pkg_version" \
  --install-location "/" \
  "$out_abs/brw_${version}_macos_universal.pkg"
