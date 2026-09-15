# Private recipes and browser-host artifacts

`brw` now has two complementary escape hatches from repeated model work:

- a deterministic recipe runtime for browser flows already understood; and
- a browser-host artifact store for observations too large or sensitive to put
  into every model response.

The engine, schema, safety checks, provider interface, and MCP/HTTP APIs are open
source. The recipe corpus is not.

## Where recipes belong

Do not commit operational recipes to the `brw` repository. A recipe can reveal
site names, internal workflows, element labels, risk decisions, and business
processes even when it contains no credential.

Choose one of these private providers:

1. `--recipe-root /absolute/private/path` loads owner-only JSON files from a
   directory outside the source checkout. This is convenient for a personal or
   modest team collection. The directory must be mode `0700` or stricter and
   files must be mode `0600` or stricter. Symlinks and paths inside the `brw`
   repository—or any other Git checkout—are rejected. The directory provider
   notices atomic additions and new immutable versions without a daemon restart.
2. `--recipe-provider-url https://recipes.internal.example` delegates search
   and exact fetch to an operator-owned service. This is the recommended shape
   for a large bank: use database full-text search and a vector index there,
   apply organization authorization there, and keep the browser runtime small.
   Loopback HTTP is allowed for a local sidecar. Supply a bearer credential via
   `--recipe-provider-token-file /absolute/owner-only/file`; the token file must
   be a non-symlink regular `0600` file and its contents are never logged.

The corresponding environment variables are `BRW_RECIPE_ROOT`,
`BRW_RECIPE_PROVIDER_URL`, and `BRW_RECIPE_PROVIDER_TOKEN_FILE`. Directory and
HTTP providers are mutually exclusive.

The HTTPS provider contract consists of two strict JSON POST endpoints:

```text
POST /v1/recipes/search
{"query":"download monthly document","origin":"https://billing.example.test","limit":10}
-> {"matches":[{"id":"billing.download","version":"2026.09.1","name":"Download monthly document","description":"Retrieve one reviewed billing document.","origins":["https://billing.example.test"],"risk":"read_only","digest":"<sha256>","score":0.98}]}

POST /v1/recipes/fetch
{"id":"billing.download","version":"2026.09.1","digest":"<sha256>"}
-> {"recipe":{...the exact schema-v1 recipe...}}
```

Responses are limited to 8 MiB, requests have a hard 20-second deadline,
unknown or trailing JSON is rejected, and redirects are never followed with the
bearer credential. The browser host independently revalidates provider metadata
and the fetched body, so a custom provider cannot bypass identity, origin,
disclosure, or digest checks.

A Codex or MCP skill is a useful *discovery adapter*: it can tell an agent when
to call `brw_recipe_search` and how to collect the declared inputs. It should not
be the canonical recipe database. Skills are commonly synced, installed, or
published as files; a private provider gives recipes an explicit access-control,
versioning, audit, and revocation boundary.

The bundled `brw` skill also tells agents to promote a successfully completed,
stable workflow when reuse is reasonably likely and to repair deterministic site
drift after a recipe failure. Promotion still writes through the private provider,
never into the skill or repository. For a local directory, validate and install an
owner-only draft atomically with:

```sh
brwctl recipe validate --file /absolute/private/draft.json
brwctl recipe install --file /absolute/private/draft.json --root "$BRW_RECIPE_ROOT"
```

Installing the same content is idempotent. Reusing an existing `id + version`
with different content is rejected: create a new semantic version. Local search
advertises only the newest version for each recipe ID while an older version
remains fetchable by an already-pinned digest.

## Search, pin, then run

The model-facing flow has two calls:

```text
brw_recipe_search { query: "download invoices from the billing page", origin: "https://billing.example.com" }
  -> { id, version, digest, description, origins, risk, score }

brw_recipe_run { id, version, digest, inputs: { month: "2026-08" }, tab_id }
  -> { status, step timings, artifact handles }
```

Search discloses metadata only. It never returns steps or inputs. Execution then
fetches exactly the selected `id + version + SHA-256 digest`; substitution or a
changed recipe fails closed. The in-memory catalogue has an inverted
lexical/origin index, authored natural-language intent aliases, bounded top-K
ranking, and O(1) immutable fetches. The local directory provider deliberately
stats its files before each operation so newly installed versions appear without
a restart; that change-detection pass is O(number of files), making it a modest
single-user store rather than the huge-bank design. At large scale, the HTTPS
provider should own embeddings/vector search and return already-ranked
candidates.

When a recipe fails, first distinguish deterministic site drift from expired
authentication, missing authorization, bad inputs, outages, rate limits, or
human challenges. Only drift should create a repaired version. Inspect the live
page, make the smallest semantic-target/event correction, increment the version,
validate, install, and confirm search returns the new head. Never blindly retry
an ambiguous external write merely to test a repair.

