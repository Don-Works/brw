#!/usr/bin/env bash
#
# install.sh — build, package, and install agent-browser (daemon + bridge
# extension) to a target. Mirrors the `make install-mac` layout:
#   ~/Library/Application Support/agent-browser/{bin,extension,tests,config}
#
# Usage:
#   scripts/install.sh local                 # this machine
#   scripts/install.sh maxrevitt@max-air     # an ssh target (Tailscale/host alias)
#
# What it does: builds darwin/arm64 binaries, backs up the live binary +
# extension on the target, copies the new binary + the (versioned) bridge
# extension into place, and stops the bridge daemon so it respawns on the new
# build. The ONE thing it cannot do is reload the unpacked Chrome extension
# (Chrome has no headless reload and the daemon is bridge-mode, so it can't
# trigger one) — it prints that final manual step.
#
set -euo pipefail

TARGET="${1:-}"
if [ -z "$TARGET" ]; then
  echo "usage: scripts/install.sh <local|user@host>" >&2
  exit 2
fi

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"
APP_REL="Library/Application Support/agent-browser"
EXT_VER="$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' extension/manifest.json | head -1)"

echo "==> building darwin/arm64 binaries (extension v$EXT_VER)"
GOOS=darwin GOARCH=arm64 go build -o bin/agent-browserd-darwin-arm64 ./cmd/browserd
GOOS=darwin GOARCH=arm64 go build -o bin/agent-browserctl-darwin-arm64 ./cmd/agent-browserctl
GOOS=darwin GOARCH=arm64 go build -o bin/browsercheck-darwin-arm64 ./cmd/browsercheck

print_manual_step() {
  cat <<EOF

============================================================
 agent-browser installed to: $1
 bridge extension staged at:  v$EXT_VER
 backups: bin/agent-browserd.bak  +  extension.bak

 FINAL MANUAL STEP on that machine's Chrome (required — unpacked
 extensions do not hot-reload and the daemon cannot trigger it):
   open chrome://extensions  ->  Reload the agent-browser bridge extension
   (or restart that Chrome)

 Then verify the bridge:  curl -s 127.0.0.1:17310/api/browser/tabs
 If the daemon isn't auto-managed, relaunch it:
   agent-browserd --bridge --http 127.0.0.1:17310 --bridge-addr 127.0.0.1:17311
============================================================
EOF
}

if [ "$TARGET" = "local" ]; then
  APP="$HOME/$APP_REL"
  echo "==> installing locally to $APP"
  mkdir -p "$APP/bin" "$APP/extension"
  [ -f "$APP/bin/agent-browserd" ] && cp "$APP/bin/agent-browserd" "$APP/bin/agent-browserd.bak" || true
  cp bin/agent-browserd-darwin-arm64  "$APP/bin/agent-browserd"
  cp bin/agent-browserctl-darwin-arm64 "$APP/bin/agent-browserctl"
  rm -rf "$APP/extension.bak"; [ -d "$APP/extension" ] && mv "$APP/extension" "$APP/extension.bak" || true
  mkdir -p "$APP/extension"; cp -R extension/. "$APP/extension/"
  pkill -f "agent-browserd --bridge" 2>/dev/null || true
  print_manual_step "local ($APP)"
else
  echo "==> deploying to $TARGET"
  RHOME="$(ssh "$TARGET" 'echo $HOME')"
  APP="$RHOME/$APP_REL"
  STAGE="$RHOME/.agent-browser-stage"
  ssh "$TARGET" "mkdir -p '$STAGE' '$APP/bin' '$APP/extension'"
  scp bin/agent-browserd-darwin-arm64  "$TARGET:$STAGE/agent-browserd"
  scp bin/agent-browserctl-darwin-arm64 "$TARGET:$STAGE/agent-browserctl"
  scp -r extension "$TARGET:$STAGE/extension"
  ssh "$TARGET" "cp '$APP/bin/agent-browserd' '$APP/bin/agent-browserd.bak' 2>/dev/null || true"
  ssh "$TARGET" "cp '$STAGE/agent-browserd' '$APP/bin/agent-browserd' && cp '$STAGE/agent-browserctl' '$APP/bin/agent-browserctl'"
  ssh "$TARGET" "rm -rf '$APP/extension.bak'; mv '$APP/extension' '$APP/extension.bak' 2>/dev/null || true"
  ssh "$TARGET" "mkdir -p '$APP/extension' && cp -R '$STAGE/extension/.' '$APP/extension/'"
  ssh "$TARGET" "pkill -f 'agent-browserd --bridge' 2>/dev/null || true"
  print_manual_step "$TARGET ($APP)"
fi
