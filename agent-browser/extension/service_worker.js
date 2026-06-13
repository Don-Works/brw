const BRIDGE_URL = "ws://127.0.0.1:17311/extension";
const PROTOCOL_VERSION = "0.1.0";

const state = {
  socket: null,
  reconnectTimer: null,
  keepAliveTimer: null,
  attachedTabs: new Set(),
  reconnectAttempt: 0
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
chrome.tabs.onActivated.addListener(connect);
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
      const tabs = await chrome.tabs.query({});
      send({ id: message.id, ok: true, result: tabs.map(tabSummary) });
      return;
    }
    if (message.type === "open_tab") {
      const tab = await chrome.tabs.create({ url: message.params?.url || "about:blank", active: true });
      send({ id: message.id, ok: true, result: tabSummary(tab) });
      return;
    }
    if (message.type === "focus_tab") {
      const tabId = Number(message.params?.tabId);
      const tab = await chrome.tabs.update(tabId, { active: true });
      if (tab?.windowId) await chrome.windows.update(tab.windowId, { focused: true });
      send({ id: message.id, ok: true, result: tabSummary(tab) });
      return;
    }
    if (message.type === "close_tab") {
      const tabId = Number(message.params?.tabId);
      await chrome.tabs.remove(tabId);
      send({ id: message.id, ok: true, result: { closed: tabId } });
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
  const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
  if (tabs[0]?.id) return tabs[0].id;
  const any = await chrome.tabs.query({ active: true });
  if (any[0]?.id) return any[0].id;
  throw new Error("no active tab");
}

function tabSummary(tab) {
  return {
    id: tab.id,
    url: tab.url || "",
    title: tab.title || "",
    active: Boolean(tab.active)
  };
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
