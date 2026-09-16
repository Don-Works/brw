#!/bin/sh
# Split the Go suite across CI runners.
#
# `task test` deliberately runs `go test -p=1 ./...`: the browser, snapshot and
# artifact tests each launch a real headless Chrome, and running many Chrome roots
# on one runner turns the Manager's 20s operation timeout into a load race. So
# this script keeps `-p=1` inside every shard and the parallelism comes from
# running shards on separate runners instead.
#
# usage:
#   test-shard.sh rest [index count]        # every package no other shard claims
#   test-shard.sh package <index> <count> <package>...
#
# `package` slices by test NAME, so one package can span several runners. The
# slice is a modulo partition of `go test -list`, which means every test runs in
# exactly one shard: a test added later lands in a shard by arithmetic rather
# than by someone remembering to file it, and a shard that selects nothing is a
# hard error rather than a silently green run.
#
# There is no `-shard` flag in `go test`; this is that flag.
set -eu

# The packages the other shards claim. `rest` excludes exactly these, so a new
# package is picked up by the rest shard without editing this list.
slow_groups='./internal/browser ./internal/snapshot ./internal/extensionbridge'

mode=${1:-}
shard_failed=0
# The check runs on every exit path below, including a failing go test: a suite
# that fails AND leaks is the case most likely to leave browsers behind, since a
# killed test binary is what strands them.
repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
case "$mode" in
  rest)
    # Resolve through `go list` rather than matching the paths above directly:
    # `go list ./...` prints import paths (github.com/Don-Works/brw/internal/...),
    # so a relative exclusion pattern would match nothing and this shard would
    # silently re-run the whole suite on top of the shards that already own it.
    # A group that does not exist fails here, because `go list` does.
    all=$(go list ./...)
    claimed=$(go list $slow_groups)
    # Flatten before handing it to awk: BSD awk rejects a -v assignment that
    # contains a newline. Import paths never contain spaces, so splitting on one
    # is safe.
    claimed_flat=$(printf '%s\n' "$claimed" | tr '\n' ' ')
    rest=$(printf '%s\n' "$all" | awk -v known="$claimed_flat" '
      BEGIN { n = split(known, a, " "); for (i = 1; i <= n; i++) skip[a[i]] = 1 }
      !skip[$0] { print }')
    if [ -z "$rest" ]; then
      echo "rest shard selected no packages: every package is claimed by another shard" >&2
      exit 1
    fi
    # The shards must partition the module: a package in two shards wastes a
    # runner, and a package in none never runs at all.
    total=$(printf '%s\n' "$all" | wc -l | tr -d ' ')
    claimed_count=$(printf '%s\n' "$claimed" | wc -l | tr -d ' ')
    rest_count=$(printf '%s\n' "$rest" | wc -l | tr -d ' ')
    if [ "$((claimed_count + rest_count))" -ne "$total" ]; then
      echo "shard partition is wrong: $claimed_count claimed + $rest_count rest != $total packages; run go test ./... instead of trusting this split" >&2
      exit 1
    fi
    echo "== rest shard: $rest_count of $total packages (claimed elsewhere: $slow_groups)"
    index=${2:-1}
    count=${3:-1}
    if [ "$count" -gt 1 ]; then
      # Sliced by package rather than by test name: these are all small, and
      # slicing whole packages keeps each one's tests in a single process.
      rest=$(printf '%s\n' "$rest" | awk -v i="$index" -v n="$count" '(NR - 1) % n == (i - 1)')
      if [ -z "$rest" ]; then
        echo "rest shard $index/$count selected no packages" >&2
        exit 1
      fi
      echo "== rest shard $index/$count: $(printf '%s\n' "$rest" | wc -l | tr -d ' ') packages"
    fi
    # shellcheck disable=SC2086 # the package list is deliberately word-split
    go test -p=1 $rest || shard_failed=1
    ;;
  package)
    index=${2:-}
    count=${3:-}
    if [ -z "$index" ] || [ -z "$count" ] || [ "$#" -lt 4 ]; then
      echo "usage: test-shard.sh package <index> <count> <package>..." >&2
      exit 2
    fi
    shift 3
    for pkg in "$@"; do
      names=$(go test -list '^Test' "$pkg" | grep -E '^Test' || true)
      if [ -z "$names" ]; then
        echo "$pkg lists no tests: refusing to pass a shard that would run nothing" >&2
        exit 1
      fi
      total=$(printf '%s\n' "$names" | wc -l | tr -d ' ')
      chosen=$(printf '%s\n' "$names" | awk -v i="$index" -v n="$count" '(NR - 1) % n == (i - 1)')
      picked=$(printf '%s\n' "$chosen" | wc -l | tr -d ' ')
      if [ -z "$chosen" ]; then
        echo "$pkg shard $index/$count selected no tests out of $total" >&2
        exit 1
      fi
      pattern=$(printf '%s\n' "$chosen" | awk 'BEGIN { ORS = "" } { printf "%s%s", (NR > 1 ? "|" : ""), $0 }')
      echo "== $pkg shard $index/$count: $picked of $total tests"
      go test -p=1 -run "^($pattern)$" "$pkg" || shard_failed=1
    done
    ;;
  *)
    echo "usage: test-shard.sh rest [index count] | test-shard.sh package <index> <count> <package>..." >&2
    exit 2
    ;;
esac

# A killed test binary cannot reap its own browser, so this is the only place
# that notices one was left holding a temp profile.
"$repo_root/scripts/check-no-orphan-browsers.sh" || shard_failed=1
exit "$shard_failed"
