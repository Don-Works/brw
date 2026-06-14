#!/usr/bin/env bash
# Headless exploration of decathlon.co.uk to design a basket scenario.
# Starts an ISOLATED headless browserd (no focus theft), drives the HTTP API,
# prints what the page looks like at each stage, then tears down.
set -u
APPDIR="$HOME/Library/Application Support/agent-browser"
BIN="${AB_BIN:-$APPDIR/bin}"
PORT="${AB_PORT:-17380}"
DBG="${AB_DBG:-17381}"
PROFILE="$(mktemp -d /tmp/ab-decathlon-XXXXXX)"
BASE="http://127.0.0.1:$PORT"

if lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then echo "FATAL: port $PORT busy" >&2; exit 4; fi
cleanup(){ [ -n "${PID:-}" ] && kill "$PID" 2>/dev/null; pkill -f "$PROFILE" 2>/dev/null; rm -rf "$PROFILE"; }
trap cleanup EXIT

"$BIN/agent-browserd" --http "127.0.0.1:$PORT" --user-data-dir "$PROFILE" --remote-debugging-port "$DBG" \
  --chrome-arg=--headless=new --chrome-arg=--no-first-run --chrome-arg=--window-size=1400,2200 \
  1>/tmp/ab-decathlon.out 2>/tmp/ab-decathlon.err &
PID=$!
for i in $(seq 1 40); do curl -fsS "$BASE/health" >/dev/null 2>&1 && break; kill -0 "$PID" 2>/dev/null || { echo "daemon died"; cat /tmp/ab-decathlon.err; exit 3; }; sleep 0.5; done
echo "daemon up (pid $PID)"

post(){ curl -fsS -H 'content-type: application/json' -d "$2" "$BASE$1"; }
get(){ curl -fsS "$BASE$1"; }

echo "== open homepage =="
post /api/browser/open '{"url":"https://www.decathlon.co.uk/"}' >/dev/null
post /api/page/wait_for '{"condition":"ready","timeout_ms":15000}' >/dev/null
sleep 2
echo "-- title/url + first headings/links --"
get /api/page/read | head -c 900
echo
echo "== snapshot: buttons/links matching consent/cookie/accept =="
get "/api/page/find?query=accept%20cookies&limit=8" | head -c 700
echo
echo "== snapshot: searchbox/combobox/textbox top of page =="
get "/api/page/find?role=searchbox&limit=5" | head -c 500
echo
get "/api/page/find?query=search&limit=8" | head -c 800
echo
echo "== done =="
