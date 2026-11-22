"""Tests for scripts/fixture.py. Synthetic inputs only: no real recordings.

Run: cd scripts && python3 -m unittest -q fixture_test
"""
import gzip
import io
import json
import os
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from unittest import mock

import fixture

# Synthetic "real" epoch for the tests: the year 2030, so no test datum looks
# like a recording. START is 2030-06-01T10:00:00Z.
START = "2030-06-01T10:00:00Z"
END = "2030-06-01T11:00:00Z"
FAKE = "2001-01-01T00:00:00"

METER = ('{"device_id":"hot_water_meter","timestamp":"%s","clock_synced":true,'
         '"trigger":"sweep","seq":%d,"litres_per_pulse":1,"level":"closed",'
         '"counting_since":"2030-06-01T09:00:00Z","count":3,'
         '"last_pulse_at":"2030-06-01T09:59:58.125Z","last_interval_ms":9000,'
         '"last_closed_ms":2500,"noise":0,"short":0,"glitches":0,"lost":0}')

HEARTBEAT = ('{"status":{"event":"HEARTBEAT","ch":"OFF","hw":"ON","ready":true,'
             '"uptime_seconds":1000,"start_time":"2030-05-30T00:00:00Z",'
             '"timestamp":"%s","mqtt":{"connected":true,"broker":"tcp://10.9.9.9:1883"},'
             '"event_counts":{"ch_on":1},"network":{"type":"wifi","ip":"10.9.9.8",'
             '"gateway":"10.9.9.1","ssid":"examplenet"},"config":{"poll_ms":100}}}')

CLIMATE = ('{"battery":100,"humidity":40.5,"humidity_calibration":0,'
           '"last_seen":1906452000000,"linkquality":170,"temperature":21.25,'
           '"temperature_calibration":0,"update":{"installed_version":1,"state":"idle"}}')


def line(recv, topic, payload):
    return "%s %s %s\n" % (recv, topic, payload)


def recv(hms):
    """A mosquitto_sub %I receive time on the synthetic day."""
    return "2030-06-01T%s+0000" % hms


class Reduce(unittest.TestCase):
    def test_keeps_tanktemp_and_old_hot_water_topics_verbatim(self):
        p = METER % ("2030-06-01T10:00:05Z", 1)
        self.assertEqual(fixture.reduce("water/hot/tanktemp/meter", p), p)
        self.assertEqual(fixture.reduce("energy/hot_water/status", '{"online":true}'),
                         '{"online":true}')
        self.assertEqual(fixture.reduce("energy/boiler/sensor/events", '{"boiler":{}}'),
                         '{"boiler":{}}')

    def test_tanktemp_version_is_replaced(self):
        # A version is a public commit hash, and its commit date would pin
        # the fixture to a real day. Key order and the rest stay verbatim.
        p = '{"online":true,"event":"HEARTBEAT","version":"8009cb7","ready":true}'
        self.assertEqual(fixture.reduce("water/hot/tanktemp/status", p),
                         '{"online":true,"event":"HEARTBEAT","version":"fixture","ready":true}')
        self.assertEqual(fixture.reduce("energy/hot_water/status", '{"version":"ee654ac"}'),
                         '{"version":"fixture"}')

    def test_drops_other_topics(self):
        for topic in ("house/state/devices/hot_water", "water/hot/washhouse/draw",
                      "water/hot/tanktempX/meter", "energy/boiler/sensor/other",
                      "zigbee2mqtt/climate_groundfloor_kitchen",
                      "zigbee2mqtt/climate_groundfloor_boiler-room/availability"):
            self.assertIsNone(fixture.reduce(topic, '{"a":1}'), topic)

    def test_heartbeat_reduced_to_state_fields(self):
        out = fixture.reduce("energy/boiler/sensor/system",
                             HEARTBEAT % "2030-06-01T10:00:00Z")
        self.assertEqual(out, '{"status":{"event":"HEARTBEAT","ch":"OFF","hw":"ON",'
                              '"ready":true,"timestamp":"2030-06-01T10:00:00Z"}}')

    def test_system_event_keeps_its_fields(self):
        p = ('{"system":{"timestamp":"2030-06-01T10:00:00Z","event":"SHUTDOWN",'
             '"reason":"MQTT_DISCONNECT","source":"last_will","network":{"ip":"x"}}}')
        self.assertEqual(fixture.reduce("energy/boiler/sensor/system", p),
                         '{"system":{"timestamp":"2030-06-01T10:00:00Z","event":"SHUTDOWN",'
                         '"reason":"MQTT_DISCONNECT","source":"last_will"}}')

    def test_unknown_system_shape_dropped(self):
        self.assertIsNone(fixture.reduce("energy/boiler/sensor/system", '{"other":{}}'))
        self.assertIsNone(fixture.reduce("energy/boiler/sensor/system", 'not json'))

    def test_climate_reduced_to_temperature_and_humidity(self):
        out = fixture.reduce("zigbee2mqtt/climate_groundfloor_boiler-room", CLIMATE)
        self.assertEqual(json.loads(out), {"temperature": 21.25, "humidity": 40.5})
        self.assertNotIn("last_seen", out)
        self.assertIsNone(fixture.reduce("zigbee2mqtt/climate_groundfloor_boiler-room",
                                         '{"battery":100}'))


