// The pure part of tools/live.html: how each message is classified,
// summarised and explained, and the figures the page derives. No DOM, no
// MQTT, so tools/live_core_test.js runs it under node against the wire
// goldens (washhouse's and tanktemp's, in common/wire).
//
// In a browser it sets window.LiveCore; under node it is a module.
"use strict";
(function (root) {
  const num = (v, d = 1) => v == null || Number.isNaN(Number(v)) ? "—" : Number(v).toFixed(d);
  const short = id => String(id).split("_").pop();            // hot_water_cylinder_right -> right
  const onoff = v => v == null ? "unknown" : v ? "on" : "off";

  function fmtDur(s) {
    if (s == null || !Number.isFinite(Number(s))) return "—";
    s = Math.round(s);
    const d = Math.floor(s / 86400), h = Math.floor(s % 86400 / 3600), m = Math.floor(s % 3600 / 60);
    return d ? `${d}d ${h}h` : h ? `${h}h ${m}m` : m ? `${m}m ${s % 60}s` : `${s}s`;
  }
  function fmtMs(ms) {
    if (ms == null) return "—";
    return ms >= 60000 ? fmtDur(ms / 1000) : (ms / 1000).toFixed(ms < 10000 ? 1 : 0) + " s";
  }

  // Why washhouse calls a draw's cross-check unreliable (docs/WIRE.md,
  // draw_balanced's reasons). A reason not here is shown as it is.
  const REASONS = {
    small: "under 20 L: too little for the probes to measure",
    thermocline_gap: "the cold layer sat between two widely spaced probes, so they barely saw it",
    gap_unread: "a probe the cold layer passed had no reading",
    hw_on: "the hot-water timer was on: a reheat hides the draw",
    tank_gained: "a cylinder gained heat across the draw: a reheat",
    hw_unknown: "the programmer's state was unknown",
    interrupted: "the next draw started before the probes settled",
    shares_early: "the split came from the sweep a minute after, not the settled one",
    no_loss: "no heat was lost to balance",
    tank_missing: "a cylinder's sweeps were missing",
  };
  const STATE_WHY = {
    heating: "its stored heat is rising faster than a threshold",
    cooling: "losing heat to the room and its pipework",
    drawing: "water is being drawn: both cylinders feed the outlet",
    unknown: "not judged yet: at start, after a draw while the probes settle (about 17 min), or stale data",
  };

  // classify a topic under washhouse's base and tanktemp's prefix.
  function classify(topic, base, tt) {
    if (topic === base + "/state") return "w-state";
    if (topic.startsWith(base + "/tank/")) return "w-tank";
    if (topic === base + "/events") return "w-event";
    if (topic === base + "/status") return "w-status";
    if (topic === tt + "/status") return "t-status";
    if (topic === tt + "/meter") return "t-meter";
    if (topic.startsWith(tt + "/") && topic.endsWith("/state")) return "t-state";
    return "other";
  }
  // The log's filter group a class belongs to.
  const group = cls => cls === "t-status" || cls === "w-status" ? "status" : cls.startsWith("t-") ? "tt" : cls === "w-event" ? "events" : "state";

  const METER_WHY = {
    pulse: "A litre went through the meter: the reed closed.",
    open: "The reed opened again, ending the litre: how long it was closed says how fast the water ran.",
    sweep: "The meter's count, sent with each probe sweep so it is never older than a sweep.",
  };

  // describe a message: {kind, summary, meaning}. kind picks its colour.
  function describe(cls, m) {
    if (!m || typeof m !== "object") return {kind: "OTHER", summary: "(not JSON)", meaning: ""};
    switch (cls) {
      case "t-state": {
        const ss = m.sensors || [], ok = ss.filter(s => s.temp_c != null).length;
        return {kind: ok === ss.length ? "SWEEP" : "SWEEP_FAIL",
          summary: `${short(m.device_id)}: ${ss.map(s => s.temp_c == null ? s.position + " " + s.error : s.position + " " + Number(s.temp_c).toFixed(1)).join(" · ")}`,
          meaning: `tanktemp's raw probes on the ${short(m.device_id)} cylinder, ${ok} of ${ss.length} reading${m.clock_synced === false ? "; its clock isn't synced, so washhouse skips this sweep" : ""}.`};
      }
      case "t-meter":
        return {kind: "METER",
          summary: `${m.trigger || "meter"} · count ${m.count} · seq ${m.seq}${m.last_closed_ms != null ? " · closed " + fmtMs(m.last_closed_ms) : ""}`,
          meaning: METER_WHY[m.trigger] || "The meter's reading."};
      case "t-status":
        return {kind: m.event === "LWT" ? "LWT" : "STATUS", summary: `tanktemp ${m.event}${m.version ? " · " + m.version : ""}`,
          meaning: m.event === "LWT" ? "tanktemp vanished: the broker sent its last will." : `tanktemp, the probe and meter reader, is ${m.online === false ? "down" : "up"}.`};
      case "w-status":
        return {kind: m.event === "LWT" ? "LWT" : "STATUS",
          summary: `washhouse ${m.event}${m.version ? " · " + m.version : ""}${m.uptime_s != null ? " · up " + fmtDur(m.uptime_s) : ""}`,
          meaning: {STARTUP: "washhouse started.", HEARTBEAT: "washhouse's regular I'm-alive.", SHUTDOWN: "washhouse stopped cleanly.",
            LWT: "washhouse vanished: the broker sent its last will."}[m.event] || ""};
      case "w-state": {
        const ad = m.active_draw, p = m.partial;
        if (p) return {kind: "PARTIAL",
          summary: `plan on at least ${num(p.usable_l40_low, 0)} L · ${p.missing.map(short).join(", ")} blind${m.low_hot_water === false ? " · not low" : ""}${ad ? ` · drawing ${ad.litres} L` : ""}`,
          meaning: `A cylinder is blind (its probes can't be read), so the house's totals are unknown. washhouse gives what the others hold instead: a lower bound, since the blind ${p.missing.length === 1 ? "one" : "ones"} can only add to it.`};
        return {kind: m.low_hot_water ? "LOW" : "WSTATE",
          summary: `plan on ${num(m.usable_l40_low, 0)} L (up to ${num(m.usable_l40, 0)}) · ${num(m.stored_kwh, 2)} kWh${m.low_hot_water ? " · LOW" : ""}${ad ? ` · drawing ${ad.litres} L` : ""}`,
          meaning: ad ? "The house's state, mid-draw: published with every litre." : "The house's state, published when anything in it changes: about every sweep."};
      }
      case "w-tank":
        if (m.fault && m.stored_kwh != null) return {kind: "RESTORING",
          summary: `${short(m.device_id)} reading again · lost since ${String(m.fault.since).slice(11, 19)} · mean ${num(m.mean_c)} °C`,
          meaning: `The ${short(m.device_id)} cylinder reads normally again, but washhouse calls it restored only once it has for two minutes (tank_restored), so a flickering connection doesn't come and go. Its figures are back already.`};
        if (m.fault) return {kind: "FAULT",
          summary: `${short(m.device_id)} BLIND since ${String(m.fault.since).slice(11, 19)} · ${m.fault.reason}${errorsText(m.fault.errors)}`,
          meaning: `washhouse can't read the ${short(m.device_id)} cylinder: ${FAULT_WHY[m.fault.reason] || m.fault.reason}. Its figures are unknown until it reads again.`};
        return {kind: "WTANK",
          summary: `${short(m.device_id)} ${m.state}${m.heat_source ? " (" + m.heat_source + ")" : ""} · mean ${num(m.mean_c)} °C · ${m.power_kw == null ? "power unknown" : num(m.power_kw, 2) + " kW"}`,
          meaning: `washhouse's reading of the ${short(m.device_id)} cylinder: ${STATE_WHY[m.state] || m.state}.`};
      case "w-event": return describeEvent(m);
    }
    return {kind: "OTHER", summary: JSON.stringify(m).slice(0, 160), meaning: ""};
  }

  function describeEvent(m) {
    const e = m.event;
    if (e === "draw_started" || e === "draw_updated") {
      const d = m.active_draw || {};
      return {kind: "DRAW", summary: `${e} · ${d.litres} L${d.flow_l_min != null ? " at " + num(d.flow_l_min) + " L/min" : ""} · ${d.kind} (so far)`,
        meaning: e === "draw_started" ? "The first metered litre: someone has turned on a hot tap." : "The draw so far. Its kind is provisional until it ends: a shower starts as a tap."};
    }
    if (e === "draw_ended") {
      const d = m.draw_ended || {};
      return {kind: "DRAW", summary: `draw_ended · ${d.litres} L in ${fmtDur(d.duration_s)}${d.flow_l_min != null ? " at " + num(d.flow_l_min) + " L/min" : ""} · ${d.kind}`,
        meaning: `The meter's final account, a minute after the last litre: a ${d.kind}. Volume is good to about ±1 L.`};
    }
    if (e === "draw_balanced") {
      const d = m.draw_balanced || {};
      return {kind: "DRAW", summary: `draw_balanced · ${shareText(d) || "split not measured"} · probes say ${num(d.heat_balance_l, 1)} L${d.heat_balance_reliable ? "" : " (unreliable)"}`,
        meaning: "The probes have settled: who supplied the draw, and the heat it took. " +
          (d.reasons && d.reasons.length ? "Unreliable because " + d.reasons.map(r => REASONS[r] || r).join("; ") + "." : "The cross-check is good.")};
    }
    if (e === "continuous_flow_started" || e === "continuous_flow_ended") {
      const c = m.continuous_flow || {};
      return {kind: "CREEP", summary: `${e} · ${c.litres} L at ${num(c.flow_l_min, 3)} L/min`,
        meaning: e.endsWith("started") ? "Lone slow litres: a tap not quite closed, or water circulating through the meter. Never a draw." :
          "The creep has stopped: 45 min with no creep litre."};
    }
    if (e === "tank_state_changed" || e === "heating_started" || e === "heating_ended") {
      const t = m.tank || {}, who = `The ${short(t.device_id)} cylinder`;
      return {kind: e.startsWith("heating") ? "HEAT" : "TANKEV",
        summary: `${e} · ${short(t.device_id)}: ${t.prev} → ${t.state}${t.heat_source ? " (" + t.heat_source + ")" : ""}`,
        meaning: e === "heating_started" ? `${who} is heating${t.heat_source ? ", put down to the " + String(t.heat_source).toUpperCase() + " channel" : ""}.` :
          e === "heating_ended" ? (t.state === "unknown" ? `${who}'s readings were lost (it went blind or stale), so washhouse can no longer say it is heating; not that the heating was seen to stop.` : `${who} has stopped heating.`) : `${who} is now ${t.state}: ${STATE_WHY[t.state] || ""}.`};
    }
    if (e === "tank_lost") {
      const x = m.tank_lost || {};
      return {kind: "FAULT", summary: `tank_lost · ${short(x.device_id)}: ${x.reason}${errorsText(x.errors).replace(/^: (.*)$/, " ($1)")}`,
        meaning: `The ${short(x.device_id)} cylinder has been blind for five minutes: ${FAULT_WHY[x.reason] || x.reason}. washhouse's health is now degraded, and the house's figure is the other cylinder's, as a lower bound.`};
    }
    if (e === "tank_restored") {
      const x = m.tank_restored || {};
      return {kind: "RESTORED", summary: `tank_restored · ${short(x.device_id)} · blind for ${fmtDur(x.duration_s)}`,
        meaning: `The ${short(x.device_id)} cylinder has been read normally for two minutes, so it is back: the house's totals are whole again.`};
    }
    return {kind: "EVENT", summary: String(e), meaning: "An event this page doesn't know (new ones are additive)."};
  }

  // Why a cylinder is blind (WIRE.md, Faults).
  const FAULT_WHY = {
    probes: "its latest sweep came with too few readable probes. Every probe going bus_low, then not_present, is one whole bus failing at once (a connection or wiring fault), not five probes",
    stale: "no sweep has come from it for two minutes",
    tanktemp_offline: "tanktemp, the probe reader, is offline",
  };
  // errorsText: ": bus_low ×5", or "" when there are none.
  function errorsText(errs) {
    const ks = Object.keys(errs || {}).sort();
    return ks.length ? ": " + ks.map(k => `${k} ×${errs[k]}`).join(", ") : "";
  }

  // shareText: "left 11% · right 89%", or "" when the split wasn't measured.
  function shareText(b) {
    const tk = (b && b.tanks) || [];
    if (!tk.length || !tk.every(t => t.share != null)) return "";
    return tk.map(t => `${short(t.device_id)} ${Math.round(t.share * 100)}%`).join(" · ");
  }

  // crossing40: where the probes' profile, read upwards, first reaches
  // 40 °C, by straight lines between probes ({mm}); {mm: null, all:
  // "hot"|"cool"} when it never crosses; null with fewer than two readings
  // with heights. washhouse's own profile is finer; this is for the eye.
  function crossing40(sensors) {
    const p = (sensors || []).filter(s => s.temp_c != null && s.height_mm != null).sort((a, b) => a.height_mm - b.height_mm);
    if (p.length < 2) return null;
    if (p[0].temp_c >= 40) return {mm: null, all: "hot"};
    for (let i = 1; i < p.length; i++) if (p[i].temp_c >= 40) {
      const a = p[i - 1], b = p[i], f = (40 - a.temp_c) / (b.temp_c - a.temp_c);
      return {mm: a.height_mm + f * (b.height_mm - a.height_mm)};
    }
    return {mm: null, all: "cool"};
  }

  // lowWhy explains low_hot_water against the threshold, when the page was
  // given it (it isn't on the wire): which side of the band the water is.
  function lowWhy(u, flag, low, margin, partial) {
    if (u == null && partial) {
      const at = num(partial.usable_l40_low, 0), who = partial.missing.map(short).join(", ");
      if (flag === false) return `The ${who} cylinder is blind, but the others alone hold <b>at least ${at} L</b> to plan on, above the threshold and its margin, so low hot water is off whatever the blind one holds.`;
      return `The ${who} cylinder is blind and the others hold at least ${at} L, which isn't enough to be sure, so washhouse can't say whether hot water is low.`;
    }
    if (u == null) return "washhouse can't estimate the water yet: it needs a recent sweep from every cylinder.";
    if (low == null) return `washhouse says low hot water is <b>${onoff(flag)}</b>. Open the page with the namespace's threshold (live.sh --low and --margin) to see why.`;
    const clear = low + (margin || 0);
    if (u < low) return `<b>Below ${low} L</b>, so low hot water is on. It turns off only once the water to plan on is back above ${clear} L, which needs a reheat.`;
    if (u > clear) return `<b>Above ${clear} L</b>, so low hot water is off. It turns on below ${low} L.`;
    return `<b>Between ${low} and ${clear} L</b>, inside the margin, so low hot water holds what it was (${onoff(flag)}). The margin stops it flicking on and off at every sweep near the line.`;
  }

  // meterNews: what a meter message is against the last one seen (tanktemp's
  // WIRE.md): a replay (same counting_since and seq), a new counting run
  // (tanktemp restarted: a draw in progress was cut), or skipped messages
  // (seq jumped). Litres only come from count.
  function meterNews(prev, m) {
    if (!prev) return {dup: false, restarted: false, skipped: 0};
    if (prev.counting_since === m.counting_since && prev.seq === m.seq) return {dup: true, restarted: false, skipped: 0};
    if (prev.counting_since !== m.counting_since) return {dup: false, restarted: true, skipped: 0};
    return {dup: false, restarted: false, skipped: Math.max(0, m.seq - prev.seq - 1)};
  }

  const api = {num, short, onoff, fmtDur, fmtMs, REASONS, STATE_WHY, FAULT_WHY, errorsText, classify, group, describe, shareText, crossing40, lowWhy, meterNews};
  if (typeof module === "object" && module.exports) module.exports = api; else root.LiveCore = api;
})(typeof window !== "undefined" ? window : globalThis);
