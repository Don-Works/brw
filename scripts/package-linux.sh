#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: scripts/package-linux.sh <version> <amd64|arm64> [out-dir]" >&2
}

if [ "$#" -lt 2 ] || [ "$#" -gt 3 ]; then
  usage
  exit 2
fi

version="$1"
arch="$2"
out_dir="${3:-dist/release}"

case "$version" in
  v*)
    echo "version must not include the leading v: $version" >&2
    exit 2
    ;;
esac
if ! [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-].*)?$ ]]; then
  echo "version must start with x.y.z: $version" >&2
  exit 2
fi

case "$arch" in
  amd64|arm64)
    goarch="$arch"
    ;;
  *)
    usage
    exit 2
    ;;
esac

if ! command -v nfpm >/dev/null 2>&1; then
  echo "nfpm is required. Install with: go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest" >&2
  exit 127
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_dir="$repo_root/dist/package/linux-$arch"
root_dir="$work_dir/root"
case "$out_dir" in
  /*) out_abs="$out_dir" ;;
  *) out_abs="$repo_root/$out_dir" ;;
esac

rm -rf "$work_dir"
mkdir -p "$root_dir/usr/bin" "$root_dir/usr/share/brw/doc" "$out_abs"
cd "$repo_root"
export COPYFILE_DISABLE=1

for cmd in brwd brwctl brwcheck brw-devtools-mcp; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" \
    go build -trimpath -ldflags="-s -w" -o "$root_dir/usr/bin/$cmd" "./cmd/$cmd"
done

cp -R "$repo_root/extension" "$root_dir/usr/share/brw/extension"
cp -R "$repo_root/tests" "$root_dir/usr/share/brw/tests"
cp "$repo_root/LICENSE" "$root_dir/usr/share/brw/doc/LICENSE"
cp "$repo_root/README.md" "$root_dir/usr/share/brw/doc/README.md"
find "$root_dir" -name '._*' -delete

export BRW_VERSION="$version"
export BRW_PACKAGE_ROOT="$root_dir"
export NFPM_ARCH="$arch"
nfpm_config="$work_dir/nfpm.yaml"
perl -pe '
  s/\@NFPM_ARCH\@/$ENV{NFPM_ARCH}/g;
  s/\@BRW_VERSION\@/$ENV{BRW_VERSION}/g;
  s/\@BRW_PACKAGE_ROOT\@/$ENV{BRW_PACKAGE_ROOT}/g;
' "$repo_root/packaging/linux/nfpm.yaml" > "$nfpm_config"

nfpm package \
  --config "$nfpm_config" \
  --packager deb \
  --target "$out_abs/brw_${version}_linux_${arch}.deb"

nfpm package \
  --config "$nfpm_config" \
  --packager rpm \
  --target "$out_abs/brw_${version}_linux_${arch}.rpm"