class Shift(unittest.TestCase):
    def setUp(self):
        self.shift = fixture.shift_for(fixture.parse_time(START))

    def test_start_maps_to_fake_epoch(self):
        self.assertEqual(fixture.shift_text("2030-06-01T10:00:00Z", self.shift),
                         FAKE + "Z")

    def test_shift_is_whole_seconds(self):
        self.assertIsInstance(self.shift, int)

    def test_keeps_precision_and_zone_text(self):
        s = self.shift
        self.assertEqual(fixture.shift_text("2030-06-01T10:00:01.5Z", s),
                         "2001-01-01T00:00:01.5Z")
        self.assertEqual(fixture.shift_text("2030-06-01T09:59:58.125Z", s),
                         "2000-12-31T23:59:58.125Z")
        self.assertEqual(fixture.shift_text("2030-06-01T10:00:02+0000", s),
                         "2001-01-01T00:00:02+0000")
        self.assertEqual(fixture.shift_text("2030-06-01T11:00:02+01:00", s),
                         "2001-01-01T01:00:02+01:00")
        self.assertEqual(fixture.shift_text("2030-06-01T10:00:03", s),
                         "2001-01-01T00:00:03")

    def test_shifts_every_timestamp_in_a_payload(self):
        p = METER % ("2030-06-01T10:00:05Z", 7)
        out = fixture.shift_text(p, self.shift)
        o = json.loads(out)
        self.assertEqual(o["timestamp"], "2001-01-01T00:00:05Z")
        self.assertEqual(o["counting_since"], "2000-12-31T23:00:00Z")
        self.assertEqual(o["last_pulse_at"], "2000-12-31T23:59:58.125Z")
        self.assertNotIn("2030", out)
        # everything else untouched, byte for byte
        self.assertEqual(out.replace("2001-01-01T00:00:05Z", "2030-06-01T10:00:05Z")
                         .replace("2000-12-31T23:00:00Z", "2030-06-01T09:00:00Z")
                         .replace("2000-12-31T23:59:58.125Z", "2030-06-01T09:59:58.125Z"), p)

    def test_intervals_preserved_across_days_and_months(self):
        s = self.shift
        a = fixture.parse_time(fixture.shift_text("2030-06-30T23:59:59Z", s))
        b = fixture.parse_time(fixture.shift_text("2030-07-01T00:00:01Z", s))
        self.assertEqual(b - a, 2)

    def test_parse_time_forms(self):
        t = fixture.parse_time(START)
        self.assertEqual(fixture.parse_time("2030-06-01T10:00:00+0000"), t)
        self.assertEqual(fixture.parse_time("2030-06-01T12:00:00+02:00"), t)
        for bad in ("2030-06-01T10:00:00", "2030-06-01T10:00:00.5Z", "yesterday"):
            with self.assertRaises(ValueError, msg=bad):
                fixture.parse_time(bad)


