// Regression harness: runs the ACTUAL service_worker.js in a vm sandbox with a
// mocked chrome API, then asserts the agent-tab-pin + PWA/app-window guard
// behaviour that keeps brw from drifting onto the user's tabs (e.g. the Google
// Chat PWA) on a Chrome the human drives at the same time. Run: `make test-extension`
// or `node extension/tab_resolution_test.mjs`.
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import vm from "node:vm";

const here = dirname(fileURLToPath(import.meta.url));
const SRC_PATH = process.argv[2] || join(here, "service_worker.js");
let src = readFileSync(SRC_PATH, "utf8");

// ---- mutable browser model the mock reads from ----
const model = {
  tabs: new Map(),      // id -> {id, windowId, active, url, title}
  windows: new Map(),   // id -> {id, type, focused}
  events: {},           // captured event listeners by full path
};
function setTab(t) { model.tabs.set(t.id, t); }
function setWin(w) { model.windows.set(w.id, w); }

function makeEvent(path) {
  const ev = { _l: [], addListener(f) { this._l.push(f); }, removeListener() {}, hasListener() { return false; } };
  model.events[path] = ev;
  return ev;
}

// Flat override map keyed by full dotted path.
const overrides = {
  "runtime.lastError": undefined,
  "runtime.getURL": (p) => "mock://" + p,
  "runtime.getPlatformInfo": async () => ({ os: "mac" }),
  "windows.WINDOW_ID_NONE": -1,
  "offscreen.Reason": { BLOBS: "BLOBS" },
  "offscreen.hasDocument": async () => true,
  "storage.local.get": async () => ({}),
  "storage.local.set": async () => {},
  "alarms.create": async () => {},
  "action.setBadgeText": async () => {},
  "action.setBadgeBackgroundColor": async () => {},
  "action.setTitle": async () => {},
  "tabs.get": async (id) => {
    const t = model.tabs.get(typeof id === "object" ? id.tabId : id);
    if (!t) { const e = new Error("No tab with id"); throw e; }
    return { ...t };
  },
  "tabs.query": async (q = {}) => {
    let out = [...model.tabs.values()];
    if (q.active === true) out = out.filter((t) => t.active);
    if (q.lastFocusedWindow === true) {
      const lf = [...model.windows.values()].find((w) => w.focused) || [...model.windows.values()][0];
      out = lf ? out.filter((t) => t.windowId === lf.id) : [];
    }
    if (typeof q.windowId === "number") out = out.filter((t) => t.windowId === q.windowId);
    return out.map((t) => ({ ...t }));
  },
  "windows.get": async (id) => {
    const w = model.windows.get(id);
    if (!w) throw new Error("No window");
    return { ...w };
  },
  "windows.getAll": async () => [...model.windows.values()].map((w) => ({
    ...w, tabs: [...model.tabs.values()].filter((t) => t.windowId === w.id).map((t) => ({ ...t })),
  })),
  "tabGroups.query": async () => [],
  "tabGroups.get": async () => null,
};

function automock(path) {
  const target = function () {};
  return new Proxy(target, {
    get(_t, prop) {
      if (typeof prop === "symbol") return undefined;
      const full = path ? path + "." + prop : prop;
      if (Object.prototype.hasOwnProperty.call(overrides, full)) return overrides[full];
      if (prop === "then") return undefined; // never look thenable
      if (prop.startsWith && prop.startsWith("on") && prop[2] >= "A" && prop[2] <= "Z") {
        return model.events[full] || makeEvent(full);
      }
      return automock(full);
    },
    apply() { return Promise.resolve(undefined); },
  });
}

class MockWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  constructor() {
    this.readyState = MockWebSocket.CONNECTING;
    this.closeCalls = 0;
  }
  send() {}
  close() {
    this.closeCalls += 1;
    this.readyState = MockWebSocket.CLOSED;
    if (typeof this.onclose === "function") this.onclose({ code: 1000 });
  }
  addEventListener() {}
}

const sandbox = {
  chrome: automock(""),
  WebSocket: MockWebSocket,
  fetch: async () => ({ ok: true, json: async () => ({}), text: async () => "" }),
  setInterval: () => 0, clearInterval: () => {}, setTimeout: () => 0, clearTimeout: () => {},
  AbortSignal, URL, console,
};
sandbox.globalThis = sandbox;
sandbox.self = sandbox;

// Expose the module-scope symbols we want to drive/inspect.
src += `
;globalThis.__test = {
  get state() { return state; },
  resolveForegroundTabId,
  publishActiveTab,
  isControllableWindowType,
  preferredNormalWindowId,
  groupTabForParams,
  listTabSummaries,
  probeDaemonStatus,
  ensureTabDrivable
};`;

vm.createContext(sandbox);
vm.runInContext(src, sandbox, { filename: "service_worker.js" });
const T = sandbox.__test;

