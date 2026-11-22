#!/usr/bin/env python3
"""Smoke-check the live hot-water inputs washhouse depends on.

Reads a snapshot of retained MQTT messages (lines of 'TOPIC PAYLOAD', as
mosquitto_sub -F '%t %p' prints them) and, optionally, tanktemp's /healthz
body, and prints one PASS / WARN / FAIL line per check. Exit status: 0 all
pass, 1 any FAIL, 2 only WARNs. Read-only; tools/smoke.sh fetches the inputs.

Usage: smoke.py --retained FILE [--healthz FILE] [--now ISO] [--expect-meter]
"""
import argparse
import json
import sys
from datetime import datetime, timezone

TT = "water/hot/tanktemp"
TANKS = ("hot_water_cylinder_left", "hot_water_cylinder_right")
METER_ID = "hot_water_meter"

# docs/WIRE.md (tanktemp) ## Meter: field order is part of the contract.
METER_KEYS = [
    "device_id", "timestamp", "clock_synced", "trigger", "seq",
    "litres_per_pulse", "level", "counting_since", "count", "last_pulse_at",
    "last_interval_ms", "last_closed_ms", "noise", "short", "glitches", "lost",
]
TRIGGERS = {"pulse", "open", "sweep"}

STATE_MAX_AGE_S = 90         # statehouse's freshness rule for tank states
METER_MAX_AGE_S = 90         # a sweep message at least every 30 s
HEARTBEAT_MAX_AGE_S = 20 * 60  # 15 min heartbeats (tanktemp and boiler-sensor), 5 min slack


def parse_time(s):
    if s is None:
        return None
    return datetime.fromisoformat(s.replace("Z", "+00:00"))


class Report:
    def __init__(self):
        self.lines = []

    def add(self, level, what, detail=""):
        self.lines.append((level, what, detail))

    def ok(self, what, detail=""):
        self.add("PASS", what, detail)

    def warn(self, what, detail=""):
        self.add("WARN", what, detail)

    def fail(self, what, detail=""):
        self.add("FAIL", what, detail)

    def check(self, cond, what, detail="", level="FAIL"):
        self.add("PASS" if cond else level, what, detail)
        return cond

    def status(self):
        levels = {lv for lv, _, _ in self.lines}
        return 1 if "FAIL" in levels else 2 if "WARN" in levels else 0

    def render(self):
        return "\n".join(f"{lv:4}  {w}" + (f": {d}" if d else "") for lv, w, d in self.lines)


def read_retained(text):
    msgs = {}
    for line in text.splitlines():
        if not line.strip():
            continue
        topic, _, payload = line.partition(" ")
        msgs[topic] = payload
    return msgs


def decode(r, msgs, topic, what):
    raw = msgs.get(topic)
    if raw is None:
        r.fail(what, f"no retained message on {topic}")
        return None
    try:
        return json.loads(raw)
    except json.JSONDecodeError as e:
        r.fail(what, f"not JSON on {topic}: {e}")
        return None


def age_s(now, ts):
    t = parse_time(ts)
    return None if t is None else (now - t).total_seconds()


def check_fresh(r, now, ts, limit, what):
    a = age_s(now, ts)
    if a is None:
        return r.fail(what, "no timestamp")
    if a < -5:
        return r.fail(what, f"timestamp {ts} is {-a:.0f}s in the future")
    return r.check(a <= limit, what, f"{a:.0f}s old (limit {limit}s)")


def check_tanktemp_status(r, now, msgs, expect_meter):
    st = decode(r, msgs, f"{TT}/status", "tanktemp status")
    if st is None:
        return None
    if not r.check(st.get("online") is True and st.get("event") in ("STARTUP", "HEARTBEAT"),
                   "tanktemp online", f"event {st.get('event')}, online {st.get('online')}"):
        return st
    check_fresh(r, now, st.get("timestamp"), HEARTBEAT_MAX_AGE_S, "tanktemp heartbeat fresh")
    r.check(st.get("ready") is True, "tanktemp ready")
    r.check(st.get("sensors_failed") == 0, "tanktemp probes all OK",
            f"{st.get('sensors_ok')} ok, {st.get('sensors_failed')} failed")
    probs = st.get("startup_problems") or []
    r.check(not probs, "tanktemp startup problems", "; ".join(
        f"{p.get('code')}: {p.get('message')}" for p in probs) or "none")
    if expect_meter:
        r.check(METER_ID in (st.get("meters") or []), "tanktemp configures the meter",
                f"meters {st.get('meters')}")
    r.ok("tanktemp version", str(st.get("version")))
    return st


def check_tanks(r, now, msgs):
    for tank in TANKS:
        s = decode(r, msgs, f"{TT}/{tank}/state", f"{tank} state")
        if s is None:
            continue
        check_fresh(r, now, s.get("timestamp"), STATE_MAX_AGE_S, f"{tank} fresh")
        r.check(s.get("clock_synced") is True, f"{tank} clock synced")


