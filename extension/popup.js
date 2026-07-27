// Canonical state lexicon (matches toolbar badge tooltips):
//   Idle · Agent active · Reconnecting · Down
const LEXICON = {
  connected: { state: "connected", label: "Idle", heading: "Idle", summary: "" },
  used: { state: "used", label: "Agent active", heading: "Agent active", summary: "An agent is driving a page in this browser right now." },
  connecting: { state: "connecting", label: "Reconnecting", heading: "Reconnecting", summary: "Retrying automatically. Use Reconnect if this sticks." },
  disconnected: { state: "disconnected", label: "Down", heading: "Down", summary: "" },
  error: { state: "error", label: "Down", heading: "Worker unavailable", summary: "" },
  loading: { state: "loading", label: "Checking", heading: "Checking bridge…", summary: "Confirming extension, daemon, and identity." }
};

const popup = document.getElementById("popup");
const reconnectButton = document.getElementById("reconnect");
const optionsButton = document.getElementById("openOptions");
const formMessage = document.getElementById("formMessage");
const detailsPanel = document.getElementById("detailsPanel");

let refreshTimer = 0;
let busy = false;
let operatorOpenedDetails = false;

reconnectButton.addEventListener("click", reconnect);
optionsButton.addEventListener("click", () => {
  chrome.runtime.openOptionsPage();
});
detailsPanel.addEventListener("toggle", () => {
  operatorOpenedDetails = detailsPanel.open;
});

init();

async function init() {
  await refresh({ announce: false });
  refreshTimer = window.setInterval(() => {
    if (!document.hidden && !busy) refresh({ announce: false });
  }, 1500);
  window.addEventListener("pagehide", () => window.clearInterval(refreshTimer), { once: true });
}

async function reconnect() {
  if (busy) return;
  busy = true;
  reconnectButton.disabled = true;
  optionsButton.disabled = true;
  reconnectButton.textContent = "Reconnecting…";
  setMessage("Probing the local daemon…");
  try {
    const response = await chrome.runtime.sendMessage({ type: "BRW_RECONNECT" });
    if (!response?.ok) throw new Error(response?.error || "Reconnect failed");
    render(response.status || {}, false);

    // Verified reconnect: poll until Idle/Agent active or deadline.
    const deadline = Date.now() + 12_000;
    while (Date.now() < deadline) {
      await sleep(400);
      const poll = await chrome.runtime.sendMessage({ type: "BRW_GET_STATUS" });
      if (!poll?.ok) continue;
      const status = poll.status || {};
      render(status, false);
      if (isVerifiedUp(status)) {
        const port = portFromConfig(status.config || {});
        const name = profileName(status);
        setMessage(
          name
            ? `Verified · ${name}${port ? ` · :${port}` : ""}`
            : `Verified${port ? ` · :${port}` : ""}`,
          "success"
        );
        return;
      }
    }
    setMessage("Still down. Start brwd or open Options to pick the right port.", "error");
  } catch (error) {
    setMessage(humanize(error), "error");
  } finally {
    busy = false;
    reconnectButton.disabled = false;
    optionsButton.disabled = false;
    reconnectButton.textContent = "Reconnect";
  }
}

async function refresh({ announce = false } = {}) {
  if (busy) return;
  try {
    const response = await chrome.runtime.sendMessage({ type: "BRW_GET_STATUS" });
    if (!response?.ok) throw new Error(response?.error || "Status unavailable");
    render(response.status || {}, announce);
  } catch (error) {
    applyLexicon("error", humanize(error));
    popup.dataset.state = "error";
    detailsPanel.open = true;
    document.getElementById("socketFact").textContent = "—";
    document.getElementById("daemonFact").textContent = "—";
    document.getElementById("portFact").textContent = "—";
    document.getElementById("badgeFact").textContent = "Down";
    if (announce) setMessage(humanize(error), "error");
  }
}

