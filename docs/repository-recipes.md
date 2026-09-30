# Repository-owned recipes and optional registries

A project can keep reviewed, sanitized schema-v1 recipes in `.brw/recipes/` and
run them with `brw run --file`. This is a project convention over the existing
caller-supplied recipe API: brw does not automatically scan `.brw/recipes/`,
install its contents, or add them to provider search.

## Review and run a committed recipe

Use a versioned filename, for example `.brw/recipes/catalog-search.1.0.0.json`.
The recipe body carries its own `id` and semantic `version`. Review exact origins,
semantic selectors, runtime inputs, browser requirements, effects and bounded
postconditions before committing it. Keep passwords, payment data, customer
records, account-specific values, raw traces, screenshots, cookies and private
business details out of committed files. Parameterize appropriate variable data;
resolve credentials through the configured credential mechanism rather than
embedding values. A private repository still needs this review.

From the owning repository:

```sh
brwctl recipe validate --file .brw/recipes/catalog-search.1.0.0.json
brw run --file .brw/recipes/catalog-search.1.0.0.json --input query=chairs
```

These are the current commands: validation is a `brwctl` administration command;
execution is a `brw` command. Validation reports ID, version and canonical digest.
It validates structure, not the live merchant or the user's authority to act.
Choose runtime inputs declared by the actual recipe; `query` is illustrative.
A run uses the configured browser host and returns its normal JSON outcome.

The file is read once per invocation, parsed and sent as a complete recipe body.
Do not combine `--file` with a positional recipe ID, `--recipe-version`, or
`--digest`. No recipe provider is necessary for an ordinary inline recipe, but
receipt-dependent external-write steps require a receipt-capable provider.
Site consent, profile requirements, exact-origin enforcement, write verification
and other runner checks still apply. Committing a recipe grants no permission to
purchase, publish or submit data.

Pin the repository commit and file path in automation. Verify an artifact hash
when distributing a file separately. The file's byte hash and brw's canonical
parsed `recipe_digest` are different identities. A changed recipe version or
repair does not make an ambiguous prior write safe to repeat; reconcile its
business-operation identity first. See [scheduling](scheduling.md).

## Promote from private staging

Compile and author raw traces, plans and drafts in owner-only private staging
outside every Git checkout. The compiler rejects a destination inside a checkout;
do not point `--out` directly at `.brw/recipes/`. Review the emitted recipe and
its review report there, remove private context, and create a separately reviewed,
sanitized repository artifact only when its owning project's disclosure policy
allows it. Validate that artifact again before committing. A successful compile
is not a privacy review or proof that a live write completed.

Operational private recipes remain in the owning workflow's private artifact
store or configured private provider. They do not belong in the public brw source
repository or bundled skill. `--recipe-root` remains an owner-only directory
outside every Git checkout; `.brw/recipes/` cannot become that private directory
provider. Its directories are `0700` or stricter, recipe files `0600` or stricter,
and symlinks are rejected. `brwctl recipe install` also requires private staging
and installs there atomically. This rule is unchanged by file-based execution.

## Optional registry adapters

The existing provider interface separates disclosure-safe `Search(query, origin,
limit)` metadata from `Fetch(id, version, digest)` of one exact immutable recipe.
The HTTP implementation uses `POST /v1/recipes/search` and
`POST /v1/recipes/fetch`, configured with `--recipe-provider-url`; optional bearer
authentication comes from `--recipe-provider-token-file`. See the exact wire
contract in [recipes and artifacts](recipes-and-artifacts.md).

A registry is optional. Repository files use the inline path. A private directory
serves a modest collection; an operator-owned HTTP adapter can index a larger
bank without adding database clients to the browser runtime. Search candidates
must match intent, exact origin and risk. Run the returned ID, version and digest
unchanged. A provider must preserve pinned approved versions; a mutable document
head alone cannot satisfy an immutable fetch.

Maix, Notion and Postgres are possible backing services for an operator-owned
adapter, not bundled brw registry implementations:

- A Maix-backed adapter must bind authenticated requests to the intended
  workspace context and enforce its authorized workspace ancestry on the
  server. Include context in authorization and cache keys; missing, mismatched
  or unauthorized scope must fail closed. Do not silently fall back to another
  workspace, a global recipe head or an unrelated registry when scoped search
  misses. brw's current generic search/fetch payload has no workspace field,
  so the adapter must derive a trusted binding from its endpoint or authenticated
  deployment context rather than pretend brw forwards one.
- A Notion-backed adapter must publish reviewed bodies into immutable snapshots
  with ID, version and canonical digest. Edits create a new approved version;
  fetching an older pin must return that exact snapshot or fail, not today's
  page content.
- A Postgres-backed adapter can enforce uniqueness of ID/version and preserve
  approved bodies and digests in immutable rows. Authorization, search ranking,
  audit and version publication belong to the service. This describes a design
  requirement, not a supplied schema or migration.

Use a separately authorized publication API for registry writes. Search/fetch
configuration does not authorize publication. brw revalidates fetched recipes
and metadata, but a provider must enforce organization/workspace authorization
before disclosing either. No product Maix changes are needed for this repository
file convention.
