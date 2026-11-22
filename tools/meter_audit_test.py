#!/usr/bin/env python3
"""Tests for meter_audit.py. Scenarios are built the way tanktemp's WIRE.md
says the meter publishes: a sweep every 30 s, a pulse at each counted
closing, an open when it reopens. Run: python3 tools/meter_audit_test.py"""
import json
import unittest
from datetime import datetime, timedelta, timezone

import meter_audit as ma

T0 = datetime(2026, 11, 21, 7, 0, 0, tzinfo=timezone.utc)
SINCE = "2026-11-21T06:59:30Z"


def z(d, ms=False):
    if ms:
        return d.strftime("%Y-%m-%dT%H:%M:%S.") + f"{d.microsecond // 1000:03d}Z"
    return d.strftime("%Y-%m-%dT%H:%M:%SZ")


class Meter:
    """Emits meter messages in counter order, numbering seq at publish."""

    def __init__(self, since=SINCE):
        self.since = since
        self.seq = 0
        self.count = 0
        self.level = "open"
        self.last_pulse = None
        self.last_interval = None
        self.last_closed = None
        self.c = dict(noise=0, short=0, glitches=0, lost=0)
        self.out = []   # (rx, topic, payload)

    def emit(self, trigger, at, rx=None):
        self.seq += 1
        m = {
            "device_id": "hot_water_meter", "timestamp": z(at), "clock_synced": True,
            "trigger": trigger, "seq": self.seq, "litres_per_pulse": 1, "level": self.level,
            "counting_since": self.since, "count": self.count,
            "last_pulse_at": z(self.last_pulse, ms=True) if self.last_pulse else None,
            "last_interval_ms": self.last_interval, "last_closed_ms": self.last_closed,
            **self.c,
        }
        self.out.append((rx or at, ma.METER_TOPIC, json.dumps(m, separators=(",", ":"))))
        return m

    def litre(self, closing, closed_s):
        if self.last_pulse is not None:
            self.last_interval = round((closing - self.last_pulse).total_seconds() * 1000)
        self.count += 1
        self.level = "closed"
        self.last_pulse = closing
        self.emit("pulse", closing, closing + timedelta(milliseconds=60))
        opened = closing + timedelta(seconds=closed_s)
        self.level = "open"
        self.last_closed = round(closed_s * 1000)
        self.emit("open", opened)

    def sweep(self, at):
        self.emit("sweep", at)

    def lines(self):
        return [(rx.replace(microsecond=0).isoformat(), t, p) for rx, t, p in self.out]


def run_audit(rows, boiler=()):
    au = ma.Audit()
    events = [(datetime.fromisoformat(rx), tp, p) for rx, tp, p in rows]
    for at, ev in boiler:
        events.append((at, ma.BOILER_EVENTS,
                       json.dumps({"boiler": {"timestamp": z(at), "event": ev}})))
    events.sort(key=lambda e: e[0])
    for rx, tp, p in events:
        au.feed(rx, tp, p)
    au.finish()
    return au


def shower(m, start, litres, interval_s, duty=0.23):
    at = start
    for _ in range(litres):
        m.litre(at, interval_s * duty)
        at += timedelta(seconds=interval_s)
    return at


