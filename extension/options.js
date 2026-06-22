const fields = ["bridgeUrl", "statusUrl", "workspace", "profile", "label"];
const statusEl = document.getElementById("status");

document.getElementById("config").addEventListener("submit", async (event) => {
  event.preventDefault();
  const config = readForm();
  const response = await chrome.runtime.sendMessage({ type: "BRW_CONFIGURE", config });
  if (!response?.ok) {
    render({ error: response?.error || "configure failed" });
    return;
  }
  render({ config: response.config });
});

for (const button of document.querySelectorAll("[data-port]")) {
  button.addEventListener("click", () => {
    const port = button.dataset.port;
    document.getElementById("bridgeUrl").value = `ws://127.0.0.1:${port}/extension`;
    document.getElementById("statusUrl").value = `http://127.0.0.1:${port}/status`;
  });
}

init();

async function init() {
  const response = await chrome.runtime.sendMessage({ type: "BRW_GET_STATUS" });
  if (!response?.ok) {
    render({ error: response?.error || "status unavailable" });
    return;
  }
  const config = response.status?.config || {};
  for (const id of fields) {
    document.getElementById(id).value = config[id] || "";
  }
  render(response.status || {});
}

function readForm() {
  const config = {};
  for (const id of fields) {
    config[id] = document.getElementById(id).value.trim();
  }
  return config;
}

function render(value) {
  statusEl.textContent = JSON.stringify(value, null, 2);
}