Recipes use semantic targets (`role`, accessible name, test id, or href fragment)
that are resolved immediately before each action. Ephemeral observation refs
such as `e17` are forbidden. Exact origins are checked before every step and
again around every target resolution and actuation. Accessible-name, test-id,
and href selectors may interpolate declared inputs at runtime; the role remains
static, and resolution must still produce exactly one element before brw acts.

The public schema supports clicks, fills, typing, selection, key presses,
navigation, bounded timers, event waits, and artifact capture. Every browser
actuation explicitly declares `effect: read` or `effect: external_write`; the
recipe-level risk must agree with the strongest step. Events include:

- page ready, URL match, and text present/absent;
- element visible/hidden, exact element value (including empty), and
  composer-scoped value containment;
- completed download and newly opened tab; and
- matching network response.

Transient postconditions such as a network response, download, or popup are
armed before the action that causes them, so a fast event cannot occur in the
gap between clicking and waiting. Retried read actions require an idempotency key
and a bounded postcondition. Externally mutating actions require the same
declarations but are never automatically retried: their pre-armed postcondition
is used only to reconcile a lost acknowledgement after the one allowed attempt.
A recipe carrying an external write must also carry an `assert` step after it;
see "Write receipts" below for why a receipt cannot stand in for that read-back.
Before that attempt, the runner probes durable desired state; if it already
holds, the step reports zero attempts and issues no write. A generic page-ready
condition and transient network/download/tab events cannot prove a rerun safe,
so they are rejected for external writes. A recipe declaring an external write
must itself carry `risk: external_write`.

For message-like workflows, keep finding/capturing, preparing a draft, and
sending the current draft as separate recipes. Guard draft preparation with an
exact `element.value` equal to the requested text. Guard delivery with the same
composer's exact empty value, so rerunning after a successful send performs zero
actuations. A recipe remains browser mechanics rather than standing permission:
the caller must authorize each external send or event creation.

This is deliberately at-most-one actuation per run, not a claim of distributed
exactly-once delivery. The idempotency key documents the logical operation and
the durable page-state preflight prevents ordinary rerun duplication, but `brw`
does not own the site's transaction database. High-consequence providers should
also use the site's own backend idempotency key or an outer durable run ledger,
and an orchestrator must not blindly retry an ambiguous failed run.

A zero-attempt result is a statement about current UI state, not a receipt for a
past remote transaction. This matters especially for negative postconditions
such as `element.hidden` or `text.absent`: the marker may be absent because a
prior run succeeded or because the caller opened the wrong same-origin view.
Authors should prefer a positive, value-specific completion marker and an exact
page-context assertion where the site exposes them. Callers must inspect the
reported attempt count and must not claim that a write occurred when it is zero.

Key presses also have a semantic target. The runner focuses the freshly resolved
element immediately before pressing rather than trusting whichever control
happens to own ambient focus. Screenshot steps may likewise use a semantic
target; persisted observation refs are rejected everywhere in the recipe ABI.

Runtime inputs are declared by name and expanded only while running. Literal
credentials, undeclared templates, raw refs, wildcard origins, unbounded waits,
unbounded video, unknown fields, and oversized recipes are rejected. Inputs are
not returned in run results or usage logs.

A credential is not an input. A recipe names it as `secret://<name>`, which is
legal only as the *entire* value of that recipe's own `fill` or `type` step; the
caller neither supplies the value nor receives it, and an input that expands to
something shaped like a reference is typed literally. The value is resolved at
the step by an operator-installed plugin holding `credential.read`, so a recipe
file never contains one. See [plugins and capabilities](plugins.md).

## Compiling a recipe from a trace

Hand-authoring a recipe means reading a trace and writing the schema by hand.
`brwctl recipe draft` has two modes for turning a recorded run into one.

Without `--plan` it produces a skeleton: the mechanical parts filled in, a
`TODO` marker everywhere a human must decide, and `brwctl recipe validate`
failing until every marker is resolved.

With `--plan` it compiles instead. The compiler needs more than a bare trace —
it needs the page state observed either side of every action — and in exchange
it emits a recipe that parses and validates with no markers left:

```sh
brwctl recipe draft --from-trace /absolute/private/trace.json \
  --plan /absolute/private/plan.json \
  --out /absolute/private/draft.json
```

