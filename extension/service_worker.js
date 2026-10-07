const BRIDGE_URL = "ws://127.0.0.1:17311/extension";
const BRIDGE_STATUS_URL = "http://127.0.0.1:17311/status";
const BRIDGE_CONFIG_KEY = "brwBridgeConfig";
const BRIDGE_STATUS_KEY = "brwBridge";
const BRIDGE_CONSENT_KEY = "brwBrowserControlConsent";
const BRIDGE_CONSENT_VERSION = 1;
const PROTOCOL_VERSION = "0.2.0";
const KEEPALIVE_INTERVAL_MS = 5 * 1000;
const DAEMON_STATUS_INTERVAL_MS = 10 * 1000;
const DAEMON_STATUS_TIMEOUT_MS = 2 * 1000;
const MAX_DAEMON_STATUS_FAILURES = 3;
const MAX_RECONNECT_DELAY_MS = 3 * 1000;
const BRIDGE_ACCEPT_GRACE_MS = 1000;
const WS_CLOSE_TRY_AGAIN_LATER = 1013;
const WS_CLOSE_POLICY_VIOLATION = 1008;
const IDLE_DETACH_MS = 120 * 1000;
const BADGE_IDLE_BG = "#1a7f37";
const BADGE_AGENT_BG = "#9f006f";
const BADGE_AGENT_PULSE_BG = "#d1008f";
const BADGE_CONNECTING_BG = "#bf8700";
const BADGE_CONNECTING_DIM_BG = "#8a6200";
const BADGE_DOWN_BG = "#c5221f";
const BADGE_USED_WINDOW_MS = 10 * 1000;
const BADGE_ANIM_MS = 450;
const DISCONNECT_NOTIFY_MS = 12 * 1000;
const DISCONNECT_NOTIFY_COOLDOWN_MS = 5 * 60 * 1000;
const BRW_ACTING_WINDOW_MS = 8 * 1000;
const CLOSE_TAB_BUDGET_MS = 2 * 1000;
const CLOSE_TAB_SETTLE_MS = 8 * 1000;
const SELF_UPDATE_SETTLE_MS = 5 * 1000;
const SELF_UPDATE_MAX_DEFER_MS = 5 * 60 * 1000;
const SELF_UPDATE_RETRY_MS = 10 * 60 * 1000;
const SELF_UPDATE_KEY = "brwSelfUpdate";
const RESPONSE_DIRECT_MAX_BYTES = 3 * 1024 * 1024;
const RESPONSE_CHUNK_BYTES = 2 * 1024 * 1024;
const RESPONSE_TOTAL_MAX_BYTES = 64 * 1024 * 1024;
const WORKER_INSTANCE_ID = (() => {
  try {
    if (globalThis.crypto?.randomUUID) return globalThis.crypto.randomUUID();
  } catch (_) {}
  return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}-${Math.random().toString(36).slice(2)}`;
})();
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
async function sendPolicedCdp(tabId, debuggee, method, params) {
  if (isDeniedCdpMethod(method)) {
    throw new Error(`cdp method ${method} is blocked by brw policy: cookie and storage access are not permitted`);
  }
  markActing(tabId);
  if (!debuggee) {
    const result = await sendDebuggerCommand(tabId, method, params || {});
    rememberDeviceEmulation(tabId, method, params || {});
    return result;
  }
  return await chrome.debugger.sendCommand(debuggee, method, params || {});
}
function rememberDeviceEmulation(tabId, method, params) {
  if (method === "Emulation.clearDeviceMetricsOverride") {
    state.deviceEmulationOverrides.delete(tabId);
    return;
  }
  if (method === "Emulation.setDeviceMetricsOverride" && !state.deviceEmulationOverrides.has(tabId)) {
    state.deviceEmulationOverrides.set(tabId, new Map());
  }
  const overrides = state.deviceEmulationOverrides.get(tabId);
  if (overrides && ["Emulation.setDeviceMetricsOverride", "Emulation.setTouchEmulationEnabled", "Emulation.setEmitTouchEventsForMouse", "Emulation.setUserAgentOverride"].includes(method)) {
    overrides.set(method, structuredClone(params));
  }
}
let offscreenSetupPromise = null;
let packagedDefaultConfigPromise = null;
let tabJuggleQueue = Promise.resolve();

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
  attachUsedAt: new Map(),
  deviceEmulationOverrides: new Map(),
  activeTabId: null,
  agentTabId: null,
  handling: 0,
  selfUpdateCheck: null,
  selfUpdatePending: null,
  reconnectAttempt: 0,
  acceptedSocket: null,
  acceptTimer: null,
  lastError: "",
  lastAgentActivityAt: 0,
  reportedStatus: "starting",
  bridgeConfig: null,
  bridgeConfigSource: "built-in",
  snapshotCache: new Map(),
  observerInjected: new Set(),
  documentEpochs: new Map(),
  foreignExtensionFrames: new Map(),
  fileChooserEvents: new Map(),
  containment: { allowed: [], blocked: [], enabled: false },
  containmentGuard: "",
  containmentTabs: new Set(),
  webmcpSource: "",
  webmcpTabs: new Set(),
  inlineDocumentTabs: new Map(),
  navigationOutcomes: new Map(),
  blockedRequests: new Map(),
  routeRuleIds: new Map(),
  dialogArm: new Map(),
  dialogLog: new Map(),
  actingUntil: new Map(),
	consoleMessages: new Map(),
  forcedHoverNodes: new Map(),
  forcedHoverTimers: new Map(),
  downloads: new Map(),
  downloadCorrelation: new Map(),
  downloadProvenance: []
};

const MAX_TRACKED_DOWNLOADS = 200;
const MAX_DOWNLOAD_PROVENANCE = MAX_TRACKED_DOWNLOADS * 2;
const DOWNLOAD_PROVENANCE_WINDOW_MS = 5 * 1000;
const MAX_DOWNLOAD_URL_CHARS = 8 * 1024;
const MAX_DOWNLOAD_FILENAME_CHARS = 1000;
const MAX_CONSOLE_MESSAGES = 200;
const MAX_DIALOG_RECORDS = 20;
const MAX_BLOCKED_REQUESTS = 100;

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

function recordDialog(tabId, record) {
  if (typeof tabId !== "number") return;
  const entries = state.dialogLog.get(tabId) || [];
  entries.push(record);
  if (entries.length > MAX_DIALOG_RECORDS) entries.splice(0, entries.length - MAX_DIALOG_RECORDS);
  state.dialogLog.set(tabId, entries);
}

function containmentHostMatches(host, domain) {
  if (!host || !domain) return false;
  host = String(host).toLowerCase();
  domain = String(domain).toLowerCase();
  return host === domain || host.endsWith("." + domain);
}

function containmentHostOf(raw) {
  try {
    const u = new URL(String(raw));
    if (u.protocol === "ws:" || u.protocol === "wss:" || u.protocol === "http:" || u.protocol === "https:") {
      return u.hostname.toLowerCase();
    }
    return "";
  } catch (e) {
    return "";
  }
}

function containmentPermits(raw) {
  const host = containmentHostOf(raw);
  if (!host) return true;
  const { allowed, blocked } = state.containment;
  for (const b of blocked) if (containmentHostMatches(host, b)) return false;
  if (!allowed.length) return true;
  for (const a of allowed) if (containmentHostMatches(host, a)) return true;
  return false;
}

function recordBlockedRequest(tabId, record) {
  if (typeof tabId !== "number") return;
  const entries = state.blockedRequests.get(tabId) || [];
  entries.push(record);
  if (entries.length > MAX_BLOCKED_REQUESTS) entries.splice(0, entries.length - MAX_BLOCKED_REQUESTS);
  state.blockedRequests.set(tabId, entries);
}

const INLINE_DOCUMENT_TEXT_TYPES = new Set([
  "text/csv", "text/tab-separated-values", "application/csv",
  "application/x-ndjson", "application/ndjson", "application/jsonl", "application/x-jsonlines",
  "application/yaml", "application/x-yaml", "application/toml"
]);
const INLINE_DOCUMENT_BODY_LIMIT = 8 << 20;

function inlineDocumentRewrite(headers) {
  const out = [];
  let changed = false;
  let needsBody = false;
  for (const header of Array.isArray(headers) ? headers : []) {
    const name = String(header?.name || "");
    const value = String(header?.value || "");
    const lower = name.toLowerCase();
    if (lower === "content-disposition") {
      const kind = value.split(";")[0].trim().toLowerCase();
      if (kind === "attachment") { changed = true; continue; }
    } else if (lower === "content-type") {
      const [mediaType, ...params] = value.split(";");
      if (INLINE_DOCUMENT_TEXT_TYPES.has(mediaType.trim().toLowerCase())) {
        const rest = params.join(";");
        out.push({ name, value: rest.trim() ? "text/plain;" + rest : "text/plain" });
        changed = true;
        needsBody = true;
        continue;
      }
    }
    out.push({ name, value });
  }
  const kept = needsBody
    ? out.filter((h) => !["content-encoding", "content-length", "transfer-encoding"].includes(h.name.toLowerCase()))
    : out;
  return { headers: kept, changed, needsBody };
}

function inlineDocumentBodyWithinLimit(headers) {
  for (const header of Array.isArray(headers) ? headers : []) {
    if (String(header?.name || "").toLowerCase() !== "content-length") continue;
    const length = Number(String(header?.value || "").trim());
    return !Number.isFinite(length) || length <= INLINE_DOCUMENT_BODY_LIMIT;
  }
  return true;
}

function inlineDocumentPattern(url) {
  let parsed;
  try {
    parsed = new URL(String(url || ""));
  } catch (_) {
    return "";
  }
  if (parsed.protocol !== "http:" && parsed.protocol !== "https:") return "";
  return parsed.origin.replace(/[\\*?]/g, (c) => "\\" + c) + "/*";
}

function redirectLocation(params) {
  const status = Number(params?.responseStatusCode) || 0;
  if (status < 300 || status > 399) return "";
  const header = (params.responseHeaders || []).find((h) => String(h?.name || "").toLowerCase() === "location");
  if (!header) return "";
  try {
    return new URL(String(header.value || "").trim(), params.request?.url).href;
  } catch (_) {
    return "";
  }
}

function recordDocumentResponse(tabId, params) {
  const outcome = state.navigationOutcomes.get(tabId);
  if (!outcome) return;
  const status = Number(params?.responseStatusCode) || 0;
  const headerName = status === 407 ? "proxy-authenticate" : "www-authenticate";
  outcome.status = status;
  outcome.url = String(params?.request?.url || outcome.url).slice(0, 2000);
  outcome.authenticate = (status === 401 || status === 407)
    ? (params?.responseHeaders || [])
      .filter((h) => String(h?.name || "").toLowerCase() === headerName)
      .map((h) => String(h?.value || "").slice(0, 1000))
      .slice(0, 8)
    : [];
}

async function answerInlineDocumentResponse(tabId, params, resourceType) {
  const requestId = params.requestId;
  const continueAsIs = () => chrome.debugger.sendCommand({ tabId }, "Fetch.continueResponse", { requestId });
  const arm = state.inlineDocumentTabs.get(tabId);
  if (!arm || resourceType !== "Document") return continueAsIs();
  if (arm.mainFrameId && params.frameId && params.frameId !== arm.mainFrameId) return continueAsIs();
  const location = redirectLocation(params);
  if (!location) recordDocumentResponse(tabId, params);
  if (location) {
    const pattern = inlineDocumentPattern(location);
    if (pattern && !arm.patterns.includes(pattern)) {
      arm.patterns.push(pattern);
      await syncFetchInterception(tabId).catch(() => {});
    }
    return continueAsIs();
  }
  try {
    return await answerInlineDocument(tabId, params);
  } finally {
    if (state.inlineDocumentTabs.get(tabId) === arm) {
      state.inlineDocumentTabs.delete(tabId);
      await syncFetchInterception(tabId).catch(() => {});
    }
  }
}

async function answerInlineDocument(tabId, params) {
  const requestId = params.requestId;
  const continueAsIs = () => chrome.debugger.sendCommand({ tabId }, "Fetch.continueResponse", { requestId });
  const rewrite = inlineDocumentRewrite(params.responseHeaders);
  if (!rewrite.changed) return continueAsIs();
  if (!rewrite.needsBody) {
    return chrome.debugger.sendCommand({ tabId }, "Fetch.continueResponse", {
      requestId,
      responseCode: params.responseStatusCode,
      responseHeaders: rewrite.headers
    });
  }
  if (!inlineDocumentBodyWithinLimit(params.responseHeaders)) return continueAsIs();
  let body;
  try {
    body = await chrome.debugger.sendCommand({ tabId }, "Fetch.getResponseBody", { requestId });
  } catch (_) {
    return continueAsIs();
  }
  const encoded = body?.base64Encoded ? body.body : btoa(unescape(encodeURIComponent(String(body?.body || ""))));
  return chrome.debugger.sendCommand({ tabId }, "Fetch.fulfillRequest", {
    requestId,
    responseCode: params.responseStatusCode,
    responseHeaders: rewrite.headers,
    body: encoded
  });
}

function fetchPatternsForTab(tabId) {
  const patterns = [];
  if (state.containmentTabs.has(tabId)) patterns.push({ urlPattern: "*" });
  for (const urlPattern of state.inlineDocumentTabs.get(tabId)?.patterns || []) {
    patterns.push({ urlPattern, resourceType: "Document", requestStage: "Response" });
  }
  return patterns;
}

async function syncFetchInterception(tabId) {
  const patterns = fetchPatternsForTab(tabId);
  if (!patterns.length) {
    if (state.attachedTabs.has(tabId)) await sendDebuggerCommand(tabId, "Fetch.disable", {}).catch(() => {});
    return;
  }
  await sendDebuggerCommand(tabId, "Fetch.enable", { patterns });
}

async function armInlineDocument(tabId, url) {
  state.navigationOutcomes.delete(tabId);
  const pattern = url ? inlineDocumentPattern(url) : "*";
  if (!pattern) return;
  state.navigationOutcomes.set(tabId, { url: String(url || ""), status: 0, authenticate: [], error: "" });
  await attach(tabId);
  const tree = await sendDebuggerCommand(tabId, "Page.getFrameTree", {}).catch(() => null);
  state.inlineDocumentTabs.set(tabId, { patterns: [pattern], mainFrameId: tree?.frameTree?.frame?.id || "" });
  await syncFetchInterception(tabId);
}

async function disarmInlineDocument(tabId) {
  if (!state.inlineDocumentTabs.delete(tabId)) return;
  await syncFetchInterception(tabId);
}

const MAX_ROUTE_RULES = 50;

const ROUTE_RULE_ID_MIN = 1;
const ROUTE_RULE_ID_MAX = 100000;

const ROUTE_RESOURCE_TYPES = [
  "main_frame", "sub_frame", "stylesheet", "script", "image", "font", "object",
  "xmlhttprequest", "ping", "csp_report", "media", "websocket", "webtransport",
  "webbundle", "other"
];

function routeResourceTypes() {
  const declared = chrome.declarativeNetRequest && chrome.declarativeNetRequest.ResourceType;
  if (!declared || typeof declared !== "object") return ROUTE_RESOURCE_TYPES.slice();
  const known = new Set(Object.values(declared).filter((value) => typeof value === "string"));
  const supported = ROUTE_RESOURCE_TYPES.filter((type) => known.has(type));
  return supported.length ? supported : ROUTE_RESOURCE_TYPES.slice();
}

function hasRouteRuleApi() {
  return Boolean(chrome.declarativeNetRequest
    && chrome.declarativeNetRequest.updateSessionRules
    && chrome.declarativeNetRequest.getSessionRules);
}

async function liveRouteRules() {
  const rules = await chrome.declarativeNetRequest.getSessionRules();
  if (!Array.isArray(rules)) return [];
  return rules.filter((rule) => Number.isInteger(rule && rule.id)
    && rule.id >= ROUTE_RULE_ID_MIN && rule.id <= ROUTE_RULE_ID_MAX);
}

function routeRuleTargetsTab(rule, tabId) {
  const tabIds = rule && rule.condition && rule.condition.tabIds;
  return Array.isArray(tabIds) && tabIds.includes(tabId);
}

async function dropOrphanedRouteRules() {
  if (!hasRouteRuleApi()) return;
  const live = await liveRouteRules();
  if (!live.length) return;
  const openTabs = await chrome.tabs.query({});
  const open = new Set((Array.isArray(openTabs) ? openTabs : []).map((tab) => tab && tab.id));
  const removeRuleIds = live
    .filter((rule) => {
      const tabIds = rule && rule.condition && rule.condition.tabIds;
      if (!Array.isArray(tabIds) || !tabIds.length) return true;
      return !tabIds.some((id) => open.has(id));
    })
    .map((rule) => rule.id);
  if (!removeRuleIds.length) return;
  await chrome.declarativeNetRequest.updateSessionRules({ removeRuleIds, addRules: [] });
}

async function setTabRouteRules(tabId, rules) {
  if (!hasRouteRuleApi()) {
    throw new Error("this Chrome build has no chrome.declarativeNetRequest session rules; request interception is unavailable");
  }
  if (!Number.isFinite(tabId) || tabId <= 0) throw new Error("set_routes needs a tab id");
  if (rules.length > MAX_ROUTE_RULES) {
    throw new Error(`at most ${MAX_ROUTE_RULES} routes per tab`);
  }
  for (const rule of rules) {
    if (typeof rule?.regex !== "string" || !rule.regex) throw new Error("each route rule needs a regex");
    if (rule?.behaviour !== "abort") {
      throw new Error(`route behaviour ${JSON.stringify(rule?.behaviour)} cannot be enforced by declarativeNetRequest; only abort can`);
    }
    if (rule.resourceTypes !== undefined) {
      if (!Array.isArray(rule.resourceTypes) || !rule.resourceTypes.length
          || rule.resourceTypes.some((type) => typeof type !== "string" || !type)) {
        throw new Error("a rule's resourceTypes must be a non-empty array of type names");
      }
    }
  }

  const remembered = new Set(state.routeRuleIds.get(tabId) || []);
  const live = await liveRouteRules();
  const removeRuleIds = [];
  const taken = new Set();
  for (const rule of live) {
    if (routeRuleTargetsTab(rule, tabId) || remembered.has(rule.id)) removeRuleIds.push(rule.id);
    else taken.add(rule.id);
  }

  let nextId = ROUTE_RULE_ID_MIN;
  const allocateRuleId = () => {
    while (taken.has(nextId)) nextId++;
    if (nextId > ROUTE_RULE_ID_MAX) throw new Error("no free declarativeNetRequest session rule id is left for a route");
    taken.add(nextId);
    return nextId;
  };

  const resourceTypes = routeResourceTypes();
  const addRules = [];
  const addedIds = [];
  for (const rule of rules) {
    const id = allocateRuleId();
    addedIds.push(id);
    const ruleResourceTypes = Array.isArray(rule.resourceTypes) && rule.resourceTypes.length
      ? rule.resourceTypes.filter((type) => resourceTypes.includes(type))
      : resourceTypes;
    addRules.push({
      id,
      priority: MAX_ROUTE_RULES - addRules.length,
      action: { type: "block" },
      condition: {
        regexFilter: rule.regex,
        isUrlFilterCaseSensitive: true,
        resourceTypes: ruleResourceTypes,
        tabIds: [tabId]
      }
    });
  }
  await chrome.declarativeNetRequest.updateSessionRules({ removeRuleIds, addRules });
  if (addedIds.length) state.routeRuleIds.set(tabId, addedIds);
  else state.routeRuleIds.delete(tabId);
  return { tabId, count: addedIds.length };
}

async function clearTabRouteRules(tabId) {
  const remembered = new Set(state.routeRuleIds.get(tabId) || []);
  state.routeRuleIds.delete(tabId);
  try {
    if (!hasRouteRuleApi()) return;
    const removeRuleIds = (await liveRouteRules())
      .filter((rule) => routeRuleTargetsTab(rule, tabId) || remembered.has(rule.id))
      .map((rule) => rule.id);
    if (!removeRuleIds.length) return;
    await chrome.declarativeNetRequest.updateSessionRules({ removeRuleIds, addRules: [] });
  } catch (_) {
  }
}

function mapDownloadState(state) {
  switch (state) {
    case "complete": return "completed";
    case "interrupted": return "canceled";
    case "in_progress": return "inProgress";
    default: return state || "inProgress";
  }
}

function recordDownload(item) {
  if (!item || typeof item.id !== "number") return;
  const guid = String(item.id);
  const prev = state.downloads.get(guid) || { guid };
  const path = (typeof item.filename === "string" && item.filename) ? item.filename : prev.path;
  const url = boundedDownloadText(item.url || item.finalUrl || prev.url || "", MAX_DOWNLOAD_URL_CHARS);
  const suggestedFilename = boundedDownloadText(
    path ? downloadBasename(path) : (prev.suggested_filename || ""),
    MAX_DOWNLOAD_FILENAME_CHARS
  );
  const nextState = item.state ? mapDownloadState(item.state) : (prev.state || "inProgress");
  let changedAt = Number(prev.changed_at_ms) || 0;
  if (!changedAt || nextState !== prev.state) changedAt = Date.now();
  if (nextState === "completed" || nextState === "canceled") {
    const ended = item.endTime ? Date.parse(item.endTime) : NaN;
    if (Number.isFinite(ended)) changedAt = ended;
  }
  const next = {
    guid,
    url,
    suggested_filename: suggestedFilename,
    state: nextState,
    received_bytes: typeof item.bytesReceived === "number" ? item.bytesReceived : (prev.received_bytes || 0),
    total_bytes: typeof item.totalBytes === "number" && item.totalBytes > 0 ? item.totalBytes : (prev.total_bytes || (typeof item.fileSize === "number" && item.fileSize > 0 ? item.fileSize : 0)),
    path: path || "",
    changed_at_ms: changedAt
  };
  const priorCorrelation = state.downloadCorrelation.get(guid);
  const urls = new Set(priorCorrelation?.urls || []);
  for (const candidate of [item.url, item.finalUrl, url]) {
    const bounded = boundedDownloadText(candidate || "", MAX_DOWNLOAD_URL_CHARS);
    if (bounded) urls.add(bounded);
  }
  let directTabId = priorCorrelation?.directTabId || "";
  let directConflict = priorCorrelation?.directConflict === true;
  if (Number.isInteger(item.tabId) && item.tabId >= 0) {
    const supplied = String(item.tabId);
    if (directTabId && directTabId !== supplied) directConflict = true;
    directTabId = directConflict ? "" : supplied;
  }
  state.downloadCorrelation.set(guid, {
    urls: Array.from(urls),
    suggestedFilename,
    observedAt: priorCorrelation?.observedAt || Date.now(),
    directTabId,
    directConflict
  });
  state.downloads.delete(guid);
  state.downloads.set(guid, next);
  while (state.downloads.size > MAX_TRACKED_DOWNLOADS) {
    let oldest = null;
    for (const [candidateGUID, candidate] of state.downloads) {
      if (candidate.state === "completed" || candidate.state === "canceled") {
        oldest = candidateGUID;
        break;
      }
    }
    if (oldest == null) oldest = state.downloads.keys().next().value;
    state.downloads.delete(oldest);
    state.downloadCorrelation.delete(oldest);
  }
}

function boundedDownloadText(value, limit) {
  const text = String(value || "");
  if (text.length <= limit) return text;
  let out = text.slice(0, limit);
  const last = out.charCodeAt(out.length - 1);
  if (last >= 0xD800 && last <= 0xDBFF) out = out.slice(0, -1);
  return out;
}

function downloadBasename(path) {
  return String(path || "").split(/[\\/]/).pop() || "";
}

function recordDownloadProvenance(tabId, params) {
  if (!Number.isInteger(tabId) || tabId < 0 || !params) return;
  const url = boundedDownloadText(params.url || "", MAX_DOWNLOAD_URL_CHARS);
  if (!url) return;
  const guid = boundedDownloadText(params.guid || "", 500);
  const observation = {
    guid,
    tabId: String(tabId),
    url,
    suggestedFilename: boundedDownloadText(params.suggestedFilename || "", MAX_DOWNLOAD_FILENAME_CHARS),
    observedAt: Date.now()
  };
  const duplicate = guid ? state.downloadProvenance.findIndex((entry) => entry.guid === guid) : -1;
  if (duplicate >= 0) {
    const previous = state.downloadProvenance[duplicate];
    observation.tabId = previous.tabId === observation.tabId ? observation.tabId : "";
    observation.observedAt = previous.observedAt;
    state.downloadProvenance[duplicate] = observation;
  } else {
    state.downloadProvenance.push(observation);
  }
  if (state.downloadProvenance.length > MAX_DOWNLOAD_PROVENANCE) {
    state.downloadProvenance.splice(0, state.downloadProvenance.length - MAX_DOWNLOAD_PROVENANCE);
  }
}

function matchingDownloadProvenance(correlation) {
  let candidates = state.downloadProvenance.filter((entry) =>
    entry.tabId &&
    Math.abs(entry.observedAt - correlation.observedAt) <= DOWNLOAD_PROVENANCE_WINDOW_MS &&
    correlation.urls.includes(entry.url)
  );
  if (candidates.length > 1 && correlation.suggestedFilename) {
    const filenameMatches = candidates.filter((entry) =>
      entry.suggestedFilename && entry.suggestedFilename === correlation.suggestedFilename
    );
    if (filenameMatches.length > 0) candidates = filenameMatches;
  }
  return candidates;
}

function downloadSnapshot() {
  const candidatesByGUID = new Map();
  const usesByProvenance = new Map();
  for (const [guid, correlation] of state.downloadCorrelation) {
    if (correlation.directTabId) continue;
    const candidates = matchingDownloadProvenance(correlation);
    candidatesByGUID.set(guid, candidates);
    for (const candidate of candidates) {
      usesByProvenance.set(candidate, (usesByProvenance.get(candidate) || 0) + 1);
    }
  }
  return Array.from(state.downloads.entries()).map(([guid, entry]) => {
    const out = { ...entry };
    const correlation = state.downloadCorrelation.get(guid);
    let tabId = correlation?.directTabId || "";
    if (!tabId) {
      const candidates = candidatesByGUID.get(guid) || [];
      if (candidates.length === 1 && usesByProvenance.get(candidates[0]) === 1) {
        tabId = candidates[0].tabId;
      }
    }
    if (tabId) out.tab_id = tabId;
    return out;
  });
}

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

if (chrome.downloads && chrome.downloads.onCreated) {
  chrome.downloads.onCreated.addListener((item) => recordDownload(item));
  chrome.downloads.onChanged.addListener((delta) => {
    const item = flattenDownloadDelta(delta);
    if (item) recordDownload(item);
  });
}

function markActing(tabId) {
  if (typeof tabId === "number") state.actingUntil.set(tabId, Date.now() + BRW_ACTING_WINDOW_MS);
  queueAgentActivity(tabId);
}

function isActing(tabId) {
  return Date.now() < (state.actingUntil.get(tabId) || 0);
}

function isOperatorChromeUrl(url) {
  return /^(chrome|chrome-extension|devtools|edge|about|brave|chrome-search):/i.test(String(url || ""));
}

function queueAgentActivity(tabId) {
  Promise.resolve().then(async () => {
    if (typeof tabId === "number") {
      const tab = await chrome.tabs.get(tabId).catch(() => null);
      if (isOperatorChromeUrl(tab?.url)) return;
    }
    touchAgentActivity();
  }).catch(() => {});
}

function touchAgentActivity() {
  state.lastAgentActivityAt = Date.now();
  if (isBridgeLive() || state.reportedStatus === "connected") {
    setBridgeBadge(isBridgeLive() ? "connected" : state.reportedStatus);
  }
}

chrome.runtime.onInstalled.addListener(async (details) => {
  ensureConnectAlarm();
  ensureOffscreen();
  reconcileDebuggerAttachments().catch(() => {});
  if (!(await hasBrowserControlConsent())) {
    await markBridgeStatus("consent_required", "Browser control has not been enabled by the user.");
    if (details?.reason === "install" || details?.reason === "update") {
      chrome.runtime.openOptionsPage().catch(() => {});
    }
  }
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
  if (message?.type === "BRW_SET_CONSENT") {
    setBrowserControlConsent(Boolean(message.granted)).then(() => bridgeDebugStatus())
      .then((status) => sendResponse({ ok: true, status }))
      .catch((error) => sendResponse({ ok: false, error: String(error?.message || error) }));
    return true;
  }
  if (message?.type === "BRW_GET_STATUS") {
    bridgeDebugStatus().then((status) => sendResponse({ ok: true, status })).catch((error) => {
      sendResponse({ ok: false, error: String(error?.message || error) });
    });
    return true;
  }
  if (message?.type === "BRW_GET_CONSENT") {
    fetchSiteConsent().then((consent) => sendResponse({ ok: true, consent }))
      .catch((error) => sendResponse({ ok: false, error: String(error?.message || error) }));
    return true;
  }
  if (message?.type === "BRW_REVOKE_CONSENT") {
    revokeSiteConsent(message.request || {}).then((result) => sendResponse({ ok: true, result }))
      .catch((error) => sendResponse({ ok: false, error: String(error?.message || error) }));
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
  if (message?.type === "BRW_RECONNECT") {
    connect({ probe: true }).then(() => bridgeDebugStatus())
      .then((status) => sendResponse({ ok: true, status }))
      .catch((error) => sendResponse({ ok: false, error: String(error?.message || error) }));
    return true;
  }
  if (message?.type !== "SW_KEEPALIVE") return false;
  connect({ probe: true });
  sendResponse({ ok: true });
  return false;
});
let keepAlivePort = null;
chrome.runtime.onConnect.addListener((port) => {
  if (port.name !== "brw-keepalive") return;
  keepAlivePort = port;
  ensureOffscreen();
  connect({ probe: true });
  port.onDisconnect.addListener(() => {
    if (keepAlivePort === port) keepAlivePort = null;
  });
});
chrome.storage.onChanged.addListener((changes, area) => {
  if (area !== "local") return;
  if (changes[BRIDGE_CONSENT_KEY]) {
    const granted = isGrantedConsent(changes[BRIDGE_CONSENT_KEY].newValue);
    if (granted) {
      connect({ probe: true });
    } else {
      disconnectForConsent().catch(() => {});
    }
    return;
  }
  if (!changes[BRIDGE_CONFIG_KEY]) return;
  loadBridgeConfig().then(() => {
    state.lastError = "";
    if (state.socket) {
      try { state.socket.close(); } catch (_) {}
      state.socket = null;
    }
    connect({ probe: true });
  }).catch((error) => {
    state.lastError = `invalid bridge config: ${String(error?.message || error)}`;
    markBridgeStatus("error", state.lastError).catch(() => {});
  });
});
chrome.alarms.onAlarm.addListener((alarm) => {
  if (alarm.name === "brw-connect") {
    ensureOffscreen();
    connect({ probe: true });
    selfUpdateIfStale().catch(() => {});
  }
});
chrome.runtime.onSuspend.addListener(() => {
  stopKeepAlive();
  setBridgeBadge("disconnected");
  detachAll().catch(() => {});
});
chrome.tabs.onActivated.addListener(async (activeInfo) => {
  await publishActiveTab(activeInfo?.tabId);
});
chrome.tabs.onCreated.addListener(async (tab) => {
  if (tab?.active) await publishActiveTab(tab.id);
});
chrome.tabs.onRemoved.addListener((tabId) => {
  state.foreignExtensionFrames.delete(tabId);
  state.attachedTabs.delete(tabId);
  state.attachUsedAt.delete(tabId);
  state.deviceEmulationOverrides.delete(tabId);
  state.snapshotCache.delete(tabId);
  state.observerInjected.delete(tabId);
  state.documentEpochs.delete(tabId);
  state.navigationOutcomes.delete(tabId);
  state.fileChooserEvents.delete(tabId);
  state.actingUntil.delete(tabId);
	state.consoleMessages.delete(tabId);
  state.forcedHoverNodes.delete(tabId);
  if (state.forcedHoverTimers.has(tabId)) clearTimeout(state.forcedHoverTimers.get(tabId));
  state.forcedHoverTimers.delete(tabId);
  void clearTabRouteRules(tabId);
  if (state.activeTabId === tabId) state.activeTabId = null;
  if (state.agentTabId === tabId) state.agentTabId = null;
  send({ type: "tab_removed", tabId });
});
chrome.windows.onFocusChanged.addListener(async (windowId) => {
  if (windowId === chrome.windows.WINDOW_ID_NONE) return;
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
  state.containmentTabs.delete(source.tabId);
  state.webmcpTabs.delete(source.tabId);
  state.inlineDocumentTabs.delete(source.tabId);
  state.blockedRequests.delete(source.tabId);
  state.dialogArm.delete(source.tabId);
  state.dialogLog.delete(source.tabId);
    state.actingUntil.delete(source.tabId);
    state.forcedHoverNodes.delete(source.tabId);
    if (state.forcedHoverTimers.has(source.tabId)) clearTimeout(state.forcedHoverTimers.get(source.tabId));
    state.forcedHoverTimers.delete(source.tabId);
  }
});
chrome.debugger.onEvent.addListener((source, method, params) => {
	if ((method === "Page.downloadWillBegin" || method === "Browser.downloadWillBegin") && typeof source.tabId === "number") {
	  recordDownloadProvenance(source.tabId, params);
	  return;
	}
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
  if (method === "Fetch.requestPaused" && typeof source.tabId === "number") {
    const requestId = params?.requestId;
    const url = params?.request?.url || "";
    const resourceType = params?.resourceType || "";
    if (!requestId) return;
    if (params?.responseStatusCode !== undefined || Array.isArray(params?.responseHeaders)) {
      answerInlineDocumentResponse(source.tabId, params, resourceType).catch(() => {});
      return;
    }
    if (!state.containment.enabled || containmentPermits(url)) {
      chrome.debugger.sendCommand({ tabId: source.tabId }, "Fetch.continueRequest", { requestId }).catch(() => {});
      return;
    }
    recordBlockedRequest(source.tabId, {
      url: String(url).slice(0, 2000),
      resource_type: resourceType,
      reason: "not permitted by the brw navigation policy (--allowed-domains/--blocked-domains)",
      at: new Date().toISOString()
    });
    chrome.debugger.sendCommand({ tabId: source.tabId }, "Fetch.failRequest", { requestId, errorReason: "BlockedByClient" }).catch(() => {});
    return;
  }
  if (method === "Page.javascriptDialogOpening" && typeof source.tabId === "number") {
    const type = params?.type || "";
    const arm = state.dialogArm.get(source.tabId);
    let accept;
    let promptText;
    let decidedBy;
    if (arm && arm.remaining > 0) {
      accept = arm.accept;
      promptText = arm.promptText;
      decidedBy = "armed";
      arm.remaining -= 1;
      if (arm.remaining <= 0) state.dialogArm.delete(source.tabId);
    } else {
      accept = isActing(source.tabId) || type === "alert";
      decidedBy = isActing(source.tabId) ? "agent_acting" : "user_safe_default";
    }
    const command = { accept };
    if (type === "prompt" && typeof promptText === "string") command.promptText = promptText;
    recordDialog(source.tabId, {
      type,
      message: String(params?.message || "").slice(0, 2000),
      default_prompt: String(params?.defaultPrompt || "").slice(0, 2000),
      url: String(params?.url || "").slice(0, 2000),
      accepted: accept,
      prompt_text: command.promptText || "",
      decided_by: decidedBy,
      at: new Date().toISOString()
    });
    chrome.debugger.sendCommand(
      { tabId: source.tabId },
      "Page.handleJavaScriptDialog",
      command
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
chrome.webNavigation.onCommitted.addListener((details) => {
  if (typeof details.tabId === "number" && details.frameId === 0) {
    state.snapshotCache.delete(details.tabId);
    state.consoleMessages.delete(details.tabId);
    state.observerInjected.delete(details.tabId);
    state.foreignExtensionFrames.delete(details.tabId);
    state.documentEpochs.set(details.tabId, (state.documentEpochs.get(details.tabId) || 0) + 1);
  }
  if (typeof details.tabId === "number" && details.frameId !== 0) {
    noteSubframeCommit(details.tabId, details.frameId, details.url);
  }
});
chrome.webNavigation.onErrorOccurred.addListener((details) => {
  if (typeof details?.tabId !== "number" || details.frameId !== 0) return;
  const outcome = state.navigationOutcomes.get(details.tabId);
  if (!outcome) return;
  outcome.error = String(details.error || "").slice(0, 200);
});
chrome.webNavigation.onHistoryStateUpdated.addListener((details) => {
  if (typeof details.tabId === "number" && details.frameId === 0) {
    state.snapshotCache.delete(details.tabId);
  }
});

ensureConnectAlarm();
ensureOffscreen();
reconcileDebuggerAttachments().catch(() => {});
dropOrphanedRouteRules().catch(() => {});
markBridgeStatus("starting").catch(() => {});
connect();

async function onDiskBuild() {
  try {
    const response = await fetch(chrome.runtime.getURL("manifest.json"), { cache: "no-store" });
    if (!response.ok) return "";
    const manifest = await response.json();
    return String(manifest?.version || "");
  } catch (_) {
    return "";
  }
}

function selfUpdateBusy(overdue = false) {
  return state.handling > 0 || (!overdue && isAgentActive());
}

function selfUpdateIfStale(options = {}) {
  if (state.selfUpdateCheck) return state.selfUpdateCheck;
  state.selfUpdateCheck = selfUpdateCheckOnce(options).finally(() => {
    state.selfUpdateCheck = null;
  });
  return state.selfUpdateCheck;
}

async function selfUpdateCheckOnce({ settleMs = SELF_UPDATE_SETTLE_MS, now = Date.now } = {}) {
  const loaded = (chrome.runtime.getManifest?.() || {}).version || "";
  const disk = await onDiskBuild();
  if (!loaded || !disk || disk === loaded) {
    state.selfUpdatePending = null;
    return "current";
  }
  if (state.selfUpdatePending?.to !== disk) state.selfUpdatePending = { to: disk, since: now() };
  const overdue = now() - state.selfUpdatePending.since >= SELF_UPDATE_MAX_DEFER_MS;
  if (selfUpdateBusy(overdue)) return "busy";
  const stored = await chrome.storage.local.get(SELF_UPDATE_KEY).catch(() => ({}));
  const last = stored?.[SELF_UPDATE_KEY];
  if (last?.to === disk && now() - Number(last.at || 0) < SELF_UPDATE_RETRY_MS) return "held";
  if (settleMs > 0) await new Promise((resolve) => setTimeout(resolve, settleMs));
  if ((await onDiskBuild()) !== disk) return "settling";
  if (selfUpdateBusy(overdue)) return "busy";
  await chrome.storage.local.set({ [SELF_UPDATE_KEY]: { from: loaded, to: disk, at: now() } });
  chrome.runtime.reload();
  return "reloading";
}

async function connect(options = {}) {
  if (!(await hasBrowserControlConsent())) {
    if (state.socket) await disconnectForConsent();
    else await markBridgeStatus("consent_required", "Browser control has not been enabled by the user.");
    return;
  }
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
  if (!(await hasBrowserControlConsent())) {
    await markBridgeStatus("consent_required", "Browser control has not been enabled by the user.");
    return;
  }
  if (state.reportedStatus !== "rejected") await markBridgeStatus("connecting");

  const socket = new WebSocket(config.bridgeUrl);
  state.socket = socket;
  clearTimeout(state.acceptTimer);
  state.acceptTimer = null;
  let acceptDetail = "";
  const accept = () => {
    if (state.socket !== socket || socket.readyState !== WebSocket.OPEN) return;
    if (state.acceptedSocket === socket) return;
    clearTimeout(state.acceptTimer);
    state.acceptTimer = null;
    state.acceptedSocket = socket;
    state.reconnectAttempt = 0;
    state.lastError = acceptDetail;
    markBridgeStatus("connected", acceptDetail).catch(() => {});
  };

  socket.onopen = async () => {
    if (state.socket !== socket) return;
    if (!(await hasBrowserControlConsent())) {
      if (state.socket !== socket) return;
      if (state.socket === socket) state.socket = null;
      try { socket.close(); } catch (_) {}
      await disconnectForConsent();
      return;
    }
    if (state.socket !== socket) return;
    state.statusProbeFailures = 0;
    const platform = await chrome.runtime.getPlatformInfo().catch(() => ({}));
    const auth = await fetchBridgeToken(config);
    if (state.socket !== socket) return;
    if (!(await hasBrowserControlConsent())) {
      if (state.socket === socket) await disconnectForConsent();
      return;
    }
    if (state.socket !== socket) return;
    const token = auth.token;
    if (!token) {
      acceptDetail = auth.reachable
        ? `${auth.detail} at ${config.statusUrl}. A daemon that requires the token will refuse this connection; it is probably older than the extension.`
        : `${auth.detail} at ${config.statusUrl}. The connection will be refused. Check the bridge address on the options page.`;
      state.lastError = acceptDetail;
    }
    send({
      type: "hello",
      hello: {
        source: "brw-extension",
        version: PROTOCOL_VERSION,
        build: (chrome.runtime.getManifest?.() || {}).version || "",
        chrome: navigator.userAgent,
        platform: platform.os || "",
        workspace: config.workspace || "",
        profile: config.profile || "",
        label: config.label || "",
        agent_tab_id: agentOwnedTabIdForHello(),
        status_url: config.statusUrl,
        bridge_url: config.bridgeUrl,
        config_source: state.bridgeConfigSource,
        token
      }
    });
    startKeepAlive();
    if (state.socket === socket) state.acceptTimer = setTimeout(accept, BRIDGE_ACCEPT_GRACE_MS);
    const tabs = await chrome.tabs.query({ active: true, lastFocusedWindow: true }).catch(() => []);
    if (tabs[0]?.id) await publishActiveTab(tabs[0].id);
    probeDaemonStatus().catch(() => {});
    selfUpdateIfStale().catch(() => {});
  };
  socket.onclose = (event) => {
    if (state.socket !== socket) return;
    state.socket = null;
    clearTimeout(state.acceptTimer);
    state.acceptTimer = null;
    const wasAccepted = state.acceptedSocket === socket;
    if (wasAccepted) state.acceptedSocket = null;
    detachAll().catch(() => {});
    const refusal = wasAccepted ? "" : refusalDetail(event, acceptDetail);
    if (refusal) {
      state.lastError = refusal;
      scheduleReconnect(refusal, { rejected: true });
      return;
    }
    scheduleReconnect(`closed ${event?.code || ""}`.trim());
  };
  socket.onerror = (event) => {
    state.lastError = `websocket error ${String(event?.type || "")}`;
    if (state.reportedStatus !== "rejected") markBridgeStatus("error", state.lastError).catch(() => {});
    try { socket.close(); } catch (_) {}
  };
  socket.onmessage = async (event) => {
    if (state.socket !== socket) return;
    if (!(await hasBrowserControlConsent())) {
      if (state.socket !== socket) return;
      await disconnectForConsent();
      return;
    }
    if (state.socket !== socket) return;
    accept();
    let message;
    try {
      message = JSON.parse(event.data);
    } catch (error) {
      send({ id: null, ok: false, error: String(error) });
      return;
    }
    state.handling += 1;
    try {
      await handle(message);
    } finally {
      state.handling -= 1;
    }
  };
}

function refusalDetail(event, handshakeDetail) {
  const code = Number(event?.code) || 0;
  const reason = String(event?.reason || "").trim();
  if (code === WS_CLOSE_TRY_AGAIN_LATER) {
    return `the daemon refused this browser because another browser profile already holds the bridge${reason ? ` (${reason})` : ""}. Disable brw in the other profile, or give this one its own bridge port.`;
  }
  if (code === WS_CLOSE_POLICY_VIOLATION) {
    return `the daemon refused the handshake${reason ? ` (${reason})` : ""}.${handshakeDetail ? ` ${handshakeDetail}` : " Reload the extension and restart the daemon so both run the same build."}`;
  }
  return "";
}

function agentOwnedTabIdForHello() {
  return Number.isSafeInteger(state.agentTabId) && state.agentTabId > 0
    ? state.agentTabId
    : 0;
}

async function loadBridgeConfig() {
  const defaults = await packagedDefaultBridgeConfig();
  const data = await chrome.storage.local.get(BRIDGE_CONFIG_KEY).catch(() => ({}));
  const stored = data[BRIDGE_CONFIG_KEY] || {};
  state.bridgeConfig = normalizeBridgeConfig({ ...defaults, ...stored });
  state.bridgeConfigSource = bridgeConfigSource(defaults, stored);
  return state.bridgeConfig;
}

const BRIDGE_ENDPOINT_KEYS = ["statusUrl", "bridgeUrl", "url", "bridgePort"];

function hasConfigValue(layer, key) {
  const value = layer && typeof layer === "object" ? layer[key] : undefined;
  return value !== undefined && value !== null && value !== "";
}

function bridgeConfigSource(defaults, stored) {
  for (const key of BRIDGE_ENDPOINT_KEYS) {
    if (hasConfigValue(stored, key)) return "stored";
    if (hasConfigValue(defaults, key)) return "packaged";
  }
  return "built-in";
}

function isGrantedConsent(value) {
  return Boolean(
    value &&
    value.granted === true &&
    Number(value.version) === BRIDGE_CONSENT_VERSION
  );
}

async function readBrowserControlConsent() {
  const data = await chrome.storage.local.get(BRIDGE_CONSENT_KEY).catch(() => ({}));
  const value = data[BRIDGE_CONSENT_KEY] || null;
  return {
    granted: isGrantedConsent(value),
    version: Number(value?.version) || 0,
    grantedAt: typeof value?.grantedAt === "string" ? value.grantedAt : ""
  };
}

async function hasBrowserControlConsent() {
  return (await readBrowserControlConsent()).granted;
}

async function setBrowserControlConsent(granted) {
  const value = {
    granted: Boolean(granted),
    version: BRIDGE_CONSENT_VERSION,
    grantedAt: granted ? new Date().toISOString() : ""
  };
  await chrome.storage.local.set({ [BRIDGE_CONSENT_KEY]: value });
  if (value.granted) {
    await markBridgeStatus("starting", "Browser control enabled by the user.");
    await connect({ probe: true });
  } else {
    await disconnectForConsent();
  }
}

async function disconnectForConsent() {
  const socket = state.socket;
  state.socket = null;
  if (socket) {
    try { socket.close(); } catch (_) {}
  }
  clearTimeout(state.reconnectTimer);
  state.reconnectTimer = null;
  stopKeepAlive();
  await detachAll().catch(() => {});
  await markBridgeStatus("consent_required", "Browser control has been disabled by the user.");
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
  state.bridgeConfigSource = "stored";
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
  const consent = await readBrowserControlConsent();
  const daemon = consent.granted
    ? await fetchDaemonSummary(config)
    : { reachable: false, connected: false, consentRequired: true };
  const badge = resolveBadgeMode(state.reportedStatus || "disconnected");
  return {
    config,
    configSource: state.bridgeConfigSource,
    consent,
    bridge: data[BRIDGE_STATUS_KEY] || null,
    socket: isBridgeLive() ? "open" : (isSocketOpen() || isSocketConnecting() ? "connecting" : "closed"),
    daemon,
    badge,
    agentActive: isAgentActive(),
    lastAgentActivityAt: state.lastAgentActivityAt
      ? new Date(state.lastAgentActivityAt).toISOString()
      : "",
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
      let tabId = null;
      let resolveError = "";
      try {
        tabId = await activeTabId();
      } catch (error) {
        resolveError = String(error?.message || error);
      }
      send({ id: message.id, ok: true, result: { tabId: tabId || 0, error: resolveError } });
      return;
    }
    if (message.type === "get_document_identity") {
      const tabId = Number(message.params?.tabId || (await activeTabId()));
      if (!Number.isSafeInteger(tabId) || tabId <= 0) {
        throw new Error("main-document identity is unavailable");
      }
      let frame = null;
      try {
        frame = await chrome.webNavigation.getFrame({ tabId, frameId: 0 });
      } catch (_) {
        throw new Error("main-document identity is unavailable");
      }
      const documentId = String(frame?.documentId || "").trim();
      let origin = "";
      try {
        origin = new URL(String(frame?.url || "")).origin;
      } catch (_) {
        throw new Error("main-document identity is unavailable");
      }
      if (!documentId || documentId.length > 256 || !origin || origin.length > 2048) {
        throw new Error("main-document identity is unavailable");
      }
      send({ id: message.id, ok: true, result: {
        document_id: documentId,
        document_epoch: state.documentEpochs.get(tabId) || 0,
        worker_instance: WORKER_INSTANCE_ID,
        origin,
        tab_id: tabId
      }});
      return;
    }
    if (message.type === "open_tab") {
      const makeActive = message.params?.active !== false;
      const targetUrl = message.params?.url || "about:blank";
      const inlineDocument = message.params?.inlineDocument === true && targetUrl !== "about:blank";
      const webmcpSource = typeof message.params?.webmcp === "string" ? message.params.webmcp : "";
      const armFirst = inlineDocument || (webmcpSource !== "" && targetUrl !== "about:blank");
      const createParams = { url: armFirst ? "about:blank" : targetUrl, active: makeActive };
      const normalWindowId = await preferredNormalWindowId();
      if (typeof normalWindowId === "number") createParams.windowId = normalWindowId;
      let tab;
      try {
        tab = await chrome.tabs.create(createParams);
      } catch (err) {
        if (!/no current window/i.test(String(err?.message || err))) throw err;
        const win = await chrome.windows.create({
          url: createParams.url,
          focused: false,
        });
        tab = win?.tabs?.[0];
        if (!tab) throw err;
      }
      let webmcpArmed = false;
      if (armFirst && tab.id) {
        if (webmcpSource) {
          webmcpArmed = await armWebMCP(tab.id, webmcpSource, false).then((r) => r.armed).catch(() => false);
        }
        if (inlineDocument) await armInlineDocument(tab.id, targetUrl).catch(() => {});
        tab = await chrome.tabs.update(tab.id, { url: targetUrl });
      }
      if (tab.id) await chrome.tabs.update(tab.id, { autoDiscardable: false }).catch(() => {});
      if (makeActive) state.activeTabId = tab.id || null;
      state.agentTabId = tab.id || null;
      let resultTab = tab;
      let groupWarning = "";
      if (tab.id && hasGroupTarget(message.params)) {
        try {
          const groupId = await groupTabForParams(tab, message.params);
          if (typeof groupId === "number" && groupId >= 0 && makeActive) {
            await chrome.tabGroups.update(groupId, { collapsed: false }).catch(() => {});
            await chrome.tabs.update(tab.id, { active: true }).catch(() => {});
          }
          resultTab = await chrome.tabs.get(tab.id).catch(() => tab);
        } catch (error) {
          groupWarning = `tab opened ungrouped: ${tabGroupingFailureMessage(error)}`;
        }
      }
      const summary = await tabSummary(resultTab);
      if (groupWarning) summary.groupWarning = groupWarning;
      if (webmcpArmed) summary.webmcpArmed = true;
      send({ id: message.id, ok: true, result: summary });
      return;
    }
    if (message.type === "focus_tab") {
      const tabId = Number(message.params?.tabId);
      const raiseWindow = message.params?.raiseWindow === true;
      const before = await chrome.tabs.get(tabId).catch(() => null);
      if (raiseWindow && before?.windowId) await chrome.windows.update(before.windowId, { focused: true });
      if (typeof before?.groupId === "number" && before.groupId >= 0) {
        await chrome.tabGroups.update(before.groupId, { collapsed: false }).catch(() => {});
      }
      const tab = await chrome.tabs.update(tabId, { active: true });
      state.activeTabId = tabId;
      state.agentTabId = tabId;
      send({ id: message.id, ok: true, result: await tabSummary(tab) });
      return;
    }
    if (message.type === "close_tab") {
      const tabId = Number(message.params?.tabId);
      const tab = await chrome.tabs.get(tabId);
      const needsPageClose = !tab.discarded && isAgentDrivableUrl(tab.url);
      if (!needsPageClose) {
        await chrome.tabs.remove(tabId);
      } else {
        let closedWhilePending = false;
        try {
          await promiseWithin(
            attach(tabId, { skipRevive: true, requirePageEvents: true }),
            CLOSE_TAB_BUDGET_MS,
            `Page.enable timed out after ${CLOSE_TAB_BUDGET_MS}ms`
          );
        } catch (error) {
          const current = await chrome.tabs.get(tabId).catch(() => null);
          if (!current?.pendingUrl) throw error;
          forceDetach(tabId).catch(() => {});
          chrome.tabs.remove(tabId).catch(() => {});
          if (!(await waitForTabGone(tabId, CLOSE_TAB_SETTLE_MS))) throw error;
          closedWhilePending = true;
        }
        if (!closedWhilePending) {
          markActing(tabId);
          const closeDeadline = Date.now() + CLOSE_TAB_BUDGET_MS;
          let closeError = null;
          try {
            await promiseWithin(
              chrome.debugger.sendCommand({ tabId }, "Page.close", {}),
              CLOSE_TAB_BUDGET_MS,
              `Page.close timed out after ${CLOSE_TAB_BUDGET_MS}ms`
            );
          } catch (error) {
            closeError = error;
          }
          const remaining = Math.max(0, closeDeadline - Date.now());
          const closeAccepted = !closeError || isDetachedDebuggerError(closeError);
          const gone = (await waitForTabGone(tabId, remaining)) ||
            (closeAccepted && (await waitForTabGone(tabId, CLOSE_TAB_SETTLE_MS)));
          if (!gone) {
            forceDetach(tabId).catch(() => {});
            if (closeError && !isDetachedDebuggerError(closeError)) throw closeError;
            const detail = closeError ? `: ${String(closeError?.message || closeError)}` : "";
            const waited = closeAccepted ? CLOSE_TAB_BUDGET_MS + CLOSE_TAB_SETTLE_MS : CLOSE_TAB_BUDGET_MS;
            throw new Error(`tab ${tabId} did not close within ${waited}ms${detail}`);
          }
        }
      }
      if (!(await waitForTabGone(tabId, 2000))) {
        throw new Error(`tab ${tabId} did not close within 2000ms`);
      }
      send({ id: message.id, ok: true, result: { closed: tabId } });
      return;
    }
    if (message.type === "resize_window") {
      const tabId = Number(message.params?.tabId);
      const tab = await chrome.tabs.get(tabId).catch(() => null);
      if (!tab || typeof tab.windowId !== "number") {
        send({ id: message.id, ok: false, error: "could not resolve a window for the tab" });
        return;
      }
      const state = String(message.params?.state || "").trim().toLowerCase();
      const geometry = {};
      for (const key of ["width", "height", "left", "top"]) {
        const value = message.params?.[key];
        if (value !== undefined && value !== null) geometry[key] = Math.round(Number(value));
      }
      const wantsGeometry = Object.keys(geometry).length > 0;
      const current = await chrome.windows.get(tab.windowId);
      const priorState = current.state;
      if (wantsGeometry && priorState !== "normal") {
        await chrome.windows.update(tab.windowId, { state: "normal" });
      }
      if (wantsGeometry) {
        await chrome.windows.update(tab.windowId, geometry);
      }
      if (state && (state !== "normal" || !wantsGeometry)) {
        await chrome.windows.update(tab.windowId, { state });
      } else if (wantsGeometry && !state && priorState !== "normal") {
        await chrome.windows.update(tab.windowId, { state: priorState });
      }
      const applied = await chrome.windows.get(tab.windowId);
      send({
        id: message.id,
        ok: true,
        result: {
          window_id: applied.id,
          width: applied.width,
          height: applied.height,
          left: applied.left,
          top: applied.top,
          state: applied.state,
        },
      });
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
        let liveUrl = null;
        try { liveUrl = (await chrome.tabs.get(tabId))?.url ?? null; } catch (_) {}
        if (cached.url != null && liveUrl != null && liveUrl !== cached.url) {
          state.snapshotCache.delete(tabId);
          state.observerInjected.delete(tabId);
          send({ id: message.id, ok: true, result: { cached: false } });
          return;
        }
        let pageDirty = false;
        try {
          await attach(tabId);
          const evalResult = await chrome.debugger.sendCommand(
            { tabId },
            "Runtime.evaluate",
            { expression: "!!window.__brwDirty", returnByValue: true }
          );
          pageDirty = Boolean(evalResult?.result?.value);
        } catch (_) {
          pageDirty = true;
        }
        if (!pageDirty && !cached.dirty) {
          send({ id: message.id, ok: true, result: { cached: true, snapshot: cached.snapshot } });
          return;
        }
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
	  const forced = await forceHoverAt(tabId, x, y).catch(() => 0);
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
	if (message.type === "capture_screenshot" || message.type === "capture_presentation") {
	  const tabId = Number(message.params?.tabId || (await activeTabId()));
	  const params = { ...(message.params?.params || {}), fromSurface: true };
	  if (message.type === "capture_presentation") params.presentation = true;
	  const result = await captureScreenshotForTab(tabId, params);
	  send({ id: message.id, ok: true, result });
	  return;
	}
	if (message.type === "cdp") {
      const method = message.params?.method;
      if (isDeniedCdpMethod(method)) {
        send({ id: message.id, ok: false, error: `cdp method ${method} is blocked by brw policy: cookie and storage access are not permitted` });
        return;
      }
      const tabId = Number(message.params?.tabId || (await activeTabId()));
      const result = await runCdpForTab(tabId, method, message.params?.params || {});
      send({ id: message.id, ok: true, result: result || {} });
      return;
    }
    if (message.type === "set_intercept_file_chooser") {
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
      const tabId = Number(message.params?.tabId || (await activeTabId()));
      const ev = state.fileChooserEvents.get(tabId);
      if (ev) state.fileChooserEvents.delete(tabId);
      send({ id: message.id, ok: true, result: ev ? { captured: true, ...ev } : { captured: false } });
      return;
    }
    if (message.type === "set_containment") {
      const allowed = Array.isArray(message.params?.allowed) ? message.params.allowed.map(String) : [];
      const blocked = Array.isArray(message.params?.blocked) ? message.params.blocked.map(String) : [];
      const enabled = message.params?.enabled === true;
      state.containment = { allowed, blocked, enabled };
      state.containmentGuard = enabled && typeof message.params?.guard === "string" ? message.params.guard : "";
      if (!enabled) {
        send({ id: message.id, ok: true, result: { enabled: false } });
        return;
      }
      const tabId = Number(message.params?.tabId || (await activeTabId()));
      await attach(tabId);
      await rearmContainment(tabId);
      if (state.containmentGuard) {
        await sendDebuggerCommand(tabId, "Runtime.evaluate", {
          expression: state.containmentGuard,
          returnByValue: true
        }).catch(() => {});
      }
      send({ id: message.id, ok: true, result: { enabled: true, tabId } });
      return;
    }
    if (message.type === "set_webmcp") {
      const source = typeof message.params?.source === "string" ? message.params.source : "";
      if (message.params?.enabled !== true || !source) {
        state.webmcpSource = "";
        send({ id: message.id, ok: true, result: { enabled: false } });
        return;
      }
      const tabId = Number(message.params?.tabId || (await activeTabId()));
      const result = await armWebMCP(tabId, source, message.params?.catchUp !== false);
      send({ id: message.id, ok: true, result: { enabled: true, tabId, ...result } });
      return;
    }
    if (message.type === "arm_inline_document") {
      const tabId = Number(message.params?.tabId || (await activeTabId()));
      await armInlineDocument(tabId, typeof message.params?.url === "string" ? message.params.url : "");
      send({ id: message.id, ok: true, result: { armed: true, tabId } });
      return;
    }
    if (message.type === "navigation_outcome") {
      const tabId = Number(message.params?.tabId || (await activeTabId()));
      const outcome = state.navigationOutcomes.get(tabId);
      send({ id: message.id, ok: true, result: outcome ? { known: true, ...outcome } : { known: false } });
      return;
    }
    if (message.type === "disarm_inline_document") {
      const tabId = Number(message.params?.tabId || (await activeTabId()));
      await disarmInlineDocument(tabId);
      send({ id: message.id, ok: true, result: { armed: false, tabId } });
      return;
    }
    if (message.type === "set_routes") {
      const tabId = Number(message.params?.tabId || (await activeTabId()));
      const result = await setTabRouteRules(tabId, Array.isArray(message.params?.rules) ? message.params.rules : []);
      send({ id: message.id, ok: true, result });
      return;
    }
    if (message.type === "get_blocked_requests") {
      const tabId = Number(message.params?.tabId || (await activeTabId()));
      const entries = state.blockedRequests.get(tabId) || [];
      if (message.params?.peek !== true) state.blockedRequests.delete(tabId);
      send({ id: message.id, ok: true, result: { blocked: entries, count: entries.length } });
      return;
    }
    if (message.type === "arm_dialog") {
      const tabId = Number(message.params?.tabId || (await activeTabId()));
      await attach(tabId);
      await sendDebuggerCommand(tabId, "Page.enable", {}).catch(() => {});
      const count = Number(message.params?.count);
      if (message.params?.clear === true) {
        state.dialogArm.delete(tabId);
        send({ id: message.id, ok: true, result: { armed: false, tabId } });
        return;
      }
      const arm = {
        accept: message.params?.accept === true,
        promptText: typeof message.params?.promptText === "string" ? message.params.promptText : undefined,
        remaining: Number.isFinite(count) && count > 0 ? Math.min(count, 50) : 1,
        armedAt: Date.now()
      };
      state.dialogArm.set(tabId, arm);
      send({ id: message.id, ok: true, result: { armed: true, tabId, accept: arm.accept, remaining: arm.remaining } });
      return;
    }
    if (message.type === "get_dialogs") {
      const tabId = Number(message.params?.tabId || (await activeTabId()));
      const entries = state.dialogLog.get(tabId) || [];
      if (message.params?.peek !== true) state.dialogLog.delete(tabId);
      const arm = state.dialogArm.get(tabId);
      send({
        id: message.id,
        ok: true,
        result: {
          dialogs: entries,
          count: entries.length,
          armed: arm ? { accept: arm.accept, remaining: arm.remaining, prompt_text: arm.promptText || "" } : null
        }
      });
      return;
    }
    if (message.type === "get_downloads") {
      if (!chrome.downloads || !chrome.downloads.search) {
        send({ id: message.id, ok: true, result: { downloads: [], count: 0, supported: false, note: "chrome.downloads API unavailable in this Chrome/extension build" } });
        return;
      }
      const ids = Array.from(state.downloads.entries())
        .filter(([, entry]) => entry.state === "inProgress")
        .slice(-20)
        .map(([guid]) => guid);
      await Promise.all(ids.map(async (guid) => {
        try {
          const found = await chrome.downloads.search({ id: Number(guid) });
          if (found && found[0]) recordDownload(found[0]);
        } catch (_) {}
      }));
      const downloads = downloadSnapshot();
      send({ id: message.id, ok: true, result: { downloads, count: downloads.length, supported: true } });
      return;
    }
    if (message.type === "read_cross_origin_frames") {
      const tabId = Number(message.params?.tabId || (await activeTabId()));
      const origins = Array.isArray(message.params?.origins) ? message.params.origins : null;
      const expression = String(message.params?.expression || "");
      if (origins && !expression) {
        send({ id: message.id, ok: false, error: "read_cross_origin_frames needs an expression to run inside each frame" });
        return;
      }
      const frames = await readCrossOriginFrames(tabId, expression, origins).catch(() => []);
      const skipped = Number(frames.skippedExtensionFrames || 0);
      send({ id: message.id, ok: true, result: skipped ? { frames, skippedExtensionFrames: skipped } : { frames } });
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
      const result = await createNotification(message.params || {});
      send({ id: message.id, ok: true, result });
      return;
    }
    send({ id: message.id, ok: false, error: `unknown message type ${message.type}` });
  } catch (error) {
    state.lastError = `request failed: ${String(error?.message || error)}`;
    if (isBridgeLive()) {
      noteRequestFault(state.lastError).catch(() => {});
    } else {
      markBridgeStatus("error", state.lastError).catch(() => {});
    }
    send({ id: message.id, ok: false, error: String(error?.message || error) });
  }
}

async function readCrossOriginFrames(tabId, expression, origins) {
  const allowed = origins ? new Set(origins.map((o) => String(o))) : null;
  try {
    await attach(tabId);
  } catch (error) {
    if (error?.code !== FOREIGN_EXTENSION_FRAME) throw error;
    const frames = await readCrossOriginFramesWithoutDebugger(tabId, expression, origins);
    frames.skippedExtensionFrames = (error.blockingFrames || []).length;
    return frames;
  }
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
        wanted.push({ id: child.frame?.id, url, origin });
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
    const tgt = iframeTargets.find((t) => !usedTargets.has(t.id) && t.id === w.id && t.url === w.url);
    if (!tgt) {
      out.push({ url: w.url, origin: w.origin });
      continue;
    }
    usedTargets.add(tgt.id);
    if (!allowed || !allowed.has(w.origin)) {
      out.push({ url: w.url, origin: w.origin });
      continue;
    }
    const snapshot = await evaluateInFrameTarget(tabId, tgt.id, frameReadExpression(expression, w.origin)).catch(() => null);
    out.push({ url: w.url, origin: w.origin, snapshot });
  }
  return out;
}

async function evaluateInFrameTarget(tabId, targetId, expression) {
  let owned = false;
  try {
    try {
      await chrome.debugger.attach({ targetId }, "1.3");
      owned = true;
    } catch (error) {
      if (!String(error?.message || error).includes("Another debugger is already attached")) throw error;
      owned = false;
    }
    const res = await sendPolicedCdp(tabId, { targetId }, "Runtime.evaluate", {
      expression,
      returnByValue: true,
      awaitPromise: true
    });
    const value = res?.result?.value;
    return value && typeof value === "object" ? value : null;
  } finally {
    if (owned) {
      try { await chrome.debugger.detach({ targetId }); } catch (_) {}
    }
  }
}

function frameReadExpression(expression, origin) {
  const expected = JSON.stringify(origin);
  return `(async () => {if (globalThis.location.origin !== ${expected}) throw new Error("frame origin changed"); const value = await (${expression}); if (globalThis.location.origin !== ${expected}) throw new Error("frame origin changed"); return value;})()`;
}

async function ensureTabDrivable(tabId) {
  let tab = await chrome.tabs.get(tabId).catch(() => null);
  if (!tab) throw new Error(`cannot find tab ${tabId}`);
  if (tab.discarded) {
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
    await enqueueTabJuggle(async () => {
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

async function waitForTabLoad(tabId, deadlineMs) {
  const deadline = Date.now() + deadlineMs;
  while (Date.now() < deadline) {
    const tab = await chrome.tabs.get(tabId).catch(() => null);
    if (!tab) throw new Error(`cannot find tab ${tabId}`);
    if (!tab.discarded && tab.status === "complete") return;
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
}

async function waitForTabGone(tabId, deadlineMs) {
  const deadline = Date.now() + deadlineMs;
  while (Date.now() < deadline) {
    const tab = await chrome.tabs.get(tabId).catch(() => null);
    if (!tab) return true;
    await new Promise((resolve) => setTimeout(resolve, 25));
  }
  return !(await chrome.tabs.get(tabId).catch(() => null));
}

async function promiseWithin(promise, timeoutMs, timeoutMessage) {
  let timer = null;
  try {
    return await Promise.race([
      Promise.resolve(promise),
      new Promise((_, reject) => {
        timer = setTimeout(() => reject(new Error(timeoutMessage)), Math.max(0, timeoutMs));
      })
    ]);
  } finally {
    if (timer != null) clearTimeout(timer);
  }
}

async function attach(tabId, opts = {}) {
  if (!opts.skipRevive) await ensureTabDrivable(tabId);
  if (state.attachedTabs.has(tabId)) {
    state.attachUsedAt.set(tabId, Date.now());
    if (opts.requirePageEvents) {
      try {
        await chrome.debugger.sendCommand({ tabId }, "Page.enable", {});
      } catch (error) {
        if (!isForeignExtensionRefusal(error)) throw error;
        state.attachedTabs.delete(tabId);
        throw await foreignExtensionFrameError(tabId, error);
      }
    }
    return;
  }
  try {
    await chrome.debugger.attach({ tabId }, "1.3");
  } catch (error) {
    if (isForeignExtensionRefusal(error)) throw await foreignExtensionFrameError(tabId, error);
    if (!String(error?.message || error).includes("Another debugger is already attached")) throw error;
    try {
      await chrome.debugger.sendCommand({ tabId }, "Runtime.evaluate", { expression: "0", returnByValue: true });
    } catch (_) {
      throw new Error(`cannot control tab ${tabId}: another debugger (likely DevTools) is already attached; close DevTools on that tab and retry`);
    }
  }
  state.attachedTabs.add(tabId);
  state.attachUsedAt.set(tabId, Date.now());
	try {
	  const pageEvents = chrome.debugger.sendCommand({ tabId }, "Page.enable", {});
	  await Promise.all([
	    opts.requirePageEvents ? pageEvents : pageEvents.catch(() => {}),
	    chrome.debugger.sendCommand({ tabId }, "Runtime.enable", {}).catch(() => {}),
	    chrome.debugger.sendCommand({ tabId }, "Emulation.setFocusEmulationEnabled", { enabled: true }).catch(() => {})
	  ]);
	  for (const [method, params] of state.deviceEmulationOverrides.get(tabId) || []) {
	    await chrome.debugger.sendCommand({ tabId }, method, params);
	  }
	} catch (error) {
	  await detach(tabId).catch(() => {});
	  throw new Error(`cannot safely arm Page events for tab ${tabId}: ${String(error?.message || error)}`);
	}
	await rearmContainment(tabId).catch(() => {});
	await registerWebMCP(tabId).catch(() => {});
}

async function rearmContainment(tabId) {
  if (!state.containment.enabled || state.containmentTabs.has(tabId) || !state.attachedTabs.has(tabId)) return false;
  state.containmentTabs.add(tabId);
  await syncFetchInterception(tabId);
  if (state.containmentGuard) {
    await chrome.debugger.sendCommand({ tabId }, "Page.addScriptToEvaluateOnNewDocument", { source: state.containmentGuard })
      .catch(() => {});
  }
  return true;
}

async function registerWebMCP(tabId) {
  const source = state.webmcpSource;
  if (!source || state.webmcpTabs.has(tabId) || !state.attachedTabs.has(tabId)) return false;
  await chrome.debugger.sendCommand({ tabId }, "Page.addScriptToEvaluateOnNewDocument", { source });
  state.webmcpTabs.add(tabId);
  return true;
}

async function armWebMCP(tabId, source, catchUp) {
  state.webmcpSource = source;
  await attach(tabId);
  await registerWebMCP(tabId);
  let installed = false;
  if (catchUp) {
    await sendDebuggerCommand(tabId, "Runtime.evaluate", { expression: source, returnByValue: true })
      .then(() => { installed = true; })
      .catch(() => {});
  }
  return { armed: state.webmcpTabs.has(tabId), installed };
}

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
    }
  }
}

async function detach(tabId) {
  await clearForcedHover(tabId).catch(() => {});
  state.attachUsedAt.delete(tabId);
  if (!state.attachedTabs.has(tabId)) return;
  state.attachedTabs.delete(tabId);
  state.observerInjected.delete(tabId);
  state.fileChooserEvents.delete(tabId);
  state.containmentTabs.delete(tabId);
  state.webmcpTabs.delete(tabId);
  state.inlineDocumentTabs.delete(tabId);
  try {
    await chrome.debugger.detach({ tabId });
  } catch (_) {
  }
}

async function forceDetach(tabId) {
  if (state.forcedHoverTimers.has(tabId)) clearTimeout(state.forcedHoverTimers.get(tabId));
  state.forcedHoverTimers.delete(tabId);
  state.forcedHoverNodes.delete(tabId);
  state.attachedTabs.delete(tabId);
  state.attachUsedAt.delete(tabId);
  state.observerInjected.delete(tabId);
  state.fileChooserEvents.delete(tabId);
  state.containmentTabs.delete(tabId);
  state.webmcpTabs.delete(tabId);
  state.inlineDocumentTabs.delete(tabId);
  try { await chrome.debugger.detach({ tabId }); } catch (_) {}
}

async function detachAll() {
  for (const tabId of Array.from(state.attachedTabs)) {
    await detach(tabId);
  }
}

async function sweepIdleDebuggers() {
  const now = Date.now();
  for (const tabId of Array.from(state.attachedTabs)) {
    const usedAt = state.attachUsedAt.get(tabId) || 0;
    if (now - usedAt > IDLE_DETACH_MS) await detach(tabId);
  }
}

async function sendDebuggerCommand(tabId, method, params) {
  state.attachUsedAt.set(tabId, Date.now());
  queueAgentActivity(tabId);
  try {
    return await chrome.debugger.sendCommand({ tabId }, method, params);
  } catch (error) {
    if (isForeignExtensionRefusal(error)) {
      state.attachedTabs.delete(tabId);
      throw await foreignExtensionFrameError(tabId, error);
    }
    if (!isDetachedDebuggerError(error) || method === "Input.dispatchTouchEvent" || (method === "Input.dispatchMouseEvent" && ["mousePressed", "mouseReleased"].includes(params?.type))) throw error;
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

async function forceHoverAt(tabId, x, y) {
  await clearForcedHover(tabId);
  await Promise.all([
    sendDebuggerCommand(tabId, "DOM.enable", {}).catch(() => {}),
    sendDebuggerCommand(tabId, "CSS.enable", {}).catch(() => {})
  ]);
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
    if (params.presentation) return await capturePresentation(tabId, params);
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
          timer = setTimeout(() => reject(new Error("screenshot compositor capture timed out")), 1000);
        })
      ]);
    } catch (error) {
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

async function capturePresentation(tabId, params) {
  await attach(tabId, { skipRevive: true });
  markActing(tabId);
  const bounded = async (promise, milliseconds, label) => {
    let timer;
    try {
      return await Promise.race([
        promise,
        new Promise((_, reject) => {
          timer = setTimeout(() => reject(new Error(label + " timed out; unlock and expose the browser window")), milliseconds);
        })
      ]);
    } finally {
      if (timer) clearTimeout(timer);
    }
  };
  const evaluate = async (expression, timeout) => {
    const result = await bounded(sendDebuggerCommand(tabId, "Runtime.evaluate", {
      expression, awaitPromise: true, returnByValue: true
    }), timeout, "presentation preparation/restoration");
    if (result?.exceptionDetails) throw new Error(result.exceptionDetails.exception?.description || result.exceptionDetails.text);
    return result?.result?.value;
  };
  let failure;
  let result;
  try {
    const plan = await evaluate(params.prepare, 18000);
    if (!plan?.clip) throw new Error("capture preparation returned no clip");
    if (params.omitBackground) {
      await sendDebuggerCommand(tabId, "Emulation.setDefaultBackgroundColorOverride", { color: { r: 0, g: 0, b: 0, a: 0 } });
    }
    const capture = { format: plan.crop ? "png" : params.format, clip: plan.clip, fromSurface: true, captureBeyondViewport: true };
    if (params.format !== "png" && !plan.crop) capture.quality = params.quality;
    result = await bounded(sendDebuggerCommand(tabId, "Page.captureScreenshot", capture), 10000, "presentation compositor capture");
    if (plan.crop) {
      const tree = await sendDebuggerCommand(tabId, "Page.getFrameTree", {});
      const world = await sendDebuggerCommand(tabId, "Page.createIsolatedWorld", { frameId: tree.frameTree.frame.id, worldName: "brw screenshot encoding" });
      const encoded = await bounded(sendDebuggerCommand(tabId, "Runtime.evaluate", {
        expression: params.encode + ".apply(null," + JSON.stringify([result.data, plan.crop, params.format, params.quality]) + ")",
        contextId: world.executionContextId, awaitPromise: true, returnByValue: true
      }), 10000, "presentation bitmap encoding");
      if (encoded?.exceptionDetails) throw new Error(encoded.exceptionDetails.exception?.description || encoded.exceptionDetails.text);
      result.data = encoded.result.value;
    }
    result.width = plan.width;
    result.height = plan.height;
  } catch (error) {
    failure = error;
  } finally {
    try { await evaluate(params.cleanup, 3000); } catch (error) { failure ||= error; }
    if (params.omitBackground) {
      try {
        await bounded(sendDebuggerCommand(tabId, "Emulation.setDefaultBackgroundColorOverride", {}), 3000, "background restoration");
      } catch (error) { failure ||= error; }
    }
  }
  if (failure) throw failure;
  return result;
}

function isDetachedDebuggerError(error) {
  const message = String(error?.message || error || "").toLowerCase();
  return message.includes("detached while handling command") ||
    message.includes("debugger is not attached") ||
    message.includes("target closed");
}

function isControllableWindowType(win) {
  return !win || (win.type !== "app" && win.type !== "devtools");
}

function isAgentDrivableUrl(url) {
  const raw = String(url || "").trim();
  if (!raw) return true;
  const lower = raw.toLowerCase();
  if (lower === "about:blank" || lower.startsWith("about:blank?") || lower.startsWith("about:blank#")) return true;
  if (/^(chrome|chrome-search|chrome-untrusted|chrome-native|devtools|edge|brave|vivaldi|opera|about|view-source):/.test(lower)) return false;
  if (lower.startsWith("chrome-extension://")) {
    const ownID = String(chrome.runtime?.id || "").toLowerCase();
    return Boolean(ownID) && lower.startsWith(`chrome-extension://${ownID}/`);
  }
  if (/^https?:\/\/chromewebstore\.google\.com(\/|$)/.test(lower)) return false;
  if (/^https?:\/\/chrome\.google\.com\/webstore(\/|$)/.test(lower)) return false;
  return true;
}

