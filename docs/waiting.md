# Waiting — what answers `brw_wait_for`

Every wait is one of three mechanisms, and `brw_wait_for` names which one
answered it in `resolved_by`:

| `resolved_by` | What it means | Cost of noticing |
| --- | --- | --- |
| `event` | A browser event subscription delivered the signal, or had already recorded it when the wait registered. | None. The wait is parked on a stream that is already running. |
| `script` | One awaited in-page promise, resolving on the DOM mutation or navigation that satisfies the predicate. | One round trip to arm, then nothing until it resolves. |
| `poll` | The transport has no subscription for this signal and re-asks on a timer. | One request per check, for as long as the wait lasts. |

`wakeups` in the same result counts how many times the wait re-evaluated. An
event-driven wait wakes for the events that were actually delivered; a polled
one wakes on a cadence, so its count grows with how long the wait ran.
`wakeups: 0` alongside `resolved_by: "event"` means the subscription had already
recorded the answer before the wait registered — a dialog brw answered a moment
ago, a small download that finished first, a document that had already loaded.

## One subscription per context, not per wait

On the direct-CDP transport each chromedp context gets a single subscription
(`internal/browser/events.go`) carrying `Page.loadEventFired`,
`Page.frameNavigated`, `Page.javascriptDialogOpening` and
`Browser.downloadProgress`. Every wait against that tab reads the shared stream,
and the post-action settle takes its navigation signal from the same place.

`Network.responseReceived` and `Runtime.consoleAPICalled` are deliberately NOT
carried. They are the highest-volume events a page produces and no wait reads
either one, and a carried event is not free: it occupies a slot in every
waiter's queue and a slot in the retained ring.

Four properties, enforced by tests rather than by convention:

- A waiter is only sent the kinds it asked for. The per-waiter queue is finite
  and a full one drops what lands next, so a dialog wait that was also handed
  every page load could have its dialog pushed out by ordinary traffic
  (`TestASubscriberOnlyReceivesTheKindsItAskedFor`).
- Retention is capped per kind, not per scope, so one kind's traffic cannot
  evict another's. The ring is what answers "did this already happen?", and a
  shared ring made that answer a race against whatever else the page did in
  between (`TestRetainedEventsAreCappedPerKind`,
  `TestOneKindsTrafficDoesNotEvictAnother`).
- The scope is dropped when its context ends, and a late event does not bring it
  back. Closing a tab leaves no listener, no retained event and no registered
  waiter (`TestClosingATabLeavesNoLiveSubscription`,
  `TestScopeIsDroppedWhenItsContextEnds`).
- A wait that conjured a scope takes it with it when it releases, whatever
  landed in it meanwhile (`TestReleasedSubscriptionLeavesNoScopeBehind`).

## Per-transport matrix

The extension bridge drives pages through `chrome.debugger` only for the
operations that need it, and holds no attachment for lifecycle events. It
answers dialog and download waits by re-asking its own extension-side registries
on a bounded, backing-off cadence (60 ms rising to a 400 ms ceiling).

| Condition | DevTools Protocol (`direct-cdp`, `chrome-opt-in-cdp`) | Extension bridge |
| --- | --- | --- |
| `load` | `event` — `Page.loadEventFired`, or immediate when the subscription already recorded the load. Falls back to `script` when brw attached after the document had already loaded, since no event is coming for it. | `script` |
| `ready`, `page_ready` | `event` when the document has already fired its load event; otherwise `script`, because a document is interactive before it is loaded and only the document knows that. | `script` |
| `committed`, `text:`, `not_text:`, `url:`, `not_url:`, `title:`, `not_title:`, `ref:`, `not_ref:`, `selector:`, `not_selector:`, `fn:` | `script` | `script` |
| `dialog`, `dialog:<substring>` | `event` — `Page.javascriptDialogOpening`, including one already answered inside the recency window. | `poll` — `get_dialogs`, peeked so the wait does not consume the ring `brw_dialog` reads. |
| `download`, `download:<substring>` | `event` — `Browser.downloadProgress`. | `poll` — `get_downloads`, which does not consume a recipe's change cursor. |

`load` and `ready` are different conditions on EVERY transport. `ready` is
satisfied as soon as the document is interactive; `load` is the load event, and
the in-page script that answers it where no subscription can requires
`document.readyState === 'complete'` (`TestWaitForLoadIsNotAnAliasForReady`).

Every transport applies the same 15-second recency window, so "did my click cause
this?" is answered the same way on all of them: a DevTools Protocol wait times a
download from its own registry, and the extension bridge from the
`changed_at_ms` the extension records on each state change. On the Chrome opt-in
lane the registry is populated the same way and carries no file path, because
brw leaves that browser's downloads where its user sends them — the wait answers,
the path does not come with it. One exception, and it is a version skew
rather than a transport limit: an extension build that predates `changed_at_ms`
sends no completion times, and the bridge then treats every already-finished
download as old news — a file that finished in the second before the wait was
written is missed and the wait runs to its timeout. Reload the extension to fix
it. A wait the extension genuinely cannot answer — a build predating
`chrome.downloads` or `brw_dialog` support — returns a named capability error
rather than running out its timeout.

### The third transport: a remote `brwd` over HTTP

`brw` can also drive a remote daemon (`internal/httpclient`). It owns no browser
of its own, so every wait is whatever the upstream daemon's own transport does,
and the row above that applies is the upstream's. `resolved_by` and `wakeups`
are forwarded from that daemon; a daemon older than this change answers the
route with a bare `{"ok": true}`, so both fields are absent. `condition`,
`ok` and `waited_ms` are filled in locally and are always present. The same
applies to `brw wait` on the command line, which reads the same route body.

## Why a dialog wait reports a dialog that is already gone

brw answers JavaScript dialogs automatically: an enabled Page domain suppresses
Chrome's native dialog UI and the renderer blocks until the dialog is answered
over CDP, so leaving one on screen would wedge the tab. By the time the wait
written after the click runs, the dialog has opened, been answered and closed.
`brw_wait_for dialog` therefore reports that one opened, within the recency
window — it does not leave it open for you.
