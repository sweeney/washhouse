# washhouse MQTT wire contract

This document is normative. The golden files of `sweeney/common`'s
`wire/washhouse` package (`wire/washhouse/testdata/`) pin these bytes
exactly, and a diff there is a breaking change; every example here must
equal one of those goldens (as pinned in `go.mod`) and decode and re-encode
to itself through that package (`wiredoc_test.go`). New fields are additive. Removing or renaming a field
is a major change, announced here first (Changes, below), and bumps the
`schema` version.

**Status: v1, frozen 2026-11-23** (Changes, below). No running service
publishes it yet; washhouse's service shell is phase 5.

washhouse reads tanktemp's probe states and water meter
(tanktemp's `docs/WIRE.md`, normative for those) and the boiler programmer's
HW/CH, and publishes what they mean: hot water available, each cylinder's
thermal state, and every draw.

## Conventions

- Every payload is one line of UTF-8 JSON, an object with a **fixed key
  order** (the examples here are pretty-printed).
- **Times** are UTC, RFC 3339, **to the millisecond, always three digits**
  (`2001-01-01T00:17:46.608Z`). Draw times come from the meter's
  `last_pulse_at`, which is to the millisecond, and every other time uses
  the same form so a consumer needs one parser. Go's `time.RFC3339` parses
  it.
- **An unknown value is `null`, never `0`.** Every key is always present,
  with two exceptions, both in `draw_balanced` (below): `share` and `litres`
  per tank, which are **absent** when the draw's split can't be measured.
  A key added since v1 was frozen (the state's `partial`, the tank's
  `fault`) is always sent, but a decoder takes it as `null` when it is
  absent, so a payload from an older washhouse still decodes.
- **Numbers** are rounded to at most 3 decimals, and printed without
  trailing zeros (`62`, `0.893`). Litres of a draw are whole (the meter
  counts litres). Counts are integers.
- **`timestamp` is event time**, never washhouse's wall clock: the time of
  the latest input washhouse had applied when it published (a tanktemp
  sweep's, a meter message's, the programmer's switch time). The status
  topic is the exception: it describes the daemon, so it carries the wall
  clock. washhouse runs on a host with a real-time clock, and skips
  tanktemp sweeps whose clock wasn't synced. One case moves event time
  without an input: after a silence (2 min in the plan's config) with no
  dated input from tanktemp (a sweep or meter message with a synced
  clock; the room and the programmer don't count, since they end no
  draw), washhouse moves it on by its own clock, to the latest input's time plus the time
  since that input arrived, so a running draw still ends and the tanks
  still go stale; `timestamp` is then that time. An input dated more than
  90 s (in the plan's config) past washhouse's clock is refused, since
  event time never goes back.
- **`schema`** names the payload and its major version:
  `net.swee.washhouse.{state,tank,event,status}.v1`. It is the first key.
- Enumerations are lower case, except the status `event`, which matches
  tanktemp's and boiler-sensor's (`STARTUP`…). **Closed**, so a value
  outside them is a broken publisher and a decode error: a tank `state`
  (and an event's `prev`), `heat_source`, `volume_kind`, and the status
  `event`. **Open**, so a consumer accepts one it doesn't know: the event
  type (`event` on `{base}/events`, which it ignores), a draw's `kind`
  (which it treats as unknown), `reasons`, and a tank fault's `reason`. **Adding a value to a closed vocabulary is a breaking change** (a v2
  `schema`): a v1 decoder refuses the whole message, so it is announced
  here first, like a removal.
- A `device_id` matches `[a-z0-9_-]+`. Keys are exact: a key that differs
  from a known one only in case is a decode error (Go's `encoding/json`
  would otherwise let it override the known one).

## Topics

`{base}` is configured. The default is `water/hot`; test deployments use
`test/water/hot`. washhouse never publishes inside `{base}/tanktemp/`, which
is tanktemp's (statehouse's tanktemp adapter would parse it).

| Topic | Payload | When | QoS | Retained |
|---|---|---|---|---|
| `{base}/state` | State | when anything in it changes, other than `timestamp`; once per tanktemp sweep, when every tank's state for it is in | 0 | yes |
| `{base}/tank/{device_id}` | Tank | when anything in that tank's message changes, other than `timestamp` | 0 | yes |
| `{base}/events` | Event | as each is decided | 1 | no |
| `{base}/status` | Status | STARTUP, HEARTBEAT, SHUTDOWN; the last will | 1 | yes |

`device_id` is the cylinder's key in `devices_<site>` (`[a-z0-9_-]+`), the
same as tanktemp's. tanktemp sends each sweep as separate messages with one
timestamp (the meter's reading, then each tank's state); State waits until
every tank that isn't stale has sent its state for that sweep, so it goes
out once per sweep, complete. If a tank's state for a sweep never comes (a
lost message, a tank whose clock isn't synced, a sweep cut short at
shutdown), State is held for at most 2 minutes past the sweep's time
(`thermal.stale_after` in the behaviour document), so size a staleness
window on State to that, not to the 30 s sweep. tanktemp's status or a
programmer change ends a hold at once. Tank messages go out as each tank's own state
arrives. A metered litre, or any event, publishes State at once (the state
that drops `active_draw` follows `draw_ended` in the same publish), so
during a draw it goes out with every litre too.

**Don't de-duplicate on `timestamp` alone.** Two publishes of State can
still share a `timestamp` with different content: a litre and a sweep in
the same second, or an event mid-sweep. A retained replay is byte-identical
to the message it repeats, and a consumer that needs to spot one compares
the payload.

## State

`{base}/state`: the whole system.

```json
{
  "schema": "net.swee.washhouse.state.v1",
  "timestamp": "2001-01-01T00:17:43.000Z",
  "stored_kwh": 21.79,
  "usable_l40": 357,
  "usable_l40_low": 303.45,
  "low_hot_water": false,
  "partial": null,
  "boiler": {"hw": false, "ch": false},
  "ambient_c": 25.7,
  "active_draw": {
    "draw_id": "20001231T235443Z-76",
    "started_at": "2001-01-01T00:10:10.308Z",
    "last_litre_at": "2001-01-01T00:17:40.112Z",
    "litres": 48,
    "flow_l_min": 6.3,
    "kind": "shower"
  },
  "meter": {
    "timestamp": "2001-01-01T00:17:43.000Z",
    "counting_since": "2000-12-31T23:54:43.000Z",
    "count": 54,
    "litres_since_start": 54,
    "continuous_flow": null
  },
  "tanktemp": {"timestamp": "2001-01-01T00:09:43.000Z", "online": true, "version": "4aba4d5"},
  "tanks": [
    {"device_id": "hot_water_cylinder_left", "state": "drawing", "heat_source": null,
     "stored_kwh": 11.05, "usable_l40": 205.8, "top_c": 63.5},
    {"device_id": "hot_water_cylinder_right", "state": "drawing", "heat_source": null,
     "stored_kwh": 10.74, "usable_l40": 151.2, "top_c": 61.9}
  ]
}
```

With one cylinder blind (its probes unreadable, or no sweep from it), the
totals are `null` and `partial` gives what the others hold:

```json
{
  "schema": "net.swee.washhouse.state.v1",
  "timestamp": "2001-01-01T00:30:47.000Z",
  "stored_kwh": null,
  "usable_l40": null,
  "usable_l40_low": null,
  "low_hot_water": false,
  "partial": {
    "missing": ["hot_water_cylinder_right"],
    "stored_kwh": 11.275,
    "usable_l40": 307.311,
    "usable_l40_low": 261.214
  },
  "boiler": {"hw": false, "ch": true},
  "ambient_c": 19.4,
  "active_draw": null,
  "meter": {
    "timestamp": "2001-01-01T00:30:47.000Z",
    "counting_since": "2000-12-31T23:54:43.000Z",
    "count": 54,
    "litres_since_start": 54,
    "continuous_flow": null
  },
  "tanktemp": {"timestamp": "2001-01-01T00:09:43.000Z", "online": true, "version": "4aba4d5"},
  "tanks": [
    {"device_id": "hot_water_cylinder_left", "state": "cooling", "heat_source": null,
     "stored_kwh": 11.275, "usable_l40": 307.311, "top_c": 63.875},
    {"device_id": "hot_water_cylinder_right", "state": "unknown", "heat_source": null,
     "stored_kwh": null, "usable_l40": null, "top_c": null}
  ]
}
```

| Field | Meaning |
|---|---|
| `stored_kwh` | heat stored above the cold-feed temperature, the tanks summed. `null` unless every tank's is known |
| `usable_l40` | litres of 40 °C water the taps could get now, mixing with cold: the tanks' own `usable_l40` summed. The cylinders feed one outlet in parallel, and the split between them moves with their temperatures (a hot tank supplies the tap while the other is cold), so no fixed split limits it. An upper bound. `null` unless every tank is known |
| `usable_l40_low` | `usable_l40` × a configured factor (0.85) for mixing in real tanks: the figure to plan on |
| `low_hot_water` | `true` when `usable_l40_low` is below a configured level (`low_hot_water_l` in the behaviour document), with hysteresis: it clears only above that level plus a margin (10 L), and holds in between. Judged for the system, never one tank. `null` while `usable_l40_low` is unknown, with one exception: with a cylinder blind, it is `false` when the others' `partial.usable_l40_low` alone is above the level plus the margin (the blind one can only add to it), and the held value clears. Otherwise it is `null`, even if it was `true` before the cylinder went blind: it is never `true` from a partial figure. The held value is kept, so when the total is known again it still holds inside the margin |
| `partial` | while one or more cylinders are blind (each with a `fault` and its own `stored_kwh` `null`) and at least one is known: `missing`, the blind ones' `device_id`s in config order, and `stored_kwh`, `usable_l40` and `usable_l40_low` summed over the others. **Lower bounds**: the house has at least this much, and the blind cylinders an unknown amount more. `null` when every cylinder is known (the totals are then set), when none is, and at a start before every cylinder has been heard from. When it is set, the three totals are `null`. Never mistake it for the total |
| `boiler` | the programmer's `hw` and `ch`: `true` on, `false` off, `null` unknown. These are the programmer's switches only, not the stats or valves: `hw: true` means the HW timer is on, not that a tank is heating. Unknown before boiler-sensor's first heartbeat or event (its retained heartbeat gives them at once when washhouse starts), after its SHUTDOWN, and after 2.5 heartbeat intervals with nothing heard |
| `ambient_c` | the boiler room's temperature, `null` once its last reading is older than a configured limit. Read from statehouse's retained device message for the sensor, dated by statehouse's `last_seen` for the sensor (when statehouse received its last report) (`null` while statehouse calls it stale or offline), so it is there at once after a restart |
| `active_draw` | the running draw, or `null` (Draws, below). It is also the draw the meter has just ended whose `draw_ended` isn't out yet, so `active_draw` goes to `null` in the same publish that follows `draw_ended`, never before. If the next draw starts first (a tap turned on again about a minute after the last litre), `active_draw` goes straight to the next draw, never `null`, and the last one's `draw_ended` comes in the same batch, just after the next one's `draw_started`: key on `draw_id`. Across a washhouse restart, a draw can leave `active_draw` with no `draw_ended` (Delivery). A consumer that starts mid-draw (events aren't retained) recovers it from here, with the same `draw_id` and `started_at` as its `draw_started` |
| `meter` | the hot-water meter as washhouse last heard it. `counting_since` and `count` are tanktemp's (its current counting run), `null` before the first meter message; `timestamp` is the latest message's whose clock was synced, `null` before one (an unsynced message still counts, but its time can't be trusted). `litres_since_start` is metered since **washhouse** started, across tanktemp's runs, so it resets when washhouse restarts. `continuous_flow` is the running creep (Continuous flow, below), or `null` |
| `tanktemp` | tanktemp's latest status: its `timestamp`, `online` (`false` after its SHUTDOWN or last will) and `version`. `timestamp` and `version` are `null` before a status, and after the last will (which has neither). `online` is `null` before a status |
| `tanks` | a summary of each cylinder, in config order, from its tank message (below): `device_id`, `state`, `heat_source`, `stored_kwh`, `usable_l40` (that tank's; the system's is these summed) and `top_c` |

## Tank

`{base}/tank/{device_id}`: one cylinder.

```json
{
  "schema": "net.swee.washhouse.tank.v1",
  "device_id": "hot_water_cylinder_right",
  "timestamp": "2001-01-01T00:36:32.000Z",
  "sweep_at": "2001-01-01T00:36:32.000Z",
  "state": "cooling",
  "heat_source": null,
  "stored_kwh": 8.315,
  "mean_c": 47.906,
  "top_c": 61.375,
  "hot_l": 140.214,
  "usable_l40": 184.331,
  "usable_l40_low": 156.681,
  "power_kw": -1.33,
  "ua_w_per_k": 17,
  "probes_ok": 5,
  "probes_total": 5,
  "fault": null
}
```

A blind cylinder (Faults, below):

```json
{
  "schema": "net.swee.washhouse.tank.v1",
  "device_id": "hot_water_cylinder_right",
  "timestamp": "2001-01-01T00:35:17.000Z",
  "sweep_at": "2001-01-01T00:35:17.000Z",
  "state": "unknown",
  "heat_source": null,
  "stored_kwh": null,
  "mean_c": null,
  "top_c": null,
  "hot_l": null,
  "usable_l40": null,
  "usable_l40_low": null,
  "power_kw": null,
  "ua_w_per_k": 17,
  "probes_ok": 0,
  "probes_total": 5,
  "fault": {"since": "2001-01-01T00:30:17.000Z", "reason": "probes", "errors": {"not_present": 5},
            "lost_at": "2001-01-01T00:35:17.000Z"}
}
```

| Field | Meaning |
|---|---|
| `timestamp` | as everywhere: the latest input's time when this was published. It can be later than `sweep_at` (a draw starting, from the meter, changes `state`) |
| `sweep_at` | the tanktemp sweep the estimate comes from, `null` before the first. **Judge the estimate's freshness on this**, not on `timestamp` |
| `state` | `heating`, `drawing`, `cooling` or `unknown` (below) |
| `heat_source` | while `heating`: `hw` or `ch`, which programmer channel is on that the gain is put down to. Inferred from the gain observed with the channel on, never assumed from the channel alone (a CH-only call heated the right tank while its coil valve was dead, and heats neither since). `null` when not heating, or when the source can't be told |
| `stored_kwh`, `mean_c`, `top_c`, `hot_l`, `usable_l40`, `usable_l40_low` | the estimate from the probe profile: heat stored above the cold feed, the mean and top temperature, litres at 40 °C or above, and this tank's usable litres of 40 °C mix (the drawable water runs to the top of the dome: the outlet has an internal pipe up to it). The hot-outlet probe reads low at rest, heat-sunk by its pipe, and true only while water flows, by a fraction of the water's height above the cold feed (about 0.16). The profile carries it that much higher: a configured prior per tank until washhouse learns its own, then the median of the last five learned, each from a draw that follows an hour's rest and that the tank gave two litres or more of itself (its reading just before the draw against its peak during it). None while water flows; after a draw it ramps in over the hour as the pipe cools. A reload keeps what was learned; a restart starts from the prior. `null` before a sweep, when its sweep can't be estimated (fewer than two good probes), and once it is older than a configured limit (2 min) |
| `power_kw` | the smoothed rate of change of stored heat: positive heating, negative losing. `null` while unknown: at start until enough sweeps have come, and during a draw until about 17 min after the meter ends it (the inlet plume and the probes settle for 12 min, then the power needs fresh sweeps). The first value after a big draw can still carry some of that settling (−1.33 kW, 18 min after a 50 L shower, on a tank losing about 0.4 kW), so treat a value soon after a draw as indicative; the settle time is provisional |
| `ua_w_per_k` | the tank's heat loss per kelvin against the boiler room, fitted online over quiet stretches; the configured prior until the fit has evidence. It is measured against the room, but most of the right tank's loss is its secondary loop shedding heat into the house's pipework, so it reads well above a bare cylinder's |
| `probes_ok`, `probes_total` | probes the latest sweep's profile used, of those tanktemp reported. A suspect 85 °C reading is not used unless the config trusts suspects. `null` before a sweep |
| `fault` | why the cylinder is or was blind (Faults, below): `since`, when it stopped being estimated; `reason`; `errors`, tanktemp's error codes in the latest sweep, counted (`{}` when there are none); and `lost_at`, when it was declared lost (`tank_lost`), `null` until then. Set while the cylinder is blind, and while it is lost but not yet restored (its figures are back then, but `tank_restored` hasn't come), so it clears exactly at `tank_restored`. A blind spell too short to be lost clears at the first good sweep. `null` otherwise. Retained, unlike the events: a consumer that starts mid-fault reads from `lost_at` whether the cylinder was declared lost |

**Faults.** A cylinder is blind while its estimate is `null`, for one of
these `reason`s (an open vocabulary):

- `probes`: its latest sweep came, but with fewer than two usable probes.
  `since` is the first such sweep. All five probes going to `bus_low`, then
  `not_present` a few minutes later, is one whole bus failing at once (a
  connection or wiring fault), not five probes.
- `stale`: no sweep from it for a configured time (2 min). `since` is when
  its last sweep went stale (or when washhouse started, if it never swept).
- `tanktemp_offline`: as `stale`, while tanktemp's status says it is
  offline (its SHUTDOWN or last will).

A blind cylinder's `state` is `unknown` from the moment it is blind, not
only once its data is stale. If it was heating, that ends its heating
spell (`heating_ended`, with `state` `unknown`: the data was lost, not the
heating seen to stop). Once it has been blind for a configured time (5 min:
`tank_fault_after`) washhouse sends `tank_lost`; once it has been estimated
again for another (2 min: `tank_restore_after`), `tank_restored` (Tank
events), and its `fault` clears then. A blind spell shorter than the
first sends neither (its `fault` clears at the first good sweep, with
`lost_at` never set), and a cylinder that is lost stays lost, `fault` and
all, until it has been good for the whole of the second, so a flickering
bus sends one pair, not many.

**States.** `drawing` from a metered draw's first litre until a configured
time (60 s) after the meter ends it, so about two minutes after the last
litre: the probes lag the meter. Both tanks are `drawing` during every draw,
since both feed the merged outlet. A tank's `drawing` can outlast the
system's `active_draw`, which leaves with `draw_ended`.

`heating` or `cooling` is the last judgement of the power: `heating` once
the stored heat rises faster than a threshold, until it falls below a lower
one (hysteresis), else `cooling`. The judgement holds through a draw and the
power's settling after it, so after a draw the state goes back to what it
was with `power_kw` `null` for a while: `cooling` or `heating` with no power
is normal. One exception: in that while, a tank that starts heating (the
hot-water timer on, and its stored heat rising fast since the draw:
1.5 kW, configured) goes to `heating` then, about 6 minutes after the draw, rather
than when the power is known again, about 17. `unknown` before the tank's power is first judged, and while its
data is stale (then until it is judged again).

## Events

`{base}/events`, not retained, QoS 1. Every event has `schema`, `event` and
`timestamp`, then exactly one body, named by its shape:

| `event` | Body key |
|---|---|
| `draw_started`, `draw_updated` | `active_draw` |
| `draw_ended` | `draw_ended` |
| `draw_balanced` | `draw_balanced` |
| `continuous_flow_started`, `continuous_flow_ended` | `continuous_flow` |
| `heating_started`, `heating_ended`, `tank_state_changed` | `tank` |
| `tank_lost` | `tank_lost` |
| `tank_restored` | `tank_restored` |

A consumer must ignore an `event` it doesn't know (new ones are additive).

`timestamp` is the event's own time, in event time, and events go out in
the order they are decided. Two are dated before they are decided, so one
can be dated before one already sent: `draw_started`, at the draw's first
litre, which the meter can only confirm when the next litre or a sweep
comes; and `continuous_flow_started`, at the second creep litre, which is
creep only once 60 s pass with no litre after it. Events dated at a deadline
rather than an input (`continuous_flow_ended`, at the last creep litre + 45
min) carry the deadline.

### Draws

A draw is metered hot water: litres less than 60 s apart, counted by the
meter on the merged hot outlet. The probes never make or size a draw; they
only attribute it, afterwards. So every draw is real water (no confidence
field), and its volume is good to about ±1 L (a part-litre at each end).

```json
{
  "schema": "net.swee.washhouse.event.v1",
  "event": "draw_started",
  "timestamp": "2001-01-01T00:10:10.308Z",
  "active_draw": {
    "draw_id": "20001231T235443Z-76",
    "started_at": "2001-01-01T00:10:10.308Z",
    "last_litre_at": "2001-01-01T00:10:10.308Z",
    "litres": 1,
    "flow_l_min": null,
    "kind": "tap"
  }
}
```

**`active_draw`** (in `draw_started`, `draw_updated` and State):

| Field | Meaning |
|---|---|
| `draw_id` | the meter's `counting_since` (`YYYYMMDDTHHMMSSZ`) and `seq` of the draw's first litre, so never reused, and the same across every event of the draw |
| `started_at` | the first litre's closing (the meter's `last_pulse_at`) |
| `last_litre_at` | the latest litre's closing |
| `litres` | litres so far |
| `flow_l_min` | the median gap between litres, as a flow; `null` until there are two |
| `kind` | **provisional**: `tap`, `shower` or `bath`, from the flow and size so far. A shower starts as a tap. The final kind is in `draw_ended` |

