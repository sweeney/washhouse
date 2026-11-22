# Recorded fixtures

Slices of live recordings of the hot-water system, cut by
`scripts/make-fixture.sh` and shifted to a fake epoch. They are what
washhouse's meter, attribution and thermal tests replay.

## Format

Gzipped text, one MQTT message per line, as `mosquitto_sub -F '%I %t %p'`
writes them: `RECEIVE-TIME TOPIC PAYLOAD`, the payload one line of JSON.

- **Fake epoch.** Every timestamp, the receive time and every ISO-8601 time
  inside a payload, is moved by one whole number of seconds so that the
  slice's start is `2001-01-01T00:00:00Z`. Intervals are exact; times before
  the start (`counting_since`, the prelude's own timestamps) fall on
  31 Dec 2000. Real dates and times of day are gone: draw times are an
  occupancy pattern. Times below are offsets from the fixture's start.
- **Prelude.** The first lines, all stamped `2001-01-01T00:00:00+0000`, are
  each kept topic's last message before the start, in the order they were
  received: what a consumer connecting at the start would know. That
  includes topics the broker doesn't retain (boiler events, the boiler-room
  climate); their payload timestamps say how old they are.
- **Retained replays dropped.** A topic and payload seen before (the broker
  replaying retained messages when the recorder reconnected) appears once.
- **Topics kept:** `water/hot/tanktemp/#` (`energy/hot_water/#` in the
  older watch48b recording), `energy/boiler/sensor/events`,
  `energy/boiler/sensor/system` (a heartbeat keeps only
  `status.{event,ch,hw,ready,timestamp}`, a system event only
  `system.{timestamp,event,reason,source}`), and
  `zigbee2mqtt/climate_groundfloor_boiler-room` reduced to `humidity` and
  `temperature`. Nothing else: no statehouse topics, no network details.

The slice table (names and real times) lives outside the repo; with it,
`scripts/make-fixture.sh --slices TABLE` regenerates these files byte for
byte (gzip mtime 0, no file name in the header; byte identity assumes the
same zlib).

## Fixtures

Litres are `tools/meter_audit.py` run on the fixture itself (decompressed),
and agree draw for draw, to the 0.1 s, with the audit of the original
recording. Draw times are offsets from the start (h:mm:ss); "draws" are
meter_audit's segments (a draw ends after a minute with no litre).

| Fixture | Length | Size | Metered | What it shows |
|---|---|---|---|---|
| `shower-layered` | 55 min | 23 KB | 72 L | The 50 L shower from a well-layered tank, then the 15 L draw after it |
| `bath-in-reheat` | 27.5 min | 15 KB | 82 L | The 72 L bath during an HW reheat, taps before it |
| `bath-87` | 37.5 min | 22 KB | 106 L | The 87 L bath (also during that reheat), taps after |
| `taps-small` | 69.5 min | 21 KB | 9 L | Five 1–4 L tap draws in a CH-only spell |
| `creep-evening` | 2 h 35 min | 47 KB | 28 L | Creep: lone litres with the reed closed for minutes, among taps |
| `quiet-night` | 7 h 47 min | 53 KB | 0 L | A metered night with no draws |
| `reheat-morning` | 80 min | 19 KB | 3 L | An HW + CH reheat from HW_ON to HW_OFF |
| `ch-only-right-reheat` | 64 min | 8 KB | no meter | CH-only spell heating the right tank (probe-only) |
| `ch-only-no-heat` | 2 h 3.5 min | 24 KB | 8 L | A CH-only spell that heats neither tank |

### shower-layered

The tank was well layered after a reheat; tanktemp had restarted about five
minutes before the start (a fresh `counting_since`, seq from 11). This one
fixture also serves as **draw-after-shower**: the 15 L draw comes 19 minutes
after the shower ends, inside its 20-minute margin, so it is not cut
separately. It ends 16 minutes after that draw, just before a tanktemp
restart.

| Start | Litres | Over | Median flow |
|---|---|---|---|
| 0:01:22.208 | 6 | 61.4 s | 4.9 L/min |
| 0:10:10.308 | **50** (shower) | 456.3 s | 6.3 L/min |
| 0:34:08.688 | 1 | | |
| 0:36:41.928 | **15** (after the shower: the thermocline blind spot) | 131.9 s | 6.1 L/min |

Expect the heat balance within about 10% on the shower, and the 15 L draw
nearly invisible to the probes.

### bath-in-reheat

HW and CH on throughout (prelude HW_ON 1 h 7 min before the start). The 72 L
bath has a 40.7 s pause after its first 5 L (at about 28 L/min); it is one
draw because a draw ends only after a minute with no litre.

