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
service (`cmd/washhouse`) subscribes to its inputs and publishes WIRE.md's
messages, with `/healthz`; its API, Influx and the config service come
next.

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
| `cmd/washhouse` | The service: subscribes, runs the engine, publishes, and serves `/healthz`. |
| `deploy/config.example.yaml` | The service's local config: the broker, HTTP, the topics, and where the behaviour document is (the config service, read with identity client credentials). |
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

To change the behaviour, check the new document with the installed build
first, write it to the namespace, then send SIGHUP:

```
washhouse -check-behaviour new-behaviour.json
kill -HUP "$(pidof washhouse)"   # the unit's reload, once phase 6 has one
```

On SIGHUP, washhouse fetches the namespace again. A document it refuses
(invalid, or a new `ambient_topic`, which needs a restart because the
subscriptions are made at start) leaves it running the last good one, and
`/healthz` is degraded until a good one lands. An unchanged document
changes nothing. A changed one starts the engine again, which consumers
see as a restart (docs/WIRE.md, Delivery): the UA fit goes back to its
priors, and a draw in progress gets no `draw_ended`.

The tools tests need `python3`, `mosquitto`, `mosquitto_sub`/`mosquitto_pub`
and `shellcheck`. Conventions for changes are in [`CLAUDE.md`](CLAUDE.md).
