# Chrome Web Store listing (brw extension)

Everything needed to publish the `brw` extension as an **unlisted** Chrome Web
Store item. Visibility = Unlisted: installable via a direct link, not shown in
search. ~$5 one-time developer registration.

## Assets (generated, in `dist/web-store/`, gitignored)

| File | Use |
|------|-----|
| `brw-extension-0.1.0.zip` | Upload as the package (manifest at zip root). |
| `brw-store-promo-1280x800.png` | Screenshot / promo tile (1280×800). |
| `store-icon-128.png` | Store icon (128×128). |

Regenerate the zip: `python3` over `extension/` (manifest at root, excludes
README + dotfiles). The extension already pins its public `key`, so the
published id is the stable **`amocjcgddnoakjijfggdpnefdnboilpe`**.

## Listing fields

**Name:** `brw`

**Summary** (≤132 chars):
> Semantic browser control for AI agents. Drives your real, signed-in Chrome through a local brw daemon. Open source, AGPL-3.0.

**Category:** Developer Tools

**Language:** English

**Single purpose:**
> brw lets a locally-running AI agent control the user's visible Chrome tabs through a local brw daemon, so the agent can browse, read, and act on the web on the user's behalf.

**Detailed description:**
> brw is the Chrome side of brw — semantic browser control for AI agents.
>
> It connects only to a brw daemon running locally on your own machine
> (ws://127.0.0.1) and uses Chrome's debugger protocol to drive visible tabs on
> your instruction: open pages, read content, click, type, and report back a
> plain observation after each action. Agents act from stable refs instead of
> re-reading screenshots every turn.
>
> Because it bridges your real, signed-in Chrome profile, agents can work on the
> pages you're already logged into — without brw ever reading or exporting your
> cookies, passwords, or passkeys. It is a normal, visible browser: no stealth,
> no CAPTCHA/MFA bypass, no cookie extraction.
>
> Works with any agent harness that speaks MCP or HTTP (Claude Code, Codex,
> Cursor, opencode, pi, Gemini, or your own client).
>
> Open source under AGPL-3.0. Daemon + docs: https://github.com/Don-Works/brw
> Site: https://brw.donworks.co.uk

**Privacy policy URL:** https://brw.donworks.co.uk/privacy

**Homepage URL:** https://brw.donworks.co.uk

## Permission justifications (CWS asks per-permission)

- **debugger** — Core function: drive visible tabs via the Chrome DevTools Protocol (navigate, read, click, type) on the user's instruction, relayed from the local brw daemon.
- **tabs / activeTab / tabGroups** — Enumerate and organise the tabs brw is operating on.
- **notifications** — Alert the user at human-handoff points (MFA, CAPTCHA, purchase confirmation) and on completion or error.
- **webNavigation** — Observe navigation events to know when a page has finished loading before acting.
- **alarms** — Keep the local WebSocket connection to the daemon alive in MV3.
- **storage** — Persist local connection settings.
- **offscreen** — Maintain the WebSocket connection from an offscreen document (MV3 service-worker lifetime workaround).
- **Host permissions `127.0.0.1` / `localhost`** — Connect only to the local brw daemon. No remote hosts.

## Data usage disclosures (CWS form)

brw collects **no** user data. For each category (PII, health, financial,
authentication, personal communications, location, web history, user activity,
website content) select **not collected**. Then certify:

- ☑ I do not sell or transfer user data to third parties (outside approved use cases).
- ☑ I do not use or transfer user data for purposes unrelated to the item's single purpose.
- ☑ I do not use or transfer user data to determine creditworthiness or for lending.

This matches https://brw.donworks.co.uk/privacy — the extension talks only to a
local daemon and transmits nothing off the machine.

## Publish steps

1. https://chrome.google.com/webstore/devconsole → register (~$5 one-time) if needed.
2. **New item** → upload `dist/web-store/brw-extension-0.1.0.zip`.
3. Fill the fields above. Upload the screenshot + store icon.
4. **Visibility → Unlisted.** Set the privacy policy URL.
5. Submit for review. (The `debugger` permission draws extra scrutiny + a blunt
   permission warning for users — expected for browser control.)
6. After approval, copy the item URL into `brw-site/app/page.tsx`
   (`chromeStoreUrl`) to switch on the "Add to Chrome" button, and add it to
   `docs/install.md` + `extension/README.md`.
