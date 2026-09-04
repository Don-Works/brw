// Regression harness: runs the ACTUAL service_worker.js in a vm sandbox with a
// mocked chrome API, then asserts the agent-tab-pin + PWA/app-window guard
// behaviour that keeps brw from drifting onto the user's tabs (e.g. the Google
// Chat PWA) on a Chrome the human drives at the same time. Run: `make test-extension`
// or `node extension/tab_resolution_test.mjs`.
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { Buffer } from "node:buffer";
import { TextEncoder } from "node:util";
import vm from "node:vm";

const OVERSIZE_ENCODER_MARKER = "__BRW_TEST_OVERSIZE_RESPONSE__";
class HarnessTextEncoder {
  encode(value) {
    // Exercise the hard-cap branch without asking CI to allocate hundreds of
    // megabytes. All ordinary fixtures still use the platform implementation.
    if (String(value).includes(OVERSIZE_ENCODER_MARKER)) return { length: Number.MAX_SAFE_INTEGER };
    return new TextEncoder().encode(value);
  }
}

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
  "runtime.id": "amocjcgddnoakjijfggdpnefdnboilpe",
  "runtime.getPlatformInfo": async () => ({ os: "mac" }),
  "windows.WINDOW_ID_NONE": -1,
  "offscreen.Reason": { BLOBS: "BLOBS" },
  "offscreen.hasDocument": async () => true,
  "storage.local.get": async () => ({}),
  "storage.local.set": async () => {},
  "alarms.create": async () => {},
  "action.setBadgeText": async () => {},
  "action.setBadgeBackgroundColor": async () => {},
  "action.setBadgeTextColor": async () => {},
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
    this.sent = [];
    this.sentRaw = [];
  }
  send(payload) {
    this.sentRaw.push(payload);
    this.sent.push(JSON.parse(payload));
  }
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
  AbortSignal, URL, TextEncoder: HarnessTextEncoder,
  btoa: (value) => Buffer.from(value, "latin1").toString("base64"),
  console,
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
  isAgentDrivableUrl,
  preferredNormalWindowId,
  groupTabForParams,
  listTabSummaries,
  probeDaemonStatus,
  ensureTabDrivable,
  waitForTabGone,
  agentOwnedTabIdForHello,
  handle,
  send,
  RESPONSE_DIRECT_MAX_BYTES,
  RESPONSE_CHUNK_BYTES,
  RESPONSE_TOTAL_MAX_BYTES
};`;

vm.createContext(sandbox);
vm.runInContext(src, sandbox, { filename: "service_worker.js" });
const T = sandbox.__test;

// ---- assertions ----
let failures = 0;
function check(name, cond) {
  if (cond) { console.log("  PASS", name); } else { console.log("  FAIL", name); failures++; }
}

function fireEvent(path, ...args) {
  for (const listener of model.events[path]?._l || []) listener(...args);
}

async function reset() {
  model.tabs.clear(); model.windows.clear();
  T.state.activeTabId = null; T.state.agentTabId = null;
  T.state.attachedTabs.clear(); T.state.attachUsedAt.clear(); T.state.actingUntil.clear();
  T.state.downloads.clear(); T.state.downloadCorrelation.clear(); T.state.downloadProvenance.length = 0;
  T.state.documentEpochs.clear();
  T.state.socket = null;
  T.state.statusProbeFailures = 0;
  T.state.statusProbeInFlight = null;
}

async function scenarioMainDocumentIdentityIsExactAndMonotonic() {
  const saved = overrides["webNavigation.getFrame"];
  try {
    await reset();
    setWin({ id: 1, type: "normal", focused: true });
    setTab({ id: 41, windowId: 1, active: true, url: "https://one.example.test/a", title: "A" });
    const socket = new MockWebSocket(); socket.readyState = MockWebSocket.OPEN; T.state.socket = socket;
    let frame = { documentId: "doc-A", url: "https://one.example.test/a" };
    let requestedFrame = null;
    overrides["webNavigation.getFrame"] = async (details) => {
      requestedFrame = details;
      return { ...frame };
    };

    await T.handle({ id: "doc-a", type: "get_document_identity", params: { tabId: 41 } });
    const first = socket.sent.at(-1)?.result;
    check("document identity queries only the main frame",
      requestedFrame?.tabId === 41 && requestedFrame?.frameId === 0);
    check("document identity exposes opaque identity and origin but not URL",
      first?.document_id === "doc-A" && first?.origin === "https://one.example.test" &&
      first?.document_epoch === 0 && first?.worker_instance && !("url" in first));

    frame = { documentId: "doc-B", url: "https://two.example.test/b" };
    fireEvent("webNavigation.onCommitted", { tabId: 41, frameId: 0, documentId: "doc-B", url: frame.url });
    await T.handle({ id: "doc-b", type: "get_document_identity", params: { tabId: 41 } });
    const second = socket.sent.at(-1)?.result;
    check("allowed-origin replacement increments document epoch",
      second?.document_id === "doc-B" && second?.document_epoch === 1 && second?.worker_instance === first?.worker_instance);

    fireEvent("webNavigation.onHistoryStateUpdated", { tabId: 41, frameId: 0, documentId: "doc-B", url: "https://two.example.test/spa" });
    await T.handle({ id: "doc-spa", type: "get_document_identity", params: { tabId: 41 } });
    check("same-document SPA route keeps document identity epoch",
      socket.sent.at(-1)?.result?.document_epoch === 1 && socket.sent.at(-1)?.result?.document_id === "doc-B");

    frame = { documentId: "doc-A", url: "https://one.example.test/a" };
    fireEvent("webNavigation.onCommitted", { tabId: 41, frameId: 0, documentId: "doc-A", url: frame.url });
    await T.handle({ id: "doc-return", type: "get_document_identity", params: { tabId: 41 } });
    check("A-B-BFCache-A cannot reuse its starting continuity token",
      socket.sent.at(-1)?.result?.document_id === "doc-A" && socket.sent.at(-1)?.result?.document_epoch === 2);

    fireEvent("webNavigation.onCommitted", { tabId: 41, frameId: 7, documentId: "iframe", url: "https://frame.example.test/" });
    await T.handle({ id: "doc-subframe", type: "get_document_identity", params: { tabId: 41 } });
    check("subframe navigation does not break main-document continuity",
      socket.sent.at(-1)?.result?.document_epoch === 2);

		frame = { documentId: "doc-data", url: "data:text/html,opaque" };
		fireEvent("webNavigation.onCommitted", { tabId: 41, frameId: 0, documentId: "doc-data", url: frame.url });
		await T.handle({ id: "doc-opaque", type: "get_document_identity", params: { tabId: 41 } });
		check("opaque-origin replacement still exposes a trusted navigation identity",
			socket.sent.at(-1)?.result?.document_id === "doc-data" && socket.sent.at(-1)?.result?.origin === "null");

    overrides["webNavigation.getFrame"] = async () => ({ documentId: "", url: "https://private.example.test/path" });
    await T.handle({ id: "doc-invalid", type: "get_document_identity", params: { tabId: 41 } });
    const invalid = socket.sent.at(-1);
    check("unavailable identity fails closed without leaking URL",
      invalid?.ok === false && String(invalid?.error || "").includes("identity is unavailable") &&
      !String(invalid?.error || "").includes("private.example.test"));
  } finally {
    if (saved === undefined) delete overrides["webNavigation.getFrame"];
    else overrides["webNavigation.getFrame"] = saved;
  }
}

async function scenarioDownloadSnapshotsAndFailClosedProvenance() {
  await reset();
  const socket = new MockWebSocket();
  socket.readyState = MockWebSocket.OPEN;
  T.state.socket = socket;

  // CDP names the initiating tab; chrome.downloads supplies the durable numeric
  // id/path/lifecycle. Exercise both event streams in their normal order.
  fireEvent("debugger.onEvent", { tabId: 11 }, "Page.downloadWillBegin", {
    guid: "cdp-guid-1", url: "https://files.test/report.pdf", suggestedFilename: "report.pdf"
  });
  fireEvent("downloads.onCreated", {
    id: 42, url: "https://files.test/report.pdf", filename: "/var/tmp/brw-test/report.pdf",
    state: "in_progress", bytesReceived: 10, totalBytes: 100
  });
  await T.handle({ id: "downloads-1", type: "get_downloads" });
  const first = socket.sent.at(-1)?.result;
  check("extension download snapshot carries exact source tab",
    first?.count === 1 && first.downloads[0]?.guid === "42" && first.downloads[0]?.tab_id === "11");

  fireEvent("downloads.onChanged", { id: 42, state: { current: "complete" } });
  await T.handle({ id: "downloads-2", type: "get_downloads" });
  const completed = socket.sent.at(-1)?.result;
  await T.handle({ id: "downloads-3", type: "get_downloads" });
  const repeated = socket.sent.at(-1)?.result;
  check("download completion retains begin-event identity",
    completed?.downloads[0]?.state === "completed" && completed.downloads[0]?.suggested_filename === "report.pdf" && completed.downloads[0]?.tab_id === "11");
  check("get_downloads is retained and non-draining",
    repeated?.count === 1 && repeated.downloads[0]?.guid === "42" && repeated.downloads[0]?.state === "completed");

  // Two indistinguishable starts in different tabs cannot be correlated safely.
  // The correct behavior is unknown provenance, never FIFO guessing.
  await reset();
  const ambiguousSocket = new MockWebSocket();
  ambiguousSocket.readyState = MockWebSocket.OPEN;
  T.state.socket = ambiguousSocket;
  for (const tabId of [11, 22]) {
    fireEvent("debugger.onEvent", { tabId }, "Page.downloadWillBegin", {
      guid: `cdp-${tabId}`, url: "https://files.test/invoice.pdf", suggestedFilename: "invoice.pdf"
    });
  }
  fireEvent("downloads.onCreated", { id: 51, url: "https://files.test/invoice.pdf", filename: "/tmp/a/invoice.pdf", state: "complete" });
  fireEvent("downloads.onCreated", { id: 52, url: "https://files.test/invoice.pdf", filename: "/tmp/b/invoice.pdf", state: "complete" });
  await T.handle({ id: "downloads-ambiguous", type: "get_downloads" });
  const ambiguous = ambiguousSocket.sent.at(-1)?.result?.downloads || [];
  check("simultaneous same-name downloads across tabs fail closed",
    ambiguous.length === 2 && ambiguous.every((entry) => !entry.tab_id));

  // A matching human-tab download may have no CDP observation because brw was
  // never attached to that tab. One provenance event therefore cannot be
  // assigned to either of two identical chrome.downloads items.
  await reset();
  const unmatchedSocket = new MockWebSocket();
  unmatchedSocket.readyState = MockWebSocket.OPEN;
  T.state.socket = unmatchedSocket;
  fireEvent("debugger.onEvent", { tabId: 11 }, "Page.downloadWillBegin", {
    guid: "cdp-only-one", url: "https://files.test/shared.pdf", suggestedFilename: "shared.pdf"
  });
  fireEvent("downloads.onCreated", { id: 55, url: "https://files.test/shared.pdf", filename: "/tmp/a/shared.pdf", state: "complete" });
  fireEvent("downloads.onCreated", { id: 56, url: "https://files.test/shared.pdf", filename: "/tmp/b/shared.pdf", state: "complete" });
  await T.handle({ id: "downloads-unmatched-human", type: "get_downloads" });
  const unmatched = unmatchedSocket.sent.at(-1)?.result?.downloads || [];
  check("one CDP event cannot claim either of two identical downloads",
    unmatched.length === 2 && unmatched.every((entry) => !entry.tab_id));

  // Event ordering is not guaranteed across Chrome APIs; a unique reverse-order
  // pair must still correlate at snapshot time.
  await reset();
  const reverseSocket = new MockWebSocket();
  reverseSocket.readyState = MockWebSocket.OPEN;
  T.state.socket = reverseSocket;
  fireEvent("downloads.onCreated", { id: 61, url: "https://files.test/reverse.csv", filename: "/tmp/reverse.csv", state: "complete" });
  fireEvent("debugger.onEvent", { tabId: 33 }, "Page.downloadWillBegin", {
    guid: "cdp-reverse", url: "https://files.test/reverse.csv", suggestedFilename: "reverse.csv"
  });
  await T.handle({ id: "downloads-reverse", type: "get_downloads" });
  check("reverse event ordering still correlates unique source tab",
    reverseSocket.sent.at(-1)?.result?.downloads[0]?.tab_id === "33");

  // The registry is independently bounded even if Chrome produces a burst.
  await reset();
  const boundedSocket = new MockWebSocket();
  boundedSocket.readyState = MockWebSocket.OPEN;
  T.state.socket = boundedSocket;
  for (let id = 0; id < 205; id++) {
    fireEvent("downloads.onCreated", {
      id, tabId: 77, url: `https://files.test/${id}`, filename: `/tmp/${id}.bin`, state: "complete"
    });
  }
  await T.handle({ id: "downloads-bounded-1", type: "get_downloads" });
  const boundedFirst = boundedSocket.sent.at(-1)?.result;
  await T.handle({ id: "downloads-bounded-2", type: "get_downloads" });
  const boundedSecond = boundedSocket.sent.at(-1)?.result;
  check("extension download registry stays bounded at 200", boundedFirst?.count === 200 && boundedFirst.downloads[0]?.guid === "5");
  check("bounded registry remains retained across reads", boundedSecond?.count === 200 && boundedSecond.downloads[0]?.guid === "5");
}