def tank(ts):
    return ('{"device_id":"hot_water_cylinder_left","timestamp":"%s","clock_synced":true,'
            '"sweep_ms":4000,"sensors":[{"position":"bottom","temp_c":20.5}]}' % ts)


def event(ts, name):
    return ('{"boiler":{"timestamp":"%s","event":"%s","ch":{"state":"OFF"},'
            '"hw":{"state":"ON"}}}' % (ts, name))


RECORDING = [
    # retained messages at the recorder's start, then history before START
    line(recv("08:00:00"), "water/hot/tanktemp/meter", METER % ("2030-06-01T07:59:50Z", 1)),
    line(recv("08:00:00"), "house/state/devices/hot_water", '{"id":"hot_water"}'),
    line(recv("09:00:00"), "energy/boiler/sensor/events", event("2030-06-01T09:00:00Z", "HW_ON")),
    line(recv("09:59:50"), "water/hot/tanktemp/meter", METER % ("2030-06-01T09:59:50Z", 2)),
    line(recv("09:59:55"), "water/hot/tanktemp/hot_water_cylinder_left/state",
         tank("2030-06-01T09:59:55Z")),
    # the window
    line(recv("10:00:00"), "water/hot/tanktemp/meter", METER % ("2030-06-01T10:00:00Z", 3)),
    line(recv("10:00:10"), "house/state/devices/hot_water", '{"id":"hot_water"}'),
    line(recv("10:00:15"), "water/hot/tanktemp/hot_water_cylinder_left/state",
         tank("2030-06-01T10:00:15Z")),
    # recorder reconnected: the broker replays retained messages, same payloads
    line(recv("10:30:00"), "water/hot/tanktemp/meter", METER % ("2030-06-01T10:00:00Z", 3)),
    line(recv("10:30:00"), "water/hot/tanktemp/hot_water_cylinder_left/state",
         tank("2030-06-01T10:00:15Z")),
    "garbage-without-topic\n",
    line(recv("10:59:59"), "energy/boiler/sensor/events", event("2030-06-01T10:59:59Z", "HW_OFF")),
    # END is exclusive
    line(recv("11:00:00"), "water/hot/tanktemp/meter", METER % ("2030-06-01T11:00:00Z", 4)),
]


class Cut(unittest.TestCase):
    def cut(self, lines=RECORDING, start=START, end=END):
        return fixture.cut(lines, fixture.parse_time(start), fixture.parse_time(end))

    def test_prelude_is_last_before_start_per_topic_stamped_start(self):
        out, _ = self.cut()
        prelude = out[:3]
        self.assertEqual([l.split(" ")[0] for l in prelude], [FAKE + "+0000"] * 3)
        # in the order they were received
        self.assertEqual([l.split(" ")[1] for l in prelude],
                         ["energy/boiler/sensor/events", "water/hot/tanktemp/meter",
                          "water/hot/tanktemp/hot_water_cylinder_left/state"])
        self.assertIn('"seq":2,', prelude[1])
        self.assertIn('"timestamp":"2000-12-31T23:00:00Z","event":"HW_ON"', prelude[0])

    def test_window_is_start_inclusive_end_exclusive_and_shifted(self):
        out, _ = self.cut()
        window = out[3:]
        self.assertEqual([l.split(" ", 2)[:2] for l in window], [
            ["2001-01-01T00:00:00+0000", "water/hot/tanktemp/meter"],
            ["2001-01-01T00:00:15+0000", "water/hot/tanktemp/hot_water_cylinder_left/state"],
            ["2001-01-01T00:59:59+0000", "energy/boiler/sensor/events"],
        ])
        self.assertTrue(all(l.endswith("\n") for l in out))
        self.assertNotIn("2030", "".join(out))

    def test_retained_replays_dropped(self):
        out, stats = self.cut()
        self.assertEqual(stats.duplicates, 2)
        self.assertEqual(sum('"seq":3,' in l for l in out), 1)

    def test_replay_of_prelude_message_dropped(self):
        lines = RECORDING[:5] + [
            line(recv("10:00:00"), "water/hot/tanktemp/meter", METER % ("2030-06-01T09:59:50Z", 2)),
        ]
        out, stats = self.cut(lines)
        self.assertEqual(sum('"seq":2,' in l for l in out), 1)
        self.assertEqual(stats.duplicates, 1)

    def test_counts_per_topic(self):
        _, stats = self.cut()
        self.assertEqual(stats.topics["water/hot/tanktemp/meter"], [1, 1])
        self.assertEqual(stats.topics["energy/boiler/sensor/events"], [1, 1])
        self.assertNotIn("house/state/devices/hot_water", stats.topics)
        self.assertEqual(stats.malformed, 1)
        self.assertEqual(stats.window, 3)

    def test_reduced_payload_is_shifted(self):
        lines = [line(recv("10:00:01"), "energy/boiler/sensor/system",
                      HEARTBEAT % "2030-06-01T10:00:01Z")]
        out, _ = self.cut(lines)
        self.assertEqual(out, ['2001-01-01T00:00:01+0000 energy/boiler/sensor/system '
                               '{"status":{"event":"HEARTBEAT","ch":"OFF","hw":"ON",'
                               '"ready":true,"timestamp":"2001-01-01T00:00:01Z"}}\n'])

    def test_nothing_in_window_selects_nothing(self):
        out, stats = self.cut(start="2030-06-02T00:00:00Z", end="2030-06-02T01:00:00Z")
        self.assertEqual(stats.window, 0)


