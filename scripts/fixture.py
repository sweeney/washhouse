#!/usr/bin/env python3
"""Cut a washhouse test fixture from a live recording.

Input: mosquitto_sub -F '%I %t %p' lines (receive time, topic, one-line JSON
payload), as tools/record.sh writes them. Output: the same format, gzipped
deterministically, with

  - only the topics washhouse reads, the boiler heartbeat reduced to its
    state fields and the boiler-room climate to temperature and humidity;
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
# Per payload shape of the boiler system topic: the fields kept.
SYSTEM_FIELDS = {
    "status": ("event", "ch", "hw", "ready", "timestamp"),
    "system": ("timestamp", "event", "reason", "source"),
}
CLIMATE_FIELDS = ("temperature", "humidity")
# tanktemp's build version is a public commit hash; its commit date would pin
# a fixture to a real day. Replaced in place, so key order stays verbatim.
VERSION = re.compile(r'"version":"[^"]*"')

FAKE_EPOCH = calendar.timegm((2001, 1, 1, 0, 0, 0))
# An ISO-8601 timestamp: date, time, optional fraction, optional zone.
ISO = re.compile(r"(?<!\d)(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})"
                 r"(\.\d+)?(Z|[+-]\d{2}:?\d{2})?(?!\d)")

# What must not survive into a fixture.
FAKE_YEARS = ("2000", "2001")
DATE = re.compile(r"(?<!\d)(\d{4})-\d{2}-\d{2}(?!\d)")
# Unix times in seconds or milliseconds, 2001..2036 (zigbee's last_seen is one).
EPOCH_NUMBER = re.compile(r"(?<![\d.])(1\d{9}|20\d{8}|1\d{12}|20\d{11})(?![\d.])")
LEAKS = (
    ("a private network address (192.168.)", re.compile(r"192\.168\.")),
    ("an ssid", re.compile(r"ssid", re.IGNORECASE)),
    ('an "ip" key', re.compile(r'"ip"\s*:')),
    ("a Unix time", EPOCH_NUMBER),
)


def _dumps(obj):
    return json.dumps(obj, separators=(",", ":"), ensure_ascii=False)


def _subset(obj, fields):
    """obj's fields that are in fields, in obj's own order."""
    return {k: v for k, v in obj.items() if k in fields}


def _object(payload):
    try:
        obj = json.loads(payload)
    except ValueError:
        return None
    return obj if isinstance(obj, dict) else None


def reduce(topic, payload):
    """The payload to keep for topic, or None to drop the message."""
    for p in KEEP_PREFIXES:
        if topic == p or topic.startswith(p + "/"):
            return VERSION.sub('"version":"fixture"', payload)
    if topic == BOILER_EVENTS:
        return payload
    if topic == BOILER_SYSTEM:
        obj = _object(payload)
        if obj is None:
            return None
        for shape, fields in SYSTEM_FIELDS.items():
            inner = obj.get(shape)
            if isinstance(inner, dict):
                return _dumps({shape: _subset(inner, fields)})
        return None
    if topic == CLIMATE:
        obj = _object(payload)
        if obj is None:
            return None
        kept = _subset(obj, CLIMATE_FIELDS)
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
        kept = reduce(topic, payload)
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
            prelude[topic] = (i, kept)
            continue
        window.append((received_text, topic, kept))
        stats.topics.setdefault(topic, [0, 0])[1] += 1
        stats.window += 1
    out = []
    for topic, (_, kept) in sorted(prelude.items(), key=lambda kv: kv[1][0]):
        stats.topics.setdefault(topic, [0, 0])[0] += 1
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
