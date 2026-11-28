# washhouse: plan

## Context

tanktemp publishes raw probe temperatures for the two cylinders: 5 DS18B20 probes per tank on `water/hot/tanktemp/{device_id}/state` (formerly `energy/hot_water/…`). boiler-sensor publishes the programmer's HW and CH switch states. Nothing turns these into what a person or statehouse cares about: how much hot water is available, whether the tanks are heating or cooling, and when someone is drawing water. tanktemp's own docs (WIRE.md, PLAN.md) say explicitly that draw detection and litres belong downstream. **washhouse is that downstream service.**

What it does:
- consumes MQTT;
- estimates hot water available per tank and for the whole system;
- classifies each tank's state;
- detects and measures draws;
- publishes its own retained state and draw/heat events;
- serves an authenticated API, `/healthz`, and writes to Influx.

statehouse then consumes washhouse to improve occupancy. A real draw is far better evidence than the HW timer, which counts as occupancy today. The cylinders also become devices in statehouse.

Decided with the user:
- **Stateless + Influx**: state is in memory, and history goes to Influx. No SQLite.
- **Statehouse scope**: draws as activity, cylinders as devices, and fixing HW-timer occupancy.
- **Common extractions**: all four (MQTT subscriber, wire decoders, httpapi auth/serve, remote-config fetcher).
- **Host**: the home server; the real host stays in a gitignored `deploy/host`. Deploy is deferred.
- **Repo**: `sweeney/washhouse`, public, so every committed file is checked for house details. The vendor's meter datasheet and photos are committed under `meter/` (owner's choice).

Domain knowledge comes from the tanktemp session's briefing (2026-11-13, with its §7 correction). It is committed as `docs/HOT-WATER-SYSTEM.md` with its confidence labels (measured / owner said / inferred / guess), **edited, not verbatim**: the house's usual draw times and programme windows are removed (an occupancy pattern), local paths are dropped, and what changed since is marked _Update (Nov 2026)_. Facts that drive the design:
- **Probes sit on fittings.**
  - P1 (265 mm) is on the cold inlet. It is the big-draw detector.
  - P5 (1215 mm) is on the hot outlet. It is heat-sunk, so it under-reads at rest; its peak during a draw is the true top temperature.
  - **Right P4** is on the secondary return. It reads 5–9 °C low, and the left P4 is a normal probe.
- **Left and right tanks behave differently.**
  - Right tank loss was about 15–19 W/K (a gravity secondary loop), against 3–4 W/K for the left. **It isn't constant**: air after a refill on 19 Nov stopped the loop (about 5 W/K), and a large draw on 21 Nov purged it (back to 15–19 W/K). The owner plans to close the new return valve for a few days in late Nov. So UA is fitted online per tank, and must re-converge within a night or two of a step change.
  - The right tank used to heat on CH-only calls (+0.7 to +6.4 kW) because its coil valve was dead; it was replaced on 19 Nov, and CH-only spells since put about 0 kW in. So the heat source is inferred from observed gain, never assumed from CH.
- **HW/CH are only the programmer.** They tell us nothing about the stats or valves.
- **Draws:**
  - Small draws are nearly invisible to a heat balance (now superseded: volumes come from the meter).
  - Draws are masked during a reheat, because the cold inlet sits inside the coil zone. This now affects only the heat-balance cross-check.
  - A recurring small nightly draw was in the briefing; two metered nights showed none.
- **Fitted values (from hwplan):**
  - reheat: HW 12.1 kW, HW+CH 5.3 kW, CH-only 2.5 kW (before 19 Nov; about 0 since);
  - full ≈ 24.6 kWh;
  - usable ≈ 45·(E − 8.4) L.
  - The linear E-loss fit has a negative intercept, so **use per-tank UA·(T_mean − T_room)** instead, with the new `climate_groundfloor_boiler-room` sensor.

## Hot-water meter

The meter design lives in **tanktemp's repo**, moved there 20 Nov 2026:
`docs/WATER-METER-PLAN.md`, `docs/WATER-METER-WALKTHROUGH.md` and
`docs/water-meter-explainer.html`. The plan's §6–7 are washhouse's
requirements. They come back into this repo when washhouse is bootstrapped.

