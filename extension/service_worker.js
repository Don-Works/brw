const BRIDGE_URL = "ws://127.0.0.1:17311/extension";
const BRIDGE_STATUS_URL = "http://127.0.0.1:17311/status";
const BRIDGE_CONFIG_KEY = "brwBridgeConfig";
const BRIDGE_STATUS_KEY = "brwBridge";
const PROTOCOL_VERSION = "0.2.0";
const KEEPALIVE_INTERVAL_MS = 5 * 1000;
const DAEMON_STATUS_INTERVAL_MS = 10 * 1000;
const DAEMON_STATUS_TIMEOUT_MS = 2 * 1000;
// A single loopback /status fetch can fail while macOS wakes, the daemon is
// briefly busy, or Chromium is resuming its network service. The WebSocket is
// the authoritative transport, so do not tear a healthy one down on one noisy
// HTTP probe. Three consecutive failures still recover a genuinely stale link.
const MAX_DAEMON_STATUS_FAILURES = 3;
const MAX_RECONNECT_DELAY_MS = 10 * 1000;
// Detach a tab's debugger after this long without a CDP command, so brw doesn't
// hold debugger sessions on idle tabs of the user's real Chrome.
const IDLE_DETACH_MS = 120 * 1000;
// How long after a brw-initiated CDP command a JS dialog on that tab is treated
// as brw's own (auto-accepted to let the agent's flow proceed). Outside this
// window a dialog is the user's / a background script's, and is answered with the
// NON-destructive choice instead of blindly accepting.
const BRW_ACTING_WINDOW_MS = 8 * 1000;
// CDP methods brw refuses to forward, enforcing its promise not to read or export
// cookies and site storage: every cookie read/write method (which can reach
// HttpOnly cookies page JS cannot) plus the whole family of storage domains that
// can bulk-export a site's local/session/indexed/cache/SQL storage —
// Storage.*, DOMStorage.*, IndexedDB.*, CacheStorage.*, and Database.* (Web SQL).
// brw itself uses none of these, so denying them never breaks a feature — it
// turns the privacy claim from a convention into an enforced boundary that holds
// even against a rogue server that answered the extension's outbound socket.
// (Runtime.evaluate is NOT on this list — brw needs it to drive the page — so a
// caller can still read non-HttpOnly document.cookie or an input .value through
// it; the enforced boundary is "no HttpOnly cookies, no bulk storage export".)
const STORAGE_DOMAIN_PREFIXES = [
  "Storage.",
  "DOMStorage.",
  "IndexedDB.",
  "CacheStorage.",
  "Database."
];
function isDeniedCdpMethod(method) {
  const m = String(method || "");
  if (/cookie/i.test(m)) return true;
  return STORAGE_DOMAIN_PREFIXES.some((prefix) => m.startsWith(prefix));
}
let offscreenSetupPromise = null;
let packagedDefaultConfigPromise = null;
// Activate→work→restore "juggles" (screenshot capture, frozen-tab revival) are
// serialized per extension profile because each one may briefly activate its
// background tab and then restore the previously active tab. Overlapping
// juggles would race those restorations.
let tabJuggleQueue = Promise.resolve();

// enqueueTabJuggle runs fn once every earlier juggle has finished. fn's
// rejection propagates to its caller but never wedges the queue. Never call
// this from code already running inside a juggle — that deadlocks; pass
// skipRevive to attach() instead (see captureScreenshotForTab).
async function enqueueTabJuggle(fn) {
  const previous = tabJuggleQueue;
  let release;
  tabJuggleQueue = new Promise((resolve) => { release = resolve; });
  await previous.catch(() => {});
  try {
    return await fn();
  } finally {
    release();
  }
}

const state = {
  socket: null,
  connectPromise: null,
  reconnectTimer: null,
  keepAliveTimer: null,
  statusTimer: null,
  statusProbeInFlight: null,
  statusProbeFailures: 0,
  attachedTabs: new Set(),
  // attachUsedAt records the last time each attached tab's debugger was used, so
  // sweepIdleDebuggers can release debuggers that have gone idle within a long
  // connection — bounding how many debugger sessions brw holds on the user's
  // real Chrome at once (accumulating attachments destabilize renderers).
  attachUsedAt: new Map(),
  // activeTabId is the USER-foreground hint, refreshed from tab/window activation
  // events. It is only a fallback for resolving the target tab.
  activeTabId: null,
  // agentTabId is the agent's PINNED working tab — the tab it opened or explicitly
  // focused. It is the HIGHEST-precedence target for no-tab_id tools and is NEVER
  // moved by the user clicking around their own tabs/windows, which is what stops
  // the "user selected another tab" bug class on a Chrome the human drives at the
  // same time. Set only by agent intent (open_tab / focus_tab); cleared when that
  // tab closes or stops being controllable.
  agentTabId: null,
  reconnectAttempt: 0,
  lastError: "",
  bridgeConfig: null,
  snapshotCache: new Map(),
  observerInjected: new Set(),
  // Per-tab capture of the most recent Page.fileChooserOpened CDP event, keyed
  // by tabId. File-chooser-interception upload mode enables interception, clicks
  // the trigger, then reads the chooser's backendNodeId from here to set the file
  // without the native OS dialog ever opening (which would freeze the CDP
  // session). backendNodeId is frame-agnostic, so this also reaches inputs in
  // cross-origin iframes.
  fileChooserEvents: new Map(),
  // Per-tab expiry timestamp marking that brw is actively driving the tab, so a
  // JS dialog opening during the window is treated as brw's own (see
  // BRW_ACTING_WINDOW_MS). Set on every brw-initiated CDP command.
  actingUntil: new Map(),
	// Native CDP console/exception events, captured from Runtime.enable before
	// page scripts run and drained by get_console_messages. This catches load-time
	// errors that an in-page console monkeypatch installed after navigation misses.
	consoleMessages: new Map(),
  // CSS.forcePseudoState nodes/timers bridge the short interval where Chrome has
  // accepted a pointer move for a locked/background tab but has not yet applied
  // its compositor :hover state. Cleared on the next hover, after five seconds,
  // or when the debugger/tab detaches.
  forcedHoverNodes: new Map(),
  forcedHoverTimers: new Map(),
  // Downloads that started during this brw session, keyed by chrome.downloads id.
  // Populated by the chrome.downloads.onCreated/onChanged listeners and drained
  // by the get_downloads message (issue #6 — the extension bridge cannot observe
  // CDP Browser.downloadWillBegin events, so we capture via chrome.downloads).
  // Insertion order is preserved by Map, so trimming evicts the oldest first.
  downloads: new Map()
};

// MAX_TRACKED_DOWNLOADS bounds the download buffer so a long-lived session that
// triggers many downloads cannot grow it without limit. Mirrors the direct-CDP
// Manager's maxTrackedDownloads cap (internal/browser/manager_downloads.go).
const MAX_TRACKED_DOWNLOADS = 200;
const MAX_CONSOLE_MESSAGES = 200;

function remoteObjectText(arg) {
  if (!arg) return "undefined";
  if (Object.prototype.hasOwnProperty.call(arg, "value")) {
    try { return typeof arg.value === "string" ? arg.value : JSON.stringify(arg.value); } catch (_) {}
  }
  if (arg.unserializableValue) return String(arg.unserializableValue);
  return String(arg.description || arg.type || "undefined");
}

function recordConsoleMessage(tabId, level, text) {
  if (typeof tabId !== "number") return;
  const messages = state.consoleMessages.get(tabId) || [];
  messages.push({ level: level === "warning" ? "warn" : (level || "log"), text: String(text || "").slice(0, 1000), timestamp: new Date().toISOString() });
  if (messages.length > MAX_CONSOLE_MESSAGES) messages.splice(0, messages.length - MAX_CONSOLE_MESSAGES);
  state.consoleMessages.set(tabId, messages);
}

// mapDownloadState translates a chrome.downloads state to the wire vocabulary the
// Go side expects (inProgress | completed | canceled), matching the direct-CDP
// backend's DownloadEntry.State so brw_downloads reads identically on both.
function mapDownloadState(state) {
  switch (state) {
    case "complete": return "completed";
    case "interrupted": return "canceled";
    case "in_progress": return "inProgress";
    default: return state || "inProgress";
  }
}

// recordDownload upserts a chrome.downloads item into the session buffer in the
// Go DownloadEntry wire shape. filename is the full local path; suggested_filename
// is its basename. Only fields present on the item/delta are overwritten so a
// later onChanged delta never clobbers a value an earlier event already set.
function recordDownload(item) {
  if (!item || typeof item.id !== "number") return;
  const guid = String(item.id);
  const prev = state.downloads.get(guid) || { guid };
  const path = (typeof item.filename === "string" && item.filename) ? item.filename : prev.path;
  const next = {
    guid,
    url: item.url || prev.url || "",
    suggested_filename: path ? path.split(/[\\/]/).pop() : (prev.suggested_filename || ""),
    state: item.state ? mapDownloadState(item.state) : (prev.state || "inProgress"),
    received_bytes: typeof item.bytesReceived === "number" ? item.bytesReceived : (prev.received_bytes || 0),
    total_bytes: typeof item.totalBytes === "number" && item.totalBytes > 0 ? item.totalBytes : (prev.total_bytes || (typeof item.fileSize === "number" && item.fileSize > 0 ? item.fileSize : 0)),
    path: path || ""
  };
  // Re-insert at the end so the most-recently-touched download is freshest and
  // trimming drops the stalest. Map.set on an existing key keeps original order,
  // so delete first to move it to the tail.
  state.downloads.delete(guid);
  state.downloads.set(guid, next);
  while (state.downloads.size > MAX_TRACKED_DOWNLOADS) {
    const oldest = state.downloads.keys().next().value;
    state.downloads.delete(oldest);
  }
}

// flattenDownloadDelta turns a chrome.downloads.onChanged delta ({id, state:{current}})
// into the flat item shape recordDownload consumes.
function flattenDownloadDelta(delta) {
  if (!delta || typeof delta.id !== "number") return null;
  const item = { id: delta.id };
  if (delta.url && delta.url.current != null) item.url = delta.url.current;
  if (delta.filename && delta.filename.current != null) item.filename = delta.filename.current;
  if (delta.state && delta.state.current != null) item.state = delta.state.current;
  if (delta.totalBytes && delta.totalBytes.current != null) item.totalBytes = delta.totalBytes.current;
  if (delta.fileSize && delta.fileSize.current != null) item.fileSize = delta.fileSize.current;
  return item;
}

// chrome.downloads is gated on the "downloads" manifest permission and absent on
// very old Chrome; guard so the service worker still loads if it is unavailable.
if (chrome.downloads && chrome.downloads.onCreated) {
  chrome.downloads.onCreated.addListener((item) => recordDownload(item));
  chrome.downloads.onChanged.addListener((delta) => {
    const item = flattenDownloadDelta(delta);
    if (item) recordDownload(item);
  });
}

// markActing records that brw is driving tabId right now, so a dialog it triggers
// (e.g. a beforeunload while navigating, or a confirm it clicked) is auto-handled.
function markActing(tabId) {
  if (typeof tabId === "number") state.actingUntil.set(tabId, Date.now() + BRW_ACTING_WINDOW_MS);
}

// isActing reports whether brw is within its acting window for tabId.
function isActing(tabId) {
  return Date.now() < (state.actingUntil.get(tabId) || 0);
}

chrome.runtime.onInstalled.addListener(() => {
  ensureConnectAlarm();
  ensureOffscreen();
  reconcileDebuggerAttachments().catch(() => {});
  markBridgeStatus("starting").catch(() => {});
  connect();
});

