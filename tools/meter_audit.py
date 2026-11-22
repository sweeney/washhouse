#!/usr/bin/env python3
"""Audit a recording of tanktemp's meter against its wire contract, and
summarise what the litres say. Post-hoc and read-only.

Input: recordings as tools/record.sh writes them ('%I %t %p' lines). Other
topics are read for context (boiler programmer events). Output: a report;
--json writes the machine-readable form too.

Contract checks (tanktemp docs/WIRE.md ## Meter):
  - field order; trigger/level vocabulary
  - de-duplicated on (counting_since, seq): seq gap-free within a run
  - count never falls as seq rises; how often it jumps (skipped messages)
  - every open follows its pulse; open time == last_pulse_at + last_closed_ms
  - noise/short/glitches/lost stay 0; when they rise, the nearest boiler event
  - sweeps at least every ~30 s (gaps > 45 s listed)
Interpretation (washhouse's, provisional):
  - draws: a draw ends when a minute passes with no litre, so litres less
    than 60 s apart are one draw (PLAN.md lessons §2), with volume and flow
  - duty: closed ms / interval ms per litre, the early-flow constant (plan §6.2)
  - suspect re-closes: a litre whose reed was open < 300 ms first (chatter)

Usage: meter_audit.py [--json OUT] [--gap-s 60] FILE...
"""
import argparse
import json
import statistics
import sys
from datetime import datetime, timedelta

METER_TOPIC = "water/hot/tanktemp/meter"
BOILER_EVENTS = "energy/boiler/sensor/events"
METER_KEYS = [
    "device_id", "timestamp", "clock_synced", "trigger", "seq",
    "litres_per_pulse", "level", "counting_since", "count", "last_pulse_at",
    "last_interval_ms", "last_closed_ms", "noise", "short", "glitches", "lost",
]
COUNTERS = ("noise", "short", "glitches", "lost")
SWEEP_GAP_S = 45
# Q4 (83 L/min) is a litre every 0.72 s with the reed closed ~23% of it, so a
# real litre's reed is open >= ~0.55 s before it closes. A shorter open is a
# chatter re-close (a creeping pointer at the magnet's edge), not a litre.
SUSPECT_OPEN_MS = 300
ATTRIBUTE_S = 5


def t(s):
    return None if s is None else datetime.fromisoformat(s.replace("Z", "+00:00"))


def iso(d):
    return None if d is None else d.strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3] + "Z"


def read(paths):
    """Yield (received, topic, payload) in file order."""
    for p in paths:
        with open(p, errors="replace") as f:
            for line in f:
                parts = line.rstrip("\n").split(" ", 2)
                if len(parts) < 3:
                    continue
                try:
                    rx = datetime.fromisoformat(parts[0])
                except ValueError:
                    continue
                yield rx, parts[1], parts[2]


