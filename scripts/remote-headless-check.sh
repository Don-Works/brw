#!/usr/bin/env bash
# Run the agent-browser local conformance suite against a HEADLESS Chrome on this
# (the browser) machine, fully ISOLATED from any persistent daemon, so it never
# raises a window, steals foreground focus, or touches the user's real Chrome.
#
# Safety: refuses to run if its HTTP port is already in use (so it can never
# accidentally drive an existing --bridge daemon / the real visible browser).
set -u

APPDIR="$HOME/Library/Application Support/agent-browser"
# AB_BIN overrides the binary dir (e.g. a freshly-built test build in /tmp) while
# still using the installed tests/fixtures under APPDIR.
BIN="${AB_BIN:-$APPDIR/bin}"
# Dedicated, unusual ports — NOT the conventional 17310/17311 bridge ports.
PORT="${AB_PORT:-17358}"
DBG_PORT="${AB_DBG_PORT:-17368}"
PROFILE_DIR="$(mktemp -d /tmp/ab-headless-XXXXXX)"
OUT="/tmp/ab-browserd.out"
ERR="/tmp/ab-browserd.err"

echo "== agent-browser HEADLESS, ISOLATED conformance run =="
echo "appdir:   $APPDIR"
echo "profile:  $PROFILE_DIR (throwaway)"
echo "http:     127.0.0.1:$PORT   cdp: 127.0.0.1:$DBG_PORT"
echo

[ -x "$BIN/agent-browserd" ] || { echo "FATAL: agent-browserd not at $BIN" >&2; exit 2; }
[ -x "$BIN/browsercheck" ]   || { echo "FATAL: browsercheck not at $BIN" >&2; exit 2; }
[ -f "$APPDIR/tests/scenarios/core.json" ] || { echo "FATAL: core.json not under $APPDIR" >&2; exit 2; }

# HARD SAFETY: never hijack an existing listener (e.g. the persistent bridge daemon
# on 17310 wired to the real visible Chrome). If our port is taken, abort.
if lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
  echo "FATAL: port $PORT already in use — refusing to run so we never drive an existing daemon/visible Chrome." >&2
  lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >&2
  exit 4
fi

cleanup() {
  echo
  echo "== teardown =="
  if [ -n "${BROWSERD_PID:-}" ]; then kill "$BROWSERD_PID" 2>/dev/null || true; fi
  pkill -f "$PROFILE_DIR" 2>/dev/null || true
  sleep 1
  rm -rf "$PROFILE_DIR" 2>/dev/null || true
  echo "teardown done"
}
trap cleanup EXIT

# Launch OUR browserd: direct-CDP, HEADLESS chrome, throwaway profile, dedicated port.
"$BIN/agent-browserd" \
  --http "127.0.0.1:$PORT" \
  --user-data-dir "$PROFILE_DIR" \
  --remote-debugging-port "$DBG_PORT" \
  --chrome-arg=--headless=new \
  --chrome-arg=--no-first-run \
  --chrome-arg=--disable-background-networking \
  1>"$OUT" 2>"$ERR" &
BROWSERD_PID=$!
echo "browserd pid: $BROWSERD_PID"

# Wait for OUR HTTP health, and confirm OUR pid owns the listener.
READY=0
for i in $(seq 1 40); do
  if ! kill -0 "$BROWSERD_PID" 2>/dev/null; then echo "FATAL: browserd exited early" >&2; echo "--- stderr ---"; cat "$ERR" >&2; exit 3; fi
  if curl -fsS "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; then READY=1; break; fi
  sleep 0.5
done
[ "$READY" = "1" ] || { echo "FATAL: health never ready" >&2; cat "$ERR" >&2; exit 3; }

OWNER_PID="$(lsof -nP -iTCP:"$PORT" -sTCP:LISTEN -t 2>/dev/null | head -n1)"
if [ "$OWNER_PID" != "$BROWSERD_PID" ]; then
  echo "FATAL: port $PORT served by pid $OWNER_PID, not our browserd $BROWSERD_PID — aborting to avoid driving the wrong browser." >&2
  exit 5
fi
echo "browserd: HEALTHY and OWNED by us (pid $BROWSERD_PID)"

echo
echo "== focus-safety evidence: our chrome child is headless (windowless) =="
CHROME_LINE="$(pgrep -fl "$PROFILE_DIR" | grep -F -- "--headless" | head -n1)"
if [ -n "$CHROME_LINE" ]; then
  echo "HEADLESS chrome confirmed: $(echo "$CHROME_LINE" | cut -c1-150)"
else
  echo "WARN: could not confirm headless chrome child (continuing; daemon owns the port)"
fi
echo "NOTE: persistent bridge daemon (if any) is left untouched:"
pgrep -fl "agent-browserd --bridge" || echo "  (none running)"

echo
echo "== running browsercheck (local fixtures; network/auth/manual auto-skipped) =="
NETFLAG=""
if [ -n "${AB_NET:-}" ]; then NETFLAG="--include-network"; echo "(including --include-network public scenarios)"; fi
set +e
"$BIN/browsercheck" --base-url "http://127.0.0.1:$PORT" --repo-root "$APPDIR" --suite "tests/scenarios/core.json" $NETFLAG
RC=$?
set +e

echo
echo "== browserd stderr tail =="
tail -n 8 "$ERR" 2>/dev/null
echo
echo "browsercheck exit code: $RC"
exit $RC
