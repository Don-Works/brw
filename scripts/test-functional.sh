#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
run_root=$(mktemp -d "${TMPDIR:-/tmp}/brw-functional.XXXXXX")
port=${BRW_FUNCTIONAL_PORT:-17390}
fixture_port=${BRW_FIXTURE_PORT:-17391}
daemon_pid=
fixture_pid=

cleanup() {
  if [ -n "$daemon_pid" ]; then
    kill "$daemon_pid" 2>/dev/null || :
    wait "$daemon_pid" 2>/dev/null || :
  fi
  if [ -n "$fixture_pid" ]; then
    kill "$fixture_pid" 2>/dev/null || :
    wait "$fixture_pid" 2>/dev/null || :
  fi
  rm -rf "$run_root"
}
trap cleanup EXIT INT TERM

# Build a synthetic recipe at runtime in an owner-only directory outside git.
# No executable recipe corpus or site-specific operator data is checked in.
recipe_root="$run_root/private-recipe-store"
mkdir -m 0700 "$recipe_root"
cat >"$recipe_root/fixture.json" <<EOF
{"schema_version":1,"id":"example.fixture.submit-form","version":"1.0.0","name":"Submit synthetic fixture","description":"Fill and submit the local functional-test form.","intents":["submit local fixture form","test deterministic browser recipe"],"origins":["http://127.0.0.1:$fixture_port"],"risk":"external_write","inputs":{"field_name":{"required":true,"description":"Accessible name of the synthetic field"},"email":{"secret":true,"required":true,"description":"Synthetic test address"}},"steps":[{"id":"fill_email","action":"fill","target":{"role":"textbox","name":"\${input:field_name}"},"value":"\${input:email}","effect":"external_write","max_attempts":1,"idempotency_key":"fixture-fill:\${input:email}","postcondition":{"kind":"element.value","match":"\${input:email}","target":{"role":"textbox","name":"\${input:field_name}"},"timeout_ms":3000}},{"id":"submit","action":"click","target":{"role":"button","name":"Submit request"},"effect":"external_write","max_attempts":1,"idempotency_key":"fixture-submit:\${input:email}","postcondition":{"kind":"text.present","match":"Submitted \${input:email}","timeout_ms":3000}},{"id":"event_pause","action":"timer","timer_ms":5},{"id":"capture_result","action":"capture","capture":{"kind":"text","ttl_seconds":300}}]}
EOF
cat >"$recipe_root/download.json" <<EOF
{"schema_version":1,"id":"example.fixture.download-source","version":"1.0.0","name":"Download synthetic source invoice","description":"Download and retain the original synthetic fixture document.","intents":["download source invoice","get original fixture document"],"origins":["http://127.0.0.1:$fixture_port"],"risk":"read_only","steps":[{"id":"download","action":"click","target":{"role":"link","name":"Download source invoice"},"effect":"read","postcondition":{"kind":"download.completed","match":"source-invoice.txt","timeout_ms":5000}},{"id":"capture_source","action":"capture","capture":{"kind":"download","filename":"source-invoice.txt","ttl_seconds":300}}]}
EOF
cat >"$recipe_root/contenteditable.json" <<EOF
{"schema_version":1,"id":"example.fixture.contenteditable-guard","version":"1.0.0","name":"Guard a synthetic rich-text draft","description":"Prove structural empty and filled contenteditable values in the real-browser recipe runner.","intents":["test empty contenteditable recipe guard","test rich editor exact value"],"origins":["http://127.0.0.1:$fixture_port"],"risk":"external_write","inputs":{"note":{"required":true,"description":"Synthetic rich-text draft"}},"steps":[{"id":"guard_empty","action":"press","target":{"role":"textbox","name":"Project notes"},"key":"Enter","effect":"external_write","idempotency_key":"fixture-empty-editor","postcondition":{"kind":"element.value","match":"","target":{"role":"textbox","name":"Project notes"},"timeout_ms":1000}},{"id":"fill_editor","action":"fill","target":{"role":"textbox","name":"Project notes"},"value":"\${input:note}","effect":"external_write","idempotency_key":"fixture-rich-draft:\${input:note}","postcondition":{"kind":"element.value","match":"\${input:note}","target":{"role":"textbox","name":"Project notes"},"timeout_ms":1000}}]}
EOF
chmod 0600 "$recipe_root/fixture.json" "$recipe_root/download.json" "$recipe_root/contenteditable.json"

python3 -m http.server "$fixture_port" --bind 127.0.0.1 --directory "$repo_root/tests/fixtures" \
  >"$run_root/fixtures.log" 2>&1 &
fixture_pid=$!

