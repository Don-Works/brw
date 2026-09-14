#!/usr/bin/env bash
set -euo pipefail

# Builds the relocatable archive that scripts/install.sh and the Homebrew
# formula consume. Unlike the .pkg/.deb/.msi, nothing in here is anchored to a
# system prefix, so it can be unpacked into a user's home without a privileged
# step.

usage() {
  echo "usage: scripts/package-tarball.sh <version> <darwin|linux> <amd64|arm64> [out-dir]" >&2
}

if [ "$#" -lt 3 ] || [ "$#" -gt 4 ]; then
  usage
  exit 2
fi

version="$1"
os="$2"
arch="$3"
out_dir="${4:-dist/release}"

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

case "$os" in
  darwin|linux) ;;
  *)
    usage
    exit 2
    ;;
esac

case "$arch" in
  amd64|arm64) ;;
  *)
    usage
    exit 2
    ;;
esac

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
name="brw_${version}_${os}_${arch}"
work_dir="$repo_root/dist/package/tarball-$os-$arch"
stage_dir="$work_dir/$name"
case "$out_dir" in
  /*) out_abs="$out_dir" ;;
  *) out_abs="$repo_root/$out_dir" ;;
esac

rm -rf "$work_dir"
mkdir -p "$stage_dir/bin" "$stage_dir/doc" "$out_abs"
cd "$repo_root"
export COPYFILE_DISABLE=1

for cmd in brw brwd brwctl brwcheck brw-devtools-mcp; do
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags="-s -w -X github.com/Don-Works/brw/internal/mcp.Version=$version -X github.com/Don-Works/brw/internal/cli.Version=$version" -o "$stage_dir/bin/$cmd" "./cmd/$cmd"
  chmod 0755 "$stage_dir/bin/$cmd"
done

# A Go binary's signature is what lets Apple Silicon run it at all; an unsigned
# one is SIGKILLed on launch. Only a macOS host can produce it, and the release
# workflow already builds the darwin archives on macOS. Sign with the Developer
# ID identity when the release has one, so the archives the one-line installer
# downloads carry the same provenance as the .pkg rather than an anonymous
# ad-hoc signature.
if [ "$os" = "darwin" ] && command -v codesign >/dev/null 2>&1; then
  sign_identity="${MACOS_SIGN_IDENTITY:--}"
  sign_flags=""
  if [ "$sign_identity" != "-" ]; then
    sign_flags="--timestamp --options runtime"
    # The release imports the certificate into a temporary keychain that is not
    # in the default search list, so codesign has to be told where to look.
    if [ -n "${MACOS_KEYCHAIN:-}" ]; then
      sign_flags="$sign_flags --keychain ${MACOS_KEYCHAIN}"
    fi
  fi
  for cmd in brw brwd brwctl brwcheck brw-devtools-mcp; do
    # shellcheck disable=SC2086 # sign_flags is a deliberate word list
    codesign --force --sign "$sign_identity" $sign_flags "$stage_dir/bin/$cmd" >/dev/null
  done
  echo "signed the darwin binaries with identity: $sign_identity" >&2
fi

cp -R "$repo_root/extension" "$stage_dir/extension"
cp -R "$repo_root/tests" "$stage_dir/tests"
cp -R "$repo_root/skills" "$stage_dir/skills"
cp "$repo_root/LICENSE" "$stage_dir/doc/LICENSE"
cp "$repo_root/README.md" "$stage_dir/doc/README.md"
find "$stage_dir" -name '._*' -delete
if command -v xattr >/dev/null 2>&1; then
  xattr -cr "$stage_dir" || true
fi

archive="$out_abs/$name.tar.gz"
# gzip -n keeps the builder's filename and clock out of the gzip header. The tar
# entries still carry build mtimes, so the archive is not byte-reproducible.
tar -C "$work_dir" -cf - "$name" | gzip -9n > "$archive"

if command -v shasum >/dev/null 2>&1; then
  (cd "$out_abs" && shasum -a 256 "$name.tar.gz" > "$name.tar.gz.sha256")
else
  (cd "$out_abs" && sha256sum "$name.tar.gz" > "$name.tar.gz.sha256")
fi

echo "$archive"
cat "$out_abs/$name.tar.gz.sha256"
