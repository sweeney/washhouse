# washhouse

Hot-water state for a house with two stored cylinders. washhouse will read
[tanktemp](https://github.com/sweeney/tanktemp)'s probe temperatures and
hot-water meter, and the boiler programmer's HW/CH state, and publish what a
person (or statehouse) cares about: how much hot water is left, whether each
tank is heating or cooling, and every draw, with its litres, flow, kind and
which tank supplied it.

**Status: building (phase 5, the service).** The design is in [`PLAN.md`](PLAN.md). What is known
about the cylinders, with a confidence label on every point, is in
[`docs/HOT-WATER-SYSTEM.md`](docs/HOT-WATER-SYSTEM.md).
[`docs/washhouse-explainer.html`](docs/washhouse-explainer.html) is an
interactive explainer that simulates a day through the planned engine. The
service (`cmd/washhouse`) subscribes to its inputs, publishes WIRE.md's
messages, serves its API and `/healthz`, reads its behaviour from the
config service, and writes its history to Influx; deploying it is next.

## What's here

| Path | What it is |
|---|---|
| `PLAN.md` | Decisions, architecture, phases, verification. |
| `docs/WIRE.md` | **Normative:** the MQTT contract washhouse publishes (state, tanks, events, status). v1, frozen. |
| `docs/HOT-WATER-SYSTEM.md` | The cylinders, probes, plumbing and dynamics: what was measured, said, inferred or guessed. |
| `docs/washhouse-explainer.html` | Open in a browser. A simulated day (fictional times) through the planned engine. |
| `tools/` | Live-data tools used before washhouse exists: a recorder, a smoke test for tanktemp and boiler-sensor, and a meter audit. See [`tools/README.md`](tools/README.md). |
| `meter/` | The hot-water meter's datasheet and photos. |
| `internal/` | The pure core: decoders, the meter consumer, the cylinder model, thermal tracking, draw attribution, `engine`, which wires them into the system's state and events, and `publish`, which turns those into the wire messages of `common/wire/washhouse` (in `sweeney/common`; its goldens pin WIRE.md). The decoders for washhouse's inputs are `common/wire/tanktemp` and `common/wire/boiler`. |
| `cmd/washreplay` | Replays recordings through the engine and prints its events and states. |
| `cmd/washhouse` | The service: subscribes, runs the engine, publishes, and serves the API and `/healthz`. |
| `internal/influx` | The Influx points for each published message (below). |
| `internal/httpapi` | The HTTP API and its spec (`openapi.yaml`), which a test checks against every response over every fixture. |
| `deploy/config.example.yaml` | The service's local config: the broker, HTTP, the topics, where the behaviour document is (the config service, read with identity client credentials), the site, and Influx. |
| `testdata/behaviour.example.json` | The behaviour document (cylinders, meter rules, thresholds) with the plan's values: the shape of the `washhouse` namespace. |

## Development

```
make check     # vet, gofmt, staticcheck, go test -race, and the tools tests
make build     # bin/washhouse, stamped with the commit
```

Replay a recording (gzipped or plain) through the engine; the config holds
every threshold and the cylinders' geometry:

```
go run ./cmd/washreplay -config testdata/behaviour.example.json \
    [-state-every 10m] [-json] testdata/fixtures/shower-layered.log.gz
```

With `-wire` instead of `-json`, it prints every message the service would
publish, as `topic payload` lines (docs/WIRE.md).

Run the service against a broker, publishing under a test prefix (set
`base_topic: test/water/hot` in a copy of `deploy/config.example.yaml`).
Its behaviour is the config service's namespace (`remote_config`), read
as an identity client; washhouse refuses to start without it.
`-check-config` checks the local file and fetches the namespace:

```
go run ./cmd/washhouse -config my-config.yaml -check-config
go run ./cmd/washhouse -config my-config.yaml
curl -s localhost:8095/healthz
```

## API

The spec is `GET /openapi.json` (source: `internal/httpapi/openapi.yaml`).
`/healthz` and `/openapi.json` are public; the rest needs a Bearer token from
identity, a user's (active) or a service's. `http.audience` says whether the
token must name `washhouse`: `off`, `warn` (accept and log one that
doesn't) or `enforce`. A token that can't be checked because identity's
keys are unavailable is a 503 with `Retry-After`, not a 401; so is a
state asked for before the first input. Switch `http.audience` to
`enforce` once the `warn` log shows every caller's tokens name
`washhouse` (user tokens included): the draws are an occupancy record.

| Path | What |
|---|---|
| `GET /api/v1/state` | The system's state: `{base}/state`'s payload, byte for byte (503 before the first input). |
| `GET /api/v1/tanks/{id}` | One tank: `{base}/tank/{id}`'s payload. |
| `GET /api/v1/draws` | Recent draws, newest first, each its `draw_ended` and `draw_balanced` events (`balanced` is `null` until it comes); at most `recent_draws`, kept across a reload. A draw cut by a restart or a reload has no `draw_ended`, so it is never listed. |
| `GET /api/v1/model` | The running behaviour document and when it was applied. |

```
curl -s -H "Authorization: Bearer $TOKEN" localhost:8095/api/v1/state
```

To change the behaviour, check the new document with the installed build
first, write it to the namespace, then send SIGHUP:

```
washhouse -check-behaviour new-behaviour.json
kill -HUP "$(pidof washhouse)"   # the unit's reload, once phase 6 has one
```

On SIGHUP, washhouse fetches the namespace again. A fetch that fails, or a
document it refuses (invalid, or a new `ambient_topic`, which needs a
restart because the subscriptions are made at start), leaves it running
the last good one, and
`/healthz` is degraded until a good one lands (it shows each namespace's
last fetch, with `fetched_ago` in seconds, and `behaviour_applied_at`). An unchanged document
changes nothing. A changed one starts the engine again, fed the latest
input on each topic (as a restart's retained messages would be), which
consumers see as a restart (docs/WIRE.md, Delivery): the UA fit goes back to its
priors, and a draw in progress gets no `draw_ended`.

## Influx

With an `influx` section in the local config, washhouse writes what it
publishes to Influx (`internal/influx`), so the history is what went out
on the wire. Each point is tagged `site` (`site.id`, required), at event
time (the publisher timestamp WIRE.md gives, never the wall clock). Retained
topics are written when they are published, so on a change. A tank's or
the system's row is written once, with its last content at that
timestamp: Influx merges a second write at a row's time and keeps the
fields it leaves out, and WIRE.md publishes a topic more than once at one
timestamp, so each series holds its latest row until a later one arrives
(or washhouse stops). A row lands one publish late, and a crash loses at
most the held row per series. An unknown
(`null`) value is left out. A boolean is written as `0`/`1`, so a `mean`
over a window is the fraction of it spent so. A field's type never changes:
quantities are floats even when whole, counts integers, words strings.

| Measurement | Tags | Time | Fields |
|---|---|---|---|
| `hot_water_tank` | `device_id` | the tank's `timestamp` | `state`, `heat_source` (strings), `stored_kwh`, `mean_c`, `top_c` (the decayed P5 draw peak, not P5 at rest), `hot_l`, `usable_l40`, `usable_l40_low`, `power_kw`, `ua_w_per_k`, `probes_ok` (probes washhouse uses: not tanktemp's `ok`, since a suspect 85 °C reading is left out), `probes_total` |
| `hot_water_system` | | the state's `timestamp` | `stored_kwh`, `usable_l40`, `usable_l40_low`, `low_hot_water`, `boiler_hw`, `boiler_ch`, `ambient_c`, `drawing`, `continuous_flow`, `tanktemp_online` |
| `hot_water_draw` | | the draw's `ended_at` | from `draw_ended`: `draw_id`, `kind`, `litres`, `duration_s`, `flow_l_min`; from `draw_balanced`, minutes later, into the same row: `energy_kwh`, `heat_balance_l`, `heat_balance_reliable`, `masked_by_reheat`, `reasons` (comma-separated), `outlet_peak_c`, `inlet_drop_c` |
| `hot_water_draw_tank` | `device_id` | the draw's `ended_at` | from `draw_balanced`: `draw_id`, `share`, `litres` (both absent when withheld), `outlet_peak_c`, `inlet_drop_c` |
| `hot_water_continuous_flow` | | the run's `since` | from `continuous_flow_ended`: `litres`, `flow_l_min`, `duration_s` |

- A draw's row is its `ended_at`, at full millisecond precision, and the
  two events merge because they write the same series at the same time.
  That is why `kind` is a field, not a tag: `draw_balanced` doesn't carry
  it. Filter on it after a pivot.
- `device_id` is the tank's id, as in `devices_home` and statehouse.
- `ambient_c` and `boiler_*` are what washhouse saw of its inputs. The
  room's and the programmer's own series are statehouse's.
- In Flux, `aggregateWindow` stamps a window at its end by default; use
  `timeSrc: "_start"`.

`/healthz` shows `influx` (`null` when off): points `queued` and
`dropped`, writes `failed`, messages `unmapped` (a bug if ever non-zero),
`last_queued_at`, and the last error. Points go through a bounded queue to
the client (influxdb-client-go, which batches), so a hanging Influx never
holds the loop: what the queue can't hold is dropped and counted. A
failure doesn't make washhouse degraded: Influx is history, not the
service. During an outage the client's retry buffer drops its oldest
points when full, and it reports at most one error at a time, so `failed`
undercounts what was lost. `-check-config` reads the token file.

statehouse writes its booleans as Influx booleans, not `0`/`1`; a query
across both services' measurements must allow for that.

The tools tests need `python3`, `mosquitto`, `mosquitto_sub`/`mosquitto_pub`
and `shellcheck`. Conventions for changes are in [`CLAUDE.md`](CLAUDE.md).
