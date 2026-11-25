// The pure part of tools/config.html, the behaviour config explorer: it
// flattens a behaviour document (the washhouse namespace) into key paths,
// checks each value against its definition, applies the rules between
// keys, and finds what is missing, unknown or different from the sample.
// No DOM, so tools/config_core_test.js runs it under node.
//
// In a browser it sets window.ConfigCore; under node it is a module.
"use strict";
(function (root) {
  // A definition's path uses [] for "each element": tanks[].probes[].role.
  const generic = path => path.replace(/\[\d+\]/g, "[]");

  // flatten lists every key of doc as {path, value}: objects and arrays
  // themselves too (path "tanks", "tanks[0]"), then their members.
  function flatten(doc) {
    const out = [];
    const walk = (v, path) => {
      if (path) out.push({path, value: v});
      if (Array.isArray(v)) v.forEach((x, i) => { if (x && typeof x === "object") walk(x, `${path}[${i}]`); });
      else if (v && typeof v === "object") for (const [k, x] of Object.entries(v)) walk(x, path ? `${path}.${k}` : k);
    };
    walk(doc, "");
    return out;
  }

  // Go's time.ParseDuration: one or more number+unit pairs ("1h30m", "90s").
  const DUR = /^[-+]?(\d+(\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h)((\d+(\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h))*$/;
  const UNIT = {ns: 1e-9, us: 1e-6, "µs": 1e-6, "μs": 1e-6, ms: 1e-3, s: 1, m: 60, h: 3600};
  function seconds(s) {
    if (typeof s !== "string" || !DUR.test(s)) return null;
    let total = 0;
    for (const [, n, , , u] of s.replace(/^[-+]/, "").matchAll(/((\d+(\.\d*)?|\.\d+))(ns|us|µs|μs|ms|s|m|h)/g)) total += parseFloat(n) * UNIT[u];
    return s.startsWith("-") ? -total : total;
  }
  function fmtSeconds(x) {
    if (x == null) return "";
    if (x >= 3600 && x % 3600 === 0) return `${x / 3600} h`;
    if (x >= 60 && x % 60 === 0) return `${x / 60} min`;
    return `${x} s`;
  }

  // check is the problems with one value against its definition: [] if none.
  function check(def, v) {
    const p = [];
    if (v === null) return ["is null: every key must have a value"];
    const c = def.check || {};
    let n = null;
    switch (def.type) {
      case "number": if (typeof v !== "number") p.push("should be a number"); else n = v; break;
      case "integer": if (!Number.isInteger(v)) p.push("should be a whole number"); else n = v; break;
      case "string": if (typeof v !== "string") p.push("should be a string"); break;
      case "boolean": if (typeof v !== "boolean") p.push("should be true or false"); break;
      case "duration":
        n = seconds(v);
        if (n == null) p.push(`should be a duration string such as "90s" or "15m"`);
        break;
      case "array": if (!Array.isArray(v)) p.push("should be a list"); break;
      case "object": if (!v || typeof v !== "object" || Array.isArray(v)) p.push("should be an object"); break;
    }
    if (p.length) return p;
    const shown = x => def.type === "duration" ? fmtSeconds(x) : String(x);
    if (n != null) {
      if (c.min != null && (c.exclusiveMin ? !(n > c.min) : !(n >= c.min))) p.push(`must be ${c.exclusiveMin ? "above" : "at least"} ${shown(c.min)}`);
      if (c.max != null && (c.exclusiveMax ? !(n < c.max) : !(n <= c.max))) p.push(`must be ${c.exclusiveMax ? "below" : "at most"} ${shown(c.max)}`);
    }
    if (c.enum) {
      const vals = Array.isArray(v) ? v : [v];
      const bad = vals.filter(x => !c.enum.includes(x));
      if (bad.length) p.push(`${bad.map(x => JSON.stringify(x)).join(", ")} isn't one of ${c.enum.map(x => JSON.stringify(x)).join(", ")}`);
    }
    if (c.pattern && typeof v === "string" && !new RegExp(c.pattern).test(v)) p.push(`doesn't match ${c.pattern}`);
    if (c.minItems != null && Array.isArray(v) && v.length < c.minItems) p.push(`needs at least ${c.minItems}`);
    return p;
  }

  // The rules between keys the validators enforce (each also in its keys'
  // "rules" text). Each takes the document and returns a problem or null.
  const num = x => typeof x === "number" ? x : null;
  const CROSS = [
    {keys: ["estimate.draw_from_mm", "estimate.draw_to_mm"], test: d => {
      const a = num(d.estimate?.draw_from_mm), b = num(d.estimate?.draw_to_mm);
      return a != null && b != null && !(b > a) ? "draw_to_mm must be above draw_from_mm" : null; }},
    {keys: ["estimate.min_c", "estimate.mix_c", "cold_c"], test: d => {
      const mn = num(d.estimate?.min_c), mx = num(d.estimate?.mix_c), c = num(d.cold_c);
      if (mx != null && c != null && !(mx > c)) return "mix_c must be above cold_c";
      if (mn != null && mx != null && mn < mx) return "min_c must be at least mix_c";
      return null; }},
    {keys: ["thermal.heating_off_kw", "thermal.heating_on_kw", "thermal.heating_on_after_draw_kw"], test: d => {
      const off = num(d.thermal?.heating_off_kw), on = num(d.thermal?.heating_on_kw), ad = num(d.thermal?.heating_on_after_draw_kw);
      if (off != null && on != null && off > on) return "heating_off_kw must be at most heating_on_kw";
      if (on != null && ad != null && ad < on) return "heating_on_after_draw_kw must be at least heating_on_kw";
      return null; }},
    {keys: ["thermal.power_min_span", "thermal.power_window"], test: d => {
      const s = seconds(d.thermal?.power_min_span), w = seconds(d.thermal?.power_window);
      return s != null && w != null && s > w ? "power_min_span must be within power_window" : null; }},
    {keys: ["thermal.power_settle", "thermal.draw_settle"], test: d => {
      const p = seconds(d.thermal?.power_settle), s = seconds(d.thermal?.draw_settle);
      return p != null && s != null && p < s ? "power_settle must be at least draw_settle" : null; }},
    {keys: ["tanks[].outlet_split"], test: d => {
      if (!Array.isArray(d.tanks)) return null;
      const xs = d.tanks.map(t => t && t.outlet_split);
      if (!xs.every(x => typeof x === "number")) return null;
      const sum = xs.reduce((a, b) => a + b, 0);
      return Math.abs(sum - 1) > 1e-9 ? `the tanks' outlet_split must sum to 1 (they sum to ${+sum.toFixed(6)})` : null; }},
    {keys: ["tanks[].device_id"], test: d => {
      if (!Array.isArray(d.tanks)) return null;
      const ids = d.tanks.map(t => t && t.device_id).filter(x => typeof x === "string");
      const dup = ids.find((x, i) => ids.indexOf(x) !== i);
      return dup ? `device_id ${JSON.stringify(dup)} is used twice` : null; }},
    {keys: ["draw.thermocline_c", "cold_c"], test: d => {
      const t = num(d.draw?.thermocline_c), c = num(d.cold_c);
      return t != null && c != null && !(t > c) ? "draw.thermocline_c must be above cold_c" : null; }},
    {keys: ["thermal.power_window", "thermal.ua_quiet"], test: d => {
      const w = seconds(d.thermal?.power_window), q = seconds(d.thermal?.ua_quiet);
      return w != null && q != null && w > q ? "power_window must be within ua_quiet" : null; }},
    {keys: ["draw.share_settle", "draw.balance_settle"], test: d => {
      const a = seconds(d.draw?.share_settle), b = seconds(d.draw?.balance_settle);
      return a != null && b != null && a > b ? "share_settle must be at most balance_settle" : null; }},
    {keys: ["vessel.dome_depth_mm", "vessel.inner_diameter_mm"], test: d => {
      const h = num(d.vessel?.dome_depth_mm), di = num(d.vessel?.inner_diameter_mm);
      return h != null && di != null && h > di / 2 ? "dome_depth_mm must be at most half inner_diameter_mm" : null; }},
    {keys: ["tanks[].probes[].position"], test: d => {
      for (const t of Array.isArray(d.tanks) ? d.tanks : []) {
        const ps = (Array.isArray(t?.probes) ? t.probes : []).map(p => p?.position);
        const dup = ps.find((x, i) => ps.indexOf(x) !== i);
        if (dup !== undefined) return `probe position ${JSON.stringify(dup)} is used twice in ${t.device_id}`;
      }
      return null; }},
    {keys: ["draw.gaps[].low", "draw.gaps[].high", "tanks[].probes[].position"], test: d => {
      const gaps = Array.isArray(d.draw?.gaps) ? d.draw.gaps : [];
      for (const g of gaps) for (const t of Array.isArray(d.tanks) ? d.tanks : []) {
        const ps = (Array.isArray(t?.probes) ? t.probes : []).map(p => p?.position);
        for (const pos of [g?.low, g?.high]) if (typeof pos === "string" && !ps.includes(pos)) return `draw.gaps names position ${JSON.stringify(pos)}, which ${t.device_id} has no probe at`;
      }
      return null; }},
  ];

  // review is the whole picture of doc against defs (and sample, if
  // given): one row per key path, plus missing and unknown keys.
  function review(doc, defs, sample) {
    const rows = [];
    const flat = flatten(doc);
    const have = new Set(flat.map(f => f.path));
    // Compared by exact path, so each tank is set against the same tank of
    // the sample, not whichever tank came last.
    const sampleFlat = sample ? new Map(flatten(sample).map(f => [f.path, f.value])) : null;
    for (const f of flat) {
      const g = generic(f.path);
      const def = defs[g];
      const row = {path: f.path, generic: g, value: f.value, def, problems: [], differs: false};
      if (!def) row.problems.push("unknown key: washhouse refuses the whole document");
      else row.problems.push(...check(def, f.value));
      const scalar = def && !["object", "array"].includes(def.type);
      const list = def && def.type === "array" && !["tanks", "draw.gaps"].includes(g) && !g.endsWith("probes");
      if (sampleFlat && (scalar || list) && sampleFlat.has(f.path)) {
        row.differs = JSON.stringify(sampleFlat.get(f.path)) !== JSON.stringify(f.value);
      }
      rows.push(row);
    }
    // Missing: a definition's key absent under a parent that is present.
    for (const g of Object.keys(defs)) {
      const parts = g.split(".");
      const key = parts.pop(), parent = parts.join(".");
      const parents = parent === "" ? [""] : [...have].filter(p => generic(p) === parent);
      for (const p of parents) {
        const path = p ? `${p}.${key}` : key;
        if (!have.has(path) && !key.endsWith("[]")) rows.push({path, generic: g, value: undefined, def: defs[g], problems: ["missing: every key is required"], differs: false, missing: true});
      }
    }
    // Duplicates ignoring case can't be seen once parsed; the raw text check
    // is in findDuplicateKeys.
    const cross = [];
    for (const r of CROSS) { const p = r.test(doc); if (p) cross.push({keys: r.keys, problem: p}); }
    rows.sort((a, b) => a.path.localeCompare(b.path, undefined, {numeric: true}));
    return {rows, cross, problems: rows.filter(r => r.problems.length).length + cross.length, differs: rows.filter(r => r.differs).length};
  }

  // findDuplicateKeys finds keys repeated in one object, ignoring case,
  // in the raw text (JSON.parse keeps only the last; washhouse refuses them).
  function findDuplicateKeys(text) {
    const dups = [];
    const stack = [];
    // Every string is consumed whole (so a brace inside one is no brace);
    // it is a key only when a colon follows.
    const re = /"((?:[^"\\]|\\.)*)"(\s*:)?|[{}[\]]/g;
    let m;
    while ((m = re.exec(text))) {
      const t = m[0];
      if (t[0] === '"' && !m[2]) continue; // a string value
      if (t === "{") stack.push(new Set());
      else if (t === "[") stack.push(null);
      else if (t === "}" || t === "]") stack.pop();
      else {
        const set = stack[stack.length - 1];
        if (!set) continue;
        const k = m[1].toLowerCase();
        if (set.has(k)) dups.push(m[1]); else set.add(k);
      }
    }
    return dups;
  }

  const api = {generic, flatten, seconds, fmtSeconds, check, review, findDuplicateKeys, CROSS};
  if (typeof module === "object" && module.exports) module.exports = api; else root.ConfigCore = api;
})(typeof window !== "undefined" ? window : globalThis);