class Clean(unittest.TestCase):
    def setUp(self):
        m = Meter()
        m.sweep(T0)
        end = shower(m, T0 + timedelta(seconds=5), 10, 6.0)    # 10 L at 10 L/min
        m.sweep(T0 + timedelta(seconds=30))
        m.sweep(T0 + timedelta(seconds=60))
        m.sweep(T0 + timedelta(seconds=90))
        shower(m, T0 + timedelta(seconds=200), 3, 2.0)          # 3 L at 30 L/min: a bath tap
        m.sweep(T0 + timedelta(seconds=210))
        self.m = m
        self.au = run_audit(m.lines())

    def test_no_problems(self):
        self.assertEqual(self.au.problems, [])

    def test_two_draws_with_volume_and_flow(self):
        run = next(iter(self.au.runs.values()))
        d = self.au.draws(run)
        self.assertEqual([x["litres"] for x in d], [10, 3])
        self.assertEqual(d[0]["median_flow_l_min"], 10.0)
        self.assertEqual(d[1]["median_flow_l_min"], 30.0)

    def test_duty_is_recovered(self):
        run = next(iter(self.au.runs.values()))
        du = self.au.duty(run)
        self.assertEqual(len(du), 11)   # 9 within the first draw, 1 across, 2 within the second
        # the litre before the 2nd draw has a 110 s+ interval: excluded (>= 36 s)
        self.assertTrue(all(abs(x - 0.23) < 0.002 for x in du), du)

    def test_retained_replay_is_deduplicated(self):
        rows = self.m.lines()
        rows.append(rows[-1])      # a reconnect replays the retained message
        au = run_audit(rows)
        self.assertEqual(au.dups, 1)
        self.assertEqual(au.problems, [])

    def test_report_renders(self):
        text, summary = self.au.report()
        self.assertIn("draws (litres < 60s apart): 2", text)
        self.assertEqual(summary["runs"][0]["litres"], 13)


class Rules(unittest.TestCase):
    def test_a_pause_under_a_minute_is_the_same_draw(self):
        # A real bath paused 40.7 s mid-fill: one draw, not 5 + 67 L.
        m = Meter()
        m.sweep(T0)
        at = shower(m, T0 + timedelta(seconds=2), 5, 3.0)
        shower(m, at + timedelta(seconds=40.7 - 3.0), 4, 3.0)
        d = run_audit(m.lines()).draws(next(iter(run_audit(m.lines()).runs.values())))
        self.assertEqual([x["litres"] for x in d], [9])

    def test_a_minute_without_a_litre_ends_the_draw(self):
        m = Meter()
        m.sweep(T0)
        at = shower(m, T0 + timedelta(seconds=2), 3, 3.0)
        shower(m, at + timedelta(seconds=60 - 3.0), 2, 3.0)
        au = run_audit(m.lines())
        self.assertEqual([x["litres"] for x in au.draws(next(iter(au.runs.values())))], [3, 2])

    def test_a_litre_is_its_count_not_its_last_pulse_at(self):
        # tanktemp can print the same closing as .228 in the pulse and .227 in
        # the open: same count, so one litre, and no "unseen pulse" note.
        m = Meter()
        m.sweep(T0)
        m.litre(T0 + timedelta(seconds=5, milliseconds=228), 1.5)
        rx, tp, p = m.out[-1]
        d = json.loads(p)
        d["last_pulse_at"] = d["last_pulse_at"].replace(".228Z", ".227Z")
        m.out[-1] = (rx, tp, json.dumps(d, separators=(",", ":")))
        au = run_audit(m.lines())
        run = next(iter(au.runs.values()))
        self.assertEqual(len(run["litres"]), 1)
        self.assertEqual(run["litres"][0].get("closed_ms"), 1500)
        self.assertEqual(au.notes, [])

