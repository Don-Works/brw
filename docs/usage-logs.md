# Privacy-safe usage logs

`brwd` keeps a bounded NDJSON operations ledger by default so reliability,
latency, reconnects, and tab-cleanup behaviour can be analysed after an agent
run. It is operational telemetry, not a browser transcript.

## What is recorded

Each line contains only allowlisted metadata:

- UTC timestamp, brw version, process id, and layer (`mcp`, `http`, or `bridge`)
- canonical operation name, outcome, duration, and HTTP status when applicable
- stable error class, retryability, and a failure-shape fingerprint
- random session/request correlation ids and safe workspace/profile/mode labels
- extension build number on bridge connect/disconnect events
- schema version, microsecond duration, measurement scope and representation
- nullable input/output byte and Unicode character counts, separately labelled chars/4 token estimates
- snapshot mode/format, delta use, truncation and bounded element counts when available

## Context and timing measurements

`brw usage --since 1h` reads active and rotated ledgers without a daemon request.
Use `--layer mcp`, `--profile chromium-work`, or `--operation brw_snapshot` to
narrow it; `--json` preserves sample counts and `--watch 5s` refreshes it.
`--path` accepts another ledger file or directory. Scans are bounded and report
invalid records, incomplete lines, skipped reads and coverage limits explicitly.

| Boundary | Input/output meaning |
| --- | --- |
| HTTP transport | Body bytes actually consumed/written; encoded query bytes separate |
| MCP tool | Serialized arguments and result; text characters exclude recognised binary fields |
| MCP catalogue | Tool-schema output from `tools/list`; separate from tool calls |
| CLI projection | JSON array encoding of verb words and received arguments, excluding executable and shell quoting; output is bytes actually written to stdout |
| Reader model | Provider request/response bytes and provider-reported tokens where available |
| Reader job/transport | Whole-job and adapter measurements, separate from model spend |

These boundaries overlap: do not sum them as total context or tool-call counts.
MCP structured-output bytes are identified separately because a client may
expose both text and structured content. A gateway can reduce or wrap a result
after brw returns it. CLI counters omit any surrounding shell/MCP envelope.

Missing counters mean unknown, while zero means measured zero. The report shows
`?` for unknown and `*` for partial coverage. Token estimates use ceil(chars/4),
not a provider tokenizer. brw cannot see the controlling model's complete prompt,
cached input, reasoning or output tokens; only an integrated provider can report
those. Reader provider counters occur only on model rows to avoid duplicate spend.
Cached/reasoning token counters may be subsets of input/output counts.

Operation duration ends before telemetry forwarding. Proxy/CLI metadata reports
have an independent 100 ms deadline and logging failures do not fail the browser
action. Reports are best effort, so record coverage matters. The non-streaming
reader reports request-to-headers timing; first-visible-token timing stays unknown.
Neither transport duration nor reader timings alone establish page→model→action
latency for an external controlling model.

An operation can be `degraded` rather than failed. For example, a successful
tab open whose Chromium window rejects a requested group records
`tab_group_assignment`, outcome `degraded`, and error class `capability`; it
does not record the URL or requested group name.

Failure fingerprints are calculated from an allowlisted error shape such as
`timeout`, `not_connected`, or `ref_not_found`. Raw error messages are neither
stored nor hashed, so a password accidentally included in an error cannot be
tested against the ledger with an offline password dictionary.

## What is never recorded

The schema has no fields for tool arguments, prompts, typed text or form values,
page content, titles, URLs or query strings, request/response headers or bodies,
screenshots, cookies, credentials, filesystem paths, or downloaded/uploaded
file contents. Consequently the ledger can answer “which operation timed out?”
but cannot reconstruct what an agent typed or read.

The usage directory is created with owner-only permissions (`0700`) and each
ledger file is forced to `0600`.

## Location and retention

With `--usage-log auto` (the default), logs live under the operating system's
user config directory:

- macOS: `~/Library/Application Support/brw/usage/`
- Linux: `${XDG_CONFIG_HOME:-~/.config}/brw/usage/`
- Windows: the user's AppData config directory under `brw/usage/`

The filename is derived from safe workspace, profile, and transport-mode labels.
The active file rotates at 20 MiB and keeps seven backups by default.

Configure or disable it with:

```sh
brwd --usage-log /private/path/brw.ndjson \
  --usage-log-max-mb 20 \
  --usage-log-backups 7

brwd --usage-log off
```

Equivalent environment variables are `BRW_USAGE_LOG`,
`BRW_USAGE_LOG_MAX_MB`, and `BRW_USAGE_LOG_BACKUPS`.

For `--mcp --upstream-http`, the proxy forwards random correlation headers and
its metadata-only MCP measurements to the upstream daemon's `/api/usage/report`.
The daemon remains the canonical writer. The report endpoint accepts only bounded,
allowlisted metadata and is excluded from its own operation logging. Old proxies
must restart to produce the new MCP counters; upgrading the daemon alone starts
HTTP counters immediately. CLI projection measurements use the same route.

The optional Python reader shares `reader.jsonl` under this directory, with a
cross-process lock, 1 MiB rotation and three backups by default. It has its own
`--no-usage-log`, `--usage-dir`, `--usage-max-bytes` and `--usage-keep` controls;
turning off daemon logging does not turn off a separately configured reader.
Reader artifacts remain a separate, explicitly requested evidence surface.

## Harvesting a reliability summary

This example groups all active and rotated records by operation and safe failure
metadata without exposing browser data:

```sh
jq -s '
  group_by([.operation, .outcome, (.error_class // ""), (.error_fingerprint // "")])
  | map({
      operation: .[0].operation,
      outcome: .[0].outcome,
      error_class: (.[0].error_class // ""),
      error_fingerprint: (.[0].error_fingerprint // ""),
      count: length,
      max_duration_ms: (map(.duration_ms // 0) | max)
    })
  | sort_by(-.count)
' "$HOME/Library/Application Support/brw/usage/"*.ndjson*
```

To inspect lifecycle flaps only:

```sh
jq 'select(.operation == "bridge_connect" or .operation == "bridge_disconnect")' \
  "$HOME/Library/Application Support/brw/usage/"*.ndjson*
```
