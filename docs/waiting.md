# Waiting — what answers `brw_wait_for`

Every wait is one of three mechanisms, and `brw_wait_for` names which one
answered it in `resolved_by`:

| `resolved_by` | What it means | Cost of noticing |
| --- | --- | --- |
| `event` | A browser event subscription delivered the signal. | None. The wait is parked on a stream that is already running. |
| `script` | One awaited in-page promise, resolving on the DOM mutation or navigation that satisfies the predicate. | One round trip to arm, then nothing until it resolves. |
| `poll` | The transport has no subscription for this signal and re-asks on a timer. | One request per check, for as long as the wait lasts. |

`wakeups` in the same result counts how many times the wait re-evaluated. An
event-driven wait wakes for the events that were actually delivered; a polled
one wakes on a cadence, so its count grows with how long the wait ran.

## One subscription per context, not per wait

On the direct-CDP transport each chromedp context gets a single subscription
(`internal/browser/events.go`) carrying `Page.loadEventFired`,
`Page.frameNavigated`, `Page.javascriptDialogOpening`,
`Network.responseReceived`, `Runtime.consoleAPICalled` and
`Browser.downloadProgress`. Every wait against that tab reads the shared stream,
and the post-action settle takes its navigation signal from the same place.

Two properties that are enforced by tests rather than by convention:

- The scope is dropped when its context ends. Closing a tab leaves no listener,
  no retained event and no registered waiter
  (`TestClosingATabLeavesNoLiveSubscription`).
- Retention is capped per scope and response headers are redacted on the way in.
  A busy page emits a `Network.responseReceived` per request; keeping them all
  would grow with the page's traffic for the life of the tab, and a retained
  `Set-Cookie` would be a way around the redaction `brw_network_capture` already
  applies (`TestRetainedEventsAreCappedPerScope`,
  `TestRetainedResponseHeadersAreRedacted`).

## Per-transport matrix

The extension bridge drives pages through `chrome.debugger` only for the
operations that need it, and holds no attachment for lifecycle events. It
answers dialog and download waits by re-asking its own extension-side registries
on a bounded, backing-off cadence (60 ms rising to a 400 ms ceiling).

| Condition | Direct CDP (`brwd` owns Chrome) | Extension bridge |
| --- | --- | --- |
| `load` | `event` — `Page.loadEventFired`, or immediate when the subscription already recorded the load. Falls back to `script` when brw attached after the document had already loaded, since no event is coming for it. | `script` |
| `ready`, `page_ready` | `event` when the document has already fired its load event; otherwise `script`, because a document is interactive before it is loaded and only the document knows that. | `script` |
| `committed`, `text:`, `not_text:`, `url:`, `not_url:`, `title:`, `not_title:`, `ref:`, `not_ref:`, `selector:`, `not_selector:`, `fn:` | `script` | `script` |
| `dialog`, `dialog:<substring>` | `event` — `Page.javascriptDialogOpening`, including one already answered inside the recency window. | `poll` — `get_dialogs`, peeked so the wait does not consume the ring `brw_dialog` reads. |
| `download`, `download:<substring>` | `event` — `Browser.downloadProgress`. | `poll` — `get_downloads`, which does not consume a recipe's change cursor. |

Both transports apply the same 15-second recency window, so "did my click cause
this?" is answered identically on either. A wait the extension genuinely cannot
answer — a build predating `chrome.downloads` or `brw_dialog` support — returns a
named capability error rather than running out its timeout.

## Why a dialog wait reports a dialog that is already gone

brw answers JavaScript dialogs automatically: an enabled Page domain suppresses
Chrome's native dialog UI and the renderer blocks until the dialog is answered
over CDP, so leaving one on screen would wedge the tab. By the time the wait
written after the click runs, the dialog has opened, been answered and closed.
`brw_wait_for dialog` therefore reports that one opened, within the recency
window — it does not leave it open for you.