class Broken(unittest.TestCase):
    def test_lost_message_is_a_seq_gap_not_a_problem(self):
        m = Meter()
        m.sweep(T0)
        shower(m, T0 + timedelta(seconds=2), 4, 6.0)
        m.sweep(T0 + timedelta(seconds=30))
        rows = m.lines()
        del rows[3]
        au = run_audit(rows)
        run = next(iter(au.runs.values()))
        self.assertEqual(run["missing"], [(4, 4)])
        self.assertEqual(au.problems, [])

    def test_count_falling_is_a_problem(self):
        m = Meter()
        m.sweep(T0)
        shower(m, T0 + timedelta(seconds=2), 3, 6.0)
        m.count -= 2
        m.sweep(T0 + timedelta(seconds=30))
        au = run_audit(m.lines())
        self.assertTrue(any("count fell" in p for _, p in au.problems), au.problems)

    def test_open_time_must_match_pulse_plus_closed(self):
        m = Meter()
        m.sweep(T0)
        shower(m, T0 + timedelta(seconds=2), 2, 6.0)
        rows = m.lines()
        rx, tp, p = rows[2]                  # the first open
        d = json.loads(p)
        d["last_closed_ms"] += 3000
        rows[2] = (rx, tp, json.dumps(d, separators=(",", ":")))
        au = run_audit(rows)
        self.assertTrue(any("last_pulse_at + last_closed_ms" in p for _, p in au.problems), au.problems)

    def test_field_order_is_a_problem(self):
        m = Meter()
        m.sweep(T0)
        rx, tp, p = m.lines()[0]
        d = json.loads(p)
        d = {"seq": d.pop("seq"), **d}
        au = run_audit([(rx, tp, json.dumps(d))])
        self.assertTrue(any("field order" in p for _, p in au.problems))

    def test_noise_is_attributed_to_the_boiler_switch(self):
        m = Meter()
        m.sweep(T0)
        hw_off = T0 + timedelta(seconds=29)
        m.c["noise"] = 6                     # six spikes, seen at the next sweep
        m.sweep(T0 + timedelta(seconds=30))
        m.c["short"] = 1
        m.sweep(T0 + timedelta(seconds=60))
        au = run_audit(m.lines(), boiler=[(hw_off, "HW_OFF")])
        rises = next(iter(au.runs.values()))["counter_rises"]
        self.assertEqual(rises[0]["counter"], "noise")
        self.assertEqual(rises[0]["by"], 6)
        self.assertEqual(rises[0]["boiler"], {"event": "HW_OFF", "offset_s": 1.0})
        self.assertIsNone(rises[1]["boiler"])     # 31 s later: not attributed

    def test_restart_is_a_new_run(self):
        a = Meter()
        a.sweep(T0)
        shower(a, T0 + timedelta(seconds=2), 2, 6.0)
        b = Meter(since="2026-11-21T07:05:00Z")
        b.sweep(T0 + timedelta(minutes=5, seconds=10))
        au = run_audit(a.lines() + b.lines())
        self.assertEqual(len(au.runs), 2)
        self.assertEqual(au.problems, [])

    def test_joining_mid_run_is_a_note(self):
        m = Meter()
        for i in range(5):
            m.sweep(T0 + timedelta(seconds=30 * i))
        au = run_audit(m.lines()[3:])
        self.assertTrue(any("joined mid-run" in n for n in au.notes))
        self.assertEqual(au.problems, [])

    def test_a_reclose_too_soon_after_opening_is_suspect(self):
        # a creeping pointer chattering at the magnet's edge: the reed opens for
        # 80 ms and closes again 2.5 s after the last litre. Not short (it held),
        # not a glitch (> min_interval), but no real litre opens for < ~0.55 s.
        m = Meter()
        m.sweep(T0)
        c1 = T0 + timedelta(seconds=2)
        m.litre(c1, 2.42)                                   # open at +2.42 s
        m.litre(c1 + timedelta(seconds=2.5), 5.0)           # closes 80 ms later
        m.litre(c1 + timedelta(seconds=30), 1.4)            # a normal litre
        m.sweep(T0 + timedelta(seconds=60))
        au = run_audit(m.lines())
        sus = next(iter(au.runs.values()))["suspect"]
        self.assertEqual(len(sus), 1, sus)
        self.assertEqual(sus[0]["open_ms"], 80)
        self.assertEqual(au.problems, [])     # interpretation, not a contract breach

    def test_clean_draws_have_no_suspects(self):
        m = Meter()
        m.sweep(T0)
        shower(m, T0 + timedelta(seconds=2), 20, 0.75)       # near Q4: 80 L/min
        m.sweep(T0 + timedelta(seconds=30))
        au = run_audit(m.lines())
        self.assertEqual(next(iter(au.runs.values()))["suspect"], [])

    def test_sweep_gap_is_listed(self):
        m = Meter()
        m.sweep(T0)
        m.sweep(T0 + timedelta(seconds=120))
        au = run_audit(m.lines())
        self.assertEqual(len(next(iter(au.runs.values()))["sweep_gaps"]), 1)


if __name__ == "__main__":
    unittest.main()
