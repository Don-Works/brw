---
id: 01KV13YSQ5XZT7QV4TT8KRS8A3
schema: task/v1
workspace: agent-browser
title: Create public static browser automation conformance suite
status: done
priority: high
pinned: false
assignee:
  origin_kind: local
source:
  kind: agent
  session_id: e35c872e-ae0e-4910-9901-bd30cbc0347d
status_history:
  - at: 2026-06-13T18:29:55.557962Z
    evt: created
    to: open
    by_session: e35c872e-ae0e-4910-9901-bd30cbc0347d
  - at: 2026-06-14T08:37:07.231911Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:39:40.187963Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-13T18:29:55Z
updated_at: 2026-06-14T08:39:40Z
---

Create a reusable static HTML browser-automation conformance suite, likely as a separate repo that can be hosted on GitHub Pages and also vendored into agent-browser tests. It should exercise generic browser automation capabilities without relying on live third-party sites: forms, submit/results pages, popup/window flows, popup form submit with opener-visible result, file upload, shadow DOM, delayed/dynamic DOM, SPA route changes, iframes, disabled/hidden controls, aria-live updates, autocomplete/datalist, scroll/viewport targets, and keyboard interactions. Each fixture should expose both human-visible results and machine-readable expected output, e.g. JSON in a results panel or data attributes, so an AI/browser automation client can prove it performed the intended action. Acceptance: static-only assets, no backend required; GitHub Pages compatible; browsercheck/agent-browser scenario integration; public docs showing expected assertions; CI can run the suite locally and against the hosted URL; designed to be useful to other browser automation MCP/browser agents too.

## Notes
- 2026-06-13 (agent): Origin: user suggested a static HTML test suite for automated browsers with tricky interactions and explicit result/expectation outputs, potentially hosted publicly on GitHub Pages and useful outside agent-browser.