**The meter is live** (tanktemp PRs #9, #11, #12, deployed 21 Nov 2026; `water/hot/tanktemp/meter`, K = 1, one pulse per litre on the merged hot outlet). Two days of recordings with it changed this plan; see **Lessons from tanktemp and the live meter** below.

**Split agreed with the tanktemp session (20 Nov 2026).** tanktemp owns raw facts, and washhouse owns interpretation.

- **tanktemp owns:**
  - the meter docs as files;
  - all meter code: `internal/meter` is done on branch `meter-core`;
  - `gpioedge`, payload, goldens, config, cmd, health, `pulsewatch`, `setup-pi`, and the unit. The owner chose to keep the meter inside tanktemp: `DeviceAllow=/dev/gpiochip0` and `SupplementaryGroups=gpio`, replacing `PrivateDevices`;
  - `WIRE.md`, which is normative for `water/hot/tanktemp/meter`;
  - the rig and its fixtures;
  - the `devices_home` draft.
- **washhouse owns:**
  - the consumer requirements: §6–7 of the meter plan;
  - reviewing tanktemp's `WIRE.md` diff before goldens, and each meter PR;
  - all interpretation: flow and the closed-time duty, draws, kind, circulation, noise attribution, calibration, `meter_suspect`, `continuous_flow`, and the `water/hot/*` outputs;
  - the statehouse washhouse adapter;
  - creating `common/wire` with the `tanktemp` Meter decoder, tested against copies of tanktemp's goldens, which tanktemp reviews.
- **Process:**
  - neither session edits the other's repo;
  - washhouse changes to the shared docs go to tanktemp as patches, with one writer per file;
  - each session asks the owner for its own commit dates;
  - owner decisions go to the owner.

## Lessons from tanktemp and the live meter (21–22 Nov 2026)

These come from reviewing and smoke-testing tanktemp's meter work, and from about 40 hours of live recordings under `~/washhouse-recordings/`: `live-*.log` for hot water, and `full-*.log` for the whole broker over 21–22 Nov. The recordings include two showers, two baths (one during a reheat), dozens of tap draws, an evening of creep, two quiet nights, and four reheats.

1. **The meter is the source of truth for draws.** The probe heat balance was measured against it:
   - **Large draw from a layered tank:** within 8% (46 L estimated against 50 L metered).
   - **Small draws (1–6 L):** 3–5× too high, because P1, on the inlet fitting, dips for any draw.
   - **A draw soon after another:** almost invisible. One 15 L draw showed as 4 L; the cold layer sat in a gap of about 71 L between P2 (400 mm) and P3 (825 mm).
   - **During a reheat:** masked. Yet a 72 L bath during a reheat was metered exactly.
   - **At night:** three draw-like dips (inlet falls, outlet rises) came with zero litres.

   So: draws are segmented from metered litres. The probes only **attribute** them (which tank supplied it, the outlet temperature, energy used and energy left) and give a heat-balance **cross-check**, marked unreliable during a reheat and below 20 L. Volume is whole metered litres, good to ±1 L (partial litres at each end). The `lower_bound` volume type goes.
2. **Flow and kind come from the meter.** Measured hot flows:
   - taps 3.5–5 L/min;
   - showers 4–6.4 L/min, at 34–51 L;
   - baths 12–31 L/min, at 72–87 L.

   Kind follows from flow and size: a bath runs at 10 L/min or more and is 20 L or more; a shower is 20 L or more over 3 min or longer at 3 L/min or more; anything else is a tap. **Open:** a slow steady run (1.2–2 L/min, litres under a minute apart, so a draw, not creep) is a long "tap" today; it may be a tap left dribbling or circulation through the meter, and could fold into `continuous_flow` once real data shows one. A draw ends after a minute with no litre, so litres less than 60 s apart are one draw (a real bath paused 40.7 s mid-fill). Litres are counted from `count`, never from changes in `last_pulse_at` (tanktemp can print one closing 1 ms apart in two messages).
3. **Creep is a separate signal.** `last_closed_ms` tells real flow from creep: real flow holds the reed closed for 1–4 s, creep for minutes. On 21 Nov, lone litres came about every 14 min, closed for 2–23 min each, at about 0.07 L/min (about 100 L/day if left). Two such litres within 45 min raise `continuous_flow_started` / `_ended`. That is never a draw and never occupancy. Cause unknown: a tap not fully closed, or circulation through the meter.
4. **Two tanks need a system view.**
   - The right tank supplies about 75% of every draw: 2.26 of 2.95 kWh in a 50 L shower. Its P1 reacts within 4 s against the left's 24 s, and its P2 falls 20 °C or more.
   - It can also lose heat much faster: on the night of 21→22 Nov about 230–370 W, back to its old 15–19 W/K after two days at about 5 W/K. It fell below 45 °C while the left was at 66 °C.
   - So `low_hot_water` and availability are judged for the **system**, never one tank. Attribution is computed in litres: each tank's heat lost, divided by what a litre from its own outlet carries.
5. **Confirmed from the briefing:**
   - P5 at rest under-reads by 3–8 °C, and its peak during a draw is the true top: about 65 °C on the left, 62 °C on the right.
   - P1 shows a draw within 1–43 s, depending on the sweep phase.
   - Right tank loss is high.
   - Two metered nights showed **no nightly draw**.
6. **Ingest rules for tanktemp's meter wire** (tanktemp's WIRE.md is normative):
   - de-duplicate on `(counting_since, seq)`;
   - each restart starts a new counting run, and litres are summed across runs;
   - a jump in `count` is skipped messages, not lost litres;
   - the last `sweep` at shutdown matches no tank states;
   - a sweep's `timestamp` may move forward to the latest litre it includes;
   - de-dup by timestamp alone is wrong for the meter.

   `meter_audit.py` in `tools/` already does all of this and is the reference. Separately, for boiler-sensor: after a reconnect blip (one on 22 Nov, 4 s, process uptime unchanged), the retained `energy/boiler/sensor/system` can be a `RECONNECTED` event, not a heartbeat. So keep the last known HW/CH from heartbeats and events, and don't assume the retained message is a heartbeat. (`tools/smoke.py` has the same weakness; fix it.)
