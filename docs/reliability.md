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

## Making a new extension build live

`chrome.runtime.reload()` reloads an unpacked extension from disk without
restarting the browser or losing tabs. Drive it with brw:

```
brw_open({url: "chrome-extension://amocjcgddnoakjijfggdpnefdnboilpe/options.html"})
brw_evaluate({expression: "chrome.runtime.getManifest().version"})   // confirm what is RUNNING
brw_evaluate({expression: "chrome.runtime.reload(); 'go'"})          // bridge drops; expected
```

Then re-check `/status` (or the daemon log) for the new `build`. Note that
`make install-mac` only refreshes the `extension/` copy (Chrome work profile);
the `extension-chromium/` copy used by the Chromium profiles must be synced
manually, **without** overwriting its per-copy `bridge-defaults.json`.
