# Site permissions

Domain containment (`--allowed-domains` / `--blocked-domains`) decides whether a
destination may be reached. It keeps no record of what a user agreed to, so
there is nothing to show them and nothing to revoke.

Site permissions are the other half: a persistent per-origin grant, stored with
the profile policy rather than the session.

Everything here is **off unless `brwd --site-consent` is passed**. A daemon
started without it behaves exactly as it did before this existed.

## What a grant is

One record per origin and scope:

| Field | Meaning |
| --- | --- |
| `origin` | `scheme://host[:port]`, IDNA-normalised, default port dropped |
| `scope` | `read` (observe the site) or `act` (change things on it; implies read) |
| `decision` | `allow` or `deny` — a recorded "no" is not re-asked |
| `granted_at` / `granted_by` | when, and the OS account it is attributed to |
| `expiry` | absent means it does not expire on its own |
| `override_category` | the shipped blocklist category this grant was allowed to cross |
| `mac` | HMAC-SHA256 over every field above |

Records live in `site-grants.json` beside the profile policy. The ledger of
consent events is `site-grants-ledger.jsonl` next to it.

## Why the MAC

The store is a file owned by one user, and a file is exactly as trustworthy as
whatever else runs as that user. Origin Technology found permission entries
written in plaintext, with nothing binding them to the user's consent, in
another agent-browser bridge: a local process could write itself a grant for any
domain.

Each record therefore carries a MAC over `(origin, scope, decision, granted_at,
expiry, granted_by, override_category)`, keyed by `site-consent.key` — a 0600
regular file created on first use. A record whose MAC does not verify is
discarded rather than honoured, and is reported as a refusal on every listing
surface so it is not a silent disappearance.

Each field is length-prefixed inside the MAC payload, so no field's content can
be read as part of another.

What this does and does not buy:

- It cannot be forged by **writing** to the store. That is the bug it exists to
  prevent.
- It does **not** stop a process running as the same user from reading the key.
  Nothing at the filesystem layer can.

## Where each scope is enforced

Not symmetrical, deliberately:

- `read` is checked on the **navigation** that reaches an origin —
  `brw_open`, `brw_open_incognito`, `brw_navigate_to`, `brw_read_url`,
  `brw_replay_request`, `brw_cookies`, and `open` steps inside `brw_plan` /
  `brw_batch`. An origin can only be looked at after brw has been steered there.
- `act` is checked on the **action**, against the tab's live URL, because
  between the navigation and the click the page may have moved. Resolving that
  URL is a transport round trip, which is why reads do not pay it.

If the live URL cannot be resolved, the action is refused. There is no
"allow because we could not tell".

## Category blocklist

Three shipped categories — `financial-services`, `adult`, `pirated` — live in
`internal/siteconsent/categories.json`, embedded in the binary.

- **Source**: hand-curated in that file. brw subscribes to no categorisation
  feed and fetches nothing at runtime. What ships is what is in the file.
- **Update path**: edit the file and open a pull request against
  `Don-Works/brw`. `TestCategoryDataIsWellFormed` gates its shape. Operators who
  need different lists extend or add categories through the admin config's
  `category_domains` map without waiting for a release.
- **Coverage**: a seed list of well-known hosts, not a classification of the
  web. A hit is reliable; a miss means *unlisted*, not *checked and cleared*.

A blocklisted origin cannot be granted by the ordinary path at all, and an
interactive prompt is never offered for one. It takes an explicit flag:

```sh
brwctl grants allow https://example-bank.test --scope act \
    --override-category financial-services --note "quarterly reconciliation"
```

The override is written to the ledger with who and when.

## High-risk action confirmation

`--confirm-actions` (or `confirm_actions` in the admin config) gates four
classes, classified from a table in `internal/siteconsent/risk.go`:

| Class | Matched from |
| --- | --- |
| `publish` | the action's target label or clicked text |
| `purchase` | the action's target label or clicked text |
| `personal-data` | the field labels the action writes to |
| `blocklisted-category` | the origin, regardless of what the action says it does |

Labels come from the agent's own arguments or from the accessible name brw
already reported for that ref in a snapshot or find. **A click addressed only by
a ref brw has never described carries no words to match**, so only the
origin-level rule can fire for it. That is a limit of the classification, not a
gap papered over.

With nobody to confirm, the action is **refused**. Never auto-approved.

## Interactive prompting

`brwd --site-consent-prompt` asks on the terminal the daemon was started from,
and records the answer so it is asked once. It needs a real terminal on stdin
and is incompatible with `--mcp`, which owns stdin. Anything but `y`/`yes` is a
no, including EOF.

Without it the daemon is non-interactive: an un-granted origin is refused with
the origin and the missing scope named.

## Managed machines

`site-consent.json` beside the profile policy (or `--site-consent-config`):

```json
{
  "allowed_origins": ["intranet.example.test"],
  "blocked_origins": ["forbidden.example.test"],
  "category_domains": {"financial-services": ["ledger.example.test"]},
  "confirm_actions": true,
  "default_grant_ttl": "24h"
}
```

- `allowed_origins` is a standing yes: no prompt, and nothing written to the
  local store, so removing an entry from the config removes the permission.
- `blocked_origins` cannot be overridden from the machine it constrains — not by
  a prompt, not by a grant, not by a category override.
- Unknown keys are refused. A typo that silently did nothing would leave an
  operator believing a restriction was in force.
- `--confirm-actions` can only turn the gate on; it cannot turn off one the
  config set.

## Operating it

```sh
brwctl grants list                       # what this profile holds, expiries included
brwctl grants allow <origin> --scope act # grant explicitly
brwctl grants revoke <origin>            # revoke one
brwctl grants revoke-all                 # revoke everything
brwctl grants ledger                     # who granted or crossed what, and when
```

`brwctl grants` works on the file, not through a running daemon, so a revocation
is possible when the daemon is wedged or stopped. The store re-reads the file
when it changes, so a revocation applies on the agent's **next action** with no
daemon restart.

The same list is in the extension's options page under *Site permissions*, with
a Revoke button per row and a Revoke all. The daemon serves that surface on the
bridge port to a loopback request from the extension only; a web page cannot
read which sites a user has granted.

Over HTTP: `GET /api/consent/grants`, `POST /api/consent/revoke`, and the
`brw grants` / `brw grants revoke` CLI verbs.

## Content-boundary navigation guard

`brwd --content-nav-guard` (direct CDP only) refuses a top-level navigation that
**page content** initiated to another site — an injected link click, a meta
refresh, a script assignment to `location`. The same destination requested by
the agent still works.

The signal is brw's own bookkeeping, not anything read out of the page: every
entry point that steers the browser records its intent first, and a page cannot
write to that. Redirect hops of an agent navigation stay agent-initiated;
same-site navigation and cross-origin subframes are untouched.

A refused navigation is failed with `ERR_ABORTED` rather than
`ERR_BLOCKED_BY_CLIENT`, so the tab stays on the document it was on. Blocking it
the other way still moved the agent — to a Chrome error page on the attacker's
URL.

Two limits:

- Direct CDP only. The extension bridge has no equivalent interception point, so
  `--content-nav-guard` there is refused at startup rather than ignored.
- "Same site" is host equality or a subdomain relationship. brw carries no
  public-suffix list, so sibling subdomains of a public suffix are treated as
  same-site. The error is in the permissive direction for that case; the
  cross-site case the guard exists for is unaffected.