// ---- assertions ----
let failures = 0;
function check(name, cond) {
  if (cond) { console.log("  PASS", name); } else { console.log("  FAIL", name); failures++; }
}

async function reset() {
  model.tabs.clear(); model.windows.clear();
  T.state.activeTabId = null; T.state.agentTabId = null;
  T.state.statusProbeFailures = 0;
  T.state.statusProbeInFlight = null;
}

async function scenarioPinBeatsForeground() {
  await reset();
  // Agent's pinned tab (5) lives in window 1; the user has FOCUSED window 2 and is
  // on their own tab (9). The agent must still resolve to its pinned tab 5.
  setWin({ id: 1, type: "normal", focused: false });
  setWin({ id: 2, type: "normal", focused: true });
  setTab({ id: 5, windowId: 1, active: true, url: "https://app.test/agent", title: "agent" });
  setTab({ id: 9, windowId: 2, active: true, url: "https://news.test/", title: "user reading" });
  T.state.agentTabId = 5;
  const got = await T.resolveForegroundTabId();
  check("pinned agent tab beats OS-focused user window", got === 5);
}

async function scenarioUserClicksChatPWA() {
  await reset();
  setWin({ id: 1, type: "normal", focused: true });
  setWin({ id: 3, type: "app", focused: false }); // Google Chat PWA app window
  setTab({ id: 5, windowId: 1, active: true, url: "https://app.test/agent", title: "agent" });
  setTab({ id: 7, windowId: 3, active: true, url: "https://mail.google.com/chat/u/0/", title: "Google Chat" });
  T.state.agentTabId = 5;
  // User clicks into the Chat PWA → onActivated fires for tab 7.
  const onActivated = model.events["tabs.onActivated"];
  await onActivated._l[0]({ tabId: 7, windowId: 3 });
  check("onActivated for Chat PWA does NOT poison agentTabId", T.state.agentTabId === 5);
  check("onActivated for Chat PWA does NOT cache it as active", T.state.activeTabId !== 7);
  const got = await T.resolveForegroundTabId();
  check("resolution still returns the agent tab, never Chat", got === 5);
}

async function scenarioPoisonedCacheNoPin() {
  await reset();
  // No pin yet. The cache somehow points at a Chat PWA tab, and no normal window is
  // OS-focused. The step-3 guard must refuse to return the app tab.
  setWin({ id: 3, type: "app", focused: false });
  setTab({ id: 7, windowId: 3, active: true, url: "https://mail.google.com/chat/u/0/", title: "Google Chat" });
  T.state.activeTabId = 7; // poisoned
  const got = await T.resolveForegroundTabId();
  check("poisoned cache pointing at Chat PWA is NOT returned", got !== 7);
  check("poisoned uncontrollable cache is cleared", T.state.activeTabId === null);
}

async function scenarioUserSwitchesNormalTab() {
  await reset();
  // Agent pinned tab 5; user switches to their OWN normal tab 9 in same window.
  setWin({ id: 1, type: "normal", focused: true });
  setTab({ id: 5, windowId: 1, active: false, url: "https://app.test/agent", title: "agent" });
  setTab({ id: 9, windowId: 1, active: true, url: "https://news.test/", title: "user" });
  T.state.agentTabId = 5;
  const onActivated = model.events["tabs.onActivated"];
  await onActivated._l[0]({ tabId: 9, windowId: 1 });
  check("user switching to own normal tab updates activeTabId hint", T.state.activeTabId === 9);
  check("...but agent pin is untouched", T.state.agentTabId === 5);
  const got = await T.resolveForegroundTabId();
  check("agent stays on its pinned tab despite user's tab switch", got === 5);
}

async function scenarioBootstrapFallback() {
  await reset();
  // No pin (agent hasn't opened/focused yet): resolve to the OS-foreground tab.
  setWin({ id: 1, type: "normal", focused: true });
  setTab({ id: 2, windowId: 1, active: true, url: "https://start.test/", title: "start" });
  const got = await T.resolveForegroundTabId();
  check("with no pin, falls back to OS-foreground tab", got === 2);
}

async function scenarioPopupFocusStillCreatesInNormalWindow() {
  await reset();
  setWin({ id: 1, type: "normal", focused: false });
  setWin({ id: 8, type: "popup", focused: true });
  setTab({ id: 18, windowId: 8, active: true, url: "https://popup.test/", title: "popup" });
  T.state.agentTabId = 18;
  const got = await T.preferredNormalWindowId();
  check("popup-focused state still chooses a normal window for new tabs", got === 1);

  model.windows.delete(1);
  const none = await T.preferredNormalWindowId();
  check("no normal window cleanly falls back to Chrome default creation", none === null);
}

