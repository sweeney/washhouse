#!/usr/bin/env python3
"""Cut a washhouse test fixture from a live recording.

Input: mosquitto_sub -F '%I %t %p' lines (receive time, topic, one-line JSON
payload), as tools/record.sh writes them. Output: the same format, gzipped
deterministically, with

  - only the topics washhouse reads, each payload reduced to an allow-list
    of keys (tanktemp's per WIRE.md, its version replaced; the boiler's
    events and heartbeat to their state fields; the boiler-room climate to
    temperature and humidity), unknown keys dropped and counted;
  - a prelude: each kept topic's last message before START, stamped START
    (what a consumer connecting at START would be handed);
  - retained replays (the same topic and payload seen again) dropped;
  - every timestamp, in the receive-time prefix and anywhere in a payload,
    shifted by one whole number of seconds so START is 2001-01-01T00:00:00Z.

It refuses to write if a network detail or any real date survives.
Python 3 stdlib only. See scripts/make-fixture.sh for usage.
"""
import argparse
import calendar
import collections
import gzip
import io
import json
import os
import re
import sys
import tempfile
import time

KEEP_PREFIXES = ("water/hot/tanktemp", "energy/hot_water")
BOILER_EVENTS = "energy/boiler/sensor/events"
BOILER_SYSTEM = "energy/boiler/sensor/system"
CLIMATE = "zigbee2mqtt/climate_groundfloor_boiler-room"

# Allow-lists, per topic: the keys a fixture keeps, so a field nobody has
# reviewed never reaches one (fail closed). A dict is an object and the keys
# it may keep; [spec] is a list of those; VALUE is a scalar or a list of
# scalars (an object or nested list there is dropped too). Unknown keys are
# dropped, not refused: upstream fields are additive (tanktemp's WIRE.md), so
# a new one shouldn't stop a recut, and each cut reports what it dropped so
# the list can be extended once someone has looked.
VALUE = None
# tanktemp's docs/WIRE.md: State, Status (and the last will), Meter.
TANKTEMP_STATE = {
    "device_id": VALUE, "timestamp": VALUE, "clock_synced": VALUE, "sweep_ms": VALUE,
    "sensors": [{k: VALUE for k in ("position", "order", "height_mm", "rom", "temp_c",
                                     "raw_c", "offset_c", "error", "suspect")}],
}
TANKTEMP_STATUS = {
    **{k: VALUE for k in ("online", "event", "timestamp", "version", "ready", "uptime_s",
                          "tanks", "meters", "sensors_ok", "sensors_failed", "unexpected")},
    "startup_problems": [{"rom": VALUE, "code": VALUE, "message": VALUE}],
    "reason": VALUE,
}
TANKTEMP_METER = {k: VALUE for k in (
    "device_id", "timestamp", "clock_synced", "trigger", "seq", "litres_per_pulse", "level",
    "counting_since", "count", "last_pulse_at", "last_interval_ms", "last_closed_ms",
    "noise", "short", "glitches", "lost")}
# boiler-sensor: the switch events, and the system topic's two shapes (a
# heartbeat keeps its state fields only, a system event its own).
BOILER_EVENT = {"boiler": {"timestamp": VALUE, "event": VALUE,
                           "ch": {"state": VALUE}, "hw": {"state": VALUE}}}
SYSTEM_FIELDS = {
    "status": {k: VALUE for k in ("event", "ch", "hw", "ready", "timestamp")},
    "system": {k: VALUE for k in ("timestamp", "event", "reason", "source")},
}
CLIMATE_FIELDS = {"temperature": VALUE, "humidity": VALUE}
# tanktemp's build version is a public commit hash; its commit date would pin
# a fixture to a real day.
VERSION = re.compile(r'"version":"[^"]*"')
# A tank's state topic: {prefix}/{device_id}/state (WIRE.md, Topics).
DEVICE_STATE = re.compile(r"[a-z0-9_-]+/state\Z")

