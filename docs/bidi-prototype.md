# WebDriver BiDi on Firefox: what the prototype measured

brw drives Chromium over the DevTools Protocol. This is the record of a
prototype that asked whether WebDriver BiDi could carry the same guarantees on
Firefox, what it measured, and the decision taken from it.

Everything below was measured, not read off the specification. The prototype is
`internal/bidi`; each answer names the test that produced it, and those tests
run against a real Firefox on any machine that has one.

Measured against **Firefox 155.0.1**, macOS arm64, launched as
`firefox --headless --no-remote --profile <dir> --remote-debugging-port 0`.
Firefox announces `WebDriver BiDi listening on ws://127.0.0.1:<port>` on stderr
and serves BiDi directly. There is no geckodriver in the path and no
`/json/version`: it is a pure BiDi target, so none of brw's CDP code reaches it.

## 1. Does BiDi expose the events the settle machinery needs?

**Yes, all five.** brw resolves waits from a CDP event stream
(`internal/browser/events.go`) rather than by polling, and every event that
stream is built on has a BiDi equivalent that both subscribes and arrives:

| brw needs | BiDi event | Arrived |
|---|---|---|
| Navigation started | `browsingContext.navigationStarted` | Yes |
| Load complete | `browsingContext.load` | Yes |
| Console output | `log.entryAdded` | Yes, with level, text, args and a stack |
| Response finished | `network.responseCompleted` | Yes, with request, status and headers |
| Dialog opened | `browsingContext.userPromptOpened` | Yes, with type and message |

A successful `session.subscribe` is only evidence because Firefox refuses an
event name it does not implement: subscribing to `brwNoSuch.event` fails. The
test asserts that first, then requires each of the five to actually arrive from
one page interaction.

`TestBiDiSettleEventsArrive`.

## 2. Can the actionability checks be reproduced?

**Yes, by running brw's own scripts unmodified.** `script.callFunction` takes a
function declaration and arguments, so `snapshot.SnapshotFunctionScript`,
`WaitForActionableScript`, `ResolveBoxScript` and `FillElementScript` were
handed to Firefox as-is. Every verdict the actionability script can return was
reproduced:

| Fixture element | Verdict on Firefox |
|---|---|
| An ordinary visible button | `ok`, mode `ax_visible` |
| `opacity:0` under `aria-hidden`, nothing over it | `ok`, mode `hit_test` |
| The same element with an overlay over its centre pixel | not ok, `not_visible` |
| `display:none` | not ok, `not_visible` |
| `disabled` | not ok, `disabled` |

The middle two rows differ only in what `elementFromPoint` returns, so passing
both is what establishes that hit-testing works and not merely that the element
has a box. Editability was checked by filling an input through
`FillElementScript` and reading the value back.

`TestBiDiReproducesActionabilityVerdicts`.

**Refs survive, and the same-document guarantee is stronger than CDP's.** brw's
refs are page-side state: `window.__brw` plus a `data-brw-ref` attribute stamped
on each element. Both persist across separate BiDi commands — successive
`script.callFunction` calls against one browsing context run in the same realm,
and a ref taken from one call resolves to the same element in the next.

The realm is also what pins the document. Every `script.callFunction` result
names the realm it ran in, and a call pinned to that realm after a navigation
fails outright:

```text
script.callFunction: no such frame: Realm with id 20361f48-… not found
```

So "act on the document this ref came from, or fail" is expressible directly,
rather than inferred from a document epoch. A ref from the previous document
also stops resolving in the new one rather than matching something else.

`TestBiDiRefsAndSameDocumentGuarantee`.

## 3. Are downloads and dialogs addressable?

**Dialogs: yes, with a capability that has to be taken at session start.**
`browsingContext.handleUserPrompt` accepts or dismisses a prompt and the answer
reaches the page — a `confirm()` returned `true` after an accept. But
`unhandledPromptBehavior` defaults to `dismiss`, which answers every dialog
before the client sees it: `userPromptOpened` still fires, and
`handleUserPrompt` then reports `no such alert`. A backend that routes dialogs
to the caller has to pass `unhandledPromptBehavior: {default: "ignore"}` to
`session.new`, and cannot change it afterwards.

**Downloads: observable, but the destination is not addressable at runtime.**
`browsingContext.downloadWillBegin` fires with the suggested filename, and
`browsingContext.downloadEnd` fires with `status` and the final `filepath`. That
is more than the extension bridge reports. What does not exist is a runtime
command for where the bytes land:

```text
browsingContext.setDownloadBehavior -> unknown command
```

The destination is a profile preference (`browser.download.dir` with
`browser.download.folderList = 2`), read at launch. So brw could report a
download and find the file afterwards, but `brw_set_download_path` — a
per-daemon destination set after the browser is running — has no equivalent, and
neither does routing two concurrent tabs' downloads to different places.

`TestBiDiDownloadsAndDialogs`.

## 4. Can artifacts be captured within the same bounds?

**Yes.** `browsingContext.captureScreenshot` returns a PNG, full page or clipped
to a box, which is the bound `brw_screenshot {ref}` needs.
`browsingContext.print` returns a PDF. Both were decoded and checked for their
file magic rather than for a non-empty string.

`TestBiDiCapturesArtifacts`.

## Decision: defer

**Named blocker: Firefox marks every page as automated for as long as the remote
agent is enabled, and there is no way to switch that off per session.**

`navigator.webdriver` is `true` on every page while `--remote-debugging-port` is
in effect, with no BiDi client connected at all. Measured both ways: with the
remote agent on a page reports `true`, with it off the same page reports
`false`, and neither run connects a client.

brw exists to drive the browser you are already signed into. A lane that tells
every page of that session it is automated is a lane whose whole point has been
removed: sites that gate on `navigator.webdriver` see an automated session, and
the human's own browsing in that window is marked too. This is a Firefox-side
choice, not something brw can work around from the client, so the blocker is
named and the work waits on it rather than being cancelled — the four questions
above all came back positive, and if Firefox scopes the flag to a session the
prototype is the start of a lane.

`TestFirefoxRemoteAgentMarksEveryPageAutomated`.

Two further costs, recorded so a later attempt does not rediscover them:

- **BiDi is a startup switch.** brw would have to launch Firefox rather than
  attach to the one you have open, which is the direct-CDP shape, not the
  signed-in-browser shape. Firefox has no `chrome.debugger` equivalent, so there
  is no extension lane to fall back on.
- **Per-call download routing cannot be reproduced** (question 3), so a recipe
  that stages a download in brw's cache could not make the same guarantee on
  this backend.

That last point is why the capability matrix is stated **per backend**. Each
transport declares what it can do — a durable CDP session, a browser target,
extension APIs, whether it drives the profile its user is signed into — in
`internal/brwidentity/transports.go`, and each capability-gated tool declares
what it needs, in `internal/mcp/transport_catalogue.go`. What a lane advertises
is derived from the two. A Firefox lane would state its own properties once and
every tool's availability would follow, rather than each tool growing a "except
on Firefox" clause; a test enumerates transports against tools and fails on a
pair nobody has classified.

## What the prototype is not

`internal/bidi` is not a `browser.Controller` and nothing in brw's tool surface
routes through it. No capability is advertised on its behalf, and `brwctl setup`
offers no Firefox lane. It is measurement code, kept because the measurements
are the reason the decision is what it is — and because the tests fail if
Firefox changes any of the answers.
