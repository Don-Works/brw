# Staying connected — why brw stopped "going away"

Agents that drive brw over long sessions used to hit intermittent
`extension bridge is not connected; load/click the Chrome extension first`
errors — reported as brw "going away" or "disconnecting". This document explains
the failure mode and the layered defense that fixes it, plus how to verify and
recover.

## The failure mode

brw's extension runs as an **MV3 service worker**. Chrome aggressively terminates
idle service workers (the ~30s idle timer), and macOS **App Nap / Energy Saver**
can freeze the whole extension while Chrome is in the background. When the worker
dies it closes the daemon WebSocket with close code **1001 `StatusGoingAway`** —
which is exactly what the daemon logs:

```
extension bridge disconnected: failed to get reader: received close frame: status = StatusGoingAway and reason = ""
```

Until the worker respawns and re-handshakes, every agent call fails with
`errBridgeNotConnected`. Most respawns take 3–11s, but a frozen extension can
stay dead for many minutes (one observed work-profile outage lasted 97 minutes
and only a daemon restart recovered it). That window is the "brw disconnected"
an agent sees.

## The defense (three layers + a safety net)

### 1. A load-bearing offscreen keepalive — `extension/offscreen.js`

The offscreen document is exempt from the MV3 idle kill, so it keeps the worker
alive. Two mechanisms, because either alone has a gap:

- **Genuine (silent) looping audio.** An offscreen document is only exempt from
  teardown while its declared reason (`AUDIO_PLAYBACK`) is *actively in use*. A
  document that merely claims the reason but plays nothing gets reaped — and once
  it is gone, nothing resets the worker's idle timer. So it runs a real WebAudio
  graph (oscillator → **zero-gain** node → destination): inaudible, but the audio
  session is genuinely live. A running audio session also tells macOS the process
  is doing active work, which **blunts App Nap / Energy Saver** freezing.
- **A long-lived port to the worker.** `chrome.runtime.connect({name:"brw-keepalive"})`,
  re-opened whenever it drops. An open runtime port keeps the worker non-idle
  continuously (no 20s-vs-30s race), and reconnecting re-pins a worker that just
  respawned. The periodic message is a belt-and-braces nudge.

The worker side accepts this port in `service_worker.js`
(`chrome.runtime.onConnect` for `brw-keepalive`) and touches the bridge on
(re)connect so a freshly respawned worker reconnects immediately.

### 2. A reconnect grace wider than the reconnect — `internal/extensionbridge/bridge.go`

`bridgeReconnectGrace` (12s) bounds how long a call parks waiting for the worker
to come back before failing. It was 3s — shorter than the *typical* 3–11s
respawn, so routine going-away events surfaced as "not connected" even while the
worker was on its way back. 12s rides out essentially every routine event; the
overall op deadline (`b.timeout`, 20s) still applies, and idempotent reads retry
once after a transient drop. The extension's own reconnect backoff
(`MAX_RECONNECT_DELAY_MS`) is capped low (3s) so reconnects are fast.

### 3. Disable macOS App Nap for the browsers (operator setup)

App Nap can freeze a backgrounded browser's extension entirely — the case behind
the multi-minute outages. Disable it per browser (takes effect on next launch):

```sh
defaults write com.google.Chrome     NSAppSleepDisabled -bool YES
defaults write org.chromium.Chromium NSAppSleepDisabled -bool YES
```

The silent-audio keepalive already resists App Nap; this default removes the
remaining risk.

### 4. Safety net — the Hammerspoon watchdog

`~/.hammerspoon/brw_watchdog.lua` (loaded from `init.lua`) polls each daemon's
`/status` every 30s. If a bridge stays disconnected for >120s **while that
profile's browser is actually open** (matched by the daemon's exact
`user_data_dir`, so an unrelated or headless Chrome never false-triggers it), it
restarts that daemon via `launchctl kickstart -k` and notifies — then backs off
for 5 minutes so it can never restart-storm. A disconnected bridge whose browser
is simply closed is expected and left alone.

Manual controls from a shell:

```sh
hs -c 'brwWatchdog.status()'   # per-bridge state: up / down / absent
hs -c 'brwWatchdog.stop()'     # pause
hs -c 'brwWatchdog.start()'    # resume
```

## Verify

Each daemon serves `/status` on its **bridge** port (not the HTTP-API port).
On max-mac the two Chromium profiles are the only bridges (the Chrome work
profile was retired 2026-07-21 in favour of Chromium-only):

| Profile              | bridge `/status`            |
|----------------------|-----------------------------|
| Chromium scratch     | `http://127.0.0.1:17411/status` |
| Chromium work        | `http://127.0.0.1:17511/status` |

