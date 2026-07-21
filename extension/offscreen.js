// brw offscreen keepalive.
//
// One goal: never let Chrome terminate the MV3 service worker. When the worker
// dies it severs the daemon WebSocket (the daemon logs a StatusGoingAway close),
// and until it respawns every agent call fails with "extension bridge is not
// connected" — which reads to an agent as brw "going away" / "disconnecting".
//
// Two independent mechanisms, because either alone has a failure mode:
//
//  1. GENUINE (silent) looping audio. An offscreen document is exempt from
//     teardown only while a declared reason is ACTIVELY in use; AUDIO_PLAYBACK
//     counts as "in use" only while audio is truly playing. A document that
//     merely claims the reason but plays nothing gets reaped — and once it is
//     gone, nothing resets the worker's 30s idle timer. A running audio graph
//     also signals to macOS that this process is doing active audio work, which
//     blunts App Nap / Energy Saver freezing the whole extension in the
//     background (the condition behind multi-minute bridge outages). Zero-gain
//     output makes it inaudible while keeping the audio session genuinely live.
//
//  2. A LONG-LIVED PORT to the service worker, re-opened whenever it drops. An
//     open runtime port keeps the worker non-idle continuously — no 20s-vs-30s
//     race — and reconnecting the port immediately re-pins a worker that just
//     respawned. The periodic message is a belt-and-braces nudge that also
//     drives a reconnect/probe inside the worker.

const KEEPALIVE_MESSAGE_MS = 20000;
const AUDIO_RESUME_MS = 25000;
const PORT_REOPEN_MS = 1000;

// --- 1. Genuine silent audio keeps this document (and thus the SW) alive. ---
function startSilentAudio() {
  try {
    const Ctx = self.AudioContext || self.webkitAudioContext;
    if (!Ctx) return;
    const ctx = new Ctx();
    const oscillator = ctx.createOscillator();
    const gain = ctx.createGain();
    gain.gain.value = 0; // inaudible, but the audio graph is genuinely running
    oscillator.connect(gain);
    gain.connect(ctx.destination);
    oscillator.start();
    // Offscreen AudioContexts can start (or later drop) into "suspended"; keep
    // resuming so the audio session actually stays live.
    const resume = () => {
      if (ctx.state === "suspended") ctx.resume().catch(() => {});
    };
    resume();
    setInterval(resume, AUDIO_RESUME_MS);
  } catch (_) {
    // WebAudio unavailable in this context — the port + message keepalive below
    // still keeps the worker awake; we simply lose the App Nap resistance.
  }
}

// --- 2. Long-lived port to the service worker, re-opened if it drops. ---
let keepAlivePort = null;
function connectPort() {
  try {
    keepAlivePort = chrome.runtime.connect({ name: "brw-keepalive" });
    keepAlivePort.onDisconnect.addListener(() => {
      keepAlivePort = null;
      // The worker may be respawning; reopen shortly to re-pin it awake.
      setTimeout(connectPort, PORT_REOPEN_MS);
    });
  } catch (_) {
    keepAlivePort = null;
    setTimeout(connectPort, PORT_REOPEN_MS);
  }
}

function pokeWorker() {
  // Fire-and-forget nudge; also re-establish the port if it has fallen.
  chrome.runtime.sendMessage({ type: "SW_KEEPALIVE" }).catch(() => {
    // The service worker may be restarting; ensureOffscreen() recreates this
    // document from the worker's startup/alarm paths if Chrome ever drops it.
  });
  if (!keepAlivePort) connectPort();
}

startSilentAudio();
connectPort();
setInterval(pokeWorker, KEEPALIVE_MESSAGE_MS);
