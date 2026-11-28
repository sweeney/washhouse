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
| `deploy/` | Setting up the host and deploying to it (below); the unit, `washhouse.service`. |
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
/opt/washhouse/current/washhouse -check-behaviour new-behaviour.json
sudo systemctl reload washhouse   # the unit's reload is SIGHUP
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
priors, and a draw in progress gets no `draw_ended`. A cylinder's blind
spell carries over (it isn't lost again, and its restore counts on).

## A blind cylinder

When a cylinder's probes can't be read (a bus failing, a loose lead), or
no sweep comes from it, washhouse marks it blind (docs/WIRE.md, Faults):
its `fault` says since when and why, with tanktemp's error codes, and its
state is `unknown` at once. The house's totals stay `null`; `partial`
gives what the other cylinder holds, a lower bound, and `low_hot_water` is
`false` from it when that alone clears the threshold and margin. After
`tank_fault_after` (5 min) it sends `tank_lost`, logs it once, and
`/healthz` is degraded, naming the cylinder, since when and why (its
`tanks` lists each cylinder's probes, fault and `lost_since`); after
`tank_restore_after` (2 min) of good sweeps, `tank_restored` and one log
line. The blind sweeps themselves are not feed errors and aren't logged
one by one. The live page's demo (`tools/live.html`, **Blind the right
cylinder**, **A brief blip**) shows it end to end.

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
| `hot_water_tank` | `device_id` | the tank's `timestamp` | `state`, `heat_source` (strings), `stored_kwh`, `mean_c`, `top_c` (from the outlet probe corrected for its heat sink, not its low reading at rest; WIRE.md, Tank), `hot_l`, `usable_l40`, `usable_l40_low`, `power_kw`, `ua_w_per_k`, `probes_ok` (probes washhouse uses: not tanktemp's `ok`, since a suspect 85 °C reading is left out), `probes_total` , `blind` (1 while it has a `fault`), `fault_reason` |
| `hot_water_system` | | the state's `timestamp` | `stored_kwh`, `usable_l40`, `usable_l40_low`, `low_hot_water`, `boiler_hw`, `boiler_ch`, `ambient_c`, `drawing`, `continuous_flow`, `tanktemp_online`, `tanks_missing` (cylinders whose estimate is unknown); while one is blind, `partial_stored_kwh`, `partial_usable_l40`, `partial_usable_l40_low` (lower bounds, kept apart from the totals) |
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

## Deploy

The host is the first line of `deploy/host`, and its config is
`deploy/config.local.yaml` (both gitignored; the shape is
`deploy/config.example.yaml`'s). Every script takes `--check` first,
which changes nothing, and `--yes` to skip its one prompt.

```
deploy/bootstrap.sh --check   # what setting the host up would change
deploy/bootstrap.sh           # set it up (sudo on the host asks for your password)
deploy/deploy.sh --check      # build HEAD; the new build checks the host's config
deploy/deploy.sh              # deploy HEAD
deploy/deploy.sh --rollback   # back to the build before
```

A release is a build and its config together: `deploy.sh` carries
`deploy/config.local.yaml` with each build to
`/opt/washhouse/releases/<time>-<commit>/`, and `/opt/washhouse/current`
links to the one that runs. So a config change is deployed like a code
change, the build that checks a config is the build that runs it, and a
rollback takes both back. The secrets the config names stay in
`/etc/washhouse`.

`bootstrap.sh` copies itself, the unit and the config (read for the
secret's path and the Influx section, not installed) to the host and runs
there as root. It creates the `washhouse` user and directories, adds the
deploy user to `systemd-journal`, writes the identity client secret
(asked for, hidden), creates the Influx bucket and a token that can only
write to it, installs the unit, and installs a sudoers rule of two exact
commands with no wildcards: restarting washhouse, and the staged build's
`-check-config` as `washhouse`. It then checks that sudo grants the
restart. It doesn't start washhouse, and a second run changes nothing.

`deploy.sh` deploys HEAD only when it is committed and on `origin/main`,
so the hash on `/healthz` is settled. It stages the build and the config
in `/opt/washhouse/next`, shows how the config differs from the running
release's, and has the new build check the new config there as
`washhouse` (the config, the behaviour namespace, the Influx token)
before anything is switched. A refused one leaves the running release as
it was. The switch renames a new link over `current`. Then washhouse must
be active within 45 s (past its own 30 s wait for the namespace), and the
host's `/healthz` and the public URL's (`http.public_url`) must both show
the new commit. Three releases are kept, and each `--rollback` steps one
further back.

Before the first deploy, washhouse needs an identity client (client
credentials), the `washhouse` namespace holding the behaviour document
(check it first with `washhouse -check-behaviour FILE`) and readable by
that client, and a route from `http.public_url` to `http.listen`.

Tests: `deploy/bootstrap_test.sh` and `deploy/deploy_test.sh` (in
`make check`) run both scripts against stubs and a fake host.

The tools tests need `python3`, `mosquitto`, `mosquitto_sub`/`mosquitto_pub`
and `shellcheck`. Conventions for changes are in [`CLAUDE.md`](CLAUDE.md).