The plan carries what a recording cannot: the id, version, name, description,
intents, the exact origin allowlist, the risk level, and which recorded steps
commit an external write. None of it is inferred. An origin list derived from
the trace would make the cross-origin check below check nothing, and a risk
level guessed from a button's label would be a guess stamped on the field that
exists to stop guessing. An unknown key in the plan is an error, so a
misspelled `writes` cannot silently compile a write as a read. The trace is
decoded the same way, accepting the fields brw's recorder emits and nothing
else, because among the keys the compiler reads is `redacted` — the strongest of
the three credential signals — and one spelled differently would be absorbed in
silence.

What the compiler does:

- Re-derives every element identity from the role and accessible name brw
  recorded, ranks it against the observation the action was aimed at, and
  accepts it only when it names exactly one element and that element is the one
  the recording acted on. A test id or href fragment is added when a name alone
  matches more than one. Refs, CSS selectors and coordinates never appear in the
  output.
- Turns each typed value into a declared, required runtime input. The text that
  was typed during the recording stays in the recording.
- Infers each step's postcondition from the observation taken after it: the
  resulting URL, an element the step introduced, or a completed download. Each
  gets a following assertion in the `brw_assert` vocabulary — `url`,
  `element_count`, `download` — except a download the recording captured neither
  a size nor a digest for. A step whose after-observation shows no change at all
  — same URL, no new element, no new download — compiles with **no**
  postcondition, and the review body says `postcondition none` for it.
- Drops the query string and fragment from a recorded href before using it to
  identify an element. That is where a session id or a one-time token lives, and
  matching on it would both publish it and stop the recipe replaying once it
  expired. A navigation URL keeps its query — it is what the step navigates to —
  and the review body prints it on its own `url_query` line so a reviewer sees
  it before publishing.
- Composes an external write's `idempotency_key` from the recipe id, the step id
  and the inputs declared up to that step, so two runs with different inputs are
  two transactions rather than one.
- Prints a diff-able review body — one fact per line, fixed order, no map
  iteration — so recompiling an unchanged trace produces byte-identical text
  and a changed trace produces a diff the size of the change.

What stops compilation dead, naming the trace step and the reason:

- a coordinate-driven action such as `brw_click_xy`;
- a write into a password or credential field, whether brw redacted the value,
  the snapshot marked the field sensitive, or the accessible name says so —
  checked for every action that carries typed characters into the page, key
  presses included;
- a `press` whose key is a literal character rather than a key that issues a
  command, because a value entered one keystroke at a time is the value, spelled
  differently. A named key (`Enter`, `Tab`, an arrow, `F5`) and a Ctrl, Alt or
  Meta chord (`ctrl+a`, `meta+s`) are commands — Chrome attaches no text to an
  accelerator. `shift+a` is not: Shift is the modifier that still types, so it
  is the letter `A`;
- a target that matches more than one element in the recorded observation;
- a navigation to an origin the plan does not declare;
- a step whose post-action observation is empty; and
- an action with no deterministic recipe equivalent, or one that failed when
  recorded.

Nothing is written until the whole trace has compiled, so a refused trace leaves
no file to clean up. `--out` is refused unless it is outside every Git checkout,
which includes this repository. `--publish` sends the draft and its review body
to the private provider's write API at `POST /v1/recipes/drafts`, configured
through `BRW_RECIPE_PROVIDER_URL` and `BRW_RECIPE_PROVIDER_TOKEN`; the
acknowledgement must name the same id, version and digest or the publish fails.
The file at `--out` is created with `O_EXCL`, so a symlink already sitting at
that path — dangling or not — is refused rather than followed, and an existing
file is never overwritten.

## Write receipts

A recipe that submits a form and then loses its daemon cannot tell, on restart,
whether the remote side committed. Retrying blind double-submits.

When the recipe provider is an HTTP provider, the runner records external writes
with it. A local directory provider gets no receipts: a receipt exists to be
read after the daemon that wrote it died, and a file on that same machine
answers nothing. A recipe that declares a mechanism the runner cannot honour is
refused by name rather than run with the mechanism quietly switched off.

The idempotency key is `sha256` over the recipe's content digest, the step id,
the exact origin, and the normalized declared inputs. The normalization is
fixed and written down in `ReceiptKey`, because a key that cannot be reproduced
after a restart produces a receipt nobody can find, which reads exactly like a
write that never happened: only declared inputs take part, names are sorted by
byte order, an unsupplied input is encoded as absent and is a different key from
an empty one, CRLF and lone CR become LF, every field is length-prefixed so no
field's bytes read as part of the next, and nothing else is altered — no
trimming, no case folding. The key depends on no clock, host or session.

The sequence around one write is: probe the durable postcondition and skip the
write entirely if it already holds; look the key up; record an in-flight receipt
**before** dispatching; and record completion **only** after the declared
postcondition passes. A run that dies in between leaves an in-flight receipt and
no completion.

