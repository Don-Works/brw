const fieldIds = ["bridgeUrl", "statusUrl", "workspace", "profile", "label"];
const form = document.getElementById("config");
const saveButton = document.getElementById("saveConfig");
const refreshButton = document.getElementById("refreshStatus");
const statusBlock = document.getElementById("statusBlock");
const formMessage = document.getElementById("formMessage");
const rawStatus = document.getElementById("rawStatus");
const advanced = document.getElementById("advancedConfig");
let refreshTimer = 0;
let refreshing = false;

form.addEventListener("submit", save);
refreshButton.addEventListener("click", () => refreshStatus({ announce: true }));
document.getElementById("bridgeUrl").addEventListener("change", syncStatusEndpoint);
document.addEventListener("visibilitychange", () => {
  if (!document.hidden) refreshStatus();
});

for (const button of document.querySelectorAll("[data-port]")) {
  button.addEventListener("click", () => applyPort(button.dataset.port));
}

init();

async function init() {
  await refreshStatus({ populate: true });
  refreshTimer = window.setInterval(() => {
    if (!document.hidden) refreshStatus();
  }, 3000);
  window.addEventListener("pagehide", () => window.clearInterval(refreshTimer), { once: true });
}

async function save(event) {
  event.preventDefault();
  clearValidation();
  if (!validateEndpoints()) return;

  saveButton.disabled = true;
  saveButton.textContent = "Saving…";
  form.setAttribute("aria-busy", "true");
  setFormMessage("Saving configuration and reconnecting this profile…");
  try {
    const response = await chrome.runtime.sendMessage({ type: "BRW_CONFIGURE", config: readForm() });
    if (!response?.ok) throw new Error(response?.error || "Configuration failed");
    setFormMessage("Saved. Reconnecting to the selected daemon…", "success");
    updatePresetSelection();
    window.setTimeout(() => refreshStatus({ announce: true }), 350);
  } catch (error) {
    setFormMessage(humanizeError(error), "error");
    advanced.open = true;
  } finally {
    saveButton.disabled = false;
    saveButton.textContent = "Save and reconnect";
    form.removeAttribute("aria-busy");
  }
}

async function refreshStatus(options = {}) {
  if (refreshing) return;
  refreshing = true;
  refreshButton.disabled = true;
  try {
    const response = await chrome.runtime.sendMessage({ type: "BRW_GET_STATUS" });
    if (!response?.ok) throw new Error(response?.error || "Status unavailable");
    const status = response.status || {};
    if (options.populate) populateForm(status.config || {});
    renderStatus(status, options.announce);
  } catch (error) {
    renderUnavailable(error);
  } finally {
    refreshing = false;
    refreshButton.disabled = false;
  }
}

function populateForm(config) {
  for (const id of fieldIds) {
    document.getElementById(id).value = config[id] || "";
  }
  advanced.open = Boolean(config.workspace || config.profile || config.label) || !isCommonEndpoint(config.bridgeUrl);
  updatePresetSelection();
}

function renderStatus(status, announce = false) {
  const socket = status.socket || "closed";
  const daemon = status.daemon || {};
  const bridge = status.bridge || {};
  const identity = daemon.identity || {};
  const configured = status.config || {};
  const connected = socket === "open" && daemon.reachable && daemon.connected;
  const connecting = socket === "connecting" || ["starting", "connecting", "configured"].includes(bridge.status);

  let state = "error";
  let label = "Needs attention";
  let heading = "This profile is not connected";
  let summary = actionableFailure(status);
  if (connected) {
    state = "connected";
    label = "Ready for automation";
    heading = "Connected and identity-verified";
    const name = identity.label || identity.profile || configured.label || configured.profile;
    summary = name ? `This extension is bound to ${name} and the local daemon is responding.` : "The extension and local daemon are connected and responding.";
  } else if (connecting) {
    state = "connecting";
    label = "Reconnecting";
    heading = "Connecting this profile…";
    summary = "The extension is retrying automatically. Confirm the selected port if this takes more than a few seconds.";
  }

  statusBlock.dataset.state = state;
  statusBlock.setAttribute("aria-busy", state === "connecting" ? "true" : "false");
  document.getElementById("statusLabel").textContent = label;
  document.getElementById("connectionHeading").textContent = heading;
  document.getElementById("statusSummary").textContent = summary;
  document.getElementById("socketFact").textContent = socketLabel(socket);
  document.getElementById("daemonFact").textContent = daemon.reachable ? (daemon.connected ? "Connected" : "Reachable, no bridge") : "Not reachable";
  document.getElementById("workspaceFact").textContent = identity.workspace || configured.workspace || "Not bound";
  document.getElementById("profileFact").textContent = identity.profile || configured.profile || "Not bound";
  document.getElementById("extensionVersion").textContent = status.extensionVersion ? `v${status.extensionVersion}` : "";
  rawStatus.textContent = JSON.stringify(status, null, 2);
  updatePresetSelection();
  if (announce) setFormMessage(connected ? "Connection verified." : summary, connected ? "success" : "error");
}

