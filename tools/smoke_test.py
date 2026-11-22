#!/usr/bin/env python3
"""Tests for smoke.py against real (sanitised) retained snapshots and
byte copies of tanktemp's meter goldens. Run: python3 tools/smoke_test.py"""
import json
import os
import unittest
from datetime import datetime, timedelta, timezone

import smoke

HERE = os.path.dirname(os.path.abspath(__file__))
TD = os.path.join(HERE, "testdata", "smoke")


def read(name):
    with open(os.path.join(TD, name)) as f:
        return f.read()


PRE = read("premeter.retained")
PRE_NOW = datetime(2026, 11, 20, 22, 5, 50, tzinfo=timezone.utc)
PULSE = read("meter-pulse.golden").strip()   # 2026-11-20T07:05:04Z, count 1
OPEN = read("meter-open.golden").strip()


def with_meter(payload, base=PRE):
    return base + f"smoke-ignored-line\nwater/hot/tanktemp/meter {payload}\n"


def levels(r):
    return {w: lv for lv, w, _ in r.lines}


def edit(payload, **kw):
    d = json.loads(payload)  # dicts keep order, so edits keep field order
    d.update(kw)
    return json.dumps(d, separators=(",", ":"))


HEALTHZ_OK = {
    "status": "ok", "ready": True, "mqtt_connected": True,
    "meters": [{"device_id": "hot_water_meter", "line": "gpiochip0:27", "state": "counting",
                "reason": None, "level": "closed", "count": 1,
                "counting_since": "2026-11-20T07:00:41Z", "publish_dropped": 0, "queued": 0}],
}


class PreMeter(unittest.TestCase):
    def test_tonight_before_the_deploy_passes(self):
        r = smoke.run(PRE, None, PRE_NOW, expect_meter=False)
        self.assertEqual(r.status(), 0, r.render())
        self.assertIn("meter", levels(r))

    def test_expecting_the_meter_before_it_exists_fails(self):
        r = smoke.run(PRE, None, PRE_NOW, expect_meter=True)
        self.assertEqual(levels(r)["meter message"], "FAIL")
        self.assertEqual(levels(r)["tanktemp configures the meter"], "FAIL")

    def test_stale_tank_states_fail(self):
        r = smoke.run(PRE, None, PRE_NOW + timedelta(minutes=5), expect_meter=False)
        self.assertEqual(levels(r)["hot_water_cylinder_left fresh"], "FAIL")

    def test_lwt_fails_and_skips_the_rest_of_status(self):
        lines = [l for l in PRE.splitlines() if not l.startswith("water/hot/tanktemp/status ")]
        lines.append('water/hot/tanktemp/status {"online":false,"event":"LWT"}')
        r = smoke.run("\n".join(lines), None, PRE_NOW, expect_meter=False)
        self.assertEqual(levels(r)["tanktemp online"], "FAIL")
        self.assertNotIn("tanktemp ready", levels(r))

    def test_missing_boiler_fails(self):
        lines = [l for l in PRE.splitlines() if not l.startswith("energy/boiler/")]
        r = smoke.run("\n".join(lines), None, PRE_NOW, expect_meter=False)
        self.assertEqual(levels(r)["boiler-sensor system"], "FAIL")


class Meter(unittest.TestCase):
    NOW = datetime(2026, 11, 20, 7, 5, 30, tzinfo=timezone.utc)

    def run_meter(self, payload, healthz=None, now=None):
        # judge only the meter: tanks and status are hours old at the golden's time
        r = smoke.Report()
        m = smoke.check_meter(r, now or self.NOW, smoke.read_retained(with_meter(payload)), True)
        if healthz is not None:
            smoke.check_healthz(r, healthz, True, m)
        return r

    def test_pulse_golden_passes(self):
        r = self.run_meter(PULSE)
        self.assertEqual(r.status(), 0, r.render())

    def test_open_golden_flags_its_counters(self):
        # tanktemp's open golden carries noise 15, short 1, glitches 2, lost 3
        r = self.run_meter(OPEN, now=datetime(2026, 11, 20, 7, 13, 10, tzinfo=timezone.utc))
        lv = levels(r)
        self.assertEqual(lv["meter fresh"], "PASS")
        self.assertEqual(lv["meter field order matches WIRE.md"], "PASS")
        for k in ("noise", "short", "glitches"):
            self.assertEqual(lv[f"meter {k} is 0"], "WARN", k)
        self.assertEqual(lv["meter lost is 0"], "FAIL")

    def test_field_order_is_checked(self):
        d = json.loads(PULSE)
        reordered = {"timestamp": d.pop("timestamp"), **d}
        r = self.run_meter(json.dumps(reordered))
        self.assertEqual(levels(r)["meter field order matches WIRE.md"], "FAIL")

    def test_noise_warns_and_lost_fails(self):
        r = self.run_meter(edit(PULSE, noise=3, lost=1))
        lv = levels(r)
        self.assertEqual(lv["meter noise is 0"], "WARN")
        self.assertEqual(lv["meter lost is 0"], "FAIL")

    def test_stale_meter_fails(self):
        r = self.run_meter(edit(PULSE, timestamp="2026-11-20T07:00:00Z"))
        self.assertEqual(levels(r)["meter fresh"], "FAIL")

    def test_impossible_interval_warns(self):
        r = self.run_meter(edit(PULSE, last_interval_ms=300))
        self.assertEqual(levels(r)["meter last interval within Q4 (>= 720 ms)"], "WARN")

    def test_counting_since_after_timestamp_fails(self):
        r = self.run_meter(edit(PULSE, counting_since="2026-11-20T08:00:00Z"))
        self.assertEqual(levels(r)["meter counting_since before timestamp"], "FAIL")

    def test_healthz_agrees(self):
        r = self.run_meter(PULSE, HEALTHZ_OK)
        self.assertEqual(r.status(), 0, r.render())

    def test_healthz_failed_line_fails(self):
        h = json.loads(json.dumps(HEALTHZ_OK))
        h["meters"][0].update(state="failed", reason="gpio_request_failed: line busy")
        r = self.run_meter(PULSE, h)
        self.assertEqual(levels(r)["/healthz meter counting"], "FAIL")

    def test_healthz_reset_disagrees(self):
        h = json.loads(json.dumps(HEALTHZ_OK))
        h["meters"][0]["counting_since"] = "2026-11-20T07:04:00Z"
        r = self.run_meter(PULSE, h)
        self.assertEqual(levels(r)["/healthz and MQTT agree on counting_since"], "FAIL")

    def test_degraded_warns(self):
        h = dict(HEALTHZ_OK, status="degraded", detail="1 probe failing")
        r = self.run_meter(PULSE, h)
        self.assertEqual(levels(r)["/healthz not degraded"], "WARN")
        self.assertEqual(r.status(), 2)


class Fixtures(unittest.TestCase):
    def test_no_network_details(self):
        for name in os.listdir(TD):
            body = read(name)
            for bad in ("192.168.", "ssid", '"ip"'):
                self.assertNotIn(bad, body, name)


if __name__ == "__main__":
    unittest.main()
