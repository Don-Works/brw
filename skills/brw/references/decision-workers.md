# Optional decision and reading workers

brw works directly with the caller's model. It needs no intermediate model,
classifier, hosted API or Maix. Keep that direct path available. Optional workers
belong outside the browser daemon, where the caller can choose a provider, run a
job asynchronously, and return only its bounded result to the main model.

## Route cheaply

1. Use brw's native data, projected reads, semantic find, compact observations,
   deltas, assertions and recipes before asking another model to interpret a page.
2. Use deterministic code for exact matches, categories, counts, visibility,
   disabled/stale checks and candidate identity. Refresh when required evidence is
   missing. Never let a classifier invent a ref or bypass an ambiguity check.
3. Consider a typed classifier for uncertain relevance or semantic choices among
   observed, valid candidates. Keep an explicit insufficient-evidence choice.
4. Use a generative worker for prose or recovery. Keep full pages, intermediate
   snapshots and detailed traces in worker artifacts. Return a concise answer,
   source and success/failure status; expand evidence only when needed.

Jev is one possible classifier, not a requirement. It selects supplied options;
it does not write a summary or drive the browser on its own. If the global
`jev-decisions` skill is installed, use its shadow evaluation procedure. A shadow
choice is recorded without executing it or replacing the current decision.
Agreement and confidence are not independent proof of correctness.

## Portable experiment runner

The source checkout includes `scripts/browser-answer-worker.py`, a public-page
reader experiment. The optional `scripts/browser-reader-mcp.py` adapter exposes
it as one `brw_ask` MCP tool. Both entrypoints require the adjacent
`scripts/browser-reader-usage.py` helper. Neither is an unattended navigation
agent or part of the standard browser daemon's tool catalogue.
It uses the `brw` CLI and a healthy headless daemon, creates a unique tab owner,
and closes only its own tab. It supports replaying the same captured page for
paired measurements. Other callers can use their existing brw MCP transport
instead of this collector. No private profile credentials are copied.

When an operator has enabled the reader, discover `brw_ask` and use it for narrow
questions about known public URLs. Gateway namespace prefixes are installation
specific. If the tool is absent but the operator installed `brw-ask`, invoke
`brw-ask --url URL --question QUESTION` through the execution surface. Otherwise
use the direct brw tools. Do not treat this skill's presence as proof that the
worker, credentials, models or tool registration exist on the current host.

```sh
python3 scripts/browser-answer-worker.py \
  --config /private/config/reader.json \
  --url https://en.wikipedia.org/wiki/Johns_Hopkins \
  --question 'Who was Johns Hopkins, the American philanthropist?' \
  --out /private/jobs/unique-job/report.json
```

Configuration example; substitute the caller's actual service URLs and model IDs:

```json
{
  "brw": "brw",
  "daemon": "http://brwd:17710",
  "answer_model": "provider/model-version",
  "answer_endpoint": "https://model-service.example/v1/chat/completions",
  "answer_key_env": "READER_API_KEY",
  "classifier_mode": "off",
  "classifier_protocol": "decisions",
  "classifier_model": "classifier-version",
  "classifier_endpoint": "https://decision-service.example/decisions",
  "classifier_key_env": "CLASSIFIER_API_KEY",
  "evidence_mode": "full",
  "evidence_max_chars": 32000,
  "passage_chars": 2000,
  "passage_count": 8,
  "answer_max_tokens": 128,
  "answer_max_chars": 1000,
  "request_timeout": 30,
  "reasoning_effort": "none",
  "usage_log": true,
  "usage_max_bytes": 1048576,
  "usage_keep": 3
}
```

CLI flags override the JSON config. Inject credentials as environment variables;
config stores their names, not values. Set a key-env setting to `null` for an
unauthenticated local endpoint. Set `answer_model` to `null` for extractive output
without generation. Classifier mode is independently `off`, `shadow` or `select`.
The default is no model and no classifier. `evidence_mode:ranked` packs multiple
deterministically ranked passages within `evidence_max_chars`, including short
facts, and records their source ranges. A classifier in `select` mode still chooses
one supplied passage or `none`; it can lose context needed from another passage.
Full mode supplies a source prefix within the same character budget. Collection
currently reads at most 100000 characters; completeness fields show when more
source or evidence is available. A bounded excerpt or answer is not proof that all
relevant evidence was supplied.

Answer endpoints must support OpenAI-compatible chat completions. Classifiers
support TypeSafe-compatible `decisions` responses or `openai-chat` JSON choices.
For chat classifiers, configure `classifier_response_format` as `json_schema`,
`json_object` or `omit` for the serving adapter. Configure thinking explicitly;
`reasoning_effort:omit` leaves it to the server. These are supported wire formats,
not a claim that arbitrary vendor APIs are interchangeable.