chrome.runtime.onStartup.addListener(() => {
  ensureConnectAlarm();
  ensureOffscreen();
  reconcileDebuggerAttachments().catch(() => {});
  markBridgeStatus("starting").catch(() => {});
  connect();
});
chrome.runtime.onMessage.addListener((message, _sender, sendResponse) => {
  if (message?.type === "BRW_GET_STATUS") {
    bridgeDebugStatus().then((status) => sendResponse({ ok: true, status })).catch((error) => {
      sendResponse({ ok: false, error: String(error?.message || error) });
    });
    return true;
  }
  if (message?.type === "BRW_CONFIGURE") {
    configureBridge(message.config || {}).then((config) => {
      sendResponse({ ok: true, config });
    }).catch((error) => {
      sendResponse({ ok: false, error: String(error?.message || error) });
    });
    return true;
  }
  if (message?.type !== "SW_KEEPALIVE") return false;
  connect({ probe: true });
  sendResponse({ ok: true });
  return false;
});
chrome.storage.onChanged.addListener((changes, area) => {
  if (area !== "local" || !changes[BRIDGE_CONFIG_KEY]) return;
  try {
    state.bridgeConfig = normalizeBridgeConfig(changes[BRIDGE_CONFIG_KEY].newValue || {});
  } catch (error) {
    state.lastError = `invalid bridge config: ${String(error?.message || error)}`;
    markBridgeStatus("error", state.lastError).catch(() => {});
    return;
  }
  state.lastError = "";
  if (state.socket) {
    try { state.socket.close(); } catch (_) {}
    state.socket = null;
  }
  connect({ probe: true });
});
chrome.action.onClicked.addListener(async (tab) => {
  await connect({ probe: true });
  if (tab?.id) {
    send({ type: "active_tab", tabId: tab.id, url: tab.url, title: tab.title });
  }
});
chrome.alarms.onAlarm.addListener((alarm) => {
  if (alarm.name === "brw-connect") {
    ensureOffscreen();
    connect({ probe: true });
  }
});
chrome.runtime.onSuspend.addListener(() => {
  stopKeepAlive();
  setBridgeBadge("disconnected");
  // Best-effort: release every debugger before the service worker is torn down,
  // so a suspend never leaves the user's Chrome in a debugged state.
  detachAll().catch(() => {});
});
chrome.tabs.onActivated.addListener(async (activeInfo) => {
  await publishActiveTab(activeInfo?.tabId);
});
chrome.tabs.onCreated.addListener(async (tab) => {
  if (tab?.active) await publishActiveTab(tab.id);
});
chrome.tabs.onRemoved.addListener((tabId) => {
  state.attachedTabs.delete(tabId);
  state.attachUsedAt.delete(tabId);
  state.snapshotCache.delete(tabId);
  state.observerInjected.delete(tabId);
  state.fileChooserEvents.delete(tabId);
  state.actingUntil.delete(tabId);
	state.consoleMessages.delete(tabId);
  state.forcedHoverNodes.delete(tabId);
  if (state.forcedHoverTimers.has(tabId)) clearTimeout(state.forcedHoverTimers.get(tabId));
  state.forcedHoverTimers.delete(tabId);
  if (state.activeTabId === tabId) state.activeTabId = null;
  // The agent's pinned tab was closed — drop the pin so resolution falls back to a
  // live tab instead of repeatedly probing a dead one.
  if (state.agentTabId === tabId) state.agentTabId = null;
});
chrome.windows.onFocusChanged.addListener(async (windowId) => {
  if (windowId === chrome.windows.WINDOW_ID_NONE) return;
  // Ignore only genuine non-browser surfaces (PWA/app + devtools). Track every
  // other window type — normal, popup, and clone/test-profile windows that may not
  // classify as "normal" — so the agent can target them without landing on a PWA.
  const win = await chrome.windows.get(windowId).catch(() => null);
  if (win && (win.type === "app" || win.type === "devtools")) return;
  const tabs = await chrome.tabs.query({ windowId, active: true }).catch(() => []);
  if (tabs[0]?.id) await publishActiveTab(tabs[0].id);
});
chrome.debugger.onDetach.addListener((source) => {
  if (source.tabId) {
    state.attachedTabs.delete(source.tabId);
    state.attachUsedAt.delete(source.tabId);
    state.fileChooserEvents.delete(source.tabId);
    state.actingUntil.delete(source.tabId);
    state.forcedHoverNodes.delete(source.tabId);
    if (state.forcedHoverTimers.has(source.tabId)) clearTimeout(state.forcedHoverTimers.get(source.tabId));
    state.forcedHoverTimers.delete(source.tabId);
  }
});
// Capture CDP events the daemon needs to observe out-of-band. The only one today
// is Page.fileChooserOpened: when file-chooser interception is enabled
// (Page.setInterceptFileChooserDialog), clicking a file-picker trigger fires this
// event with the chooser's backendNodeId instead of opening the native OS dialog.
// We stash the latest per tab so the daemon can poll for it via
// get_file_chooser_event and then set the file with DOM.setFileInputFiles.
chrome.debugger.onEvent.addListener((source, method, params) => {
	if (method === "Runtime.consoleAPICalled" && typeof source.tabId === "number") {
	  const text = (params?.args || []).map(remoteObjectText).join(" ");
	  recordConsoleMessage(source.tabId, params?.type || "log", text);
	  return;
	}
	if (method === "Runtime.exceptionThrown" && typeof source.tabId === "number") {
	  const details = params?.exceptionDetails || {};
	  const text = details?.exception?.description || details?.exception?.value || details?.text || "Uncaught exception";
	  recordConsoleMessage(source.tabId, "error", text);
	  return;
	}
  // A JS dialog (alert/confirm/prompt/beforeunload) opening while Page is enabled
  // is intercepted by CDP and MUST be answered or the renderer hangs. We answer,
  // but the choice is no longer a blanket accept:
  //   - If brw is actively driving this tab (isActing), the dialog is the agent's
  //     own — accept it so its flow proceeds (e.g. confirm it just clicked, or a
  //     beforeunload while brw navigates).
  //   - Otherwise the dialog is the USER's (or a background script's): answer with
  //     the NON-destructive choice — Cancel/Stay for confirm/prompt/beforeunload
  //     (never auto-OK "Delete account?", never silently discard unsaved changes),
  //     and OK only for alert, whose sole button is OK.
  if (method === "Page.javascriptDialogOpening" && typeof source.tabId === "number") {
    const accept = isActing(source.tabId) || (params?.type || "") === "alert";
    chrome.debugger.sendCommand(
      { tabId: source.tabId },
      "Page.handleJavaScriptDialog",
      { accept }
    ).catch(() => {});
    return;
  }
  if (method !== "Page.fileChooserOpened" || typeof source.tabId !== "number") return;
  state.fileChooserEvents.set(source.tabId, {
    backendNodeId: params?.backendNodeId ?? 0,
    frameId: params?.frameId || "",
    mode: params?.mode || "",
    capturedAt: Date.now()
  });
});
// A full-page navigation replaces the document, so any snapshot cached for that
// tab (and the MutationObserver / console hook injected into the old execution
// context) is stale. Clear the per-tab cache + observer flag on main-frame
// commits so the next Snapshot()/Find() re-evaluates against the new document
// instead of serving pre-navigation content. frameId === 0 = main frame only;
// subframe (iframe) navigations don't replace the top document.
chrome.webNavigation.onCommitted.addListener((details) => {
  if (typeof details.tabId === "number" && details.frameId === 0) {
    state.snapshotCache.delete(details.tabId);
    state.observerInjected.delete(details.tabId);
  }
});
// SPA route changes via history.pushState/replaceState (the way frameworks like
// Decathlon's storefront navigate) do NOT fire onCommitted — the document is
// never replaced — so the snapshot cache would go stale across a client-side
// route change and serve pre-navigation content. onHistoryStateUpdated fires
// exactly for these in-page history transitions; invalidate the per-tab snapshot
// cache on a main-frame (frameId === 0) update so the next Snapshot()/Find()
// re-evaluates against the new route. The injected MutationObserver/console hook
// survive (same execution context), so observerInjected is intentionally NOT
// cleared here — only the stale snapshot is dropped.
chrome.webNavigation.onHistoryStateUpdated.addListener((details) => {
  if (typeof details.tabId === "number" && details.frameId === 0) {
    state.snapshotCache.delete(details.tabId);
  }
});

ensureConnectAlarm();
ensureOffscreen();
reconcileDebuggerAttachments().catch(() => {});
markBridgeStatus("starting").catch(() => {});
connect();

async function connect(options = {}) {
  if (isSocketOpen()) {
    if (options.probe) await probeDaemonStatus();
    return;
  }
  if (isSocketConnecting()) return state.connectPromise || undefined;
  if (state.connectPromise) return state.connectPromise;
  state.connectPromise = connectOnce().finally(() => {
    state.connectPromise = null;
  });
  return state.connectPromise;
}

async function connectOnce() {
  clearTimeout(state.reconnectTimer);
  state.reconnectTimer = null;
  stopKeepAlive();
  let config;
  try {
    config = await loadBridgeConfig();
  } catch (error) {
    state.lastError = `invalid bridge config: ${String(error?.message || error)}`;
    await markBridgeStatus("error", state.lastError);
    return;
  }
  await markBridgeStatus("connecting");

  const socket = new WebSocket(config.bridgeUrl);
  state.socket = socket;

  socket.onopen = async () => {
    if (state.socket !== socket) return;
    state.reconnectAttempt = 0;
    state.statusProbeFailures = 0;
    state.lastError = "";
    await markBridgeStatus("connected");
    const platform = await chrome.runtime.getPlatformInfo().catch(() => ({}));
    // Read the per-launch handshake token from the daemon's loopback /status
    // (our host_permissions let us read the body; a web page's cross-origin fetch
    // gets an opaque response) and present it as the FIRST frame. The daemon
    // refuses any connection whose hello lacks the token, so a malicious page or a
    // rogue local client that opened this socket cannot drive the bridge.
    const token = await fetchBridgeToken(config);
    send({
      type: "hello",
      hello: {
        source: "brw-extension",
        version: PROTOCOL_VERSION,
        // The actual manifest version of the LOADED code, distinct from the
        // wire-protocol version above. This is what lets an operator confirm
        // an unpacked-extension reload really picked up a new build — the
        // browsers on this pattern have been caught running months-stale code
        // while the on-disk extension directory was current.
        build: (chrome.runtime.getManifest?.() || {}).version || "",
        chrome: navigator.userAgent,
        platform: platform.os || "",
        workspace: config.workspace || "",
        profile: config.profile || "",
        label: config.label || "",
        token
      }
    });
    // Start keepalive only AFTER the hello so hello is guaranteed to be the
    // bridge's first frame — the authenticated handshake requires it.
    startKeepAlive();
    const tabs = await chrome.tabs.query({ active: true, lastFocusedWindow: true }).catch(() => []);
    if (tabs[0]?.id) await publishActiveTab(tabs[0].id);
    probeDaemonStatus().catch(() => {});
  };
  socket.onclose = (event) => {
    if (state.socket !== socket) return;
    state.socket = null;
    // The daemon is gone — release every debugger so brw never keeps the user's
    // real Chrome in a debugged state while disconnected (the next CDP call
    // re-attaches lazily, so this is safe). This is the primary fix for
    // debugger sessions accumulating and destabilizing Chrome / corrupting tab
    // storage (e.g. WhatsApp Web logging out).
    detachAll().catch(() => {});
    scheduleReconnect(`closed ${event?.code || ""}`.trim());
  };
  socket.onerror = (event) => {
    state.lastError = `websocket error ${String(event?.type || "")}`;
    markBridgeStatus("error", state.lastError).catch(() => {});
    try { socket.close(); } catch (_) {}
  };
  socket.onmessage = async (event) => {
    let message;
    try {
      message = JSON.parse(event.data);
    } catch (error) {
      send({ id: null, ok: false, error: String(error) });
      return;
    }
    await handle(message);
  };
}

async function loadBridgeConfig() {
  const defaults = await packagedDefaultBridgeConfig();
  const data = await chrome.storage.local.get(BRIDGE_CONFIG_KEY).catch(() => ({}));
  state.bridgeConfig = normalizeBridgeConfig({ ...defaults, ...(data[BRIDGE_CONFIG_KEY] || {}) });
  return state.bridgeConfig;
}

async function packagedDefaultBridgeConfig() {
  if (!packagedDefaultConfigPromise) {
    packagedDefaultConfigPromise = fetch(chrome.runtime.getURL("bridge-defaults.json"), { cache: "no-store" })
      .then((response) => response.ok ? response.json() : {})
      .catch(() => ({}));
  }
  return packagedDefaultConfigPromise;
}

async function configureBridge(config) {
  const normalized = normalizeBridgeConfig(config || {});
  state.bridgeConfig = normalized;
  await chrome.storage.local.set({ [BRIDGE_CONFIG_KEY]: normalized });
  state.lastError = "";
  if (state.socket) {
    try { state.socket.close(); } catch (_) {}
    state.socket = null;
  }
  await markBridgeStatus("configured");
  connect({ probe: true });
  return normalized;
}

