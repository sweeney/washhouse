#!/usr/bin/env bash
# Tests for scripts/make-fixture.sh, on synthetic recordings (no real data).
# Usage: scripts/make_fixture_test.sh
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
MF="$HERE/make-fixture.sh"
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT

pass=0 fail=0
ok()  { pass=$((pass+1)); echo "ok   $*"; }
bad() { fail=$((fail+1)); echo "FAIL $*"; }
expect() { local what="$1"; shift; if "$@"; then ok "$what"; else bad "$what"; fi; }
# run CMD...: sets $code and $out (stdout+stderr), never trips set -e
run() { set +e; out=$("$@" 2>&1); code=$?; set -e; }

# A synthetic recording on a made-up day in 2030.
mkdir -p "$T/rec"
{
    echo '2030-06-01T09:59:50+0000 water/hot/tanktemp/meter {"device_id":"hot_water_meter","timestamp":"2030-06-01T09:59:50Z","seq":1,"count":2,"last_pulse_at":"2030-06-01T09:59:40.250Z"}'
    echo '2030-06-01T10:00:00+0000 house/state/devices/hot_water {"id":"hot_water"}'
    echo '2030-06-01T10:00:05+0000 energy/boiler/sensor/system {"status":{"event":"HEARTBEAT","ch":"OFF","hw":"ON","ready":true,"timestamp":"2030-06-01T10:00:05Z","network":{"ip":"10.1.1.1","ssid":"examplenet"}}}'
    echo '2030-06-01T10:00:10+0000 water/hot/tanktemp/meter {"device_id":"hot_water_meter","timestamp":"2030-06-01T10:00:10Z","seq":2,"count":3,"last_pulse_at":"2030-06-01T10:00:09.500Z"}'
    echo '2030-06-01T10:30:00+0000 water/hot/tanktemp/meter {"device_id":"hot_water_meter","timestamp":"2030-06-01T10:00:10Z","seq":2,"count":3,"last_pulse_at":"2030-06-01T10:00:09.500Z"}'
} >"$T/rec/a.log"
FROM=2030-06-01T10:00:00Z TO=2030-06-01T11:00:00Z
OUT="$T/fx/cut.log.gz"

run "$MF" --help
expect "--help exits 0" test "$code" -eq 0
expect "--help names --check and --slices" grep -q -- '--slices' <<<"$out"

run "$MF"
expect "no arguments: usage error" test "$code" -eq 2
expect "no arguments: says what is missing" grep -q -- '--from' <<<"$out"

run "$MF" --check --from "$FROM" --to "$TO" --out "$OUT" "$T/rec/a.log"
expect "--check exits 0" test "$code" -eq 0
expect "--check reports per topic" grep -q 'water/hot/tanktemp/meter' <<<"$out"
expect "--check says it would write" grep -q 'would write' <<<"$out"
expect "--check writes nothing" test ! -e "$T/fx"

run "$MF" --from "$FROM" --to "$TO" --out "$OUT" "$T/rec/a.log"
expect "cut exits 0" test "$code" -eq 0
expect "cut writes the fixture" test -s "$OUT"
expect "fixture starts at the fake epoch" grep -q '^2001-01-01T00:00:00+0000 water/hot/tanktemp/meter ' <(gzip -dc "$OUT")
expect "prelude, heartbeat, window; replay dropped" test "$(gzip -dc "$OUT" | wc -l | tr -d ' ')" -eq 3
expect "no real date, ssid or ip left" test -z "$(gzip -dc "$OUT" | grep -iE '2030|ssid|"ip"' || true)"
cp "$OUT" "$T/first.gz" 2>/dev/null || : >"$T/first.gz"

run "$MF" --from "$FROM" --to "$TO" --out "$OUT" "$T/rec/a.log"
expect "re-run exits 0" test "$code" -eq 0
expect "re-run says unchanged" grep -q unchanged <<<"$out"
expect "re-run is byte-identical" cmp -s "$OUT" "$T/first.gz"

run "$MF" --from 2030-06-01T10:00:10Z --to "$TO" --out "$OUT" "$T/rec/a.log" </dev/null
expect "changed fixture without --yes refused" test "$code" -eq 1
expect "refusal names --yes" grep -q -- '--yes' <<<"$out"
expect "refused fixture left alone" cmp -s "$OUT" "$T/first.gz"

echo '2030-06-01T10:00:01+0000 water/hot/tanktemp/status {"online":true,"broker":"192.168.9.9"}' >"$T/rec/leak.log"
run "$MF" --from "$FROM" --to "$TO" --out "$T/fx/leak.log.gz" "$T/rec/leak.log"
expect "leak refused" test "$code" -eq 1
expect "leak refusal says why" grep -q '192\.168\.' <<<"$out"
expect "leak refusal does not echo the data" test -z "$(grep '192\.168\.9' <<<"$out" || true)"
expect "leak writes nothing" test ! -e "$T/fx/leak.log.gz"

run "$MF" --from "$FROM" --to "$TO" --out "$T/fx/x.log.gz" "$T/rec/missing.log"
expect "missing input: usage error" test "$code" -eq 2
expect "missing input named" grep -q 'missing.log' <<<"$out"

printf '# name\tfrom\tto\tinputs\tdescription\ncut\t%s\t%s\ta.log\tsynthetic\n' "$FROM" "$TO" >"$T/rec/slices.tsv"
run "$MF" --check --slices "$T/rec/slices.tsv" --out-dir "$T/sl"
expect "--slices --check exits 0" test "$code" -eq 0
expect "--slices --check writes nothing" test ! -e "$T/sl"
run bash -c "cd / && '$MF' --slices '$T/rec/slices.tsv' --out-dir '$T/sl'"
expect "--slices from another directory exits 0" test "$code" -eq 0
expect "--slices cuts the same bytes as the single cut" cmp -s "$T/sl/cut.log.gz" "$T/first.gz"

mkdir -p "$T/nopython"
run env PATH="$T/nopython" /bin/bash "$MF" --check
expect "no python3: fails" test "$code" -ne 0
expect "no python3: says so" grep -q 'python3' <<<"$out"

echo "make_fixture_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