FAKE_EPOCH = calendar.timegm((2001, 1, 1, 0, 0, 0))
# An ISO-8601 timestamp: date, time, optional fraction, optional zone.
ISO = re.compile(r"(?<!\d)(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})"
                 r"(\.\d+)?(Z|[+-]\d{2}:?\d{2})?(?!\d)")

# What must not survive into a fixture.
FAKE_YEARS = ("2000", "2001")
DATE = re.compile(r"(?<!\d)(\d{4})-\d{2}-\d{2}(?!\d)")
# Unix times in seconds or milliseconds, 2001..2036 (zigbee's last_seen is
# one), whole or fractional.
EPOCH_NUMBER = re.compile(r"(?<![\d.])(1\d{9}|20\d{8}|1\d{12}|20\d{11})(\.\d*)?(?!\d)")
# The shift moves ISO-8601 times only, so any other way of writing a date is
# real (tanktemp writes every time as ISO). Refused whatever the year, the
# fake epoch's included.
_Y, _M, _D = r"(?:19|20)\d\d", r"(?:0[1-9]|1[0-2])", r"(?:0[1-9]|[12]\d|3[01])"
_HMS = r"(?:[01]\d|2[0-3])[0-5]\d"
# YYYYMMDD then a time: 20010101T000000Z, 20010101-000000 (a build name),
# 20010101_0000, 20010101000000.
COMPACT_DATE_TIME = re.compile(r"(?<!\d)%s%s%s(?:[T_-]%s(?:[0-5]\d)?|%s[0-5]\d)(?!\d)"
                               % (_Y, _M, _D, _HMS, _HMS))
# YYYYMMDD on its own inside a name (tanktemp-20010101, v20010101.1). A bare
# number is left alone: an uptime or a count can look like one.
COMPACT_DATE = re.compile(r"(?<=[A-Za-z_-])%s%s%s(?![0-9A-Za-z])" % (_Y, _M, _D))
# 2001/01/01, 2001.01.01, 01/01/2001, 1/1/01, 1.1.2001.
NUMERIC_DATE = re.compile(r"(?<![\d./])(?:%s[/.]\d{1,2}[/.]\d{1,2}|\d{1,2}/\d{1,2}/\d{2,4}"
                          r"|\d{1,2}\.\d{1,2}\.%s)(?![\d./])" % (_Y, _Y))
_MONTH = (r"(?:jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|june?|july?"
          r"|aug(?:ust)?|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)\.?")
_DAY = r"\d{1,2}(?:st|nd|rd|th)?"
# 1 Jan 2001, 01 January 2001, Mon, 01 Jan 2001 00:00:00 GMT; Jan 1 2001,
# Jan 1, 2001, Jun 2001, Mon Jan  1 00:00:00 2001 (ctime).
WRITTEN_DATE = re.compile(r"\b(?:%s\s+%s,?\s+%s|%s[\s,]+(?:%s[\s,]+)?(?:\d\d:\d\d(?::\d\d)?\s+)?%s)\b"
                          % (_DAY, _MONTH, _Y, _MONTH, _DAY, _Y), re.IGNORECASE)
LEAKS = (
    ("a private network address (192.168.)", re.compile(r"192\.168\.")),
    ("a URL (scheme://)", re.compile(r"[A-Za-z][A-Za-z0-9+.-]*://")),
    ("an ssid", re.compile(r"ssid", re.IGNORECASE)),
    ('an "ip" key', re.compile(r'"ip"\s*:')),
    ("a Unix time", EPOCH_NUMBER),
    ("a compact date and time", COMPACT_DATE_TIME),
    ("a compact date in a name", COMPACT_DATE),
    ("a numeric date (slashes or dots)", NUMERIC_DATE),
    ("a written date", WRITTEN_DATE),
)


def _dumps(obj):
    return json.dumps(obj, separators=(",", ":"), ensure_ascii=False)


def _object(payload):
    try:
        obj = json.loads(payload)
    except ValueError:
        return None
    return obj if isinstance(obj, dict) else None


def _scalar(v):
    return not isinstance(v, (dict, list))


