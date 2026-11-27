# washhouse tools

Live-data tools used before washhouse itself exists: record what washhouse
will read, smoke-test the producers, and audit recordings after the fact.
All are read-only on the broker and the hosts. No host, IP or site detail is
named here: the broker and tanktemp's `/healthz` URL come from flags, env, or
one-line files in `~/washhouse-recordings/` (`broker`, `tanktemp-healthz`).

| Tool | What it does |
|---|---|
| `record.sh start\|stop\|status` | Records tanktemp (`water/hot/#`), washhouse's live shadow (`test/water/hot/#`), boiler-sensor (`energy/boiler/#`), boiler-room ambient, the weather station (`tempest/#` and statehouse's device) and statehouse's hot-water devices to `~/washhouse-recordings/live-YYYYMMDD.log`, one file per UTC day, each starting with the retained messages. Reconnects after broker restarts. |
| `smoke.sh [--expect-meter] [--healthz URL] [--save DIR]` | One PASS/WARN/FAIL line per check: tanktemp online, fresh, all probes OK; tank states fresh; the meter's retained message against WIRE.md (field order, vocabulary, freshness, counters 0); `/healthz` meter `counting` and agreeing with MQTT; boiler-sensor heartbeat. Exit 0 / 1 FAIL / 2 WARN / 3 couldn't fetch. |
| `meter_audit.py FILE...` | Post-hoc audit of a recording: seq gaps and duplicates, count monotonic, pulse/open pairing and timing, sweep cadence, counter rises with the nearest boiler event, then washhouse's provisional reading: draws (a draw ends after a minute with no litre), flow, and the reed's duty. Exit 1 on a contract problem. |
| `meter_scenarios.py [OUTDIR]` | Writes the synthetic meter scenarios (built with `meter_audit_test.py`'s emitter) and `meter_audit.py`'s draws for each to `internal/meter/testdata/audit/`, where the Go meter consumer's parity test replays them. `test.sh` fails if they are stale. |
| `live.sh [--check] [--demo] [--low L --margin L]` | Opens `live.html`, a live view of the hot-water system in the browser: tanktemp's probes and meter beside washhouse's reading of them (the water to plan on against the low threshold, each cylinder's state and power, draws as they run and after, the heat loss over time), with a line on every message saying what it means. It reads the broker and topics from `deploy/config.local.yaml`, connects over MQTT-over-WebSockets (port 9001), and only subscribes. `--low`/`--margin` are the namespace's `low_hot_water_l` and margin, which aren't on the wire, to draw the threshold. With no broker it plays a demo you can drive. Built on tanktemp's `tools/live.html`. Its logic is `live-core.js`, tested against the wire goldens (`live_core_test.js`); `live_page_check.sh` runs the page in headless Chrome. |
| `config.html` | The behaviour config explorer: open it in a browser, then load the `washhouse` namespace's JSON (paste or open a file) or the sample. For every key it shows what it means, its unit, the values washhouse accepts, what it affects, and whether changing it needs a restart; and it lists every problem washhouse would refuse the document for (missing, unknown or duplicate keys, values out of range, rules between keys) and what differs from the sample. Read-only. Its logic is `config-core.js` (`config_core_test.js`); a Go test in `internal/behaviour` fails if a key washhouse decodes has no definition, or a definition outlives its key. The verdicts are held to washhouse's by `testdata/configtool/cases.jsonl`, edits to the example document, each with whether washhouse accepts the result: the Go test asserts `behaviour.Load` gives each verdict, and the node test that the page does. To add a case, add a line with `"ok"` set to what `Load` says (the Go test names any it disagrees with). |
| `test.sh` | Runs every test (unit, then local-broker tests for the shell tools). |

After a tanktemp deploy: `tools/smoke.sh --expect-meter`, then later
`tools/meter_audit.py ~/washhouse-recordings/live-*.log`.
