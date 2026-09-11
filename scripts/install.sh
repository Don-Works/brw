#!/bin/sh
set -eu

# curl -fsSL https://brw.donworks.co.uk/install.sh | sh
#
# Installs brw entirely under the invoking user's home so that no step needs
# sudo: an agent running with ordinary permissions can complete the install
# without handing a script to a human.
#
# Environment:
#   BRW_VERSION           release to install, with or without the leading v
#                         (default: the latest GitHub release)
#   BRW_INSTALL_DIR       payload directory (default: the per-OS app dir below)
#   BRW_BIN_DIR           directory to link the commands into
#                         (default: ~/.local/bin, or <install dir>/bin when
#                         BRW_INSTALL_DIR was set, so a relocated install stays
#                         self-contained)
#   BRW_BASE_URL          where to fetch the archive from (default: the GitHub
#                         release download URL for the resolved version)
#   BRW_NO_SETUP          set to any value to stop before `brwctl setup`
#   BRW_NO_PATH           set to any value to leave shell startup files alone
#   BRW_SKIP_ATTESTATION  set to any value to skip the provenance check; the
#                         SHA256 check still runs

REPO="Don-Works/brw"
COMMANDS="brwd brwctl brwcheck brw-devtools-mcp"
# Everything the archive owns. The install replaces exactly these names and
# nothing else, so a re-run cannot reach config/ or a per-profile extension copy.
PAYLOAD="bin extension tests skills doc"

step() { printf '==> %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

os=""
arch=""
uname_s="$(uname -s)"
case "$uname_s" in
  Darwin) os="darwin" ;;
  Linux) os="linux" ;;
  *) die "unsupported operating system: $uname_s (brw ships tarballs for macOS and Linux; on Windows use the .msi from https://github.com/$REPO/releases)" ;;
esac

uname_m="$(uname -m)"
case "$uname_m" in
  x86_64 | amd64) arch="amd64" ;;
  arm64 | aarch64) arch="arm64" ;;
  *) die "unsupported architecture: $uname_m (brw ships amd64 and arm64)" ;;
esac

downloader=""
if command -v curl >/dev/null 2>&1; then
  downloader="curl"
elif command -v wget >/dev/null 2>&1; then
  downloader="wget"
else
  die "curl or wget is required"
fi

fetch() {
  _url="$1"
  _dest="$2"
  if [ "$downloader" = "curl" ]; then
    case "$_url" in
      # Pinning the protocol stops a redirect from downgrading the download to
      # plaintext. A BRW_BASE_URL override may legitimately be a local mirror.
      https://*) curl -fsSL --proto '=https' --tlsv1.2 --retry 2 -o "$_dest" "$_url" ;;
      *) curl -fsSL --retry 2 -o "$_dest" "$_url" ;;
    esac
  else
    wget -q -O "$_dest" "$_url"
  fi
}

sha256_of() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | cut -d' ' -f1
  elif command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  elif command -v openssl >/dev/null 2>&1; then
    openssl dgst -sha256 "$1" | sed 's/.*= *//'
  else
    return 1
  fi
}

# gh emits its attestation JSON on one line, so a greedy match would return the
# document's last occurrence of a key. Splitting on commas first makes the first
# match the first occurrence in document order.
json_field() {
  tr ',' '\n' < "$1" |
    sed -n "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p" |
    head -n 1
}

# Reads either a single-artifact `<sha>  <name>` file or the release-wide
# SHA256SUMS.txt, whose names carry a ./ prefix and may carry a binary marker.
expected_sha256() {
  tr -d '*' < "$1" | awk -v want="$2" '
    {
      name = $NF
      sub(/^.*\//, "", name)
      if (name == want) {
        print $1
        exit
      }
    }
  '
}

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/brw-install.XXXXXX")"
cleanup() { rm -rf "$tmp_dir"; }
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

version=""
if [ -n "${BRW_VERSION:-}" ]; then
  version="${BRW_VERSION#v}"
else
  step "Resolving the latest brw release from the GitHub API"
  fetch "https://api.github.com/repos/$REPO/releases/latest" "$tmp_dir/latest.json" ||
    die "could not reach the GitHub release API; set BRW_VERSION to install a specific version"
  version="$(tr ',' '\n' < "$tmp_dir/latest.json" |
    sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"v\{0,1\}\([^"]*\)".*/\1/p' |
    head -n 1)"
  [ -n "$version" ] || die "could not read a tag_name from the GitHub release API response"
fi

case "$version" in
  '' | *[!0-9.a-zA-Z_-]*) die "BRW_VERSION must look like 0.10.3, got: $version" ;;
esac

install_dir=""
install_dir_was_set="no"
if [ -n "${BRW_INSTALL_DIR:-}" ]; then
  install_dir="$BRW_INSTALL_DIR"
  install_dir_was_set="yes"
elif [ "$os" = "darwin" ]; then
  install_dir="$HOME/Library/Application Support/brw"
else
  install_dir="$HOME/.local/share/brw"
fi

