// Tests for tools/live-core.js, the pure part of tools/live.html, against
// the wire goldens in common/wire (washhouse's and tanktemp's): every
// golden classifies, and is summarised and explained with no undefined or
// NaN; and the derived figures (the 40 °C crossing, the low-water
// explanation, the meter's news) are right.
//
// Usage: node tools/live_core_test.js [GOLDENS_DIR]
//   GOLDENS_DIR is common/wire's module directory (tools/test.sh passes it,
//   from `go list -m -f '{{.Dir}}' github.com/sweeney/common/wire`).
"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { execFileSync } = require("node:child_process");
const C = require("./live-core.js");

let pass = 0, fail = 0;
function test(name, fn) {
  try { fn(); pass++; } catch (e) { fail++; console.log(`FAIL: ${name}\n  ${e.message.split("\n").join("\n  ")}`); }
}
const clean = s => !/undefined|NaN|\[object /.test(s);

const dir = process.argv[2] || execFileSync("go", ["list", "-m", "-f", "{{.Dir}}", "github.com/sweeney/common/wire"],
  {cwd: path.join(__dirname, ".."), encoding: "utf8"}).trim();
const BASE = "water/hot", TT = "water/hot/tanktemp";
// A golden's topic, from its name: what the page would receive it on.
function topicOf(service, name) {
  if (service === "washhouse") {
    if (name.startsWith("event-")) return BASE + "/events";
    if (name.startsWith("state-")) return BASE + "/state";
    if (name.startsWith("tank-")) return BASE + "/tank/hot_water_cylinder_left";
    return BASE + "/status";                              // status-*, lwt
  }
  if (name.startsWith("state-")) return TT + "/hot_water_cylinder_left/state";
  if (name.startsWith("meter-")) return TT + "/meter";
  return TT + "/status";
}

const goldens = [];
for (const service of ["washhouse", "tanktemp"]) {
  const d = path.join(dir, service, "testdata");
  for (const f of fs.readdirSync(d).filter(f => f.endsWith(".golden")).sort()) {
    goldens.push({service, name: f, topic: topicOf(service, f), msg: JSON.parse(fs.readFileSync(path.join(d, f), "utf8"))});
  }
}
test("there are goldens of both services", () => {
  assert.ok(goldens.filter(g => g.service === "washhouse").length >= 10, "washhouse goldens");
  assert.ok(goldens.filter(g => g.service === "tanktemp").length >= 5, "tanktemp goldens");
});

const expectClass = {"w-": "washhouse", "t-": "tanktemp"};
for (const g of goldens) {
  test(`${g.service}/${g.name}: classified, summarised, explained`, () => {
    const cls = C.classify(g.topic, BASE, TT);
    assert.notEqual(cls, "other");
    assert.equal(expectClass[cls.slice(0, 2)], g.service);
    const d = C.describe(cls, g.msg);
    assert.ok(d.kind && d.kind !== "OTHER", `kind ${d.kind}`);
    assert.ok(d.summary && clean(d.summary), `summary: ${d.summary}`);
    assert.ok(d.meaning && clean(d.meaning), `meaning: ${d.meaning}`);
    assert.ok(["tt", "state", "events", "status"].includes(C.group(cls)));
  });
}

const byName = Object.fromEntries(goldens.map(g => [g.service + "/" + g.name, g]));
const desc = k => C.describe(C.classify(byName[k].topic, BASE, TT), byName[k].msg);

test("draw_balanced names the shares and the cross-check", () => {
  const d = desc("washhouse/event-draw-balanced.golden");
  assert.match(d.summary, /left 11% · right 89%/);
  assert.match(d.summary, /probes say 48\.0 L/);
  assert.match(d.meaning, /cross-check is good/);
});
test("a masked draw_balanced says the split wasn't measured, and why", () => {
  const d = desc("washhouse/event-draw-balanced-masked.golden");
  assert.match(d.summary, /split not measured/);
  assert.match(d.summary, /unreliable/);
  assert.match(d.meaning, /hot-water timer was on|reheat/);
});
test("draw_ended is the meter's account", () => {
  const d = desc("washhouse/event-draw-ended.golden");
  assert.match(d.summary, /50 L in 7m 36s at 6\.3 L\/min · shower/);
});
test("the meter's trigger is named", () => {
  assert.match(desc("tanktemp/meter-pulse.golden").summary, /^pulse · count 1 · seq 1/);
  assert.match(desc("tanktemp/meter-open.golden").meaning, /reed opened/);
});
test("a last will reads as one", () => {
  assert.equal(desc("washhouse/lwt.golden").kind, "LWT");
  assert.equal(desc("tanktemp/lwt.golden").kind, "LWT");
});
test("a failing probe is a SWEEP_FAIL", () => {
  assert.equal(desc("tanktemp/state-failures.golden").kind, "SWEEP_FAIL");
  assert.equal(desc("tanktemp/state-ok.golden").kind, "SWEEP");
});
test("an unknown event is accepted", () => {
  assert.equal(C.describe("w-event", {event: "something_new"}).kind, "EVENT");
});
test("topics outside both prefixes are other", () => {
  assert.equal(C.classify("water/hot/tanktemp/x/status", BASE, TT), "other");
  assert.equal(C.classify("zigbee2mqtt/room", BASE, TT), "other");
  assert.equal(C.classify("test/water/hot/state", "test/water/hot", TT), "w-state");
});

test("crossing40: between two probes, by straight lines", () => {
  const s = [{height_mm: 265, temp_c: 20}, {height_mm: 825, temp_c: 30}, {height_mm: 1215, temp_c: 50}];
  assert.equal(C.crossing40(s).mm, 1020);
});
test("crossing40: all hot, all cool, too few", () => {
  assert.deepEqual(C.crossing40([{height_mm: 1, temp_c: 41}, {height_mm: 2, temp_c: 60}]), {mm: null, all: "hot"});
  assert.deepEqual(C.crossing40([{height_mm: 1, temp_c: 12}, {height_mm: 2, temp_c: 39}]), {mm: null, all: "cool"});
  assert.equal(C.crossing40([{height_mm: 1, temp_c: 12}, {height_mm: 2, temp_c: null}]), null);
});
test("crossing40: order doesn't matter, failing probes are skipped", () => {
  const s = [{height_mm: 1215, temp_c: 50}, {height_mm: 825, temp_c: null}, {height_mm: 265, temp_c: 30}];
  assert.equal(Math.round(C.crossing40(s).mm), 740);
});

test("lowWhy: below, inside the margin, above", () => {
  assert.match(C.lowWhy(25, true, 30, 10), /Below 30 L.*above 40 L/);
  assert.match(C.lowWhy(35, true, 30, 10), /Between 30 and 40 L.*holds what it was \(on\)/);
  assert.match(C.lowWhy(35, false, 30, 10), /holds what it was \(off\)/);
  assert.match(C.lowWhy(45, false, 30, 10), /Above 40 L/);
});
test("lowWhy: unknown water, and no threshold given", () => {
  assert.match(C.lowWhy(null, null, 30, 10), /can't estimate/);
  assert.match(C.lowWhy(35, true, null, null), /low hot water is <b>on<\/b>.*--low/);
});

test("meterNews: a replay, a new run, skipped messages", () => {
  const a = {counting_since: "x", seq: 5};
  assert.deepEqual(C.meterNews(null, a), {dup: false, restarted: false, skipped: 0});
  assert.equal(C.meterNews(a, {counting_since: "x", seq: 5}).dup, true);
  assert.equal(C.meterNews(a, {counting_since: "y", seq: 1}).restarted, true);
  assert.equal(C.meterNews(a, {counting_since: "x", seq: 9}).skipped, 3);
  assert.equal(C.meterNews(a, {counting_since: "x", seq: 6}).skipped, 0);
});

test("fmtDur and fmtMs", () => {
  assert.equal(C.fmtDur(456.3), "7m 36s");
  assert.equal(C.fmtDur(90061), "1d 1h");
  assert.equal(C.fmtDur(null), "—");
  assert.equal(C.fmtMs(1541), "1.5 s");
  assert.equal(C.fmtMs(125000), "2m 5s");
});

// The page itself: read-only, its own storage key and client id, mqtt.js
// pinned, and the core loaded before its script.
const page = fs.readFileSync(path.join(__dirname, "live.html"), "utf8");
test("the page never publishes", () => assert.ok(!/\.publish\(/.test(page)));
test("the page puts no broker value into HTML raw (tanktemp's review of #11)", () => {
  // Litres and cylinder names reach innerHTML only through num() or sh()/esc().
  assert.ok(!/\$\{[A-Za-z_.]+\.litres\}/.test(page), "a raw ${x.litres}");
  assert.ok(!/\$\{short\(/.test(page), "a raw ${short(...)}");
});
test("the page subscribes at QoS 1", () => assert.match(page, /\{qos: 1\}/));
test("the page has its own storage key and client id", () => {
  assert.match(page, /"washhouse-live"/);
  assert.match(page, /"washhouse-live-" \+/);
  assert.ok(!/"tanktemp-live"/.test(page));
});
test("mqtt.js is pinned to an exact version on cdnjs", () => assert.match(page, /cdnjs\.cloudflare\.com\/ajax\/libs\/mqtt\/\d+\.\d+\.\d+\/mqtt\.min\.js/));
test("the core loads before the page's script", () => assert.ok(page.indexOf('src="live-core.js"') > 0 && page.indexOf('src="live-core.js"') < page.indexOf('"use strict"')));
test("the page's script parses", () => {
  const body = page.slice(page.lastIndexOf("<script>") + 8, page.lastIndexOf("</script>"));
  new Function(body);   // a syntax error throws here
});

console.log(`live_core_test: ${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
