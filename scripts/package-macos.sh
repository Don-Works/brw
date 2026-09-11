#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: scripts/package-macos.sh <version> [out-dir]" >&2
  echo "" >&2
  echo "Signing is driven entirely by the environment. With none of it set the" >&2
  echo "script ad-hoc signs, which is what an unreleased local build wants." >&2
  echo "" >&2
  echo "  MACOS_SIGN_IDENTITY       Developer ID Application identity (name or SHA-1)" >&2
  echo "  MACOS_INSTALLER_IDENTITY  Developer ID Installer identity (name or SHA-1)" >&2
  echo "  MACOS_KEYCHAIN            keychain holding those identities, if not the default" >&2
  echo "  APPLE_API_KEY_PATH        App Store Connect .p8 private key, for notarytool" >&2
  echo "  APPLE_API_KEY_ID          App Store Connect key id" >&2
  echo "  APPLE_API_ISSUER_ID       App Store Connect issuer id" >&2
  echo "  APPLE_NOTARY_TIMEOUT      notarytool --wait timeout (default 30m)" >&2
  echo "  BRW_SIGNING_REPORT        file to append a Markdown signing summary to" >&2
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

sign_identity="${MACOS_SIGN_IDENTITY:-}"
installer_identity="${MACOS_INSTALLER_IDENTITY:-}"
sign_keychain="${MACOS_KEYCHAIN:-}"
notary_key="${APPLE_API_KEY_PATH:-}"
notary_key_id="${APPLE_API_KEY_ID:-}"
notary_issuer="${APPLE_API_ISSUER_ID:-}"
notary_timeout="${APPLE_NOTARY_TIMEOUT:-30m}"
signing_report="${BRW_SIGNING_REPORT:-}"

required_tools=(pkgbuild lipo codesign)
if [ -n "$installer_identity" ]; then
  required_tools+=(productsign)
fi
if [ -n "$notary_key" ]; then
  required_tools+=(xcrun)
fi
for tool in "${required_tools[@]}"; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "$tool is required to build the macOS package" >&2
    exit 127
  fi
done

banner() {
  echo "" >&2
  echo "==================================================================" >&2
  for line in "$@"; do
    echo "  $line" >&2
  done
  echo "==================================================================" >&2
  echo "" >&2
}

if [ -n "$sign_identity" ]; then
  binary_mode="developer-id"
else
  binary_mode="ad-hoc"
  banner \
    "brw macOS package: AD-HOC SIGNING" \
    "" \
    "MACOS_SIGN_IDENTITY is not set, so the binaries carry an ad-hoc" \
    "signature that identifies nobody. Gatekeeper will report this build" \
    "as coming from an unidentified developer." \
    "" \
    "See docs/release-signing.md to turn on Developer ID signing."
fi

if [ -n "$installer_identity" ]; then
  pkg_mode="developer-id"
else
  pkg_mode="unsigned"
  if [ -n "$sign_identity" ]; then
    banner \
      "brw macOS package: INSTALLER NOT SIGNED" \
      "" \
      "The binaries are Developer ID signed but MACOS_INSTALLER_IDENTITY is" \
      "not set, so the .pkg itself is unsigned and cannot be notarized."
  fi
fi

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

binaries=(brwd brwctl brwcheck brw-devtools-mcp)

for cmd in "${binaries[@]}"; do
  for arch in amd64 arm64; do
    mkdir -p "$work_dir/build/$arch"
    CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" \
      go build -trimpath -ldflags="-s -w -X github.com/Don-Works/brw/internal/mcp.Version=$version" -o "$work_dir/build/$arch/$cmd" "./cmd/$cmd"
  done
  lipo -create \
    "$work_dir/build/amd64/$cmd" \
    "$work_dir/build/arm64/$cmd" \
    -output "$root_dir/usr/local/bin/$cmd"
  chmod 0755 "$root_dir/usr/local/bin/$cmd"
done

