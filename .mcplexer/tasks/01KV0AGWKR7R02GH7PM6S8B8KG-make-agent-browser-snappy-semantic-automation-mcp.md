---
id: 01KV0AGWKR7R02GH7PM6S8B8KG
schema: task/v1
workspace: agent-browser
title: Make agent-browser snappy semantic automation MCP
status: done
priority: high
tags:
  - agent-browser
  - performance
  - mcp
  - browser-automation
pinned: false
assignee:
  origin_kind: local
composes:
  - 01KV0AGWKXKXXNSNKPJXCSQ9H9
  - 01KV0AGWMVJR87197JXG2SM0HN
  - 01KV0AGWN2Q2KK6T9Q0AWXV5HT
  - 01KV0AGWNAC09TWBRCMM69KW2B
  - 01KV0AY3PPTV5JT1ENCJ2VE6HA
  - 01KV0AY3Q2JKJX5PXG6EKX959M
  - 01KV0B84NTGR22QSQSYTPXVB5T
  - 01KV0BC67AZ2X6XRWT3H1WJYZ6
  - 01KV0BDPAPFV2H1MVZT8SQEWAS
  - 01KV0CFKGA4PQ78GXPG6DEPDG8
  - 01KV0CFKGXKVKSSNTF2WTXV31J
  - 01KV0CFKHATC0SG83AQAGHTX4Q
  - 01KV0CFKHMD922HM0WT9XPKS4K
  - 01KV0DCJCR1EZ37FQZ44GW0BJT
  - 01KV0DN8THJ5Z09J6MDR0W44ZY
  - 01KV0DN8TXVGG1N18635PM18GW
  - 01KV0DN8V6R0W0CW6SXADQSB3G
  - 01KV13YSQ5XZT7QV4TT8KRS8A3
  - 01KV141TDBK1NV0BAB6QNDVDRS
  - 01KV14NWX71KNYKN1KQ4RNCXCD
  - 01KV15QQGX3YSW26CHXY19GQ30
meta:
  touches_files:
    - agent-browser/internal/snapshot/types.go
    - agent-browser/internal/snapshot/scripts.go
    - agent-browser/internal/browser/types.go
    - agent-browser/internal/browser/manager.go
    - agent-browser/internal/extensionbridge/bridge.go
    - agent-browser/internal/mcp/server.go
    - agent-browser/internal/http/server.go
    - agent-browser/cmd/browsercheck/main.go
    - agent-browser/docs/efficiency-roadmap.md
source:
  kind: agent
  session_id: f8ed800b-c98d-4403-a5ba-ea03a0447722
