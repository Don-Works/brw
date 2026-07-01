#!/usr/bin/env bash
# Scaffold a dedicated Chromium clone + brw bridge daemon for one signed-in
# Chrome profile, so an agent can drive it reliably (Chromium is the recommended
# brw target; the real Chrome is fragile for automation). MCPlexer v0.4.0+
# auto-discovery then registers each clone as its own namespace.
#
# It automates the mechanical + verified-recipe parts and CLEARLY marks the two
# steps that need a human: the macOS Keychain approval (GUI) when reading Chrome's
# Safe Storage secret, and pointing the clone's extension at its bridge port.
#
#   setup-chromium-profile-clone.sh --profile-dir "Profile 2" --name max-personal [--http-port 17510] [--apply]
#
# Dry-run by default: prints the plan and writes nothing until --apply.
set -euo pipefail

PROFILE_DIR=""; NAME=""; HTTP_PORT=""; APPLY=0
CHROME_DIR="$HOME/Library/Application Support/Google/Chrome"
CHROMIUM_APP="/Applications/Chromium.app"
BRW_APPDIR="$HOME/Library/Application Support/brw"
POLICY="$HOME/.config/brw/browser-profiles.json"

while [[ $# -gt 0 ]]; do case "$1" in
  --profile-dir) PROFILE_DIR="$2"; shift 2;;
  --name) NAME="$2"; shift 2;;
  --http-port) HTTP_PORT="$2"; shift 2;;
  --chrome-dir) CHROME_DIR="$2"; shift 2;;
  --apply) APPLY=1; shift;;
  -h|--help) grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 0;;
  *) echo "unknown arg: $1" >&2; exit 2;;
esac; done

[[ -n "$PROFILE_DIR" && -n "$NAME" ]] || { echo "required: --profile-dir <Chrome profile dir> --name <slug>" >&2; exit 2; }
[[ -d "$CHROME_DIR/$PROFILE_DIR" ]] || { echo "Chrome profile not found: $CHROME_DIR/$PROFILE_DIR" >&2; exit 1; }

# Pick a free loopback port pair if not given: scan existing policy for the
# highest bridge_http_addr and step past it (each daemon needs http + http+1).
if [[ -z "$HTTP_PORT" ]]; then
  if [[ -f "$POLICY" ]]; then
    HIGH=$(grep -oE '127\.0\.0\.1:[0-9]+' "$POLICY" | grep -oE '[0-9]+$' | sort -n | tail -1)
    HTTP_PORT=$(( ${HIGH:-17408} + 2 )); (( HTTP_PORT % 2 == 0 )) && HTTP_PORT=$((HTTP_PORT+1))
  else HTTP_PORT=17510; fi
fi
WS_PORT=$((HTTP_PORT + 1))
CLONE_DIR="$HOME/Library/Application Support/brw-clone-$NAME"
LABEL="co.revitt.brw.$NAME"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
LAUNCHER="$HOME/.local/bin/brw-chromium-$NAME"

cat <<PLAN
brw Chromium-clone scaffold ($([[ $APPLY -eq 1 ]] && echo APPLY || echo DRY-RUN))
  source Chrome profile : $CHROME_DIR/$PROFILE_DIR
  clone name / slug     : $NAME
  clone user-data-dir   : $CLONE_DIR
  bridge http / ws      : 127.0.0.1:$HTTP_PORT / 127.0.0.1:$WS_PORT
  launchd label         : $LABEL
  launcher              : $LAUNCHER
  browser-profiles.json : add profile "$NAME" + workspace binding
PLAN
[[ $APPLY -eq 1 ]] || { echo; echo "(dry-run — re-run with --apply to create. You'll approve one Keychain prompt.)"; exit 0; }

# 1) Match Safe Storage keys so cloned cookies decrypt (Chrome stays READ-ONLY).
echo "==> Reading Chrome Safe Storage secret (approve the Keychain prompt)…"
SECRET="$(security find-generic-password -s 'Chrome Safe Storage' -w)"
security add-generic-password -U -A -s 'Chromium Safe Storage' -a Chromium -w "$SECRET" 2>/dev/null || true