```sh
curl -s http://127.0.0.1:17411/status | python3 -m json.tool | grep -E 'connected|build'
```

`connected: true` and the expected `build` (extension manifest version) confirm
the current keepalive code is live. The daemon log
(`~/Library/Application Support/brw/bridge-*.log`) shows each
`extension bridge connected (build X, label "...")` and every `StatusGoingAway`
close — grep it to audit disconnect frequency.

## Toolbar badge and popup (extension 0.4.5+)

The toolbar icon is the live operator signal — no need to open Options to know
whether the bridge is healthy. One lexicon everywhere (badge tooltip, popup,
Details panel):

| Badge | Lexicon | Meaning |
|-------|---------|---------|
| solid green **`on`** | **Idle** | Connected, ready, no agent driving |
| magenta pulse **`act`** | **Agent active** | Agent driving a real page (last ~10s) |
| amber flash **`…`** | **Reconnecting** | Connecting / reconnecting |
| solid red **`off`** | **Down** | Disconnected or error |

Green is reserved for verified Idle only — Agent active is brand magenta so the
two states never collide. Click the icon for a compact status popup: healthy
views stay collapsed (status + profile + Options); Details (socket/daemon/port +
badge key) expand when Down or Reconnecting. Reconnect polls until the bridge is
verified up (or 12s). After a prior successful connection, a drop that lasts
>12s also raises a desktop notification (rate-limited to once per 5 minutes).

## Tabs brw cannot drive (extension 0.4.6+)

Chrome refuses `chrome.debugger` access to **another extension's** pages, to
`chrome://` / `devtools://` surfaces, and to the Web Store. The refusal reads:

```
extension bridge: Cannot access a chrome-extension:// URL of different extension
```

This was reported as "brw is degraded" or "Bitwarden popups intermittently break
evaluate", and it is neither degradation nor intermittent. A password manager
that pops its vault out into its own **focused** window (Bitwarden's unlock and
passkey prompts do this on their own) becomes the authoritative foreground tab.
Every no-`tab_id` tool — evaluate, read, snapshot, click — then fails with that
error for as long as the popout is up. The bridge is perfectly healthy
throughout, which is what makes it read as random flakiness.

Three layers now keep brw off those tabs:

1. **`isAgentDrivableUrl` gates foreground resolution.** `resolveForegroundTabId`
   skips a candidate it cannot drive at every step (agent pin, focused window,
   last-focused window, cache, any-active) and falls through to a usable tab.
   brw's OWN extension pages stay drivable — that is how a new build is made live
   (below). `publishActiveTab` applies the same guard so a focus event cannot
   poison the cache fallback.
2. **`list_tabs` reports no active tab when none is drivable.** It previously fell
   back to Chrome's raw per-window active flag, and the daemon caches
   `Active && WindowFocused` as its active tab — so the popout came straight back
   through that path even after resolution learned to skip it.
3. **`get_active_tab_id` returns the reason.** The daemon distinguishes a
   transient failure (MV3 worker mid-reconnect → retry, then trust the cache)
   from a definitive "nothing is drivable" (never fall back — the cached id is
   very likely that same tab). See `isNoDrivableTabReason` in `bridge.go`.

The agent-facing error is now actionable rather than Chrome's raw refusal:

```
no drivable tab: the active tab is chrome-extension://<id>/popup/index.html,
which Chrome does not allow brw to control. Switch to a normal page tab, or
pass an explicit tab_id.
```

An explicit `tab_id` still targets these tabs, and `list_tabs` still lists them —
nothing became unreachable, brw just stopped *choosing* them.

In the usage ledger this class is `tab_not_drivable` (DevTools contention is
`debugger_conflict`). Both previously landed in the catch-all `other` bucket,
which is why a recurring, very fixable outage was invisible:

```
grep '"error_class":"tab_not_drivable"' \
  ~/Library/"Application Support"/brw/usage/*-bridge.ndjson | wc -l
```

## Making a new extension build live

`chrome.runtime.reload()` reloads an unpacked extension from disk without
restarting the browser or losing tabs. Drive it with brw:

```
brw_open({url: "chrome-extension://amocjcgddnoakjijfggdpnefdnboilpe/options.html"})
brw_evaluate({expression: "chrome.runtime.getManifest().version"})   // confirm what is RUNNING
brw_evaluate({expression: "chrome.runtime.reload(); 'go'"})          // bridge drops; expected
```

Then re-check `/status` (or the daemon log) for the new `build`. `make
install-mac` refreshes the canonical extension and every existing
`extension-*` profile copy with `make sync-installed-extensions`. The sync
preserves each copy's local `bridge-defaults.json` while deleting stale source
files. Run the sync target directly after an extension-only development change.
