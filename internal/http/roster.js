const boardEl = document.getElementById("board");
const errEl = document.getElementById("err");
const toastEl = document.getElementById("toast");
const SIGN_IN_URL = "https://accounts.google.com";
let drag = null;
let lastCreated = "";
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
  el.className = "well" + (well.offers_drag ? "" : " readonly");
  el.dataset.name = well.name;
  const h = document.createElement("header");
  const h2 = document.createElement("h2");
  h2.textContent = well.name;
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
    empty.textContent = well.accepts_drop ? "Drop a site here, or open to sign in" : readonlyReason(well);
    el.append(empty);
  }
  for (const chip of chips) el.append(chipNode(chip, well));
  if (well.daemon_error) {
    const err = document.createElement("p");
    err.className = "err";
    err.textContent = well.daemon_error;
    el.append(err);
  }
  const actions = document.createElement("div");
  actions.className = "actions";
  const open = document.createElement("button");
  open.type = "button";
  open.textContent = "Open sign-in";
  open.disabled = !well.reachable;
  open.onclick = async function () {
    try {
      await post("/api/roster/open", { profile: well.name, url: SIGN_IN_URL });
      toast("Opened " + SIGN_IN_URL + " in " + well.name);
    } catch (e) { toast(e.message || String(e)); }
  };
  actions.append(open);
  el.append(actions);
  if (!well.accepts_drop) return el;
  el.addEventListener("dragover", function (ev) {
    if (!drag || drag.from === well.name) return;
    ev.preventDefault();
    el.classList.add("drop");
  });
  el.addEventListener("dragleave", function () { el.classList.remove("drop"); });
  el.addEventListener("drop", async function (ev) {
    el.classList.remove("drop");
    ev.preventDefault();
    if (!drag || drag.from === well.name) return;
    const mode = ev.altKey ? "move" : "copy";
    const d = drag;
    try {
      const res = await post("/api/roster/copy", { from: d.from, to: well.name, domain: d.domain, mode: mode });
      toast((mode === "move" ? "Moved " : "Copied ") + d.domain + " · " + (res.copied || 0) + " cookies");
      await load();
    } catch (e) { toast(e.message || String(e)); }
  });
  return el;
}

function readonlyReason(well) {
  if (!well.reachable) return "Daemon not running";
  if (well.transport === "extension-bridge") return "Your signed-in browser. Sessions stay here.";
  return "Sessions cannot be copied to or from this profile";
}

function chipNode(chip, well) {
  const el = document.createElement("div");
  el.className = "chip";
  el.draggable = !!well.offers_drag && chip.health !== "missing";
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
  if (!el.draggable) return el;
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
  h2.textContent = "New profile";
  const m = document.createElement("div");
  m.className = "meta";
  m.textContent = "brw-owned Chromium, its own directory";
  h.append(h2, m);
  const form = document.createElement("form");
  const name = document.createElement("input");
  name.name = "name";
  name.required = true;
  name.placeholder = "bookkeeper";
  name.autocomplete = "off";
  name.spellcheck = false;
  const account = document.createElement("input");
  account.name = "account";
  account.placeholder = "Google account it should use";
  account.autocomplete = "off";
  const submit = document.createElement("button");
  submit.className = "primary";
  submit.type = "submit";
  submit.textContent = "Create";
  const next = document.createElement("p");
  next.className = "cmd";
  next.hidden = !lastCreated;
  next.textContent = lastCreated;
  form.append(name, account, submit, next);
  form.onsubmit = async function (ev) {
    ev.preventDefault();
    try {
      const res = await post("/api/roster/profiles", { name: name.value, account: account.value, browser: "chromium" });
      toast((res.created ? "Created " : "Already exists: ") + res.profile.name);
      lastCreated = "Start it: " + res.run_command + "  ·  Install as a service: " + res.service_command;
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
  setTimeout(function () { toastEl.classList.remove("show"); }, 4000);
}