async function bridgeDebugStatus() {
  const config = await loadBridgeConfig();
  const data = await chrome.storage.local.get(BRIDGE_STATUS_KEY).catch(() => ({}));
  const daemon = await fetchDaemonSummary(config);
  return {
    config,
    bridge: data[BRIDGE_STATUS_KEY] || null,
    socket: isSocketOpen() ? "open" : (isSocketConnecting() ? "connecting" : "closed"),
    daemon,
    extensionVersion: (chrome.runtime.getManifest?.() || {}).version || ""
  };
}

async function fetchDaemonSummary(config) {
  try {
    const response = await fetch(config.statusUrl, {
      cache: "no-store",
      signal: AbortSignal.timeout(1500)
    });
    if (!response.ok) throw new Error(`status ${response.status}`);
    const status = await response.json().catch(() => ({}));
    return {
      reachable: true,
      connected: Boolean(status.connected),
      identity: status.identity || null,
      extensionBuild: status.hello?.build || "",
      connectedAt: status.connected_at || "",
      lastSeenAt: status.last_seen_at || "",
      pending: Number(status.pending || 0),
      inflight: Number(status.inflight || 0),
      queued: Number(status.queued || 0),
      retries: Number(status.retries || 0),
      disconnectReason: status.disconnect_reason || ""
    };
  } catch (error) {
    return {
      reachable: false,
      connected: false,
      error: String(error?.message || error)
    };
  }
}

function normalizeBridgeConfig(input) {
  const config = input && typeof input === "object" ? input : {};
  const bridgeUrl = normalizeBridgeURL(config.bridgeUrl || config.url || bridgeURLFromPort(config.bridgePort) || BRIDGE_URL);
  const statusUrl = normalizeStatusURL(config.statusUrl || deriveStatusURL(bridgeUrl));
  return {
    bridgeUrl,
    statusUrl,
    workspace: cleanLabel(config.workspace),
    profile: cleanLabel(config.profile),
    label: cleanLabel(config.label)
  };
}

function bridgeURLFromPort(value) {
  if (value === undefined || value === null || value === "") return "";
  const port = Number(value);
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    throw new Error("bridgePort must be a TCP port number");
  }
  return `ws://127.0.0.1:${port}/extension`;
}

function normalizeBridgeURL(value) {
  const url = new URL(String(value || BRIDGE_URL));
  if (url.protocol !== "ws:") throw new Error("bridgeUrl must use ws://");
  if (url.hostname !== "127.0.0.1" && url.hostname !== "localhost") {
    throw new Error("bridgeUrl must target localhost or 127.0.0.1");
  }
  if (!url.port) throw new Error("bridgeUrl must include a port");
  if (url.pathname === "/" || url.pathname === "") url.pathname = "/extension";
  if (url.pathname !== "/extension") throw new Error("bridgeUrl path must be /extension");
  url.search = "";
  url.hash = "";
  return url.toString();
}

function deriveStatusURL(bridgeUrl) {
  const url = new URL(bridgeUrl);
  url.protocol = "http:";
  url.pathname = "/status";
  url.search = "";
  url.hash = "";
  return url.toString();
}

function normalizeStatusURL(value) {
  const url = new URL(String(value || BRIDGE_STATUS_URL));
  if (url.protocol !== "http:") throw new Error("statusUrl must use http://");
  if (url.hostname !== "127.0.0.1" && url.hostname !== "localhost") {
    throw new Error("statusUrl must target localhost or 127.0.0.1");
  }
  if (!url.port) throw new Error("statusUrl must include a port");
  if (url.pathname === "/" || url.pathname === "") url.pathname = "/status";
  if (url.pathname !== "/status") throw new Error("statusUrl path must be /status");
  url.search = "";
  url.hash = "";
  return url.toString();
}

function cleanLabel(value) {
  return String(value || "").trim().slice(0, 120);
}

globalThis.brwStatus = bridgeDebugStatus;
globalThis.brwConfigure = configureBridge;

