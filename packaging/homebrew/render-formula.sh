#!/usr/bin/env bash
set -euo pipefail

# Prints packaging/homebrew/brw.rb with the version and the four release sha256
# sums filled in, so bumping the tap after a release is mechanical.
#
# Sums come from dist/release when the archives were just built locally, and
# otherwise from the published release's SHA256SUMS.txt.

usage() {
  echo "usage: packaging/homebrew/render-formula.sh <version> [sums-dir]" >&2
}

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
  usage
  exit 2
fi

version="${1#v}"
sums_dir="${2:-dist/release}"

if ! [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-].*)?$ ]]; then
  echo "version must start with x.y.z: $version" >&2
  exit 2
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
template="$repo_root/packaging/homebrew/brw.rb"
case "$sums_dir" in
  /*) sums_abs="$sums_dir" ;;
  *) sums_abs="$repo_root/$sums_dir" ;;
esac

work_dir="$(mktemp -d "${TMPDIR:-/tmp}/brw-formula.XXXXXX")"
trap 'rm -rf "$work_dir"' EXIT

sums_file=""
targets=(darwin_arm64 darwin_amd64 linux_arm64 linux_amd64)

local_sums_complete=1
for target in "${targets[@]}"; do
  if [ ! -f "$sums_abs/brw_${version}_${target}.tar.gz.sha256" ]; then
    local_sums_complete=0
    break
  fi
done

if [ "$local_sums_complete" -eq 1 ]; then
  cat "$sums_abs"/brw_"${version}"_*.tar.gz.sha256 > "$work_dir/sums.txt"
  sums_file="$work_dir/sums.txt"
  echo "# sums from $sums_abs" >&2
else
  echo "# sums from the published v$version release" >&2
  curl -fsSL --proto '=https' --tlsv1.2 \
    -o "$work_dir/sums.txt" \
    "https://github.com/Don-Works/brw/releases/download/v$version/SHA256SUMS.txt"
  sums_file="$work_dir/sums.txt"
fi

sum_for() {
  local want="brw_${version}_$1.tar.gz"
  tr -d '*' < "$sums_file" | awk -v want="$want" '
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

# Assigned one per line, not inline in the perl invocation's environment: a
# missing sum inside an env-prefix substitution would leave an empty sha256 in
# an otherwise valid-looking formula.
SHA256_DARWIN_ARM64="$(sum_for darwin_arm64)"
SHA256_DARWIN_AMD64="$(sum_for darwin_amd64)"
SHA256_LINUX_ARM64="$(sum_for linux_arm64)"
SHA256_LINUX_AMD64="$(sum_for linux_amd64)"
for pair in \
  "darwin_arm64:$SHA256_DARWIN_ARM64" \
  "darwin_amd64:$SHA256_DARWIN_AMD64" \
  "linux_arm64:$SHA256_LINUX_ARM64" \
  "linux_amd64:$SHA256_LINUX_AMD64"; do
  if [ -z "${pair#*:}" ]; then
    echo "no sha256 for brw_${version}_${pair%%:*}.tar.gz in $sums_file" >&2
    exit 1
  fi
done

export SHA256_DARWIN_ARM64 SHA256_DARWIN_AMD64 SHA256_LINUX_ARM64 SHA256_LINUX_AMD64
BRW_VERSION="$version" \
  perl -pe '
    s/\@BRW_VERSION\@/$ENV{BRW_VERSION}/g;
    s/\@SHA256_DARWIN_ARM64\@/$ENV{SHA256_DARWIN_ARM64}/g;
    s/\@SHA256_DARWIN_AMD64\@/$ENV{SHA256_DARWIN_AMD64}/g;
    s/\@SHA256_LINUX_ARM64\@/$ENV{SHA256_LINUX_ARM64}/g;
    s/\@SHA256_LINUX_AMD64\@/$ENV{SHA256_LINUX_AMD64}/g;
  ' "$template" |
  awk 'body { print; next } /^#/ || /^[[:space:]]*$/ { next } { body = 1; print }'