async function scenarioNewGroupsTargetTheTabsRealWindow() {
  await reset();
  setWin({ id: 41, type: "normal", focused: false });
  setWin({ id: 99, type: "normal", focused: true });
  const tab = { id: 7, windowId: 41, active: false, url: "https://agent.test/", title: "agent" };
  setTab(tab);

  const originalGroup = overrides["tabs.group"];
  const originalQuery = overrides["tabGroups.query"];
  const originalUpdate = overrides["tabGroups.update"];
  let newGroupArgs = null;
  overrides["tabGroups.query"] = async () => [];
  overrides["tabs.group"] = async (args) => { newGroupArgs = args; return 71; };
  overrides["tabGroups.update"] = async () => ({});

  const newID = await T.groupTabForParams(tab, { groupName: "agent-brw-vertical", groupColor: "cyan" });
  check("new group is created successfully", newID === 71);
  check("new group explicitly targets the tab's real window",
    newGroupArgs?.createProperties?.windowId === 41 && newGroupArgs?.groupId === undefined);

  let existingGroupArgs = null;
  overrides["tabGroups.query"] = async () => [{ id: 72, title: "agent-brw-vertical", windowId: 41 }];
  overrides["tabs.group"] = async (args) => { existingGroupArgs = args; return 72; };
  const existingID = await T.groupTabForParams(tab, { groupName: "agent-brw-vertical" });
  check("existing group is reused", existingID === 72 && existingGroupArgs?.groupId === 72);
  check("existing group never mixes groupId with createProperties",
    existingGroupArgs?.createProperties === undefined);

  if (originalGroup === undefined) delete overrides["tabs.group"];
  else overrides["tabs.group"] = originalGroup;
  overrides["tabGroups.query"] = originalQuery;
  if (originalUpdate === undefined) delete overrides["tabGroups.update"];
  else overrides["tabGroups.update"] = originalUpdate;
}

async function scenarioListTabsErrorAndRetry() {
  await reset();
  // The harness's default setTimeout never fires its callback, which would hang
  // listTabSummaries' warm-up retry; fire immediately for this scenario only.
  const origTimeout = sandbox.setTimeout;
  const origQuery = overrides["tabs.query"];
  sandbox.setTimeout = (fn) => { if (typeof fn === "function") queueMicrotask(fn); return 0; };

  // A rejected tabs.query must surface as an ERROR to the daemon (handle()
  // replies ok:false), never as an empty-but-ok tab list the agent trusts.
  overrides["tabs.query"] = async () => { throw new Error("tabs API unavailable"); };
  let threw = false;
  try { await T.listTabSummaries(); } catch { threw = true; }
  check("rejected tabs.query propagates as error, not silent empty list", threw);

  // A just-woken service worker can answer [] before tab state is warm: one
  // retry must see the real tabs.
  setWin({ id: 1, type: "normal", focused: true });
  setTab({ id: 11, windowId: 1, active: true, url: "https://a.test/", title: "A" });
  let calls = 0;
  overrides["tabs.query"] = async (q = {}) => {
    calls += 1;
    if (calls === 1) return [];
    return origQuery(q);
  };
  const tabs = await T.listTabSummaries();
  check("empty first answer is retried once", calls === 2);
  check("retry returns the real tabs", tabs.length === 1 && tabs[0].id === 11);

  overrides["tabs.query"] = origQuery;
  sandbox.setTimeout = origTimeout;
}