fixture_ready=false
attempt=0
while [ "$attempt" -lt 50 ]; do
  if curl -fsS "http://127.0.0.1:$fixture_port/content.html" >/dev/null 2>&1; then
    fixture_ready=true
    break
  fi
  if ! kill -0 "$fixture_pid" 2>/dev/null; then
    break
  fi
  attempt=$((attempt + 1))
  sleep 0.1
done
if [ "$fixture_ready" != true ]; then
  echo "fixture server did not become ready" >&2
  sed -n '1,120p' "$run_root/fixtures.log" >&2
  exit 1
fi

"$repo_root/bin/brwd" \
  --http "127.0.0.1:$port" \
  --user-data-dir "$run_root/profile" \
  --artifact-dir "$run_root/artifacts" \
  --recipe-root "$recipe_root" \
  --usage-log off \
  --chrome-arg=--headless=new \
  --chrome-arg=--disable-gpu \
  --chrome-arg=--no-sandbox \
  >"$run_root/brwd.log" 2>&1 &
daemon_pid=$!

ready=false
attempt=0
while [ "$attempt" -lt 150 ]; do
  if curl -fsS "http://127.0.0.1:$port/health" >/dev/null 2>&1; then
    ready=true
    break
  fi
  if ! kill -0 "$daemon_pid" 2>/dev/null; then
    break
  fi
  attempt=$((attempt + 1))
  sleep 0.1
done

if [ "$ready" != true ]; then
  echo "brwd did not become ready" >&2
  sed -n '1,240p' "$run_root/brwd.log" >&2
  exit 1
fi

if ! "$repo_root/bin/brwcheck" --repo-root "$repo_root" --base-url "http://127.0.0.1:$port"; then
  sed -n '1,240p' "$run_root/brwd.log" >&2
  exit 1
fi

# Artifact smoke test on the same real browser. Capture calls must return only
# compact metadata; bytes are read later through an explicit bounded window.
fixture_url=$(python3 -c 'import pathlib,sys; print(pathlib.Path(sys.argv[1]).resolve().as_uri())' "$repo_root/tests/fixtures/content.html")
open_json=$(curl -fsS -H 'content-type: application/json' \
  --data "{\"url\":\"$fixture_url\"}" "http://127.0.0.1:$port/api/browser/open")