`draw_started` is sent at the first litre. `draw_updated` follows at a
litre, at most every 60 s (configured), while the draw runs.

**`draw_ended`** is sent once the meter has ended the draw (60 s with no
litre) and each tank's sweep after it is in, about 60–70 s after the last
litre. It is the meter's account, final; the attribution comes later, in
`draw_balanced`.

```json
{
  "schema": "net.swee.washhouse.event.v1",
  "event": "draw_ended",
  "timestamp": "2001-01-01T00:18:54.000Z",
  "draw_ended": {
    "draw_id": "20001231T235443Z-76",
    "started_at": "2001-01-01T00:10:10.308Z",
    "ended_at": "2001-01-01T00:17:46.608Z",
    "duration_s": 456.3,
    "litres": 50,
    "volume_kind": "metered",
    "flow_l_min": 6.297,
    "kind": "shower",
    "meter_from": {"counting_since": "2000-12-31T23:54:43.000Z", "seq": 76},
    "meter_to": {"counting_since": "2000-12-31T23:54:43.000Z", "seq": 220}
  }
}
```

| Field | Meaning |
|---|---|
| `draw_id`, `started_at` | as in its `draw_started` |
| `ended_at` | the last litre's closing: when the water stopped, to a litre. **Not** the event's `timestamp`, which is a minute or so later. A consumer clearing an activity at the end of the draw should use this |
| `duration_s` | `ended_at` − `started_at` |
| `litres` | the volume, whole litres from the meter's count |
| `volume_kind` | always `metered` |
| `flow_l_min` | the median gap between litres, as a flow; `null` for a one-litre draw |
| `kind` | final. `bath`: 10 L/min or more and 20 L or more. `shower`: 20 L or more, over 3 min or more, at 3 L/min or more. Otherwise `tap` (which includes a slow steady run, a tap left dribbling) |
| `meter_from`, `meter_to` | the meter messages the draw covers: its first litre's and its last's |

