#!/usr/bin/env bash
# Install freshly-built agent-browser binaries + extension on THIS (the browser)
# machine and restart the persistent --bridge daemon so the focus-safe/fast build
# goes live. Expects payload staged in /tmp/ab-release (binaries + extension/).
set -u

APPDIR="$HOME/Library/Application Support/agent-browser"
BIN="$APPDIR/bin"
SRC="/tmp/ab-release"
STAMP="$(date +%Y%m%d-%H%M%S)"
LOG="$APPDIR/bridge.log"

echo "== deploy agent-browser ($STAMP) =="
mkdir -p "$BIN" "$APPDIR/extension" "$APPDIR/tests"

# 1. Install binaries atomically, keeping one backup of each.
for f in agent-browserd browsercheck agent-browserctl agent-browser-devtools-mcp; do
  if [ -f "$SRC/$f" ]; then
    [ -f "$BIN/$f" ] && cp "$BIN/$f" "$BIN/$f.bak-$STAMP"
    cp "$SRC/$f" "$BIN/.$f.new" && chmod +x "$BIN/.$f.new" && mv -f "$BIN/.$f.new" "$BIN/$f"
    echo "installed $f"
  fi
done

# 2. Update the extension files in place (user must reload it in Chrome).
if [ -d "$SRC/extension" ]; then
  cp -R "$SRC/extension/." "$APPDIR/extension/"
  echo "updated extension at $APPDIR/extension (RELOAD it in chrome://extensions to take effect)"
fi

# 2b. Update browsercheck scenarios and fixtures.
if [ -d "$SRC/tests" ]; then
  cp -R "$SRC/tests/." "$APPDIR/tests/"
  echo "updated tests at $APPDIR/tests"
fi

# 3. Restart the persistent bridge daemon.
OLD="$(pgrep -f 'agent-browserd --bridge --http 127.0.0.1:17310' || true)"
if [ -n "$OLD" ]; then
  echo "stopping old bridge daemon: $OLD"
  kill $OLD 2>/dev/null || true
  for i in $(seq 1 10); do
    if ! pgrep -f 'agent-browserd --bridge --http 127.0.0.1:17310' >/dev/null 2>&1; then break; fi
    sleep 0.5
  done
  pkill -9 -f 'agent-browserd --bridge --http 127.0.0.1:17310' 2>/dev/null || true
fi
# wait for the port to free
for i in $(seq 1 10); do
  if ! lsof -nP -iTCP:17310 -sTCP:LISTEN >/dev/null 2>&1; then break; fi
  sleep 0.5
done

echo "starting new bridge daemon (same args, no profile env — matches prior launch)"
nohup "$BIN/agent-browserd" --bridge --http 127.0.0.1:17310 --bridge-addr 127.0.0.1:17311 >>"$LOG" 2>&1 &
disown || true

# 4. Health check.
NEWPID=""
for i in $(seq 1 20); do
  if curl -fsS http://127.0.0.1:17310/health >/dev/null 2>&1; then break; fi
  sleep 0.5
done
NEWPID="$(pgrep -f 'agent-browserd --bridge --http 127.0.0.1:17310' || true)"
if curl -fsS http://127.0.0.1:17310/health >/dev/null 2>&1; then
  echo "OK: new bridge daemon pid=$NEWPID, /health responding"
  echo "installed version:"; "$BIN/agent-browserd" --help 2>&1 | head -n1 || true
else
  echo "WARN: bridge daemon /health not responding — check $LOG" >&2
  tail -n 12 "$LOG" 2>/dev/null >&2
  exit 1
fi
echo "== deploy done =="
