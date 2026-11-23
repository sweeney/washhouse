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
| `deploy/config.example.yaml` | The service's local config: the broker, HTTP, and the topics. |
| `testdata/behaviour.example.json` | The behaviour document (cylinders, meter rules, thresholds) with the plan's values. |

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
`base_topic: test/water/hot` in a copy of `deploy/config.example.yaml`):

```
go run ./cmd/washhouse -config my-config.yaml -check-config
go run ./cmd/washhouse -config my-config.yaml
curl -s localhost:8095/healthz
```

The tools tests need `python3`, `mosquitto`, `mosquitto_sub`/`mosquitto_pub`
and `shellcheck`. Conventions for changes are in [`CLAUDE.md`](CLAUDE.md).
