# Chrome Web Store submission kit

This is the source of truth for publishing the `brw` Manifest V3 extension.
It is deliberately candid about the `debugger` permission, the local daemon,
and browser data: the store listing, in-extension disclosure, privacy policy,
and submitted code must all describe the same product.

## Release gate

Do not submit the old `0.1.0` ZIP in `dist/web-store/`. It predates the popup,
downloads support, current safety boundaries, and affirmative consent screen.

Build the current package from the repository root:

```sh
task package-web-store
unzip -l dist/web-store/brw-extension-0.5.0.zip
```

The packager puts `manifest.json` at the ZIP root and includes only runtime
files. It excludes tests, development notes, dotfiles, and local
`bridge-defaults.json` configuration.

Before uploading, load `extension/` unpacked into a clean Chrome profile and
run `task test-extension`. Check that install opens Options, no daemon
connection is attempted before the user clicks **Enable local browser
control**, Disable releases debugger attachments, and the popup explains every
state.

If a draft Web Store item already exists, compare its Item ID with
`amocjcgddnoakjijfggdpnefdnboilpe`. The `key` in this manifest keeps local and
self-hosted builds on that ID, but the Web Store item's public key is
authoritative for the store build. Do not publish with a mismatched ID: either
use the existing matching item or update the daemon trust/configuration first.

## Required listing assets

| Asset | Requirement | Current source |
|---|---|---|
| Package | Current ZIP, manifest at root | `dist/web-store/brw-extension-0.5.0.zip` |
| Icon | 128×128 PNG, with appropriate transparent padding | `extension/icons/icon-128.png` |
| Screenshots | At least one; preferably 3–5; exactly 1280×800 or 640×400 | `store-assets/brw-store-screenshot-consent-1280x800.png`; also capture the healthy popup and a real controlled-tab result |
| Small promo | 440×280, brand-led rather than a UI screenshot | `store-assets/brw-store-promo-440x280.png` (editable SVG beside it) |

The old `brw-store-promo-1280x800.png` says “Semantic browser control” and is
not an adequate screenshot of the current user experience. Replace it before
submission.

## Store listing fields

**Name:** `brw`

**Category:** Developer Tools

**Language:** English (United Kingdom)

**Summary** (132 characters maximum):

> Connect Chrome to brw for fast, local, user-authorized browser control from any AI agent, with deterministic recipes.

**Single purpose:**

> brw connects the user's visible Chrome tabs to a user-run local brw daemon so an AI agent selected by the user can inspect and control those tabs on the user's behalf.

**Detailed description:**

> brw is the Chrome bridge for fast, inspectable browser control by AI agents.
>
> After you review the in-extension data disclosure and explicitly enable browser control, brw connects only to a brw daemon running on localhost. The daemon can be used by any MCP or HTTP client you configure. There is no brw account and no Don Works or Revitt cloud service in the data path.
>
> On your instruction, brw can open and organise visible tabs; find semantic controls; read page content; click, type, fill, select, scroll, hover, drag and upload; wait for and assert outcomes; capture requested screenshots, PDFs and downloads; and expose console or network diagnostics. Proven multi-step work can be run by the daemon as versioned, origin-scoped recipes.
>
> The toolbar popup always shows whether browser control is disabled, idle, active, reconnecting or down. Debugger sessions are attached only when needed and released after inactivity or when control is disabled.
>
> The installed-profile extension blocks Chrome cookie CDP methods and bulk site-storage CDP domains, including HttpOnly cookie access. It does not access Chrome's password store, passkey store or browser profile files. It does not add stealth, CAPTCHA bypass, MFA bypass or consent bypass.
>
> All extension code is included in the package and is open source under AGPL-3.0. The extension uses Chrome's documented Debugger API to carry out browser commands received from the authenticated local daemon.
>
> Source and daemon: https://github.com/Don-Works/brw
> Product site: https://brw.donworks.co.uk

**Homepage URL:** `https://brw.donworks.co.uk`

**Privacy policy URL:** `https://brw.donworks.co.uk/privacy/extension`

**Support URL:** `https://github.com/Don-Works/brw/issues`

Start as **Unlisted** for the first controlled release. Unlisted items receive
the same review as public items. Move to **Public** when the onboarding and
support path are ready; public is the useful setting if store discovery is part
of the go-to-market plan.

## Permission justifications

Paste one concrete justification for every permission shown by the dashboard.

- **debugger** — This is the extension's core transport. On an explicit request
  from the authenticated localhost brw daemon, it attaches to a visible tab and
  uses documented Chrome DevTools Protocol commands to inspect page structure,
  perform user-requested browser actions, and report the result. Attachments are
  released after inactivity and when the user disables control. Cookie methods
  and bulk-storage domains are denied in extension code.
- **tabs** — Lists, opens, focuses, updates, and closes the visible tabs the user
  asks brw to control, and reads their URL/title so commands target the correct
  tab. It also keeps the agent's tab separate from tabs the user is operating.
- **tabGroups** — Creates or reuses a clearly named group for agent-owned tabs,
  keeping automated work visible and separate from the user's existing tabs.
- **downloads** — Observes downloads initiated by a controlled tab and
  correlates each file with that tab so a requested artifact is not confused
  with a download the user started elsewhere. It does not scan download
  history outside the bounded in-memory session buffer.
- **notifications** — Alerts the user when human action is required (for
  example MFA, CAPTCHA, or confirmation), and on requested completion or a
  sustained bridge failure.
- **webNavigation** — Observes main-frame navigation/document changes so waits,
  post-action observations, and recipe artifact capture do not act on stale
  page state.
- **alarms** — Schedules local reconnect and health checks required by the
  Manifest V3 service-worker lifecycle.