def _allow(obj, spec, path, dropped):
    """obj (an object) reduced to spec's keys, in obj's own order. Each key
    dropped is counted in dropped under its path."""
    out = {}
    for k, v in obj.items():
        where = path + k
        if k not in spec:
            dropped[where] += 1
            continue
        sub = spec[k]
        if sub is VALUE:
            if _scalar(v) or (isinstance(v, list) and all(_scalar(x) for x in v)):
                out[k] = v
            else:
                dropped[where] += 1
        elif isinstance(sub, dict):
            if isinstance(v, dict):
                out[k] = _allow(v, sub, where + ".", dropped)
            else:
                dropped[where] += 1
        else:  # [spec]: a list of objects
            if isinstance(v, list) and all(isinstance(x, dict) for x in v):
                out[k] = [_allow(x, sub[0], where + "[].", dropped) for x in v]
            else:
                dropped[where] += 1
    return out


def _filter(payload, spec, kind, dropped, fix=None):
    """payload reduced to spec: verbatim when nothing is dropped (so floats
    and key order are as sent), re-encoded otherwise; None if it isn't an
    object. fix(obj) may rewrite the kept object and says whether the
    verbatim text still stands."""
    obj = _object(payload)
    if obj is None:
        return None
    mine = collections.Counter()
    kept = _allow(obj, spec, kind + ": ", mine)
    dropped.update(mine)
    verbatim = not mine
    if fix is not None:
        verbatim = fix(kept) and verbatim
    return payload if verbatim else _dumps(kept)


def _version(payload):
    """A status fix: version becomes "fixture"."""
    def fix(kept):
        if "version" not in kept:
            return True
        was = kept["version"]
        kept["version"] = "fixture"
        return isinstance(was, str)
    return fix


def _tanktemp_spec(topic):
    """(spec, kind) for a tanktemp topic, or None for one not kept."""
    for p in KEEP_PREFIXES:
        if not topic.startswith(p + "/"):
            continue
        rest = topic[len(p) + 1:]
        if rest == "status":
            return TANKTEMP_STATUS, "tanktemp status"
        if rest == "meter":
            return TANKTEMP_METER, "tanktemp meter"
        if DEVICE_STATE.match(rest):
            return TANKTEMP_STATE, "tanktemp state"
    return None


def reduce(topic, payload, dropped=None):
    """The payload to keep for topic, or None to drop the message. Keys
    outside the topic's allow-list are dropped and counted in dropped (a
    Counter), by kind and path."""
    if dropped is None:
        dropped = collections.Counter()
    tt = _tanktemp_spec(topic)
    if tt is not None:
        spec, kind = tt
        if kind == "tanktemp status":
            out = _filter(payload, spec, kind, dropped, _version(payload))
            return out if out is None else VERSION.sub('"version":"fixture"', out)
        return _filter(payload, spec, kind, dropped)
    if topic == BOILER_EVENTS:
        return _filter(payload, BOILER_EVENT, "boiler event", dropped)
    if topic == BOILER_SYSTEM:
        obj = _object(payload)
        if obj is None:
            return None
        for shape, fields in SYSTEM_FIELDS.items():
            inner = obj.get(shape)
            if isinstance(inner, dict):
                # Reduced by design, not counted: the heartbeat's network,
                # config and counters are known and never wanted.
                return _dumps({shape: _allow(inner, fields, "", collections.Counter())})
        return None
    if topic == CLIMATE:
        obj = _object(payload)
        if obj is None:
            return None
        kept = _allow(obj, CLIMATE_FIELDS, "", collections.Counter())
        return _dumps(kept) if kept else None
    return None


def _offset_s(zone):
    if not zone or zone == "Z":
        return 0
    sign = -1 if zone[0] == "-" else 1
    digits = zone[1:].replace(":", "")
    return sign * (int(digits[:2]) * 3600 + int(digits[2:]) * 60)


def _epoch(m):
    """Whole UTC seconds of an ISO match (its fraction ignored)."""
    y, mo, d, h, mi, s = (int(m.group(i)) for i in range(1, 7))
    return calendar.timegm((y, mo, d, h, mi, s)) - _offset_s(m.group(8))


