# Private recipe authoring and repair

Use this reference after a reusable workflow succeeds or a selected recipe
fails because the site's stable structure changed. The outcome is a validated,
immutable recipe version in the configured private provider—not a recipe body
inside a source repository, bundled skill, or usage log. Until a future
trace-to-recipe compiler exists, the authorized authoring agent necessarily sees
the draft in its private working context; do not paste that draft into public
issues, chat, commits, or other unrelated systems.

## Search before rebuilding

Search with the natural-language task and, when known, the exact page origin.
Choose a result only when its description, origin, and risk match the request.
Pass the returned `id`, `version`, and `digest` unchanged to `brw_recipe_run`.
Recipe search intentionally returns metadata, not executable steps.

If no precise match exists, complete the workflow normally. Keep the tab ID
explicit and use semantic reads/finds rather than coordinates. A successful,
stable flow is the evidence from which to author a recipe.

## When to promote a workflow

Create a recipe when all of these are true:

- the same intent is reasonably likely to recur;
- the steps are deterministic on one or more exact HTTPS origins;
- every action can use role plus accessible name, test ID, or href fragment;
- required variable values can be declared as runtime inputs; and
- success or failure can be checked with bounded events or durable page state.

Do not promote exploratory browsing, an unresolved failure, coordinate-only
interaction, CAPTCHA/MFA handling, an approval decision, or a flow whose effect
cannot be safely classified. A recipe automates browser mechanics; it does not
grant authority the current task did not provide.

## Build the draft

Use `brw_trace({format:"entries"})` as evidence, then inspect the current page
with `brw_read`, `brw_find`, or a bounded semantic artifact. Do not copy the
trace's ephemeral refs into a recipe. Translate each target to stable semantic
identity and resolve it again at runtime.

A schema-v1 recipe declares:

- `id`: a namespaced lowercase identifier;
- `version`: semantic `major.minor.patch` identity;
- `name`, `description`, and one to 32 natural-language `intents`;
- one to 32 exact allowed `origins`;
- `risk`: `read_only` or `external_write`;
- optional declared `inputs`, marking secrets without embedding their values;
- one to 500 ordered `steps`; and
- optional non-secret metadata.

Supported actions are `click`, `fill`, `type`, `select`, `press`,
`navigate_to`, `wait_event`, `timer`, and `capture`. Targets require a role plus
one stable selector: exact/contained accessible name, test ID, or href fragment.
The selector fields (but not `role`) may contain `${input:name}` for a declared
runtime value, which lets one reviewed workflow find a named person,
conversation, or record while retaining exact-one-match fail-closed behavior.
Never put a credential, cookie, token, session value, or customer data in the
recipe body.

Every browser actuation (`click`, `fill`, `type`, `select`, `press`, and
`navigate_to`) must explicitly declare `effect: read` or
`effect: external_write`. Omission is rejected before execution; the recipe's
top-level risk must equal or exceed its strongest step effect.

Supported bounded events cover page readiness, URL match, text present/absent,
element visible/hidden, exact `element.value` (including the empty string),
composer-scoped `element.value_contains`, completed downloads, newly opened
tabs, and network responses. A transient
download/tab/network event must be the postcondition on the action that causes
it so brw can arm observation first. Timers are at most 60 seconds; event waits
are at most 120 seconds.

When a workflow needs the downloaded bytes, put a matching `capture` step
immediately after `download.completed`. Direct-CDP profiles stage deterministic
downloads in brw's owner-only cache and remove the managed copy only after
artifact persistence. Chrome's extension debugger cannot redirect downloads;
extension profiles preserve the user's original in Chrome's configured folder
and artifact capture may require host Files & Folders permission on macOS. Never
make a recipe depend on a pre-existing same-name file: only the freshly armed,
exact-tab download may satisfy the event or feed the capture.

