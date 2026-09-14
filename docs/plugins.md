# Plugins and capabilities

brw has one extension point: an operator-installed plugin, described by a JSON
manifest, that the daemon may call for a narrowly defined job. A plugin is not a
way to add browser verbs, and it is not a way to change what brw already
refuses. The capability list below is a closed allowlist. Everything outside it
is refused at load, including the capabilities other agent-browser products
ship.

The reason the extension point exists at all is authentication. brw is not a
secret store and will not become one, so a fresh browser context has no way to
log in: a signed-in recipe works only because the profile was already signed in
by a human. The `credential.read` capability closes that without brw holding a
vault — the value is fetched from the operator's own secret manager at the
moment of use and written into one form field.

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

### `browser.provider` — reserved, refused today

The interface shape exists (`plugin.BrowserProvider`) for the cloud-browser
work, and the loader refuses the capability with a named error. brw advertises
no plugin-supplied browser backend, so granting the name today would be a claim
with nothing behind it.

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
  call, so the manifest an operator reviewed would not decide what runs.
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