cp -R "$repo_root/extension" "$root_dir/usr/local/share/brw/extension"
cp -R "$repo_root/tests" "$root_dir/usr/local/share/brw/tests"
cp -R "$repo_root/skills" "$root_dir/usr/local/share/brw/skills"
cp "$repo_root/LICENSE" "$root_dir/usr/local/share/brw/doc/LICENSE"
cp "$repo_root/README.md" "$root_dir/usr/local/share/brw/doc/README.md"
find "$root_dir" -name '._*' -delete
if command -v xattr >/dev/null 2>&1; then
  xattr -cr "$root_dir" || true
fi

# Signing is the last mutation of the payload tree: xattr -cr strips the
# com.apple.cs.* attributes a signature can live in, and any later rewrite of a
# binary invalidates it silently, leaving pkgbuild to package a broken signature.
codesign_args=(--force)
if [ "$binary_mode" = "developer-id" ]; then
  # --timestamp and --options runtime are both preconditions for notarization.
  codesign_args+=(--timestamp --options runtime --sign "$sign_identity")
  if [ -n "$sign_keychain" ]; then
    codesign_args+=(--keychain "$sign_keychain")
  fi
else
  codesign_args+=(--sign -)
fi
for cmd in "${binaries[@]}"; do
  codesign "${codesign_args[@]}" "$root_dir/usr/local/bin/$cmd" >/dev/null
done

final_pkg="$out_abs/brw_${version}_macos_universal.pkg"
staged_pkg="$work_dir/brw_${version}_macos_universal_unsigned.pkg"

pkgbuild \
  --root "$root_dir" \
  --identifier "co.donworks.brw" \
  --version "$pkg_version" \
  --install-location "/" \
  "$staged_pkg"

rm -f "$final_pkg"
if [ "$pkg_mode" = "developer-id" ]; then
  productsign_args=(--sign "$installer_identity")
  if [ -n "$sign_keychain" ]; then
    productsign_args+=(--keychain "$sign_keychain")
  fi
  productsign "${productsign_args[@]}" "$staged_pkg" "$final_pkg"
else
  mv -f "$staged_pkg" "$final_pkg"
fi

notary_mode="skipped"
if [ "$pkg_mode" = "developer-id" ] && [ -n "$notary_key" ] && [ -n "$notary_key_id" ] && [ -n "$notary_issuer" ]; then
  echo "Submitting $(basename "$final_pkg") to the Apple notary service" >&2
  xcrun notarytool submit "$final_pkg" \
    --key "$notary_key" \
    --key-id "$notary_key_id" \
    --issuer "$notary_issuer" \
    --wait \
    --timeout "$notary_timeout"
  xcrun stapler staple "$final_pkg"
  xcrun stapler validate "$final_pkg"
  notary_mode="stapled"
elif [ "$pkg_mode" = "developer-id" ]; then
  banner \
    "brw macOS package: NOT NOTARIZED" \
    "" \
    "The .pkg is Developer ID signed but no App Store Connect API key is" \
    "configured, so it carries no notarization ticket. Gatekeeper on a" \
    "machine that has never seen this signature will still block it."
fi

echo "brw-signing-mode: binaries=$binary_mode pkg=$pkg_mode notarization=$notary_mode" >&2

if [ -n "$signing_report" ]; then
  mkdir -p "$(dirname "$signing_report")"
  {
    echo "### macOS (universal .pkg)"
    echo ""
    case "$binary_mode" in
      developer-id) echo "- Binaries: signed with a Developer ID Application certificate, hardened runtime, secure timestamp." ;;
      *) echo "- Binaries: **ad-hoc signed**. The signature identifies no developer." ;;
    esac
    case "$pkg_mode" in
      developer-id) echo "- Installer: signed with a Developer ID Installer certificate." ;;
      *) echo "- Installer: **unsigned**. Gatekeeper reports an unidentified developer." ;;
    esac
    case "$notary_mode" in
      stapled) echo "- Notarization: accepted by Apple, ticket stapled to the .pkg." ;;
      *) echo "- Notarization: **not performed**." ;;
    esac
    echo ""
  } >>"$signing_report"
fi