For an `external_write` action, declare an idempotency key and a durable,
state-specific postcondition. It receives one actuation attempt. A generic
page-ready state or transient event cannot prove a write safe on rerun. Do not
execute a write merely to test the recipe; use an approved sandbox or an
already-satisfied durable state that exercises the zero-write preflight.

An `attempts: 0` outcome means only that the current UI already matched the
postcondition. It is not a receipt proving that an earlier remote write took
place. Negative conditions such as `element.hidden` and `text.absent` are
especially ambiguous on the wrong same-origin view; prefer a positive,
value-specific completion marker plus an exact page-context assertion when the
site provides them, and never report a mutation solely from a zero-attempt
negative condition.

Treat message composition and delivery as separate workflows. A
prepare-draft recipe may fill a composer and prove its exact value; a
send-current-draft recipe may click Send or press the targeted Enter key and
prove the same composer is exactly empty. This split permits inspection before
delivery and prevents a stored recipe from turning prior authorization into
standing permission. It does not prove provider-side exactly-once delivery, so
never blindly retry an ambiguous failed send.

## Validate and install locally

Create the draft in an owner-only temporary/private path outside every Git
checkout. Validate with the installed parser:

```sh
brwctl recipe validate --file /absolute/private/draft.json
```

Install it atomically into the daemon's configured private directory:

```sh
brwctl recipe install --file /absolute/private/draft.json \
  --root "$BRW_RECIPE_ROOT"
```

When `--root` and `BRW_RECIPE_ROOT` are both omitted, `brwctl` uses the
platform user-config directory under `brw/private-recipes`. The directory is
forced to owner-only mode, installed files are `0600`, symlinks and Git
checkouts are rejected, and an existing `id + version` cannot be overwritten
with different content. The live local provider detects a newly installed
version without restarting the browser daemon.

After installation, search again using the intended phrasing and origin. Check
that the returned metadata is the expected version/digest. For read-only
recipes, run a representative test and inspect only bounded output or artifact
metadata. Delete disposable artifacts and close tabs opened for validation.

For an HTTPS recipe provider, use its separately authorized authoring API or
client instead of `brwctl recipe install`. The provider should keep immutable
versions, expose only the newest approved version in search, preserve pinned old
versions for in-flight runs, and maintain its own authorization and audit log.

## Repair a failing recipe

Treat the failure as classified evidence, not permission to retry:

1. Record the recipe ID/version/digest, failing step, current origin, and the
   bounded error. Do not record secrets or full private page content.
2. Decide whether this is deterministic drift. Changed accessible names,
   stable attributes, navigation, or completion state are recipe drift. Auth
   expiry, missing permission, outage, rate limit, CAPTCHA, and wrong input are
   operational failures and must not rewrite the recipe.
3. Inspect the affected page and re-resolve the intended element semantically.
   Fail closed when several candidates remain.
4. Copy the private source version, make the smallest justified change, and
   increment the semantic version. Use a patch for a compatible locator or
   assertion repair, minor for a backward-compatible workflow extension, and
   major when meaning, required inputs, origin, or risk changes materially.
5. Validate and install the new version. Never edit or replace the old file;
   its digest may already be pinned by another run.
6. Test the smallest safe path. Never repeat an ambiguous external write. Use
   durable-state preflight, an approved sandbox, or stop before actuation.
7. Search again and verify that the new version is the single searchable head.
   The prior version remains fetchable only by its exact pinned identity.

If the repaired recipe fails again for a different reason, repeat diagnosis.
Do not accumulate blind retries, broad name matching, wildcard origins, sleeps
in place of observable events, or weakened postconditions merely to make a test
pass.

## Private boundary

The repository and global skill contain only the engine and these authoring
instructions. Operational recipe bodies belong in an owner-only local store or
an authenticated private provider. Skills may prompt discovery and maintenance,
but they are not the recipe corpus. Long-running schedules, webhook triggers,
human approvals, secrets, and provider authorization remain outside brw.
