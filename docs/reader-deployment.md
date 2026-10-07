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
bounded native process-tree termination. A forced kill or termination cannot
guarantee tab cleanup;
inspect the job journal and owned browser resources after failures. Interrupting
the client does not promise cancellation of work already running at a provider or
zero further provider spend.

## Model failure and deadline contract

The worker collects and saves deterministic ranked evidence before attempting any
optional model. `request_timeout` is one absolute budget shared by classifier and
answer stages, including their Python child startup and response reading. Each
HTTP request runs in an owned child so slow header/body delivery cannot extend
that budget indefinitely. A timed-out or interrupted child is killed and reaped,
with at most one second of child cleanup. Failed child cleanup fails the job.
The adapter's whole-job timeout remains separate and includes collection and
artifact work; leave room for model and cleanup budgets within it.

Successful generation retains the existing `answer` and `source` packet. No-model
reads return `excerpt` and `source`. If an optional classifier (including shadow)
or answer stage fails, remaining model stages are skipped and the worker restores
the same deterministic excerpt and evidence ranges as a no-model read. It adds:

```json
{"fallback":{"stage":"answer","reason":"timeout"}}
```

`stage` is `classifier` or `answer`. Reasons are `unavailable`, `timeout`, `http`,
`invalid_response`, `oversized_response`, `truncated_response`, `empty_response`,
`unknown_candidate` or `unknown_identity`. These fixed fields reach the MCP packet
and metadata ledger; provider error bodies and credential names/values do not.
The adapter retains source completeness, hash and bounded ranges in `trace`.
A fallback excerpt is evidence for the caller to read, not a generated answer.
A valid selector choice of `none` produces a deterministic insufficient-evidence
message under `excerpt` and skips generation.

The returned model ID must exactly match the configured ID. Missing or different
IDs cause `unknown_identity`; configure the service's actual returned ID rather
than relying on an alias. This check identifies the response label, not the
checkpoint, quantization or image processor behind it. Qualification must verify
those separately. Missing optional credentials cause `unavailable` after evidence
collection. Collection, invalid job configuration, cancellation, whole-job timeout
and cleanup failures remain job errors. Cancellation suppresses result delivery.

On Windows the adapter uses native `taskkill /PID <owned-worker-pid> /T /F` with a
five-second bound to terminate its process tree. If that command is unavailable
or fails, it kills the worker and reports cleanup failure; descendant cleanup is
then uncertain. Windows process-tree behavior is covered by mocked command tests,
not a Windows runtime qualification. POSIX cancellation and slow-drip model
fallback are exercised with owned local fixtures. Terminating a local HTTP client
does not guarantee that the provider stops already submitted work or spending.

There is no persistent circuit breaker in the short-lived worker. A deployment
may add host-owned admission/cooldown around repeated failures. No fallback
switches providers, escalates to cloud or enables a model automatically.

## Qualifying optional local and visual models

Keep ordinary DOM reads and deterministic selection available. The reader is
currently text-only; an OpenAI-compatible text endpoint or a `/v1/models` listing
does not establish image support. Its fallback implementation does not enable
OCR, vision, embeddings or interactive action proposals.

Before an operator enables a local model, pin the runtime version, exact model
revision/checkpoint hash, quantization/dtype, tokenizer and chat template, thinking
mode, context/output budgets and returned model ID. Record cold load, warm request,
preprocessing and complete job timings separately, plus provider-reported tokens;
character/4 estimates remain separate. Include child startup, failures, retries,
fallback and cleanup when comparing this release to historical reader timings.
Use public or explicitly owned fixtures, freeze a held-out set before tuning and
score evidence coverage, factuality, abstention and schema/identity failures before
latency or token savings. Provider work must be approved and budgeted by the host.

