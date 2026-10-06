# Persistent page watchers

`brw_watch_page` registers a read-only watcher on the browser-host daemon. The
daemon opens a private background tab and samples it independently of the MCP
client. A persistent scheduler such as Maix reads the durable event queue and
delivers an agent follow-up; the model runs when an event arrives.

```json
{"id":"inbox","url":"https://example.com/inbox","selector":"#messages","mode":"text","interval_ms":5000}
```

The modes are `title`, `text`, and `count`. Without a selector, the default is
`title`; with one it is `text`. Text/count require a CSS selector. Sampling
defaults to 5000 ms, with bounds 1000..300000 ms. An optional stable `id` makes
identical retries idempotent; changing the configuration under that id is refused.
Remove the old watcher before replacing it.

For a static page that only sees new data after loading, set
`refresh_interval_ms`, for example 30000. The default is 0, which disables
refreshes; positive values must be 5000..3600000 ms. Refreshing replaces only the
watcher's private document and waits for the replacement to become ready. It
retains the digest baseline, so an unchanged reload emits no change. Browser
sampling detects the state observed at each sample and may miss intermediate
activity. There is no automatic detection of a site's refresh requirements.

The first successful sample is a baseline and emits no `changed` event. Events
contain `watcher_id`, increasing `seq`, `at`, `kind`, the registered `url`,
`mode`, a SHA-256 `digest`, and a matching element `count`. They never include
page text, titles, message bodies, or arbitrary JavaScript. Availability events
use `kind:"unavailable"` with a bounded `reason`, followed by `kind:"recovered"`
when a valid sample succeeds. An unexpected URL, including a login redirect,
signals unavailable without reading that document. Persistent availability state
prevents repeated unavailable notifications across daemon restarts. A missing
selector alone does not establish that a user logged out.

```json
{"watcher_id":"inbox","since_seq":0,"limit":50}
```

Pass this to `brw_page_events`. Its result includes `events`, `latest_seq`,
`oldest_seq`, `gap`, and `has_more`. Retention is 512 events per watcher, with a
limit of 64 watchers. A response defaults to 50 events and accepts 1..100.
`since_seq` is exclusive. Persist the last processed sequence after delivery;
reading events never acknowledges or deletes them. `gap:true` means the cursor
predates retention and a full page inspection may be needed.

Use `brw_page_watchers` with `action:"list"` (the default), `pause`, `resume`,
or `remove`; the last three require `id`. Pause preserves the private tab,
baseline and history while stopping sampling. Resume retries a closed tab.
After signing in elsewhere, resume also replaces a redirected watcher tab with
a fresh tab at the registered URL.
Remove closes the private tab and erases its registration and event history.
Watcher tabs appear leased in tab lists and reject ordinary browser operations.
Use your own `brw_open` tab to inspect new activity.

The exact HTTP(S) URL is pinned, including path, query and fragment; navigation
policy and current read consent are checked on background samples and refreshes.
Sampling never prompts a terminal user. The script also checks the exact URL
and origin in the page before accessing its title or selected nodes. Watchers
do not submit forms, follow links, run caller-supplied script or invoke webhooks.

`brwd --page-watch-root auto` is the default. State lives in the user config
directory under `brw/page-watchers/<workspace-profile-key>/`, outside the
repository, with 0700 directories and atomic 0600 files. The daemon holds a
store lock to prevent competing writers. `--page-watch-root off` disables the
capability; an absolute directory selects an explicit store. `BRW_PAGE_WATCH_ROOT`
sets the same option. Definitions, baselines, sequence numbers and retained
events survive restart. Sampling requires the browser-host daemon to be running;
a disposable `--upstream-http --mcp` process forwards every watcher operation to
that host and never starts its own sampler.

On daemon restart, enabled watchers acquire fresh private tabs. Browser tab IDs
can be reused after a browser restart, so saved IDs are never read, refreshed,
or closed without current ownership. An old extension watcher tab may remain
open when the browser itself survived the daemon restart; remove those old tabs
manually when convenient. Paused watchers open nothing until resumed.
Graceful shutdown closes tabs whose current ownership is proven. Extension
reconnection also invalidates old tab claims; background RPCs refuse to read,
reload or close a tab from an earlier connection and the watcher acquires a fresh
tab. Crash and reconnect leftovers remain untouched when ownership cannot be
proven.

Event queue reads remain available after a read grant is revoked so a scheduler
can deliver the `unavailable` signal. They expose only the registered URL and
already captured digest/count bookkeeping and perform no live page read.

The HTTP proxy contract uses POST `/api/watchers/register`,
`/api/watchers/manage`, and `/api/watchers/events`, with the same JSON arguments
and results as the three MCP tools. These routes do not acquire an agent's tab
lease; watcher ownership belongs to the browser-host service.
