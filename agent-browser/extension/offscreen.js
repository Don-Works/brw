// agent-browser offscreen keepalive.
//
// An MV3 service worker is killed after ~30s of inactivity. When that happens the
// daemon's WebSocket drops and the bridge falls back to a stale active-tab cache,
// which is the root cause of read/observe/snapshot resolving different tabs while
// Chrome sits idle in the background.
//
// An offscreen document, unlike the service worker, is NOT subject to the idle
// timer. By holding a long-lived chrome.runtime.connect port to the service
// worker — and re-opening it whenever it drops — we continuously reset the SW's
// idle timer, so the worker (and therefore the daemon socket and deterministic
// active-tab resolution) stays alive for as long as Chrome is running.

let port = null;

function connect() {
  try {
    port = chrome.runtime.connect({ name: "keepalive" });
    // If the worker recycles the port (or was briefly gone), reconnect promptly
    // to revive it. A fresh connect wakes a sleeping worker.
    port.onDisconnect.addListener(() => {
      port = null;
      setTimeout(connect, 250);
    });
  } catch (_) {
    setTimeout(connect, 1000);
  }
}

connect();

// Periodic ping over the port. The message traffic (and the live port itself)
// keeps resetting the service worker's idle timer. 20s is comfortably under the
// ~30s termination window.
setInterval(() => {
  if (port) {
    try {
      port.postMessage({ keepalive: true });
      return;
    } catch (_) {
      port = null;
    }
  }
  connect();
}, 20000);