| Start | Litres | Over | Median flow |
|---|---|---|---|
| 0:02:42.178 | 3 | 25.4 s | 4.7 L/min |
| 0:06:03.288 | 6 | 116.4 s | 4.4 L/min |
| 0:10:52.018 | **72** (bath) | 275.8 s | 18.6 L/min |
| 0:21:22.558 | 1 | | |

The probes are masked by the reheat; the meter is exact.

### bath-87

Overlaps the end of `bath-in-reheat`; the reheat is still on.

| Start | Litres | Over | Median flow |
|---|---|---|---|
| 0:03:22.558 | 1 | | |
| 0:10:01.358 | **87** (bath) | 424.4 s | 12.6 L/min |
| 0:23:51.358 | 1 | | |
| 0:25:12.328 | 1 | | |
| 0:27:14.938 | 12 | 80.3 s | 9.4 L/min |
| 0:31:19.428 | 4 | 28.4 s | 6.2 L/min |

### taps-small

CH on, HW off until HW_ON at 0:54:00. The 1 L at 0:37:49 held the reed
closed for about 5.7 min (creep-like); the one at 0:47:29 for about 1.5 min.

| Start | Litres | Over | Median flow |
|---|---|---|---|
| 0:10:19.268 | 4 | 49.2 s | 3.6 L/min |
| 0:37:49.228 | 1 | | |
| 0:47:29.608 | 1 | | |
| 0:49:15.838 | 2 | 12.6 s | 4.7 L/min |
| 0:59:07.538 | 1 | | |

Small draws: the probe heat balance reads 3–5× too high here.

### creep-evening

HW_OFF at 0:12:01 ends a reheat; CH_OFF at 2:12:02. Lone litres about every
14 minutes early on, then taps. `last_closed_ms` on each litre's open
message tells creep from flow:

| Start | Litres | Reed closed (`last_closed_ms` at the open) |
|---|---|---|
| 0:10:25.848 | 1 | 23.2 min (creep) |
| 0:45:18.378 | 1 | 11.0 min (creep) |
| 1:00:48.378 | 1 | 2.6 min (creep) |
| 1:14:43.568 | 1 | 2.6 s |
| 1:29:19.508 | 4 (tap, 4.4 L/min) | last litre 13 s |
| 1:33:05.988 | 1 | 45 s |
| 1:35:59.378 | 5 (tap, 4.2 L/min) | last litre 38 s |
| 2:13:32.378 | 12 (tap, 3.9 L/min, 170.6 s) | 2.9–3.7 s |
| 2:20:19.858 | 1 | 53 s |
| 2:24:37.248 | 1 | 3.3 s |

Expect `continuous_flow_started` / `_ended` from the first lone litres, and
no draw or occupancy from them.

### quiet-night

From about an hour after the last evening litre to the morning HW_ON. No
litres at all (the meter stays at count 239). It includes a boiler-sensor
reconnect blip at 2:25:58: a last-will `SHUTDOWN` on
`energy/boiler/sensor/system` (its timestamp is the sensor's boot time, 19
days earlier) then `RECONNECTED` 4 s later, so the retained system message
is briefly not a heartbeat. The right tank's overnight loss is in here too.
Expect no draws, and system `low_hot_water` false.

### reheat-morning

CH_ON and HW_ON at 0:10:00, CH_OFF at 0:55:02, HW_OFF at 1:10:02. One small
draw near the end: 3 L from 1:04:25.878 over 64.0 s (a 1 L, a 40 s pause,
then 2 L at 5.0 L/min). The meter run predates the start (`counting_since` about 8 h
earlier), and the recording joins it mid-run.

### ch-only-right-reheat

From the older watch48b recording (topics under `energy/hot_water/`, no
meter, no boiler-room climate). A two-minute CH blip at 0:07:01–0:09:00,
then the CH-only spell 0:16:58–0:36:54 with HW off. The right tank warms
from the spell: its bottom three probes rise about 7–9 °C, more than half of
it after CH_OFF as the coil's heat spreads, and its top two about 2–4 °C.
The left tank's middle and top probes don't rise. Ends before the
next HW_ON. Expect CH-only heating attributed to the right tank.

### ch-only-no-heat

CH_ON at 0:10:12 to HW_ON at 2:03:00, HW off throughout; neither tank
warms (the probes are flat or falling apart from the draws). Shares its
second hour with `taps-small`:

| Start | Litres | Over | Median flow |
|---|---|---|---|
| 1:19:19.268 | 4 | 49.2 s | 3.6 L/min |
| 1:46:49.228 | 1 | | |
| 1:56:29.608 | 1 | | |
| 1:58:15.838 | 2 | 12.6 s | 4.7 L/min |

Expect no heating claimed for either tank.
