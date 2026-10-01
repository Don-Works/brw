# Deploying the optional reader

brw bundles an optional reader separately from its normal browser tool surface.
Python is needed only when using this reader. The browser daemon continues to
work without a model or classifier.

## Payload and startup

Unix archives contain `reader/browser-answer-worker.py`,
`reader/browser-reader-mcp.py` and their shared `reader/browser-reader-usage.py`
helper. Keep these files together. The shell installer places them in
`<app-dir>/reader/`. Native packages use `/usr/share/brw/reader/` on Linux and
`/usr/local/share/brw/reader/` on macOS. Homebrew retains the archive's `reader/`
under the formula prefix. The Windows packaging source includes `share/reader/`;
Windows installers are not currently published.

Older `brwctl` versions do not know the new payload directory. For the first
upgrade, use the new shell installer or extract the release archive; subsequent
upgrades with v0.19.0's `brwctl` include it.

For a cloud image unpacking the archive at `/opt/brw`:

```sh
python3 /opt/brw/reader/browser-reader-mcp.py \
  --worker /opt/brw/reader/browser-answer-worker.py \
  --config /run/config/brw-reader.json \
  --artifacts-dir /var/lib/brw-reader/jobs \
  --max-concurrent 2 --timeout 180
```

The config supplies the `brw` executable path, healthy headless daemon endpoint,
answer/classifier models and endpoints, credential environment variable names,
and budgets. See the [configuration contract](../skills/brw/references/decision-workers.md).
Inject credential values through the host's secret mechanism. Do not copy a
developer's credentials, signed-in profile or machine-specific wrapper into an
image. The artifact directory must be private (mode 0700 on Unix); configure
retention for reports, source evidence, progress events and diagnostics.

The adapter validates HTTP(S) URLs and rejects embedded credentials. This is
not public-IP or redirect isolation; apply the deployment's normal browser
navigation and network policy. Cancellation interrupts the child worker and
suppresses its reply. The adapter retains its capacity slot until the child has
exited and cleanup has completed. On POSIX it first sends SIGINT, allowing Python
`finally` cleanup, then force-kills after at most five seconds. Windows uses
process termination. A forced kill or termination cannot guarantee tab cleanup;
inspect the job journal and owned browser resources after failures. Interrupting
the client does not promise cancellation of work already running at a provider or
zero further provider spend.

## Automatic usage metadata

The worker and adapter automatically append metadata to `reader.jsonl` under the
OS user configuration directory: `~/Library/Application Support/brw/usage` on
macOS, `${XDG_CONFIG_HOME:-~/.config}/brw/usage` on Linux and `%APPDATA%/brw/usage`
on Windows. The directory and files must be private; POSIX modes are 0700 and
0600. All reader processes coordinate through `.reader.lock`, rotating one shared
file with a default one-MiB limit and three archives. Logging waits at most
200 milliseconds for the lock; unavailable logging emits a generic warning and
allows the reader operation to continue.

Configure `usage_dir`, `usage_max_bytes`, `usage_keep` and `usage_log` in the worker
JSON config, or pass `--usage-dir`, `--usage-max-bytes`, `--usage-keep` and
`--no-usage-log` to either entrypoint. The adapter applies its chosen directory,
rotation bounds and disabled state to its child. For example, a cloud service can
use `--usage-dir /var/lib/brw-reader/usage --usage-max-bytes 1048576 --usage-keep 3`.
Set `usage_log` to `false` to disable logging. Bounds are 4096 to 67108864 bytes
and zero to sixteen archives.

Records contain timestamps, generated job/request correlations, counts and phase
durations. They exclude prompts, page text, answers, URLs, raw errors, credential
values and model names. Model requests record actual serialized input/output
bytes, explicit character/4 estimates and provider-reported input, output,
cached-input, cache-write and reasoning tokens where supplied. Missing provider
counts remain unknown rather than zero. Nonstreaming first-token timing remains
unknown. Replayed sources do not report historical collection timing as current
work. See [usage logs](usage-logs.md) for the shared summary and accounting scopes.

The bounded result trace includes collected/total source counts, source truncation and
evidence narrowing, a source hash and up to sixteen evidence ranges. Full ranges
and detailed source/progress reports remain in the private artifacts; configure
their retention separately from the metadata ledger.

## Maix handoff

The brw release does not register a server, install a mesh scheduler, deploy
models or update a Maix image. The Maix deployment owner must:

1. Pin the brw release and verify its archive checksum/provenance. Include Python
   and all three reader files in the image alongside the headless browser runtime.
2. Supply operator configuration and credentials, and create the private artifact
   volume. Use the intended local or hosted answer model and independently choose
   classifier `off`, `shadow` or `select`.
3. Register the command above as a separate stdio MCP server, route it to the
   intended workspaces, and review/accept its one-tool schema where required.
   The tool is `brw_ask` with only `url` and `question` arguments; gateway prefixes
   depend on registration. Existing browser namespaces stay available.
4. Publish/sync the complete brw skill bundle. Discover the tool through the
   actual gateway, call it on a public page, verify answer/source and inspect the
   private report for the configured model, costs, timing and successful cleanup.
   Repeat from each intended harness; a source build alone does not prove routing.

The adapter supports initialize-based MCP stdio through protocol `2025-11-25`.
Tool calls return bounded answer/source/trace packets synchronously. Background
job dispatch and mesh completion delivery belong to the host and must be tested
separately. This reader does not propose or execute interactive browser writes.

## Next integration: page event backchannel

An independent AWS dogfood report found delayed redirects after action completion
and oversized menu snapshots. These remain open usability problems, not fixes in
this release. The requested follow-up is a small event channel carrying session,
tab and navigation identities, sequence number, current URL and event type. The
agent should be able to continue work while receiving navigation changes, then
perform a targeted destination/readiness check. Dispatch, navigation commit and
application readiness must remain distinct; a changed URL is not proof of a
completed business operation.

The host integration should preserve per-session tab ownership, coalesce repeated
loading events, expose dropped-event/resync status, and test delayed redirects,
same-document navigation and background tabs. Full snapshots should be fetched
only when a specific event requires them. This page-event channel is separate
from reader job completion and from exact-content approval for external writes;
none of these three backchannels is supplied by the reader adapter.
