"""Tests for scripts/fixture.py. Synthetic inputs only: no real recordings.

Run: cd scripts && python3 -m unittest -q fixture_test
"""
import collections
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


STATE = ('{"device_id":"hot_water_cylinder_left","timestamp":"2030-06-01T10:00:00Z",'
         '"clock_synced":true,"sweep_ms":4000,"sensors":[{"position":"bottom","order":1,'
         '"height_mm":null,"rom":"28-00001172de60","temp_c":20.875,"raw_c":20.875,'
         '"offset_c":0,"error":null,"suspect":null}]}')
STATUS = ('{"online":true,"event":"HEARTBEAT","timestamp":"2030-06-01T10:00:00Z",'
          '"version":"8009cb7","ready":true,"uptime_s":100,"tanks":["a"],"meters":["m"],'
          '"sensors_ok":5,"sensors_failed":0,"unexpected":[],"startup_problems":'
          '[{"rom":null,"code":"discover_failed","message":"no 1-Wire bus masters"}]}')


def with_key(payload, key_json, before):
    """payload with key_json inserted just before the text before."""
    assert payload.count(before) >= 1, before
    return payload.replace(before, key_json + "," + before, 1)


class AllowList(unittest.TestCase):
    """tanktemp and boiler payloads keep only the keys WIRE.md (or
    boiler-sensor's wire contract) documents: an unknown key is dropped and
    counted, so a new upstream field never reaches a fixture unreviewed."""

    def reduce(self, topic, payload):
        dropped = collections.Counter()
        return fixture.reduce(topic, payload, dropped), dropped

    def test_documented_payloads_are_kept_verbatim(self):
        meter = METER % ("2030-06-01T10:00:05Z", 1)
        for topic, p in (("water/hot/tanktemp/meter", meter),
                         ("water/hot/tanktemp/hot_water_cylinder_left/state", STATE),
                         ("energy/hot_water/hot_water_cylinder_left/state", STATE),
                         ("water/hot/tanktemp/status", '{"online":false,"event":"LWT"}'),
                         ("energy/boiler/sensor/events", event("2030-06-01T10:00:00Z", "HW_ON"))):
            out, dropped = self.reduce(topic, p)
            self.assertEqual(out, p, topic)
            self.assertEqual(dropped, {}, topic)

    def test_status_version_replaced_and_rest_verbatim(self):
        out, dropped = self.reduce("water/hot/tanktemp/status", STATUS)
        self.assertEqual(out, STATUS.replace('"8009cb7"', '"fixture"'))
        self.assertEqual(dropped, {})

    def test_unknown_state_key_dropped(self):
        p = with_key(STATE, '"build":"tanktemp-x"', '"sensors"')
        out, dropped = self.reduce("water/hot/tanktemp/hot_water_cylinder_left/state", p)
        self.assertEqual(json.loads(out), json.loads(STATE))
        self.assertEqual(dropped, {"tanktemp state: build": 1})

    def test_unknown_sensor_key_dropped(self):
        p = with_key(STATE, '"serial":"x"', '"suspect"')
        out, dropped = self.reduce("energy/hot_water/hot_water_cylinder_left/state", p)
        self.assertEqual(json.loads(out), json.loads(STATE))
        self.assertEqual(dropped, {"tanktemp state: sensors[].serial": 1})

    def test_unknown_status_keys_dropped_and_version_still_replaced(self):
        p = with_key(STATUS, '"commit_date":"x","host":{"name":"y"}', '"ready"')
        p = with_key(p, '"detail":"z"', '"message"')
        out, dropped = self.reduce("water/hot/tanktemp/status", p)
        want = json.loads(STATUS)
        want["version"] = "fixture"
        self.assertEqual(json.loads(out), want)
        self.assertEqual(list(json.loads(out)), list(want), "key order kept")
        self.assertEqual(dropped, {"tanktemp status: commit_date": 1, "tanktemp status: host": 1,
                                   "tanktemp status: startup_problems[].detail": 1})

    def test_non_string_version_replaced(self):
        out, _ = self.reduce("water/hot/tanktemp/status", '{"online":true,"version":12345}')
        self.assertEqual(json.loads(out), {"online": True, "version": "fixture"})

    def test_unknown_meter_key_dropped(self):
        p = with_key(METER % ("2030-06-01T10:00:05Z", 1), '"gpio":27', '"noise"')
        out, dropped = self.reduce("water/hot/tanktemp/meter", p)
        self.assertEqual(json.loads(out), json.loads(METER % ("2030-06-01T10:00:05Z", 1)))
        self.assertEqual(dropped, {"tanktemp meter: gpio": 1})

    def test_object_where_a_value_belongs_is_dropped(self):
        p = METER % ("2030-06-01T10:00:05Z", 1)
        p = p.replace('"device_id":"hot_water_meter"', '"device_id":{"host":"x"}')
        out, dropped = self.reduce("water/hot/tanktemp/meter", p)
        self.assertNotIn("device_id", json.loads(out))
        self.assertEqual(dropped, {"tanktemp meter: device_id": 1})
        p = STATUS.replace('"tanks":["a"]', '"tanks":[{"host":"x"}]')
        out, dropped = self.reduce("water/hot/tanktemp/status", p)
        self.assertNotIn("tanks", json.loads(out))

    def test_boiler_event_reduced_to_its_fields(self):
        p = ('{"boiler":{"timestamp":"2030-06-01T10:00:00Z","event":"HW_ON","version":"abc1234",'
             '"ch":{"state":"OFF","pin":17},"hw":{"state":"ON"}},"network":{"ip":"x"}}')
        out, dropped = self.reduce("energy/boiler/sensor/events", p)
        self.assertEqual(out, event("2030-06-01T10:00:00Z", "HW_ON"))
        self.assertEqual(dropped, {"boiler event: boiler.version": 1,
                                   "boiler event: boiler.ch.pin": 1,
                                   "boiler event: network": 1})

    def test_unknown_tanktemp_topics_and_non_objects_dropped(self):
        for topic in ("water/hot/tanktemp/meter/second", "water/hot/tanktemp/debug",
                      "water/hot/tanktemp/a/b/state", "energy/hot_water/x/config",
                      "water/hot/tanktemp"):
            self.assertIsNone(fixture.reduce(topic, '{"a":1}'), topic)
        for p in ("not json", "[1]", "null", '"s"'):
            self.assertIsNone(fixture.reduce("water/hot/tanktemp/meter", p), p)
            self.assertIsNone(fixture.reduce("energy/boiler/sensor/events", p), p)

    def test_cut_counts_dropped_keys_and_report_shows_them(self):
        p = with_key(METER % ("2030-06-01T10:00:05Z", 1), '"gpio":27', '"noise"')
        _, stats = fixture.cut([line(recv("10:00:05"), "water/hot/tanktemp/meter", p)],
                               fixture.parse_time(START), fixture.parse_time(END))
        self.assertEqual(stats.unknown_keys, {"tanktemp meter: gpio": 1})
        job = fixture.Job("x", 0, 1, "x.log.gz", [])
        job.stats = stats
        buf = io.StringIO()
        job.report(buf)
        self.assertIn("dropped 1 unknown key", buf.getvalue())
        self.assertIn("tanktemp meter: gpio", buf.getvalue())


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

    # Forms the ISO shift can't move, so any of them is a real date or a
    # network detail. tanktemp's payloads use ISO for every time, so there is
    # no fake-epoch exception: a compact or written date in 2000/2001 is
    # refused too.
    def refused(self, payload):
        return self.problems(FAKE + "+0000 t " + payload + "\n")

    def test_refuses_a_url(self):
        for bad in ("tcp://broker:1883", "mqtt://10.9.9.9", "https://example.org/x",
                    "ws+tls://h"):
            self.assertTrue(self.refused('{"broker":"%s"}' % bad), bad)

    def test_refuses_a_compact_date_time_with_z(self):
        self.assertTrue(self.refused('{"at":"20010101T000000Z"}'))
        self.assertTrue(self.refused('{"at":"20301231T235959Z"}'))

    def test_refuses_a_compact_date_time_without_z(self):
        self.assertTrue(self.refused('{"at":"20300601T100000"}'))
        self.assertTrue(self.refused('{"at":"20300601100000"}'))

    def test_refuses_a_build_name_with_a_date_and_time(self):
        self.assertTrue(self.refused('{"build":"tanktemp-20301120-221500-4aba4d5"}'))
        self.assertTrue(self.refused('{"build":"tanktemp_20301120_2215"}'))

    def test_refuses_a_compact_date_in_a_name(self):
        self.assertTrue(self.refused('{"build":"tanktemp-20301120"}'))
        self.assertTrue(self.refused('{"build":"v20010101.1"}'))

    def test_refuses_a_fractional_unix_time(self):
        for bad in ("1906452000.5", "1906452000.125", "1906452000000.5"):
            self.assertTrue(self.refused('{"t":%s}' % bad), bad)

    def test_refuses_a_slashed_or_dotted_date(self):
        for bad in ("2001/01/01", "2030/6/1", "01/01/2001", "1/6/30", "2030.06.01",
                    "1.6.2030"):
            self.assertTrue(self.refused('{"d":"%s"}' % bad), bad)

    def test_refuses_a_day_month_year(self):
        for bad in ("1 Jan 2001", "01 January 2001", "1st june 2030",
                    "Mon, 01 Jan 2001 00:00:00 GMT"):
            self.assertTrue(self.refused('{"d":"%s"}' % bad), bad)

    def test_refuses_a_month_day_year(self):
        for bad in ("Jan 1 2001", "Jan 1, 2001", "September 30, 2030", "Jun 2030",
                    "Mon Jan  1 00:00:00 2001"):
            self.assertTrue(self.refused('{"d":"%s"}' % bad), bad)

    def test_no_false_alarms_on_payload_values(self):
        # ROMs, versions, temperatures, large counts, uptimes and words that
        # start like a month stay clean.
        for ok in ('{"rom":"28-00001172de60","rom2":"28-200117211234ab"}',
                   '{"v":"1.2.3","v2":"0.10.12"}', '{"temp_c":61.185,"raw_c":-0.0625}',
                   '{"uptime_s":20011231,"count":12345678}',
                   '{"note":"market 2001 marching mayday"}', '{"path":"a/b/c"}',
                   '{"ratio":"1/2"}'):
            self.assertEqual(self.refused(ok), [], ok)

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
                                             '{"online":false,"event":"SHUTDOWN","reason":"192.168.7.7"}'))
        code, _, err = self.run_main("--from", START, "--to", END, "--out", self.out, leaky)
        self.assertEqual(code, 1)
        self.assertIn("192.168.", err)
        self.assertNotIn("192.168.7.7", err)
        self.assertFalse(os.path.exists(self.out))

    def test_refuses_a_real_date_left_in_a_payload(self):
        # A timestamp over a year before START shifts out of the fake epoch.
        # (In an allowed key: the allow-list would drop an unknown one.)
        old = self.write("old.log", line(recv("10:00:01"), "water/hot/tanktemp/status",
                                         '{"timestamp":"2028-01-01T00:00:00Z"}'))
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
