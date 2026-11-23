# washhouse MQTT wire contract

This document is normative. The golden files of `sweeney/common`'s
`wire/washhouse` package (`wire/washhouse/testdata/`) pin these bytes
exactly, and a diff there is a breaking change; every example here must
decode and re-encode to itself through that package (`wiredoc_test.go`). New fields are additive. Removing or renaming a field
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
- **Numbers** are rounded to at most 3 decimals, and printed without
  trailing zeros (`62`, `0.893`). Litres of a draw are whole (the meter
  counts litres). Counts are integers.
- **`timestamp` is event time**, never washhouse's wall clock: the time of
  the latest input washhouse had applied when it published (a tanktemp
  sweep's, a meter message's, the programmer's switch time). The status
  topic is the exception: it describes the daemon, so it carries the wall
  clock. washhouse runs on a host with a real-time clock, and skips
  tanktemp sweeps whose clock wasn't synced.
- **`schema`** names the payload and its major version:
  `net.swee.washhouse.{state,tank,event,status}.v1`. It is the first key.
- Enumerations are lower case, except the status `event`, which matches
  tanktemp's and boiler-sensor's (`STARTUP`…). **Closed**, so a value
  outside them is a broken publisher and a decode error: a tank `state`
  (and an event's `prev`), `heat_source`, `volume_kind`, and the status
  `event`. **Open**, so a consumer accepts one it doesn't know: the event
  type (`event` on `{base}/events`, which it ignores), a draw's `kind`
  (which it treats as unknown), and `reasons`.
- A `device_id` matches `[a-z0-9_-]+`. Keys are exact: a key that differs
  from a known one only in case is a decode error (Go's `encoding/json`
  would otherwise let it override the known one).

## Topics

`{base}` is configured. The default is `water/hot`; test deployments use
`test/water/hot`. washhouse never publishes inside `{base}/tanktemp/`, which
is tanktemp's (statehouse's tanktemp adapter would parse it).

| Topic | Payload | When | QoS | Retained |
|---|---|---|---|---|
| `{base}/state` | State | when anything in it changes, other than `timestamp` | 0 | yes |
| `{base}/tank/{device_id}` | Tank | when anything in that tank's message changes, other than `timestamp` | 0 | yes |
| `{base}/events` | Event | as each is decided | 1 | no |
| `{base}/status` | Status | STARTUP, HEARTBEAT, SHUTDOWN; the last will | 1 | yes |

`device_id` is the cylinder's key in `devices_<site>` (`[a-z0-9_-]+`), the
same as tanktemp's. State and tank messages change with nearly every input
(the meter's and tanks' times are in them), so in practice they go out about
every sweep, and with every metered litre during a draw.

**Don't de-duplicate on `timestamp` alone.** Two publishes of one topic can
share a `timestamp` with different content: tanktemp stamps both tanks'
sweeps with the same time, so State goes out once for each. A retained
replay is byte-identical to the message it repeats, and a consumer that
needs to spot one compares the payload.

## State

`{base}/state`: the whole system.

```json
{
  "schema": "net.swee.washhouse.state.v1",
  "timestamp": "2001-01-01T00:17:43.000Z",
  "stored_kwh": 21.79,
  "usable_l40": 263.5,
  "usable_l40_low": 223.975,
  "low_hot_water": false,
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

| Field | Meaning |
|---|---|
| `stored_kwh` | heat stored above the cold-feed temperature, the tanks summed. `null` unless every tank's is known |
| `usable_l40` | litres of 40 °C water the taps could get now, mixing with cold. **Not the tanks summed:** the cylinders feed one merged outlet in parallel, so every draw takes a fixed split from each (configured; about 0.11 left, 0.89 right), and the tap gets the mix. This is a draw simulated at that split, by plug flow, until the mixed outlet falls below 40 °C. One tank taking most of the flow and running cold first leaves little usable however hot the other is. An upper bound. `null` unless every tank is known |
| `usable_l40_low` | `usable_l40` × a configured factor (0.85) for mixing in real tanks: the figure to plan on |
| `low_hot_water` | `true` when `usable_l40_low` is below a configured level (60 L, provisional), with hysteresis: it clears only above that level plus a margin (10 L), and holds in between. Judged for the system, never one tank. `null` while `usable_l40_low` is unknown; when it is known again, the earlier value still holds inside the margin |
| `boiler` | the programmer's `hw` and `ch`: `true` on, `false` off, `null` unknown. These are the programmer's switches only, not the stats or valves: `hw: true` means the HW timer is on, not that a tank is heating. Unknown before boiler-sensor's first heartbeat or event (its retained heartbeat gives them at once when washhouse starts), after its SHUTDOWN, and after 2.5 heartbeat intervals with nothing heard |
| `ambient_c` | the boiler room's temperature, `null` once its last reading is older than a configured limit |
| `active_draw` | the running draw, or `null` (Draws, below). It is also the draw the meter has just ended whose `draw_ended` isn't out yet, so `active_draw` goes to `null` in the same publish that follows `draw_ended`, never before. If the next draw starts first (a tap turned on again about a minute after the last litre), `active_draw` goes straight to the next draw, never `null`, and the last one's `draw_ended` comes in the same batch, just after the next one's `draw_started`: key on `draw_id`. Across a washhouse restart, a draw can leave `active_draw` with no `draw_ended` (Delivery). A consumer that starts mid-draw (events aren't retained) recovers it from here, with the same `draw_id` and `started_at` as its `draw_started` |
| `meter` | the hot-water meter as washhouse last heard it. `counting_since` and `count` are tanktemp's (its current counting run), `null` before the first meter message; `timestamp` is the latest message's whose clock was synced, `null` before one (an unsynced message still counts, but its time can't be trusted). `litres_since_start` is metered since **washhouse** started, across tanktemp's runs, so it resets when washhouse restarts. `continuous_flow` is the running creep (Continuous flow, below), or `null` |
| `tanktemp` | tanktemp's latest status: its `timestamp`, `online` (`false` after its SHUTDOWN or last will) and `version`. `timestamp` and `version` are `null` before a status, and after the last will (which has neither). `online` is `null` before a status |
| `tanks` | a summary of each cylinder, in config order, from its tank message (below): `device_id`, `state`, `heat_source`, `stored_kwh`, `usable_l40` (that tank alone, as if it were the only one: for display, not for the system's water) and `top_c` |

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
  "probes_total": 5
}
```

| Field | Meaning |
|---|---|
| `timestamp` | as everywhere: the latest input's time when this was published. It can be later than `sweep_at` (a draw starting, from the meter, changes `state`) |
| `sweep_at` | the tanktemp sweep the estimate comes from, `null` before the first. **Judge the estimate's freshness on this**, not on `timestamp` |
| `state` | `heating`, `drawing`, `cooling` or `unknown` (below) |
| `heat_source` | while `heating`: `hw` or `ch`, which programmer channel is on that the gain is put down to. Inferred from the gain observed with the channel on, never assumed from the channel alone (a CH-only call heated the right tank while its coil valve was dead, and heats neither since). `null` when not heating, or when the source can't be told |
| `stored_kwh`, `mean_c`, `top_c`, `hot_l`, `usable_l40`, `usable_l40_low` | the estimate from the probe profile: heat stored above the cold feed, the mean and top temperature, litres at 40 °C or above, and this tank's usable litres of 40 °C mix as if it were alone (see State's `usable_l40` for the system). `null` before a sweep, when its sweep can't be estimated (fewer than two good probes), and once it is older than a configured limit (2 min) |
| `power_kw` | the smoothed rate of change of stored heat: positive heating, negative losing. `null` while unknown: at start until enough sweeps have come, and during a draw until about 17 min after the meter ends it (the inlet plume and the probes settle for 12 min, then the power needs fresh sweeps). The first value after a big draw can still carry some of that settling (−1.33 kW, 18 min after a 50 L shower, on a tank losing about 0.4 kW), so treat a value soon after a draw as indicative; the settle time is provisional |
| `ua_w_per_k` | the tank's heat loss per kelvin against the boiler room, fitted online over quiet stretches; the configured prior until the fit has evidence. It is measured against the room, but most of the right tank's loss is its secondary loop shedding heat into the house's pipework, so it reads well above a bare cylinder's |
| `probes_ok`, `probes_total` | probes the latest sweep's profile used, of those tanktemp reported. A suspect 85 °C reading is not used unless the config trusts suspects. `null` before a sweep |

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
is normal. `unknown` before the tank's power is first judged, and while its
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
`heating_ended` comes only when the heating is judged over.

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
- **What a restart resets**, since it is all in memory: `ua_w_per_k` goes
  back to the configured prior until the fit has evidence again, so a step
  in it after a restart isn't a change in the tank; each tank is `unknown`
  until its power is first judged (at least 5 min of sweeps);
  `low_hot_water` is `null` until the first full estimate, then judged
  without its earlier hysteresis; `litres_since_start` starts at 0.

## Changes

- **2026-11-23, v1 frozen:** after statehouse's review (no blockers; it reconciles cut draws from State and throttles on its side) and tanktemp's two reviews (12 points, all taken) on sweeney/washhouse#3. The code moved to `sweeney/common`'s `wire` module the same day; the bytes are unchanged.
