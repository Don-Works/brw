#!/usr/bin/env bash
set -euo pipefail

host="${1:-max-air}"
profile="${AGENT_BROWSER_PROFILE:-agent-revitt}"
remote_dir="${AGENT_BROWSER_REMOTE_DIR:-~/agent-browser}"
http_addr="${AGENT_BROWSER_HTTP_ADDR:-127.0.0.1:17310}"

cd "$(dirname "$0")/.."

GOOS=darwin GOARCH=arm64 go build -o bin/agent-browserd-darwin-arm64 ./cmd/browserd

ssh "$host" "mkdir -p $remote_dir"
rsync -az --delete \
  bin/agent-browserd-darwin-arm64 \
  ../.mcplexer \
  "$host:$remote_dir/"

cat <<EOF
Starting agent-browserd on $host.

The browser window will be visible on $host. Keep this terminal open.
HTTP API will bind on the remote machine at $http_addr.

For a local tunnel, run in another terminal:
  ssh -L 17310:127.0.0.1:17310 $host

EOF

ssh -t "$host" "cd $remote_dir && chmod +x ./agent-browserd-darwin-arm64 && ./agent-browserd-darwin-arm64 --profile '$profile' --http '$http_addr'"