def parse_time(text):
    """Whole UTC epoch seconds of an ISO-8601 time that names its zone."""
    m = ISO.fullmatch(text.strip())
    if not m:
        raise ValueError("not an ISO-8601 time: %r" % text)
    if m.group(7):
        raise ValueError("time must be whole seconds: %r" % text)
    if not m.group(8):
        raise ValueError("time must name its zone (Z or +hh:mm): %r" % text)
    return _epoch(m)


def shift_for(start):
    """The whole-second shift that takes start to the fake epoch."""
    return FAKE_EPOCH - start


def _format(epoch, frac, zone):
    local = time.gmtime(epoch + _offset_s(zone))
    return time.strftime("%Y-%m-%dT%H:%M:%S", local) + (frac or "") + (zone or "")


def shift_text(text, shift):
    """text with every ISO-8601 timestamp moved by shift seconds, keeping
    each one's fraction and zone text as written."""
    return ISO.sub(lambda m: _format(_epoch(m) + shift, m.group(7), m.group(8)), text)


class Stats:
    """What a cut kept and dropped."""

    def __init__(self, shift):
        self.shift = shift
        self.topics = {}      # topic -> [prelude count, window count]
        self.window = 0       # kept messages in [START, END)
        self.duplicates = 0   # retained replays dropped from the window
        self.dropped = 0      # window messages on topics not kept
        self.malformed = 0    # lines that are not 'TIME TOPIC PAYLOAD'
        self.unknown_keys = collections.Counter()  # "kind: path" -> times dropped


def _split(raw):
    parts = raw.rstrip("\n").split(" ", 2)
    if len(parts) < 3:
        return None
    try:
        received = parse_time(parts[0])
    except ValueError:
        return None
    return received, parts[0], parts[1], parts[2]


def cut(lines, start, end):
    """The fixture lines for [start, end) of a recording, and its Stats.

    lines: the recording's raw lines, in the order received."""
    shift = shift_for(start)
    stats = Stats(shift)
    seen = set()
    prelude = {}  # topic -> (index received, payload)
    window = []
    for i, raw in enumerate(lines):
        msg = _split(raw)
        if msg is None:
            stats.malformed += 1
            continue
        received, received_text, topic, payload = msg
        if received >= end:
            continue
        dropped = collections.Counter()
        kept = reduce(topic, payload, dropped)
        if kept is None:
            if received >= start:
                stats.dropped += 1
            continue
        key = (topic, payload)
        if key in seen:
            if received >= start:
                stats.duplicates += 1
            continue
        seen.add(key)
        if received < start:
            prelude[topic] = (i, kept, dropped)
            continue
        stats.unknown_keys.update(dropped)
        window.append((received_text, topic, kept))
        stats.topics.setdefault(topic, [0, 0])[1] += 1
        stats.window += 1
    out = []
    for topic, (_, kept, dropped) in sorted(prelude.items(), key=lambda kv: kv[1][0]):
        stats.topics.setdefault(topic, [0, 0])[0] += 1
        stats.unknown_keys.update(dropped)
        out.append("%s %s %s\n" % (_format(FAKE_EPOCH, None, "+0000"), topic,
                                   shift_text(kept, shift)))
    for received_text, topic, kept in window:
        out.append("%s %s %s\n" % (shift_text(received_text, shift), topic,
                                   shift_text(kept, shift)))
    return out, stats



def problems(lines):
    """Why these fixture lines must not be written: one message per line and
    kind, naming the line but never echoing its data."""
    found = []
    for n, text in enumerate(lines, 1):
        for what, pattern in LEAKS:
            if pattern.search(text):
                found.append("line %d: %s" % (n, what))
        years = {m.group(1) for m in DATE.finditer(text)} - set(FAKE_YEARS)
        if years:
            found.append("line %d: a date outside the fake epoch (a real date survived)" % n)
    return found


def render(lines):
    """Deterministic gzip of the fixture: no file name, mtime 0."""
    buf = io.BytesIO()
    with gzip.GzipFile(filename="", mode="wb", fileobj=buf, mtime=0, compresslevel=9) as gz:
        gz.write("".join(lines).encode("utf-8"))
    return buf.getvalue()


