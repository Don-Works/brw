# Plugins and capabilities

brw has one extension point: an operator-installed plugin, described by a JSON
manifest, that the daemon may call for a narrowly defined job. A plugin is not a
way to add browser verbs, and it is not a way to change what brw already
refuses. The capability list below is a closed allowlist. Everything outside it
is refused at load, including the capabilities other agent-browser products
ship.

Two capabilities are granted. `credential.read` exists because brw is not a
secret store and will not become one, so a fresh browser context has no way to
log in: a signed-in recipe works only because the profile was already signed in
by a human. That capability closes the gap without brw holding a vault — the
value is fetched from the operator's own secret manager at the moment of use and
written into one form field.

`browser.provider` exists because the browser does not have to be on this
machine. A plugin answers with a CDP websocket URL and brw drives what is on the
other end of it, keeping every guard it applies to a local browser. brw ships
one backend kind rather than a list of named cloud vendors, and the capability
carries the rest.

## Manifest

One manifest per file, `*.json`, directly inside the plugin directory
(`brwd --plugin-dir <dir>`). Unknown fields are rejected, so a typo is an error
rather than a silently ignored setting.

```json
{
  "schema_version": 1,
  "id": "onepassword.cli",
  "name": "1Password CLI",
  "version": "1.0.0",
  "description": "Reads one field from the signed-in 1Password vault",
  "capabilities": ["credential.read"],
  "credential": {
    "kind": "exec",
    "command": ["/usr/local/bin/op", "read", "op://{reference}"],
    "timeout_ms": 10000
  }
}
```

| field | meaning |
| --- | --- |
| `schema_version` | must be `1` |
| `id` | dotted lowercase, unique across the directory; names the plugin in errors and in `brw plugins` |
| `name`, `description` | shown to an operator, never to a page and never to an agent's page tools |
| `version` | `major.minor.patch` |
| `capabilities` | exact capability names, matched byte for byte |
| `credential` | required if and only if `credential.read` is granted |
| `browser` | required if and only if `browser.provider` is granted |

The `credential` block has two kinds.

`exec` runs a fixed argv. `command[0]` must be an absolute, already-clean path:
a bare `op` is whatever `PATH` resolves at the moment of the call, and the point
of the manifest is that an operator decided what runs. It must also not contain
`{reference}`, which would let the requested name spell a different program than
the one the loader checked. The loader resolves it once and runs the resolved
path at every call, so a symlink moved after startup does not change the
program. Exactly one other argument must contain the token `{reference}`, which
is replaced by the requested reference name. There is no shell: the argv is
passed to `execve` as written, so a reference cannot inject a second command, a
pipe, or a redirect. Zero occurrences of the token is a load error, because a
provider that ignores the reference would answer every request with the same
secret.

`file` reads `<directory>/<reference>`. It is the reference implementation used
by the test suite and by anyone who has no CLI vault. `timeout_ms` applies to it
as well as to `exec`. Each credential file is checked as it is read and refused
if it is readable by group or other. The directory is held to the same write and
ownership rules as the plugin directory, because whoever can write it chooses
the value brw types into a password field. Its READ mode is not checked, so a
shared-readable directory still leaks the NAMES of the credentials in it.

```json
{
  "schema_version": 1,
  "id": "local.files",
  "name": "Local credential files",
  "version": "1.0.0",
  "description": "One file per credential under a 0700 directory",
  "capabilities": ["credential.read"],
  "credential": { "kind": "file", "directory": "/etc/brw/credentials" }
}
```

At most one loaded plugin may hold `credential.read`. Two would make "which
vault answered?" unanswerable from a failure, and a silently shadowed provider
is the kind of ambiguity that ends with the wrong password typed into the right
box.

## Capabilities

### `credential.read` — granted

**Can reach:** one reference name at a time, supplied by the daemon at the
moment a recipe step is dispatched. The provider returns one value.

**Cannot reach through brw:** the page, the DOM, the recipe, the recipe's other
steps, the element the value is typed into, cookies, storage, the trace, the
artifact store, the usage ledger, or any brw HTTP or MCP route. Nothing calls
the provider except the recipe runner, and it calls it with a name and nothing
else.

