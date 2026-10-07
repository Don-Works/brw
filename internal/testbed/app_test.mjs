import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const origin = new URL(process.argv[2]);
assert.equal(origin.hostname, "127.0.0.1");
const framesEnabled = process.argv[3] !== "none";
if (!framesEnabled) origin.searchParams.set("frames", "none");
const calls = [], tools = [], elements = new Map();
const runtime = { registerTool(tool) { tools.push(tool); } };
function element(id = "") {
  const listeners = {};
  const node = { id, dataset: {}, value: "", textContent: "", hidden: false, children: [], listeners,
    addEventListener(kind, fn) { (listeners[kind] ||= []).push(fn); }, setAttribute() {},
    replaceChildren(...children) { this.children = children; }, append(child) { this.children.push(child); },
    focus() {}, remove() { this.removed = true; }, querySelector() { return element(); }, attachShadow() { return element(); },
    getContext() { return new Proxy({}, { get: (_, name) => () => {} }); },
    tBodies: [{ insertRow: () => ({ insertCell: () => ({}) }) }]
  };
  return node;
}
const page = fs.readFileSync("web/index.html", "utf8");
for (const [, id] of page.matchAll(/\bid="([^"]+)"/g)) elements.set(id, element(id));
elements.get("choice-list").children = [element(), element(), element()];
const frames = [elements.get("same-frame"), elements.get("cross-frame"), element("opaque-frame")];
assert.equal((page.match(/<iframe\b/g) || []).length, frames.length);
const document = { modelContext: runtime, getElementById: id => { assert.ok(elements.has(id) && !elements.get(id).removed, `missing HTML id ${id}`); return elements.get(id); },
  createElement: () => element(), querySelector: () => element(), querySelectorAll: selector => { assert.equal(selector, "iframe"); return frames.filter(frame => !frame.removed); }, dispatchEvent() {} };
class WebSocket { static OPEN = 1; readyState = 1; send() {} close() {} }
class EventSource { close() {} addEventListener() {} }
const context = vm.createContext({ document, navigator: {}, window: { addEventListener() {} }, location: origin, URL, URLSearchParams, WebSocket, EventSource,
  setTimeout, clearTimeout, setInterval, clearInterval, Date, console,
  fetch: async (path, options) => { calls.push({ path, options }); return fetch(new URL(path, origin), options); }
});
vm.runInContext(fs.readFileSync("web/app.js", "utf8"), context, { filename: "app.js" });
for (let count = 0; tools.length < 2 && count < 100; count++) await new Promise(resolve => setTimeout(resolve, 10));
assert.deepEqual(tools.map(tool => tool.name), ["fixture_read_report", "fixture_delete_account"]);
assert.equal(tools[0].annotations.readOnlyHint, true);
assert.equal(tools[1].annotations.readOnlyHint, false);
assert.equal(tools[1].annotations.destructiveHint, true);
assert.equal(typeof tools[1].execute, "function");
assert.equal(frames.filter(frame => !frame.removed).length, framesEnabled ? 3 : 0);
if (framesEnabled) { assert.match(frames[0].src, /^\/frame\?version=1$/); assert.match(frames[1].src, /\/frame\?version=1$/); }
const before = await fetch(new URL("/api/state", origin)).then(response => response.json());
const report = await tools[0].execute({});
assert.equal(report.site_code, before.reading.facts.site_code);
assert.equal(report.water_litres, before.reading.facts.water_litres);
assert.equal(report.observer, before.reading.facts.observer);
assert.ok(!calls.some(call => call.path === "/api/action"), "read-only tool performed a fixture write");
await elements.get("hover-target").onmouseenter();
await elements.get("note").listeners.focus[0]();
const after = await fetch(new URL("/api/state", origin)).then(response => response.json());
assert.equal(after.action_counts.hover, 1);
assert.equal(after.action_counts.focus, 1);
assert.equal(after.focus_name, "Fixture note");
assert.equal(after.action_counts["webmcp-mutation"] || 0, 0);
assert.equal(after.form_state.account_deleted, false);
const oldButton = elements.get("action-slot").children[0];
const reset = await fetch(new URL("/api/reset", origin), { method: "POST", body: JSON.stringify({ seed: 17, chaos: 3, max_events: 12 }) }).then(response => response.json());
context.resetState = reset;
vm.runInContext("initialize(resetState)", context);
vm.runInContext("renderView({...resetState, frame_version: 2}, 'frame')", context);
assert.equal(frames.filter(frame => !frame.removed).length, framesEnabled ? 3 : 0);
if (framesEnabled) { assert.match(frames[0].src, /version=2$/); assert.match(frames[1].src, /version=2$/); }
await oldButton.onclick();
const final = await fetch(new URL("/api/state", origin)).then(response => response.json());
assert.equal(final.action_counts["stable-action"] || 0, 0, "old node committed into a reset run with the same epoch");
console.log("actual embedded read-only WebMCP registration/result and hover/focus bindings pass; destructive tool never invoked");