status_history:
  - at: 2026-06-13T11:05:25.367987Z
    evt: created
    to: doing
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:05:25.367987Z
    evt: assigned
    to: f8ed800b-c98d-4403-a5ba-ea03a0447722
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:05:25.39612Z
    evt: composed
    to: 01KV0AGWKXKXXNSNKPJXCSQ9H9
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:05:25.404008Z
    evt: composed
    to: 01KV0AGWMVJR87197JXG2SM0HN
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:05:25.411186Z
    evt: composed
    to: 01KV0AGWN2Q2KK6T9Q0AWXV5HT
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:05:25.418343Z
    evt: composed
    to: 01KV0AGWNAC09TWBRCMM69KW2B
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:05:58.159628Z
    evt: status_changed
    from: doing
    to: open
    note: lease expired, demoted from working status
  - at: 2026-06-13T11:05:58.159628Z
    evt: lease_expired
  - at: 2026-06-13T11:06:39.832248Z
    evt: status_changed
    from: open
    to: doing
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:06:39.832248Z
    evt: assigned
    to: f8ed800b-c98d-4403-a5ba-ea03a0447722
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:11:58.094176Z
    evt: status_changed
    from: doing
    to: open
    note: lease expired, demoted from working status
  - at: 2026-06-13T11:11:58.094176Z
    evt: lease_expired
  - at: 2026-06-13T11:12:38.615341Z
    evt: composed
    to: 01KV0AY3PPTV5JT1ENCJ2VE6HA
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:12:38.627173Z
    evt: composed
    to: 01KV0AY3Q2JKJX5PXG6EKX959M
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:16:19.454916Z
    evt: status_changed
    from: open
    to: done
    by_session: 1774dac6-8a46-4d1a-812b-56c118706e88
  - at: 2026-06-13T11:16:19.454916Z
    evt: closed
    to: done
    by_session: 1774dac6-8a46-4d1a-812b-56c118706e88
  - at: 2026-06-13T11:18:07.291286Z
    evt: composed
    to: 01KV0B84NTGR22QSQSYTPXVB5T
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:20:19.967031Z
    evt: composed
    to: 01KV0BC67AZ2X6XRWT3H1WJYZ6
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:21:09.207093Z
    evt: composed
    to: 01KV0BDPAPFV2H1MVZT8SQEWAS
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:39:40.428219Z
    evt: composed
    to: 01KV0CFKGA4PQ78GXPG6DEPDG8
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:39:40.445922Z
    evt: composed
    to: 01KV0CFKGXKVKSSNTF2WTXV31J
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:39:40.458685Z
    evt: composed
    to: 01KV0CFKHATC0SG83AQAGHTX4Q
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:39:40.469083Z
    evt: composed
    to: 01KV0CFKHMD922HM0WT9XPKS4K
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:55:29.561254Z
    evt: composed
    to: 01KV0DCJCR1EZ37FQZ44GW0BJT
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T12:00:14.673894Z
    evt: composed
    to: 01KV0DN8THJ5Z09J6MDR0W44ZY
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T12:00:14.685598Z
    evt: composed
    to: 01KV0DN8TXVGG1N18635PM18GW
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T12:00:14.694406Z
    evt: composed
    to: 01KV0DN8V6R0W0CW6SXADQSB3G
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T18:29:55.560728Z
    evt: composed
    to: 01KV13YSQ5XZT7QV4TT8KRS8A3
    by_session: e35c872e-ae0e-4910-9901-bd30cbc0347d
  - at: 2026-06-13T18:31:34.571599Z
    evt: composed
    to: 01KV141TDBK1NV0BAB6QNDVDRS
    by_session: e35c872e-ae0e-4910-9901-bd30cbc0347d
  - at: 2026-06-13T18:42:32.490119Z
    evt: composed
    to: 01KV14NWX71KNYKN1KQ4RNCXCD
    by_session: e35c872e-ae0e-4910-9901-bd30cbc0347d
  - at: 2026-06-13T19:01:01.089231Z
    evt: composed
    to: 01KV15QQGX3YSW26CHXY19GQ30
    by_session: 920d1707-1199-4611-a29e-71740df38255
created_at: 2026-06-13T11:05:25Z
updated_at: 2026-06-13T19:01:01.089243Z
---

Implement first pass toward near-realtime semantic browser use: compact MCP output, targeted find/fill, snapshot options, lazy AX, post-action observations, and local + max-air Decathlon checkout-path smoke test.

## Notes
- 2026-06-13 (agent): Created while implementing the efficiency pass. Local tests/build are currently green; local daemon smoke and max-air Decathlon e2e are next.
- 2026-06-13 (agent): Local fixture smoke passed on 127.0.0.1:17319: browser_find, filtered browser_snapshot, browser_fill, and action observations all returned expected semantic payloads. Max-air Decathlon shoe stock/basket test is active now.
- 2026-06-13 (agent): User correctly rejected direct stdio workaround. Acceptance criterion tightened: configured mcplexer agent_browser downstream itself must expose browser_find/browser_fill and run the Decathlon max-air flow through normal mcplexer MCP calls.
- 2026-06-13 (agent): Snappy semantic automation MCP surface is present in mcplexer: browser_find and browser_fill are exposed, action results include compact observations, and screenshots remain fallback-only.
- 2026-06-13 (agent): Created blocking sign-off task 01KV0B84NTGR22QSQSYTPXVB5T: Selenium form -> Decathlon S6 1TX stock/basket -> intervals.pro. Related implementation/verification tasks are marked blocked pending this gate.
- 2026-06-13 (agent): Generic speed pass follow-ups added after Decathlon friction: event journal, frontier-smart action observations, semantic field/autocomplete commit, and hardened ref re-resolution. Existing browser_batch task remains the native transaction primitive.
