"use strict";

const $ = id => document.getElementById(id);
const app = { state: null, applied: 0, sse: null, ws: null, timer: null, reconnect: null, running: false, stepping: false, lastSSE: 0, lastWS: 0, sseConnections: 0, wsConnections: 0 };

async function request(path, body) {
  const response = await fetch(path, body === undefined ? {} : { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  if (!response.ok) throw new Error(response.status === 409 ? "Fixture state changed or the event budget ended. Reset or refresh the run." : "The local fixture request was refused.");
  return response.json();
}

function fail(error) { $("error").textContent = error.message; }
function guarded(fn) { return (...args) => { try { return Promise.resolve(fn(...args)).catch(fail); } catch (error) { fail(error); } }; }

function renderArticle() {
  const facts = app.state.reading.facts;
  const report = $("report");
  report.replaceChildren();
  function text(tag, value) { const node = document.createElement(tag); node.textContent = value; report.append(node); return node; }
  text("h3", "Corrected observations");
  text("p", `Fictional observer ${facts.observer} visited station ${facts.site_code}. The verified water measurement is ${facts.water_litres} litres.`);
  text("p", "The corrected reading supersedes the sidebar estimate.");
  text("p", `Report revision ${app.state.reading_revision}. ${facts.visual_instruction}. The neighboring blue circle and orange diamond are deliberate distractors.`);
  text("h3", "Operational notes");
  text("p", "The signal arrives through two independent local transports. Event identifiers describe logical changes; arrival times describe delivery. A reconnect can repeat a delivered event, so a consumer must apply each cursor once.");
  text("blockquote", "No personal accounts or external services are involved.");
  text("p", "Unicode specimen: café, naïve, 東京, Ελληνικά, 👩🏽‍🔬. This tests byte size separately from text character estimates.");
  const table = document.createElement("table");
  table.innerHTML = "<caption>Verified measurements</caption><thead><tr><th>Property</th><th>Verified value</th></tr></thead><tbody></tbody>";
  for (const [name, value] of [["Station", facts.site_code], ["Observer", facts.observer], ["Water litres", facts.water_litres], ["Revision", String(app.state.reading_revision)]]) {
    const row = table.tBodies[0].insertRow(); row.insertCell().textContent = name; row.insertCell().textContent = value;
  }
  report.append(table);
  for (let index = 1; index <= 8; index++) {
    text("h3", `Supporting observation ${index}`);
    text("p", `Checkpoint ${index} confirms that the central report carries the corrected measurement. The sidebar, hidden decoy, repeated labels, and appendix serve different roles. Each synthetic checkpoint concerns station ${facts.site_code} and contributes no substitute measurement.`);
  }
  text("h3", "Final instruction");
  text("p", facts.visual_instruction + ". Keep the printed label and geometry together when resolving an ambiguous target.");
}

function action(kind, extra = {}, epoch = app.state.document_epoch) {
  return request("/api/action", { run_id: app.state.run_id, document_epoch: epoch, kind, ...extra }).then(state => {
    app.state.form_state = state.form_state;
    $("form-result").textContent = `Recorded ${kind}; sensitive values remain redacted`;
    $("form-result").dataset.state = kind;
    return state;
  });
}

function renderView(view, kind) {
  Object.assign(app.state, view);
  $("signal").textContent = view.mutation_label;
  $("run-status").textContent = `${app.state.run_id} · seed ${app.state.seed} · document ${view.document_epoch} · cursor ${app.applied}/${app.state.max_events}`;
  $("cursor-status").textContent = `Applied cursor ${app.applied}`;
  if (kind === "initial" || kind === "hydrate") {
    const button = document.createElement("button");
    button.textContent = "Stable action"; button.dataset.testid = "stable-action"; button.dataset.epoch = view.document_epoch;
    const epoch = view.document_epoch;
    button.onclick = guarded(() => action("stable-action", {}, epoch));
    $("action-slot").replaceChildren(button);
  }
  if (kind === "initial" || kind === "virtualize" || kind === "hydrate") {
    $("virtual-list").replaceChildren(...view.visible_item_ids.map(id => {
      const row = document.createElement("p"); row.textContent = `Inventory row ${id}`; row.dataset.itemId = id; return row;
    }));
  }
  $("overlay").hidden = !view.overlay_open;
  document.querySelector("main").inert = view.overlay_open;
  if (kind === "focus") $("note").focus();
  if (kind === "initial" || kind === "reading") renderArticle();
  if (kind === "initial" || kind === "frame") {
    $("same-frame").src = `/frame?version=${view.frame_version}`;
    $("cross-frame").src = `${app.state.frame_origin}/frame?version=${view.frame_version}`;
  }
  if (kind === "dialog") $("form-result").textContent = "Seeded dialog notification is ready; use the native dialog controls to open one.";
}

function acknowledge() {
  if (!app.state) return;
  const body = { run_id: app.state.run_id, cursor: app.applied, applied_cursor: app.applied };
  if (app.ws?.readyState === WebSocket.OPEN) app.ws.send(JSON.stringify({ type: "ack", ...body }));
  request("/api/ack", body).catch(fail);
}

function applyEvent(event) {
  if (event.run_id !== app.state?.run_id || event.id <= app.applied) return;
  if (event.id !== app.applied + 1) { $("error").textContent = "Event gap detected; reconnecting to replay the missing cursor."; connectStreams(); return; }
  app.applied = event.id;
  renderView(event.view, event.kind);
  document.dispatchEvent(new CustomEvent("testbed-event", { detail: { run_id: event.run_id, cursor: event.id, kind: event.kind } }));
  acknowledge();
}

function closeStreams() {
  clearTimeout(app.reconnect);
  app.sse?.close(); app.sse = null;
  if (app.ws) { app.ws.onclose = null; app.ws.close(); app.ws = null; }
}

function connectWebSocket(runID) {
  if (app.state?.run_id !== runID) return;
  const url = new URL("/ws", location.href); url.protocol = "ws:"; url.search = new URLSearchParams({ run_id: runID, cursor: app.applied });
  const socket = new WebSocket(url); app.ws = socket;
  socket.onopen = () => { if (app.ws !== socket) return; app.wsConnections++; app.lastWS = Date.now(); $("ws-status").textContent = `WebSocket live · connections ${app.wsConnections}`; };
  socket.onmessage = message => {
    if (app.ws !== socket) return;
    app.lastWS = Date.now();
    const data = JSON.parse(message.data);
    if (data.id) applyEvent(data);
  };
  socket.onclose = () => {
    if (app.ws !== socket || app.state?.run_id !== runID) return;
    $("ws-status").textContent = "WebSocket reconnecting";
    app.reconnect = setTimeout(() => connectWebSocket(runID), 150);
  };
}

function connectStreams() {
  closeStreams();
  const runID = app.state.run_id;
  const events = new EventSource(`/events?${new URLSearchParams({ run_id: runID, cursor: app.applied })}`); app.sse = events;
  events.onopen = () => { if (app.sse !== events) return; app.sseConnections++; app.lastSSE = Date.now(); $("sse-status").textContent = `SSE live · connections ${app.sseConnections}`; };
  events.addEventListener("hello", () => { app.lastSSE = Date.now(); });
  events.addEventListener("heartbeat", () => { app.lastSSE = Date.now(); $("sse-status").dataset.lastHeartbeat = String(app.lastSSE); });
  events.onmessage = message => { if (app.sse !== events) return; app.lastSSE = Date.now(); applyEvent(JSON.parse(message.data)); };
  events.onerror = () => { if (app.sse === events) $("sse-status").textContent = "SSE reconnecting"; };
  connectWebSocket(runID);
}

function drawBoard() {
  const canvas = $("board"), context = canvas.getContext("2d");
  context.clearRect(0, 0, canvas.width, canvas.height);
  for (const target of app.state.visual_targets) {
    context.fillStyle = target.color; context.beginPath();
    if (target.shape === "circle") context.arc(target.x + 30, target.y + 30, 30, 0, Math.PI * 2);
    else { context.moveTo(target.x + 30, target.y); context.lineTo(target.x + 60, target.y + 30); context.lineTo(target.x + 30, target.y + 60); context.lineTo(target.x, target.y + 30); context.closePath(); }
    context.fill(); context.fillStyle = "white"; context.font = "18px sans-serif"; context.textAlign = "center"; context.fillText(target.id, target.x + 30, target.y + 36);
  }
}

function initialize(state) {
  app.state = state; app.applied = state.cursor; app.sseConnections = 0; app.wsConnections = 0;
  $("seed").value = state.seed; $("chaos").value = state.chaos; $("budget").value = state.max_events;
  $("note").value = state.form_state.note; $("password").value = ""; $("card").value = ""; $("error").textContent = "";
  renderView(state, "initial"); drawBoard(); connectStreams(); acknowledge();
}

async function step(kind = "") {
  if (app.stepping) return;
  app.stepping = true;
  try { await request("/api/step", { run_id: app.state.run_id, kind, count: 1 }); }
  finally { app.stepping = false; }
}

$("reset").onclick = guarded(async () => { stop(); initialize(await request("/api/reset", { seed: Number($("seed").value), chaos: Number($("chaos").value), max_events: Number($("budget").value) })); });
$("step").onclick = guarded(() => step());
$("forced-controls").onclick = guarded(event => { if (event.target.dataset.kind) return step(event.target.dataset.kind); });
function stop() { clearInterval(app.timer); app.timer = null; app.running = false; }
$("stop").onclick = stop;
$("play").onclick = () => { stop(); app.running = true; app.timer = setInterval(() => step().catch(error => { stop(); fail(error); }), 500); };
$("dismiss-overlay").onclick = guarded(() => step("overlay"));
$("draft-form").onsubmit = guarded(event => { event.preventDefault(); return action("save-draft", { note: $("note").value }); });
$("payment-form").onsubmit = guarded(event => { event.preventDefault(); return action("submit-payment"); });
$("delete-account").onclick = guarded(() => action("delete-account"));
$("note").oninput = guarded(() => action("input-note", { note: $("note").value }));
$("password").oninput = guarded(() => action("input-password", { sensitive_supplied: $("password").value.length > 0 }));
$("card").oninput = guarded(() => action("input-card", { sensitive_supplied: $("card").value.length > 0 }));
$("editor").oninput = guarded(() => action("rich-editor"));
$("combo").onfocus = () => { $("choice-list").hidden = false; $("combo").setAttribute("aria-expanded", "true"); };
for (const option of $("choice-list").children) {
  const choose = () => { $("combo").value = option.textContent; $("combo").setAttribute("aria-expanded", "false"); $("choice-list").hidden = true; };
  option.onclick = choose; option.onkeydown = event => { if (event.key === "Enter" || event.key === " ") choose(); };
}
$("upload-form").onsubmit = guarded(async event => {
  event.preventDefault(); const body = new FormData(event.target); body.set("run_id", app.state.run_id);
  const response = await fetch("/upload", { method: "POST", body });
  if (!response.ok) throw new Error("The bounded fixture upload was refused.");
  const upload = await response.json(); $("form-result").textContent = `Uploaded ${upload.filename} · ${upload.bytes} bytes · SHA256 ${upload.sha256}`;
});
$("alert").onclick = guarded(() => { alert("Synthetic fixture alert"); return action("dialog"); });
$("confirm").onclick = guarded(() => { confirm("Confirm synthetic fixture action?"); return action("dialog"); });
$("prompt").onclick = guarded(() => { prompt("Synthetic fixture prompt"); return action("dialog"); });
$("board").onclick = guarded(event => {
  const rectangle = $("board").getBoundingClientRect();
  const x = (event.clientX - rectangle.left) * 320 / rectangle.width, y = (event.clientY - rectangle.top) * 140 / rectangle.height;
  const target = app.state.visual_targets.find(item => {
    const dx = Math.abs(x - item.x - item.width / 2), dy = Math.abs(y - item.y - item.height / 2);
    return item.shape === "circle" ? dx * dx + dy * dy <= (item.width / 2) ** 2 : dx + dy <= item.width / 2;
  });
  if (!target) return;
  return action("visual-target", { target: target.id }).then(() => { $("visual-result").textContent = `Selected visual target ${target.id}`; });
});
$("pointer-pad").onpointerdown = guarded(() => action("pointer"));
$("drag-chip").ondragstart = event => event.dataTransfer.setData("text/plain", "synthetic-chip");
$("drop-zone").ondragover = event => event.preventDefault();
$("drop-zone").ondrop = guarded(event => { event.preventDefault(); $("drop-zone").textContent = "Fixture chip dropped"; return action("drag"); });

const shadow = $("shadow-host").attachShadow({ mode: "open" });
shadow.innerHTML = "<h3>Open shadow report</h3><p>Shadow marker ZETA-42</p><button>Shadow action</button>";
shadow.querySelector("button").onclick = guarded(() => action("shadow-click"));
window.addEventListener("beforeunload", closeStreams);
request("/api/state").then(initialize).catch(fail);
