const BRIDGE_URL = "ws://127.0.0.1:17311/extension";
const BRIDGE_STATUS_URL = "http://127.0.0.1:17311/status";
const BRIDGE_CONFIG_KEY = "brwBridgeConfig";
const BRIDGE_STATUS_KEY = "brwBridge";
const PROTOCOL_VERSION = "0.2.0";
const KEEPALIVE_INTERVAL_MS = 5 * 1000;
const DAEMON_STATUS_INTERVAL_MS = 10 * 1000;
const MAX_RECONNECT_DELAY_MS = 10 * 1000;
// Detach a tab's debugger after this long without a CDP command, so brw doesn't
// hold debugger sessions on idle tabs of the user's real Chrome.
const IDLE_DETACH_MS = 120 * 1000;
// How long after a brw-initiated CDP command a JS dialog on that tab is treated
// as brw's own (auto-accepted to let the agent's flow proceed). Outside this
// window a dialog is the user's / a background script's, and is answered with the
// NON-destructive choice instead of blindly accepting.
const BRW_ACTING_WINDOW_MS = 8 * 1000;
// CDP methods brw refuses to forward, enforcing the README's promise that it
// "never reads cookies/passwords/passkeys": all cookie read/write methods (which
// can reach HttpOnly cookies that page JS cannot) and the entire Storage domain.
// brw itself uses none of these, so denying them never breaks a feature — it
// turns the privacy claim from a convention into an enforced boundary that holds
// even against a rogue server that answered the extension's outbound socket.
function isDeniedCdpMethod(method) {
  const m = String(method || "");
  return /cookie/i.test(m) || m.startsWith("Storage.");
}
let offscreenSetupPromise = null;
let packagedDefaultConfigPromise = null;

const state = {
  socket: null,
  connectPromise: null,
  reconnectTimer: null,
  keepAliveTimer: null,
  statusTimer: null,
  attachedTabs: new Set(),
  // attachUsedAt records the last time each attached tab's debugger was used, so
  // sweepIdleDebuggers can release debuggers that have gone idle within a long
  // connection — bounding how many debugger sessions brw holds on the user's
  // real Chrome at once (accumulating attachments destabilize renderers).
  attachUsedAt: new Map(),
  activeTabId: null,
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
  actingUntil: new Map()
};

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
  if (state.activeTabId === tabId) state.activeTabId = null;
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
  }
});
// Capture CDP events the daemon needs to observe out-of-band. The only one today
// is Page.fileChooserOpened: when file-chooser interception is enabled
// (Page.setInterceptFileChooserDialog), clicking a file-picker trigger fires this
// event with the chooser's backendNodeId instead of opening the native OS dialog.
// We stash the latest per tab so the daemon can poll for it via
// get_file_chooser_event and then set the file with DOM.setFileInputFiles.
chrome.debugger.onEvent.addListener((source, method, params) => {
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
  return {
    config,
    bridge: data[BRIDGE_STATUS_KEY] || null,
    socket: isSocketOpen() ? "open" : (isSocketConnecting() ? "connecting" : "closed")
  };
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
      // Create the tab ACTIVE within its window so it becomes the authoritative
      // foreground tab (resolveForegroundTabId returns it) and subsequent
      // no-tab_id page tools — read, observe, snapshot — follow to the new tab,
      // matching what list_tabs now reports as active. active:true only changes
      // which tab is active inside the window; it does NOT raise Chrome over
      // other OS apps (that needs chrome.windows.update({focused:true}), which we
      // intentionally do not call here so automation never steals the user's OS
      // foreground).
      const tab = await chrome.tabs.create({ url: message.params?.url || "about:blank", active: true });
      state.activeTabId = tab.id || null;
      let resultTab = tab;
      if (tab.id && hasGroupTarget(message.params)) {
        const groupId = await groupTabForParams(tab, message.params);
        // Grouping can DEMOTE the freshly-opened active tab: a collapsed group
        // cannot hold the active tab, so Chrome deactivates the newcomer and
        // activates an adjacent visible tab. Re-expand the group and re-activate
        // the opened tab so it stays the foreground tab the agent will act on —
        // otherwise the next no-tab_id tool resolves the wrong tab.
        if (typeof groupId === "number" && groupId >= 0) {
          await chrome.tabGroups.update(groupId, { collapsed: false }).catch(() => {});
        }
        await chrome.tabs.update(tab.id, { active: true }).catch(() => {});
        resultTab = await chrome.tabs.get(tab.id).catch(() => tab);
      }
      send({ id: message.id, ok: true, result: await tabSummary(resultTab) });
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
      const groupId = await chrome.tabs.group(groupArgs);
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

async function attach(tabId) {
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
  // Enable Page events so we receive javascriptDialogOpening for auto-dismiss.
  chrome.debugger.sendCommand({ tabId }, "Page.enable", {}).catch(() => {});
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

async function resolveForegroundTabId() {
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
  // 3. Honor the last-targeted tab if it is still alive (focus_tab/open set this).
  if (state.activeTabId) {
    const cached = await chrome.tabs.get(state.activeTabId).catch(() => null);
    if (cached?.id) return cached.id;
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
  const allTabs = await chrome.tabs.query({}).catch(() => []);
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
    openerTabId: tab.openerTabId || 0
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
  state.activeTabId = tabId;
  await connect();
  const tab = await chrome.tabs.get(tabId).catch(() => null);
  const summary = tab ? await tabSummary(tab) : { id: tabId };
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
    ['log','warn','error','info'].forEach(function(level) {
      const orig = console[level];
      console[level] = function() {
        var text = Array.from(arguments).map(function(a) {
          try { return typeof a === 'object' ? JSON.stringify(a) : String(a); } catch(e) { return String(a); }
        }).join(' ');
        window.__brwConsole.push({level: level, text: text.slice(0, 500)});
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
    iconUrl: chrome.runtime.getURL("icon.png"),
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
    const response = await fetch(config.statusUrl, { cache: "no-store", signal: AbortSignal.timeout(2000) });
    if (!response.ok) return "";
    const status = await response.json().catch(() => ({}));
    return typeof status?.token === "string" ? status.token : "";
  } catch (_) {
    return "";
  }
}

async function probeDaemonStatus() {
  if (!isSocketOpen()) return false;
  try {
    const config = await loadBridgeConfig();
    const response = await fetch(config.statusUrl, { cache: "no-store" });
    if (!response.ok) throw new Error(`status ${response.status}`);
    const status = await response.json().catch(() => ({}));
    if (!status.connected) throw new Error("daemon reports no extension connection");
    assertDaemonIdentity(config, status.identity || {});
    return true;
  } catch (error) {
    const message = `daemon status probe failed: ${String(error?.message || error)}`;
    state.lastError = message;
    const socket = state.socket;
    if (socket && socket.readyState === WebSocket.OPEN) {
      try { socket.close(); } catch (_) {}
    }
    state.socket = null;
    scheduleReconnect(message);
    return false;
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