class Privacy(unittest.TestCase):
    CLEAN = (FAKE + "+0000 water/hot/tanktemp/meter "
             '{"timestamp":"2000-12-31T23:59:59.5Z","count":3,"uptime_s":1561600,'
             '"temp_c":20.875,"rom":"28-00001172de60"}\n')

    def problems(self, text):
        return fixture.problems([self.CLEAN, text])

    def test_clean_output_has_no_problems(self):
        self.assertEqual(fixture.problems([self.CLEAN]), [])

    def test_refuses_private_address(self):
        self.assertTrue(self.problems('x t {"broker":"tcp://192.168.0.2:1883"}\n'))

    def test_refuses_ssid_any_case(self):
        for word in ("ssid", "SSID", "wifi_Ssid"):
            self.assertTrue(self.problems('x t {"%s":"n"}\n' % word), word)

    def test_refuses_ip_key(self):
        self.assertTrue(self.problems('x t {"net":{"ip" : "10.0.0.1"}}\n'))
        self.assertEqual(self.problems('x t {"tip":"a","ip_count":1}\n'), [])

    def test_refuses_any_real_date(self):
        for bad in ("2030-06-01T10:00:00Z", "1999-12-31T23:59:59Z", "2030-06-01",
                    "2002-01-01T00:00:00+0000"):
            self.assertTrue(self.problems(FAKE + '+0000 t {"at":"%s"}\n' % bad), bad)
        self.assertTrue(self.problems("2030-06-01T10:00:00+0000 t {}\n"))

    def test_refuses_epoch_numbers(self):
        for bad in ("1906452000", "1906452000000"):
            self.assertTrue(self.problems('x t {"last_seen":%s}\n' % bad), bad)
        self.assertEqual(self.problems('x t {"v":12345678901,"f":0.1906452000}\n'), [])

    def test_problem_names_the_line_not_the_data(self):
        (msg,) = self.problems('x t {"ssid":"examplenet"}\n')
        self.assertIn("line 2", msg)
        self.assertNotIn("examplenet", msg)