def read_lines(paths):
    """Every line of each input, in the order given (.gz inputs too)."""
    for path in paths:
        opener = gzip.open if path.endswith(".gz") else open
        with opener(path, "rt", encoding="utf-8", errors="replace") as f:
            for raw in f:
                yield raw


class Job:
    """One fixture to cut: its output, window and inputs."""

    def __init__(self, name, start, end, out, inputs):
        self.name, self.start, self.end, self.out, self.inputs = name, start, end, out, inputs
        self.lines = self.stats = self.data = None
        self.state = None  # new / unchanged / changed

    def run(self):
        self.lines, self.stats = cut(read_lines(self.inputs), self.start, self.end)
        self.data = render(self.lines)
        if not os.path.exists(self.out):
            self.state = "new"
        else:
            with open(self.out, "rb") as f:
                self.state = "unchanged" if f.read() == self.data else "changed"

    def refusals(self):
        if self.stats.window == 0:
            return ["%s: the window selects nothing (no kept message in [--from, --to))"
                    % self.name]
        found = problems(self.lines)
        shown = ["%s: %s" % (self.name, p) for p in found[:10]]
        if len(found) > 10:
            shown.append("%s: ... and %d more" % (self.name, len(found) - 10))
        return shown

    def report(self, out):
        st = self.stats
        prelude = sum(c[0] for c in st.topics.values())
        out.write("%s: %d lines (%d prelude + %d window), shift %+d s; dropped %d replays, "
                  "%d other-topic messages, %d malformed lines\n"
                  % (self.name, prelude + st.window, prelude, st.window, st.shift,
                     st.duplicates, st.dropped, st.malformed))
        n = sum(st.unknown_keys.values())
        out.write("  dropped %d unknown key%s (not in the topic's allow-list)%s\n"
                  % (n, "" if n == 1 else "s", ":" if n else ""))
        for key in sorted(st.unknown_keys):
            out.write("    %s x%d\n" % (key, st.unknown_keys[key]))
        width = max([len(t) for t in st.topics] + [5])
        out.write("  %-*s %7s %7s\n" % (width, "topic", "prelude", "window"))
        for topic in sorted(st.topics):
            p, w = st.topics[topic]
            out.write("  %-*s %7d %7d\n" % (width, topic, p, w))


def interactive():
    return sys.stdin.isatty()


def _confirm(changed, assume_yes, err):
    if not changed or assume_yes:
        return True
    names = ", ".join(j.out for j in changed)
    if not interactive():
        err.write("refusing to replace %s: it exists and differs; re-run with --yes to "
                  "replace it\n" % names)
        return False
    sys.stdout.write("about to replace %s (it differs). Continue? [y/N] " % names)
    sys.stdout.flush()
    return sys.stdin.readline().strip().lower() in ("y", "yes")


def _write(path, data):
    os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix=".fixture-", dir=os.path.dirname(os.path.abspath(path)))
    try:
        with os.fdopen(fd, "wb") as f:
            f.write(data)
        os.chmod(tmp, 0o644)
        os.replace(tmp, path)
    except BaseException:
        os.unlink(tmp)
        raise


class _Usage(Exception):
    pass


def _parser():
    ap = argparse.ArgumentParser(
        prog="make-fixture.sh",
        description="Cut a privacy-shifted washhouse fixture from a recording.")
    ap.add_argument("--check", action="store_true",
                    help="report what would be written; write nothing")
    ap.add_argument("--yes", action="store_true",
                    help="replace an existing fixture that differs without asking")
    ap.add_argument("--from", dest="start", metavar="ISO",
                    help="window start, inclusive (whole seconds, with zone)")
    ap.add_argument("--to", dest="end", metavar="ISO", help="window end, exclusive")
    ap.add_argument("--out", metavar="FILE.log.gz", help="the fixture to write")
    ap.add_argument("inputs", nargs="*", metavar="INPUT.log",
                    help="recordings, in time order")
    ap.add_argument("--slices", metavar="FILE.tsv",
                    help="cut every row of a TSV: name, from, to, inputs (space-separated, "
                         "relative to the TSV), description")
    ap.add_argument("--out-dir", metavar="DIR", default=DEFAULT_OUT_DIR,
                    help="where --slices writes NAME.log.gz (default: testdata/fixtures)")
    return ap