`POST /v1/receipts/begin` answers with `created`, and that flag is the only
compare-and-set in the mechanism. Two runners against one shared store can both
miss on lookup, so a record `begin` did not create belongs to somebody else's
dispatch and is handled exactly as a lookup hit would be. A provider that omits
the flag reads as "not created", which refuses the write rather than duplicating
it.

A rerun that finds an in-flight receipt does not re-submit, ever. It runs the
recipe's declared verification step against live remote state. If that confirms
the write landed, the receipt is committed and no browser action is issued. If it
does not, the run fails saying so, and the receipt is left in flight for a human.

Because a receipt is written on brw's side of the network, it is never proof the
remote transaction committed — a process killed a millisecond after the request
left writes the same receipt either way. So a recipe containing an
`external_write` step is refused unless an `assert` step follows it. That
assertion is the read-back, and it is what an interrupted rerun consults. A
compiled write carries two assertions — the evidence the compiler inferred from
the recording, and the read-back the operator declared — so the declared one
carries `verifies: "<write step id>"` and the tag is what the rerun reads. Their
order is an emission detail. A write followed by several `assert` steps none of
which is tagged is refused: nothing says which one reads the write back.

Where the site exposes its own duplicate suppression as a form nonce, the step
declares it with a `site_idempotency` object naming `kind: form_nonce` and the
semantic target of the token field. brw reads that field before dispatch and
records a digest of the token on the receipt — equality is all the comparison
needs, and the receipt is held by a third party — and refuses to dispatch at all
if the field cannot be read or is empty. The token is recorded, never acted on:
a page still carrying the token an interrupted attempt used is equally consistent
with the site having consumed it and with the token being a per-session CSRF
value the site would accept again, and brw cannot tell those apart from outside,
so an unchanged token is not grounds to dispatch again. The refusal reports the
comparison so a human can use it. Only `form_nonce` exists: a request header is
the same idea, but a recipe drives a page rather than an HTTP client, so
declaring one would name a mechanism that never runs.

The receipt endpoints extend the provider contract with `POST /v1/receipts/lookup`,
`POST /v1/receipts/begin` and `POST /v1/receipts/commit`. As with search and
fetch, every reply is revalidated: a receipt answering about another key, step or
origin is refused rather than trusted.

A deployment whose provider is a local directory gets no receipts at all: a
receipt has to outlive the daemon that wrote it, and a file on the machine that
crashed answers nothing. The daemon says so at startup, naming the flag that
configures a provider that can hold them, and refuses any recipe declaring a
site nonce rather than running it with the declaration switched off.

## Timers, page events, cron, and webhooks

Timers and browser/page events belong inside the deterministic recipe because
they are part of one bounded browser transaction. Long-lived scheduling does
not. Use MCPlexer, cron, a workflow engine, or a webhook receiver to decide
*when* to search/run a recipe. That outer system owns retry policy,
authorization, calendars, notifications, and audit. It invokes `brw`; `brw`
does not keep an invisible scheduler alive beside a browser profile.

## Artifacts instead of context flooding

`brw_artifact_capture` stores bytes on the machine that owns the browser and
returns compact metadata only:

- `text`: the readable page body;
- `semantic_json`: a full semantic snapshot;
- `screenshot`: viewport or one semantic ref;
- `pdf`: Chrome's print-to-PDF output;
- `download`: a copy of one completed browser download; or
- `video`: a bounded WebM assembled from streamed screenshots on the browser
  host (requires `ffmpeg`, at most 30 seconds and 300 frames). Service-manager
  installs also probe the standard Homebrew/system locations; set the absolute
  `BRW_FFMPEG_PATH` only for a non-standard installation.

Video owns a hard operation budget equal to the requested duration plus two to
five seconds of bounded startup/mux/persistence headroom (35 seconds maximum).
The encoder is terminated on timeout, temporary files are removed, and stderr
retains only a 4 KiB tail with at most 1,000 bytes surfaced in an error.

Recipe-scoped captures additionally bind to Chrome's exact committed main
document, not merely an allowlisted URL or origin. Direct CDP uses the main
frame loader identifier; the extension uses `webNavigation.documentId`. Both
pair it with a monotonic replacement-document epoch, so a reload, navigation
between two allowed origins, same-origin replacement, or A → B → back-to-A
BFCache round trip rejects the capture and removes any bytes already committed.
`pushState`, `replaceState`, and hash-only SPA changes retain the same identity.
An extension-worker restart also invalidates an in-flight capture. This guard is
required only for deterministic recipes; manual capture remains compatible and
does not pay for the identity probes.

