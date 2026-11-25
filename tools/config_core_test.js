// Tests for tools/config-core.js, the logic of tools/config.html (the
// config explorer), against the sample behaviour document and the page's
// own definitions: the sample has no problems, and each kind of problem
// washhouse refuses a document for is found.
//
// Usage: node tools/config_core_test.js
"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const C = require("./config-core.js");

let pass = 0, fail = 0;
function test(name, fn) {
  try { fn(); pass++; } catch (e) { fail++; console.log(`FAIL: ${name}\n  ${e.message.split("\n").join("\n  ")}`); }
}
const page = fs.readFileSync(path.join(__dirname, "config.html"), "utf8");
const block = id => JSON.parse(page.match(new RegExp(`<script type="application/json" id="${id}">([\\s\\S]*?)</script>`))[1]);
const DEFS = block("defs"), SAMPLE = block("sample");
const clone = x => JSON.parse(JSON.stringify(x));
const problemsAt = (res, p) => (res.rows.find(r => r.path === p) || {problems: []}).problems;

test("the sample has no problems", () => {
  const res = C.review(SAMPLE, DEFS, null);
  const bad = res.rows.filter(r => r.problems.length).map(r => `${r.path}: ${r.problems}`);
  assert.deepEqual(bad, []);
  assert.deepEqual(res.cross, []);
});
test("every definition has the fields the page shows", () => {
  for (const [k, d] of Object.entries(DEFS)) {
    assert.ok(d.meaning && d.type, `${k}: meaning and type`);
    assert.ok(typeof d.rules === "string", `${k}: rules`);
  }
});
test("a missing key is found", () => {
  const d = clone(SAMPLE); delete d.thermal.heating_on_after_draw_kw;
  assert.match(problemsAt(C.review(d, DEFS, SAMPLE), "thermal.heating_on_after_draw_kw").join(), /missing/);
});
test("a missing key in one tank is found", () => {
  const d = clone(SAMPLE); delete d.tanks[1].prior_outlet_underread;
  assert.match(problemsAt(C.review(d, DEFS, SAMPLE), "tanks[1].prior_outlet_underread").join(), /missing/);
});
test("an unknown key is found", () => {
  const d = clone(SAMPLE); d.thermal.heating_on_watts = 500;
  assert.match(problemsAt(C.review(d, DEFS, SAMPLE), "thermal.heating_on_watts").join(), /unknown key/);
});
test("a wrong type is found", () => {
  const d = clone(SAMPLE); d.thermal.power_window = 600;
  assert.match(problemsAt(C.review(d, DEFS, SAMPLE), "thermal.power_window").join(), /duration/);
  const e = clone(SAMPLE); e.recent_draws = 4.5;
  assert.match(problemsAt(C.review(e, DEFS, SAMPLE), "recent_draws").join(), /whole number/);
});
test("a null is found", () => {
  const d = clone(SAMPLE); d.cold_c = null;
  assert.match(problemsAt(C.review(d, DEFS, SAMPLE), "cold_c").join(), /null/);
});
test("a role outside its vocabulary is found", () => {
  const d = clone(SAMPLE); d.tanks[0].probes[2].role = "middle";
  assert.match(problemsAt(C.review(d, DEFS, SAMPLE), "tanks[0].probes[2].role").join(), /isn't one of/);
});
test("a value out of range is found", () => {
  const d = clone(SAMPLE); d.tanks[0].prior_outlet_underread = 1;
  assert.ok(problemsAt(C.review(d, DEFS, SAMPLE), "tanks[0].prior_outlet_underread").length > 0);
  const e = clone(SAMPLE); e.estimate.low_factor = 1.5;
  assert.ok(problemsAt(C.review(e, DEFS, SAMPLE), "estimate.low_factor").length > 0);
});
test("rules between keys are found", () => {
  const cases = [
    [d => { d.estimate.draw_to_mm = 200; }, /draw_to_mm must be above/],
    [d => { d.thermal.heating_off_kw = 0.9; }, /heating_off_kw must be at most/],
    [d => { d.thermal.heating_on_after_draw_kw = 0.2; }, /heating_on_after_draw_kw must be at least/],
    [d => { d.thermal.power_min_span = "20m"; }, /power_min_span must be within/],
    [d => { d.tanks[0].outlet_split = 0.5; }, /sum to 1/],
    [d => { d.tanks[1].device_id = d.tanks[0].device_id; }, /used twice/],
  ];
  for (const [mut, re] of cases) {
    const d = clone(SAMPLE); mut(d);
    const res = C.review(d, DEFS, SAMPLE);
    assert.ok(res.cross.some(c => re.test(c.problem)), `${re}: ${JSON.stringify(res.cross)}`);
  }
});
test("a duplicate key, ignoring case, is found in the raw text", () => {
  assert.deepEqual(C.findDuplicateKeys('{"cold_c": 10, "Cold_C": 11, "a": {"x": "{\\"cold_c\\": 1}"}}'), ["Cold_C"]);
  assert.deepEqual(C.findDuplicateKeys(JSON.stringify(SAMPLE)), []);
});
test("what differs from the sample is marked", () => {
  const d = clone(SAMPLE); d.low_hot_water_l = 30; d.tanks[0].prior_outlet_underread = 0.15;
  const res = C.review(d, DEFS, SAMPLE);
  assert.equal(res.rows.find(r => r.path === "low_hot_water_l").differs, true);
  assert.equal(res.rows.find(r => r.path === "tanks[0].prior_outlet_underread").differs, true);
  assert.equal(res.rows.find(r => r.path === "cold_c").differs, false);
  assert.equal(res.differs, 2);
});
test("durations parse as Go's", () => {
  assert.equal(C.seconds("90s"), 90); assert.equal(C.seconds("1h30m"), 5400); assert.equal(C.seconds("2700s"), 2700);
  assert.equal(C.seconds("15"), null); assert.equal(C.seconds("1d"), null);
});
test("the page loads the core first and never sends anything", () => {
  assert.ok(page.indexOf('src="config-core.js"') < page.indexOf('"use strict"'));
  assert.ok(!/fetch\(|XMLHttpRequest|\.publish\(/.test(page));
});
test("the page's script parses", () => {
  new Function(page.slice(page.lastIndexOf("<script>") + 8, page.lastIndexOf("</script>")));
});

console.log(`config_core_test: ${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