- **storage** — Stores the user's consent choice, localhost endpoints, optional
  profile labels, and connection status in `chrome.storage.local`.
- **offscreen** — Hosts only the localhost WebSocket keepalive port needed to
  maintain the bridge across Manifest V3 service-worker suspension. It does not
  render or inspect websites.
- **Host access to `127.0.0.1` and `localhost`** — Connects to the user-run brw
  daemon and its status endpoint on the same computer. No remote hostname is in
  the manifest.

`activeTab` was removed because the implementation did not use it. Do not add
permissions for planned features; Chrome requires the narrowest set needed by
the submitted version.

## Privacy practices answers

Do **not** select “does not collect user data.” Chrome defines handling to
include local processing, page scraping, screenshots, form data, URLs, and web
request data. The extension handles those things for its user-facing purpose
even though its own network hop is only to a native program on the same machine.

For this general-purpose signed-in browser controller, disclose every dashboard
category that may be present in a page or action the user asks brw to handle:

- personally identifiable information;
- health information;
- financial and payment information;
- authentication information (for example a credential the user explicitly
  asks their agent to type; Chrome's password and cookie stores remain blocked);
- personal communications;
- location;
- web history (URLs, titles, and navigation state);
- user activity (requested clicks, typing, scrolling, and action results); and
- website content (text, forms, controls, screenshots, diagnostics, PDFs, and
  requested downloads).

For each category, select only **App functionality**. Do not select advertising,
analytics, personalisation, or unrelated purposes. The accurate explanation is:

> Data is handled only when the user enables brw and asks their configured agent to perform a browser task. The extension sends requested observations and actions to a user-run daemon on localhost. Don Works and Revitt do not receive the data. If the user connects that daemon to a third-party agent or model service, the requested observations may be sent to that user-selected service under its terms.

Complete all Limited Use certifications: no sale, no advertising, no unrelated
use, no lending/credit use, and no publisher human access. The affirmative
Limited Use statement is published at `https://brw.donworks.co.uk/privacy/extension`.

The `ws://127.0.0.1` transport is intentional. Chrome's policy says the secure
transmission requirement does not apply between an extension and a native
program on the same computer. Remote brw deployments should use SSH/WSS/HTTPS
for any hop that leaves that computer.

## Manifest V3 / remote-code answer

If the dashboard asks whether brw uses remote code, answer **No** and explain:

> Every JavaScript file used by the extension is readable and packaged in the ZIP. The extension does not fetch scripts, use eval in an extension context, or load remote WebAssembly. It receives JSON browser-control commands from the user-run localhost daemon. Commands that evaluate page logic are executed only through Chrome's documented Debugger API, which the Manifest V3 policy explicitly permits for this use. The submitted service worker shows command dispatch, method validation, safety deny-lists, attachment lifecycle, and response handling in full.

Do not describe the command stream as a hidden interpreter. Point reviewers to
`service_worker.js`, especially `handle`, `isDeniedCdpMethod`, attachment idle
cleanup, and the consent gate.

## Reviewer test instructions

Include these in **Test instructions** so a reviewer can exercise the single
purpose without an account or private credentials:

> No account or login is required. On install, brw opens Options and remains disabled. Review the disclosure and click “Enable local browser control.” Without the companion daemon, the UI correctly reports “Not reachable.”
>
> Download or build the open-source daemon from https://github.com/Don-Works/brw, then run:
> `brwd --bridge --http 127.0.0.1:17310 --bridge-addr 127.0.0.1:17311`
>
> In the extension popup click Reconnect; it should show Idle and Connected. To perform a harmless browser action, run:
> `curl -s 127.0.0.1:17310/api/browser/open -H 'content-type: application/json' -d '{"url":"https://example.com"}'`
> Then run:
> `curl -s 127.0.0.1:17310/api/page/snapshot`
> A visible example.com tab opens and the second command returns its semantic controls. The toolbar changes to Agent active during work. Click Options → Disable browser control; the socket closes and debugger attachments are released.
>
> All extension source is unminified. The only permitted hosts are localhost/127.0.0.1. No test credentials are needed.

Attach a short reviewer note calling out the `debugger` permission and the
Debugger API's Manifest V3 remote-execution exemption directly; making the
reviewer infer that architecture is avoidable delay.

## Submission sequence

1. Enable 2-Step Verification on the publishing Google account and verify the
   developer contact email and publisher identity.
2. If the currently pending package is the stale `0.1.0` build or its privacy
   answers say “no data,” stop that submission and replace it with the current
   package and disclosures rather than waiting for a predictable rejection.
3. Run `task test-extension package-web-store`; load the exact staged runtime
   files unpacked in a clean profile and complete the reviewer flow above.
4. Upload `dist/web-store/brw-extension-0.5.0.zip` to the matching draft item.
5. Add the icon, 440×280 promo, and 3–5 real 1280×800 screenshots. Fill the
   listing, privacy practices, permission justifications, and test instructions.
6. Save every tab, re-open the privacy answers, and compare them line by line
   with the in-product disclosure and live privacy policy.
7. Submit as Unlisted. Expect extra review time because `debugger`, `tabs`, and
   `downloads` are sensitive permissions. Most reviews finish in days, but they
   can take weeks; contact Chrome Web Store developer support after three weeks.
8. After approval, put the item URL in `brw-site/app/page.tsx`
   (`chromeStoreUrl`) and update `docs/install.md` and `extension/README.md`.

## Rejection response

Fix the cited policy issue and submit a new version; do not repeatedly appeal an
accurate finding. If the finding appears mistaken, use the one dashboard appeal
to identify the exact file/function and the matching disclosure. Keep the
reviewer instructions, store metadata, source, and privacy policy versioned
together for every subsequent release.
