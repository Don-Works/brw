const BRIDGE_URL = "ws://127.0.0.1:17311/extension";
const PROTOCOL_VERSION = "0.1.5";

const state = {
  socket: null,
  reconnectTimer: null,
  keepAliveTimer: null,
  attachedTabs: new Set(),
  activeTabId: null,
  reconnectAttempt: 0,
  snapshotCache: new Map(),
  observerInjected: new Set()
};

chrome.runtime.onInstalled.addListener(() => {
  chrome.alarms.create("agent-browser-connect", { periodInMinutes: 1 });
  connect();
});

chrome.runtime.onStartup.addListener(connect);
chrome.action.onClicked.addListener(async (tab) => {
  await connect();
  if (tab?.id) {
    send({ type: "active_tab", tabId: tab.id, url: tab.url, title: tab.title });
  }
});
chrome.alarms.onAlarm.addListener((alarm) => {
  if (alarm.name === "agent-browser-connect") connect();
});
chrome.tabs.onActivated.addListener(async (activeInfo) => {
  await publishActiveTab(activeInfo?.tabId);
});
chrome.tabs.onCreated.addListener(async (tab) => {
  if (tab?.active) await publishActiveTab(tab.id);
});
chrome.tabs.onRemoved.addListener((tabId) => {
  state.attachedTabs.delete(tabId);
  state.snapshotCache.delete(tabId);
  state.observerInjected.delete(tabId);
  if (state.activeTabId === tabId) state.activeTabId = null;
});
chrome.windows.onFocusChanged.addListener(async (windowId) => {
  if (windowId === chrome.windows.WINDOW_ID_NONE) return;
  const tabs = await chrome.tabs.query({ windowId, active: true }).catch(() => []);
  if (tabs[0]?.id) await publishActiveTab(tabs[0].id);
});
chrome.debugger.onDetach.addListener((source) => {
  if (source.tabId) state.attachedTabs.delete(source.tabId);
});

connect();

async function connect() {
  if (state.socket && state.socket.readyState === WebSocket.OPEN) return;
  if (state.socket && state.socket.readyState === WebSocket.CONNECTING) return;

  clearTimeout(state.reconnectTimer);
  await rememberStatus("connecting");
  state.socket = new WebSocket(BRIDGE_URL);
  state.socket.onopen = async () => {
    state.reconnectAttempt = 0;
    await rememberStatus("connected");
    chrome.action.setBadgeText({ text: "on" }).catch(() => {});
    chrome.action.setBadgeBackgroundColor({ color: "#1a7f37" }).catch(() => {});
    startKeepAlive();
    const platform = await chrome.runtime.getPlatformInfo().catch(() => ({}));
    send({
      type: "hello",
      hello: {
        source: "agent-browser-extension",
        version: PROTOCOL_VERSION,
        chrome: navigator.userAgent,
        platform: platform.os || ""
      }
    });
  };
  state.socket.onclose = () => {
    stopKeepAlive();
    chrome.action.setBadgeText({ text: "" }).catch(() => {});
    const delay = Math.min(1000 * (state.reconnectAttempt + 1), 10000);
    state.reconnectAttempt += 1;
    rememberStatus(`closed; reconnecting in ${delay}ms`).catch(() => {});
    state.reconnectTimer = setTimeout(connect, delay);
  };
  state.socket.onerror = (event) => {
    rememberStatus(`websocket error ${String(event?.type || "")}`).catch(() => {});
    try { state.socket.close(); } catch (_) {}
  };
  state.socket.onmessage = async (event) => {
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
    if (message.type === "open_tab") {
      const tab = await chrome.tabs.create({ url: message.params?.url || "about:blank", active: false });
      state.activeTabId = tab.id || null;
      const groupName = message.params?.groupName;
      if (groupName && tab.id) {
        try {
          const groupId = await chrome.tabs.group({ tabIds: [tab.id] });
          await chrome.tabGroups.update(groupId, { title: groupName, color: message.params?.groupColor || "blue" });
        } catch (_) {}
      }
      send({ id: message.id, ok: true, result: await tabSummary(tab) });
      return;
    }
    if (message.type === "focus_tab") {
      const tabId = Number(message.params?.tabId);
      const before = await chrome.tabs.get(tabId).catch(() => null);
      if (before?.windowId) await chrome.windows.update(before.windowId, { focused: true });
      const tab = await chrome.tabs.update(tabId, { active: true });
      state.activeTabId = tabId;
      send({ id: message.id, ok: true, result: await tabSummary(tab) });
      return;
    }
    if (message.type === "close_tab") {
      const tabId = Number(message.params?.tabId);
      await chrome.tabs.remove(tabId);
      send({ id: message.id, ok: true, result: { closed: tabId } });
      return;
    }
    if (message.type === "group_tabs") {
      const tabIds = (message.params?.tabIds || []).map(Number);
      const name = message.params?.name || "agent-browser";
      const color = message.params?.color || "blue";
      if (tabIds.length === 0) {
        send({ id: message.id, ok: false, error: "tabIds is required" });
        return;
      }
      const groupId = await chrome.tabs.group({ tabIds });
      await chrome.tabGroups.update(groupId, { title: name, color });
      send({ id: message.id, ok: true, result: { groupId, tabIds, name, color } });
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
      const cached = state.snapshotCache.get(tabId);
      if (cached) {
        // Check if the page's MutationObserver flagged DOM changes
        let pageDirty = false;
        try {
          await attach(tabId);
          const evalResult = await chrome.debugger.sendCommand(
            { tabId },
            "Runtime.evaluate",
            { expression: "!!window.__agentBrowserDirty", returnByValue: true }
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
            { expression: "window.__agentBrowserDirty = false", returnByValue: true }
          );
        } catch (_) {}
      }
      send({ id: message.id, ok: true, result: { cached: false } });
      return;
    }
    if (message.type === "snapshot_result") {
      const tabId = Number(message.params?.tabId || (await activeTabId()));
      state.snapshotCache.set(tabId, { dirty: false, snapshot: message.params?.snapshot });
      ensureObserver(tabId);
      send({ id: message.id, ok: true, result: { stored: true } });
      return;
    }
    if (message.type === "cdp") {
      const tabId = Number(message.params?.tabId || (await activeTabId()));
      await attach(tabId);
      const result = await chrome.debugger.sendCommand(
        { tabId },
        message.params?.method,
        message.params?.params || {}
      );
      send({ id: message.id, ok: true, result: result || {} });
      return;
    }
    if (message.type === "show_indicator") {
      const tabId = Number(message.params?.tabId || (await activeTabId()));
      await attach(tabId);
      const indicatorScript = `(function() {
        if (window.__agentBrowserIndicator) return;
        window.__agentBrowserIndicator = true;
        var el = document.createElement('div');
        el.id = 'agent-browser-indicator';
        el.style.cssText = 'position:fixed;top:8px;right:8px;z-index:2147483647;background:#1a7f37;color:white;padding:6px 12px;border-radius:6px;font:600 12px system-ui;box-shadow:0 2px 8px rgba(0,0,0,0.2);pointer-events:none;opacity:0.95;transition:opacity 0.3s;';
        el.textContent = '🤖 agent-browser active';
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
        var el = document.getElementById('agent-browser-indicator');
        if (el) el.remove();
        window.__agentBrowserIndicator = false;
      })()`;
      await chrome.debugger.sendCommand({ tabId }, "Runtime.evaluate", { expression: hideScript, returnByValue: true }).catch(() => {});
      send({ id: message.id, ok: true, result: { hidden: true } });
      return;
    }
    send({ id: message.id, ok: false, error: `unknown message type ${message.type}` });
  } catch (error) {
    rememberStatus(`request failed: ${String(error?.message || error)}`).catch(() => {});
    send({ id: message.id, ok: false, error: String(error?.message || error) });
  }
}