Use `brw_artifact_search` to retrieve only matching text excerpts,
`brw_artifact_read` to request a bounded byte window, `brw_artifact_info` for
metadata, and `brw_artifact_delete` as soon as the data is no longer useful.
Binary data becomes base64 only on an explicit read; capture itself never sends
the payload through MCP. This boundary also holds through an upstream HTTP/SSH
proxy: capture, storage, search, and video encoding happen on the browser host.
The upstream client uses fixed-path, bounded JSON `POST` operations for artifact
info, reads, searches, and deletion, so opaque handles and literal search terms
do not enter reverse-proxy URL/access logs. The older ID-in-path `GET`/`DELETE`
routes remain compatibility-only and return `Deprecation` plus successor-link
headers; new integrations should not use them.

`brw_artifact_search` is an in-artifact literal search: it requires both an
`artifact_id` and a query. It is not a catalog search. Keep the handle returned
by capture in the calling task/workflow state. There is deliberately no global
artifact-listing tool: artifacts are shared by callers authorized to the same
daemon, so enumerating opaque IDs would unnecessarily disclose other sessions'
captures. If durable cataloging is required, store handles and non-sensitive
labels in the private outer workflow system rather than in brw or source code.

The default store is an owner-only user cache outside source checkouts,
namespaced by the daemon's runtime/profile identity so two local profiles do not
silently share handles or quota. Defaults are 128 MiB per artifact, 2 GiB total,
and 24-hour retention. Configure them with `--artifact-dir`,
`--artifact-max-mb`, `--artifact-total-mb`, and `--artifact-ttl`, or disable the
capability with `--artifact-dir off`. Writes are atomic, files are `0600`, IDs
are random and path-confined, MIME/kind pairs are checked, SHA-256 is recorded,
expired entries are denied immediately and physically purged at startup or by
an idle-daemon janitor (at most five minutes at the default policy),
crash-stranded partial commits are reconciled, and reads are hard capped at
1 MiB per call. Broad roots such as a home, cache, configuration,
or temporary directory are rejected: give the store a dedicated subdirectory.

Identical payloads are stored once. A second capture of the same bytes adopts
the copy already on disk, so repeating a capture costs a handle rather than the
payload again. The handles stay separate objects: each keeps its own random id,
its own metadata and its own expiry, and deleting one does not disturb the other.
Ids are random 128-bit values either way, so dedup does not make the store answer
"have these bytes been captured?" to anyone who can guess a payload.

The quota charges by on-disk identity rather than by digest, so a copy that could
not be shared — a filesystem without hard links, or a store written before dedup
existed — is charged for what it really occupies, as is a blob a crash stranded
without its metadata. A capture the store can adopt is admitted even when the
quota has no room for a second copy, because adopting it costs nothing. Deciding
that needs the payload hashed, so a capture is written to a staging file before
the quota rules on it: the root can transiently hold one `--artifact-max-mb` more
than `--artifact-total-mb`.

A rendered PDF is pulled from the browser as a stream (`Page.printToPDF` with
`transferMode=ReturnAsStream`, then chunked `IO.read`) and written straight to
the store, so the daemon holds one chunk rather than the whole document. Driving
a 50 MiB payload through the capture path from a synthetic in-process source
measured 61 KiB of Go heap growth, against 52 MiB for the buffered path on the
same payload; the CDP transport itself is exercised chunk by chunk against real
Chrome rather than at that size. Each CDP round trip carries its own deadline,
so a long transfer is bounded per read rather than by one wall clock covering
the render and the whole document, and the browser-side stream handle is
released even when the capture was cancelled or timed out. A captured download
is streamed from its completed file for the same reason: the staged file is the
stream, and the daemon holds one `io.Copy` buffer rather than the payload. Both
first-party transports implement the streaming capability; an upstream
controller that cannot stream still produces a PDF through the buffered path.

There is a CDP stream handle for a download's bytes, and brw deliberately does
not use it. With `Fetch` enabled at the response stage,
`Fetch.takeResponseBodyAsStream` does hand back the body of a request that was
about to become a download — including a navigation turned into one by
`Content-Disposition`, and an `<a download>` click, both of which arrive as
document requests — and it streams, delivering its first chunk long before the
server has finished writing. What it does not do is leave the download intact.
Taking the stream means the request can no longer be continued, so the browser
never writes its copy and the transfer the rest of brw is built on never
happens. The download manager either never hears about it — a navigation turned
into a download raises no `Browser.download*` event at all — or records a
cancellation at zero bytes, which is what an `<a download>` click produces.
Either way there is no completed entry: no guid to select by, no suggested
filename, no progress, and nothing for `brw_wait_for {condition:"download"}` to
resolve on.

