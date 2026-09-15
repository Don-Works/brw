const fieldIds = ["bridgeUrl", "statusUrl", "workspace", "profile", "label"];
const form = document.getElementById("config");
const saveButton = document.getElementById("saveConfig");
const refreshButton = document.getElementById("refreshStatus");
const statusBlock = document.getElementById("statusBlock");
const formMessage = document.getElementById("formMessage");
const rawStatus = document.getElementById("rawStatus");
const advanced = document.getElementById("advancedConfig");
const consentPanel = document.getElementById("consentPanel");
const grantConsentButton = document.getElementById("grantConsent");
const revokeConsentButton = document.getElementById("revokeConsent");
const grantsList = document.getElementById("grantsList");
const grantsEmpty = document.getElementById("grantsEmpty");
const grantsRejected = document.getElementById("grantsRejected");
const grantsSource = document.getElementById("grantsSource");
const revokeAllGrantsButton = document.getElementById("revokeAllGrants");
let refreshTimer = 0;
let refreshing = false;
let consentGranted = false;

form.addEventListener("submit", save);
refreshButton.addEventListener("click", () => refreshStatus({ announce: true }));
grantConsentButton.addEventListener("click", () => updateConsent(true));
revokeConsentButton.addEventListener("click", () => {
  if (window.confirm("Disable brw browser control and disconnect the local daemon?")) {
    updateConsent(false);
  }
});
document.getElementById("bridgeUrl").addEventListener("change", syncStatusEndpoint);
document.addEventListener("visibilitychange", () => {
  if (!document.hidden) refreshStatus();
});

for (const button of document.querySelectorAll("[data-port]")) {
  button.addEventListener("click", () => applyPort(button.dataset.port));
}

init();

revokeAllGrantsButton.addEventListener("click", () => {
  if (window.confirm("Revoke every site permission this profile holds? Agents will be refused until each site is granted again.")) {
    revokeGrant({ all: true });
  }
});

async function init() {
  await loadProfileManagerPref();
  await refreshStatus({ populate: true });
  await refreshGrants();
  refreshTimer = window.setInterval(() => {
    if (!document.hidden) refreshStatus();
  }, 3000);
  window.addEventListener("pagehide", () => window.clearInterval(refreshTimer), { once: true });
}

// refreshGrants renders the per-origin permissions the daemon holds. It is a
// separate, on-demand read rather than part of the 3-second status poll: the
// list changes when a person changes it, and polling it would put the sites the
// user has visited through the message channel every few seconds for nothing.
async function refreshGrants() {
  try {
    const response = await chrome.runtime.sendMessage({ type: "BRW_GET_CONSENT" });
    if (!response?.ok) throw new Error(response?.error || "Site permissions unavailable");
    renderGrants(response.consent || {});
  } catch (error) {
    grantsList.replaceChildren();
    grantsRejected.hidden = true;
    grantsEmpty.hidden = false;
    grantsEmpty.textContent = humanizeError(error);
    grantsSource.textContent = "";
  }
}

function renderGrants(consent) {
  grantsList.replaceChildren();
  grantsRejected.replaceChildren();
  grantsRejected.hidden = true;

  if (!consent.enabled) {
    grantsEmpty.hidden = false;
    grantsEmpty.textContent = "This daemon runs without site permissions, so every site an agent reaches is allowed.";
    grantsSource.textContent = "";
    revokeAllGrantsButton.disabled = true;
    return;
  }
  const grants = Array.isArray(consent.grants) ? consent.grants : [];
  revokeAllGrantsButton.disabled = grants.length === 0;
  grantsEmpty.hidden = grants.length > 0;
  if (grants.length === 0) {
    grantsEmpty.textContent = "No sites have been granted yet. An agent will be refused until one is.";
  }
  for (const grant of grants) {
    grantsList.appendChild(grantRow(grant));
  }
  const rejected = Array.isArray(consent.rejected) ? consent.rejected : [];
  if (rejected.length > 0) {
    grantsRejected.hidden = false;
    for (const record of rejected) {
      const item = document.createElement("li");
      item.textContent = `Refused record for ${record.origin}: ${record.reason}`;
      grantsRejected.appendChild(item);
    }
  }
  grantsSource.textContent = consent.category_source
    ? `Category blocklist ${consent.category_version || ""}. ${consent.category_source} ${consent.category_update || ""}`.trim()
    : "";
}