def check_meter(r, now, msgs, expect_meter):
    topic = f"{TT}/meter"
    if topic not in msgs:
        if expect_meter:
            r.fail("meter message", f"no retained message on {topic}")
        else:
            r.ok("meter", "not deployed yet (no retained message); pass --expect-meter once it is")
        return None
    try:
        m = json.loads(msgs[topic], object_pairs_hook=lambda kv: kv)
    except json.JSONDecodeError as e:
        r.fail("meter message", f"not JSON: {e}")
        return None
    keys = [k for k, _ in m]
    m = dict(m)
    r.check(keys == METER_KEYS, "meter field order matches WIRE.md",
            "" if keys == METER_KEYS else f"got {keys}")
    r.check(m.get("device_id") == METER_ID, "meter device_id", str(m.get("device_id")))
    r.check(m.get("trigger") in TRIGGERS, "meter trigger", str(m.get("trigger")))
    r.check(m.get("level") in ("open", "closed"), "meter level", str(m.get("level")))
    r.check(m.get("clock_synced") is True, "meter clock synced")
    r.check(m.get("litres_per_pulse") == 1, "meter litres_per_pulse", str(m.get("litres_per_pulse")))
    # A retained pulse/open can be the newest message only if a sweep hasn't
    # followed it yet, so the newest message of any kind is at most a sweep old.
    check_fresh(r, now, m.get("timestamp"), METER_MAX_AGE_S, "meter fresh")
    cs, ts = parse_time(m.get("counting_since")), parse_time(m.get("timestamp"))
    r.check(cs is not None and ts is not None and cs <= ts, "meter counting_since before timestamp",
            f"since {m.get('counting_since')}")
    count = m.get("count")
    r.check(isinstance(count, int) and count >= 0, "meter count", str(count))
    if isinstance(count, int) and count > 0:
        r.check(m.get("last_pulse_at") is not None, "meter last_pulse_at set once counting")
    if m.get("last_closed_ms") is not None:
        r.check(m["last_closed_ms"] >= 0, "meter last_closed_ms non-negative")
    if m.get("last_interval_ms") is not None:
        r.check(m["last_interval_ms"] >= 720, "meter last interval within Q4 (>= 720 ms)",
                f"{m['last_interval_ms']} ms", level="WARN")
    for k, meaning in (("noise", "the kernel debounce isn't holding"),
                       ("short", "spikes that flipped the reading got through"),
                       ("glitches", "closings faster than the meter can turn"),
                       ("lost", "count is a lower bound")):
        v = m.get(k)
        r.check(v == 0, f"meter {k} is 0", f"{v}" + ("" if v == 0 else f" ({meaning})"),
                level="FAIL" if k == "lost" else "WARN")
    return m


def check_healthz(r, h, expect_meter, meter_msg):
    st = h.get("status")
    r.check(st in ("ok", "degraded"), "/healthz up", str(st))
    r.check(st == "ok", "/healthz not degraded", str(h.get("detail") or st), level="WARN")
    r.check(h.get("ready") is True and h.get("mqtt_connected") is True, "/healthz ready, MQTT connected",
            f"ready {h.get('ready')}, mqtt {h.get('mqtt_connected')}")
    meters = {x.get("device_id"): x for x in h.get("meters") or []}
    if not meters:
        if expect_meter:
            r.fail("/healthz meter", "no meters block")
        return
    hm = meters.get(METER_ID)
    if not r.check(hm is not None, "/healthz lists the meter", ", ".join(meters)):
        return
    r.check(hm.get("state") == "counting", "/healthz meter counting",
            f"{hm.get('state')}" + (f" ({hm.get('reason')})" if hm.get("reason") else ""))
    r.check(hm.get("publish_dropped", 0) == 0, "/healthz meter publish_dropped is 0",
            str(hm.get("publish_dropped")), level="WARN")
    if meter_msg and hm.get("counting_since") and meter_msg.get("counting_since"):
        r.check(hm["counting_since"] == meter_msg["counting_since"],
                "/healthz and MQTT agree on counting_since",
                f"{hm['counting_since']} vs {meter_msg['counting_since']}")
        if isinstance(hm.get("count"), int) and isinstance(meter_msg.get("count"), int):
            r.check(hm["count"] >= meter_msg["count"], "/healthz count not behind MQTT",
                    f"{hm['count']} vs {meter_msg['count']}")


def check_boiler(r, now, msgs):
    b = decode(r, msgs, "energy/boiler/sensor/system", "boiler-sensor system")
    if b is None:
        return
    s = b.get("status") or {}
    check_fresh(r, now, s.get("timestamp"), HEARTBEAT_MAX_AGE_S, "boiler-sensor heartbeat fresh")
    r.check(s.get("ready") is True, "boiler-sensor ready")
    r.check(s.get("hw") in ("ON", "OFF") and s.get("ch") in ("ON", "OFF"), "boiler HW/CH known",
            f"HW {s.get('hw')}, CH {s.get('ch')}")


def run(retained_text, healthz_text, now, expect_meter):
    r = Report()
    msgs = read_retained(retained_text)
    check_tanktemp_status(r, now, msgs, expect_meter)
    check_tanks(r, now, msgs)
    m = check_meter(r, now, msgs, expect_meter)
    if healthz_text is not None:
        try:
            check_healthz(r, json.loads(healthz_text), expect_meter, m)
        except json.JSONDecodeError as e:
            r.fail("/healthz", f"not JSON: {e}")
    check_boiler(r, now, msgs)
    return r


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--retained", required=True)
    ap.add_argument("--healthz")
    ap.add_argument("--now", help="ISO time to judge freshness against (default: now)")
    ap.add_argument("--expect-meter", action="store_true")
    a = ap.parse_args(argv)
    now = parse_time(a.now) if a.now else datetime.now(timezone.utc)
    with open(a.retained) as f:
        retained = f.read()
    healthz = None
    if a.healthz:
        with open(a.healthz) as f:
            healthz = f.read()
    r = run(retained, healthz, now, a.expect_meter)
    print(r.render())
    return r.status()


if __name__ == "__main__":
    sys.exit(main())