Nor would it save a copy. brw's own staged file is deleted once the artifact is
persisted, so the disk round trip is transient and already bounded, and the
stream would trade it for the download lifecycle — on direct CDP only. The
extension bridge intercepts with `declarativeNetRequest`, which is never handed
a response body at all, and reaching CDP's `Fetch` domain there would mean
holding interception across the `chrome.debugger` attach and detach that wraps
each operation, which is the same reason `brw_route` `fulfill` is direct-CDP
only. The measurements are in
`TestFetchResponseStageStreamsADownloadInsteadOfTheBrowserTakingIt`.

Artifacts can be encrypted at rest with `--artifact-encrypt off|recipe|all` and
an operator key file given by `--artifact-key-file`. The default is off, and
`recipe` — encrypt what a private-recipe run captured — is the setting most
deployments want. The key is never generated by brw and must live outside the
artifact root, owner-readable only; a key stored next to the ciphertext protects
nothing and is refused. Blobs are chunked AES-256-GCM with a per-blob key
derived from the store key, so a bounded read decrypts only its window and a
truncated or modified blob fails authentication rather than returning a prefix.
Encrypted blobs are deliberately excluded from dedup: recognising a duplicate
would mean producing identical ciphertext for identical plaintext, which is the
equivalence encrypting a sensitive capture exists to hide. For the same reason
an encrypted artifact records no plaintext SHA-256 — `brw_artifact_info` returns
none for it — because a digest sitting unencrypted beside the ciphertext would
let anyone who can read the store confirm a payload they can guess, and see that
two encrypted artifacts hold the same bytes. The AEAD already authenticates the
blob chunk by chunk, so the digest bought nothing it does not already provide.

## Failure evidence bundles

A failed recipe step returns an error string, and diagnosing it otherwise means
running the recipe again with tracing on and hoping the failure reproduces. With
`--artifact-failure-bundles recipe` (default `off`) and a recipe that sets
`capture_on_failure`, brw collects the evidence in one pass on the page that has
just failed: the redacted action trace, a console summary, bounded network
metadata, the semantic snapshot, and a screenshot. Each part is a separate
artifact with its own expiry, and the failed call returns only the manifest
artifact id. It arrives as `failure_bundle_artifact_id` on the run result — which
a failed call returns alongside its error on every transport — and is named in
the error text as well.

Read the manifest with `brw_artifact_read`; it lists artifact ids, kinds, sizes
and expiries, never payloads. Parts that could not be collected are listed too,
with the class of failure, so a bundle missing its screenshot cannot be mistaken
for a page that had none. Every part whose bytes come out of the loaded document
— the console summary, the network metadata, the snapshot and the screenshot —
is checked against the recipe's origin allowlist before it is collected, so a
run that navigated away before failing records those parts as uncollected rather
than capturing the page it ended up on. The action trace is brw's own record of
what it did and is always collected. Network metadata drops request and response
bodies outright, removes every header on the shared credential denylist
(recording only that such a header was present), and blanks a credential carried
in a URL's userinfo or query string. The collection runs on its own 20-second
budget detached from the run's deadline, because a run that timed out is a
common reason to want the evidence.

Evidence retention is `--artifact-failure-bundle-ttl` (default one hour,
clamped to `--artifact-ttl`), shorter than an ordinary capture because it is the
most sensitive thing the store holds. Under `--artifact-encrypt recipe` the
whole bundle from a private-recipe run is encrypted at rest, the manifest
included: it carries the failing step's error text, so a bundle whose parts are
encrypted and whose manifest is not is not an encrypted bundle.

Both switches are off by default and both must agree: collection costs a browser
round trip per part on a page that has just misbehaved, and the daemon policy
overrides a recipe that asks for it.

Give every daemon its own artifact root. The store's quota and maintenance lock
is process-local; pointing two daemon processes at the same explicit
`--artifact-dir` is unsupported and can exceed the configured aggregate quota.

The 128 MiB per-artifact value is a store limit, not an extension-transport
promise. Direct CDP keeps raw bytes on the browser host. The installed-profile
extension bridge keeps each WebSocket frame below 4 MiB and reassembles a
maximum 64 MiB serialized response (roughly 48 MiB for base64-backed binary
data). A larger extension response fails deterministically with
`BRW_EXTENSION_RESPONSE_TOO_LARGE` and leaves the bridge usable. Use a smaller
capture or a direct-CDP profile for larger material; raising this limit would
otherwise require several hundred MiB of transient base64/JSON allocations.