function renderUnavailable(error) {
  statusBlock.dataset.state = "error";
  statusBlock.setAttribute("aria-busy", "false");
  document.getElementById("statusLabel").textContent = "Status unavailable";
  document.getElementById("connectionHeading").textContent = "The extension worker did not respond";
  document.getElementById("statusSummary").textContent = humanizeError(error);
  document.getElementById("socketFact").textContent = "Unknown";
  document.getElementById("daemonFact").textContent = "Unknown";
  rawStatus.textContent = JSON.stringify({ error: String(error?.message || error) }, null, 2);
}

function actionableFailure(status) {
  const daemon = status.daemon || {};
  const bridge = status.bridge || {};
  if (!daemon.reachable) return "Start brwd on the selected port, or choose the port used by this browser profile.";
  if (!daemon.connected) return "The daemon is reachable but does not see this extension. Save the configuration to reconnect.";
  if (bridge.lastError) return humanizeError(bridge.lastError);
  if (bridge.detail) return humanizeError(bridge.detail);
  return "Start the local daemon or select the correct bridge port, then refresh.";
}

function applyPort(port) {
  document.getElementById("bridgeUrl").value = `ws://127.0.0.1:${port}/extension`;
  document.getElementById("statusUrl").value = `http://127.0.0.1:${port}/status`;
  updatePresetSelection();
  setFormMessage(`Port ${port} selected. Save to reconnect this profile.`);
}

function syncStatusEndpoint() {
  const bridgeUrl = document.getElementById("bridgeUrl").value.trim();
  try {
    const url = new URL(bridgeUrl);
    document.getElementById("statusUrl").value = `http://${url.host}/status`;
    updatePresetSelection();
  } catch (_) {
    // Save-time validation provides the actionable error.
  }
}

function updatePresetSelection() {
  let selectedPort = "";
  try { selectedPort = new URL(document.getElementById("bridgeUrl").value).port; } catch (_) {}
  for (const button of document.querySelectorAll("[data-port]")) {
    button.setAttribute("aria-pressed", String(button.dataset.port === selectedPort));
  }
}

function validateEndpoints() {
  const bridgeInput = document.getElementById("bridgeUrl");
  const statusInput = document.getElementById("statusUrl");
  let valid = true;
  try {
    const url = new URL(bridgeInput.value.trim());
    if (url.protocol !== "ws:" || !isLoopback(url.hostname) || url.pathname !== "/extension") throw new Error();
  } catch (_) {
    bridgeInput.setAttribute("aria-invalid", "true");
    valid = false;
  }
  try {
    const url = new URL(statusInput.value.trim());
    if (url.protocol !== "http:" || !isLoopback(url.hostname) || url.pathname !== "/status") throw new Error();
  } catch (_) {
    statusInput.setAttribute("aria-invalid", "true");
    valid = false;
  }
  if (!valid) {
    advanced.open = true;
    setFormMessage("Use local endpoints such as ws://127.0.0.1:17311/extension and http://127.0.0.1:17311/status.", "error");
    window.setTimeout(() => form.querySelector('[aria-invalid="true"]')?.focus(), 0);
  }
  return valid;
}

function clearValidation() {
  for (const input of form.querySelectorAll("[aria-invalid]")) input.removeAttribute("aria-invalid");
}

function readForm() {
  const config = {};
  for (const id of fieldIds) config[id] = document.getElementById(id).value.trim();
  return config;
}

function setFormMessage(message, kind = "") {
  formMessage.textContent = message;
  if (kind) formMessage.dataset.kind = kind;
  else delete formMessage.dataset.kind;
}

function socketLabel(socket) {
  if (socket === "open") return "Open";
  if (socket === "connecting") return "Connecting";
  return "Closed";
}

function isCommonEndpoint(value = "") {
  try { return ["17311", "17411", "17511"].includes(new URL(value).port); } catch (_) { return false; }
}

function isLoopback(hostname) {
  return hostname === "127.0.0.1" || hostname === "localhost";
}

function humanizeError(error) {
  const text = String(error?.message || error || "Connection failed").replace(/^Error:\s*/i, "");
  if (/failed to fetch|networkerror/i.test(text)) return "The local daemon is not reachable on the selected status URL.";
  if (/workspace mismatch/i.test(text)) return "This daemon belongs to a different workspace. Check the identity binding below.";
  if (/profile mismatch/i.test(text)) return "This daemon belongs to a different browser profile. Check the identity binding below.";
  return text.charAt(0).toUpperCase() + text.slice(1);
}
