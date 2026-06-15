---
id: 01KV5AGZYNKGEQPPDJ149K1X9Y
schema: task/v1
workspace: agent-browser
title: 'Full delivery: review→build→test→live acceptance (34" running shorts checkout)'
status: review
priority: high
pinned: false
assignee:
  origin_kind: local
meta:
  acceptance: live shopping flow to checkout, no purchase
  composed_by: 01KV5A3X61WX1RDN0HYDZT2GYT
  phase: delivery
  touches_files: []
source:
  kind: agent
  session_id: e38a3838-7a61-4a9e-be28-d1fa62656aad
status_history:
  - at: 2026-06-15T09:41:40.949602Z
    evt: created
    to: doing
    by_session: e38a3838-7a61-4a9e-be28-d1fa62656aad
  - at: 2026-06-15T09:41:40.980742Z
    evt: assigned
    to: e38a3838-7a61-4a9e-be28-d1fa62656aad
    by_session: e38a3838-7a61-4a9e-be28-d1fa62656aad
  - at: 2026-06-15T09:47:33.334195Z
    evt: status_changed
    from: doing
    to: open
    note: lease expired, demoted from working status
  - at: 2026-06-15T09:47:33.334195Z
    evt: lease_expired
  - at: 2026-06-15T10:47:25.413574Z
    evt: status_changed
    from: open
    to: review
    by_session: 59050316-1ea7-4a5b-9634-46aa6a29fb4f
created_at: 2026-06-15T09:41:40Z
updated_at: 2026-06-15T10:47:25Z
---

Capstone delivery for the browser-optimisation epic. Phases: (1) review epic spec, (2) verify build green, (3) run browsercheck/tests, (4) live acceptance — drive Chrome via agent_browser MCP to clear basket, find a well-reviewed pair of men's 34" waist running shorts, add to basket, proceed through to checkout WITHOUT purchasing. Fix any browser-MCP bugs surfaced during the live drive.

## Notes
- 2026-06-15 (agent): Phases 1-3 verified: (1) spec reviewed — epic = generic browser actionability/ergonomics/speed, no ecommerce-only heuristics; (2) `make build` green (4 binaries); (3) `go test ./...` all pass (browserctl, browser, extensionbridge, http, mcp, profilepolicy, store). Live browser = user's REAL Chrome via extension bridge (11 tabs incl Gmail/Calendar/HA). Target shop = decathlon.co.uk (prior Decathlon context + existing search tab). Next: live acceptance drive — clear basket → find well-reviewed mens 34" running shorts → add → checkout (NO purchase).
- 2026-06-15 (agent): LIVE DRIVE — BASKET CLEARED ✅ (fresh cart tab reads "No items yet / Subtotal 0 items £0.00"). Took ~12 tool calls due to friction. Generic friction observed so far (for ext+MCP usability/speed):
  1. FRESH-TAB NAV RACE: after browser_open, wait_for("load") returns ok:true against still-about:blank doc; doesn't track the new tab's pending navigation. Had to poll observe.url manually.
  2. CLICK LATENCY ~5.1s: every browser_click round-trip ~5100ms, even no-op clicks. Biggest perceived slowness — looks like a fixed post-click settle wait.
  3. find RETURNS HIDDEN/DUP REFS: find("remove") gave refs (removeItem e14/e16) that click rejected "not found or not visible"; find result lacked visible/in_viewport flags. frontier snapshot gave the right visible ref.
  4. SILENT NO-OP CLICK: CDP clicks on decrement(e10) + snapshot "Delete"(e17) returned success+post-action frontier but did NOT fire the site handler (item stayed). No signal handler didn't fire.
  5. TRANSPORT SPLIT: browser_click_text failed "extension bridge is not connected" while CDP click/read/evaluate/snapshot worked on the same tab same instant. Two transports, divergent health, no auto-fallback.
  6. STALE SEMANTIC CACHE (ROOT CAUSE): observe.version stuck at 1 across real DOM mutations; browser_read kept returning pre-mutation main-text ("1 item") long after live DOM (evaluate) showed "0 item". read/snapshot cache keyed on a version that never bumps on SPA mutations.
  7. evaluate VOID ERROR: evaluate("location.reload()") throws "runtime evaluation returned no by-value result" instead of treating undefined as success.
  Removal ultimately required an evaluate .click() on the real "Remove" vp-link, then a fresh-tab reload to read clean state.