For deterministic download recipes, a completed file must be attributable to
the exact source tab and uniquely identified by GUID or filename. On the
extension transport this attribution depends on Chromium emitting a download
event while brw's debugger is attached; unobserved or ambiguous downloads stay
visible to manual callers but fail closed for recipes. Chrome does not expose
browser-level download routing through `chrome.debugger`, so an extension-backed
profile cannot safely redirect just one recipe download into a daemon-owned
directory. It uses Chrome's configured destination and never deletes the user's
original. Direct CDP does support browser-level routing and keeps deterministic
downloads in an owner-only brw staging directory; managed copies are removed
only after artifact persistence succeeds.

Manual capture of a pre-existing file still preserves the user's original and
therefore has to open the browser-reported path. On macOS, Desktop, Documents,
and Downloads are protected by Files & Folders consent. A background LaunchAgent
may wait inside the OS `open` call when that consent is unavailable. brw bounds
this path to one three-second attempt: later download captures fail immediately
instead of accumulating blocked threads, while every other browser/artifact
operation remains usable. Grant the installed `brwd` binary Downloads access in
System Settings > Privacy & Security > Files & Folders, or use a direct-CDP
profile whose deterministic downloads stay in private staging, before capturing
files from `~/Downloads`.

An artifact ID is an opaque handle, not a separate authorization boundary:
any caller authorized to use that `brwd` control plane can request it. Keep the
HTTP listener on loopback or behind authenticated transport. Screenshots, PDFs,
downloads, and video may visibly contain secrets. The `redaction` field records
redaction provenance; it does not perform pixel redaction by itself.

`pdf` means Chrome's rendered print output. It is useful evidence of page state,
but it is not a vendor-issued original. Workflows that require accounting or
legal source evidence should capture the site's own completed `download` (or an
unaltered source message/file) and fail as source-unavailable when none exists;
they must not reconstruct a substitute document.

Video capacity is checked atomically when the encoded file enters the store.
There is not yet an early reserve/consume transaction, so a concurrent writer
can win the remaining quota after encoding has begun and cause that work to be
discarded. A deliberately broken third-party controller that ignores context
cancellation can also strand one screenshot goroutine, although the request,
encoder process, and temporary file remain bounded.

## Regression baselines

`brw_diff` answers "did my click change the page" against a mark taken seconds
ago. `brw_baseline` answers "did this page change since the last release", which
needs three things a live comparison has none of.

**A key that says what the stored capture was captured under.** A baseline is
keyed by the recipe's content digest, the step index, and an environment
fingerprint the daemon measures itself: browser build, viewport, device pixel
ratio, locale and the browser host's operating system. Editing a recipe changes
its digest and orphans its baselines rather than silently comparing new
behaviour against old. Running on a different display reports
`status: "environment_mismatch"` naming the fields that moved
(`device_pixel_ratio 1 -> 2`), and compares no pixels — a diff that is real,
meaningless and unactionable is how a visual gate gets switched off.

**Two comparisons, because pixels miss things.** The visual half counts moved
pixels with `pixel_tolerance` (a fraction of compared pixels),
`channel_tolerance` (per-channel slack that absorbs anti-aliasing) and
`ignore_regions` — named rectangles in CSS pixels for the clock, the avatar and
the ad slot, reported back by name so you can see which exclusion swallowed a
change. CSS pixels are not capture pixels: every transport clip-captures the
viewport at `min(1, 800/viewport_width)` on top of the device pixel ratio, so a
rectangle is placed by the capture's own width divided by the viewport width in
the key — measured, not assumed to be the DPR, which on a 1400px viewport is
wrong by a factor of 1.75. A region that lands off the capture excluded nothing
and is reported under `regions_outside_capture` rather than counted as applied.
Regions recorded with a baseline keep applying to every later check, so a region
that ends up covering the whole capture fails with "the visual half compared
nothing" rather than passing: a comparison that covered no pixels is not a pass. The structural half
diffs the page's ARIA structure, role and accessible name, with presentational
wrappers flattened away. It carries no geometry: a button that loses its
`aria-label` renders identically and fails the structural check, while a font
bump or a colour change moves pixels and leaves the structure alone.

**An update that is never implicit.** `action: "check"` writes nothing, ever,
including on a passing run. `action: "update"` is the only thing that records or
replaces a baseline, and it reports the differences it accepted.

Both transports work. The ARIA structure is computed in the page rather than
read from a browser-level Accessibility domain the extension bridge does not
have, and the viewport capture — which every transport takes as JPEG for wire
size — is re-encoded losslessly before it is stored or compared, so a baseline
directory holds the PNG its file name claims.

```sh
brwd --baseline-root /var/lib/brw/baselines   # auto uses the user cache; off (default) disables
```