`request_timeout` is one absolute optional-stage budget shared by classifier and
answer, including child startup and response reads. Deterministic ranked evidence
is retained first. If either optional stage fails, including a shadow classifier,
the worker skips later model stages and returns the usual bounded `excerpt`,
`source` and completeness/range trace with a fixed `fallback:{stage,reason}`.
Treat that excerpt as source evidence to read; do not describe it as a generated
answer. Missing credentials, unavailable endpoints, timeouts, HTTP failures,
invalid/oversized/truncated/empty responses, unknown candidate IDs and missing or
mismatched returned model IDs trigger fallback. The returned model ID must equal
the configured ID; that label does not attest the underlying checkpoint/runtime.
A valid `none` choice skips generation and returns a deterministic abstention under
`excerpt`. Collection, cancellation, whole-job deadline and cleanup failures remain
errors. No fallback changes provider or escalates to cloud. There is no cross-job
circuit breaker; admission/cooldown belongs to the host when required. See the
[deployment and qualification contract](https://github.com/Don-Works/brw/blob/main/docs/reader-deployment.md).

The same settings work for local or cloud services. Maix can own job scheduling,
credential injection, cancellation and mesh delivery without teaching brw about
those services. A completion handler should forward only the bounded stdout
packet and artifact location to the parent; do not hydrate the full source into
its context. A generic coding-agent wrapper may add repository prompts, tool
catalogues and mandatory report formats that defeat a tiny reading job. Use a
bounded worker contract and measure that wrapper separately.

The runner writes a report, `.source.json` and `.jsonl` progress events. Retain
request IDs/hashes, requested/returned models, usage, phase timings, source
truncation, evidence size and parent-result size. Unknown first-token, queue and
prefill times stay unmeasured. Provider usage in phase reports is normalized to
validated counts; arbitrary provider metadata is excluded. Failed calls and
fallback stage/reason remain visible.
The source and report directories must already exist; use a unique private output
path per concurrent job. The host scheduler remains responsible for the whole-job
deadline and cancellation when invoking the standalone worker. The MCP adapter
enforces its configured deadline and interrupts its child on cancellation.

Worker and adapter metadata logging is on by default. One shared `reader.jsonl`
under the OS configuration directory's `brw/usage` retains at most one active
one-MiB file plus three archives, using a cross-process lock. The macOS directory
is `~/Library/Application Support/brw/usage`; Linux uses `XDG_CONFIG_HOME` or
`~/.config`, and Windows uses `APPDATA`. Override with `usage_dir` or `--usage-dir`.
Configure `usage_max_bytes`/`--usage-max-bytes` (4096 to 67108864) and
`usage_keep`/`--usage-keep` (zero to sixteen). Disable with `usage_log:false` or
`--no-usage-log`. The adapter propagates its selected directory, rotation bounds
and disabled state to its child.

These records contain counts, generated correlations, timestamps and phase times
only: no prompts, source or answer bodies, raw URLs/errors, credentials or model
names. Provider counts are separate from explicit character/4 estimates, and
unknown counts stay unknown. Collection health/open/read/cleanup and model
serialization/request/decode durations are recorded where observable. Replayed
sources do not attribute their historical collection timing to the current job.
First-token, provider queue/prefill and the caller model's context remain
unobservable to this nonstreaming worker. Detailed artifacts remain separately
retained and private. Use the [usage summary](../../../docs/usage-logs.md) with its
scope labels; adding model, worker and transport byte counts together double
counts their different boundaries.

## Optional MCP surface and deployment verification

Launch the stdio adapter with an operator-controlled worker and configuration:

```sh
python3 scripts/browser-reader-mcp.py \
  --worker /opt/brw/scripts/browser-answer-worker.py \
  --config /private/config/reader.json \
  --artifacts-dir /private/jobs/brw-reader
```

A `.py` worker runs with the adapter's Python interpreter; an installed `brw-ask`
executable can be used instead. Credentials must be supplied by the host or the
operator's wrapper. Tool arguments contain only `url` and `question`; they cannot
change the worker executable, provider configuration or output path. The adapter
returns a bounded result and trace location, with collected evidence retained in
private job artifacts. The trace forwards source total/collected counts,
source truncation/evidence narrowing, source hash and up to sixteen evidence ranges with
their total count and range-list truncation status. Model output remains untrusted
evidence.

Register this as a separate optional MCP server in each intended host or gateway.
Installing brw's skill, publishing a registry bundle, or installing the browser
binary does not register this adapter. The adapter uses newline-delimited stdio
and negotiates the initialize-based MCP protocol through `2025-11-25`; do not
claim support for newer protocol families without an integration test.

Verify each deployment through its actual client: initialize, list `brw_ask`,
invoke it on a public test page, inspect the concise answer and source, and
confirm the private report records the intended provider and model. Repeat the
discovery check after any gateway tool-schema acceptance or reload. A local
stdio test does not verify a cloud deployment or an existing client's cache.

Calls run synchronously from the caller's perspective. The adapter can service
other requests while a bounded number of reads run; use the host's job scheduler
for background execution and mesh completion delivery. Cancellation interrupts
the child and suppresses delivery; its capacity slot remains occupied through
child exit and cleanup. POSIX uses SIGINT followed by force-kill after at most
five seconds. Windows uses bounded native process-tree termination and reports
cleanup failure if it cannot complete; it has not been runtime qualified. Forced
termination can prevent tab cleanup. Cancellation does not promise interruption
of already-running provider work or zero further spend. The adapter does not
install a mesh trigger or an approval channel for browser writes.

## Evidence and promotion

The 2026-10-01 exploratory run found that code resolved all 24 exact synthetic
decisions. Jev also resolved those cases, but an ambiguous semantic case failed
with high confidence. The best tested 4B native-tool prompt scored 21/24; DeepSeek
Flash scored 22/24 and twice proposed a stale fill. Candidate filtering changed
the contract, so typed-choice scores are not intrinsic rankings against native
function-calling scores.

Across three public pages and three repetitions per path, full-page local answers
had a 710 ms median, Jev-selected local answers 829 ms, and deterministic-passage
local answers 441 ms. Shorter evidence caused a factual regression on the Merkle
tree question. These are exploratory replay timings with warm/cache effects;
collection and main-model review are separate. They do not qualify unattended
browsing or justify enabling a classifier by default. See the repository's
`docs/competitive-review-2026-10.md` and measurement artifacts for full limits.
