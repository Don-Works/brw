---
id: 01KV5H81ZAC7V98QRTX03XWYGC
schema: memory/v1
kind: note
name: root-cause-cost-real-debugging-time-8d25bd8d
workspace: agent-browser
tags:
  - agent-browser
  - chrome
  - performance
  - throttling
  - cdp
  - root-cause
  - debugging
pinned: false
source:
  kind: agent
  session_id: eec9898e-0967-4c8d-a37f-bf0d3612b52d
created_at: 2026-06-15T11:39:08Z
updated_at: 2026-06-15T11:39:08Z
---

ROOT CAUSE (cost real debugging time): Chrome throttles in-page setInterval/setTimeout to ~1Hz AND fully PAUSES requestAnimationFrame on hidden/headless/backgrounded pages. agent-browser's WaitForActionable used setInterval(100ms) for box-stability → silently became ~700-900ms PER CLICK on the direct-CDP/headless path (bench). Diagnosed via in-page __waDiag (ticks:2 but ms:841). FIX (committed 51329fa): (1) re-pace stability with a short setTimeout(16) chain bounded by an in-page perf.now() deadline — NOT setInterval, NOT rAF; (2) add --disable-background-timer-throttling --disable-backgrounding-occluded-windows --disable-renderer-backgrounding to cdp/launcher.go (standard automation flags). Result: direct-CDP clicks ~900ms→172ms. CAVEAT: launch flags only reach Chromes WE launch (direct-CDP / agent-revitt profile). The extension-bridge path into the user's real Chrome can't get flags — but its clicks bypass WaitForActionable (bridge.clickRef) and its waits are MutationObserver-driven, so it's largely immune. Lesson: for the bridge/background-tab path, prefer event-driven waits (MutationObserver/CDP events) over any in-page timer poll.