**Where a baseline lands is decided by the recipe and the page, not by the
caller.** A baseline of a signed-in page is a screenshot of private content, and
the private provider already holds the recipe that reached the page. So brw asks
the configured provider whether it owns the recipe the digest pins, and if it
does, the baseline is stored with the provider. `--baseline-root` keeps the
rest: public fixtures, and every recipe no provider claims. Each answer names
the destination it used in `stored_in`, and a daemon with a provider needs no
local root to gate that provider's own recipes.

`recipe_digest` is a caller argument, so it does not decide this on its own. An
agent on a page a private recipe reached can pass any well-formed digest the
provider does not own, and routing on the digest alone would write that page's
screenshot and its ARIA names — which carry its text — to the local root. brw
therefore asks about both in one question: the digest, and the origin the tab is
showing. A provider that has a recipe for that origin and does not own the
digest is a refusal, not a destination — brw does not know which of its recipes
the capture belongs to and will not guess by keeping it locally. Only the origin
crosses to the provider; the path and query of a signed-in page never do.

A daemon started with `--upstream-http` runs `brw_baseline` itself while the
provider lives on the browser host. It asks the host `/api/baselines/route` —
two booleans, no recipe — and refuses by name anything that belongs with the
provider rather than writing it to its own `--baseline-root`. Public fixtures
still gate there. Take a private recipe's baselines on the browser-host daemon.

Both shipped providers answer the routing question and hold baselines. The
directory provider keeps them under a
reserved `baselines/` subdirectory of the recipe root, which recipe discovery
skips — a stored baseline is a `.json` file, and every other `.json` file under
that root is parsed as a recipe. The name is reserved: a root whose `baselines/`
holds anything but baseline records is refused at load with the file named,
rather than serving a corpus with those recipes silently missing. The name is
not the whole check — each `.json` file under the reserved tree is read and
required to decode as a baseline record, because a recipe saved as
`baseline.json` would otherwise pass on its name and be exactly the silently
undiscovered recipe this refuses. The HTTPS provider `POST`s them:

| route | body |
| --- | --- |
| `/v1/baselines/owner` | `{recipe_digest, origin?}` → `{owns, owns_origin}` — both required; an absent field is refused, not read as `false` |
| `/v1/baselines/put` | the baseline → `{stored}` |
| `/v1/baselines/fetch` | `{recipe_digest, step_index, environment_fingerprint}` → `{found, baseline}` |
| `/v1/baselines/environments` | `{recipe_digest, step_index}` → `{environments}` |
| `/v1/baselines/delete` | `{recipe_digest, step_index, environment_fingerprint}` → `{deleted}` |

A baseline on the wire is the recipe/step key, the environment fingerprint
alongside the environment it was computed from, the screenshot as base64 PNG,
the ARIA tree and the ignore regions. The image bound is derived from the 8 MiB
provider response cap rather than picked, because base64 inflates by 4/3 and a
capture the put accepts that no fetch can return is a baseline stored and never
usable again. The fingerprint is re-derived and the
image re-decoded on the way back, and a record that does not describe the key
that was asked for is refused: the provider is authenticated, not trusted, and a
baseline from another environment would be compared against this page and
reported as a regression in it.

If the provider cannot be asked, the check fails. It does not fall back to the
local root — that fallback would put a screenshot of a private page there at
exactly the moment the provider is unreachable to say it is private.

The local store refuses a root inside a Git working tree and writes an
ignore-everything file into the root it does accept, so baselines cannot be
committed to this repository or to yours.

## Verification

`task test-functional` launches a real headless browser and a private recipe
directory created under a temporary path. It exercises semantic search,
id/version/digest pinning, a guarded write, a pre-armed event, a timer, secret
input non-disclosure, an idempotent zero-write rerun, text search, screenshot,
PDF, a real completed-download-to-artifact handoff, and (when installed) video.
The temporary recipes and every artifact are deleted at exit.

One recorded flow is replayed against a real browser rather than a page model:
`TestCompiledRecipeReplaysTwiceAgainstRealChromeOnASignedInFixture` records a
login, a search and a report open against an httptest site that refuses an
unauthenticated request, compiles the trace, and replays the compiled recipe
twice on browser profiles with no cookies — so each run signs itself in. A third
replay runs against the same fixture issuing a session cookie the browser
discards, and must fail: without that case, pages that render for anyone would
replay just as green and the other two would prove nothing about the session.

The normal test suite also covers directory permissions, repository/symlink
escape attempts, strict parsing, provider substitution, ambiguous targets,
origin drift, cross-allowed-origin and same-origin document replacement, SPA
continuity, lost acknowledgements, quotas, expiry, traversal, concurrent writes,
bounded reads, raw screenshot transport, and fuzz inputs. Synthetic
benchmarks contain no operational recipe corpus.