async function scenarioWaitForTabGoneIsBoundedAndHonest() {
  await reset();
  check("already-removed tab is immediately confirmed gone", await T.waitForTabGone(404, 2000) === true);
  setWin({ id: 1, type: "normal", focused: true });
  setTab({ id: 5, windowId: 1, active: true, url: "https://app.test/", title: "app" });
  check("live tab at a zero deadline is not falsely reported gone", await T.waitForTabGone(5, 0) === false);
}

async function scenarioTabRemovalPublishesDaemonInvalidation() {
  await reset();
  const socket = new MockWebSocket();
  socket.readyState = MockWebSocket.OPEN;
  T.state.socket = socket;
  T.state.activeTabId = 88;
  T.state.agentTabId = 88;
  T.state.snapshotCache.set(88, { stale: true });
  T.state.observerInjected.add(88);
  T.state.fileChooserEvents.set(88, { backendNodeId: 1 });
  T.state.actingUntil.set(88, Date.now() + 1000);
  T.state.consoleMessages.set(88, [{ level: "log", text: "stale" }]);

  fireEvent("tabs.onRemoved", 88, { windowId: 1, isWindowClosing: false });
  check("tab removal clears extension-local reusable-id state",
    T.state.activeTabId === null && T.state.agentTabId === null &&
    !T.state.snapshotCache.has(88) && !T.state.observerInjected.has(88) &&
    !T.state.fileChooserEvents.has(88) && !T.state.actingUntil.has(88) &&
    !T.state.consoleMessages.has(88));
  check("tab removal immediately tells the daemon to invalidate its caches",
    socket.sent.length === 1 && socket.sent[0]?.type === "tab_removed" && socket.sent[0]?.tabId === 88);

  // A disconnect can make the best-effort control frame unsendable. The next
  // hello must still report the cleared AGENT pin rather than substituting the
  // user's foreground hint, which is deliberately unrelated ownership state.
  T.state.socket = null;
  T.state.activeTabId = 91;
  T.state.agentTabId = 89;
  fireEvent("tabs.onRemoved", 89, { windowId: 1, isWindowClosing: false });
  check("reconnect hello reports a lost pin even when tab_removed could not send",
    T.agentOwnedTabIdForHello() === 0 && T.state.activeTabId === 91);
  T.state.agentTabId = 92;
  check("reconnect hello reports only the agent-owned pin, never foreground",
    T.agentOwnedTabIdForHello() === 92 && T.state.activeTabId === 91);
}

