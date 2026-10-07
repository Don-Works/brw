#!/bin/sh
set -eu

tmp_root=$(printf '%s' "${TMPDIR:-/tmp}" | sed 's:/*$::')

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
