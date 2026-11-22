# meter_audit.py parity scenarios

Synthetic, tanktemp-shaped meter recordings (`%I %t %p` lines) and
`tools/meter_audit.py`'s draws for each (`NAME.draws.json`). Written by
`python3 tools/meter_scenarios.py`, which builds them with
`tools/meter_audit_test.py`'s emitter; `tools/test.sh` fails if they no
longer match what the audit says. Don't edit by hand.

`audit_test.go` replays each one through the Go consumer and requires the
same draws. Only scenarios where the two are meant to agree are here (see
the generator's docstring).