tab_id=$(printf '%s' "$open_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["tab"]["id"])')

capture_artifact() {
  kind=$1
  extra=${2:-}
  curl -fsS -H 'content-type: application/json' \
    --data "{\"kind\":\"$kind\",\"tab_id\":\"$tab_id\"$extra}" \
    "http://127.0.0.1:$port/api/artifacts/capture"
}

text_meta=$(capture_artifact text)
text_id=$(printf '%s' "$text_meta" | python3 -c 'import json,sys; value=json.load(sys.stdin); assert value["size_bytes"] > 0; print(value["artifact_id"])')
if [ "$(printf '%s' "$text_meta" | wc -c | tr -d ' ')" -gt 2048 ]; then
  echo "artifact capture returned payload instead of compact metadata" >&2
  exit 1
fi
text_chunk=$(curl -fsS "http://127.0.0.1:$port/api/artifacts/$text_id/read?offset=0&max_bytes=4096")
printf '%s' "$text_chunk" | python3 -c 'import json,sys; value=json.load(sys.stdin); assert value["encoding"] == "utf-8"; assert "Semantic Browser Coverage" in value["text"]'
curl -fsS "http://127.0.0.1:$port/api/artifacts/$text_id/search?query=structured%20content&limit=3" \
  | python3 -c 'import json,sys; assert len(json.load(sys.stdin)) >= 1'

screenshot_meta=$(capture_artifact screenshot)
printf '%s' "$screenshot_meta" | python3 -c 'import json,sys; value=json.load(sys.stdin); assert value["kind"] == "screenshot"; assert value["size_bytes"] > 100'
screenshot_id=$(printf '%s' "$screenshot_meta" | python3 -c 'import json,sys; print(json.load(sys.stdin)["artifact_id"])')

pdf_meta=$(capture_artifact pdf)
pdf_id=$(printf '%s' "$pdf_meta" | python3 -c 'import json,sys; value=json.load(sys.stdin); assert value["kind"] == "pdf"; print(value["artifact_id"])')
curl -fsS "http://127.0.0.1:$port/api/artifacts/$pdf_id/read?offset=0&max_bytes=8" \
  | python3 -c 'import base64,json,sys; value=json.load(sys.stdin); assert base64.b64decode(value["base64"]).startswith(b"%PDF")'

artifact_ids="$text_id $screenshot_id $pdf_id"
if command -v ffmpeg >/dev/null 2>&1; then
  video_meta=$(capture_artifact video ',"duration_ms":200,"fps":2')
  video_id=$(printf '%s' "$video_meta" | python3 -c 'import json,sys; value=json.load(sys.stdin); assert value["kind"] == "video"; assert value["size_bytes"] > 0; print(value["artifact_id"])')
  artifact_ids="$artifact_ids $video_id"
fi

for artifact_id in $artifact_ids; do
  curl -fsS -X DELETE "http://127.0.0.1:$port/api/artifacts/$artifact_id" >/dev/null
done
curl -fsS -H 'content-type: application/json' --data "{\"id\":\"$tab_id\"}" \
  "http://127.0.0.1:$port/api/browser/close" >/dev/null
echo "PASS browser-host artifacts          text/search/screenshot/PDF/video metadata boundary"

# Search returns metadata only. Execution then fetches exactly the pinned
# id/version/digest, expands the private input without echoing it, waits on a
# pre-armed postcondition, and returns only the captured artifact handle.
recipe_query=$(curl -fsS -H 'content-type: application/json' \
  --data '{"query":"submit the deterministic local form","origin":"http://127.0.0.1:'"$fixture_port"'","limit":3}' \
  "http://127.0.0.1:$port/api/recipes/search")
recipe_pin=$(printf '%s' "$recipe_query" | python3 -c 'import json,sys; value=json.load(sys.stdin); match=next(x for x in value if x["id"]=="example.fixture.submit-form"); print(match["id"], match["version"], match["digest"], sep="\n")')
recipe_id=$(printf '%s\n' "$recipe_pin" | sed -n '1p')
recipe_version=$(printf '%s\n' "$recipe_pin" | sed -n '2p')
recipe_digest=$(printf '%s\n' "$recipe_pin" | sed -n '3p')
recipe_owner='X-Brw-Owner: functional-recipe-test'
recipe_tab_json=$(curl -fsS -H 'content-type: application/json' -H "$recipe_owner" \
  --data '{"url":"http://127.0.0.1:'"$fixture_port"'/forms.html"}' \
  "http://127.0.0.1:$port/api/browser/open")
recipe_tab=$(printf '%s' "$recipe_tab_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["tab"]["id"])')
private_input='functional@example.com'
run_json=$(curl -fsS -H 'content-type: application/json' -H "$recipe_owner" \
  --data '{"id":"'"$recipe_id"'","version":"'"$recipe_version"'","digest":"'"$recipe_digest"'","inputs":{"field_name":"Email","email":"'"$private_input"'"},"tab_id":"'"$recipe_tab"'"}' \
  "http://127.0.0.1:$port/api/recipes/run")
if printf '%s' "$run_json" | grep -F "$private_input" >/dev/null; then
  echo "recipe response echoed a private input" >&2
  exit 1
fi
trace_json=$(curl -fsS -H "$recipe_owner" "http://127.0.0.1:$port/api/page/trace")
if printf '%s' "$trace_json" | grep -F "$private_input" >/dev/null; then
  echo "recipe secret input leaked into the browser action trace" >&2
  exit 1
fi
printf '%s' "$trace_json" | python3 -c 'import json,sys; value=json.load(sys.stdin); assert any(item.get("action") == "fill" and item.get("redacted") is True for item in value["entries"])'
recipe_artifact=$(printf '%s' "$run_json" | python3 -c 'import json,sys; value=json.load(sys.stdin); assert value["status"]=="done"; assert len(value["steps"])==4; assert value["steps"][0]["attempts"]==1; print(value["artifacts"][0]["artifact_id"])')
curl -fsS "http://127.0.0.1:$port/api/artifacts/$recipe_artifact/search?query=functional%40example.com&limit=2" \
  | python3 -c 'import json,sys; assert len(json.load(sys.stdin)) == 1'
rerun_json=$(curl -fsS -H 'content-type: application/json' -H "$recipe_owner" \
  --data '{"id":"'"$recipe_id"'","version":"'"$recipe_version"'","digest":"'"$recipe_digest"'","inputs":{"field_name":"Email","email":"'"$private_input"'"},"tab_id":"'"$recipe_tab"'"}' \
  "http://127.0.0.1:$port/api/recipes/run")
rerun_artifact=$(printf '%s' "$rerun_json" | python3 -c 'import json,sys; value=json.load(sys.stdin); assert value["status"]=="done"; assert value["steps"][0].get("attempts", 0)==0; assert value["steps"][1].get("attempts", 0)==0; print(value["artifacts"][0]["artifact_id"])')
curl -fsS -X DELETE "http://127.0.0.1:$port/api/artifacts/$recipe_artifact" >/dev/null
curl -fsS -X DELETE "http://127.0.0.1:$port/api/artifacts/$rerun_artifact" >/dev/null
curl -fsS -H 'content-type: application/json' -H "$recipe_owner" --data "{\"id\":\"$recipe_tab\"}" \
  "http://127.0.0.1:$port/api/browser/close" >/dev/null
echo "PASS private deterministic recipe    search/pin/dynamic-target/exact-value/events/timer/redaction/idempotent no-op/artifact"

# Rich browser composers often retain a structural <p><br></p> even while empty.
# Prove the external-write preflight treats that state as exact empty (zero
# actuation), then prove a filled contenteditable value using the same event.
rich_query=$(curl -fsS -H 'content-type: application/json' \
  --data '{"query":"test structural empty rich editor guard","origin":"http://127.0.0.1:'"$fixture_port"'","limit":3}' \
  "http://127.0.0.1:$port/api/recipes/search")
rich_pin=$(printf '%s' "$rich_query" | python3 -c 'import json,sys; value=json.load(sys.stdin); match=next(x for x in value if x["id"]=="example.fixture.contenteditable-guard"); print(match["id"], match["version"], match["digest"], sep="\n")')
rich_id=$(printf '%s\n' "$rich_pin" | sed -n '1p')
rich_version=$(printf '%s\n' "$rich_pin" | sed -n '2p')
rich_digest=$(printf '%s\n' "$rich_pin" | sed -n '3p')
rich_tab_json=$(curl -fsS -H 'content-type: application/json' -H "$recipe_owner" \
  --data '{"url":"http://127.0.0.1:'"$fixture_port"'/forms.html"}' \
  "http://127.0.0.1:$port/api/browser/open")
rich_tab=$(printf '%s' "$rich_tab_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["tab"]["id"])')
rich_run=$(curl -fsS -H 'content-type: application/json' -H "$recipe_owner" \
  --data '{"id":"'"$rich_id"'","version":"'"$rich_version"'","digest":"'"$rich_digest"'","inputs":{"note":"prepared rich draft"},"tab_id":"'"$rich_tab"'"}' \
  "http://127.0.0.1:$port/api/recipes/run")
printf '%s' "$rich_run" | python3 -c 'import json,sys; value=json.load(sys.stdin); assert value["status"]=="done"; assert value["steps"][0].get("attempts", 0)==0; assert value["steps"][1]["attempts"]==1'
curl -fsS -H 'content-type: application/json' -H "$recipe_owner" --data "{\"id\":\"$rich_tab\"}" \
  "http://127.0.0.1:$port/api/browser/close" >/dev/null
echo "PASS recipe contenteditable guard     structural-empty no-op/exact filled value"

# A completed-download event is a draining browser queue. This real-browser
# recipe proves the postcondition retains the new (not pre-arm) event long
# enough for the following capture step to copy the exact source file.
download_query=$(curl -fsS -H 'content-type: application/json' \
  --data '{"query":"get the original source invoice","origin":"http://127.0.0.1:'"$fixture_port"'","limit":3}' \
  "http://127.0.0.1:$port/api/recipes/search")
download_pin=$(printf '%s' "$download_query" | python3 -c 'import json,sys; value=json.load(sys.stdin); match=next(x for x in value if x["id"]=="example.fixture.download-source"); print(match["id"], match["version"], match["digest"], sep="\n")')
download_id=$(printf '%s\n' "$download_pin" | sed -n '1p')
download_version=$(printf '%s\n' "$download_pin" | sed -n '2p')
download_digest=$(printf '%s\n' "$download_pin" | sed -n '3p')
download_tab_json=$(curl -fsS -H 'content-type: application/json' -H "$recipe_owner" \
  --data '{"url":"http://127.0.0.1:'"$fixture_port"'/downloads.html"}' \
  "http://127.0.0.1:$port/api/browser/open")
download_tab=$(printf '%s' "$download_tab_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["tab"]["id"])')
download_run=$(curl -fsS -H 'content-type: application/json' -H "$recipe_owner" \
  --data '{"id":"'"$download_id"'","version":"'"$download_version"'","digest":"'"$download_digest"'","tab_id":"'"$download_tab"'"}' \
  "http://127.0.0.1:$port/api/recipes/run")
download_artifact=$(printf '%s' "$download_run" | python3 -c 'import json,sys; value=json.load(sys.stdin); assert value["status"]=="done"; assert value["steps"][0]["attempts"]==1; print(value["artifacts"][0]["artifact_id"])')
curl -fsS "http://127.0.0.1:$port/api/artifacts/$download_artifact/read?offset=0&max_bytes=128" \
  | python3 -c 'import base64,json,sys; value=json.load(sys.stdin); payload=value.get("text", "").encode() if value["encoding"]=="utf-8" else base64.b64decode(value["base64"]); assert b"SYNTHETIC-SOURCE-INVOICE" in payload'
curl -fsS -X DELETE "http://127.0.0.1:$port/api/artifacts/$download_artifact" >/dev/null
curl -fsS -H 'content-type: application/json' -H "$recipe_owner" --data "{\"id\":\"$download_tab\"}" \
  "http://127.0.0.1:$port/api/browser/close" >/dev/null
echo "PASS recipe download artifact        pre-arm/new-event/source-file continuity"