**What that does not mean.** The list above is what brw *hands* a provider. It
is not a statement about what the provider's process can do, because brw does
not sandbox one: an `exec` provider is a child of the daemon, running as the
daemon's user with the daemon's environment and the daemon's network access. It
can therefore open brw's own HTTP control plane on loopback — which enforces no
authentication — and ask it for cookies, which is exactly what the refusal table
below says `cookies.export` exists to prevent. A capability constrains what brw
gives a plugin; nothing in brw constrains what a plugin process takes. That is
why the trust boundary is the permission on the plugin directory, and why the
loader spends its effort there. See [Trust and sandboxing](#trust-and-sandboxing).

**Where the value goes:** into `Surface.Fill` or `Surface.Type` for that one
step, and nowhere else. It is wiped from brw's own buffer when the step returns.
An error raised by that fill is scrubbed of the value before it propagates, so
the value cannot reach the run result, the MCP response, or the failure bundle's
reason through an error string.

**What an agent can do with it:** ask for a step it cannot see the result of. A
recipe references `secret://<name>`; the agent passes the recipe id, version and
digest. The agent never supplies, names, or receives the value.

### `browser.provider` — granted

**Can reach:** nothing of brw's. brw asks the provider to open a browser session
and, later, to release it. The provider answers with a CDP websocket URL, a
session id and a lifetime.

**Cannot reach through brw:** the page, the DOM, the navigation policy, the
containment boundary, the site-consent grants, cookies, storage, the trace, the
artifact store, the usage ledger, or any brw HTTP or MCP route. As with
`credential.read`, that is what brw *hands* a provider — it is not a statement
about what the provider's process can do, because brw does not sandbox one: the
mint and teardown programs are children of the daemon, running as the daemon's
user with the daemon's environment and network access. See
[Trust and sandboxing](#trust-and-sandboxing).

**What a provider decides that a credential plugin does not.** Which browser brw
drives, and therefore the IP a site sees, where the bytes of a page are rendered,
and who else can reach that browser. A provider that answered with somebody
else's websocket URL would be handing brw's whole session to them, which is why
the trust boundary on the plugin directory matters at least as much here as it
does for a vault.

**What brw keeps.** Everything above the socket. The identity guard, the
navigation allow/block policy, subresource containment and the site-consent gate
are the same code on a remote target as on a local one, and a remote target is
treated as LESS trusted than a local one, never more.

**What a remote browser cannot do**, each refused by name with
`browser.ErrRemoteTargetUnsupported` rather than attempted and silently getting
the wrong answer:

| capability | why |
| --- | --- |
| profile reuse | A profile lives on the machine running the browser. `--user-data-dir`, `--profile-directory` and a workspace profile policy are refused at startup. |
| the extension bridge | It drives the Chrome you are personally signed into on this machine. The print-renderer screenshot fallback goes with it: that is a bridge-only path that shells out to a local PDF rasteriser. |
| installed-profile auth | A recipe declaring `"requires": ["profile_session"]` is refused before its first browser action, so a signed-in flow never runs signed out. |
| local downloads | Chrome writes a download on the machine it runs on. `brw_downloads`, `brw_set_download_path` and a `download:` wait are all refused: the guard is on the bookkeeping they share, not on the two verbs somebody thought of first. |
| local uploads | `brw_upload_file` hands Chrome a path it resolves on its own machine, so a local path names a file on the provider's disk instead of yours. |
| the local clipboard | `brw_clipboard` would read or set the provider host's clipboard. |

`brw_identity` reports `transport: off-host-cdp` — its own lane, and not the
`remote-cdp` that `brwd --remote` reports: that endpoint is on this machine, so a
path, an upload and the clipboard still name what the caller meant there.
`brw_downloads`, `brw_set_download_path`, `brw_upload_file` and `brw_clipboard`
are not advertised in `tools/list` on `off-host-cdp`, so an agent does not spend
a round trip discovering a tool that can only fail; calling one anyway still
returns the named refusal.

#### Manifest

```json
{
  "schema_version": 1,
  "id": "acme.browsers",
  "name": "ACME hosted browsers",
  "version": "1.0.0",
  "description": "Mints a hosted Chrome session",
  "capabilities": ["browser.provider"],
  "browser": {
    "kind": "exec",
    "command": ["/usr/local/bin/acme-browser", "open"],
    "teardown": ["/usr/local/bin/acme-browser", "close", "{session}"],
    "credential": "work/acme/api-key",
    "timeout_ms": 20000
  }
}
```

One kind ships, `exec`, and it is the whole point: whatever service you use, the
auth story is your program's, not brw's. Six named vendors inside brw would be
six auth stories and six breakage surfaces; the capability carries them instead.

`command` mints a session and prints ONE JSON object on stdout:

```json
{"websocket_url":"wss://…","session_id":"sess-42","expires_in_ms":600000}
```

- `websocket_url` must be `ws` or `wss` with a host. brw dials it exactly as
  given — no `/json/version` discovery, no DNS-to-IP rewrite — because those
  would be brw second-guessing the provider and would break TLS on a hosted
  endpoint. A URL carrying userinfo is refused; pass a credential by reference
  instead (below).
- `session_id` is substituted for `{session}` in `teardown`, so it is held to a
  narrow character set. `teardown` must contain the token exactly once: a
  teardown that never sees the id releases whatever the provider considers
  current, which on a shared account is somebody else's browser.
- `expires_in_ms` is required, between one second and 24 hours. brw refuses to
  start an operation past it with a named error rather than letting the socket
  fail with nothing to attribute it to. A provider that states no lifetime is
  asking brw to report an expiry it has no way to notice.

Unknown fields, trailing output and anything over 64 KiB are refused.

#### The provider's own credential

`credential` names a reference, never a value. brw resolves it through the
plugin holding `credential.read` — the same mechanism a recipe uses, not a
second one — at the moment of each call, and writes the value to the program's
**stdin**. A manifest that names a reference therefore needs a `credential.read`
plugin installed as well, which may be the same manifest declaring both
capabilities or a separate one; with nothing to resolve it, the session is
refused rather than minted unauthenticated. A provider that needs no key (a
stand-in endpoint on your own machine) omits the field and is handed nothing. Not its argv: an argv is readable by every process on the machine,
which is the difference between "passed by reference" and "passed by reference
and then printed in `ps`".

It is resolved again for the teardown rather than held for the life of the
session, so brw retains no provider key. The consequence, stated rather than
hidden: revoking the `credential.read` plugin between open and release makes the
release fail, and the error names the session the provider still holds.

The websocket URL is redacted on every rendering path — logs, errors, JSON — to
`scheme://host`. Its path authenticates the connection (Chrome's own
`/devtools/browser/<uuid>` is a bearer token, and a hosted provider usually
carries its key in the query), so only the CDP dialer ever sees the whole thing.

#### Lifecycle

At most one loaded plugin may hold `browser.provider`, for the reason at most one
may hold `credential.read`: two make "which browser am I driving?" unanswerable
from a failure.

`brw plugin revoke <id>` stops the NEXT session being opened. It deliberately
does not break the release of one already open — a revoke that leaked the cloud
browser it was running at the time would cost money to use.

A `--bridge`, `--upstream-http`, `--remote`, `--login`, `--headless`,
`--extension`, `--chrome-arg`, `--proxy-server`, `--profile` or explicit
`--user-data-dir` launch alongside a loaded `browser.provider` plugin is a
startup failure naming the conflict, not a flag that quietly does nothing.

### Never granted

Each of these is refused by name with the security default it would widen. The
refusal by name is only a better error message — the closed allowlist is what
actually refuses them, and an unknown capability is refused the same way.

| capability | the default it would widen |
| --- | --- |
| `cookies.export`, `storage.export` | brw performs no bulk cookie or storage export on the signed-in extension transport. A plugin that could read them would make brw a credential exfiltration tool against the human's own browser profile. |
| `command.run` | brw runs no caller-supplied command. A plugin's argv is fixed in a manifest the operator wrote; a capability that let a plugin (or anything that can reach a plugin) choose the argv would make every other guard irrelevant. |
| `launch.mutate` | Chrome's launch flags are brw's, and they carry the sandbox and the site isolation settings. A plugin that could edit them could turn web security off for every later page. |
| `captcha.solve` | Solving means shipping page content to a third party. brw sends page bytes nowhere the operator did not point it. |
| `navigation.policy.disable` | `--allow-domains` / `--block-domains` is a guardrail an operator sets. Nothing loaded from a file in a directory gets to remove it. |
| `secret.store` | brw stores no secret. A plugin that could write one back into brw would be the vault this whole design exists to avoid. |

Capability names are matched exactly. `credential.read ` (trailing space),
`Credential.Read`, `credential.readonly` and `credential.read/../cookies.export`
are all unknown names and all refused. There is no prefix, case-folding or
normalisation step for an attacker-supplied variant to pass through.

## Trust and sandboxing

Say it plainly: **brw does not sandbox a plugin.** An `exec` provider runs as the
daemon's own user with the daemon's environment. Anyone who can write a manifest
into the plugin directory can make brwd run a program as that user. The trust
boundary is who can write that directory — its mode, its owner and its ancestors
— not a sandbox, and brw enforces the boundary it can:

- The plugin directory and every manifest in it are refused if group- or
  other-writable, or if some other local user owns them. root is accepted as an
  owner: a system install is a legitimate deployment, and root can replace the
  daemon binary anyway.
- Every ancestor of the plugin directory up to `/` is refused if group- or
  other-writable, unless it carries the sticky bit. A 0700 directory inside a
  world-writable parent is one rename away from being somebody else's directory,
  and the sticky bit is the flag that stops that rename.
- An `exec` provider's `command[0]` must be an absolute, already-clean path, and
  the program it names is held to the same mode, owner and ancestor rules as the
  manifest. A bare name would be resolved from the daemon's `PATH` at every
  call, so the manifest an operator reviewed would not decide what runs. A
  `browser.provider` declares two programs, the mint and the teardown, and both
  are held to the rule: checking only the one an operator reads first is not a
  boundary.
- A location has more than one spelling, so both are checked: the ancestors of
  the declared path as well as the ancestors of the file it resolves to. A 0700
  binary in a 0700 directory, named through a world-writable directory, is a
  program whoever can write that directory chooses, because they choose where
  the symlink points. What brw then runs is the resolved path the loader
  checked, not the declared name looked up again at exec.
- The program's mode, owner and ancestors are re-checked immediately before
  every call, not only at load. brwd runs for weeks, and a load-time answer is
  about the machine as it was at boot.
- A manifest is refused above 64 KiB, and unknown fields are refused.
- `file` credential files are refused if group- or other-readable, and the
  resolved path (after symlinks) must stay inside the configured directory. The
  directory itself is refused if group- or other-writable, if another local user
  owns it, or if any ancestor of either spelling of it is writable: choosing the
  answer a provider gives is the same authority as choosing the program.
- brw passes the provider no shell, no brw state and no page data. It does not
  strip the environment: an `exec` child inherits the daemon's, which is what
  `op` and `pass` need to find their own sessions.
- Output is capped at 64 KiB, must be valid UTF-8, must be non-empty, and must
  not contain an embedded newline or NUL. A trailing newline is stripped so
  `op read` and `cat` behave.
- A provider that exits non-zero fails the step. Its stderr is discarded rather
  than quoted into the error, because a program that prints the secret on the
  failure path would otherwise put it in the caller's transcript.
- Every provider call has a deadline (default 10s, maximum 60s), on both kinds.
  For `exec` that includes a provider that backgrounds a grandchild and exits: a
  kill alone leaves the daemon waiting on the inherited stdout pipe, so the wait
  is bounded too. For `file` it covers a read that cannot complete, such as a
  wedged network mount or a named pipe with no writer.

Ownership is the one check with a platform hole: Windows reports no uid through
`os.FileInfo`, so there the mode is the whole check. Ancestor and mode rules
apply everywhere.

Two honest limits. `Secret.Reveal` hands the value to the browser transport as a
Go string, and a Go string cannot be wiped; brw wipes its own buffer and lets
the copy become garbage. And a credential typed into a page can be rendered by
that page — a screenshot of a form that displays the value is the page's
content, not a copy brw made. brw's guarantee covers what brw writes down, not
what a website chooses to show.

## Revocation

`brw plugins` lists what this daemon loaded and what each plugin holds.

`brw plugin revoke <id>` (or `POST /api/plugins/revoke`) drops the grant
immediately. The next recipe run that references a credential fails with a named
error naming the revoked plugin, before its first browser action: a revoked
provider does not leave a half-filled login form behind, and it does not fall
back to an unresolved placeholder, an empty field, or a prompt. Revocation is
process-local and lasts until brwd restarts — delete the manifest to make it
permanent.

There is deliberately no grant route. Revoking narrows what brw can do and is
safe for anything that can reach the control plane to call; granting widens it
and stays an operator action against the filesystem.

## Using a credential in a recipe

A recipe step references the value; it never contains it.

```json
{ "id": "password", "action": "fill", "effect": "read",
  "target": { "role": "textbox", "name": "Password" },
  "value": "secret://work/login/password" }
```

Rules the schema enforces:

- `secret://` is legal only as the **entire** value of a `fill` or `type` step.
  A reference embedded in a longer string, in a selector, in a `navigate_to`
  URL, in an idempotency key, in an event match, in an assertion or in a
  `select` value is a validation error. Those all end up in error text, traces
  and results; a form field does not.
- The reference is read from the recipe as written, before input expansion. An
  input that expands to `secret://x` is ordinary text and is typed literally, so
  a caller cannot use an input to name a credential the recipe did not.
- A step carrying a reference is automatically marked sensitive, so the
  transport's trace records that a fill happened and withholds the value on
  every transport.

`brwctl recipe draft` (and any other trace-to-recipe compilation) **fails** on a
trace action that was credential-sourced, rather than emitting a placeholder to
fill in later. brw records that the value came from a provider; it does not
record which reference, so there is nothing to infer, and guessing would produce
a recipe that types the wrong secret into the right field. The hook is
`recipe.GuardTraceActionForCompilation`.

## Distribution

There is no plugin registry, no marketplace, and no download. brw will not fetch
a plugin, resolve one by name, or update one. A plugin is:

1. a program already installed on the machine (`op`, `pass`, anything that
   prints one secret to stdout), or a directory of files; and
2. a manifest an operator wrote into their own plugin directory.

Nothing about a plugin travels with a recipe. A recipe names `secret://work/login/password`;
what that resolves to, and whether it resolves at all, is a property of the
machine running brwd. A recipe shared with someone else carries no credential
and no vault coordinates beyond a name the recipient must map themselves.