Google's [Gemma releases](https://ai.google.dev/gemma/docs/releases) identify
EmbeddingGemma 2, released 6 October 2026, as a retrieval candidate, and Gemma 4
[E2B](https://huggingface.co/google/gemma-4-E2B-it) and
[12B](https://huggingface.co/google/gemma-4-12B-it) as visual-reading candidates.
[EmbeddingGemma 2's model card](https://huggingface.co/google/embeddinggemma-2)
requires float32 or bfloat16; float16 can silently degrade results. Its embeddings
could improve passage recall, but brw has no measured gain from these models.
Gemma 2 is an older 2024 family and is not this release's newer visual candidate.

For image experiments verify the exact multimodal runtime and processor, image
request format, resize/crop/token budget and response schema. Text-only MLX-LM
support is insufficient; use a verified image-serving adapter, such as a qualified
[MLX-VLM Gemma 4 build](https://github.com/Blaizzy/mlx-vlm/blob/main/mlx_vlm/models/gemma4/README.md),
and test its exact checkpoint. Record image hashes and geometry; measure small-text
OCR and ambiguous/unsupported cases against the same deterministic evidence.
Do not send signed-in pages or screenshots to a provider without the operator's
permission. A loopback URL alone does not prove local processing.

The checkout's `scripts/measure-local-browser-model.py` accepts public/owned PNG
corpora separately from the text reader. Prepare a corpus and runtime identity
manifest, then freeze and validate the plan without contacting any endpoint:

```sh
python3 scripts/measure-local-browser-model.py \
  --model EXACT_RETURNED_MODEL_ID \
  --vision-fixtures /private/qualification/corpus.json \
  --identity-manifest /private/qualification/runtime.json \
  --plan-only --out /private/qualification/plan.json
```

The corpus has `schema_version:1`, `source_policy:public_fixture` or `owned_fixture`,
and `cases` with `name`, `goal`, relative PNG `image` and expected tool `name`/
`arguments`. Optional `target_region` uses image pixels; `image_transform` declares
viewport offset and CSS pixels per image pixel for coordinate proposals. Never
assume image coordinates equal browser coordinates. The identity manifest pins
`schema_version:1`, `model`, `runtime:{name,version,backend,device}`,
`checkpoint:{publisher,revision,quantization,adapter,sha256}` and
`tokenizer:{revision,chat_template_sha256}`, with lowercase SHA-256 hashes.
Its identity values are operator declarations, not runtime attestations.

`--plan-only` makes no network or inference call and writes a frozen
`.manifest.json` alongside the output. After separate operator approval and budget
admission, a run against a qualified image server may omit that flag. The harness
proposes tool calls only; it executes no browser effects. `--first-request-state`
is `unknown`, `cold` or `warm`, an operator declaration rather than an inferred
cache state. Verify rendering/preprocessing and actual task outcomes separately;
a passing fixture score does not activate vision in `brw_ask` or qualify private
page processing.

Model output remains untrusted evidence. A proposed action must be checked against
fresh DOM refs and the existing browser policies. Models cannot authorize writes,
bypass consent/approvals or turn failed perception into an executed action.

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
durations, plus fixed fallback stage/reason fields. They exclude prompts, page
text, answers, URLs, raw errors, credential values and model names. Model requests record actual serialized input/output
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
Tool calls return bounded answer-or-excerpt/source/trace packets. Calls can overlap
within the configured capacity; ping and cancellation remain responsive while a
worker runs. A supplied `_meta.progressToken` enables elapsed-time progress every
five seconds until completion or cancellation. Progress is a liveness signal,
not a percentage, page change or task result. Background job dispatch and mesh
completion delivery belong to the host. This reader does not propose or execute
interactive browser writes.

The browser MCP server likewise accepts correlated concurrent calls and cancellation
and emits elapsed-time `notifications/progress` for long tool calls with a supplied
string or integer token. Preserve notification routing and session identity when
proxying it. brw does not advertise experimental MCP Tasks support.

The current Maix integration needs three host-side fixes before its native reader
and browser-event polling meet this contract: preserve `SessionID` when looking
up a browser event journal; accept `excerpt`/`fallback` with source evidence in
addition to a generated `answer`; and terminate the owned reader process tree
on cancellation. Killing only its Python parent can leave HTTP children running.
The standalone brw reader adapter already owns and reaps its process group.
These host changes belong to the Maix deployment; the brw release does not install
them. See the [MCP progress](https://modelcontextprotocol.io/specification/2025-11-25/basic/utilities/progress)
and [cancellation](https://modelcontextprotocol.io/specification/2025-11-25/basic/utilities/cancellation)
contracts when checking gateway routing.

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
