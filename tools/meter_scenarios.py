#!/usr/bin/env python3
"""Write the synthetic meter scenarios that washhouse's Go meter consumer is
cross-checked against: each as a '%I %t %p' recording, built with
meter_audit_test.py's tanktemp-shaped emitter, plus meter_audit.py's draws
for it.

Out: internal/meter/testdata/audit/NAME.log and NAME.draws.json (the audit's
draws, all runs, in order). The Go test (internal/meter/audit_test.go)
replays each .log and must find the same draws.

Only scenarios where the two are meant to agree go here: no creep (the
audit has no creep rule) and no litre whose pulse and open were both lost
(the audit only times litres it saw; the Go counts them from `count`).

Usage: python3 tools/meter_scenarios.py [OUTDIR]
"""
import json
import os
import sys
from datetime import timedelta

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import meter_audit as ma  # noqa: E402
from meter_audit_test import T0, Meter, run_audit, shower, z  # noqa: E402

S = timedelta(seconds=1)


def clean():
    # meter_audit_test.Clean: 10 L at 10 L/min, then 3 L at 30 L/min
    m = Meter()
    m.sweep(T0)
    shower(m, T0 + 5 * S, 10, 6.0)
    m.sweep(T0 + 30 * S)
    m.sweep(T0 + 60 * S)
    m.sweep(T0 + 90 * S)
    shower(m, T0 + 200 * S, 3, 2.0)
    m.sweep(T0 + 210 * S)
    return m.lines()


def replayed():
    # Clean, with a reconnect replaying the retained message
    rows = clean()
    return rows + [rows[-1]]


def lost_message():
    # meter_audit_test: a lost message is a seq gap (here the 2nd pulse)
    m = Meter()
    m.sweep(T0)
    shower(m, T0 + 2 * S, 4, 6.0)
    m.sweep(T0 + 30 * S)
    rows = m.lines()
    del rows[3]
    return rows


def restart():
    # meter_audit_test: a restart is a new run; litres add across runs
    a = Meter()
    a.sweep(T0)
    shower(a, T0 + 2 * S, 2, 6.0)
    a.sweep(T0 + 30 * S)
    b = Meter(since=z(T0 + 5 * 60 * S))
    b.sweep(T0 + 5 * 60 * S + 10 * S)
    shower(b, T0 + 5 * 60 * S + 20 * S, 3, 5.0)
    b.sweep(T0 + 5 * 60 * S + 40 * S)
    return a.lines() + b.lines()


def reclose():
    # meter_audit_test: a chatter re-close 80 ms after opening is still a
    # counted litre, in the same draw
    m = Meter()
    m.sweep(T0)
    c1 = T0 + 2 * S
    m.litre(c1, 2.42)
    m.litre(c1 + 2.5 * S, 5.0)
    m.litre(c1 + 30 * S, 1.4)
    m.sweep(T0 + 60 * S)
    return m.lines()


def near_q4():
    m = Meter()
    m.sweep(T0)
    shower(m, T0 + 2 * S, 20, 0.75)
    m.sweep(T0 + 30 * S)
    return m.lines()


def kinds():
    # a shower, a bath, taps and a 20 L shower, with a sweep every 30 s,
    # emitted in time order (a sweep inside a litre's closed time comes
    # after that litre's open: the emitter sends pulse and open together)
    plan = [
        (40, 9.0),    # shower: 6.7 L/min, 40 L over 351 s
        (80, 4.0),    # bath: 15 L/min
        (5, 15.0),    # tap
        (19, 10.0),   # tap: 6 L/min over 180 s, but under 20 L
        (20, 10.0),   # shower: 20 L over 190 s
        (12, 20.0),   # tap: 3 L/min over 220 s, under 20 L
    ]
    events, at = [], T0 + 7 * S
    for litres, iv in plan:
        for i in range(litres):
            events.append((at + i * iv * S, "litre", iv))
        at += (litres * iv + 120) * S
    t = T0
    while t < at:
        events.append((t, "sweep", None))
        t += 30 * S
    events.sort(key=lambda e: (e[0], e[1]))
    m = Meter()
    for when, kind, iv in events:
        if kind == "sweep":
            m.sweep(when)
        else:
            m.litre(when, iv * 0.23)
    return m.lines()


def gaps():
    # litres 59.5 s apart join (a draw ends a minute after its last litre);
    # 60.5 s apart split. 40.7 s: a real bath's pause mid-fill.
    m = Meter()
    m.sweep(T0)
    t = T0 + 1 * S
    for gap in (40.7, 59.5, 60.5, 10.0, 60.5):
        m.litre(t, 1.0)
        t += gap * S
    m.litre(t, 1.0)
    m.sweep(t + 61 * S)
    return m.lines()


SCENARIOS = {
    "clean": clean, "replayed": replayed, "lost-message": lost_message,
    "restart": restart, "reclose": reclose, "near-q4": near_q4,
    "kinds": kinds, "gaps": gaps,
}


def main(argv):
    here = os.path.dirname(os.path.abspath(__file__))
    out = argv[1] if len(argv) > 1 else os.path.join(here, "..", "internal", "meter", "testdata", "audit")
    os.makedirs(out, exist_ok=True)
    for name, build in SCENARIOS.items():
        rows = build()
        au = run_audit(rows)
        if au.problems:
            raise SystemExit(f"{name}: audit problems {au.problems}")
        draws = [d for run in au.runs.values() for d in au.draws(run)]
        with open(os.path.join(out, name + ".log"), "w") as f:
            for rx, tp, p in rows:
                f.write(f"{rx} {tp} {p}\n")
        with open(os.path.join(out, name + ".draws.json"), "w") as f:
            json.dump(draws, f, indent=1)
            f.write("\n")
        print(f"{name}: {len(rows)} messages, {len(draws)} draws "
              f"{[d['litres'] for d in draws]}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