const FOREIGN_EXTENSION_FRAME = "foreign_extension_frame";

function isForeignExtensionRefusal(error) {
  return String(error?.message || error || "").includes("Cannot access a chrome-extension:// URL of different extension");
}

function isForeignExtensionUrl(url) {
  const lower = String(url || "").toLowerCase();
  if (!lower.startsWith("chrome-extension://")) return false;
  const ownID = String(chrome.runtime?.id || "").toLowerCase();
  return !ownID || !lower.startsWith(`chrome-extension://${ownID}/`);
}

function extensionIdOf(url) {
  const match = /^chrome-extension:\/\/([^/?#]+)/i.exec(String(url || ""));
  return match ? match[1] : "";
}

function noteSubframeCommit(tabId, frameId, url) {
  let frames = state.foreignExtensionFrames.get(tabId);
  if (isForeignExtensionUrl(url)) {
    if (!frames) {
      frames = new Map();
      state.foreignExtensionFrames.set(tabId, frames);
    }
    frames.set(frameId, String(url));
    return;
  }
  if (!frames) return;
  frames.delete(frameId);
  if (!frames.size) state.foreignExtensionFrames.delete(tabId);
}

async function foreignExtensionFramesIn(tabId) {
  const recorded = Array.from(state.foreignExtensionFrames.get(tabId)?.entries() || [])
    .map(([frameId, url]) => ({ frame_id: frameId, url, extension_id: extensionIdOf(url) }));
  if (!recorded.length) return recorded;
  let live = null;
  try {
    const targets = await chrome.debugger.getTargets();
    live = new Set((targets || []).map((target) => String(target?.url || "")).filter(isForeignExtensionUrl));
  } catch (_) {
    return recorded;
  }
  const present = recorded.filter((frame) => live.has(frame.url));
  return present.length ? present : recorded;
}

async function foreignExtensionFrameError(tabId, cause) {
  const tab = await chrome.tabs.get(tabId).catch(() => null);
  if (isForeignExtensionUrl(tab?.url)) return cause?.message ? cause : new Error(String(cause));
  const frames = await foreignExtensionFramesIn(tabId).catch(() => []);
  const which = frames.length
    ? `${frames.length === 1 ? "a frame" : `${frames.length} frames`} from another extension (${frames.map((frame) => `extension ${frame.extension_id} at ${frame.url}`).join("; ")})`
    : "a frame from another extension";
  const error = new Error(
    `${FOREIGN_EXTENSION_FRAME}: Chrome refuses brw's debugger for tab ${tabId} while the page embeds ${which}. ` +
    "A password manager or autofill menu is the usual source; the tab is drivable again once that frame closes " +
    "(dismiss the menu or move focus off the field) or once that extension's inline menu is turned off for the site. " +
    `Chrome said: ${String(cause?.message || cause)}`
  );
  error.code = FOREIGN_EXTENSION_FRAME;
  error.blockingFrames = frames;
  return error;
}

async function scriptingEvaluate(expression, awaitPromise) {
  try {
    let value = (0, eval)(expression);
    if (awaitPromise && value && typeof value.then === "function") value = await value;
    if (value === undefined) return { type: "undefined" };
    let copy = null;
    try {
      const encoded = JSON.stringify(value);
      copy = encoded === undefined ? null : JSON.parse(encoded);
    } catch (_) {}
    return { type: value === null ? "object" : typeof value, value: copy };
  } catch (error) {
    const description = error && (error.stack || error.message) ? String(error.stack || error.message) : String(error);
    return { exception: description };
  }
}

function scriptingInsertText(text) {
  let el = document.activeElement;
  while (el && el.shadowRoot && el.shadowRoot.activeElement) el = el.shadowRoot.activeElement;
  if (!el || el === document.body) return { ok: false, error: "no focused element to insert text into" };
  if (typeof document.execCommand === "function" && document.execCommand("insertText", false, text)) return { ok: true };
  if ("value" in el) {
    el.value = String(el.value || "") + text;
    el.dispatchEvent(new InputEvent("input", { bubbles: true, inputType: "insertText", data: text }));
    el.dispatchEvent(new Event("change", { bubbles: true }));
    return { ok: true };
  }
  return { ok: false, error: "focused element does not accept text" };
}

function runtimeEvaluateResultFromScripting(out) {
  if (out && out.exception != null) {
    const exception = { type: "object", subtype: "error", description: String(out.exception) };
    return {
      result: exception,
      exceptionDetails: { text: "Uncaught", lineNumber: 0, columnNumber: 0, exception }
    };
  }
  const type = out?.type || "undefined";
  return type === "undefined" ? { result: { type } } : { result: { type, value: out.value } };
}

async function cdpWithoutDebugger(tabId, method, params, refusal) {
  if (!chrome.scripting?.executeScript) return null;
  let func;
  let args;
  if (method === "Runtime.evaluate") {
    func = scriptingEvaluate;
    args = [String(params?.expression || ""), params?.awaitPromise === true];
  } else if (method === "Input.insertText") {
    func = scriptingInsertText;
    args = [String(params?.text || "")];
  } else {
    return null;
  }
  let injected;
  try {
    injected = await chrome.scripting.executeScript({ target: { tabId, frameIds: [0] }, world: "MAIN", func, args });
  } catch (error) {
    refusal.message += ` No fallback: chrome.scripting could not reach the page either (${String(error?.message || error)}).`;
    throw refusal;
  }
  const out = injected?.[0]?.result;
  const transport = {
    path: "scripting",
    frame_id: 0,
    skipped_extension_frames: (refusal.blockingFrames || []).length,
    blocked_by: refusal.blockingFrames || []
  };
  if (method === "Input.insertText") {
    if (!out?.ok) throw new Error(`Input.insertText via chrome.scripting: ${out?.error || "insert failed"}`);
    return { brwTransport: transport };
  }
  return { ...runtimeEvaluateResultFromScripting(out), brwTransport: transport };
}

async function runCdpForTab(tabId, method, params) {
  try {
    await attach(tabId);
    return await sendPolicedCdp(tabId, null, method, params);
  } catch (error) {
    if (error?.code !== FOREIGN_EXTENSION_FRAME) throw error;
    markActing(tabId);
    const answered = await cdpWithoutDebugger(tabId, method, params, error);
    if (answered === null) throw error;
    return answered;
  }
}

async function readCrossOriginFramesWithoutDebugger(tabId, expression, origins) {
  if (!chrome.scripting?.executeScript) return [];
  const allowed = origins ? new Set(origins.map((o) => String(o))) : null;
  let frames = [];
  try { frames = await chrome.webNavigation.getAllFrames({ tabId }); } catch (_) { return []; }
  const top = (frames || []).find((frame) => frame.frameId === 0);
  let topOrigin = "";
  try { topOrigin = new URL(top?.url || "").origin; } catch (_) {}
  const out = [];
  for (const frame of frames || []) {
    if (!frame || frame.frameId === 0 || isForeignExtensionUrl(frame.url)) continue;
    let origin = "";
    try { origin = new URL(frame.url).origin; } catch (_) {}
    if (!/^https?:/i.test(frame.url || "") || !origin || origin === topOrigin) continue;
    if (!allowed || !allowed.has(origin)) {
      out.push({ url: frame.url, origin });
      continue;
    }
    let snapshot = null;
    try {
      const injected = await chrome.scripting.executeScript({
        target: { tabId, frameIds: [frame.frameId] },
        world: "MAIN",
        func: scriptingEvaluate,
        args: [frameReadExpression(expression, origin), true]
      });
      const value = injected?.[0]?.result?.value;
      snapshot = value && typeof value === "object" ? value : null;
    } catch (_) {}
    out.push({ url: frame.url, origin, snapshot });
  }
  return out;
}

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
  if (state.agentTabId) {
    const pinned = await chrome.tabs.get(state.agentTabId).catch(() => null);
    if (pinned?.id) {
      const win = await chrome.windows.get(pinned.windowId).catch(() => null);
      if (!isControllableWindowType(win)) {
        state.agentTabId = null;
      } else if (isAgentDrivableUrl(pinned.url)) {
        return pinned.id;
      }
      throw new Error(
        `no drivable tab: agent-pinned tab ${pinned.id} is ${String(pinned.url || "a browser-internal page").split("?")[0]}`
      );
    } else {
      state.agentTabId = null;
    }
  }
  const windows = await chrome.windows.getAll({
    populate: true,
    windowTypes: ["normal", "popup", "panel", "app", "devtools"]
  }).catch(() => []);
  for (const win of windows) {
    if (!win.focused || !isControllableWindowType(win)) continue;
    const tab = (win.tabs || []).find((candidate) => candidate.active);
    if (tab?.id && isAgentDrivableUrl(tab.url)) return tab.id;
  }
  const lastFocused = await chrome.tabs.query({ active: true, lastFocusedWindow: true }).catch(() => []);
  for (const tab of lastFocused) {
    if (!tab?.id || !isAgentDrivableUrl(tab.url)) continue;
    const win = await chrome.windows.get(tab.windowId).catch(() => null);
    if (isControllableWindowType(win)) return tab.id;
  }
  if (state.activeTabId) {
    const cached = await chrome.tabs.get(state.activeTabId).catch(() => null);
    if (cached?.id) {
      const win = await chrome.windows.get(cached.windowId).catch(() => null);
      if (!isControllableWindowType(win)) {
        state.activeTabId = null;
      } else if (isAgentDrivableUrl(cached.url)) {
        return cached.id;
      }
    }
  }
  const any = await chrome.tabs.query({ active: true }).catch(() => []);
  for (const tab of any) {
    if (!tab?.id || !isAgentDrivableUrl(tab.url)) continue;
    const win = await chrome.windows.get(tab.windowId).catch(() => null);
    if (isControllableWindowType(win)) return tab.id;
  }
  return null;
}

async function activeTabId() {
  const id = await resolveForegroundTabId();
  if (id) {
    state.activeTabId = id;
    return id;
  }
  state.activeTabId = null;
  const blocked = (await chrome.tabs.query({ active: true }).catch(() => []))
    .filter((tab) => !isAgentDrivableUrl(tab?.url));
  if (blocked.length) {
    const where = String(blocked[0].url || "").split("?")[0] || "a browser-internal page";
    throw new Error(
      `no drivable tab: the active tab is ${where}, which Chrome does not allow brw to control. ` +
      "Switch to a normal page tab, or pass an explicit tab_id."
    );
  }
  throw new Error("no active tab");
}

async function listTabSummaries() {
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
  const foregroundId = await resolveForegroundTabId().catch(() => null);
  const out = [];
  for (const tab of allTabs) {
    const win = await getWin(tab.windowId);
    if (!isControllableWindowType(win)) continue;
    let fresh = tab;
    if (typeof tab.id === "number") {
      const got = await chrome.tabs.get(tab.id).catch(() => null);
      if (got) fresh = got;
    }
    const summary = await tabSummaryFrom(fresh, win, groupsById);
    if (typeof fresh.id === "number") {
      const isForeground = foregroundId != null && fresh.id === foregroundId;
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
  const win = await chrome.windows.get(tab.windowId).catch(() => null);
  if (!isControllableWindowType(win)) return;
  if (!isAgentDrivableUrl(tab.url)) return;
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
    const observer = new MutationObserver(function() {
      window.__brwDirty = true;
    });
    observer.observe(document.documentElement, {
      childList: true,
      subtree: true,
      attributes: true,
      characterData: true
    });
    ['input', 'change'].forEach(function(type) {
      document.addEventListener(type, function() {
        window.__brwDirty = true;
      }, true);
    });
  })()`;
  attach(tabId)
    .then(() => chrome.debugger.sendCommand({ tabId }, "Runtime.evaluate", {
      expression: observerScript,
      returnByValue: true
    }))
    .catch(() => { state.observerInjected.delete(tabId); });
}

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

function bytesToBase64(bytes) {
  const parts = [];
  const blockSize = 0x8000;
  for (let offset = 0; offset < bytes.length; offset += blockSize) {
    parts.push(String.fromCharCode(...bytes.subarray(offset, Math.min(offset + blockSize, bytes.length))));
  }
  return btoa(parts.join(""));
}

function send(payload) {
  const socket = state.socket;
  if (!socket || socket.readyState !== WebSocket.OPEN) return false;
  try {
    const serialized = JSON.stringify(payload);
    if (serialized.length <= Math.floor(RESPONSE_DIRECT_MAX_BYTES / 3)) {
      socket.send(serialized);
      return true;
    }

    const encoded = new TextEncoder().encode(serialized);
    if (encoded.length <= RESPONSE_DIRECT_MAX_BYTES) {
      socket.send(serialized);
      return true;
    }
    if (typeof payload?.id !== "string" || payload.id === "") {
      state.lastError = "oversized uncorrelated bridge message was not sent";
      return false;
    }
    if (encoded.length > RESPONSE_TOTAL_MAX_BYTES) {
      socket.send(JSON.stringify({
        id: payload.id,
        ok: false,
        error: `BRW_EXTENSION_RESPONSE_TOO_LARGE: serialized response exceeds ${RESPONSE_TOTAL_MAX_BYTES}-byte bridge transfer limit`
      }));
      return true;
    }

    const chunkCount = Math.ceil(encoded.length / RESPONSE_CHUNK_BYTES);
    for (let chunkIndex = 0; chunkIndex < chunkCount; chunkIndex++) {
      const start = chunkIndex * RESPONSE_CHUNK_BYTES;
      const end = Math.min(start + RESPONSE_CHUNK_BYTES, encoded.length);
      socket.send(JSON.stringify({
        type: "response_chunk",
        id: payload.id,
        encoding: "base64",
        chunk_index: chunkIndex,
        chunk_count: chunkCount,
        total_bytes: encoded.length,
        data: bytesToBase64(encoded.subarray(start, end))
      }));
    }
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

function consentURL(config, path) {
  const url = new URL(config.statusUrl);
  url.pathname = path;
  url.search = "";
  return url.toString();
}

async function fetchSiteConsent() {
  const config = await loadBridgeConfig();
  const response = await fetch(consentURL(config, "/consent"), {
    cache: "no-store",
    signal: AbortSignal.timeout(DAEMON_STATUS_TIMEOUT_MS)
  });
  if (!response.ok) throw new Error(`the daemon consent endpoint answered HTTP ${response.status}`);
  return response.json();
}

async function revokeSiteConsent(request) {
  const config = await loadBridgeConfig();
  const body = request.all ? { all: true } : { origin: String(request.origin || "") };
  if (!request.all && request.scope) body.scope = String(request.scope);
  const response = await fetch(consentURL(config, "/consent/revoke"), {
    method: "POST",
    cache: "no-store",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
    signal: AbortSignal.timeout(DAEMON_STATUS_TIMEOUT_MS)
  });
  const result = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(result?.error || `the daemon refused the revocation (HTTP ${response.status})`);
  return result;
}

async function fetchBridgeToken(config) {
  try {
    const response = await fetch(config.statusUrl, { cache: "no-store", signal: AbortSignal.timeout(DAEMON_STATUS_TIMEOUT_MS) });
    if (!response.ok) {
      return { token: "", reachable: false, detail: `the daemon status endpoint answered HTTP ${response.status}` };
    }
    const status = await response.json().catch(() => ({}));
    const token = typeof status?.token === "string" ? status.token : "";
    return { token, reachable: true, detail: token ? "" : "the daemon offered no handshake token" };
  } catch (err) {
    return { token: "", reachable: false, detail: `the daemon status endpoint could not be reached (${err?.message || err})` };
  }
}

async function probeDaemonStatus() {
  if (!isSocketOpen()) return false;
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

function scheduleReconnect(reason, { rejected = false } = {}) {
  stopKeepAlive();
  clearTimeout(state.reconnectTimer);
  const delay = Math.min(1000 * (state.reconnectAttempt + 1), MAX_RECONNECT_DELAY_MS);
  state.reconnectAttempt += 1;
  markBridgeStatus(rejected ? "rejected" : "disconnected", `${reason}; reconnecting in ${delay}ms`).catch(() => {});
  state.reconnectTimer = setTimeout(() => {
    connect({ probe: true });
  }, delay);
}

function isSocketOpen() {
  return Boolean(state.socket && state.socket.readyState === WebSocket.OPEN);
}

function isBridgeLive() {
  return isSocketOpen() && state.acceptedSocket === state.socket;
}

function isSocketConnecting() {
  return Boolean(state.socket && state.socket.readyState === WebSocket.CONNECTING);
}

let badgeMode = "disconnected";
let badgeAnimTimer = null;
let badgeAnimPhase = 0;
let everConnectedThisWorker = false;
let disconnectNotifyTimer = null;
let lastDisconnectNotifyAt = 0;

function isAgentActive() {
  return Date.now() - (state.lastAgentActivityAt || 0) < BADGE_USED_WINDOW_MS;
}

function resolveBadgeMode(status) {
  if (isBridgeLive()) return isAgentActive() ? "used" : "connected";
  if (status === "rejected") return "rejected";
  if (status === "connected") return isAgentActive() ? "used" : "connected";
  if (status === "connecting" || status === "starting" || status === "configured") {
    return "connecting";
  }
  if (status === "consent_required") return "consent";
  return "disconnected";
}

async function noteRequestFault(detail = "") {
  const config = state.bridgeConfig || normalizeBridgeConfig({});
  const status = isBridgeLive() ? "connected" : (state.reportedStatus || "error");
  if (isBridgeLive()) state.reportedStatus = "connected";
  const value = {
    status,
    badge: resolveBadgeMode(status),
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

function setBadgeVisual(text, color, title) {
  chrome.action.setBadgeText({ text }).catch(() => {});
  chrome.action.setBadgeBackgroundColor({ color }).catch(() => {});
  chrome.action.setTitle({ title }).catch(() => {});
  if (typeof chrome.action.setBadgeTextColor === "function") {
    chrome.action.setBadgeTextColor({ color: "#ffffff" }).catch(() => {});
  }
}

function applyBadgeFrame(mode, phase) {
  if (mode === "connected") {
    setBadgeVisual("on", BADGE_IDLE_BG, "brw · Idle");
    return;
  }
  if (mode === "used") {
    if (phase % 2 === 0) {
      setBadgeVisual("act", BADGE_AGENT_PULSE_BG, "brw · Agent active");
    } else {
      setBadgeVisual("act", BADGE_AGENT_BG, "brw · Agent active");
    }
    return;
  }
  if (mode === "connecting") {
    if (phase % 2 === 0) {
      setBadgeVisual("…", BADGE_CONNECTING_BG, "brw · Reconnecting");
    } else {
      setBadgeVisual("···", BADGE_CONNECTING_DIM_BG, "brw · Reconnecting");
    }
    return;
  }
  if (mode === "consent") {
    setBadgeVisual("!", BADGE_CONNECTING_BG, "brw · Browser control not enabled");
    return;
  }
  if (mode === "rejected") {
    setBadgeVisual("!", BADGE_DOWN_BG, "brw · Refused by the daemon — click for status");
    return;
  }
  setBadgeVisual("off", BADGE_DOWN_BG, "brw · Down — click for status");
}

function ensureBadgeAnim(mode) {
  const needsAnim = mode === "connecting" || mode === "used";
  if (!needsAnim) {
    if (badgeAnimTimer) {
      clearInterval(badgeAnimTimer);
      badgeAnimTimer = null;
    }
    return;
  }
  if (badgeAnimTimer) return;
  badgeAnimTimer = setInterval(() => {
    const next = resolveBadgeMode(state.reportedStatus || "disconnected");
    if (next !== badgeMode) {
      badgeMode = next;
      badgeAnimPhase = 0;
      applyBadgeFrame(next, 0);
      if (next !== "connecting" && next !== "used") {
        clearInterval(badgeAnimTimer);
        badgeAnimTimer = null;
      }
      return;
    }
    badgeAnimPhase += 1;
    applyBadgeFrame(badgeMode, badgeAnimPhase);
  }, BADGE_ANIM_MS);
}

function setBridgeBadge(status) {
  state.reportedStatus = status;
  const mode = resolveBadgeMode(status);
  if (mode !== badgeMode) {
    badgeMode = mode;
    badgeAnimPhase = 0;
    applyBadgeFrame(mode, 0);
  } else if (!badgeAnimTimer) {
    applyBadgeFrame(mode, badgeAnimPhase);
  }
  ensureBadgeAnim(mode);
}

function noteConnectionLifecycle(status) {
  if (status === "consent_required") {
    if (disconnectNotifyTimer) {
      clearTimeout(disconnectNotifyTimer);
      disconnectNotifyTimer = null;
    }
    return;
  }
  if (status === "connected") {
    everConnectedThisWorker = true;
    if (disconnectNotifyTimer) {
      clearTimeout(disconnectNotifyTimer);
      disconnectNotifyTimer = null;
    }
    return;
  }
  if (!everConnectedThisWorker) return;
  if (disconnectNotifyTimer) return;
  disconnectNotifyTimer = setTimeout(() => {
    disconnectNotifyTimer = null;
    if (isBridgeLive() || state.reportedStatus === "connected") return;
    const now = Date.now();
    if (now - lastDisconnectNotifyAt < DISCONNECT_NOTIFY_COOLDOWN_MS) return;
    lastDisconnectNotifyAt = now;
    notifyBridgeDisconnected().catch(() => {});
  }, DISCONNECT_NOTIFY_MS);
}

async function notifyBridgeDisconnected() {
  const config = state.bridgeConfig || normalizeBridgeConfig({});
  const label = config.label || config.profile || "this browser";
  const detail = state.lastError || "The local daemon bridge dropped.";
  await createNotification({
    kind: "error",
    title: "brw · Down",
    message: `${label}: ${detail} Click the brw icon to reconnect.`
  });
}

async function markBridgeStatus(status, detail = "") {
  setBridgeBadge(status);
  noteConnectionLifecycle(status);
  const config = state.bridgeConfig || normalizeBridgeConfig({});
  const value = {
    status,
    badge: resolveBadgeMode(status),
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