async function scenarioLargeResponsesUseBoundedFrames() {
  await reset();
  const socket = new MockWebSocket();
  socket.readyState = MockWebSocket.OPEN;
  T.state.socket = socket;

  const small = { id: "small-response", ok: true, result: { value: "ok" } };
  check("a small response sends successfully", T.send(small) === true);
  check("a small response remains one ordinary frame",
    socket.sent.length === 1 && socket.sent[0].type !== "response_chunk" && socket.sent[0].id === small.id);
  check("a direct response stays below the advertised safe threshold",
    Buffer.byteLength(socket.sentRaw[0], "utf8") <= T.RESPONSE_DIRECT_MAX_BYTES);

  socket.sent.length = 0;
  socket.sentRaw.length = 0;
  // Multibyte text catches implementations that confuse JS UTF-16 length with
  // UTF-8 wire bytes. The logical JSON body is intentionally larger than the
  // daemon's unchanged 4 MiB per-WebSocket-message limit.
  const value = "€".repeat(1_500_000);
  const large = { id: "large-response", ok: true, result: { value } };
  const serialized = JSON.stringify(large);
  const serializedBytes = Buffer.byteLength(serialized, "utf8");
  check("large-response fixture exceeds 4 MiB", serializedBytes > 4 * 1024 * 1024);
  check("a large response sends successfully", T.send(large) === true);
  check("a large response is split across multiple frames", socket.sent.length > 1);
  check("every chunk envelope stays below the 4 MiB frame read limit",
    socket.sentRaw.every((frame) => Buffer.byteLength(frame, "utf8") < 4 * 1024 * 1024));

  const chunks = socket.sent;
  const declaredCount = chunks[0]?.chunk_count;
  check("all frames are sequential chunk envelopes",
    chunks.every((chunk, index) => chunk.type === "response_chunk" &&
      chunk.id === large.id && chunk.encoding === "base64" &&
      chunk.chunk_index === index && chunk.chunk_count === declaredCount));
  check("chunk metadata declares the exact UTF-8 byte total",
    chunks.every((chunk) => chunk.total_bytes === serializedBytes) && declaredCount === chunks.length);
  const rebuiltBytes = Buffer.concat(chunks.map((chunk) => Buffer.from(chunk.data, "base64")));
  const rebuilt = JSON.parse(rebuiltBytes.toString("utf8"));
  check("chunk bytes reconstruct one exact logical response",
    rebuiltBytes.length === serializedBytes && rebuilt.id === large.id && rebuilt.ok === true && rebuilt.result.value === value);

  socket.sent.length = 0;
  socket.sentRaw.length = 0;
  const overLimit = {
    id: "over-limit-response",
    ok: true,
    // Cross the fast-path character threshold so HarnessTextEncoder can model
    // an over-cap UTF-8 body without a real 64+ MiB test allocation.
    result: { value: OVERSIZE_ENCODER_MARKER + "x".repeat(Math.floor(T.RESPONSE_DIRECT_MAX_BYTES / 3) + 1) }
  };
  check("an over-cap response returns a deterministic transport error", T.send(overLimit) === true &&
    socket.sent.length === 1 && socket.sent[0].id === overLimit.id && socket.sent[0].ok === false &&
    socket.sent[0].error.startsWith("BRW_EXTENSION_RESPONSE_TOO_LARGE:"));
  check("the over-cap error itself stays below one frame",
    Buffer.byteLength(socket.sentRaw[0], "utf8") < 4 * 1024 * 1024);
  check("the socket remains usable after rejecting an over-cap response",
    T.send({ id: "after-over-limit", ok: true, result: { value: "ok" } }) === true &&
    socket.sent.at(-1)?.id === "after-over-limit" && socket.sent.at(-1)?.ok === true);
}

