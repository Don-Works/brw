#!/bin/sh
# Fail the build if a test run left a browser behind.
#
# A test binary that is killed cannot clean up after itself: SIGKILL skips
# t.Cleanup, and a Go `-timeout` panic calls os.Exit the same way. When that
# happens its throwaway browser survives as a child of init, still holding a
# profile directory in the OS temp dir, and every later run competes with it for
# CPU until the machine is full of them. This check turns that from an invisible
# mess into a failed build, and clears what it finds.
#
# "Orphan" is detected precisely rather than guessed. A browser counts only when
# BOTH hold:
#
#   1. its parent is init (the test that launched it has exited, and nothing else
#      would have started it), and
#   2. its profile is in the OS temp dir (a throwaway some run created).
#
# A browser someone is actually using has a live parent and a real profile, so it
# is never matched. Neither is a brw daemon's browser.
set -eu

tmp_root=$(printf '%s' "${TMPDIR:-/tmp}" | sed 's:/*$::')

# pid + profile dir for every automation browser parented to init. Matching on
# --headless / --remote-debugging-port rather than on a vendor path keeps this
# working for Chrome, Chromium, Edge, Brave and the rest.
orphans=$(ps -eo pid=,ppid=,args= | awk '
  $2 != 1 { next }
  {
    args = $0
    sub(/^[ \t]*[0-9]+[ \t]+[0-9]+[ \t]+/, "", args)
  }
  args !~ /--user-data-dir=/ { next }
  args !~ /--headless/ && args !~ /--remote-debugging-port/ { next }
  match(args, /--user-data-dir=[^ \t]+/) {
    print $1, substr(args, RSTART + 16, RLENGTH - 16)
  }')

leaked=0
# A here-doc rather than a pipe: a pipeline would run this loop in a subshell and
# the count would be lost with it.
while read -r pid dir; do
  [ -n "${pid:-}" ] || continue
  case "$dir" in
    "$tmp_root"/* | /tmp/* | /private/tmp/* | /var/folders/* | /private/var/folders/*) ;;
    *) continue ;;
  esac
  leaked=$((leaked + 1))
  echo "orphaned test browser: pid $pid, profile $dir" >&2
  kill -9 "$pid" 2>/dev/null || true
done <<EOF
$orphans
EOF

if [ "$leaked" -gt 0 ]; then
  echo "a test run left $leaked browser(s) behind; they have been killed, but whatever leaked them needs fixing" >&2
  exit 1
fi

echo "no orphaned test browsers"