function grantRow(grant) {
  const item = document.createElement("li");
  const text = document.createElement("div");

  const origin = document.createElement("strong");
  origin.textContent = grant.origin;
  text.appendChild(origin);

  const facts = [grant.scope === "act" ? "may act" : "may read"];
  if (grant.decision === "deny") facts.push("refused");
  if (grant.granted_by) facts.push(`by ${grant.granted_by}`);
  if (grant.granted_at) facts.push(`on ${grant.granted_at}`);
  if (grant.expiry) facts.push(grant.expired ? `expired ${grant.expiry}` : `expires ${grant.expiry}`);
  if (grant.override_category) facts.push(`category override: ${grant.override_category}`);
  const detail = document.createElement("small");
  detail.textContent = facts.join(" · ");
  text.appendChild(detail);

  const button = document.createElement("button");
  button.type = "button";
  button.className = "button secondary";
  button.textContent = "Revoke";
  button.addEventListener("click", () => revokeGrant({ origin: grant.origin, scope: grant.scope }));

  item.appendChild(text);
  item.appendChild(button);
  return item;
}

async function revokeGrant(request) {
  revokeAllGrantsButton.disabled = true;
  try {
    const response = await chrome.runtime.sendMessage({ type: "BRW_REVOKE_CONSENT", request });
    if (!response?.ok) throw new Error(response?.error || "Could not revoke");
    const removed = response.result?.removed ?? 0;
    setFormMessage(`Revoked ${removed} site permission${removed === 1 ? "" : "s"}. This applies to the agent's next action.`, "success");
  } catch (error) {
    setFormMessage(humanizeError(error), "error");
  } finally {
    await refreshGrants();
  }
}

async function save(event) {
  event.preventDefault();
  clearValidation();
  if (!consentGranted) {
    setFormMessage("Enable local browser control before connecting this profile.", "error");
    grantConsentButton.focus();
    return;
  }
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
    saveButton.disabled = !consentGranted;
    saveButton.textContent = "Save and reconnect";
    form.removeAttribute("aria-busy");
  }
}

async function updateConsent(granted) {
  grantConsentButton.disabled = true;
  revokeConsentButton.disabled = true;
  setFormMessage(
    granted
      ? "Enabling browser control and connecting to the local daemon…"
      : "Disabling browser control and releasing controlled tabs…"
  );
  try {
    const response = await chrome.runtime.sendMessage({ type: "BRW_SET_CONSENT", granted });
    if (!response?.ok) throw new Error(response?.error || "Could not update browser control");
    renderStatus(response.status || {}, false);
    setFormMessage(
      granted
        ? "Browser control enabled. The extension may now connect to your local brw daemon."
        : "Browser control disabled. The daemon is disconnected and debugger sessions are released.",
      "success"
    );
  } catch (error) {
    setFormMessage(humanizeError(error), "error");
  } finally {
    grantConsentButton.disabled = false;
    revokeConsentButton.disabled = false;
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
  renderConsent(status.consent || {});
  const connected = consentGranted && socket === "open" && daemon.reachable && daemon.connected;
  const connecting = socket === "connecting" || ["starting", "connecting", "configured"].includes(bridge.status);

  let state = "error";
  let label = "Needs attention";
  let heading = "This profile is not connected";
  let summary = actionableFailure(status);
  if (!consentGranted) {
    state = "consent";
    label = "Not enabled";
    heading = "Enable browser control first";
    summary = "The extension will not connect to a daemon or handle page data until you use the enable button above.";
  } else if (connected) {
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

function renderConsent(consent = {}) {
  consentGranted = consent.granted === true;
  consentPanel.dataset.enabled = String(consentGranted);
  document.getElementById("consentKicker").textContent = consentGranted
    ? "Browser control enabled"
    : "Required before connection";
  document.getElementById("consentHeading").textContent = consentGranted
    ? "Local browser control is on"
    : "Enable local browser control";
  document.getElementById("consentSummary").textContent = consentGranted
    ? "brw may connect to your local daemon and handle browser data only when your configured agent requests it."
    : "When you ask an agent to use brw, this extension can read and change content in visible tabs and return the result to a brw daemon on this computer.";
  grantConsentButton.hidden = consentGranted;
  revokeConsentButton.hidden = !consentGranted;
  saveButton.disabled = !consentGranted;
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
  if (status.consent?.granted !== true) return "Enable local browser control above before connecting this profile.";
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

async function loadProfileManagerPref() {
  const box = document.getElementById("profileManager");
  if (!box) return;
  const stored = await chrome.storage.local.get("profileManagerEnabled");
  box.checked = stored.profileManagerEnabled === true;
  box.addEventListener("change", async () => {
    await chrome.storage.local.set({ profileManagerEnabled: box.checked });
    setFormMessage(
      box.checked
        ? "Profile manager is on. Open the toolbar popup to launch it."
        : "Profile manager hidden from the toolbar menu.",
      "success"
    );
  });
}