class CLI(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = self.tmp.name
        self.input = self.write("rec.log", "".join(RECORDING))
        self.out = os.path.join(self.dir, "fx", "cut.log.gz")
        patcher = mock.patch.object(fixture, "interactive", return_value=False)
        patcher.start()
        self.addCleanup(patcher.stop)
        self.addCleanup(self.tmp.cleanup)

    def write(self, name, text):
        path = os.path.join(self.dir, name)
        with open(path, "w") as f:
            f.write(text)
        return path

    def run_main(self, *argv):
        out, err = io.StringIO(), io.StringIO()
        with redirect_stdout(out), redirect_stderr(err):
            code = fixture.main(list(argv))
        return code, out.getvalue(), err.getvalue()

    def cut_args(self, *extra):
        return ("--from", START, "--to", END, "--out", self.out) + extra + (self.input,)

    def test_writes_shifted_gzip(self):
        code, out, err = self.run_main(*self.cut_args())
        self.assertEqual(code, 0, err)
        with gzip.open(self.out, "rt") as f:
            text = f.read()
        self.assertEqual(text, "".join(fixture.cut(RECORDING, fixture.parse_time(START),
                                                   fixture.parse_time(END))[0]))
        self.assertIn("wrote", out)

    def test_gzip_is_deterministic_and_rerun_is_a_no_op(self):
        self.run_main(*self.cut_args())
        with open(self.out, "rb") as f:
            first = f.read()
        self.assertEqual(first[4:8], b"\0\0\0\0", "gzip mtime must be 0")
        self.assertNotIn(b"cut.log", first, "no file name in the gzip header")
        os.utime(self.out, (1, 1))
        code, out, _ = self.run_main(*self.cut_args())
        self.assertEqual(code, 0)
        self.assertIn("unchanged", out)
        self.assertEqual(os.stat(self.out).st_mtime, 1, "an unchanged file is not rewritten")
        with open(self.out, "rb") as f:
            self.assertEqual(f.read(), first)

    def test_check_reports_and_writes_nothing(self):
        code, out, err = self.run_main(*self.cut_args("--check"))
        self.assertEqual(code, 0, err)
        self.assertFalse(os.path.exists(self.out))
        self.assertFalse(os.path.exists(os.path.dirname(self.out)))
        self.assertIn("water/hot/tanktemp/meter", out)
        self.assertIn("would write", out)
        self.assertIn("shift", out)

    def test_reads_gzip_input(self):
        gz = os.path.join(self.dir, "rec.log.gz")
        with gzip.open(gz, "wt") as f:
            f.write("".join(RECORDING))
        code, _, err = self.run_main("--from", START, "--to", END, "--out", self.out, gz)
        self.assertEqual(code, 0, err)

    def test_changed_output_needs_yes(self):
        self.run_main(*self.cut_args())
        with open(self.out, "rb") as f:
            before = f.read()
        later = "2030-06-01T10:00:15Z"
        code, _, err = self.run_main("--from", later, "--to", END, "--out", self.out,
                                     self.input)
        self.assertEqual(code, 1)
        self.assertIn("--yes", err)
        with open(self.out, "rb") as f:
            self.assertEqual(f.read(), before)
        code, out, err = self.run_main("--yes", "--from", later, "--to", END,
                                       "--out", self.out, self.input)
        self.assertEqual(code, 0, err)
        with open(self.out, "rb") as f:
            self.assertNotEqual(f.read(), before)

    def test_refuses_a_leak_and_writes_nothing(self):
        leaky = self.write("leaky.log", line(recv("10:00:01"), "water/hot/tanktemp/status",
                                             '{"online":true,"broker":"192.168.7.7"}'))
        code, _, err = self.run_main("--from", START, "--to", END, "--out", self.out, leaky)
        self.assertEqual(code, 1)
        self.assertIn("192.168.", err)
        self.assertNotIn("192.168.7.7", err)
        self.assertFalse(os.path.exists(self.out))

    def test_refuses_a_real_date_left_in_a_payload(self):
        # A timestamp over a year before START shifts out of the fake epoch.
        old = self.write("old.log", line(recv("10:00:01"), "water/hot/tanktemp/status",
                                         '{"since":"2028-01-01T00:00:00Z"}'))
        code, _, err = self.run_main("--from", START, "--to", END, "--out", self.out, old)
        self.assertEqual(code, 1)
        self.assertIn("real date", err)

    def test_refuses_an_empty_selection(self):
        code, _, err = self.run_main("--from", "2030-06-02T00:00:00Z",
                                     "--to", "2030-06-02T01:00:00Z", "--out", self.out,
                                     self.input)
        self.assertEqual(code, 1)
        self.assertIn("nothing", err)
        self.assertFalse(os.path.exists(self.out))

    def test_refuses_bad_arguments(self):
        for argv in (("--from", END, "--to", START, "--out", self.out, self.input),
                     ("--from", "2030-06-01T10:00:00", "--to", END, "--out", self.out,
                      self.input),
                     ("--from", START, "--to", END, "--out", self.dir + "/x.log",
                      self.input),
                     ("--from", START, "--to", END, "--out", self.out,
                      self.dir + "/missing.log")):
            code, _, err = self.run_main(*argv)
            self.assertEqual(code, 2, argv)
            self.assertTrue(err.strip(), argv)
        self.assertFalse(os.path.exists(self.out))


class Slices(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = self.tmp.name
        self.rec = os.path.join(self.dir, "rec")
        os.mkdir(self.rec)
        with open(os.path.join(self.rec, "a.log"), "w") as f:
            f.write("".join(RECORDING[:6]))
        with open(os.path.join(self.rec, "b.log"), "w") as f:
            f.write("".join(RECORDING[6:]))
        self.outdir = os.path.join(self.dir, "fixtures")
        patcher = mock.patch.object(fixture, "interactive", return_value=False)
        patcher.start()
        self.addCleanup(patcher.stop)

    def tsv(self, rows):
        path = os.path.join(self.rec, "slices.tsv")
        with open(path, "w") as f:
            f.write("# name\tfrom\tto\tinputs\tdescription\n\n")
            for r in rows:
                f.write("\t".join(r) + "\n")
        return path

    def run_main(self, *argv):
        out, err = io.StringIO(), io.StringIO()
        with redirect_stdout(out), redirect_stderr(err):
            code = fixture.main(list(argv))
        return code, out.getvalue(), err.getvalue()

    def test_cuts_each_row_with_inputs_relative_to_the_tsv(self):
        path = self.tsv([
            ("whole", START, END, "a.log b.log", "both files"),
            ("late", "2030-06-01T10:00:15Z", END, "b.log", "second only"),
        ])
        code, out, err = self.run_main("--slices", path, "--out-dir", self.outdir)
        self.assertEqual(code, 0, err)
        with gzip.open(os.path.join(self.outdir, "whole.log.gz"), "rt") as f:
            whole = f.read()
        expect, _ = fixture.cut(RECORDING, fixture.parse_time(START), fixture.parse_time(END))
        self.assertEqual(whole, "".join(expect))
        self.assertTrue(os.path.exists(os.path.join(self.outdir, "late.log.gz")))
        code, out, _ = self.run_main("--slices", path, "--out-dir", self.outdir)
        self.assertEqual(code, 0)
        self.assertEqual(out.count("unchanged"), 2)

    def test_check_writes_nothing(self):
        path = self.tsv([("whole", START, END, "a.log b.log", "")])
        code, out, err = self.run_main("--check", "--slices", path, "--out-dir", self.outdir)
        self.assertEqual(code, 0, err)
        self.assertIn("whole", out)
        self.assertFalse(os.path.exists(self.outdir))

    def test_one_bad_row_writes_nothing(self):
        path = self.tsv([
            ("good", START, END, "a.log b.log", ""),
            ("empty", "2030-06-02T00:00:00Z", "2030-06-02T01:00:00Z", "b.log", ""),
        ])
        code, _, err = self.run_main("--slices", path, "--out-dir", self.outdir)
        self.assertEqual(code, 1)
        self.assertIn("empty", err)
        self.assertFalse(os.path.exists(self.outdir))

    def test_refuses_malformed_tables(self):
        for rows in ([("bad name", START, END, "a.log", "")],
                     [("x", START, END, "a.log", ""), ("x", START, END, "b.log", "")],
                     [("x", START, END)],
                     [("x", START, END, "missing.log", "")]):
            code, _, err = self.run_main("--check", "--slices", self.tsv(rows),
                                         "--out-dir", self.outdir)
            self.assertEqual(code, 2, rows)
            self.assertIn("slices.tsv", err, rows)

    def test_slices_excludes_single_cut_arguments(self):
        path = self.tsv([("x", START, END, "a.log", "")])
        code, _, _ = self.run_main("--slices", path, "--from", START)
        self.assertEqual(code, 2)


if __name__ == "__main__":
    unittest.main()
