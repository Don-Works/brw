// agent-browser offscreen keepalive.
//
// This mirrors Claude's Chrome extension pattern: the offscreen document is not
// subject to MV3's ~30s service-worker idle kill, so it sends a runtime message
// every 20s. That wakes/resets the service worker, which keeps the daemon
// WebSocket connected and active-tab resolution deterministic while Chrome is
// idle in the background.
setInterval(() => {
  chrome.runtime.sendMessage({ type: "SW_KEEPALIVE" }).catch(() => {
    // The service worker may be restarting; ensureOffscreen() will recreate this
    // document from service_worker.js startup/alarm paths if Chrome drops it.
  });
}, 20000);
