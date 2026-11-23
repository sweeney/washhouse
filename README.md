# washhouse

Hot-water state for a house with two stored cylinders. washhouse will read
[tanktemp](https://github.com/sweeney/tanktemp)'s probe temperatures and
hot-water meter, and the boiler programmer's HW/CH state, and publish what a
person (or statehouse) cares about: how much hot water is left, whether each
tank is heating or cooling, and every draw, with its litres, flow, kind and
which tank supplied it.

**Status: planning.** The design is in [`PLAN.md`](PLAN.md). What is known
about the cylinders, with a confidence label on every point, is in
[`docs/HOT-WATER-SYSTEM.md`](docs/HOT-WATER-SYSTEM.md).
[`docs/washhouse-explainer.html`](docs/washhouse-explainer.html) is an
interactive explainer that simulates a day through the planned engine. The
service itself (`cmd/washhouse`) only reports its version so far.

## What's here

| Path | What it is |
|---|---|
| `PLAN.md` | Decisions, architecture, phases, verification. |
| `docs/WIRE.md` | **Normative:** the MQTT contract washhouse will publish (state, tanks, events, status). Draft, under review. |
| `docs/HOT-WATER-SYSTEM.md` | The cylinders, probes, plumbing and dynamics: what was measured, said, inferred or guessed. |
| `docs/washhouse-explainer.html` | Open in a browser. A simulated day (fictional times) through the planned engine. |
| `tools/` | Live-data tools used before washhouse exists: a recorder, a smoke test for tanktemp and boiler-sensor, and a meter audit. See [`tools/README.md`](tools/README.md). |
| `meter/` | The hot-water meter's datasheet and photos. |
| `internal/` | The pure core: decoders, the meter consumer, the cylinder model, thermal tracking, draw attribution, `engine`, which wires them into the system's state and events, and `publish`, which turns those into the wire messages of `internal/wire/washhouse` (its goldens pin WIRE.md). |
| `cmd/washreplay` | Replays recordings through the engine and prints its events and states. |
| `cmd/washhouse` | The service, a placeholder until phase 5. |

## Development

```
make check     # vet, gofmt, staticcheck, go test -race, and the tools tests
make build     # bin/washhouse, stamped with the commit
```

Replay a recording (gzipped or plain) through the engine; the config holds
every threshold and the cylinders' geometry:

```
go run ./cmd/washreplay -config testdata/washreplay-config.example.json \
    [-state-every 10m] [-json] testdata/fixtures/shower-layered.log.gz
```

With `-wire` instead of `-json`, it prints every message the service would
publish, as `topic payload` lines (docs/WIRE.md).

The tools tests need `python3`, `mosquitto`, `mosquitto_sub`/`mosquitto_pub`
and `shellcheck`. Conventions for changes are in [`CLAUDE.md`](CLAUDE.md).