**`draw_balanced`** is sent when the attribution is complete: once each
tank's sweep about 8 min after the last litre is in (the probes have
settled), or earlier if the next draw starts. It has its `draw_ended`'s
`draw_id` and an identical `ended_at`.

```json
{
  "schema": "net.swee.washhouse.event.v1",
  "event": "draw_balanced",
  "timestamp": "2001-01-01T00:25:54.000Z",
  "draw_balanced": {
    "draw_id": "20001231T235443Z-76",
    "ended_at": "2001-01-01T00:17:46.608Z",
    "tanks": [
      {"device_id": "hot_water_cylinder_left", "share": 0.107, "litres": 5.338,
       "outlet_peak_c": 65.25, "inlet_drop_c": 20.75},
      {"device_id": "hot_water_cylinder_right", "share": 0.893, "litres": 44.662,
       "outlet_peak_c": 62, "inlet_drop_c": 24.5}
    ],
    "outlet_peak_c": 62.347,
    "inlet_drop_c": 24.5,
    "energy_kwh": 2.924,
    "heat_balance_l": 48.041,
    "heat_balance_reliable": true,
    "reasons": [],
    "masked_by_reheat": false
  }
}
```

| Field | Meaning |
|---|---|
| `tanks` | every cylinder, in config order |
| `tanks[].share`, `tanks[].litres` | the tank's part of the draw: its heat lost over what a litre from its own outlet carries, scaled to the metered litres; the shares sum to 1. **Absent** (both keys) when the split can't be measured: on a draw masked by a reheat or with the programmer's HW unknown (the coil's heat, not the pipework, would set the split), or when a tank's sweeps are missing. `reasons` says which. Below about 10 L shares scatter, and are indicative only |
| `tanks[].outlet_peak_c` | the tank's hot-outlet probe at its highest during the draw: the temperature the tank delivered. That probe is heat-sunk and reads low at rest, so its peak is the true top. `null` if it had no reading |
| `tanks[].inlet_drop_c` | how far the tank's cold-inlet probe fell during the draw. `null` if it had no reading |
| `outlet_peak_c` | the outlet peaks weighted by the shares: what reached the taps before mixing. When the shares are absent because of a reheat or HW unknown, they are weighted by the configured outlet split (the pipework's, about 0.11/0.89) instead, since the split the probes give then measures the coil. A tank with no outlet reading counts with its profile's top. `null` when a tank's sweeps are missing, or (unmasked) no heat was lost (`no_loss`) |
| `inlet_drop_c` | the largest of the tanks' inlet drops |
| `energy_kwh` | the heat the tanks lost from just before the draw to the settled sweep. During a reheat it can be small or negative |
| `heat_balance_l` | the probes' estimate of the volume from that heat at `outlet_peak_c`, a **cross-check** of `litres`, never the volume. `0`, never negative, when the tanks gained heat overall; `null` when it can't be worked out |
| `heat_balance_reliable` | `false` when any reason in `reasons` applies |
| `reasons` | why the cross-check (and, for the masking ones, the shares) can't be trusted; `[]` when it can. Masking: `hw_on` (the HW timer was on), `tank_gained` (a tank's stored heat rose across the draw), `hw_unknown` (the programmer's HW was unknown at some point). Others: `small` (under 20 L), `thermocline_gap` (the cold layer sat between two widely spaced probes, so it was barely seen), `gap_unread` (one of those probes had no reading), `interrupted` (the next draw started before the settled sweep), `shares_early` (the shares came from the sweep about a minute after the draw, the settled one being cut short), `no_loss` (no heat was lost to balance), `tank_missing` (a tank's sweeps were missing). A consumer must accept a reason it doesn't know |
| `masked_by_reheat` | `true` when `hw_on` or `tank_gained` applies: a reheat was seen |