- 2026-06-15 (agent): ITEM IN BASKET ✅ (verified on /checkout/cart): "KIPRUN Men's Running Breathable Shorts Dry+ - Black", smoked black, Size UK 34"/EU L, qty 1, £12.99 (sale, was £14.99), 4.8★ from 13,194 reviews. Well-reviewed + 34" waist + basket-cleared-first all satisfied.
  
  DECISIVE ROOT-CAUSE EVIDENCE (us vs site): same custom combobox — browser_click(ref)=5.1s and DID NOT open; synthetic JS dispatchEvent updated DOM display but NOT React state (untrusted, isTrusted=false, so 'Add to basket' CTA never mounted); browser_click_xy (TRUSTED CDP coord click)=opened+selected in 13-30ms and React registered → CTA appeared. Conclusion: it's OUR actuation path, not Decathlon (a standard React/Vitamin vp-* site). browser_click also has a consistent ~5.1s fixed latency vs click_xy ~15-30ms. Add-to-basket via browser_click(ref) DID work (standard vp-button) but still 5.15s.
  
  GENERIC FIXES (no site-specifics): (1) route ALL actuation through trusted CDP input events at hit-tested coords, never element.click()/dispatchEvent; (2) Playwright-grade actionability gate (scroll-in, stable, visible, enabled, occlusion hit-test); (3) replace ~5s fixed settle with event-driven quiet-window; (4) generic ARIA custom-widget driver (combobox/listbox/menu open→pick-by-name); (5) post-action effect-confirmation (observable delta or 'dispatched, no effect'); (6) fix semantic cache version-not-bumping. Maps onto the 7 friction child tasks.
- 2026-06-15 (agent): ACCEPTANCE TEST COMPLETE (no purchase) ✅
  - Basket cleared first ✅
  - Found well-reviewed pick: KIPRUN Men's Running Breathable Shorts Dry+ Black, 4.8★ / 13,194 reviews
  - Selected 34" waist (UK 34" / EU L) ✅, added to basket, £12.99 (sale)
  - Initiated checkout → reached member/guest identification gate ✅
  - STOPPED before entering any personal details or payment (won't fabricate user PII; no purchase)
  
  EXTRA FINDING (checkout gate): the guest 'identification' drawer renders OFF-CANVAS at x=2594 while innerWidth=2319 — a mispositioned React/Vitamin portal (same class as the size-combobox dropdown rendering at viewport 0,0). browser_click(ref) reported 'clicked' but didn't actuate it; click_xy can't reach off-viewport coords; scroll/zoom don't move the fixed drawer. Generic: portal/drawer elements rendering outside the reported layout viewport break coordinate clicks → need ref-actuation that works regardless of on-screen position (focus+trusted key activation, or element-centroid CDP click after ensuring in-viewport).
  
  Next: FIX phase per user — start with highest-impact generic root cause (browser_click ~5.1s latency + trusted actuation).
- 2026-06-15 (agent): LIVE-VERIFY BLOCKED by mcplexer route-wedge. Sequence: committed fixes (88d5159, branch fix/browser-click-latency-actuation) -> make install-mac (new binary in app dir) -> kill old daemon 63809 to force respawn. mcplexer respawned daemon (PID 81052, new binary, confirmed via pgrep) BUT the session's execute route to agent-browser is wedged ('failed to acquire lock: context canceled'); reload_server refreshes catalog only. So I could not run the live 5.1s->ms click re-test this session. Fix is committed+deployed; void-eval probe confirmed OLD binary was live pre-kill (errored on 'void 0'), and new binary is now the running process. Verification still rests on build+test green + the in-session live evidence that click_xy (identical in-page mechanism) was 13-30ms vs browser_click 5.1s. Recorded route-wedge as repro on task 01KV18MXNNG4Z46YRRRH06HE4R.
- 2026-06-15 (agent): TURN WRAP STATE:
  DONE: (1) acceptance test — basket cleared, 4.8★/13,194-review men's 34" (UK34/EU L) Dry+ short added, checkout reached, no purchase; (2) 3 root-cause fixes committed (88d5159, branch fix/browser-click-latency-actuation) + deployed (make install-mac, daemon PID 81052 runs new binary); (3) us-vs-site + MCP-honesty + speed brainstorm answered to user.
  IN FLIGHT: grok worker del-1e3a0d3b building a browsercheck benchmark scorecard (task 01KV5CKY1ZVK9TX3NXZPRP5VGE) — running, review on return.
  BLOCKED: live-verify (mcplexer execute-route wedged by mid-session daemon kill; recover via new session/gateway restart). 
  NOT STARTED: API/fetch shortcut prototype (browser_api_observe/replay + __NEXT_DATA__ read) — to delegate next.
  Friction child tasks: 3 in review (clickRef latency, actuation, evaluate void), 4 open (find visibility, transport unify, cache version-bump, post-action effect-confirm).
- 2026-06-15 (agent): Step 3 done: daemon restarted cleanly (pid 79468, new binary; mcplexer respawned + reload_server re-introspected — route did NOT wedge). browser_read_data LIVE on Decathlon 333374: name='Men''s Running Breathable Shorts Dry+ - Black', price=12.99 GBP, availability=OnlineOnly, images — 2-4ms, ~1.1KB, picks MAIN product correctly. GAP FOUND: rating + reviews come back undefined though page shows 13194 reviews — extractor (snapshot.EvaluateStructured) misses aggregateRating/reviewCount. Fixing next.