class Audit:
    def __init__(self, gap_s=60.0):
        self.gap_s = gap_s
        self.problems = []      # (when, what)
        self.notes = []
        self.msgs = 0
        self.dups = 0
        self.runs = {}          # counting_since -> run dict
        self.boiler = []        # (time, event)
        self.order = []         # meter messages, de-duplicated, in arrival order

    def problem(self, when, what):
        self.problems.append((iso(when) if isinstance(when, datetime) else when, what))

    def feed(self, rx, topic, payload):
        if topic == BOILER_EVENTS:
            try:
                b = json.loads(payload)["boiler"]
                self.boiler.append((t(b["timestamp"]), b["event"]))
            except (ValueError, KeyError, TypeError):
                pass
            return
        if topic != METER_TOPIC:
            return
        self.msgs += 1
        try:
            pairs = json.loads(payload, object_pairs_hook=lambda kv: kv)
        except ValueError as e:
            self.problem(rx, f"meter payload not JSON: {e}")
            return
        keys = [k for k, _ in pairs]
        m = dict(pairs)
        if keys != METER_KEYS:
            self.problem(rx, f"field order {keys}")
        if m.get("trigger") not in ("pulse", "open", "sweep"):
            self.problem(rx, f"trigger {m.get('trigger')!r}")
        if m.get("level") not in ("open", "closed"):
            self.problem(rx, f"level {m.get('level')!r}")
        cs = m.get("counting_since")
        run = self.runs.get(cs)
        if run is None:
            run = self.runs[cs] = {"since": cs, "seen": set(), "last": None, "msgs": [],
                                   "max_seq": 0, "missing": []}
        seq = m.get("seq")
        if seq in run["seen"]:
            self.dups += 1
            return
        run["seen"].add(seq)
        m["_rx"] = rx
        run["msgs"].append(m)
        self.order.append(m)

    def finish(self):
        for run in self.runs.values():
            self._check_run(run)
        self.boiler.sort()

    def _check_run(self, run):
        msgs = sorted(run["msgs"], key=lambda m: m["seq"])
        seqs = [m["seq"] for m in msgs]
        first = seqs[0]
        if first != 1:
            self.notes.append(f"run {run['since']}: recording starts at seq {first} (joined mid-run)")
        for a, b in zip(seqs, seqs[1:]):
            if b != a + 1:
                run["missing"].append((a + 1, b - 1))
        run["sorted"] = msgs
        prev = None
        pending_pulse = None
        jumps = 0
        for m in msgs:
            when = t(m["timestamp"])
            if prev is not None:
                if m["count"] < prev["count"]:
                    self.problem(when, f"count fell {prev['count']} -> {m['count']} (seq {prev['seq']} -> {m['seq']})")
                d = m["count"] - prev["count"]
                if m["trigger"] == "pulse" and d > 1 and m["seq"] == prev["seq"] + 1:
                    jumps += 1
                for k in COUNTERS:
                    if m[k] < prev[k]:
                        self.problem(when, f"{k} fell {prev[k]} -> {m[k]}")
            if m["trigger"] == "pulse":
                if m["level"] != "closed":
                    self.problem(when, f"pulse seq {m['seq']} with level {m['level']}")
                if m["last_pulse_at"] is None or abs((t(m["last_pulse_at"]) - when).total_seconds()) >= 1:
                    self.problem(when, f"pulse seq {m['seq']} timestamp != last_pulse_at {m['last_pulse_at']}")
                pending_pulse = m
            elif m["trigger"] == "open":
                if m["level"] != "open":
                    self.problem(when, f"open seq {m['seq']} with level {m['level']}")
                if m["last_closed_ms"] is None or m["last_pulse_at"] is None:
                    self.problem(when, f"open seq {m['seq']} without last_closed_ms/last_pulse_at")
                else:
                    opened = t(m["last_pulse_at"]) + timedelta(milliseconds=m["last_closed_ms"])
                    if opened.replace(microsecond=0) != when:
                        self.problem(when, f"open seq {m['seq']} timestamp != last_pulse_at + last_closed_ms ({iso(opened)})")
                if pending_pulse is not None and pending_pulse["count"] != m["count"]:
                    self.notes.append(f"open seq {m['seq']} closes a pulse whose own message wasn't seen")
                pending_pulse = None
            prev = m
        run["jumps"] = jumps
        sweeps = [t(m["timestamp"]) for m in msgs if m["trigger"] == "sweep"]
        run["sweep_gaps"] = [(a, b) for a, b in zip(sweeps, sweeps[1:])
                             if (b - a).total_seconds() > SWEEP_GAP_S]
        run["first"], run["last"] = msgs[0], msgs[-1]
        run["counter_rises"] = self._counter_rises(msgs)
        run["litres"] = self._litres(msgs)
        run["suspect"] = self._suspect(run["litres"])

    def _counter_rises(self, msgs):
        out = []
        for a, b in zip(msgs, msgs[1:]):
            for k in COUNTERS:
                if b[k] > a[k]:
                    when = t(b["timestamp"])
                    near = self.nearest_boiler(when)
                    out.append({"at": iso(when), "counter": k, "by": b[k] - a[k], "seq": b["seq"],
                                "boiler": near})
        return out

    def nearest_boiler(self, when):
        best = None
        for bt, ev in self.boiler:
            d = (when - bt).total_seconds()
            if abs(d) <= ATTRIBUTE_S and (best is None or abs(d) < abs(best[1])):
                best = (ev, d)
        return None if best is None else {"event": best[0], "offset_s": round(best[1], 1)}

    def _litres(self, msgs):
        """Each counted litre, from pulse and open messages: closing time,
        interval to the previous closing, closed ms. A litre is its count:
        tanktemp can print the same closing a millisecond apart in the pulse
        and the open. A litre seen only as a count jump has no time."""
        litres = {}
        for m in msgs:
            if m["trigger"] in ("pulse", "open") and m["last_pulse_at"]:
                L = litres.setdefault(m["count"], {"at": m["last_pulse_at"], "count": m["count"]})
                if m["trigger"] == "pulse":
                    L["interval_ms"] = m["last_interval_ms"]
                else:
                    L["closed_ms"] = m["last_closed_ms"]
        return sorted(litres.values(), key=lambda L: L["at"])

    @staticmethod
    def _suspect(litres):
        """Litres whose closing came < SUSPECT_OPEN_MS after the reed opened
        from the litre before: open_ms = interval(n) - closed(n-1)."""
        out = []
        for a, b in zip(litres, litres[1:]):
            c, iv = a.get("closed_ms"), b.get("interval_ms")
            if c is None or iv is None:
                continue
            if iv - c < SUSPECT_OPEN_MS:
                out.append({"at": b["at"], "count": b["count"], "open_ms": iv - c,
                            "interval_ms": iv, "prev_closed_ms": c})
        return out

    def draws(self, run):
        """Group litres into draws: a draw ends gap_s after its last litre, so
        a litre less than gap_s after the previous one joins it."""
        out, cur = [], None
        for L in run["litres"]:
            at = t(L["at"])
            if cur is not None and (at - cur["end"]).total_seconds() < self.gap_s:
                cur["end"] = at
                cur["litres"] += 1
                cur["intervals"].append((at - cur["prev"]).total_seconds())
            else:
                cur = {"start": at, "end": at, "litres": 1, "intervals": []}
                out.append(cur)
            cur["prev"] = at
        res = []
        for d in out:
            iv = d["intervals"]
            res.append({
                "start": iso(d["start"]), "end": iso(d["end"]),
                "litres": d["litres"],
                "duration_s": round((d["end"] - d["start"]).total_seconds(), 1),
                "median_flow_l_min": round(60 / statistics.median(iv), 1) if iv else None,
                "peak_flow_l_min": round(60 / min(iv), 1) if iv else None,
            })
        return res

    @staticmethod
    def duty(run):
        """closed ms / interval ms per litre, where both are known: the closed
        time of litre n over the interval from n's closing to n+1's."""
        L = run["litres"]
        out = []
        for a, b in zip(L, L[1:]):
            c, iv = a.get("closed_ms"), b.get("interval_ms")
            if c is not None and iv and iv < 36000:
                out.append(c / iv)
        return out

    def report(self):
        lines = [f"meter messages: {self.msgs}, duplicates dropped: {self.dups}, runs: {len(self.runs)}, "
                 f"boiler events: {len(self.boiler)}"]
        summary = {"messages": self.msgs, "duplicates": self.dups, "runs": [], "problems": self.problems,
                   "notes": self.notes}
        for run in self.runs.values():
            f, l = run["first"], run["last"]
            dr = self.draws(run)
            du = self.duty(run)
            lines.append(f"\nrun counting_since {run['since']}: seq {f['seq']}..{l['seq']}, "
                         f"count {f['count']} -> {l['count']} ({l['count'] - f['count']} L), "
                         f"{f['timestamp']} .. {l['timestamp']}")
            gaps = sum(b - a + 1 for a, b in run["missing"])
            lines.append(f"  missing seq: {gaps} in {len(run['missing'])} gaps"
                         + (f" (first {run['missing'][:5]})" if run["missing"] else ""))
            lines.append(f"  count jumps on a pulse (skipped messages): {run['jumps']}")
            lines.append(f"  sweep gaps > {SWEEP_GAP_S}s: {len(run['sweep_gaps'])}"
                         + "".join(f"\n    {iso(a)} -> {iso(b)}" for a, b in run["sweep_gaps"][:10]))
            lines.append(f"  counters at end: " + ", ".join(f"{k} {l[k]}" for k in COUNTERS))
            for c in run["counter_rises"]:
                b = c["boiler"]
                lines.append(f"    {c['at']} {c['counter']} +{c['by']} (seq {c['seq']})"
                             + (f" — {b['event']} {b['offset_s']:+}s" if b else " — no boiler event within 5 s"))
            lines.append(f"  suspect re-closes (reed open < {SUSPECT_OPEN_MS} ms before a litre): {len(run['suspect'])}"
                         + "".join(f"\n    {x['at']} count {x['count']}: open {x['open_ms']} ms" for x in run["suspect"][:10]))
            if du:
                lines.append(f"  duty (closed/interval) over {len(du)} litres: median {statistics.median(du):.3f}, "
                             f"range {min(du):.3f}..{max(du):.3f}")
            lines.append(f"  draws (litres < {self.gap_s:g}s apart): {len(dr)}")
            for d in dr:
                fl = f", median {d['median_flow_l_min']} L/min, peak {d['peak_flow_l_min']}" if d["median_flow_l_min"] else ""
                lines.append(f"    {d['start']}  {d['litres']:4d} L over {d['duration_s']:7.1f}s{fl}")
            summary["runs"].append({"counting_since": run["since"], "first_seq": f["seq"], "last_seq": l["seq"],
                                    "litres": l["count"] - f["count"], "missing": run["missing"],
                                    "jumps": run["jumps"], "sweep_gaps": [[iso(a), iso(b)] for a, b in run["sweep_gaps"]],
                                    "counters": {k: l[k] for k in COUNTERS},
                                    "counter_rises": run["counter_rises"], "draws": dr,
                                    "suspect": run["suspect"],
                                    "duty": {"n": len(du), "median": statistics.median(du) if du else None}})
        lines.append(f"\ncontract problems: {len(self.problems)}")
        lines += [f"  {w} {p}" for w, p in self.problems[:50]]
        if self.notes:
            lines.append("notes:")
            lines += [f"  {n}" for n in self.notes[:20]]
        return "\n".join(lines), summary


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("files", nargs="+")
    ap.add_argument("--json")
    ap.add_argument("--gap-s", type=float, default=60.0)
    a = ap.parse_args(argv)
    au = Audit(a.gap_s)
    for rx, topic, payload in read(a.files):
        au.feed(rx, topic, payload)
    if not au.order:
        print("no meter messages")
        return 0
    au.finish()
    text, summary = au.report()
    print(text)
    if a.json:
        with open(a.json, "w") as f:
            json.dump(summary, f, indent=1, default=str)
    return 1 if au.problems else 0


if __name__ == "__main__":
    sys.exit(main())