7. **statehouse's occupancy issue is confirmed live.**
   - The morning programmer timers alone switched the house to "day" and "active".
   - AV gear drawing power reads as activity all day.
   - A real tap-rate draw showed presence while the owner was out.

   That moves statehouse PR 3 (timers aren't people) up the list, and drops the draw confidence gate: a metered draw is real water.
8. **How tanktemp was built, adopted for washhouse:**
   - hardware and outside services behind small interfaces, with a shared contract test run against fakes, so the core is testable on a laptop;
   - everything declared in config, with strict validation (for example, integer-only pins: yaml.v3 silently truncates 4.5 to 4);
   - tests that fail first, with deliberate mutants that must fail them;
   - concurrency tests run `-race -count=200`, which caught a flaky sweep-ordering test that hid a real ordering bug;
   - goldens that pin key order;
   - scripts that enforce rollout order: setup checked a new config with the installed build, since an older build rejects an unknown key.
9. **Fixture privacy:** draw times are an occupancy pattern. tanktemp shifted its first-night fixture to a fake epoch, and washhouse fixtures do the same.

## Topic and coexistence decisions

- **One hot-water tree, `water/hot/`, with one owner per subtree** (decided 20 Nov 2026).
  - tanktemp's prefix moves from `energy/hot_water` to `water/hot/tanktemp`: probes at `…/{device_id}/state`, `…/status`, and the meter at `…/meter`.
    - This is a config-only, clean-cut migration owned by the tanktemp session: a script with `--check`, then clearing the old retained topics.
    - statehouse's `base_topic` moves with it, as a `statehouse_behaviour` remote-config write.
    - live.html, live.sh and record.sh are updated.
  - statehouse re-reads adapter subscriptions only on restart; SIGHUP isn't enough. That applies to the future `adapters.washhouse` too: write the config, then restart statehouse straight away.
  - statehouse already receives `water/hot/tanktemp/#`, so `water/hot/#` needs no broker change for washhouse (checked with the statehouse session; broker details stay out of the repo).
  - Never publish inside `water/hot/tanktemp/`. statehouse's tanktemp adapter subscribes to `+/state` and `status` there, and would parse washhouse's messages.
- **washhouse publishes** (base configurable; `test/water/hot` for testing):
  - `water/hot/state`: retained, QoS 0, published on every input sweep and on a heartbeat.
  - `water/hot/tank/{device_id}`: retained, QoS 0.
  - `water/hot/events`: not retained, QoS 1. Carries `draw_started` (at the first metered litre), `draw_updated`, `draw_ended`, `continuous_flow_started`, `continuous_flow_ended`, `heating_started`, `heating_ended`, `tank_state_changed`.
  - `water/hot/status`: retained, QoS 1. Carries STARTUP, HEARTBEAT and SHUTDOWN, with LWT `{"online":false,"event":"LWT"}`. This is tanktemp's shape.
- **Envelope conventions** follow tanktemp's WIRE.md:
  - `timestamp` is UTC RFC 3339, taken from the **source sweep**, never the wall clock;
  - an unknown value is `null`, never 0;
  - key order is fixed and pinned by goldens;
  - a `schema` field such as `net.swee.washhouse.tank.v1`.
- **Normative contract**: `docs/WIRE.md`. It is reviewed with the statehouse and tanktemp sessions before statehouse's adapter is built.

Draft tank state (field list; finalised in WIRE.md):
- `device_id`, `timestamp`, `state`, `heat_source`, `stored_kwh`, `mean_c`, `top_c`, `power_kw`, `ua_w_per_k`, `probes_ok`, `inputs`, `schema`, `model`;
- `state` is `heating|drawing|cooling|unknown`, and `heat_source` is `hw|ch|null`;
- `hot_l` is the litres at ≥40 °C;
- `usable_l40` is the litres of 40 °C mix (plug-flow upper bound), with `usable_l40_low` at ×0.85;
- `power_kw` is the smoothed dE/dt;
- `inputs` records the freshness of each source.

The system state carries:
- the totals, a summary per tank, `boiler{hw,ch}`, and `ambient_c`;
- `active_draw` (or null), with the same `draw_id` and `started_at` as the events, so a consumer that restarts mid-draw (events aren't retained) recovers the draw;
- a system-level `low_hot_water`;
- `meter{count, counting_since, continuous_flow}`.

Every retained message carries a real publisher `timestamp` (the source sweep's): statehouse treats a retained message without a usable one as an undated replay.

`draw_ended` carries:
- `draw_id`, `started_at`, `ended_at`, `duration_s`. `draw_id` is the first litre's meter message, `counting_since` and `seq` (`meter.DrawID`, e.g. `20010101T070000Z-2`): stable across washhouse restarts, never reused. `started_at` is the first litre's closing, `ended_at` the last litre's;
- `litres` with `volume_kind: "metered"`, `flow_l_min` (the median gap between litres), and `kind` (`tap|shower|bath`);
- `tanks[{device_id, share}]`, typically right ≈ 0.75;
- `masked_by_reheat`, plus `heat_balance_l` and `heat_balance_reliable` as the cross-check;
- `inlet_drop_c`, `outlet_peak_c`, and the meter `seq` range it covers.

There is no `confidence`: every draw is metered water (statehouse fills its own `Confidence` with 1.0). `kind` is final only in `draw_ended`; `draw_started`/`draw_updated` carry a provisional one.

## Architecture (Go, statehouse-shaped, with tanktemp's runtime)

The pure core (no OS, network or MQTT, enforced by a `boundaries_test.go` as in tanktemp) is **driven by event time** from message timestamps, with an injected clock.

| Package | Role |
|---|---|
| `internal/vessel` | Port of `avail.py`'s geometry: spherical-cap domes, slices, and the straight length solved to hold the capacity. Every constant is config. |
| `internal/profile` | Linear interpolation between probe heights, flat beyond the end probes. The `strat` running max from the bottom up is the default. Probe **roles** (inlet, outlet, return, bulk) decide which probes are excluded or treated as heat-sunk. |
| `internal/estimate` | E above T_cold, `hot_l`, mix40, mean, top. The top temperature uses the last P5 draw peak, decayed, rather than P5's resting value. |
| `internal/thermal` | Per-tank power dE/dt from the timestamps; online UA fit against ambient over draw-free stretches; classifier for heating, cooling, drawing and unknown, with heat source inferred from programmer state plus observed gain (e.g. right tank on CH). |
| `internal/meter` | The meter consumer, built on `common/wire/tanktemp`'s Meter decoder (moved there from `internal/wire` in phase 4). Stitches counting runs and de-duplicates on `(counting_since, seq)`. Segments litres into draws: a draw ends 60 s after its last litre, so litres under 60 s apart are one draw. Litres come from `count` deltas only. Computes flow (median interval) and kind. Detects creep: a lone litre with `last_closed_ms` ≥ 10 s is creep only if no litre follows within 60 s (a slow-opening tap can hold a draw's first litre closed for 14–41 s); two creep litres within 45 min give `continuous_flow`. Done in phase 1, with fixture parity against `meter_audit.py` (`testdata/fixtures/draws.json`). Tracks meter health (noise, short, glitches, lost, freshness). |
| `internal/draw` | **Attribution, not detection.** For each metered draw: each tank's share, in litres from its heat lost over its own outlet's peak temperature; the outlet temperature (P5's peak); and energy used. A heat-balance volume, kept as a cross-check and flagged unreliable when masked by a reheat, under 20 L, or when the cold layer sits between P2 and P3. |
| `internal/engine` | Wires ingest, estimate, classify and draw. Holds the in-memory state and a ring buffer of recent draws (since boot). |
| `internal/ingest` | Applies the same freshness rules as statehouse's tanktemp adapter: drop a retained message more than 90 s old or with an unsynced clock, and de-duplicate by timestamp. The meter is the exception: it is de-duplicated on `(counting_since, seq)`, and a retained replay is not news. Also handles boiler events/system: HW/CH come from heartbeats and events, and a retained `RECONNECTED` is not a heartbeat. Ambient comes from `zigbee2mqtt/climate_groundfloor_boiler-room`. |
| `internal/publish`, `internal/influx`, `internal/httpapi` | Outputs. The Influx writer follows `statehouse/internal/influx/writer.go`: one measurement per domain, fields named with their unit (`stored_kwh`, `temperature_c`), a `site` tag on every point (left out when empty), no room tag, point time = event time. The API has statehouse's path-coverage and two-way field-coverage spec tests from the start. |
| `cmd/washhouse` | `common/lifecycle` for status and heartbeats, `common/health`, and `mqttpub` for both publishing and subscribing. |
| `cmd/washreplay` | Replays mosquitto_sub `%I %t %p` recordings through the engine and prints events and states. It is the main tuning and test tool. |

**Config**
- The local YAML holds connection settings only: site, mqtt, identity, remote_config, http, influx. Boot refuses anything else, as statehouse does (remote-only fields are `yaml:"-"` and listed, and a local file setting one is refused).
- Remote namespaces are decoded strictly: unknown keys and duplicate keys (ignoring case) are refused. Boot refuses to start without remote config; a failed SIGHUP reload keeps the last good config.
- Publisher timestamps are sanitised as statehouse's `timeutil` does: unparseable, more than 30 days old or more than 24 h ahead is undated.
- Behaviour comes from config.swee.net:
  - **`devices_home`**: the cylinders and probes. Add a `cylinder` block (capacity_l 200, height_mm 1498, diameter_mm 550); `common/devices` v0.2.0 already decodes it.
  - **A new `washhouse` namespace**: inner diameter, dome depth, z0, T_cold, T_min, T_mix, the drawable band (cold inlet to the top of the dome: 281–1412 mm on the STI 210; the outlet has an internal pipe up to the dome top, the owner said on 24 Nov 2026, replacing an earlier guess of no dip pipe and a band ending at the fitting, 1237 mm), per-tank probe roles by position, whether to trust suspect 85 °C readings (default no), draw thresholds, prior UA per tank, reheat priors, and topics. No geometry is built into the code: the STI 210 values exist only as test data.
  - The user makes the admin writes; I supply the JSON.

**Probe heights** come from tanktemp's own payload (`height_mm`), with the config as fallback.

**API** (stdlib mux; JWT through the extracted common auth; spec in `internal/httpapi/openapi.yaml` with a spec test):
- `GET /healthz`: public, `common/health` shape. Adds `mqtt_connected`, the age of each input, `remote_config`, and `influx`.
- `GET /openapi.json`: public.
- `GET /api/v1/state`, `GET /api/v1/tanks/{id}`
- `GET /api/v1/draws`: recent draws since boot, from memory. Longer history is in Influx and greenhouse.
- `GET /api/v1/model`: geometry, parameters, and the live UA fit.

## Common extractions (in `sweeney/common`; each tried through a gitignored repo-local `go.work`, then tagged and pinned)

1. **`mqttpub` v0.2.0: `Subscribe(filter string, qos byte, h func(Received))`**, with `Received{Topic string; Payload []byte; Retained bool}`. It resubscribes on reconnect and is tested with `mqtttest` against a real broker. This replaces statehouse-style hand-written paho clients.
2. **A new `wire` module**: `wire/tanktemp` (state and status), `wire/boiler` (events and system), and `wire/washhouse` (washhouse's own contract), each with types, decoders and the publisher's goldens. statehouse's adapters and washhouse share them.
3. **`web/apiauth` and `web/serve`**:
   - `apiauth` provides a RequireAuth middleware for user or service tokens that returns 503 + Retry-After on keys-unavailable. It takes a small `Verifier` interface, so common/web takes **no** dependency on identity.
   - `serve` provides the Server with timeouts and graceful shutdown, `WriteJSON`/`WriteError`, and CORS.
4. **`remoteconfig.Fetcher`**: keep-last-good, a `Cold()` boot gate, a SIGHUP reload hook, and a `/healthz` block.

Moving statehouse, greenhouse and countinghouse onto 3 and 4 is a follow-up, not in scope; I'll list it in PLAN.md. Later candidates: the shared status/LWT payloads, and tanktemp's `deploy/lib.sh` (washhouse would be its 3rd copy, which is PLAN's trigger).

## Statehouse changes (separate PRs in the statehouse repo, coordinated with a statehouse session)

1. **A `washhouse` adapter**, using a narrow Engine interface like the intercom adapter's.
   - `draw_started` → `IngestSignal{ID:"washhouse:draw:<draw_id>", Type:"hot_water_draw", Location:"house", Since: started_at, ExpiresAt: engine clock + 30 m safety}` plus `RecordActivity`. `draw_updated` refreshes ExpiresAt. There's no confidence gate: a draw exists only once a litre is metered.
   - On startup, a retained `active_draw` on `water/hot/state` is ingested the same way, so a statehouse restart mid-draw recovers it.
   - `draw_ended` → `ClearSignal(id, ended_at)`, so `quiet_after`/`empty_after` count from the last litre, not from the event 60 s later; plus `UpdateActivity` with EndedAt, the litres and the kind (taken only from `draw_ended`).
   - `continuous_flow_*`: never an occupancy signal. **Design open:** statehouse has no per-device warning mechanism (the nearest, `divergence_warning`, is energy-specific). Options: a `Reading` field on the hot-water device, or an event statehouse relays without storing. To settle with the statehouse session before the adapter.
   - It uses `wire/washhouse`, and `DeclareAvailabilitySource` through the status topic (from PR #94).
2. **Cylinders as devices.**
   - Set `telemetry: true`, scheme `washhouse`, primary = the device_id.
   - A class `hot_water_cylinder` needs Go code in statehouse (`statehouse_classes` can only override existing classes): a constant in `internal/device/profiles.go`, an entry in `IsPassiveSensor` (a passive class missing from it is stuck at `unknown`), and a staleness case (90 s).
   - Add `model.Reading` fields `HotWaterL40 *float64` and `StoredKWh *float64`: update `HasAnyMeasurement`, the canonical-event mapping, the Influx writer, the API DTO, `openapi.yaml` and `documentedSchemas` in `spec_fields_test.go`.
   - Write them to Influx so greenhouse can chart tank level. The probes keep their own rooms (`part_of` never passes room down).
3. **HW-timer occupancy fix (now first).** Give `hot_water` and `central_heating` a new class, such as `schedule_switch`, that keeps the binary-state machine (keyed on `ClassBinaryState` in `statemachine.go`) but is left out of **both** `isOccupancyRelevant` and `activeCount` in `internal/state/house.go`. Leaving out only occupancy would still make the house "active", since `activeCount` drives the activity dimension. This was confirmed live: the morning timers alone flipped the house to "day" and "active". AV standby power reading as activity is a separate statehouse issue, worth raising with that session.
4. **Ordering hazard.** `statehouse_behaviour` is decoded strictly (unknown keys and case-insensitive duplicates refused). On SIGHUP a refused document keeps the running config, but at **boot** statehouse exits, and under `Restart=always` that is a crash loop. So: deploy the statehouse that knows `adapters.washhouse`, then write the namespace, then restart (adapters subscribe only at startup).

## Phases

0. **Bootstrap.**
   - `git init`, create the GitHub repo (confirm first).
   - Add `docs/HOT-WATER-SYSTEM.md` (the briefing), `PLAN.md`, `README.md`, Makefile, CI (`statehouse/.github/workflows/ci.yml` pattern), and `CLAUDE.md`. CLAUDE.md carries the conventions from statehouse and tanktemp:
     - gofmt; TDD with failing tests first, and mutants that must fail them;
     - `-race -count=200` on anything concurrent;
     - an injected clock;
     - no PII, and fixtures shifted to a fake epoch;
     - outside systems behind interfaces with contract tests;
     - config-declared, strictly validated settings;
     - OpenAPI and README in the same change;
     - pin common tags;
     - scripts enforce rollout order.
   - Move `tools/` (the recorder, smoke test and meter audit, with their tests) and `docs/washhouse-explainer.html` into the repo, checking both for house details first. The explainer's simulated day is shifted by 85 min so it shows no real routine; the smoke fixtures are tanktemp's published WIRE.md examples plus one resting snapshot.
   - A Go module with a placeholder `cmd/washhouse` and `boundaries_test.go`, so CI has something to build from the start.
   - **Done 22 Nov 2026** (first commit dated at commit time, as the owner asked).
   - **Ask for commit dates before committing.**
1. **Fixtures, meter decoder, pure core.**
   - `scripts/make-fixture.sh`:
     - keep only `water/hot/tanktemp/#` (and `energy/hot_water/#` in recordings made before the move), `energy/boiler/sensor/events`, the ch/hw/timestamp fields of the system heartbeat, and the boiler-room climate;
     - strip the network block;
     - de-duplicate retained replays;
     - **shift every timestamp by a whole number of seconds to a fake epoch** (as tanktemp's first-night fixture did), keeping every interval;
     - fail if `192.168.` or `ssid` remains, or if any real date survives.
     - It has a `--check` mode and its own test.
   - **Metered slices from `~/washhouse-recordings/` (21–22 Nov)** replace the probe-only slices:
     - a 50 L shower from a layered tank;
     - the 15 L draw straight after it (the thermocline blind spot);
     - the 72 L bath during a reheat and the 87 L bath;
     - tap draws of 1–6 L;
     - the evening creep;
     - a quiet night;
     - the morning reheats;
     - the right tank's loss changing (about 5 W/K on 20 Nov, back to 15–19 W/K on 21→22 Nov).

     Keep one probe-only slice from tanktemp's `watch48b` recording of a CH-only right reheat (8 Nov, 20 min), and one CH-only spell from 20–22 Nov that heats neither tank.
   - The decoders in `internal/wire/tanktemp` and `internal/wire/boiler`, tested against copies of tanktemp's goldens and reviewed by the tanktemp session. They start in washhouse so phase 1 needs no common release, and move to `common/wire` in phase 4 (22 Nov 2026 decision). Then `internal/meter`, whose golden draws must match `tools/meter_audit.py`'s output on the same fixtures.
   - Port vessel, profile and estimate, with **parity tests against `avail.py --geometry` and its per-sweep output.**
2. **Attribution, thermal, cross-check.** Assertions from the metered fixtures:
   - every draw's litres equal the meter's, and its kind is right (shower, bath, tap);
   - the right tank's share on unmasked draws of 10 L or more, taken from the settled (+8 min) sweep, the only one whose total closes: about 0.89 on the 50 L shower (tanktemp's review of PR #2; the earlier "0.75" came from a +60 s reading that accounted for only 37 of 50 L, and tanktemp's briefing figure has the same dependence). Shares can't be measured during a reheat (the coil's heat, not the pipework, sets the apparent split), so they are absent then; under about 10 L they scatter and are indicative only;
   - the heat balance is within 10% on the layered shower, and flagged unreliable on the reheat bath, the small draws and the draw straight after the shower;
   - creep raises `continuous_flow` and makes no draw;
   - no draws on the quiet night;
   - the right P4 gap is corrected by strat: measured, strat removes P4's dip below P3 but leaves the profile up to 2.4 °C under the P3–P5 line (P5 at rest still reads above P3); the energy effect is ≤ 0.06 kWh;
   - CH-only heating is attributed to the right tank when it happens (8 Nov), and no heating is claimed when it doesn't (20–22 Nov);
   - the online UA fit follows the right tank's loss from about 5 W/K (loop stopped) to its loop-running value. **Measured against the boiler room, that is about 30–39 W/K, not 15–19:** the loop sheds its heat into the house's pipework, not the room. Hour by hour the loss fits 18 W/K × (T − 14.4 °C), the old slope with a reference about 10 °C below the boiler room; the 15–19 was measured against the colder hall. The fit stays against the room for now; fitting the reference temperature too (loss = UA·(T − T_ref)) is a later option;
   - system `low_hot_water` is judged through the **merged outlet**: the tanks are in parallel, so every draw takes a fixed split from both, and the tap gets the mix. The system's usable litres come from simulating a draw at the configured split (each tank's top pushed up by its share, the tops mixed), not from summing the tanks. The earlier check ("false overnight while the right tank alone is cold") was physically wrong: on quiet-night's morning (right top 35.8 °C, left 63.1 °C) the first litre at the measured split is about 39–43 °C and falling, so hot water is low. **Open, for the owner:** whether buoyancy shifts the split toward the hotter tank when one is cold. No fixture has a draw with one tank cold; a shower the morning after a loop night, before the HW timer, recorded by washreplay, would settle it.

   Built in phase 2: `internal/tank` (sweeps, with bulk-probe energy for comparisons across draws), `internal/thermal`, `internal/draw`, `internal/engine` and `cmd/washreplay`. Decisions from the data: a tank's power is unknown while its window holds a draw or the 12 min after it (the inlet plume settling plus the probes' lag; empirical), returning about 17.5 min after a draw. **Watch:** after the 50 L shower the first power back (about 18 min on) was still −1.33 kW of settle tail, so 12 min doesn't always cover it; it only reads as cooling (UA ignores it, UAQuiet is 60 min). Revisit with more showers before tuning; `low_hot_water` uses 60 L of total usable_l40_low, **provisional, for the owner to confirm**. **Agreed with statehouse (PR #2 review), to build before phase 3:** `draw_ended` goes out at the meter's end once the share sweep is in (about +60 s), with litres, kind and shares; a later `draw_balanced` carries the heat-balance cross-check. `active_draw` leaves the state in the same publish as `draw_ended`. `ended_at` (the last litre's closing) is identical in both events; statehouse passes it to `ClearSignal`. Fields known only at balance time are absent from `draw_ended`, not null. Shares (per tank) move to `draw_balanced` too (they need the settled sweep), and on a reheat-masked draw they are absent there, not null; statehouse adds them to the activity record with `UpdateActivity`. `low_hot_water` gets hysteresis (set below `low_hot_water_l`, clear above it plus a margin, both config), stays null when unknown, and WIRE.md says it is judged on `usable_l40_low`. statehouse will relay it on a system-level device (`hot_water_system`, scheme `washhouse`, primary `system`) carrying the totals. WIRE.md: the LWT gives `online: false`; `ActiveDraw.kind` is provisional; the meter's litres are named as since the engine started.
3. **Wire contract.** `docs/WIRE.md`, the `wire/washhouse` goldens, and a review with the statehouse and tanktemp sessions.
   Built: `docs/WIRE.md` (draft v1); `internal/wire/washhouse` (types, encoders, strict decoders that accept unknown keys, goldens; every WIRE.md example must equal a golden); `internal/publish` (engine → messages: retained topics on a content change, events first so the state dropping `active_draw` follows `draw_ended`); `washreplay -wire`, checked on every fixture against the contract's rules across messages. Decisions: times to the millisecond everywhere (draw times come from `last_pulse_at`); `share`/`litres` absent together when the split can't be measured, the only absent keys; a repeated `timestamp` isn't a replay on washhouse's topics (both tanks' sweeps share one), so compare payloads; a draw cut by a washhouse restart gets no `draw_ended`, and consumers expire it. The power settle tail is documented in WIRE.md (`power_kw`), not tuned yet. From the reviews of PR #3: on a draw whose shares are withheld, `outlet_peak_c` and `heat_balance_l` use the configured outlet split (the probes' split then measures the coil, 0–1 across the fixtures); `meter.timestamp` only from synced messages; device ids must match `[a-z0-9_-]+` (engine config); closed vocabularies (`state`, `heat_source`, `volume_kind`, status `event`) and case-variant keys are decode errors; status invariants checked; `ready` never falls; statehouse reconciles cut draws from State at `last_litre_at` rather than by expiry.
4. **Common extractions.** Re-scoped with the owner (23 Nov 2026) against what `common` already had: **(1)** `mqttpub` v0.2.0 gains `Subscribe(filter, qos, onMsg func(Received), onLost)`, `Received{Topic, Payload, Retained}`, the `Subscriber` interface and the fake's `Deliver`/`Lose`, with `Watch` rebuilt on it; it stays in `mqttpub` (one connection, one will), whose README now says it is the shared connection (sweeney/common#15). **(2)** a new `wire` module takes `wire/tanktemp`, `wire/boiler` and `wire/washhouse` from `internal/wire` (sweeney/common#16); washhouse keeps the fixture-decode test and checks every WIRE.md example round-trips through it. WIRE.md v1 frozen. **Not done:** `web/apiauth`/`web/serve` wait for phase 5, extracted from greenhouse's and countinghouse's copies when washhouse's API needs them; `remoteconfig` is used as it is (`Client`, `Tracker`), since common deliberately leaves keep-last-good to each service.
   **Carried forward from tanktemp's re-review of PR #2:**
   - Phase 5: boiler-sensor dates its will at connect time, so a live dropout is caught only by the silence rule. A SHUTDOWN delivered with retain 0 is live (MQTT spec): ingest should pass a Live bit so the engine makes HW and CH unknown at once. **But** (tanktemp, common#15): through a bridge, a retained message the bridge replays on reconnecting arrives with retain 0, so a Live bit must not rest on the flag alone; combine it with the will arriving after the last applied input in event time (the engine's ordering guard already bounds a stale replay).
   - Phase 3: after the 50 L shower the first power back was still −1.33 kW of settle tail. Either raise `power_settle` to 20 min or more, or say in WIRE.md that power just after a draw can still carry it. Decide with more showers.
   - After the valve test: re-fit the 0.11/0.89 outlet split from the extra unmasked draws. The merged usable steps when the right tank's strat plateau crosses about 36.8 °C, which a better split and profile will smooth.

5. **Service shell.** Ingest, publish, lifecycle, health, API, Influx, config, plus the remote-config namespace JSON for the user to write. In four PRs:
   - **5a, ingest and the loop.** `internal/behaviour` (washreplay's strict config, shared: the same document becomes the namespace in 5b), `internal/ingest` (subscriptions, decoding by topic, `Feed`, and a freshness gate: an input its publisher dates more than 90 s past the wall clock is refused, since event time never goes back; a retained sweep more than 90 s old is refused, as statehouse does; nothing rests on the retained flag), and `cmd/washhouse`: a local YAML config (broker, HTTP, and the topics: `base_topic`, `tanktemp_topic`, `boiler_topic` are local, not in the namespace, so a test run can publish under `test/water/hot` against the real behaviour), one loop goroutine owning the engine (paho's handlers only queue), `lifecycle` for STARTUP/HEARTBEAT/SHUTDOWN and a HEARTBEAT on each reconnect, and `/healthz`. Event time moves by the clock only after `freshness.silence` with no dated tanktemp input (a synced sweep or meter message: the room reports more often than that and would hide tanktemp's silence, tanktemp's review of #5), and `/healthz` is degraded on the same silence (WIRE.md, Conventions). `freshness.stale` must exceed tanktemp's sweep interval plus a sweep's length (30 s + about 4 s by default), or a current retained sweep is refused. The service publishes exactly what `washreplay -wire` does for the same recording (a test). Until 5b the behaviour document is a local file.
   - **5b, config.** The behaviour document becomes the `washhouse` namespace, fetched with `remoteconfig.Client` as an identity client (`identity.client_secret_file`; `behaviour_file` goes, and a local file setting it is refused). washhouse refuses to start without it (systemd retries), and `-check-config` fetches it. SIGHUP fetches it again: a refused document (invalid, or a changed `ambient_topic`, which needs a restart since subscriptions are made at start) keeps the last good one and makes `/healthz` degraded until one lands; an unchanged one (compared compacted) changes nothing; a changed one rebuilds the engine on the loop and replays into it the latest admitted input on each topic (tanktemp's review of #6: otherwise the programmer, the room and tanktemp's status stay unknown until each speaks again, up to 15 min), keeping event time; it shows on `/healthz` as `behaviour_applied_at` (statehouse's review), which consumers see as a restart (WIRE.md, Delivery, clarified). `-check-behaviour FILE` checks a document offline, so a new one is checked with the installed build before it is written. Both base URLs must be https (http only to loopback, for tests). The namespace JSON is `testdata/behaviour.example.json`.
   - **5c, API.** `web/apiauth` and `web/serve` extracted to common (sweeney/common#17, `web/v0.3.0`): the Bearer middleware greenhouse, countinghouse and statehouse each carry (an active user's token or a service's; a token that can't be checked is a 503, never a 401), generic over the claim types so common takes no dependency on identity, and the JSON writers, CORS and graceful server. `internal/httpapi` serves `/api/v1/state` and `/api/v1/tanks/{id}` as the published payloads byte for byte, `/api/v1/draws` as the draw events (a reload keeps them), and `/api/v1/model` as the running behaviour; the loop records each input into an immutable snapshot, so a handler never touches the engine. `http.audience` (`off`/`warn`/`enforce`) is the audience check; the issuer is `identity.base_url`. The spec test validates every response over every fixture against `openapi.yaml` both ways (every key sent is declared, with its type and enum; every declared property is sent), and every route is in the spec; spectral lints it in CI. Moving the three services onto `web/apiauth`/`web/serve` is a follow-up.
   - **5d, Influx.** `internal/influx` (pure, in the core list) maps each published message, decoded through `wire/washhouse`, to points, so Influx holds what went out on the wire; `cmd/washhouse` queues them through influxdb-client-go's async WriteAPI (its errors drained from the start, flushed at shutdown within 5 s). Measurements `hot_water_tank`, `hot_water_system`, `hot_water_draw` (a row per draw at its `ended_at`, `draw_balanced` merging into `draw_ended`'s row, so `kind` is a field), `hot_water_draw_tank` and `hot_water_continuous_flow`; nulls left out; booleans as 0/1 (greenhouse's review: a mean is the fraction); field types fixed (statehouse's). `site.id` is required (greenhouse: a missing tag is a series a filter misses); `influx: {url, org, bucket, token_file}` is optional, https or loopback. A write failure shows on `/healthz` but doesn't degrade it (statehouse's review). A tank's or the system's row is held until a later time and written once with its final content (`influx.Rows`), since Influx keeps fields a later write at the same time leaves out (tanktemp's review: 65 tank rows kept a stale `heat_source`, 224 a `power_kw` gone unknown); it also cuts about a third of the writes. **Later (tanktemp's suggestion):** a `hot_water_meter` series from the meter's sweep messages, `count` per `counting_since`, to reconcile a day's litres with cut draws and creep. Settled with statehouse: washhouse is the only writer of hot-water figures to Influx, and statehouse's adapter writes no cylinder or system readings into `device_*` (the owner to confirm when the adapter is built).
   - Still open: the Live bit for boiler-sensor's will (above).
6. **Deploy.** `http.audience` starts at `warn`; once the log shows every caller's tokens (the app's user tokens too) name `washhouse`, switch to `enforce` and don't leave it on `warn`: the draws list is an occupancy record (statehouse's review of #7). Built (24 Nov 2026) with the swee:deploy-service skill. A release is a build and its config together (`/opt/washhouse/releases/<time>-<commit>/`, `current` linking to the running one): tanktemp's review of #9 showed that checking a new config with the installed build, and a new build with the installed config, deadlocks a change that adds a required key. `deploy/bootstrap.sh` runs from the dev machine, copies itself, the unit and the config to the host and runs there as root: the user, directories, the deploy user in `systemd-journal`, the client secret, a new Influx bucket `washhouse` with a write-only token, the unit, and a sudoers rule of two exact commands (restart; the staged build's `-check-config` as washhouse), checked with `sudo -n -l`. `deploy/deploy.sh` deploys HEAD only when it is on `origin/main`; stages build and config in `next/`, where the new build checks the new config as washhouse before the switch (a new link renamed over `current`); then active within 45 s (past bootWait), the host's and the public `/healthz` on the new commit; three releases kept; `--rollback` steps back. Each takes `--check` and `--yes`, tested against stubs and a fake host (`deploy/*_test.sh`, mutants killed). **Corrected after going live (24 Nov 2026, the owner):** the system's usable water is the tanks' summed (a 40 L run with the left tank at 62 °C and the right at 40 °C was too hot to hold, where the fixed 0.89 split from the right gave 43 °C; washhouse's own split for it was 33/67, and 84/16 on an earlier tap, so the split moves with conditions); the drawable band runs to the dome top (an internal outlet pipe); and the outlet probe's resting under-read is corrected by an offset learned from a rested draw (8.9 and 7.6 °C on ch-only-no-heat; 8.5 and 6 °C measured live). **The post-draw heating hold** (the owner, 24 Nov 2026, "not fit for purpose": a reheat 12 min after a tap showed as heating 9 min after the HW timer came on): with HW on now (not within source_hold, and not CH alone: the coil's afterglow reached 1.48–1.58 kW, and CH heats neither tank since the valve; tanktemp's reviews of #15), the sweeps since the draw say heating at 1.5 kW or more (`heating_on_after_draw_kw`, a new required key), from the fixtures: a draw's settling showed at most 0.72 kW with no heat going in (CH on, ch-only-no-heat), reheats 2–9 kW; it calls them about 6 min after a draw instead of 17. Cooling still waits. **tanktemp's four-day replay of 4417f7e:** the summed usable_l40 falls by what the meter says within about ±20 L on every unmasked draw of 40 L or more, and drifts 9–10 L/h overnight (the standing loss); some small draws drop it 50–60 L for an hour, which is mostly real (cold water mixing up past the 400 mm probe as it crosses 40 °C; that probe stands for a ~50 L band, so the step is coarse). **Follow-up (tanktemp's review of #15, pre-existing):** when HW goes off with CH on, the source flips to ch, and back to hw through the hold when CH goes off; CH heats neither tank since the valve was replaced, so it should stay hw. **Follow-ups from statehouse's review of #12:** common/wire's next release updates the State golden (its `usable_l40` predates the sum); tanktemp's review of #12 (four days of rested draws: left 7.2–9.5 °C, right 6.9–7.9 when hot but 1.1–1.5 when the cool tank the other supplies) made the correction a fraction of the water's height above the cold feed (0.16–0.17; a cold tank gets none), learned per tank only from a draw it gave two litres or more of, the median of the last five, with a configured prior per tank (`prior_outlet_underread`, a new required key: write it to the namespace before deploying the build that needs it), carried across a reload; the attribution compares energy without the correction. **A blind cylinder (sweeney/washhouse#17):** a loose jumper lead took the right tank's whole probe bus out for 46 h; washhouse kept it `unknown` (right) but left the house's figure `null`, its state "heating" for 9 min, `/healthz` ok, and a log line and feed error per sweep (7,439). Now: the tank's `fault` and `unknown` state from its first unreadable sweep; `partial`, the other tank's figures as a lower bound, with `low_hot_water` false from it when it clears the level and margin (never true); `tank_lost` after `tank_fault_after` (5 min, a new required key) and `tank_restored` after `tank_restore_after` (2 min, a new required key) of good sweeps, one log line each and `/healthz` degraded between; common/wire v0.2.0 carries the new keys and events (additive: older payloads still decode). The fixture `right-bus-lost` pins it against tanktemp's independent replay. **Follow-up (tanktemp's replay):** the 27 draws in that spell got no shares (`tank_missing`); the left tank was fully known, so its own litres could be given and the right's as metered minus left. Owner's choices: port 8787 (on loopback, behind the tunnel), a new bucket while testing (greenhouse is climate-only and doesn't read it; whether anything charts it is the owner's call), a public route, and the first deploy publishing under `test/water/hot` for the live shadow (Verification), switched to `water/hot` by deploying a config change. Host: the home server (named only in gitignored `deploy/host`); settle commit dates before deploying.
7. **Statehouse PRs** (1→4 above).
8. **Later:** time-to-empty and time-to-full forecasts, a daily usage summary, a hwplan-style programme advisor, and watching the before/after lagging protocol through the UA fit.

## Verification

- `make test` (`-race`), `go vet`, staticcheck, spectral on the OpenAPI, and gofmt in CI.
- `washreplay` over each fixture prints the expected events, and golden files pin them.
- Run against a local Mosquitto (`mqtttest`) with recorded messages replayed onto `test/water/hot/tanktemp/...`. Check the retained `test/water/hot/*` outputs and `/healthz`.
- Live shadow: run locally against the real broker, publishing to the `test/` prefix, for a day. Every published draw must match `tools/meter_audit.py` on the same day's recording, litre for litre. Compare `usable_l40` with `avail.py --live`, and check `tools/smoke.sh` passes throughout.
- statehouse: adapter unit tests with a fake engine. After deploy, `/state/activity` shows `hot_water_draw` signals and the cylinder devices carry litres.
