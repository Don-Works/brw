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

For `--mcp --upstream-http`, the disposable proxy forwards random correlation
headers but does not write a duplicate ledger. The long-lived upstream daemon
is the canonical writer for operations it receives.

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