DEFAULT_OUT_DIR = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                               "testdata", "fixtures")
NAME = re.compile(r"[a-z0-9][a-z0-9-]*\Z")


def _jobs(args):
    if args.slices:
        if args.start or args.end or args.out or args.inputs:
            raise _Usage("--slices cuts every row of its table; it takes no --from, --to, "
                         "--out or INPUT")
        return _slice_jobs(args.slices, args.out_dir)
    if not (args.start and args.end and args.out and args.inputs):
        raise _Usage("need --from, --to, --out and at least one INPUT (or --slices FILE)")
    return [_job(os.path.basename(args.out), args.start, args.end, args.out, args.inputs)]


def _slice_jobs(path, out_dir):
    try:
        with open(path, encoding="utf-8") as f:
            rows = f.read().splitlines()
    except OSError as e:
        raise _Usage("cannot read %s: %s" % (path, e.strerror))
    base = os.path.dirname(os.path.abspath(path))
    jobs, names = [], set()
    for n, row in enumerate(rows, 1):
        if not row.strip() or row.lstrip().startswith("#"):
            continue
        where = "%s line %d" % (path, n)
        cols = row.split("\t")
        if len(cols) not in (4, 5):
            raise _Usage("%s: want name, from, to, inputs[, description] separated by "
                         "tabs; got %d columns" % (where, len(cols)))
        name, start, end, inputs = (c.strip() for c in cols[:4])
        if not NAME.match(name):
            raise _Usage("%s: name %r must be lower-case letters, digits and -" % (where, name))
        if name in names:
            raise _Usage("%s: name %r appears twice" % (where, name))
        names.add(name)
        paths = [os.path.join(base, os.path.expanduser(p)) for p in inputs.split()]
        if not paths:
            raise _Usage("%s: no inputs" % where)
        try:
            jobs.append(_job(name, start, end, os.path.join(out_dir, name + ".log.gz"), paths))
        except _Usage as e:
            raise _Usage("%s: %s" % (where, e))
    if not jobs:
        raise _Usage("%s has no rows" % path)
    return jobs


def _job(name, start, end, out, inputs):
    try:
        t0, t1 = parse_time(start), parse_time(end)
    except ValueError as e:
        raise _Usage("%s: %s" % (name, e))
    if t1 <= t0:
        raise _Usage("%s: --to must be after --from" % name)
    if not out.endswith(".log.gz"):
        raise _Usage("%s: the output must be a .log.gz file" % name)
    for path in inputs:
        if not os.path.isfile(path):
            raise _Usage("%s: no such input file: %s" % (name, path))
    return Job(name, t0, t1, out, inputs)


def main(argv=None):
    out, err = sys.stdout, sys.stderr
    ap = _parser()
    try:
        args = ap.parse_args(argv)
        jobs = _jobs(args)
    except SystemExit as e:
        return e.code
    except _Usage as e:
        err.write("make-fixture: %s\n" % e)
        return 2
    refused = []
    for job in jobs:
        job.run()
        job.report(out)
        refused += job.refusals()
        if args.check:
            out.write("  would write %s (%s, %d bytes)\n" % (job.out, job.state, len(job.data)))
    if refused:
        for r in refused:
            err.write("make-fixture: refusing: %s\n" % r)
        return 1
    if args.check:
        return 0
    if not _confirm([j for j in jobs if j.state == "changed"], args.yes, err):
        return 1
    for job in jobs:
        if job.state == "unchanged":
            out.write("unchanged %s\n" % job.out)
            continue
        _write(job.out, job.data)
        out.write("wrote %s\n" % job.out)
    return 0


if __name__ == "__main__":
    sys.exit(main())