async function attach(tabId) {
  if (state.attachedTabs.has(tabId)) return;
  try {
    await chrome.debugger.attach({ tabId }, "1.3");
  } catch (error) {
    if (!String(error?.message || error).includes("Another debugger is already attached")) throw error;
  }
  state.attachedTabs.add(tabId);
}

async function activeTabId() {
  if (state.activeTabId) {
    const tab = await chrome.tabs.get(state.activeTabId).catch(() => null);
    if (tab?.id) return tab.id;
    state.activeTabId = null;
  }
  const windows = await chrome.windows.getAll({
    populate: true,
    windowTypes: ["normal", "popup"]
  }).catch(() => []);
  for (const win of windows) {
    if (!win.focused) continue;
    const tab = (win.tabs || []).find((candidate) => candidate.active);
    if (tab?.id) {
      state.activeTabId = tab.id;
      return tab.id;
    }
  }
  const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
  if (tabs[0]?.id) {
    state.activeTabId = tabs[0].id;
    return tabs[0].id;
  }
  const any = await chrome.tabs.query({ active: true });
  if (any[0]?.id) {
    state.activeTabId = any[0].id;
    return any[0].id;
  }
  throw new Error("no active tab");
}

async function listTabSummaries() {
  const windows = await chrome.windows.getAll({
    populate: true,
    windowTypes: ["normal", "popup"]
  });
  const out = [];
  for (const win of windows) {
    for (const tab of win.tabs || []) out.push(tabSummaryFrom(tab, win));
  }
  return out;
}

async function tabSummary(tab) {
  if (!tab) return {};
  let win = null;
  if (tab?.windowId) win = await chrome.windows.get(tab.windowId).catch(() => null);
  return tabSummaryFrom(tab, win);
}

function tabSummaryFrom(tab, win) {
  if (!tab) return {};
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
    openerTabId: tab.openerTabId || 0
  };
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
  }, 20 * 1000);
}

function stopKeepAlive() {
  if (state.keepAliveTimer) clearInterval(state.keepAliveTimer);
  state.keepAliveTimer = null;
}

function ensureObserver(tabId) {
  if (state.observerInjected.has(tabId)) return;
  state.observerInjected.add(tabId);
  const observerScript = `(function() {
    if (window.__agentBrowserObserver) return;
    window.__agentBrowserObserver = true;
    window.__agentBrowserDirty = false;
    window.__agentBrowserConsole = [];
    const observer = new MutationObserver(function() {
      window.__agentBrowserDirty = true;
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
        window.__agentBrowserConsole.push({level: level, text: text.slice(0, 500)});
        if (window.__agentBrowserConsole.length > 200) window.__agentBrowserConsole.shift();
        if (orig.apply) orig.apply(console, arguments); else orig(arguments);
      };
    });
  })()`;
  chrome.debugger.attach({ tabId }, "1.3").catch(() => {}).then(() => {
    chrome.debugger.sendCommand({ tabId }, "Runtime.evaluate", {
      expression: observerScript,
      returnByValue: true
    }).catch(() => {});
  });
}

function send(payload) {
  if (!state.socket || state.socket.readyState !== WebSocket.OPEN) return;
  state.socket.send(JSON.stringify(payload));
}

async function rememberStatus(status) {
  const value = {
    status,
    bridgeUrl: BRIDGE_URL,
    at: new Date().toISOString()
  };
  await chrome.storage.local.set({ agentBrowserBridge: value });
}
