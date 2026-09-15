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

Every tool carries a rule in `internal/siteconsent/toolgate.go`, or a written
reason it needs no grant. `TestEveryToolIsClassifiedForConsent` fails on a tool
that has neither, so a new tool cannot ship ungated by being forgotten.

- `read` is checked twice: on the **destination** a call names (`brw_open`,
  `brw_open_incognito`, `brw_navigate_to`, `brw_read_url`, `brw_replay_request`,
  `brw_cookies`, and `open` / `navigate_to` steps inside `brw_plan` /
  `brw_batch`), and on the **live page** every reading tool is pointed at
  (`brw_read`, `brw_snapshot`, `brw_screenshot`, `brw_find`, `brw_get`,
  `brw_console`, the assertions, …).

  Gating the arrival alone was not enough. On the extension bridge brw attaches
  to a Chrome the user is already driving, so tabs exist that brw never opened;
  and a server redirect from a granted origin produces a document brw never
  asked for. Both are pages no grant was given for. Each read therefore costs one
  transport round trip to resolve the tab's URL, the same one `act` pays.
- `act` is checked on the **action**, against the tab's live URL, because
  between the navigation and the click the page may have moved.
- A URL the **daemon itself** fetches (`brw_read_url`, and the `url` a
  `brw_upload_file` pulls its file from) is checked on the URL the call named
  **and on every redirect hop after it**. A grant is for an origin, not for a
  request: gating only the first URL made a read grant on one site a read of
  whatever that site chose to point at.

A tool is checked in the argument it really uses, not in a field named `url`:
`brw_authenticate` is addressed by `origin`, `brw_cookies` by `url` or `domain`
or neither, `brw_set_extra_headers` by a list of origins. A named argument does
not always replace the tab, either. `brw_cookies action=list` reads the cookies
the **tab's** scope returns and uses `domain` only to filter them, so both are
checked; `action=set` and `action=delete` address the cookie by domain directly
and never consult the tab. That rule has one definition, `CookieScopeIsTab`,
which the cookie implementation and the gate both call, so neither can decide
something the other does not do. A call whose own arguments can escalate it does:
`brw_cookies action=set`, `brw_storage action=set`, a non-GET
`brw_replay_request` and a `fn:` predicate in `brw_wait_for` all need `act`, not
`read`.

A plan or batch is gated twice. Before anything is dispatched it is walked in
step order: `open` and `navigate_to` move the working tab, so the steps after one
of them are checked against the destination that step named, and `focus_tab`
against the tab it moves to. That walk is refused before any step runs, which is
where a refusal costs nothing.

The walk stops being true the moment a step **acts**. A click on a link is a
navigation, so the steps after it can land on an origin the arguments never
named. Each step is therefore re-checked by the runner immediately before it
runs, against the origin the tab is showing at that moment — on every transport,
since a gate one runner honours and the other does not is a bypass by choice of
transport. The high-risk confirmation for a step the walk could not place is
asked there too, so a person is asked once per action and about the origin it
really runs on.

Every step verb both runners implement is classified;
`TestEveryPlanAndBatchStepActionIsClassified` reads the verbs out of the runners
themselves, so a step kind that reaches the controller with no rule fails that
test rather than walking through the gate, and
`TestEveryRunnerConsultsTheStepGate` fails on a runner that skips the re-check.

If the live URL cannot be resolved, or the arguments do not say where a step
lands, the call is refused. There is no "allow because we could not tell".

`file:`, `filesystem:`, `view-source:`, `chrome:`, `chrome-extension:` and
`javascript:` targets are refused outright while the guard is on. They carry no
origin that could be granted, but they are not nothing either: domain containment
is blocklist-only by default and lets non-network schemes past, so treating them
as "no site here" made `brw_read_url` a local-file reader. `about:`, `data:`,
`blob:` and same-origin relative references stay exempt — there is genuinely no
site there.

`POST /dashboard/input` is outside the gate on purpose. It carries a takeover
token and dispatches the input of a **human** who has taken the browser over, and
a person at the keyboard is the consent this gate exists to obtain.

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

`brwd --content-nav-guard` (any CDP transport; not the extension bridge) refuses a top-level
navigation that **page content** initiated to another site — an injected link
click, a meta refresh, a script assignment to `location`. What the agent asked
for still works.

The signal is brw's own bookkeeping, not anything read out of the page: every
entry point that steers the browser records its intent first, and a page cannot
write to that. Two kinds of intent:

- A **navigation verb** (`brw_open`, `brw_navigate_to`, `brw_navigate`, and the
  `open` / `navigate_to` steps) names its destination, and that destination is
  allowed for up to a minute.
- The agent's own **input** (click, click_text, click_xy, press, type, fill,
  select, commit, drag, upload_file, the key and mouse halves, evaluate) names no
  destination — the agent chose a control, not a URL — so it allows the next
  navigation wherever it goes, for five seconds. Without this, clicking an
  ordinary cross-site link (an OAuth sign-in, a checkout handing off to a payment
  processor) was refused as though the page had done it.

Either intent is spent the moment the tab lands, so a page cannot re-use it.
Redirect hops of an agent navigation stay agent-initiated; same-site navigation
and cross-origin subframes are untouched.

A refused navigation is failed with `ERR_ABORTED` rather than
`ERR_BLOCKED_BY_CLIENT`, so the tab stays on the document it was on. Blocking it
the other way still moved the agent — to a Chrome error page on the attacker's
URL.

Three limits:

- A navigation that the page makes within five seconds of the agent's own click
  is allowed: brw cannot tell it apart from the navigation the click was for.
  The window is deliberately short, and an intent is spent on the first arrival.
- The extension bridge has no equivalent interception point, so
  `--content-nav-guard` there is refused at startup rather than ignored. Every
  CDP lane has one: a Chrome brw launched, `--remote`, the Chrome opt-in and a
  browser on another machine alike.
- "Same site" is host equality or a subdomain relationship. brw carries no
  public-suffix list, so sibling subdomains of a public suffix are treated as
  same-site. The error is in the permissive direction for that case; the
  cross-site case the guard exists for is unaffected.