### Continuous flow

Creep: a litre whose reed stayed closed 10 s or more (configured), with no
other litre within 60 s of it. In practice creep litres hold the reed for
minutes and come minutes apart, about 0.07 L/min. Two creep litres within
45 min start a continuous flow. It is never a draw and never evidence of a person;
it may be a tap not fully closed, or water circulating through the meter.

```json
{
  "schema": "net.swee.washhouse.event.v1",
  "event": "continuous_flow_started",
  "timestamp": "2001-01-01T00:14:11.903Z",
  "continuous_flow": {
    "since": "2001-01-01T00:01:05.217Z",
    "last": "2001-01-01T00:14:11.903Z",
    "litres": 2,
    "flow_l_min": 0.076
  }
}
```

| Field | Meaning |
|---|---|
| `since` | the first creep litre's closing |
| `last` | the latest creep litre's closing |
| `litres` | creep litres so far |
| `flow_l_min` | the median gap between creep litres, as a flow |

`continuous_flow_started` is dated at `last` (see Events). `continuous_flow_ended`
carries the flow's final shape, dated `last` + 45 min, once 45 min pass with
no creep litre (a draw in between doesn't end it). The running flow is in State's
`meter.continuous_flow`.

### Tank events

```json
{
  "schema": "net.swee.washhouse.event.v1",
  "event": "heating_started",
  "timestamp": "2001-01-01T06:42:10.000Z",
  "tank": {
    "device_id": "hot_water_cylinder_left",
    "state": "heating",
    "prev": "cooling",
    "heat_source": "hw",
    "power_kw": 9.84
  }
}
```

| Field | Meaning |
|---|---|
| `state`, `prev` | the tank's state after and before the change |
| `heat_source` | the new heat source; for `heating_ended`, the heating spell's last. `null` when none or not known |
| `power_kw` | the power when the change was decided; `null` when unknown |

Every change of a tank's `state` or `heat_source` is a `tank_state_changed`.
`heating_started` and `heating_ended` are sent as well, after it, when the
tank's heating decision changes. A draw during a heating spell makes the
state `drawing` without ending the spell: the decision holds through the
draw (its loss swamps the power) and is judged again after it, so a
`heating_ended` comes only when the heating is judged over, or the tank
goes blind (its `state` then `unknown`).

A cylinder lost (Faults, above), `tank_fault_after` into its blind spell:

```json
{
  "schema": "net.swee.washhouse.event.v1",
  "event": "tank_lost",
  "timestamp": "2001-01-01T00:35:17.000Z",
  "tank_lost": {
    "device_id": "hot_water_cylinder_right",
    "reason": "probes",
    "errors": {"not_present": 5},
    "since": "2001-01-01T00:30:17.000Z"
  }
}
```

and restored, `tank_restore_after` after its first estimated sweep:

```json
{
  "schema": "net.swee.washhouse.event.v1",
  "event": "tank_restored",
  "timestamp": "2001-01-02T22:55:23.000Z",
  "tank_restored": {
    "device_id": "hot_water_cylinder_right",
    "lost_since": "2001-01-01T00:30:17.000Z",
    "duration_s": 166986
  }
}
```

| Field | Meaning |
|---|---|
| `reason`, `errors`, `since` | as the tank's `fault` when it was lost |
| `lost_since` | the blind spell's `since` |
| `duration_s` | from `lost_since` to the first estimated sweep that began the recovery: how long the cylinder was blind |

## Status

`{base}/status`, retained, QoS 1: the daemon, in tanktemp's shape.

```json
{
  "schema": "net.swee.washhouse.status.v1",
  "online": true,
  "event": "HEARTBEAT",
  "timestamp": "2001-01-01T00:15:00.000Z",
  "version": "e34968b",
  "ready": true,
  "uptime_s": 900,
  "tanks": ["hot_water_cylinder_left", "hot_water_cylinder_right"]
}
```

| Field | Meaning |
|---|---|
| `online` | `true`; `false` on SHUTDOWN and in the last will |
| `event` | `STARTUP`, `HEARTBEAT`, `SHUTDOWN` or `LWT` |
| `timestamp` | washhouse's wall clock (the only wall-clock time on the wire) |
| `version` | the commit washhouse was built from |
| `ready` | `true` once washhouse has its config and is subscribed to its inputs. It never goes back to `false` while washhouse runs (a reconnect resubscribes, and a failed config reload keeps the last good config); a new run starts `false` at STARTUP |
| `uptime_s` | seconds since start |
| `tanks` | the configured cylinders, in config order |
| `reason` | SHUTDOWN only, after `tanks`: the signal (`SIGTERM`, `SIGINT`) or `context` |

The **last will** is exactly `{"online":false,"event":"LWT"}`, retained,
QoS 1, with no `schema` or timestamp: a will is fixed when washhouse
connects. A consumer sees `online: false` whether washhouse stopped cleanly
or vanished.

## Delivery

- **Nothing is queued while the broker is unreachable.** State and tank
  messages published then are lost; the next change replaces them. Events
  decided then are lost too (QoS 1 covers a flaky link, not a down broker),
  and State's `active_draw` is how a consumer recovers a running draw.
- **Events aren't retained.** A consumer that restarts misses the events
  sent while it was down; State and the tank messages give it the present.
- **washhouse restarting** takes the retained meter message as its
  baseline, so litres counted while it was down are never a draw. A draw
  running across the restart is cut: the part before it never gets a
  `draw_ended`, and the litres after it are a new draw with its own
  `draw_id`. A consumer shouldn't leave such a draw to an expiry, which
  would date its end too late. Reconcile from State: when `active_draw` no
  longer names a draw that has had no `draw_ended`, or on washhouse's
  STARTUP or last will, end it at its last known `last_litre_at`. This
  relies on `active_draw` leaving, or changing to the next draw, only in
  the publish after `draw_ended`.
- **The retained messages outlive washhouse.** While its status is
  `online: false`, the retained state and tank messages are the last it
  published, not the present (`active_draw`, `low_hot_water` and the tank
  states included). Gate on status, as for tanktemp.
- **A behaviour reload is a restart** of everything below, though the
  process and its status carry on: when washhouse applies a changed
  behaviour document (on SIGHUP), its engine starts again, so a draw
  running then is cut as above, and the next state comes from the new
  engine. It is given the latest input on each topic, as a restart's
  retained messages would be, so the programmer, the room, tanktemp's
  status, each tank's estimate and `low_hot_water` (judged afresh,
  without its earlier hysteresis) carry on; what is learnt over time
  resets: each tank's `state` is `unknown` with `power_kw` `null` until
  its power is judged again, `ua_w_per_k` goes back to the configured
  prior, and `litres_since_start` starts at 0. Reconcile a cut draw the
  same way, from `active_draw`. `/healthz` shows when the running
  document was applied (`behaviour_applied_at`).