async function handle(message) {
  try {
    if (message.type === "ping") {
      send({ id: message.id, ok: true, result: { pong: true } });
      return;
    }
    if (message.type === "list_tabs") {
      send({ id: message.id, ok: true, result: await listTabSummaries() });
      return;
    }
    if (message.type === "list_tab_groups") {
      send({ id: message.id, ok: true, result: await listTabGroups() });
      return;
    }
    if (message.type === "get_active_tab_id") {
      // Resolve the browser's genuinely focused/active tab dynamically rather
      // than letting the daemon trust a cached reference that drifts when the
      // user switches tabs manually. activeTabId() prefers the focused window's
      // active tab and self-heals the cached state.activeTabId.
      const tabId = await activeTabId().catch(() => null);
      send({ id: message.id, ok: true, result: { tabId: tabId || 0 } });
      return;
    }
    if (message.type === "open_tab") {
      // Foreground vs background. By default (active !== false) the tab is created
      // ACTIVE within its window so it becomes the authoritative foreground tab
      // (resolveForegroundTabId returns it) and subsequent no-tab_id page tools —
      // read, observe, snapshot — follow it, matching what list_tabs reports as
      // active. The daemon passes active:false in isolation mode: the tab opens in
      // the BACKGROUND so it never switches the tab the user is looking at, while
      // still being pinned as the agent's working target below (the daemon resolves
      // it by id, so it does not need to be the foreground tab). Either way we
      // never call chrome.windows.update({focused:true}), so automation never
      // raises Chrome over the user's other OS apps.
      const makeActive = message.params?.active !== false;
      const createParams = { url: message.params?.url || "about:blank", active: makeActive };
      // chrome.tabs.create without windowId inherits Chrome's last-focused
      // window — including a popup created by the previous automation step.
      // Popup windows cannot host tab groups, so the new tab would be created
      // successfully and then open_tab would fail while grouping it. Prefer a
      // normal browser window explicitly; omit windowId only when none exists so
      // Chrome can create its normal fallback window.
      const normalWindowId = await preferredNormalWindowId();
      if (typeof normalWindowId === "number") createParams.windowId = normalWindowId;
      let tab;
      try {
        tab = await chrome.tabs.create(createParams);
      } catch (err) {
        // Chrome rejects tabs.create with "No current window" when the browser
        // process is alive with zero windows — routine on macOS, where closing
        // the last window leaves the app running. Chrome does not create the
        // fallback window here, and an agent cannot open one itself, so the
        // call used to dead-end on a state that is trivially recoverable.
        // Create the window brw needs and carry on.
        if (!/no current window/i.test(String(err?.message || err))) throw err;
        const win = await chrome.windows.create({
          url: createParams.url,
          focused: false,
        });
        tab = win?.tabs?.[0];
        if (!tab) throw err;
      }
      // brw drives this tab for the rest of the agent session, usually in the
      // background. Memory Saver would see an idle background tab and discard
      // it — killing the renderer so every later CDP call hangs. Opt the tab
      // out of automatic discard for its lifetime.
      if (tab.id) await chrome.tabs.update(tab.id, { autoDiscardable: false }).catch(() => {});
      if (makeActive) state.activeTabId = tab.id || null;
      // Pin the agent's own tab as its working target so subsequent no-tab_id tools
      // stay on it no matter which tab/window the human selects next. This holds for
      // background opens too — the pin, not the foreground state, is what no-tab_id
      // resolution follows.
      state.agentTabId = tab.id || null;
      let resultTab = tab;
      let groupWarning = "";
      if (tab.id && hasGroupTarget(message.params)) {
        try {
          const groupId = await groupTabForParams(tab, message.params);
          if (typeof groupId === "number" && groupId >= 0 && makeActive) {
            // Grouping can DEMOTE the freshly-opened active tab: a collapsed group
            // cannot hold the active tab, so Chrome deactivates the newcomer and
            // activates an adjacent visible tab. Re-expand the group and re-activate
            // the opened tab so it stays the foreground tab the agent will act on —
            // otherwise the next no-tab_id tool resolves the wrong tab. Skipped for a
            // background open, which intentionally stays inactive.
            await chrome.tabGroups.update(groupId, { collapsed: false }).catch(() => {});
            await chrome.tabs.update(tab.id, { active: true }).catch(() => {});
          }
          resultTab = await chrome.tabs.get(tab.id).catch(() => tab);
        } catch (error) {
          // Grouping is organizational, not required for tab isolation (the
          // agentTabId pin above is the actual safety boundary). Chrome can refuse
          // grouping for special/transient window states. Do not turn a successfully
          // created, controllable tab into a failed open or leak an orphan; surface a
          // candid warning and continue ungrouped.
          groupWarning = `tab opened ungrouped: ${tabGroupingFailureMessage(error)}`;
        }
      }
      const summary = await tabSummary(resultTab);
      if (groupWarning) summary.groupWarning = groupWarning;
      send({ id: message.id, ok: true, result: summary });
      return;
    }
    if (message.type === "focus_tab") {
      const tabId = Number(message.params?.tabId);
      // Only RAISE the Chrome window to the OS foreground when the daemon
      // explicitly asks (raiseWindow === true). The default is to NOT raise, so
      // automation never steals the user's focus while they work in another app
      // or window — we still activate the tab within its window below, which is
      // all the no-tab_id resolver needs in the common single-window case.
      const raiseWindow = message.params?.raiseWindow === true;
      const before = await chrome.tabs.get(tabId).catch(() => null);
      if (raiseWindow && before?.windowId) await chrome.windows.update(before.windowId, { focused: true });
      // Expand the target's group first: a tab inside a collapsed group cannot
      // become (and stay) the active tab, so activating it without expanding
      // would let Chrome bounce focus back to a visible tab.
      if (typeof before?.groupId === "number" && before.groupId >= 0) {
        await chrome.tabGroups.update(before.groupId, { collapsed: false }).catch(() => {});
      }
      const tab = await chrome.tabs.update(tabId, { active: true });
      state.activeTabId = tabId;
      // The agent explicitly chose this tab — pin it as the working target so
      // no-tab_id tools follow the agent's intent, not the user's later clicks.
      state.agentTabId = tabId;
      send({ id: message.id, ok: true, result: await tabSummary(tab) });
      return;
    }
    if (message.type === "close_tab") {
      const tabId = Number(message.params?.tabId);
      // Detach our debugger before removing the tab so the session is released
      // explicitly rather than relying solely on the onRemoved/onDetach events.
      await detach(tabId);
      await chrome.tabs.remove(tabId);
      send({ id: message.id, ok: true, result: { closed: tabId } });
      return;
    }
    if (message.type === "group_tabs") {
      const tabIds = (message.params?.tabIds || []).map(Number);
      const requestedName = String(message.params?.name || "").trim();
      const existingID = parseGroupId(message.params?.groupId);
      const name = requestedName || (existingID == null ? "brw" : "");
      const hasColor = message.params?.color !== undefined && message.params?.color !== null && message.params?.color !== "";
      const color = normalizeGroupColor(message.params?.color, "blue");
      if (tabIds.length === 0) {
        send({ id: message.id, ok: false, error: "tabIds is required" });
        return;
      }
      const firstTab = await chrome.tabs.get(tabIds[0]).catch(() => null);
      const existing = existingID == null && name ? await findGroupByTitle(name, firstTab?.windowId) : null;
      const groupArgs = { tabIds };
      if (existingID != null) groupArgs.groupId = existingID;
      else if (existing?.id != null) groupArgs.groupId = existing.id;
      else if (typeof firstTab?.windowId === "number") {
        // In an MV3 service worker Chrome's implicit "current window" is not
        // necessarily the tab's window. This matters especially with native
        // vertical tabs: tabs.group() can otherwise resolve an unrelated tab
        // strip and reject a perfectly groupable normal window. Pin new-group
        // creation to the first requested tab's real window. Do not combine
        // createProperties with groupId; Chromium rejects that parameter pair.
        groupArgs.createProperties = { windowId: firstTab.windowId };
      }
      let groupId;
      try {
        groupId = await chrome.tabs.group(groupArgs);
      } catch (error) {
        throw new Error(tabGroupingFailureMessage(error));
      }
      const update = {};
      if (name) update.title = name;
      if (hasColor || existingID == null) update.color = color;
      if (Object.keys(update).length > 0) await chrome.tabGroups.update(groupId, update);
      const group = await chrome.tabGroups.get(groupId);
      // Report the group's full membership, not just the tabs moved in this
      // call. Otherwise adding tabs to an existing group (by group_id or by
      // reusing a title) undercounts tab_ids/tab_count, diverging from
      // list_tab_groups, which always reports every member.
      const members = (await chrome.tabs.query({ groupId }).catch(() => []))
        .map((t) => t.id)
        .filter((id) => typeof id === "number");
      send({ id: message.id, ok: true, result: tabGroupSummaryFrom(group, members.length ? members : tabIds) });
      return;
    }
    if (message.type === "ungroup_tabs") {
      const tabIds = (message.params?.tabIds || []).map(Number);
      if (tabIds.length === 0) {
        send({ id: message.id, ok: false, error: "tabIds is required" });
        return;
      }
      await chrome.tabs.ungroup(tabIds);
      send({ id: message.id, ok: true, result: { ungrouped: tabIds } });
      return;
    }
    if (message.type === "cached_snapshot") {
      const tabId = Number(message.params?.tabId || (await activeTabId()));
      const cacheKey = String(message.params?.cacheKey || "");
      const cached = state.snapshotCache.get(tabId);
      if (cached && cached.cacheKey === cacheKey) {
        // A full-document navigation can happen without our webNavigation.onCommitted
        // hook clearing the cache (e.g. debugger/CDP-driven navigations don't always
        // surface there). The snapshot cacheKey is URL-agnostic, so verify the tab is
        // still on the URL the snapshot was captured at; if it moved, the cache is
        // stale and must be re-evaluated against the new document.
        let liveUrl = null;
        try { liveUrl = (await chrome.tabs.get(tabId))?.url ?? null; } catch (_) {}
        if (cached.url != null && liveUrl != null && liveUrl !== cached.url) {
          state.snapshotCache.delete(tabId);
          state.observerInjected.delete(tabId);
          send({ id: message.id, ok: true, result: { cached: false } });
          return;
        }
        // Check if the page's MutationObserver flagged DOM changes
        let pageDirty = false;
        try {
          await attach(tabId);
          const evalResult = await chrome.debugger.sendCommand(
            { tabId },
            "Runtime.evaluate",
            { expression: "!!window.__brwDirty", returnByValue: true }
          );
          pageDirty = Boolean(evalResult?.result?.value);
        } catch (_) {}
        if (!pageDirty && !cached.dirty) {
          send({ id: message.id, ok: true, result: { cached: true, snapshot: cached.snapshot } });
          return;
        }
        // Reset dirty flags
        cached.dirty = false;
        try {
          await chrome.debugger.sendCommand(
            { tabId },
            "Runtime.evaluate",
            { expression: "window.__brwDirty = false", returnByValue: true }
          );
        } catch (_) {}
      }
      send({ id: message.id, ok: true, result: { cached: false } });
      return;
    }
    if (message.type === "snapshot_result") {
      const tabId = Number(message.params?.tabId || (await activeTabId()));
      let snapUrl = null;
      try { snapUrl = (await chrome.tabs.get(tabId))?.url ?? null; } catch (_) {}
      state.snapshotCache.set(tabId, {
        cacheKey: String(message.params?.cacheKey || ""),
        url: snapUrl,
        dirty: false,
        snapshot: message.params?.snapshot
      });
      ensureObserver(tabId);
      send({ id: message.id, ok: true, result: { stored: true } });
      return;
    }
    if (message.type === "clear_snapshot_cache") {
      const tabId = Number(message.params?.tabId || (await activeTabId()));
      state.snapshotCache.delete(tabId);
      send({ id: message.id, ok: true, result: { cleared: tabId || 0 } });
      return;
    }
    if (message.type === "move_pointer") {
	  const tabId = Number(message.params?.tabId || (await activeTabId()));
	  const x = Number(message.params?.x);
	  const y = Number(message.params?.y);
	  if (!Number.isFinite(x) || !Number.isFinite(y)) {
	    send({ id: message.id, ok: false, error: "move_pointer requires finite x and y" });
	    return;
	  }
	  await attach(tabId);
	  markActing(tabId);
	  // A locked/fully occluded Chrome can delay applying real :hover even after
	  // accepting the pointer event. Force the hit-tested node + ancestors FIRST
	  // so its CDP commands are not queued behind Chrome's slow mouse-event ACK;
	  // the short bounded window makes CSS menus/tooltips immediately deterministic.
	  // synthetic JS hover listeners were already fired by the daemon.
	  const forced = await forceHoverAt(tabId, x, y).catch(() => 0);
	  // Only queue native input when Chrome can actually route it now. On an
	  // inactive/unfocused/locked tab the ACK stalls for seconds and blocks every
	  // later debugger command (breaking nested hover menus). The daemon already
	  // dispatched the standard JS hover events and forced CSS state above covers
	  // :hover; a foreground target additionally gets the trusted pointer event.
	  const tab = await chrome.tabs.get(tabId).catch(() => null);
	  const win = tab ? await chrome.windows.get(tab.windowId).catch(() => null) : null;
	  const trustedQueued = Boolean(tab?.active && win?.focused);
	  if (trustedQueued) {
	    sendDebuggerCommand(tabId, "Input.dispatchMouseEvent", { type: "mouseMoved", x, y }).catch(() => {});
	  }
	  send({ id: message.id, ok: true, result: { queued: trustedQueued, forced, x, y } });
	  return;
	}
	if (message.type === "get_console_messages") {
	  const tabId = Number(message.params?.tabId || (await activeTabId()));
	  await attach(tabId);
	  const messages = state.consoleMessages.get(tabId) || [];
	  state.consoleMessages.set(tabId, []);
	  send({ id: message.id, ok: true, result: { messages } });
	  return;
	}
	if (message.type === "get_tab_input_state") {
	  const tabId = Number(message.params?.tabId || (await activeTabId()));
	  const tab = await chrome.tabs.get(tabId);
	  const win = await chrome.windows.get(tab.windowId).catch(() => null);
	  send({ id: message.id, ok: true, result: {
	    active: Boolean(tab.active),
	    windowFocused: Boolean(win?.focused)
	  }});
	  return;
	}
	if (message.type === "capture_screenshot") {
	  const tabId = Number(message.params?.tabId || (await activeTabId()));
	  const params = { ...(message.params?.params || {}), fromSurface: true };
	  const result = await captureScreenshotForTab(tabId, params);
	  send({ id: message.id, ok: true, result });
	  return;
	}
	if (message.type === "cdp") {
      const method = message.params?.method;
      // Enforce the cookie/storage denylist regardless of who sent this command —
      // a rogue server that answered our outbound socket cannot exfiltrate cookies
      // through brw, because brw simply will not run those methods.
      if (isDeniedCdpMethod(method)) {
        send({ id: message.id, ok: false, error: `cdp method ${method} is blocked by brw policy: cookie and storage access are not permitted` });
        return;
      }
      const tabId = Number(message.params?.tabId || (await activeTabId()));
      // brw is now actively driving this tab: a dialog it triggers in the next few
      // seconds is its own and may be auto-accepted (see the dialog handler).
      markActing(tabId);
      await attach(tabId);
      const result = await sendDebuggerCommand(tabId, method, message.params?.params || {});
      send({ id: message.id, ok: true, result: result || {} });
      return;
    }
    if (message.type === "set_intercept_file_chooser") {
      // Toggle native-file-dialog interception for file-chooser-interception
      // upload mode. When enabling we clear any stale captured chooser event so a
      // subsequent poll only sees the chooser this upload actually triggers. The
      // daemon ALWAYS disables on exit so the user's manual uploads are unaffected.
      const tabId = Number(message.params?.tabId || (await activeTabId()));
      const enabled = message.params?.enabled === true;
      await attach(tabId);
      await sendDebuggerCommand(tabId, "Page.enable", {}).catch(() => {});
      if (enabled) state.fileChooserEvents.delete(tabId);
      await sendDebuggerCommand(tabId, "Page.setInterceptFileChooserDialog", { enabled });
      send({ id: message.id, ok: true, result: { enabled } });
      return;
    }
    if (message.type === "get_file_chooser_event") {
      // Return (and consume) the most recent Page.fileChooserOpened event for the
      // tab, captured by the chrome.debugger.onEvent listener. Returns
      // captured:false until the click actually opens a chooser.
      const tabId = Number(message.params?.tabId || (await activeTabId()));
      const ev = state.fileChooserEvents.get(tabId);
      if (ev) state.fileChooserEvents.delete(tabId);
      send({ id: message.id, ok: true, result: ev ? { captured: true, ...ev } : { captured: false } });
      return;
    }
    if (message.type === "get_downloads") {
      // Drain the session download buffer (issue #6). chrome.downloads is gated on
      // the manifest "downloads" permission; if unavailable, report supported:false
      // so the daemon surfaces the same graceful note the old stub returned.
      if (!chrome.downloads || !chrome.downloads.search) {
        send({ id: message.id, ok: true, result: { downloads: [], count: 0, supported: false, note: "chrome.downloads API unavailable in this Chrome/extension build" } });
        return;
      }
      // Best-effort refresh each buffered download from chrome.downloads.search so
      // one that completed between onChanged events reports its final filename and
      // state, then drain (matching the direct-CDP Downloads() drain semantics).
      const ids = Array.from(state.downloads.keys());
      await Promise.all(ids.map(async (guid) => {
        try {
          const found = await chrome.downloads.search({ id: Number(guid) });
          if (found && found[0]) recordDownload(found[0]);
        } catch (_) {}
      }));
      const downloads = Array.from(state.downloads.values());
      state.downloads.clear();
      send({ id: message.id, ok: true, result: { downloads, count: downloads.length, supported: true } });
      return;
    }
    if (message.type === "read_cross_origin_frames") {
      // Read interactive controls inside cross-origin (out-of-process) iframes of
      // the tab (issue #11 P0-2). Best-effort: any failure yields an empty list so
      // the daemon keeps the same-origin snapshot.
      const tabId = Number(message.params?.tabId || (await activeTabId()));
      const frames = await readCrossOriginFrames(tabId).catch(() => []);
      send({ id: message.id, ok: true, result: { frames } });
      return;
    }
    if (message.type === "show_indicator") {
      const tabId = Number(message.params?.tabId || (await activeTabId()));
      await attach(tabId);
      const indicatorScript = `(function() {
        if (window.__brwIndicator) return;
        window.__brwIndicator = true;
        var el = document.createElement('div');
        el.id = 'brw-indicator';
        el.style.cssText = 'position:fixed;top:8px;right:8px;z-index:2147483647;background:#1a7f37;color:white;padding:6px 12px;border-radius:6px;font:600 12px system-ui;box-shadow:0 2px 8px rgba(0,0,0,0.2);pointer-events:none;opacity:0.95;transition:opacity 0.3s;';
        el.textContent = '🤖 brw active';
        document.documentElement.appendChild(el);
      })()`;
      await chrome.debugger.sendCommand({ tabId }, "Runtime.evaluate", { expression: indicatorScript, returnByValue: true }).catch(() => {});
      send({ id: message.id, ok: true, result: { shown: true } });
      return;
    }
    if (message.type === "hide_indicator") {
      const tabId = Number(message.params?.tabId || (await activeTabId()));
      await attach(tabId);
      const hideScript = `(function() {
        var el = document.getElementById('brw-indicator');
        if (el) el.remove();
        window.__brwIndicator = false;
      })()`;
      await chrome.debugger.sendCommand({ tabId }, "Runtime.evaluate", { expression: hideScript, returnByValue: true }).catch(() => {});
      send({ id: message.id, ok: true, result: { hidden: true } });
      return;
    }
    if (message.type === "notify") {
      // Surface a desktop notification so the user is pulled back to a
      // human-handoff point (MFA/CAPTCHA/purchase confirmation), a completed
      // run, or an error — even when the agent tab is backgrounded.
      // chrome.notifications.create works regardless of which tab is focused.
      const result = await createNotification(message.params || {});
      send({ id: message.id, ok: true, result });
      return;
    }
    send({ id: message.id, ok: false, error: `unknown message type ${message.type}` });
  } catch (error) {
    state.lastError = `request failed: ${String(error?.message || error)}`;
    markBridgeStatus("error", state.lastError).catch(() => {});
    send({ id: message.id, ok: false, error: String(error?.message || error) });
  }
}

