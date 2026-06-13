# Efficiency Roadmap

This note captures a speed and token-efficiency pass over the current
`agent-browser` daemon. The product goal is simple: keep full browser state in
the daemon, then show the model only the smallest semantic frontier needed for
the next decision.

## Current Bottlenecks

- `browser_snapshot` is whole-page and pull-based. It runs `SnapshotScript`,
  walks document plus open shadow roots, recomputes roles/names/visibility, and
  returns every visible candidate.
- Direct CDP snapshots call `Accessibility.getFullAXTree` every time. That is
  useful enrichment, but expensive and rarely needed on every action loop.
- `browser_read` re-runs a snapshot just to refresh refs, then runs readability
  extraction. That doubles page evaluation work on content-heavy pages.
- `browser_wait_for` polls every 250 ms and repeatedly reads broad page state
  such as `document.body.innerText`.
- Actions resolve refs by scanning all roots again for
  `data-agent-browser-ref`, then compute boxes at action time.
- MCP tool results serialize structured data as indented JSON inside text
  content. For snapshots and reads this wastes tokens before any real page data
  appears.
- The extension bridge already has a WebSocket, but it is command/response only.
  It does not stream tab, mutation, focus, viewport, network, or live-region
  changes back to the daemon.

## North Star

Install a small page kernel once per document. The kernel owns:

- stable semantic refs
- an element and region index
- bounding-box and viewport state
- a mutation/change journal
- focused element and active form state
- live-region, route, title, URL, and navigation signals

The model should usually see action observations and deltas, not fresh full
snapshots. Full snapshots become a reset/debug tool.

## P0 Quick Wins

1. Compact MCP output.
   Replace `json.MarshalIndent` with compact JSON for tool text, and add
   `structuredContent` for clients that support it. Keep text fallback for
   compatibility.

2. Add targeted query tools.
   Add `browser_find({query, role, text, limit})`, `browser_fill({ref|query,
   text, replace:true})`, and optionally `browser_click_query` for high-confidence
   exact matches. This makes common flows skip full snapshots.

3. Add snapshot arguments.
   Evolve `browser_snapshot` to accept `mode`, `query`, `limit`, `viewport_only`,
   `include_ax`, `since`, and `max_bytes`. Defaults should be compact, viewport
   and changed-first.

4. Return observations from actions.
   `browser_click`, `browser_type`, `browser_select`, `browser_press`, and
   `browser_scroll` should return a concise post-action observation:

   ```json
   {
     "ok": true,
     "version": 42,
     "url": "https://example.com/checkout",
     "title": "Checkout",
     "focus": "e17",
     "changed": [
       "+ button e31 \"Place order\"",
       "~ textbox e17 value:\"max@example.com\"",
       "+ status \"Saved\""
     ]
   }
   ```

   That removes the common action -> wait -> snapshot round trip.

5. Make AX lazy.
   Default `include_ax:false`. Cache and throttle full AX summaries, and expose a
   focused `browser_ax({ref})` or `browser_snapshot({include_ax:true})` for cases
   where accessibility data materially helps.

6. Make waits event-driven.
   Replace polling loops with an in-page promise backed by `MutationObserver`,
   `popstate`/history wrappers, load events, focus/input events, and optional CDP
   lifecycle/network signals. Poll only as a fallback.

## P1 Changed Parts

The strong version of "changed DOM parts" is a semantic journal, not raw DOM
mutation spam.

### Page Kernel

Inject once through direct CDP with `Page.addScriptToEvaluateOnNewDocument`, and
through the extension bridge for installed profiles. Expose tiny calls such as:

- `window.__agentBrowser.snapshot(opts)`
- `window.__agentBrowser.find(opts)`
- `window.__agentBrowser.drain(since, opts)`
- `window.__agentBrowser.resolve(ref)`
- `window.__agentBrowser.wait(condition, timeout)`

### Semantic Atoms

Represent each useful element as a compact atom:

```json
["e17","textbox","Email","input","email","",1,1,0]
```

The daemon can expand this to named fields for HTTP/debug, but MCP should prefer
compact or structured output.

### Parts And Deltas

Partition the page into stable semantic parts:

- document
- frame
- dialog
- form
- nav
- table
- list
- live region
- viewport band
- shadow root host

Each part gets an id, parent id, role/name summary, content hash, and child count.
The model receives a top-level atlas first, then asks to expand a part only when
needed.

The journal should emit:

- `add`: new semantic atoms
- `update`: name/value/checked/disabled/visibility/box/focus changes
- `remove`: refs no longer present
- `part`: region hash or summary changed
- `live`: live-region/status text
- `nav`: URL/title/history/load changes
- `viewport`: visible refs entered/exited
- `network`: coarse pending/idle/error state

Network observation should start as passive context: pending request count,
recent failed requests, navigation requests, and coarse API endpoint hints. Direct
API-call synthesis is a later optimization and should only happen when the
browser has already exposed a clear same-origin request shape and the action is
safe to mirror without bypassing visible user controls.

### Daemon Cache

Keep per-tab state in Go:

- current version
- ref map and stale-ref resolver
- part tree
- atom index for `browser_find`
- latest boxes for visible refs
- recent action observations

This cache lets tools answer from memory when the page is stable and drain only
new events when it is not.

## P2 Bigger Innovations

- Semantic frontier: every tool response includes the next most useful few facts,
  not the whole page. Prioritize focus neighborhood, live regions, changed parts,
  visible CTAs, validation errors, and newly opened dialogs.
- Intent engine: run local BM25/trigram matching over the ref index, names,
  labels, placeholders, form ownership, ARIA controls, nearby text, and URL
  context. Return ranked refs with reasons and confidence.
- Layout action map: maintain visible element boxes with `ResizeObserver` and
  `IntersectionObserver`. Clicks can use cached boxes, with a cheap freshness
  check before dispatch.
- Form lens: when focus is inside a form, return only the form schema, current
  invalid fields, submit controls, and live validation text.
- Macro runner: support deterministic compound actions like fill field, press
  enter, wait for changed part, return observation. Keep this as browser
  primitives, not hidden site bypass logic.
- Site affordance cache: persist successful query-to-ref patterns per origin and
  route hash. Use it as a hint, invalidate on DOM hash drift, and never skip
  visible confirmation for sensitive operations.
- Visual islands: detect canvas/map/chart/image-only regions and return tiny
  cropped screenshots or OCR only for those refs. Keep screenshot fallback
  scoped to the visual island, not the whole page.
- Self-healing stale refs: recover refs by stable key, role/name, form owner,
  path, and nearby text. Return recovery details so the model can see when a ref
  was re-bound.
- Speculative waits: after click/type, predict likely effects from element type
  and event history: dialog open, route change, live region update, validation
  error, new tab, or table/list change. Watch those signals first.
- Flow trace: record compact before/action/after traces. Use them for tests,
  replay, and performance metrics without re-querying the page.

## MCP Surface Shape

Recommended tool additions:

- `browser_find`
- `browser_observe`
- `browser_part`
- `browser_fill`
- `browser_action` for small deterministic batches

Recommended resource surface:

- `agent-browser://tab/{id}/frontier`
- `agent-browser://tab/{id}/parts`
- `agent-browser://tab/{id}/events`

MCP resource subscriptions can notify a capable client that browser resources
changed. Do not depend on notifications alone for model context, because many
hosts still decide when resource contents are actually fed to the model. The
robust path is action results plus `browser_observe({since})`.

## Implementation Order

1. Compact JSON, action observations, lazy AX, and `browser_find`.
2. Add snapshot args and compact atom output while keeping legacy full output.
3. Install a page kernel and refactor snapshot/find/resolve/wait through it.
4. Add mutation journal and `browser_observe({since})`.
5. Add part atlas and lazy `browser_part`.
6. Add extension unsolicited event messages and direct-CDP drain support.
7. Add form lens, visual islands, stale-ref recovery, and site affordance cache.

## Success Metrics

- Median tool round trips per common task.
- Snapshot/read response bytes and approximate token count.
- Time from user action to usable post-action observation.
- Percentage of actions that avoid a full snapshot.
- Ref recovery success rate after dynamic rerenders.
- False-positive rate for high-confidence query actions.