- **What a restart resets**, since it is all in memory: `ua_w_per_k` goes
  back to the configured prior until the fit has evidence again, so a step
  in it after a restart isn't a change in the tank; each tank is `unknown`
  until its power is first judged (at least 5 min of sweeps);
  `low_hot_water` is `null` until the first full estimate, then judged
  without its earlier hysteresis; `litres_since_start` starts at 0.

## Changes

- **2026-11-28, v1, a change of source (not of shape):** `ambient_c` comes
  from statehouse's retained device message for the boiler-room sensor,
  dated by statehouse's `last_seen` for it, rather than zigbee2mqtt's own
  message, which isn't retained (after a restart `ambient_c` was `null`
  until the sensor next reported). sweeney/washhouse#19.

- **2026-11-28, v1, additive:** a blind cylinder. The state's `partial`
  (the known cylinders' totals, a lower bound, while one is blind) and the
  tank's `fault` (why it is blind, since when), both `null` otherwise;
  the events `tank_lost` and `tank_restored`. `fault.lost_at` is the
  retained record of `tank_lost` (statehouse's review of
  sweeney/common#18), and `fault` holds until `tank_restored`. `low_hot_water` can be
  `false`, rather than `null`, from a partial figure that clears its
  level (never `true`). A blind cylinder's `state` is `unknown` from its
  first unreadable sweep, not 2 min later. A v1 decoder ignores the new
  keys and events; common/wire v0.2.0 decodes them, and takes a missing
  `partial` or `fault` as `null`. The State example's `usable_l40` is now
  its tanks' summed (357), as since 2026-11-24 (sweeney/washhouse#17).

- **2026-11-24, v1, sooner (not a change of shape):** a reheat that starts
  soon after a draw shows as `heating` about 6 minutes after the draw,
  not 17 (States). `power_kw` stays `null` until then as before.
- **2026-11-24, v1, when State is published (not its payload):** once per
  tanktemp sweep, when every tank's state for it is in, rather than after
  each of the sweep's three messages, two of which carried a half-updated
  state under the sweep's timestamp (the owner noticed them as
  duplicates). Litres and events still publish it at once. The shower
  fixture's States fall from 954 to 437; its events are unchanged.
- **2026-11-24, v1, a change of value (not of shape):** the drawable water
  runs to the top of the dome, not the outlet fitting (the outlet has an
  internal pipe up to it; the owner), and the outlet probe's resting
  under-read is corrected (Tank). Each tank's `top_c`, `stored_kwh`,
  `mean_c`, `hot_l`, `usable_l40` and `usable_l40_low` rise. Draws and
  their attribution are unchanged: it compares energy without the
  correction.
- **2026-11-24, v1, a change of value (not of shape):** the state's
  `usable_l40` and `usable_l40_low` are the tanks' own summed, no longer a
  draw simulated at a fixed outlet split. A 40 L run with one tank at
  62 °C and the other at 40 °C was too hot to hold, where a fixed 0.89
  from the cooler tank would have given 43 °C: the split moves with the
  tanks' temperatures. The figures rise (both tanks hot at the start of
  the shower fixture: 291 L before, 521 L now), and `low_hot_water` sets
  less often. Keys, types and nulls are unchanged.
- **2026-11-23, v1 clarified (not a change to any payload):** a behaviour
  reload restarts the engine without a STARTUP (Delivery), so a consumer
  reconciles a cut draw from `active_draw`, not from status alone.
- **2026-11-23, v1 clarified (not a change to any payload):** `timestamp`
  after a silence with no input, and the refusal of an input dated too far
  ahead (Conventions). Both describe what the service does when
  it runs; the bytes are unchanged.
- **2026-11-23, v1 frozen:** after statehouse's review (no blockers; it reconciles cut draws from State and throttles on its side) and tanktemp's two reviews (12 points, all taken) on sweeney/washhouse#3. The code moved to `sweeney/common`'s `wire` module the same day; the bytes are unchanged.