// FRAME_EXTRACT_SCRIPT runs inside a cross-origin iframe's own document (via a
// short-lived debugger attach to that frame's target) and returns its visible
// interactive controls with FRAME-LOCAL viewport boxes. The daemon translates
// those boxes to top-level coordinates using the iframe's position recorded by
// the same-origin walker, so the merged elements are clickable via brw_click_xy.
const FRAME_EXTRACT_SCRIPT = `(function(){
  var SEL = 'a[href],button,input,select,textarea,[role=button],[role=link],[role=textbox],[role=checkbox],[role=radio],[role=combobox],[role=switch],[role=menuitem],[role=tab],[contenteditable=""],[contenteditable=true],[onclick]';
  function vis(el){ try { var s=getComputedStyle(el); if(s.display==='none'||s.visibility==='hidden'||parseFloat(s.opacity||'1')===0) return false; var r=el.getBoundingClientRect(); return r.width>0&&r.height>0; } catch(_){ return false; } }
  function nm(el){
    try {
      var n = el.getAttribute && (el.getAttribute('aria-label')||el.getAttribute('placeholder')||el.getAttribute('title')||el.getAttribute('alt')||el.getAttribute('name'));
      if(n) return String(n).trim();
      var t = (el.innerText||el.value||el.textContent||'').replace(/\\s+/g,' ').trim();
      return t.slice(0,120);
    } catch(_){ return ''; }
  }
  function rl(el){
    var r = el.getAttribute && el.getAttribute('role'); if(r) return r;
    var tag = el.tagName.toLowerCase();
    if(tag==='a') return 'link';
    if(tag==='button') return 'button';
    if(tag==='select') return 'combobox';
    if(tag==='textarea') return 'textbox';
    if(tag==='input'){ var ty=(el.getAttribute('type')||'text').toLowerCase(); if(ty==='checkbox')return 'checkbox'; if(ty==='radio')return 'radio'; if(ty==='button'||ty==='submit'||ty==='reset')return 'button'; return 'textbox'; }
    return tag;
  }
  var els = [];
  try { els = Array.prototype.slice.call(document.querySelectorAll(SEL)); } catch(_){ els = []; }
  var out = [];
  for (var i=0;i<els.length && out.length<200;i++){
    var el = els[i];
    if(!vis(el)) continue;
    var r = el.getBoundingClientRect();
    out.push({ role: rl(el), name: nm(el), tag: el.tagName.toLowerCase(), type: (el.getAttribute&&el.getAttribute('type'))||'', x: r.left, y: r.top, w: r.width, h: r.height });
  }
  return out;
})()`;

// readCrossOriginFrames enumerates the tab's cross-origin child frames (from
// Page.getFrameTree on the tab's own session, so we only ever read frames that
// belong to THIS tab), matches each to its debugger iframe target by URL, and
// extracts its controls. Same-origin frames are skipped — the in-page walker
// already reads those. Never throws; returns [] on any failure.
async function readCrossOriginFrames(tabId) {
  await attach(tabId);
  let tree;
  try {
    tree = await sendDebuggerCommand(tabId, "Page.getFrameTree", {});
  } catch (_) {
    return [];
  }
  const topURL = tree?.frameTree?.frame?.url || "";
  let topOrigin = "";
  try { topOrigin = new URL(topURL).origin; } catch (_) {}
  const wanted = [];
  (function walk(node) {
    if (!node) return;
    for (const child of node.childFrames || []) {
      const url = child.frame?.url || "";
      let origin = "";
      try { origin = new URL(url).origin; } catch (_) {}
      if (url && /^https?:/i.test(url) && origin && origin !== topOrigin) {
        wanted.push({ url, origin });
      }
      walk(child);
    }
  })(tree.frameTree);
  if (!wanted.length) return [];

  let targets = [];
  try { targets = await chrome.debugger.getTargets(); } catch (_) { return []; }
  const iframeTargets = targets.filter((t) => t.type === "iframe" && t.url);
  const usedTargets = new Set();
  const out = [];
  for (const w of wanted) {
    const tgt = iframeTargets.find((t) => !usedTargets.has(t.id) && t.url === w.url);
    if (!tgt) {
      out.push({ url: w.url, origin: w.origin, elements: [] });
      continue;
    }
    usedTargets.add(tgt.id);
    const elements = await extractFrameElements(tgt.id).catch(() => []);
    out.push({ url: w.url, origin: w.origin, elements });
  }
  return out;
}

// extractFrameElements briefly attaches a debugger to ONE cross-origin frame
// target, runs FRAME_EXTRACT_SCRIPT in it, and ALWAYS detaches (even on error) so
// no extra debugger session lingers on the user's Chrome. If another debugger
// already owns the target, it is left untouched and an empty list is returned.
async function extractFrameElements(targetId) {
  let owned = false;
  try {
    try {
      await chrome.debugger.attach({ targetId }, "1.3");
      owned = true;
    } catch (error) {
      if (!String(error?.message || error).includes("Another debugger is already attached")) throw error;
      // Someone else (or a leaked session) owns it; do not detach what we did not
      // attach. Try to use the existing session opportunistically.
      owned = false;
    }
    const res = await chrome.debugger.sendCommand({ targetId }, "Runtime.evaluate", {
      expression: FRAME_EXTRACT_SCRIPT,
      returnByValue: true
    });
    const value = res?.result?.value;
    return Array.isArray(value) ? value : [];
  } finally {
    if (owned) {
      try { await chrome.debugger.detach({ targetId }); } catch (_) {}
    }
  }
}

// ensureTabDrivable revives a tab whose renderer cannot execute work before we
// try to drive it. A DISCARDED tab (Memory Saver) has no renderer at all — any
// CDP command hangs until the daemon's deadline. A FROZEN tab (collapsed tab
// group ≥5 min, or Energy Saver since Chrome 133) has its event loop paused —
// injected work never runs. An attached debugger is NOT exempt from either, so
// detect-and-revive here converts silent multi-second hangs into fast recovery
// (or one candid, classified error the agent can act on).
async function ensureTabDrivable(tabId) {
  let tab = await chrome.tabs.get(tabId).catch(() => null);
  if (!tab) throw new Error(`cannot find tab ${tabId}`);
  if (tab.discarded) {
    // Reload recreates the renderer at the tab's committed URL. Pin
    // autoDiscardable off so Memory Saver does not immediately re-discard the
    // tab brw is actively driving.
    await chrome.tabs.update(tabId, { autoDiscardable: false }).catch(() => {});
    await chrome.tabs.reload(tabId).catch(() => {});
    await waitForTabLoad(tabId, 10000);
    tab = await chrome.tabs.get(tabId).catch(() => null);
    if (!tab || tab.discarded) {
      throw new Error(`tab ${tabId} was discarded by Chrome (Memory Saver) and could not be revived by reload; reopen the page with brw_open`);
    }
    return;
  }
  if (tab.frozen && !tab.active) {
    // Unfreezing needs visibility, not a reload (which would lose page state):
    // expand a collapsed group, flash the tab active inside its own window —
    // never raising the window over other OS apps — then restore the user's
    // tab. Serialized on the juggle queue so concurrent revivals/screenshots
    // cannot restore each other's target.
    await enqueueTabJuggle(async () => {
      // Re-read once at the front of the queue: an earlier juggle (or the
      // user) may have unfrozen or moved the tab while this one waited.
      const fresh = await chrome.tabs.get(tabId).catch(() => null);
      if (!fresh || !fresh.frozen || fresh.active) return;
      if (typeof fresh.groupId === "number" && fresh.groupId >= 0) {
        await chrome.tabGroups.update(fresh.groupId, { collapsed: false }).catch(() => {});
      }
      const previous = await chrome.tabs.query({ windowId: fresh.windowId, active: true }).catch(() => []);
      const restoreTabId = previous?.[0]?.id || null;
      await chrome.tabs.update(tabId, { active: true }).catch(() => {});
      await new Promise((resolve) => setTimeout(resolve, 150));
      if (restoreTabId && restoreTabId !== tabId) {
        await chrome.tabs.update(restoreTabId, { active: true }).catch(() => {});
      }
    });
    tab = await chrome.tabs.get(tabId).catch(() => null);
    if (tab?.frozen && !tab?.active) {
      throw new Error(`tab ${tabId} is frozen by Chrome (collapsed tab group or Energy Saver) and could not be revived; expand its tab group or focus it once, then retry`);
    }
  }
}

