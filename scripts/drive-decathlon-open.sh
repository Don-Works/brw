#!/usr/bin/env bash
# Drive the REAL Chrome via the bridge daemon (127.0.0.1:17310) on this machine:
# open decathlon.co.uk, focus it for human assist, wait, and report Cloudflare status.
set -u
BASE="http://127.0.0.1:17310"

echo "== bridge tabs (confirms extension connected) =="
curl -fsS "$BASE/api/browser/tabs" 2>/dev/null | head -c 400
echo
echo "== open decathlon =="
OPEN="$(curl -fsS -H 'content-type: application/json' -d '{"url":"https://www.decathlon.co.uk/"}' "$BASE/api/browser/open" 2>/dev/null)"
echo "$OPEN" | head -c 300
TABID="$(printf '%s' "$OPEN" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p' | head -n1)"
echo
echo "tabid: $TABID"
if [ -n "$TABID" ]; then
  curl -fsS -H 'content-type: application/json' -d "{\"id\":\"$TABID\"}" "$BASE/api/browser/focus" >/dev/null 2>&1
fi
curl -fsS -H 'content-type: application/json' -d '{"condition":"ready","timeout_ms":20000}' "$BASE/api/page/wait_for" >/dev/null 2>&1
sleep 7
echo "== read =="
READ="$(curl -fsS "$BASE/api/page/read" 2>/dev/null)"
printf '%s' "$READ" | head -c 600
echo
TITLE="$(printf '%s' "$READ" | sed -n 's/.*"title":"\([^"]*\)".*/\1/p' | head -n1)"
echo
case "$TITLE" in
  *"Just a moment"*|*"moment"*|*"Verifying"*|*"Attention"*)
    echo "STATUS: CLOUDFLARE_CHALLENGE — needs you to clear it in the focused tab" ;;
  *)
    echo "STATUS: THROUGH — title: $TITLE" ;;
esac