async function scenarioCloseTabIsBoundedAndFailClosed() {
  const keys = ["debugger.attach", "debugger.detach", "debugger.sendCommand", "tabs.remove", "tabs.update"];
  const saved = new Map(keys.map((key) => [key, overrides[key]]));
  try {
    let commands = [];
    let removeCalls = 0;
    let updateCalls = 0;
    let attachCalls = 0;
    overrides["debugger.attach"] = async () => { attachCalls++; };
    overrides["debugger.detach"] = async () => {};
    overrides["tabs.remove"] = async (id) => { removeCalls++; model.tabs.delete(id); };
    overrides["tabs.update"] = async (id, patch) => {
      updateCalls++;
      const tab = model.tabs.get(id);
      if (tab) Object.assign(tab, patch);
      return tab ? { ...tab } : null;
    };
    overrides["debugger.sendCommand"] = async ({ tabId }, method) => {
      commands.push(method);
      if (method === "Page.close") model.tabs.delete(tabId);
      return {};
    };

    await reset();
    setWin({ id: 1, type: "normal", focused: true });
    setTab({ id: 31, windowId: 1, active: true, url: "https://dirty.test/", title: "dirty" });
    const socket = new MockWebSocket(); socket.readyState = MockWebSocket.OPEN; T.state.socket = socket;
    await T.handle({ id: "close-normal", type: "close_tab", params: { tabId: 31 } });
    check("close_tab enables Page events before Page.close", commands.indexOf("Page.enable") >= 0 && commands.indexOf("Page.enable") < commands.indexOf("Page.close"));
    check("close_tab removes a normal page and reports success", !model.tabs.has(31) && socket.sent.at(-1)?.ok === true);

    commands = [];
    await reset();
    setWin({ id: 1, type: "normal", focused: true });
    setTab({ id: 32, windowId: 1, active: true, url: "https://dirty.test/", title: "dirty" });
    const detachedSocket = new MockWebSocket(); detachedSocket.readyState = MockWebSocket.OPEN; T.state.socket = detachedSocket;
    overrides["debugger.sendCommand"] = async ({ tabId }, method) => {
      commands.push(method);
      if (method === "Page.close") {
        model.tabs.delete(tabId);
        throw new Error("Detached while handling command.");
      }
      return {};
    };
    await T.handle({ id: "close-detached", type: "close_tab", params: { tabId: 32 } });
    check("a detach race after target destruction is successful", detachedSocket.sent.at(-1)?.ok === true && !model.tabs.has(32));

    attachCalls = 0;
    await reset();
    setWin({ id: 1, type: "normal", focused: true });
    setTab({ id: 33, windowId: 1, active: false, discarded: true, url: "https://old.test/", title: "old" });
    const discardedSocket = new MockWebSocket(); discardedSocket.readyState = MockWebSocket.OPEN; T.state.socket = discardedSocket;
    await T.handle({ id: "close-discarded", type: "close_tab", params: { tabId: 33 } });
    check("discarded close does not revive or attach", attachCalls === 0 && !model.tabs.has(33));

    attachCalls = 0; updateCalls = 0;
    overrides["debugger.sendCommand"] = async ({ tabId }, method) => {
      if (method === "Page.close") model.tabs.delete(tabId);
      return {};
    };
    await reset();
    setWin({ id: 1, type: "normal", focused: true });
    setTab({ id: 34, windowId: 1, active: false, frozen: true, url: "https://frozen.test/", title: "frozen" });
    const frozenSocket = new MockWebSocket(); frozenSocket.readyState = MockWebSocket.OPEN; T.state.socket = frozenSocket;
    await T.handle({ id: "close-frozen", type: "close_tab", params: { tabId: 34 } });
    check("frozen close does not activate or reload", attachCalls === 1 && updateCalls === 0 && !model.tabs.has(34));

    removeCalls = 0;
    overrides["debugger.sendCommand"] = async (_target, method) => {
      if (method === "Page.enable") throw new Error("Page domain unavailable");
      return {};
    };
    await reset();
    setWin({ id: 1, type: "normal", focused: true });
    setTab({ id: 35, windowId: 1, active: true, url: "https://dirty.test/", title: "dirty" });
    const unsafeSocket = new MockWebSocket(); unsafeSocket.readyState = MockWebSocket.OPEN; T.state.socket = unsafeSocket;
    await T.handle({ id: "close-no-page", type: "close_tab", params: { tabId: 35 } });
    check("Page.enable failure leaves normal tab open", model.tabs.has(35) && removeCalls === 0 && unsafeSocket.sent.at(-1)?.ok === false);

    overrides["debugger.attach"] = async () => { throw new Error("Another debugger is already attached"); };
    overrides["debugger.sendCommand"] = async (_target, method) => {
      if (method === "Runtime.evaluate") throw new Error("debugger is not attached");
      return {};
    };
    await reset();
    setWin({ id: 1, type: "normal", focused: true });
    setTab({ id: 36, windowId: 1, active: true, url: "https://dirty.test/", title: "dirty" });
    const conflictSocket = new MockWebSocket(); conflictSocket.readyState = MockWebSocket.OPEN; T.state.socket = conflictSocket;
    await T.handle({ id: "close-conflict", type: "close_tab", params: { tabId: 36 } });
    check("debugger conflict fails closed without tabs.remove", model.tabs.has(36) && removeCalls === 0 && conflictSocket.sent.at(-1)?.ok === false);

		// Page.close can itself remain pending (the failure that originally wedged
		// close_tab). Use real timers only for this bounded scenario; the harness
		// otherwise stubs timers so service-worker background loops cannot leak.
		const savedSetTimeout = sandbox.setTimeout;
		const savedClearTimeout = sandbox.clearTimeout;
		let resolveLateClose;
		let detachCalls = 0;
		try {
			sandbox.setTimeout = globalThis.setTimeout.bind(globalThis);
			sandbox.clearTimeout = globalThis.clearTimeout.bind(globalThis);
			overrides["debugger.attach"] = async () => { attachCalls++; };
			overrides["debugger.detach"] = async () => { detachCalls++; };
			overrides["debugger.sendCommand"] = async (_target, method) => {
				if (method === "Page.close") {
					return new Promise((resolve) => { resolveLateClose = resolve; });
				}
				return {};
			};
			await reset();
			setWin({ id: 1, type: "normal", focused: true });
			setTab({ id: 37, windowId: 1, active: true, url: "https://wedged.test/", title: "wedged" });
			const wedgedSocket = new MockWebSocket(); wedgedSocket.readyState = MockWebSocket.OPEN; T.state.socket = wedgedSocket;
			const startedAt = Date.now();
			await T.handle({ id: "close-never-resolves", type: "close_tab", params: { tabId: 37 } });
			const elapsed = Date.now() - startedAt;
			check("a never-resolving Page.close is bounded by the close budget",
				elapsed >= 1500 && elapsed < 4000 && wedgedSocket.sent.length === 1 &&
				wedgedSocket.sent[0]?.ok === false && wedgedSocket.sent[0]?.error?.includes("Page.close timed out"));
			check("timed-out Page.close detaches and leaves the tab fail-closed", detachCalls >= 1 && model.tabs.has(37));

			await T.handle({ id: "after-close-timeout", type: "list_tabs" });
			check("the bridge remains usable after a Page.close timeout",
				wedgedSocket.sent.length === 2 && wedgedSocket.sent[1]?.ok === true &&
				wedgedSocket.sent[1]?.result?.some((tab) => tab.id === 37));
			resolveLateClose?.({});
			await new Promise((resolve) => globalThis.setTimeout(resolve, 10));
			check("late Page.close resolution cannot emit a second reply", wedgedSocket.sent.length === 2);
		} finally {
			sandbox.setTimeout = savedSetTimeout;
			sandbox.clearTimeout = savedClearTimeout;
		}
  } finally {
    for (const key of keys) {
      const value = saved.get(key);
      if (value === undefined) delete overrides[key]; else overrides[key] = value;
    }
  }
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

// Regression for "Bitwarden popups intermittently break evaluate": a password
// manager pops its vault out into its own FOCUSED window. Chrome refuses
// chrome.debugger access to another extension's pages, so if brw resolves that
// tab as the foreground tab, every no-tab_id tool fails with
// "Cannot access a chrome-extension:// URL of different extension" until the
// human closes the popout. brw must skip it and keep working on a real page.
const BITWARDEN_POPOUT = "chrome-extension://nngceckbapebfimnlniiiahkandclblb/popup/index.html";

async function scenarioForeignExtensionPopoutNeverStealsForeground() {
  await reset();

  // The predicate itself: foreign + own extension pages and browser chrome are
  // not drivable; real pages and brw's own about:blank scratch target are.
  check("foreign extension page is not drivable", T.isAgentDrivableUrl(BITWARDEN_POPOUT) === false);
  // brw CAN debug its own pages, and the documented "make a new build live" flow
  // opens options.html and calls chrome.runtime.reload() on it — so own-extension
  // pages must stay drivable. Only foreign extensions are refused by Chrome.
  check("brw's own extension page stays drivable", T.isAgentDrivableUrl("chrome-extension://amocjcgddnoakjijfggdpnefdnboilpe/options.html") === true);
  check("own-extension check is id-scoped, not prefix-loose", T.isAgentDrivableUrl("chrome-extension://amocjcgddnoakjijfggdpnefdnboilpeEVIL/x.html") === false);
  check("chrome:// settings is not drivable", T.isAgentDrivableUrl("chrome://settings/") === false);
  check("devtools:// is not drivable", T.isAgentDrivableUrl("devtools://devtools/bundled/inspector.html") === false);
  check("the Web Store is not drivable", T.isAgentDrivableUrl("https://chromewebstore.google.com/detail/abc") === false);
  check("a real page is drivable", T.isAgentDrivableUrl("https://example.test/x") === true);
  check("about:blank is drivable", T.isAgentDrivableUrl("about:blank") === true);
  check("a URL-less (mid-creation) tab is drivable", T.isAgentDrivableUrl("") === true);

  // 1. The popout is a FOCUSED popup window; the user's real page sits in an
  //    unfocused normal window. Resolution must land on the real page.
  setWin({ id: 1, type: "normal", focused: false });
  setTab({ id: 11, windowId: 1, active: true, url: "https://app.test/invoices", title: "app" });
  setWin({ id: 2, type: "popup", focused: true });
  setTab({ id: 22, windowId: 2, active: true, url: BITWARDEN_POPOUT, title: "Bitwarden" });
  check("focused foreign-extension popout does not become the foreground tab",
    (await T.resolveForegroundTabId()) === 11);

  // 2. The focus event for that popout must not poison the cache either — that
  //    would re-break resolution through step 3's cache fallback.
  await T.publishActiveTab(22);
  check("a foreign-extension tab is never cached as active", T.state.activeTabId !== 22);

  // 3. With an agent pin on a real tab, the pin still wins outright.
  T.state.agentTabId = 11;
  check("agent pin still wins over a focused popout", (await T.resolveForegroundTabId()) === 11);

  // 4. If the agent's OWN pinned tab is showing a non-drivable page, fail closed
  //    rather than silently moving work onto a usable human tab. Keep the pin so
  //    it resumes after navigating back.
  await reset();
  setWin({ id: 1, type: "normal", focused: true });
  setTab({ id: 5, windowId: 1, active: false, url: BITWARDEN_POPOUT, title: "vault" });
  setTab({ id: 6, windowId: 1, active: true, url: "https://real.test/", title: "real" });
  T.state.agentTabId = 5;
  let pinnedRejected = false;
  try { await T.resolveForegroundTabId(); } catch (error) { pinnedRejected = String(error).includes("agent-pinned tab 5"); }
  check("non-drivable pinned tab fails closed instead of selecting a human tab", pinnedRejected);
  check("the agent pin is retained, not cleared", T.state.agentTabId === 5);
  model.tabs.get(5).url = "https://back.test/";
  check("the pin resumes once it navigates back to a real page", (await T.resolveForegroundTabId()) === 5);

  // 5. Degenerate case: the ONLY tab is a foreign extension page. Resolution must
  //    return null (surfacing "no active tab") rather than a tab that cannot be
  //    driven — a clear failure beats a confusing CDP error on every call.
  await reset();
  setWin({ id: 1, type: "normal", focused: true });
  setTab({ id: 9, windowId: 1, active: true, url: BITWARDEN_POPOUT, title: "Bitwarden" });
  check("no drivable tab resolves to null, not the extension page",
    (await T.resolveForegroundTabId()) === null);
  // ...and list_tabs must agree. Falling back to Chrome's raw per-window active
  // flag here is what let the daemon re-cache the undrivable tab as active and
  // keep targeting it, defeating the resolver fix entirely.
  const nothingDrivable = await T.listTabSummaries();
  check("list_tabs reports the undrivable tab", nothingDrivable.some((t) => t.id === 9));
  check("list_tabs marks NO tab active when none is drivable",
    nothingDrivable.every((t) => t.active === false));

  // 6. list_tabs must still REPORT the extension tab (it is a real target an
  //    agent can address explicitly by tab_id) — it just must not be marked active.
  await reset();
  setWin({ id: 1, type: "normal", focused: true });
  setTab({ id: 11, windowId: 1, active: true, url: "https://app.test/", title: "app" });
  setWin({ id: 2, type: "popup", focused: false });
  setTab({ id: 22, windowId: 2, active: true, url: BITWARDEN_POPOUT, title: "Bitwarden" });
  const listed = await T.listTabSummaries();
  check("list_tabs still reports the extension tab", listed.some((t) => t.id === 22));
  check("the extension tab is not reported active", listed.find((t) => t.id === 22)?.active === false);
  check("the real page is reported active", listed.find((t) => t.id === 11)?.active === true);
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
  await scenarioForeignExtensionPopoutNeverStealsForeground();
  await scenarioDownloadSnapshotsAndFailClosedProvenance();
  await scenarioMainDocumentIdentityIsExactAndMonotonic();
  await scenarioLargeResponsesUseBoundedFrames();
  await scenarioWaitForTabGoneIsBoundedAndHonest();
  await scenarioTabRemovalPublishesDaemonInvalidation();
  await scenarioCloseTabIsBoundedAndFailClosed();
  console.log(failures === 0 ? "\nALL PASS" : `\n${failures} FAILURES`);
  process.exit(failures === 0 ? 0 : 1);
})();