# The install replaces fixed child names of this directory, so refusing a
# non-absolute path or the filesystem root keeps that replacement bounded.
case "$install_dir" in
  /) die "BRW_INSTALL_DIR must not be the filesystem root" ;;
  /*) ;;
  *) die "BRW_INSTALL_DIR must be an absolute path, got: $install_dir" ;;
esac

if [ -n "${BRW_BIN_DIR:-}" ]; then
  bin_dir="$BRW_BIN_DIR"
elif [ "$install_dir_was_set" = "yes" ]; then
  bin_dir="$install_dir/bin"
else
  bin_dir="$HOME/.local/bin"
fi

archive_name="brw_${version}_${os}_${arch}.tar.gz"
base_url="${BRW_BASE_URL:-https://github.com/$REPO/releases/download/v$version}"
archive_url="$base_url/$archive_name"

printf '\n'
step "brw $version for $os/$arch"
info "archive      $archive_name"
info "source       $base_url"
info "payload      $install_dir"
info "commands     $bin_dir"
if [ -n "${BRW_NO_SETUP:-}" ]; then
  info "after        stop (BRW_NO_SETUP is set)"
else
  info "after        brwctl setup, when the installed brwctl supports it"
fi
printf '\n'

step "Downloading $archive_url"
fetch "$archive_url" "$tmp_dir/$archive_name" ||
  die "could not download $archive_url (check that release $version publishes a $os/$arch tarball)"

step "Verifying the SHA256 checksum"
if ! actual_sha="$(sha256_of "$tmp_dir/$archive_name")"; then
  die "no SHA256 tool found (need shasum, sha256sum or openssl); refusing to install an unverified archive"
fi
expected_sha=""
if fetch "$archive_url.sha256" "$tmp_dir/archive.sha256" 2>/dev/null; then
  expected_sha="$(expected_sha256 "$tmp_dir/archive.sha256" "$archive_name")"
fi
if [ -z "$expected_sha" ]; then
  info "no $archive_name.sha256; falling back to SHA256SUMS.txt"
  fetch "$base_url/SHA256SUMS.txt" "$tmp_dir/SHA256SUMS.txt" ||
    die "could not download a checksum for $archive_name; refusing to install an unverified archive"
  expected_sha="$(expected_sha256 "$tmp_dir/SHA256SUMS.txt" "$archive_name")"
fi
[ -n "$expected_sha" ] || die "no checksum for $archive_name in the published checksum file"
actual_sha="$(printf '%s' "$actual_sha" | tr 'A-F' 'a-f')"
expected_sha="$(printf '%s' "$expected_sha" | tr 'A-F' 'a-f')"
if [ "$actual_sha" != "$expected_sha" ]; then
  die "checksum mismatch for $archive_name
  expected $expected_sha
  got      $actual_sha"
fi
info "sha256 $actual_sha"

if [ -n "${BRW_SKIP_ATTESTATION:-}" ]; then
  step "Skipping the build provenance check (BRW_SKIP_ATTESTATION is set)"
elif command -v gh >/dev/null 2>&1; then
  step "Verifying build provenance with gh attestation verify"
  attest_status=0
  gh attestation verify "$tmp_dir/$archive_name" \
    --repo "$REPO" \
    --signer-workflow "$REPO/.github/workflows/release.yml" \
    --format json > "$tmp_dir/attestation.json" 2> "$tmp_dir/attestation.err" ||
    attest_status=$?
  if [ "$attest_status" -eq 0 ]; then
    signer="$(json_field "$tmp_dir/attestation.json" buildSignerURI)"
    workflow_ref="$(json_field "$tmp_dir/attestation.json" githubWorkflowRef)"
    issuer="$(json_field "$tmp_dir/attestation.json" issuer)"
    info "signed by    ${signer:-unknown}"
    info "workflow ref ${workflow_ref:-unknown}"
    info "issuer       ${issuer:-unknown}"
  elif [ "$attest_status" -eq 4 ]; then
    # gh reserves exit 4 for "not authenticated", which is an inability to check
    # rather than a failed check, so the install continues with a warning.
    warn "gh is not authenticated, so build provenance was not verified. Run 'gh auth login', or verify by hand:"
    warn "  gh attestation verify $archive_name --repo $REPO"
  else
    cat "$tmp_dir/attestation.err" >&2
    die "build provenance verification failed for $archive_name; refusing to install"
  fi
else
  step "Skipping the build provenance check (gh is not installed)"
  info "brw attests every release artifact. To check this download yourself:"
  info "  gh attestation verify $archive_name --repo $REPO"
fi

step "Unpacking into $install_dir"
mkdir -p "$tmp_dir/unpack"
tar -xzf "$tmp_dir/$archive_name" -C "$tmp_dir/unpack"
unpacked="$tmp_dir/unpack/brw_${version}_${os}_${arch}"
[ -d "$unpacked" ] || die "$archive_name did not contain the expected brw_${version}_${os}_${arch} directory"

mkdir -p "$install_dir"
# The bridge endpoint and token live inside the installed extension directory.
# They are per-install state rather than payload, so replacing the tree without
# carrying them over would silently revoke a working bridge.
if [ -f "$install_dir/extension/bridge-defaults.json" ]; then
  cp "$install_dir/extension/bridge-defaults.json" "$tmp_dir/bridge-defaults.json"
fi
for item in $PAYLOAD; do
  [ -e "$unpacked/$item" ] || continue
  if [ -e "$install_dir/$item" ] || [ -L "$install_dir/$item" ]; then
    rm -rf -- "${install_dir:?}/${item:?}"
  fi
  cp -R "$unpacked/$item" "$install_dir/$item"
done
if [ -f "$tmp_dir/bridge-defaults.json" ]; then
  cp "$tmp_dir/bridge-defaults.json" "$install_dir/extension/bridge-defaults.json"
fi

for cmd in $COMMANDS; do
  chmod 0755 "$install_dir/bin/$cmd"
done

if [ "$os" = "darwin" ]; then
  if command -v xattr >/dev/null 2>&1; then
    xattr -cr "$install_dir/bin" 2>/dev/null || true
  fi
  if command -v codesign >/dev/null 2>&1; then
    for cmd in $COMMANDS; do
      # An unpacked Go binary whose signature did not survive the round trip is
      # SIGKILLed on Apple Silicon ("Killed: 9"), and an ad-hoc re-sign fixes
      # that. Only re-sign what actually fails verification: forcing it over a
      # Developer ID signature would throw away the release's provenance to
      # solve a problem that signature does not have.
      if codesign --verify --strict "$install_dir/bin/$cmd" >/dev/null 2>&1; then
        continue
      fi
      codesign --force --sign - "$install_dir/bin/$cmd" >/dev/null 2>&1 || true
    done
  fi
fi

brwctl_probe="$("$install_dir/bin/brwctl" 2>&1 || true)"
case "$brwctl_probe" in
  *brwctl*) ;;
  *) die "the installed brwctl did not run. Check $install_dir/bin/brwctl" ;;
esac

if [ "$bin_dir" = "$install_dir/bin" ]; then
  step "Commands are in $bin_dir"
else
  step "Linking the commands into $bin_dir"
  mkdir -p "$bin_dir"
  for cmd in $COMMANDS; do
    ln -sf "$install_dir/bin/$cmd" "$bin_dir/$cmd"
  done
fi
for cmd in $COMMANDS; do
  info "$bin_dir/$cmd"
done

on_path="no"
case ":$PATH:" in
  *":$bin_dir:"*) on_path="yes" ;;
esac
if [ "$on_path" = "no" ]; then
  printf '\n'
  # An installer that leaves "command not found" behind has not installed
  # anything as far as the person running it is concerned.
  rc_file=""
  case "${SHELL:-}" in
    */zsh)  rc_file="$HOME/.zshrc" ;;
    */bash) if [ "$os" = "darwin" ]; then rc_file="$HOME/.bash_profile"; else rc_file="$HOME/.bashrc"; fi ;;
    */fish) rc_file="$HOME/.config/fish/config.fish" ;;
  esac
  path_line="export PATH=\"$bin_dir:\$PATH\""
  if [ "${SHELL:-}" != "${SHELL%fish}" ]; then
    path_line="fish_add_path $bin_dir"
  fi
  # Only the default location is written into a startup file. A relocated
  # install is someone testing or packaging, and editing their shell config
  # behind their back is not what they asked for.
  if [ -n "${BRW_NO_PATH:-}" ] || [ -z "$rc_file" ] || [ "$bin_dir" != "$HOME/.local/bin" ]; then
    warn "$bin_dir is not on your PATH. Add it:"
    warn "  $path_line"
  elif [ -f "$rc_file" ] && grep -qF "$bin_dir" "$rc_file" 2>/dev/null; then
    warn "$bin_dir is in $rc_file but not in this shell. Open a new terminal, or:"
    warn "  $path_line"
  else
    mkdir -p "$(dirname "$rc_file")"
    printf '\n# Added by the brw installer\n%s\n' "$path_line" >> "$rc_file"
    step "Added $bin_dir to your PATH in $rc_file"
    info "This shell does not have it yet. Open a new terminal, or run:"
    info "  $path_line"
    info "BRW_NO_PATH=1 skips this next time."
  fi
fi

printf '\n'
if [ -n "${BRW_NO_SETUP:-}" ]; then
  step "Installed. BRW_NO_SETUP is set, so setup was not run."
  info "Next: $bin_dir/brwctl setup"
elif "$install_dir/bin/brwctl" setup --help >/dev/null 2>&1; then
  step "Running brwctl setup"
  # Detach stdin: when this script is itself being read from a pipe, a child
  # reading stdin would consume the rest of the script. That also means setup
  # cannot ask for confirmation, so answer for it — running this installer is
  # the consent, and BRW_NO_SETUP stops before this point.
  if ! "$install_dir/bin/brwctl" setup --yes < /dev/null; then
    printf '\n'
    warn "brw is installed in $install_dir but 'brwctl setup' did not finish."
    warn "Re-run it once the problem above is fixed:  $bin_dir/brwctl setup"
    exit 1
  fi
else
  step "Installed brw $version."
  info "This release's brwctl has no 'setup' command. Next steps are in"
  info "https://github.com/$REPO/blob/main/docs/install.md"
fi
