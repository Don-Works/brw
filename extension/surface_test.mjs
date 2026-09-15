// Surface guards for the two operator-facing pages.
//
// These are enumeration guards, not spot checks: each derives what it expects
// from the source of truth and fails on a member nobody wired up, so adding a
// badge mode or renaming an element cannot silently skip them.

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const read = (name) => readFileSync(join(here, name), "utf8");

let failures = 0;
function check(name, fn) {
  try {
    fn();
    console.log(`ok   ${name}`);
  } catch (error) {
    failures += 1;
    console.error(`FAIL ${name}\n     ${error.message}`);
  }
}

// ── 1. The popup legend decodes the toolbar badge ──────────────────────────
//
// The legend's whole job is to tell an operator what the colour on their
// toolbar means. If popup.css drifts from the BADGE_*_BG constants the legend
// is not merely ugly, it is wrong: it names a colour Chrome never paints.

check("popup legend dots match the toolbar badge colours", () => {
  const worker = read("service_worker.js");
  const css = read("popup.css");

  // Every mode applyBadgeFrame can paint, paired with the constant it uses.
  // BADGE_AGENT_PULSE_BG and BADGE_CONNECTING_DIM_BG are the second animation
  // frame of a mode the legend already lists, so they get no legend row of
  // their own; the resting frame is what the legend names.
  const wanted = {
    connected: "BADGE_IDLE_BG",
    used: "BADGE_AGENT_BG",
    connecting: "BADGE_CONNECTING_BG",
    disconnected: "BADGE_DOWN_BG"
  };

  const constants = {};
  for (const match of worker.matchAll(/const (BADGE_\w+_BG)\s*=\s*"(#[0-9a-fA-F]{6})"/g)) {
    constants[match[1]] = match[2].toLowerCase();
  }
  if (Object.keys(constants).length === 0) {
    throw new Error("no BADGE_*_BG constants found; this guard is reading the wrong file");
  }

  const used = new Set(Object.values(wanted));
  const unclassified = Object.keys(constants).filter(
    (name) => !used.has(name) && !/PULSE|DIM/.test(name)
  );
  if (unclassified.length) {
    throw new Error(
      "service_worker.js declares badge colours this guard does not classify: " +
      `${unclassified.join(", ")}. Give each one a legend row, or the animation-frame exemption.`
    );
  }

  for (const [mode, constant] of Object.entries(wanted)) {
    const expected = constants[constant];
    if (!expected) throw new Error(`service_worker.js no longer declares ${constant}`);

    const rule = new RegExp(`\\.dot\\.${mode}\\s*\\{[^}]*background:\\s*(#[0-9a-fA-F]{6})`, "i");
    const found = css.match(rule);
    if (!found) throw new Error(`popup.css has no literal .dot.${mode} background`);
    if (found[1].toLowerCase() !== expected) {
      throw new Error(`.dot.${mode} is ${found[1]} but the badge paints ${expected} (${constant})`);
    }
  }
});

// ── 2. Every element the page scripts address actually exists ──────────────
//
// getElementById on a renamed node returns null and the page throws mid-render,
// leaving whatever the previous pass wrote on screen. That reads as a stale
// status rather than a crash, so it survives a casual look at the page.

check("every getElementById target exists in its page", () => {
  for (const [script, page] of [["options.js", "options.html"], ["popup.js", "popup.html"]]) {
    const js = read(script);
    const html = read(page);
    const ids = new Set([...js.matchAll(/getElementById\(\s*"([^"]+)"\s*\)/g)].map((m) => m[1]));
    if (ids.size === 0) {
      throw new Error(`${script} matched no getElementById calls; this guard is not reading it`);
    }
    const missing = [...ids].filter((id) => !html.includes(`id="${id}"`));
    if (missing.length) {
      throw new Error(`${script} addresses ids absent from ${page}: ${missing.join(", ")}`);
    }
  }
});

// ── 3. The consent disclosure keeps every word ─────────────────────────────
//
// The Chrome Web Store listing rests on this wording. Collapsing it into a
// <details> once the decision is made is a layout choice; dropping a clause is
// a different thing entirely.

check("the consent disclosure still carries each required clause", () => {
  const html = read("options.html");
  const required = [
    "personal, communication, financial, health, location or authentication information",
    "Don Works and Revitt do not receive the data",
    "Refuses HttpOnly cookie and bulk-storage CDP methods",
    "can be disabled here at any time",
    "under its privacy terms"
  ];
  const missing = required.filter((clause) => !html.includes(clause));
  if (missing.length) {
    throw new Error(`options.html no longer discloses: ${missing.map((m) => JSON.stringify(m)).join(", ")}`);
  }
  if (!/<details[^>]*id="consentMore"/.test(html)) {
    throw new Error("the consentMore disclosure is gone; options.js addresses it and will throw on load");
  }
});

// ── 4. No raw exception text reaches either page ───────────────────────────
//
// Both pages route failures through a humanizer that names the problem and the
// recovery. A textContent assignment taking a caught error directly undoes
// that for exactly the reader who is already having a bad day.

check("caught errors are humanized before they are shown", () => {
  for (const script of ["options.js", "popup.js"]) {
    const js = read(script);
    const raw = [...js.matchAll(/textContent\s*=\s*(?:String\()?\s*(error|err|e)\b[^;]*/g)]
      .map((m) => m[0].trim());
    if (raw.length) {
      throw new Error(`${script} puts a caught error straight on screen: ${raw.join(" | ")}`);
    }
  }
});

if (failures) {
  console.error(`\n${failures} surface guard(s) failed`);
  process.exit(1);
}
console.log("\nall surface guards passed");