# 2) Clone the profile into a fresh Chromium user-data-dir (skip caches).
echo "==> Cloning profile → $CLONE_DIR/Default"
mkdir -p "$CLONE_DIR/Default"
rsync -a --delete \
  --exclude 'Cache' --exclude 'Code Cache' --exclude 'GPUCache' --exclude 'Service Worker/CacheStorage' \
  --exclude 'Application Cache' --exclude 'DawnCache' --exclude 'GrShaderCache' \
  "$CHROME_DIR/$PROFILE_DIR/" "$CLONE_DIR/Default/"

# 3) Launcher (loads the brw extension so the clone bridges).
mkdir -p "$(dirname "$LAUNCHER")"
cat > "$LAUNCHER" <<L
#!/bin/zsh
set -euo pipefail
exec open -na "$CHROMIUM_APP" --args \\
  --user-data-dir="$CLONE_DIR" --profile-directory=Default \\
  --restore-last-session=false --no-first-run \\
  --load-extension="$BRW_APPDIR/extension" "\$@"
L
chmod +x "$LAUNCHER"

# 4) launchd bridge daemon on the assigned ports.
cat > "$PLIST" <<P
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key><array>
    <string>$BRW_APPDIR/bin/brwd</string>
    <string>--workspace</string><string>brw-$NAME</string>
    <string>--profile</string><string>$NAME</string>
    <string>--profile-policy</string><string>$POLICY</string>
    <string>--bridge</string>
    <string>--http</string><string>127.0.0.1:$HTTP_PORT</string>
    <string>--bridge-addr</string><string>127.0.0.1:$WS_PORT</string>
  </array>
  <key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>$BRW_APPDIR/bridge-$NAME.log</string>
  <key>StandardErrorPath</key><string>$BRW_APPDIR/bridge-$NAME.log</string>
</dict></plist>
P

# 5) Merge a profile entry + workspace binding into browser-profiles.json.
python3 - "$POLICY" "$NAME" "$CLONE_DIR" "127.0.0.1:$HTTP_PORT" "127.0.0.1:$WS_PORT" <<'PY'
import json,sys,os
path,name,udd,http,ws=sys.argv[1:6]
pol=json.load(open(path)) if os.path.exists(path) else {"workspace_bindings":[],"profiles":[]}
pol.setdefault("workspace_bindings",[]); pol.setdefault("profiles",[])
if not any(p.get("name")==name for p in pol["profiles"]):
    pol["profiles"].append({"name":name,"description":f"Chromium clone of {name} for brw.",
        "user_data_dir":udd,"profile_directory":"Default","direct_cdp_allowed":False,
        "extension_bridge_allowed":True,"bridge_extension_id":"amocjcgddnoakjijfggdpnefdnboilpe",
        "bridge_install_mode":"load_extension","bridge_http_addr":http,"bridge_ws_addr":ws})
if not any(b.get("workspace")==f"brw-{name}" for b in pol["workspace_bindings"]):
    pol["workspace_bindings"].append({"workspace":f"brw-{name}","default_profile":name,"allowed_profiles":[name]})
os.makedirs(os.path.dirname(path),exist_ok=True)
json.dump(pol,open(path,"w"),indent=2); print("updated",path)
PY

launchctl load -w "$PLIST" 2>/dev/null || launchctl bootstrap "gui/$(id -u)" "$PLIST" 2>/dev/null || true

cat <<DONE

==> Scaffolded clone "$NAME".
Two manual steps remain (GUI):
  1. Launch the clone:  $LAUNCHER
  2. In that Chromium window: chrome://extensions → brw → Options →
     set bridge URL to ws://127.0.0.1:$WS_PORT/extension (and status http://127.0.0.1:$HTTP_PORT/status), Save.
Then it self-registers in MCPlexer (v0.4.0 auto-discovery) as namespace brw_$NAME.
DONE
