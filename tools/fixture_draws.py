#!/usr/bin/env python3
"""Write meter_audit.py's reading of every recorded fixture to
testdata/fixtures/draws.json, for internal/meter's fixture parity test.

Each fixture: total litres (count rise over all runs) and the audit's draws
(start, litres, duration, median flow). Re-running on unchanged fixtures
writes an identical file. tools/test.sh fails if the file is stale.

Usage: python3 tools/fixture_draws.py [--check]
"""
import gzip
import json
import os
import sys
from datetime import datetime

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import meter_audit as ma  # noqa: E402

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
FIXTURES = os.path.join(ROOT, "testdata", "fixtures")
OUT = os.path.join(FIXTURES, "draws.json")


def audit(path):
    au = ma.Audit()
    with gzip.open(path, "rt") as f:
        for line in f:
            rx, topic, payload = line.rstrip("\n").split(" ", 2)
            au.feed(datetime.fromisoformat(rx), topic, payload)
    au.finish()
    litres, draws = 0, []
    for run in au.runs.values():
        litres += run["last"]["count"] - run["first"]["count"]
        for d in au.draws(run):
            draws.append({k: d[k] for k in ("start", "litres", "duration_s", "median_flow_l_min")})
    draws.sort(key=lambda d: d["start"])
    return {"litres": litres, "draws": draws}


def render():
    out = {}
    for name in sorted(os.listdir(FIXTURES)):
        if name.endswith(".log.gz"):
            out[name[: -len(".log.gz")]] = audit(os.path.join(FIXTURES, name))
    return json.dumps(out, indent=1, sort_keys=True) + "\n"


def main():
    text = render()
    if sys.argv[1:] == ["--check"]:
        try:
            current = open(OUT).read()
        except FileNotFoundError:
            current = None
        if current != text:
            sys.exit(f"{OUT} is stale: run python3 tools/fixture_draws.py")
        return
    if sys.argv[1:]:
        sys.exit(__doc__)
    with open(OUT, "w") as f:
        f.write(text)


if __name__ == "__main__":
    main()