async function scenarioFrozenDiscardedRevival() {
  await reset();
  // ensureTabDrivable sleeps 150ms mid-juggle and waitForTabLoad polls with
  // setTimeout; fire timers immediately for this scenario only.
  const origTimeout = sandbox.setTimeout;
  sandbox.setTimeout = (fn) => { if (typeof fn === "function") queueMicrotask(fn); return 0; };
  const origUpdate = overrides["tabs.update"];
  const origReload = overrides["tabs.reload"];
  const origGroupsUpdate = overrides["tabGroups.update"];

  setWin({ id: 1, type: "normal", focused: true });
  setTab({ id: 5, windowId: 1, active: false, url: "https://agent.test/", title: "agent" });
  setTab({ id: 6, windowId: 1, active: true, url: "https://user.test/", title: "user" });

  const updates = [];
  let reloads = 0;
  overrides["tabs.update"] = async (id, props) => {
    updates.push({ id, ...props });
    const t = model.tabs.get(id);
    if (t && props.active === true) {
      for (const other of model.tabs.values()) {
        if (other.windowId === t.windowId) other.active = other.id === id;
      }
      if (t.frozen) t.frozen = false; // visibility unfreezes
    }
    return { ...model.tabs.get(id) };
  };
  overrides["tabs.reload"] = async (id) => {
    reloads += 1;
    const t = model.tabs.get(id);
    if (t) { t.discarded = false; t.status = "complete"; }
  };
  overrides["tabGroups.update"] = async (groupId, props) => ({ id: groupId, ...props });

  // 1. Healthy tab: driving must add zero side effects.
  await T.ensureTabDrivable(5);
  check("healthy tab needs no revival side effects", reloads === 0 && updates.length === 0);

  // 2. Discarded (Memory Saver) tab: reload revives, autoDiscardable pinned off,
  //    and the user's foreground tab is never touched.
  Object.assign(model.tabs.get(5), { discarded: true, status: "unloaded" });
  await T.ensureTabDrivable(5);
  check("discarded tab is reloaded", reloads === 1 && model.tabs.get(5).discarded === false);
  check("revived tab is pinned autoDiscardable:false", updates.some((u) => u.id === 5 && u.autoDiscardable === false));
  check("discard revival never activates the tab", !updates.some((u) => u.id === 5 && u.active === true));

  // 3. Frozen background tab in a collapsed group: expand group, flash active
  //    inside its window, restore the user's tab.
  updates.length = 0;
  const groupCalls = [];
  overrides["tabGroups.update"] = async (groupId, props) => { groupCalls.push({ groupId, ...props }); return { id: groupId }; };
  Object.assign(model.tabs.get(5), { frozen: true, groupId: 71 });
  await T.ensureTabDrivable(5);
  check("frozen tab's collapsed group is expanded", groupCalls.some((c) => c.groupId === 71 && c.collapsed === false));
  check("frozen tab is flashed active to unfreeze", updates.some((u) => u.id === 5 && u.active === true));
  const last = updates[updates.length - 1];
  check("the user's previously active tab is restored", last?.id === 6 && last?.active === true && model.tabs.get(6).active === true);
  check("frozen flag is cleared by the revival", model.tabs.get(5).frozen === false);

  // 4. Unrevivable frozen tab (activation does not stick): one classified error
  //    instead of a silent daemon-deadline hang.
  updates.length = 0;
  overrides["tabs.update"] = async (id, props) => { updates.push({ id, ...props }); return { ...model.tabs.get(id) }; };
  Object.assign(model.tabs.get(5), { frozen: true, active: false });
  let message = "";
  try { await T.ensureTabDrivable(5); } catch (e) { message = String(e?.message || e); }
  check("unrevivable frozen tab throws the classified error", message.includes("frozen by Chrome"));

  if (origUpdate === undefined) delete overrides["tabs.update"]; else overrides["tabs.update"] = origUpdate;
  if (origReload === undefined) delete overrides["tabs.reload"]; else overrides["tabs.reload"] = origReload;
  if (origGroupsUpdate === undefined) delete overrides["tabGroups.update"]; else overrides["tabGroups.update"] = origGroupsUpdate;
  sandbox.setTimeout = origTimeout;
}

async function scenarioStatusProbeToleratesTransientFailure() {
  await reset();
  const originalFetch = sandbox.fetch;
  const socket = new MockWebSocket();
  socket.readyState = MockWebSocket.OPEN;
  T.state.socket = socket;

  sandbox.fetch = async () => { throw new Error("temporary loopback failure"); };
  await T.probeDaemonStatus();
  await T.probeDaemonStatus();
  check("two transient status failures keep the healthy WebSocket", T.state.socket === socket && socket.closeCalls === 0);
  check("transient status failures are counted", T.state.statusProbeFailures === 2);

  sandbox.fetch = async () => ({
    ok: true,
    json: async () => ({ connected: true, identity: {} })
  });
  await T.probeDaemonStatus();
  check("a successful status probe resets the failure streak", T.state.statusProbeFailures === 0 && T.state.socket === socket);

  sandbox.fetch = async () => { throw new Error("persistent loopback failure"); };
  await T.probeDaemonStatus();
  await T.probeDaemonStatus();
  check("persistent failure still waits for the third strike", T.state.socket === socket && socket.closeCalls === 0);
  await T.probeDaemonStatus();
  check("third consecutive failure closes and schedules recovery", T.state.socket === null && socket.closeCalls === 1);

  sandbox.fetch = originalFetch;
}

(async () => {
  await scenarioPinBeatsForeground();
  await scenarioUserClicksChatPWA();
  await scenarioPoisonedCacheNoPin();
  await scenarioUserSwitchesNormalTab();
  await scenarioBootstrapFallback();
  await scenarioPopupFocusStillCreatesInNormalWindow();
  await scenarioNewGroupsTargetTheTabsRealWindow();
  await scenarioListTabsErrorAndRetry();
  await scenarioFrozenDiscardedRevival();
  await scenarioStatusProbeToleratesTransientFailure();
  console.log(failures === 0 ? "\nALL PASS" : `\n${failures} FAILURES`);
  process.exit(failures === 0 ? 0 : 1);
})();
