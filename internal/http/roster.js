
const boardEl = document.getElementById("board");
const errEl = document.getElementById("err");
const toastEl = document.getElementById("toast");
let drag = null;
document.getElementById("refresh").onclick = load;
load();

async function load() {
  errEl.hidden = true;
  try {
    const r = await fetch("/api/roster/board", { cache: "no-store" });
    const data = await r.json();
    if (!r.ok) throw new Error(data.error || r.statusText);
    render(data.wells || []);
  } catch (e) {
    errEl.hidden = false;
    errEl.textContent = e.message || String(e);
  }
}

function render(wells) {
  boardEl.replaceChildren();
  for (const well of wells) boardEl.append(wellNode(well));
  boardEl.append(createNode());
}

function wellNode(well) {
  const el = document.createElement("section");
  el.className = "well" + (well.source_only ? " source" : "");
  el.dataset.name = well.name;
  el.dataset.accepts = well.accepts_drop ? "1" : "0";
  const h = document.createElement("header");
  const h2 = document.createElement("h2");
  h2.textContent = well.display_name || well.name;
  const a = document.createElement("div");
  a.className = "meta";
  a.textContent = well.account || "no account yet";
  const b = document.createElement("div");
  b.className = "meta";
  b.textContent = (well.namespace || "") + " · " + (well.transport || "") + " · " + (well.reachable ? "up" : "down");
  h.append(h2, a, b);
  el.append(h);
  const chips = well.chips || [];
  if (!chips.length) {
    const empty = document.createElement("p");
    empty.className = "empty";
    empty.textContent = well.source_only ? "Source well — drag from here" : "Drop a login, or open to sign in";
    el.append(empty);
  }
  for (const chip of chips) el.append(chipNode(chip, well));
  const actions = document.createElement("div");
  actions.className = "actions";
  const open = document.createElement("button");
  open.textContent = "Open";
  open.onclick = function () { post("/api/roster/open", { profile: well.name }); };
  actions.append(open);
  el.append(actions);
  el.addEventListener("dragover", function (ev) {
    if (el.dataset.accepts !== "1") return;
    ev.preventDefault();
    el.classList.add("drop");
  });
  el.addEventListener("dragleave", function () { el.classList.remove("drop"); });
  el.addEventListener("drop", async function (ev) {
    el.classList.remove("drop");
    ev.preventDefault();
    if (!drag || drag.from === well.name) return;
    if (el.dataset.accepts !== "1") { toast("Cannot drop onto daily Chrome"); return; }
    const mode = ev.altKey ? "move" : "copy";
    try {
      const res = await post("/api/roster/copy", { from: drag.from, to: well.name, domain: drag.domain, mode: mode });
      toast((mode === "move" ? "Moved " : "Copied ") + drag.domain + " · " + (res.copied || 0) + " cookies");
      await load();
    } catch (e) { toast(e.message || String(e)); }
  });
  return el;
}

function chipNode(chip, well) {
  const el = document.createElement("div");
  el.className = "chip";
  el.draggable = true;
  el.dataset.health = chip.health || "unknown";
  const pip = document.createElement("span");
  pip.className = "pip";
  const wrap = document.createElement("span");
  const b = document.createElement("b");
  b.textContent = chip.domain;
  const sm = document.createElement("small");
  sm.textContent = chip.account || chip.health || "";
  wrap.append(b, sm);
  el.append(pip, wrap);
  el.addEventListener("dragstart", function (ev) {
    drag = { from: well.name, domain: chip.domain };
    ev.dataTransfer.effectAllowed = "copyMove";
  });
  el.addEventListener("dragend", function () { drag = null; });
  return el;
}

function createNode() {
  const el = document.createElement("section");
  el.className = "well";
  el.id = "create";
  const h = document.createElement("header");
  const h2 = document.createElement("h2");
  h2.textContent = "New well";
  const m = document.createElement("div");
  m.className = "meta";
  m.textContent = "isolated Chromium jar";
  h.append(h2, m);
  const form = document.createElement("form");
  form.innerHTML = '<input name="name" required placeholder="bookkeeper" autocomplete="off" spellcheck="false">' +
    '<input name="account" placeholder="agent.bookkeeper@…" autocomplete="off">' +
    '<button class="primary" type="submit">Create</button>';
  form.onsubmit = async function (ev) {
    ev.preventDefault();
    const fd = new FormData(ev.target);
    try {
      await post("/api/roster/profiles", { name: fd.get("name"), account: fd.get("account"), browser: "chromium" });
      toast("Created " + fd.get("name"));
      await load();
    } catch (e) { toast(e.message || String(e)); }
  };
  el.append(h, form);
  return el;
}

async function post(url, body) {
  const r = await fetch(url, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify(body) });
  const data = await r.json().catch(function () { return {}; });
  if (!r.ok) throw new Error(data.error || r.statusText);
  return data;
}
function toast(msg) {
  toastEl.textContent = msg;
  toastEl.classList.add("show");
  setTimeout(function () { toastEl.classList.remove("show"); }, 2800);
}