function render(status, announce) {
  const socket = status.socket || "closed";
  const daemon = status.daemon || {};
  const bridge = status.bridge || {};
  const config = status.config || {};
  const connected = socket === "open" && daemon.reachable && daemon.connected;
  const derived = deriveBadge(socket, daemon, status.agentActive);
  let badge = status.badge || derived;
  if (connected && (badge === "disconnected" || badge === "error")) badge = derived;
  if (!connected && (badge === "used" || badge === "connected")) badge = derived;
  const connecting = badge === "connecting" || socket === "connecting"
    || ["starting", "connecting", "configured"].includes(bridge.status);

  let mode = "disconnected";
  if (badge === "used" || (connected && status.agentActive)) mode = "used";
  else if (connected) mode = "connected";
  else if (connecting) mode = "connecting";

  const name = profileName(status);
  const port = portFromConfig(config);
  let summary = LEXICON[mode].summary;
  if (mode === "connected") {
    summary = name
      ? `Bound to ${name}${port ? ` · :${port}` : ""}. Ready for automation.`
      : `Extension and daemon connected${port ? ` · :${port}` : ""}.`;
  } else if (mode === "disconnected") {
    summary = actionable(status);
  }

  applyLexicon(mode, summary);
  popup.dataset.state = mode;

  document.getElementById("socketFact").textContent = socketLabel(socket);
  document.getElementById("daemonFact").textContent = daemon.reachable
    ? (daemon.connected ? "Connected" : "Reachable, no bridge")
    : "Not reachable";
  document.getElementById("portFact").textContent = port || "—";
  document.getElementById("badgeFact").textContent = lexiconName(mode);
  document.getElementById("extensionVersion").textContent = status.extensionVersion
    ? `v${status.extensionVersion}`
    : "";
  document.getElementById("profileLine").textContent = name || "Unbound profile";
  document.getElementById("detailsMeta").textContent = port
    ? `:${port} · ${socketLabel(socket).toLowerCase()}`
    : socketLabel(socket).toLowerCase();

  // Progressive disclosure: open Details when unhealthy unless the operator
  // already chose. Healthy Idle stays collapsed so the popup can disappear.
  if (!operatorOpenedDetails) {
    detailsPanel.open = mode !== "connected" && mode !== "used";
  }

  if (announce && mode === "disconnected") setMessage(summary, "error");
  if (!announce && mode === "connected" && formMessage.dataset.kind !== "success") {
    clearMessage();
  }
}

function applyLexicon(mode, summary) {
  const entry = LEXICON[mode] || LEXICON.disconnected;
  document.getElementById("statusLabel").textContent = entry.label;
  document.getElementById("statusHeading").textContent = entry.heading;
  document.getElementById("statusSummary").textContent = summary || entry.summary;
}

function deriveBadge(socket, daemon, agentActive) {
  if (socket === "open" && daemon.reachable && daemon.connected) {
    return agentActive ? "used" : "connected";
  }
  if (socket === "connecting") return "connecting";
  return "disconnected";
}

function lexiconName(mode) {
  if (mode === "used") return "Agent active";
  if (mode === "connected") return "Idle";
  if (mode === "connecting") return "Reconnecting";
  return "Down";
}

function isVerifiedUp(status) {
  const socket = status.socket || "closed";
  const daemon = status.daemon || {};
  return socket === "open" && daemon.reachable && daemon.connected;
}

function profileName(status) {
  const identity = status.daemon?.identity || {};
  const config = status.config || {};
  return identity.label || identity.profile || config.label || config.profile || "";
}

function actionable(status) {
  const daemon = status.daemon || {};
  const bridge = status.bridge || {};
  if (!daemon.reachable) return "Start brwd on this profile's port, or open Options to pick the right one.";
  if (!daemon.connected) return "Daemon is up but does not see this extension. Hit Reconnect.";
  if (bridge.lastError) return humanize(bridge.lastError);
  if (bridge.detail) return humanize(bridge.detail);
  return "Bridge is down. Hit Reconnect or open Options.";
}

function portFromConfig(config = {}) {
  try {
    return new URL(config.bridgeUrl || config.statusUrl || "").port || "";
  } catch (_) {
    return "";
  }
}

function socketLabel(socket) {
  if (socket === "open") return "Open";
  if (socket === "connecting") return "Connecting";
  return "Closed";
}

function setMessage(text, kind = "") {
  if (!text) {
    clearMessage();
    return;
  }
  formMessage.hidden = false;
  formMessage.textContent = text;
  if (kind) formMessage.dataset.kind = kind;
  else delete formMessage.dataset.kind;
}

function clearMessage() {
  formMessage.hidden = true;
  formMessage.textContent = "";
  delete formMessage.dataset.kind;
}

function humanize(error) {
  const text = String(error?.message || error || "Something went wrong").replace(/^Error:\s*/i, "");
  if (/failed to fetch|networkerror/i.test(text)) {
    return "The local daemon is not reachable on the configured status URL.";
  }
  return text.charAt(0).toUpperCase() + text.slice(1);
}

function sleep(ms) {
  return new Promise((resolve) => window.setTimeout(resolve, ms));
}
