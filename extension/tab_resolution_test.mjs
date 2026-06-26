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

const sandbox = {
  chrome: automock(""),
  WebSocket: class { constructor() {} send() {} close() {} addEventListener() {} },
  fetch: async () => ({ ok: true, json: async () => ({}), text: async () => "" }),
  setInterval: () => 0, clearInterval: () => {}, setTimeout: () => 0, clearTimeout: () => {},
  URL, console,
};
sandbox.globalThis = sandbox;
sandbox.self = sandbox;

// Expose the module-scope symbols we want to drive/inspect.
src += `
;globalThis.__test = {
  get state() { return state; },
  resolveForegroundTabId,
  publishActiveTab,
  isControllableWindowType
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

(async () => {
  await scenarioPinBeatsForeground();
  await scenarioUserClicksChatPWA();
  await scenarioPoisonedCacheNoPin();
  await scenarioUserSwitchesNormalTab();
  await scenarioBootstrapFallback();
  console.log(failures === 0 ? "\nALL PASS" : `\n${failures} FAILURES`);
  process.exit(failures === 0 ? 0 : 1);
})();