// waitForTabLoad polls until the tab's renderer reports a settled load or the
// deadline passes. Deliberately non-fatal on deadline: a slow page that is
// still loading already has a live renderer, which is all driving requires —
// the subsequent CDP call surfaces any real failure.
async function waitForTabLoad(tabId, deadlineMs) {
  const deadline = Date.now() + deadlineMs;
  while (Date.now() < deadline) {
    const tab = await chrome.tabs.get(tabId).catch(() => null);
    if (!tab) throw new Error(`cannot find tab ${tabId}`);
    if (!tab.discarded && tab.status === "complete") return;
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
}

async function attach(tabId, opts = {}) {
  // Revive frozen/discarded tabs before every drive, including on an existing
  // attachment — a tab can freeze WHILE attached. The screenshot juggle skips
  // this (skipRevive): it already activates the tab itself, and re-entering
  // the juggle queue from inside it would deadlock.
  if (!opts.skipRevive) await ensureTabDrivable(tabId);
  if (state.attachedTabs.has(tabId)) {
    state.attachUsedAt.set(tabId, Date.now());
    return;
  }
  try {
    await chrome.debugger.attach({ tabId }, "1.3");
  } catch (error) {
    if (!String(error?.message || error).includes("Another debugger is already attached")) throw error;
    // A debugger is already attached. It is EITHER ours (a previous attach this
    // service worker lost track of — e.g. across an SW restart) OR the user's
    // DevTools. Probe with a trivial command: an extension can only drive a
    // session it owns, so if this succeeds we hold the session and adopt it; if it
    // fails, DevTools owns the tab and brw genuinely cannot control it. Previously
    // we marked the tab attached unconditionally, so a DevTools conflict left brw
    // believing it was attached while every subsequent command failed silently.
    try {
      await chrome.debugger.sendCommand({ tabId }, "Runtime.evaluate", { expression: "0", returnByValue: true });
    } catch (_) {
      throw new Error(`cannot control tab ${tabId}: another debugger (likely DevTools) is already attached; close DevTools on that tab and retry`);
    }
  }
  state.attachedTabs.add(tabId);
  state.attachUsedAt.set(tabId, Date.now());
	// Enable Page events for dialogs, Runtime events for console/load-time
	// exceptions, and focus emulation so trusted pointer/key input reaches a
	// background automation tab without stealing the user's OS focus.
	await Promise.all([
	  chrome.debugger.sendCommand({ tabId }, "Page.enable", {}).catch(() => {}),
	  chrome.debugger.sendCommand({ tabId }, "Runtime.enable", {}).catch(() => {}),
	  chrome.debugger.sendCommand({ tabId }, "Emulation.setFocusEmulationEnabled", { enabled: true }).catch(() => {})
	]);
}

// reconcileDebuggerAttachments releases brw debugger sessions that leaked across
// a service-worker restart or abrupt kill. After such an event state.attachedTabs
// is empty, but Chrome may still hold attachments this extension made — which
// show as a stuck "being debugged" banner and are never released by detachAll /
// sweepIdleDebuggers (they only know about tracked tabs). We enumerate targets
// and, for any attached one we are NOT currently tracking, attempt a detach: an
// extension can only detach its OWN session, so this releases brw's leaks while a
// DevTools/other-client attachment fails harmlessly and is left untouched.
async function reconcileDebuggerAttachments() {
  let targets;
  try {
    targets = await chrome.debugger.getTargets();
  } catch (_) {
    return;
  }
  for (const target of targets || []) {
    if (!target?.attached || typeof target.tabId !== "number") continue;
    if (state.attachedTabs.has(target.tabId)) continue;
    try {
      await chrome.debugger.detach({ tabId: target.tabId });
    } catch (_) {
      // Not our session (DevTools / another client) — detach refused; leave it.
    }
  }
}

// detach releases the debugger brw holds on one tab and forgets its per-tab
// caches. Safe to call when not attached (no-op). The next CDP call re-attaches
// lazily via attach(), so detaching an idle tab never breaks a later action.
async function detach(tabId) {
  await clearForcedHover(tabId).catch(() => {});
  state.attachUsedAt.delete(tabId);
  if (!state.attachedTabs.has(tabId)) return;
  state.attachedTabs.delete(tabId);
  state.observerInjected.delete(tabId);
  state.fileChooserEvents.delete(tabId);
  try {
    await chrome.debugger.detach({ tabId });
  } catch (_) {
    // Already detached (tab closed / Chrome reclaimed it) — nothing to do.
  }
}

// forceDetach is used to cancel a CDP command that itself is stuck (notably
// captureScreenshot on a locked/fully occluded Chrome Stable). Do not trust the
// in-memory set here: onDetach can clear bookkeeping before the browser has fully
// released the session, so always ask Chrome to detach.
async function forceDetach(tabId) {
  if (state.forcedHoverTimers.has(tabId)) clearTimeout(state.forcedHoverTimers.get(tabId));
  state.forcedHoverTimers.delete(tabId);
  state.forcedHoverNodes.delete(tabId);
  state.attachedTabs.delete(tabId);
  state.attachUsedAt.delete(tabId);
  state.observerInjected.delete(tabId);
  state.fileChooserEvents.delete(tabId);
  try { await chrome.debugger.detach({ tabId }); } catch (_) {}
}

// detachAll releases every debugger brw currently holds. Called when the daemon
// disconnects or the service worker suspends so brw never leaves the user's
// real Chrome in a debugged state.
async function detachAll() {
  for (const tabId of Array.from(state.attachedTabs)) {
    await detach(tabId);
  }
}

// sweepIdleDebuggers detaches any tab whose debugger has not been used within
// IDLE_DETACH_MS, bounding how many debugger sessions pile up during a single
// long-lived connection (one run can touch dozens of tabs). Runs on the
// keepalive tick while connected.
async function sweepIdleDebuggers() {
  const now = Date.now();
  for (const tabId of Array.from(state.attachedTabs)) {
    const usedAt = state.attachUsedAt.get(tabId) || 0;
    if (now - usedAt > IDLE_DETACH_MS) await detach(tabId);
  }
}

async function sendDebuggerCommand(tabId, method, params) {
  state.attachUsedAt.set(tabId, Date.now());
  try {
    return await chrome.debugger.sendCommand({ tabId }, method, params);
  } catch (error) {
    if (!isDetachedDebuggerError(error)) throw error;
    state.attachedTabs.delete(tabId);
    await attach(tabId);
    return await chrome.debugger.sendCommand({ tabId }, method, params);
  }
}

async function clearForcedHover(tabId) {
  const timer = state.forcedHoverTimers.get(tabId);
  if (timer) clearTimeout(timer);
  state.forcedHoverTimers.delete(tabId);
  const nodeIds = state.forcedHoverNodes.get(tabId) || [];
  state.forcedHoverNodes.delete(tabId);
  if (!state.attachedTabs.has(tabId)) return;
  await Promise.all(nodeIds.map((nodeId) =>
    sendDebuggerCommand(tabId, "CSS.forcePseudoState", { nodeId, forcedPseudoClasses: [] }).catch(() => {})
  ));
}

// forceHoverAt walks from the deepest painted element at x/y through its DOM,
// shadow-host, and same-origin iframe ancestors, then briefly forces :hover on
// each node. This makes ancestor selectors such as `.figure:hover .caption` and
// nested menus deterministic while Chrome catches up with the trusted pointer
// move on a locked/background desktop.
async function forceHoverAt(tabId, x, y) {
  await clearForcedHover(tabId);
  await Promise.all([
    sendDebuggerCommand(tabId, "DOM.enable", {}).catch(() => {}),
    sendDebuggerCommand(tabId, "CSS.enable", {}).catch(() => {})
  ]);
  // DOM.requestNode returns nodeId:0 until the frontend document has been
  // requested at least once. A depth-0 request is cheap and unlocks stable IDs.
  await sendDebuggerCommand(tabId, "DOM.getDocument", { depth: 0, pierce: true });
  const objectGroup = `brw-hover-${tabId}-${Date.now()}`;
  const expression = `(function(x,y){
    function deepest(doc, px, py) {
      var el = null;
      try { el = doc.elementFromPoint(px, py); } catch (_) { return null; }
      if (!el) return null;
      try {
        if (el.shadowRoot && el.shadowRoot.elementFromPoint) {
          var shadowHit = el.shadowRoot.elementFromPoint(px, py);
          if (shadowHit && shadowHit !== el) return shadowHit;
        }
      } catch (_) {}
      if (String(el.tagName || '').toLowerCase() === 'iframe') {
        try {
          var rect = el.getBoundingClientRect();
          var frameHit = deepest(el.contentDocument, px - rect.left - (el.clientLeft || 0), py - rect.top - (el.clientTop || 0));
          if (frameHit) return frameHit;
        } catch (_) {}
      }
      return el;
    }
    var nodes = [], node = deepest(document, x, y);
    while (node && nodes.length < 8) {
      nodes.push(node);
      if (node.parentElement) { node = node.parentElement; continue; }
      var root = null;
      try { root = node.getRootNode && node.getRootNode(); } catch (_) {}
      if (root && root.host) { node = root.host; continue; }
      try { node = node.ownerDocument && node.ownerDocument.defaultView && node.ownerDocument.defaultView.frameElement; }
      catch (_) { node = null; }
    }
    window.__brwForcedHoverNodes = nodes;
    return nodes;
  })(${JSON.stringify(x)},${JSON.stringify(y)})`;
  try {
    const evaluated = await sendDebuggerCommand(tabId, "Runtime.evaluate", {
      expression,
      returnByValue: false,
      objectGroup
    });
    const arrayID = evaluated?.result?.objectId;
    if (!arrayID) return 0;
    const properties = await sendDebuggerCommand(tabId, "Runtime.getProperties", {
      objectId: arrayID,
      ownProperties: true
    });
    const objectIDs = (properties?.result || [])
      .filter((property) => /^\d+$/.test(String(property?.name || '')) && property?.value?.objectId)
      .map((property) => property.value.objectId);
    const requested = await Promise.all(objectIDs.map((objectId) =>
      sendDebuggerCommand(tabId, "DOM.requestNode", { objectId }).catch(() => null)
    ));
    const nodeIds = requested.map((item) => item?.nodeId).filter(Boolean);
    await Promise.all(nodeIds.map((nodeId) =>
      sendDebuggerCommand(tabId, "CSS.forcePseudoState", { nodeId, forcedPseudoClasses: ["hover"] })
    ));
    state.forcedHoverNodes.set(tabId, nodeIds);
    state.forcedHoverTimers.set(tabId, setTimeout(() => {
      clearForcedHover(tabId).catch(() => {});
    }, 5000));
    return nodeIds.length;
  } finally {
    await sendDebuggerCommand(tabId, "Runtime.evaluate", {
      expression: "delete window.__brwForcedHoverNodes",
      returnByValue: true
    }).catch(() => {});
    await sendDebuggerCommand(tabId, "Runtime.releaseObjectGroup", { objectGroup }).catch(() => {});
  }
}

// captureScreenshotForTab obtains a real compositor surface without leaving the
// user's selected tab changed. Chrome's extension debugger only allows surface
// screenshots, and Page.captureScreenshot can remain pending indefinitely when
// its target tab is inactive. Activate inside the existing window (never raise
// the OS window), give the compositor one frame, capture with a hard deadline,
// then restore the prior tab in a finally block. The queue prevents concurrent
// captures from restoring each other's target.
async function captureScreenshotForTab(tabId, params) {
  return enqueueTabJuggle(() => captureScreenshotJuggled(tabId, params));
}

async function captureScreenshotJuggled(tabId, params) {
  let restoreTabId = null;
  try {
    const tab = await chrome.tabs.get(tabId);
    if (!tab.active) {
      const active = await chrome.tabs.query({ windowId: tab.windowId, active: true });
      restoreTabId = active?.[0]?.id || null;
      await chrome.tabs.update(tabId, { active: true });
      await new Promise((resolve) => setTimeout(resolve, 50));
    }
    const captureParams = { ...params };
    const fallbackViewport = captureParams.fallbackViewport || null;
    delete captureParams.fallbackViewport;
    await attach(tabId, { skipRevive: true });
    markActing(tabId);
    let timer = null;
    try {
      return await Promise.race([
        sendDebuggerCommand(tabId, "Page.captureScreenshot", captureParams),
        new Promise((_, reject) => {
          // Capped 800–900px captures normally complete in well under 250ms.
          // One second leaves generous load headroom while keeping the locked-
          // session print fallback responsive instead of burning the daemon's
          // whole request deadline on a compositor that cannot produce a frame.
          timer = setTimeout(() => reject(new Error("screenshot compositor capture timed out")), 1000);
        })
      ]);
    } catch (error) {
      // Chrome Stable can suspend every compositor surface while the macOS user
      // session is locked. Page.captureScreenshot then never resolves, even for
      // an active tab. Cancel that command and use Chrome's print renderer, which
      // remains available without a surface. The daemon rasterizes page 1 and
      // applies the original viewport clip, preserving the screenshot contract.
      await forceDetach(tabId);
      await attach(tabId, { skipRevive: true });
      const width = Math.max(1, Number(fallbackViewport?.width || captureParams?.clip?.width || 1280));
      const height = Math.max(1, Number(fallbackViewport?.height || captureParams?.clip?.height || 720));
      await sendDebuggerCommand(tabId, "Emulation.setEmulatedMedia", { media: "screen" }).catch(() => {});
      let printTimer = null;
      try {
        const printed = await Promise.race([
          sendDebuggerCommand(tabId, "Page.printToPDF", {
            printBackground: true,
            displayHeaderFooter: false,
            preferCSSPageSize: false,
            paperWidth: width / 96,
            paperHeight: height / 96,
            marginTop: 0,
            marginBottom: 0,
            marginLeft: 0,
            marginRight: 0,
            pageRanges: "1",
            transferMode: "ReturnAsBase64"
          }),
          new Promise((_, reject) => {
            printTimer = setTimeout(() => reject(new Error("screenshot print fallback timed out after 5000ms")), 5000);
          })
        ]);
        return { data: printed?.data || "", fallback: "pdf", viewport: { width, height }, captureError: String(error?.message || error) };
      } finally {
        if (printTimer) clearTimeout(printTimer);
        await sendDebuggerCommand(tabId, "Emulation.setEmulatedMedia", {}).catch(() => {});
      }
    } finally {
      if (timer) clearTimeout(timer);
    }
  } finally {
    if (restoreTabId && restoreTabId !== tabId) {
      await chrome.tabs.update(restoreTabId, { active: true }).catch(() => {});
    }
  }
}

function isDetachedDebuggerError(error) {
  const message = String(error?.message || error || "").toLowerCase();
  return message.includes("detached while handling command") ||
    message.includes("debugger is not attached") ||
    message.includes("target closed");
}

// resolveForegroundTabId computes the SINGLE authoritative "active tab": the
// active tab of the focused window. This is the one source of truth that both
// get_active_tab_id AND list_tabs's active flag are derived from, so every
// no-tab_id page tool (read, observe, snapshot, click, ...) targets the exact
// tab list_tabs marks active. Returns null when no foreground tab can be found
// (e.g. no window is focused and no fallback active tab exists).
//
// Precedence, in order:
//   1. The active tab of the focused normal/popup window — the genuine
//      foreground tab the user (or the agent's last focus_tab/open) is on.
//   2. state.activeTabId, but ONLY when it still resolves to a live tab — used
//      when Chrome reports no focused window (e.g. another OS app is foreground)
//      so the agent keeps acting on the tab it last targeted instead of drifting.
//   3. The active tab of the current window, then any active tab — last-resort
//      fallbacks so a headless/odd-focus state still resolves something.
//
// Critically, the cache is NOT trusted ahead of the focused-window scan: the
// previous implementation returned state.activeTabId whenever the tab merely
// existed, which drifted away from list_tabs (which scans focused windows) the
// moment the cache pointed at a background tab — the root cause of read/observe/
// list_tabs each resolving a different tab.
// A window is controllable unless it is a PWA/app or devtools surface. Unknown or
// undetermined window types default to controllable so tab resolution and
// list_tabs never silently drop real browser tabs — e.g. a freshly launched
// Chromium clone/test profile whose window has not yet classified as "normal".
function isControllableWindowType(win) {
  return !win || (win.type !== "app" && win.type !== "devtools");
}

// preferredNormalWindowId returns the safest window for a newly-created agent
// tab. A still-live agent tab wins when it already lives in a normal window;
// otherwise prefer the focused normal window, then any normal window. Popup/app
// windows are deliberately excluded because popup windows cannot be grouped and
// app/PWA windows must never become agent workspaces.
async function preferredNormalWindowId() {
  if (state.agentTabId) {
    const pinned = await chrome.tabs.get(state.agentTabId).catch(() => null);
    if (pinned?.windowId != null) {
      const win = await chrome.windows.get(pinned.windowId).catch(() => null);
      if (win?.type === "normal") return win.id;
    }
  }
  const windows = await chrome.windows.getAll({ windowTypes: ["normal"] }).catch(() => []);
  const normals = (windows || []).filter((win) => win?.type === "normal");
  const focused = normals.find((win) => win.focused);
  if (focused?.id != null) return focused.id;
  return normals[0]?.id ?? null;
}

async function resolveForegroundTabId() {
  // 0. The agent's PINNED working tab wins over the OS foreground. Set when the
  //    agent opens or focuses a tab (open_tab / focus_tab), and never moved by the
  //    user clicking around their own tabs/windows. This is the general fix for the
  //    "user selected another tab" bug class on a shared Chrome: once the agent owns
  //    a tab, every no-tab_id tool stays on it until the agent explicitly focuses
  //    elsewhere. Falls through only when that tab is gone or no longer controllable
  //    (e.g. it somehow became a PWA/app surface).
  if (state.agentTabId) {
    const pinned = await chrome.tabs.get(state.agentTabId).catch(() => null);
    if (pinned?.id) {
      const win = await chrome.windows.get(pinned.windowId).catch(() => null);
      if (isControllableWindowType(win)) return pinned.id;
    }
    state.agentTabId = null;
  }
  // 1. Active tab of the OS-focused controllable window. Enumerate ALL window types
  //    (not just normal/popup) so a clone/test-profile window is seen, then drop
  //    only PWA/devtools.
  const windows = await chrome.windows.getAll({
    populate: true,
    windowTypes: ["normal", "popup", "panel", "app", "devtools"]
  }).catch(() => []);
  for (const win of windows) {
    if (!win.focused || !isControllableWindowType(win)) continue;
    const tab = (win.tabs || []).find((candidate) => candidate.active);
    if (tab?.id) return tab.id;
  }
  // 2. No window is OS-focused (Chrome is backgrounded behind another app — the
  // common case when an agent drives it while the human works elsewhere). Use the
  // active tab of the LAST-focused window if it is controllable: deterministic and
  // stable, unlike currentWindow (unreliable in a service worker, which has no
  // window of its own) and unlike trusting the cache ahead of a live query. This is
  // the single source of truth that list_tabs and every no-tab_id page tool share.
  const lastFocused = await chrome.tabs.query({ active: true, lastFocusedWindow: true }).catch(() => []);
  for (const tab of lastFocused) {
    if (!tab?.id) continue;
    const win = await chrome.windows.get(tab.windowId).catch(() => null);
    if (isControllableWindowType(win)) return tab.id;
  }
  // 3. Honor the last-targeted tab if it is still alive (focus_tab/open set this)
  //    AND its window is still controllable — re-validate the type here so a stale
  //    cache (or a window that became a PWA/app surface) can NEVER leak a Google
  //    Chat / installed-PWA tab to the agent, even though publishActiveTab already
  //    refuses to cache one. Defense in depth on the exact path that drove Chat.
  if (state.activeTabId) {
    const cached = await chrome.tabs.get(state.activeTabId).catch(() => null);
    if (cached?.id) {
      const win = await chrome.windows.get(cached.windowId).catch(() => null);
      if (isControllableWindowType(win)) return cached.id;
      // Cached tab is no longer controllable; drop it so we don't keep retrying it.
      state.activeTabId = null;
    }
  }
  // 4. Last resort: any active tab in a controllable window.
  const any = await chrome.tabs.query({ active: true }).catch(() => []);
  for (const tab of any) {
    if (!tab?.id) continue;
    const win = await chrome.windows.get(tab.windowId).catch(() => null);
    if (isControllableWindowType(win)) return tab.id;
  }
  return null;
}

// activeTabId resolves and CACHES the authoritative foreground tab. The cache is
// a hint that self-heals on every call — it is refreshed to match the resolver
// rather than being trusted ahead of it, so it can never cause divergence.
async function activeTabId() {
  const id = await resolveForegroundTabId();
  if (id) {
    state.activeTabId = id;
    return id;
  }
  state.activeTabId = null;
  throw new Error("no active tab");
}

async function listTabSummaries() {
  // Enumerate EVERY tab via chrome.tabs.query({}) — which returns tabs across all
  // window types — then drop only PWA/app and devtools surfaces. The previous
  // chrome.windows.getAll({windowTypes:["normal","popup"]}) allowlist silently
  // returned 0 tabs whenever a window was not classified as normal/popup (e.g. a
  // freshly launched Chromium clone/test profile), even though those tabs were fully
  // controllable. The denylist preserves the PWA-exclusion intent without the false
  // negatives.
  //
  // A REJECTED query must propagate (handle() answers ok:false) instead of being
  // swallowed into an empty-but-ok list: the daemon cannot tell "no tabs" from
  // "tabs API failed" and agents then act on a fake-empty world — the
  // "list_tabs suddenly returns []" flake. An empty RESULT gets one brief retry
  // (a just-woken service worker can answer before tab state is warm); empty
  // after the retry is trusted, since macOS Chrome can genuinely run windowless.
  let allTabs = await chrome.tabs.query({});
  if (!allTabs?.length) {
    await new Promise((resolve) => setTimeout(resolve, 250));
    allTabs = await chrome.tabs.query({});
  }
  const winCache = new Map();
  const getWin = async (windowId) => {
    if (typeof windowId !== "number") return null;
    if (winCache.has(windowId)) return winCache.get(windowId);
    const win = await chrome.windows.get(windowId).catch(() => null);
    winCache.set(windowId, win);
    return win;
  };
  const groupsById = await tabGroupsById();
  // Resolve the authoritative foreground tab ONCE and mark exactly that tab as
  // active in the list. This guarantees list_tabs's active flag is identical to
  // what get_active_tab_id (and therefore every no-tab_id page tool) resolves —
  // they share resolveForegroundTabId(). Without this, list_tabs reported
  // Chrome's per-window active flag while page tools used the cache, and the two
  // diverged whenever they disagreed about which window was foreground.
  const foregroundId = await resolveForegroundTabId().catch(() => null);
  const out = [];
  for (const tab of allTabs) {
    const win = await getWin(tab.windowId);
    // Drop only PWA/app and devtools surfaces; include normal, popup, and any
    // window whose type cannot be determined (default to controllable).
    if (!isControllableWindowType(win)) continue;
    // chrome.tabs can lag a recent navigation by a few seconds. Re-fetch each tab
    // with chrome.tabs.get(), which talks to the live tab record, so list_tabs
    // reports the current URL/title. Fall back to the queried tab if the per-tab
    // fetch fails (tab closed mid enumeration), preserving metadata either way.
    let fresh = tab;
    if (typeof tab.id === "number") {
      const got = await chrome.tabs.get(tab.id).catch(() => null);
      if (got) fresh = got;
    }
    const summary = await tabSummaryFrom(fresh, win, groupsById);
    // Override Chrome's per-window active flag with the single authoritative
    // foreground tab so only one tab in the whole list is reported active, and it
    // is the same tab page tools act on. windowFocused is also forced true for that
    // tab so the daemon's (Active && WindowFocused) filter selects it even when
    // Chrome briefly reports no focused window.
    if (foregroundId != null && typeof fresh.id === "number") {
      const isForeground = fresh.id === foregroundId;
      summary.active = isForeground;
      if (isForeground) summary.windowFocused = true;
    }
    out.push(summary);
  }
  return out;
}

async function tabSummary(tab) {
  if (!tab) return {};
  let win = null;
  if (tab?.windowId) win = await chrome.windows.get(tab.windowId).catch(() => null);
  return tabSummaryFrom(tab, win);
}

async function tabSummaryFrom(tab, win, groupsById = null) {
  if (!tab) return {};
  const groupId = typeof tab.groupId === "number" ? tab.groupId : -1;
  const group = groupId >= 0
    ? (groupsById?.get(groupId) || await chrome.tabGroups.get(groupId).catch(() => null))
    : null;
  return {
    id: tab.id,
    url: tab.url || "",
    pendingUrl: tab.pendingUrl || "",
    title: tab.title || "",
    active: Boolean(tab.active),
    highlighted: Boolean(tab.highlighted),
    windowId: tab.windowId || win?.id || 0,
    windowFocused: Boolean(win?.focused),
    windowType: win?.type || "",
    groupId,
    groupTitle: group?.title || "",
    groupColor: group?.color || "",
    groupCollapsed: Boolean(group?.collapsed),
    openerTabId: tab.openerTabId || 0,
    // Renderer-health flags: a discarded (Memory Saver) or frozen (Energy
    // Saver / collapsed group) tab cannot run work until revived. tab.frozen
    // requires Chrome 132+; earlier Chrome simply reports false.
    discarded: Boolean(tab.discarded),
    frozen: Boolean(tab.frozen)
  };
}

async function listTabGroups() {
  const [groups, tabs] = await Promise.all([
    chrome.tabGroups.query({}).catch(() => []),
    chrome.tabs.query({}).catch(() => [])
  ]);
  const tabIdsByGroup = new Map();
  for (const tab of tabs || []) {
    if (typeof tab.groupId !== "number" || tab.groupId < 0 || typeof tab.id !== "number") continue;
    if (!tabIdsByGroup.has(tab.groupId)) tabIdsByGroup.set(tab.groupId, []);
    tabIdsByGroup.get(tab.groupId).push(tab.id);
  }
  return (groups || []).map((group) => tabGroupSummaryFrom(group, tabIdsByGroup.get(group.id) || []));
}

function tabGroupSummaryFrom(group, tabIds = []) {
  return {
    id: group.id,
    title: group.title || "",
    color: group.color || "",
    collapsed: Boolean(group.collapsed),
    windowId: group.windowId || 0,
    tabIds,
    tabCount: tabIds.length
  };
}

async function tabGroupsById() {
  const groups = await chrome.tabGroups.query({}).catch(() => []);
  return new Map((groups || []).map((group) => [group.id, group]));
}

async function groupTabForParams(tab, params = {}) {
  if (typeof tab?.id !== "number") return null;
  const explicitGroupId = parseGroupId(params?.groupId);
  const groupName = String(params?.groupName || "").trim();
  const hasColor = params?.groupColor !== undefined && params?.groupColor !== null && params?.groupColor !== "";
  const color = normalizeGroupColor(params?.groupColor, "blue");
  if (explicitGroupId != null) {
    const groupId = await chrome.tabs.group({ tabIds: [tab.id], groupId: explicitGroupId });
    const update = {};
    if (groupName) update.title = groupName;
    if (hasColor) update.color = color;
    if (Object.keys(update).length > 0) await chrome.tabGroups.update(groupId, update);
    return groupId;
  }
  if (!groupName) return null;
  const existing = await findGroupByTitle(groupName, tab.windowId);
  const groupArgs = { tabIds: [tab.id] };
  if (existing?.id != null) groupArgs.groupId = existing.id;
  else if (typeof tab.windowId === "number") {
    // See group_tabs: service workers have no stable implicit current window.
    // Explicit window targeting is required for reliable native vertical-tab
    // group creation and is also safer when Chrome has several windows.
    groupArgs.createProperties = { windowId: tab.windowId };
  }
  const groupId = await chrome.tabs.group(groupArgs);
  const update = { title: groupName };
  if (hasColor || !existing) update.color = color;
  await chrome.tabGroups.update(groupId, update);
  return groupId;
}

async function findGroupByTitle(title, windowId = null) {
  const query = {};
  if (typeof windowId === "number") query.windowId = windowId;
  const groups = await chrome.tabGroups.query(query).catch(() => []);
  return (groups || []).find((group) => (group.title || "") === title) || null;
}

function parseGroupId(value) {
  if (value === undefined || value === null || value === "") return null;
  const n = Number(value);
  if (!Number.isInteger(n) || n < 0) return null;
  return n;
}

function tabGroupingFailureMessage(error) {
  const message = String(error?.message || error || "tab grouping failed");
  if (message.includes("Grouping is not supported by tabs in this window")) {
    return "tab grouping unavailable: Chromium rejected grouping in the target window; keep tracking and closing the owned tab by id";
  }
  return message;
}

function hasGroupTarget(params = {}) {
  return parseGroupId(params?.groupId) != null || String(params?.groupName || "").trim() !== "";
}

function normalizeGroupColor(value, fallback = "") {
  const color = String(value || "").trim();
  const allowed = new Set(["grey", "blue", "red", "yellow", "green", "pink", "purple", "cyan", "orange"]);
  return allowed.has(color) ? color : fallback;
}

async function publishActiveTab(tabId) {
  if (!tabId) return;
  const tab = await chrome.tabs.get(tabId).catch(() => null);
  if (!tab) return;
  // CHOKE POINT: never let a PWA/app or devtools surface become the cached active
  // tab. chrome.tabs.onActivated / onCreated fire for EVERY window — including the
  // Google Chat (and any installed-PWA) app window — so without this guard a user
  // clicking into Google Chat would stash that tab in state.activeTabId, and
  // resolveForegroundTabId's cache fallback would then hand the agent the Chat PWA.
  // The agent must NEVER drive those windows. onFocusChanged already filters them;
  // this closes the onActivated/onCreated path too.
  const win = await chrome.windows.get(tab.windowId).catch(() => null);
  if (!isControllableWindowType(win)) return;
  state.activeTabId = tabId;
  await connect();
  const summary = await tabSummary(tab);
  send({
    type: "active_tab",
    tabId,
    tab: summary,
    url: summary.url || "",
    title: summary.title || ""
  });
}

function startKeepAlive() {
  stopKeepAlive();
  state.keepAliveTimer = setInterval(() => {
    send({ type: "keepalive", at: Date.now() });
    sweepIdleDebuggers().catch(() => {});
  }, KEEPALIVE_INTERVAL_MS);
  state.statusTimer = setInterval(() => {
    probeDaemonStatus().catch(() => {});
  }, DAEMON_STATUS_INTERVAL_MS);
}

function stopKeepAlive() {
  if (state.keepAliveTimer) clearInterval(state.keepAliveTimer);
  if (state.statusTimer) clearInterval(state.statusTimer);
  state.keepAliveTimer = null;
  state.statusTimer = null;
}

function ensureConnectAlarm() {
  chrome.alarms.create("brw-connect", { delayInMinutes: 0.05, periodInMinutes: 0.5 }).catch(() => {});
}

// ensureOffscreen creates the offscreen keepalive document if it is not already
// open. The offscreen page is exempt from the MV3 idle timer and holds a
// long-lived port to this worker (offscreen.js), preventing Chrome from
// terminating it — which keeps the daemon WebSocket connected and active-tab
// resolution reliable while Chrome is idle in the background. Safe to call
// repeatedly; a second create on an existing document is caught and ignored.
async function ensureOffscreen() {
  if (offscreenSetupPromise) return offscreenSetupPromise;
  offscreenSetupPromise = (async () => {
    try {
      if (!chrome.offscreen) return;
      if (await chrome.offscreen.hasDocument()) return;
      const reason = chrome.offscreen.Reason || {};
      await chrome.offscreen.createDocument({
        url: "offscreen.html",
        reasons: [reason.AUDIO_PLAYBACK || "AUDIO_PLAYBACK", reason.BLOBS || "BLOBS"],
        justification:
          "Keep the service worker alive so the bridge WebSocket and active-tab resolution remain reliable while Chrome is idle."
      });
    } catch (_) {
      // Document already exists (creation race) or the offscreen API is unavailable.
    }
  })().finally(() => {
    offscreenSetupPromise = null;
  });
  return offscreenSetupPromise;
}

function ensureObserver(tabId) {
  if (state.observerInjected.has(tabId)) return;
  state.observerInjected.add(tabId);
  const observerScript = `(function() {
    if (window.__brwObserver) return;
    window.__brwObserver = true;
    window.__brwDirty = false;
    window.__brwConsole = [];
    const observer = new MutationObserver(function() {
      window.__brwDirty = true;
    });
    observer.observe(document.documentElement, {
      childList: true,
      subtree: true,
      attributes: true,
      characterData: true
    });
    function stringify(value) {
      try {
        if (value instanceof Error) return value.stack || value.message || String(value);
        if (typeof value === 'object' && value !== null) return JSON.stringify(value, function(_key, item) {
          return typeof item === 'bigint' ? String(item) : item;
        });
        return String(value);
      } catch (_err) {
        try { return String(value); } catch (_ignored) { return '[unprintable]'; }
      }
    }
    ['log','warn','error','info','debug'].forEach(function(level) {
      const orig = console[level];
      console[level] = function() {
        var text = Array.from(arguments).map(stringify).join(' ');
        window.__brwConsole.push({level: level, text: text.slice(0, 1000), timestamp: new Date().toISOString()});
        if (window.__brwConsole.length > 200) window.__brwConsole.shift();
        if (orig.apply) orig.apply(console, arguments); else orig(arguments);
      };
    });
  })()`;
  // Attach via the TRACKED attach() so this debugger session is recorded in
  // state.attachedTabs and is therefore released by detachAll / sweepIdleDebuggers
  // / detach. The previous raw chrome.debugger.attach() here was invisible to that
  // bookkeeping, so the observer's attachment leaked and left a stuck "being
  // debugged" banner. On failure, drop the flag so a later snapshot can retry.
  attach(tabId)
    .then(() => chrome.debugger.sendCommand({ tabId }, "Runtime.evaluate", {
      expression: observerScript,
      returnByValue: true
    }))
    .catch(() => { state.observerInjected.delete(tabId); });
}

// createNotification turns a bridge "notify" command into a basic desktop
// notification. The icon path falls back to the extension action icon if none
// is bundled; chrome.notifications requires an iconUrl, so we use the
// extension's own packaged URL. Returns { ok, delivery, note } so the daemon
// can report the honest delivery channel rather than faking success.
function createNotification(params) {
  const title = String(params.title || "brw");
  const messageText = String(params.message || "");
  const options = {
    type: "basic",
    iconUrl: chrome.runtime.getURL("icons/icon-128.png"),
    title,
    message: messageText,
    priority: params.kind === "needs_input" || params.kind === "error" ? 2 : 0,
    requireInteraction: params.kind === "needs_input"
  };
  return new Promise((resolve) => {
    try {
      chrome.notifications.create("", options, (notificationId) => {
        if (chrome.runtime.lastError) {
          // Retry without an iconUrl — a missing packaged icon is the most
          // common create() failure, and the notification is still useful
          // without one.
          const fallback = Object.assign({}, options);
          delete fallback.iconUrl;
          chrome.notifications.create("", fallback, (retryId) => {
            if (chrome.runtime.lastError) {
              resolve({ ok: false, delivery: "unavailable", note: String(chrome.runtime.lastError.message || chrome.runtime.lastError) });
            } else {
              resolve({ ok: true, delivery: "extension", note: retryId || "" });
            }
          });
        } else {
          resolve({ ok: true, delivery: "extension", note: notificationId || "" });
        }
      });
    } catch (error) {
      resolve({ ok: false, delivery: "unavailable", note: String(error && error.message ? error.message : error) });
    }
  });
}

function send(payload) {
  const socket = state.socket;
  if (!socket || socket.readyState !== WebSocket.OPEN) return false;
  try {
    socket.send(JSON.stringify(payload));
    return true;
  } catch (error) {
    state.lastError = `send failed: ${String(error?.message || error)}`;
    if (state.socket === socket) {
      try { socket.close(); } catch (_) {}
      state.socket = null;
      scheduleReconnect(state.lastError);
    }
    return false;
  }
}

// fetchBridgeToken reads the per-launch handshake token from the daemon's
// loopback /status endpoint. Returns "" when the daemon serves no token (an
// older/no-auth daemon), so the extension stays compatible: such a daemon also
// skips verification. The extension can read the response body because the
// loopback origin is in host_permissions; a web page cannot.
async function fetchBridgeToken(config) {
  try {
    // Bounded so a hung /status can never block hello indefinitely; the bridge's
    // own handshake timeout would otherwise drop us and force a reconnect loop.
    const response = await fetch(config.statusUrl, { cache: "no-store", signal: AbortSignal.timeout(DAEMON_STATUS_TIMEOUT_MS) });
    if (!response.ok) return "";
    const status = await response.json().catch(() => ({}));
    return typeof status?.token === "string" ? status.token : "";
  } catch (_) {
    return "";
  }
}

async function probeDaemonStatus() {
  if (!isSocketOpen()) return false;
  // Alarms, the keepalive interval, and connect({probe:true}) can land at the
  // same moment. Coalesce them so one slow /status response cannot create a
  // burst of probes that all count as independent failures.
  if (state.statusProbeInFlight) return state.statusProbeInFlight;
  const socket = state.socket;
  const probe = (async () => {
    try {
      const config = await loadBridgeConfig();
      const response = await fetch(config.statusUrl, {
        cache: "no-store",
        signal: AbortSignal.timeout(DAEMON_STATUS_TIMEOUT_MS)
      });
      if (!response.ok) throw new Error(`status ${response.status}`);
      const status = await response.json().catch(() => ({}));
      if (!status.connected) throw new Error("daemon reports no extension connection");
      assertDaemonIdentity(config, status.identity || {});
      if (state.socket === socket) {
        state.statusProbeFailures = 0;
        if (state.lastError.startsWith("daemon status probe failed:")) state.lastError = "";
      }
      return true;
    } catch (error) {
      // A probe belonging to an old socket must never close or downgrade the
      // replacement connection that won the race while the fetch was pending.
      if (state.socket !== socket || socket?.readyState !== WebSocket.OPEN) return false;
      state.statusProbeFailures += 1;
      const message = `daemon status probe failed: ${String(error?.message || error)}`;
      state.lastError = `${message} (${state.statusProbeFailures}/${MAX_DAEMON_STATUS_FAILURES})`;
      if (state.statusProbeFailures < MAX_DAEMON_STATUS_FAILURES) return false;

      state.statusProbeFailures = 0;
      state.socket = null;
      try { socket.close(); } catch (_) {}
      detachAll().catch(() => {});
      scheduleReconnect(message);
      return false;
    }
  })();
  state.statusProbeInFlight = probe;
  try {
    return await probe;
  } finally {
    if (state.statusProbeInFlight === probe) state.statusProbeInFlight = null;
  }
}

function assertDaemonIdentity(config, identity) {
  for (const field of ["workspace", "profile"]) {
    if (!config[field]) continue;
    if (!identity[field]) throw new Error(`daemon status does not report ${field}`);
    if (identity[field] !== config[field]) {
      throw new Error(`daemon ${field} mismatch: got ${identity[field]}, want ${config[field]}`);
    }
  }
}

function scheduleReconnect(reason) {
  stopKeepAlive();
  clearTimeout(state.reconnectTimer);
  const delay = Math.min(1000 * (state.reconnectAttempt + 1), MAX_RECONNECT_DELAY_MS);
  state.reconnectAttempt += 1;
  markBridgeStatus("disconnected", `${reason}; reconnecting in ${delay}ms`).catch(() => {});
  state.reconnectTimer = setTimeout(() => {
    connect({ probe: true });
  }, delay);
}

function isSocketOpen() {
  return Boolean(state.socket && state.socket.readyState === WebSocket.OPEN);
}

function isSocketConnecting() {
  return Boolean(state.socket && state.socket.readyState === WebSocket.CONNECTING);
}

function setBridgeBadge(status) {
  if (status === "connected") {
    chrome.action.setBadgeText({ text: "on" }).catch(() => {});
    chrome.action.setBadgeBackgroundColor({ color: "#1a7f37" }).catch(() => {});
    chrome.action.setTitle({ title: "brw connected" }).catch(() => {});
    return;
  }
  if (status === "connecting" || status === "starting") {
    chrome.action.setBadgeText({ text: "..." }).catch(() => {});
    chrome.action.setBadgeBackgroundColor({ color: "#bf8700" }).catch(() => {});
    chrome.action.setTitle({ title: "brw connecting" }).catch(() => {});
    return;
  }
  chrome.action.setBadgeText({ text: "" }).catch(() => {});
  chrome.action.setTitle({ title: "brw disconnected" }).catch(() => {});
}

async function markBridgeStatus(status, detail = "") {
  setBridgeBadge(status);
  const config = state.bridgeConfig || normalizeBridgeConfig({});
  const value = {
    status,
    bridgeUrl: config.bridgeUrl,
    statusUrl: config.statusUrl,
    workspace: config.workspace,
    profile: config.profile,
    label: config.label,
    detail,
    attempt: state.reconnectAttempt,
    lastError: state.lastError,
    at: new Date().toISOString()
  };
  await chrome.storage.local.set({ [BRIDGE_STATUS_KEY]: value });
}
