#!/usr/bin/env bash
# Tests for tools/smoke.sh against a throwaway local mosquitto and a local
# /healthz. Usage: tools/smoke_sh_test.sh
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SMOKE="$HERE/smoke.sh"
MOSQ=$(command -v mosquitto || ls /opt/homebrew/sbin/mosquitto 2>/dev/null || true)
[ -n "$MOSQ" ] || { echo "smoke_sh_test: no mosquitto broker binary; skipping" >&2; exit 0; }

T=$(mktemp -d); PORT=$(( 20000 + RANDOM % 20000 )); HPORT=$(( PORT + 1 )); BPID="" HPID=""
cleanup() { for p in $BPID $HPID; do kill "$p" 2>/dev/null || true; done; rm -rf "$T"; }
trap cleanup EXIT
export WASHHOUSE_RECORDINGS="$T/conf"   # no broker file: only what the test gives
unset WASHHOUSE_BROKER TANKTEMP_HEALTHZ

pass=0 fail=0
expect_rc() { local want="$1" what="$2"; shift 2; local rc=0; "$@" >"$T/out" 2>&1 || rc=$?
    if [ "$rc" -eq "$want" ]; then pass=$((pass+1)); echo "ok   $what"; else fail=$((fail+1)); echo "FAIL $what (rc $rc, want $want)"; cat "$T/out"; fi; }

printf 'listener %s 127.0.0.1\nallow_anonymous true\n' "$PORT" >"$T/mosq.conf"
"$MOSQ" -c "$T/mosq.conf" >"$T/mosq.log" 2>&1 & BPID=$!
for _ in $(seq 50); do mosquitto_pub -p "$PORT" -t ping -m x 2>/dev/null && break; sleep 0.1; done
B="127.0.0.1:$PORT"

expect_rc 3 "no broker refuses" "$SMOKE"
expect_rc 3 "empty broker is 'couldn't fetch'" "$SMOKE" --broker "$B"

# publish a fresh, healthy snapshot: the premeter fixture with times moved to now
now=$(date -u +%FT%TZ)
python3 - "$HERE/testdata/smoke/premeter.retained" "$now" "$HERE/testdata/smoke/meter-pulse.golden" >"$T/fresh" <<'PY'
import json, re, sys
src, now, golden = sys.argv[1:]
for line in open(src).read().splitlines() + ["water/hot/tanktemp/meter " + open(golden).read().strip()]:
    t, _, p = line.partition(" ")
    p = re.sub(r'"timestamp":"[^"]+"', f'"timestamp":"{now}"', p)
    if t.endswith("/meter"):
        d = json.loads(p); d["counting_since"] = now; p = json.dumps(d, separators=(",", ":"))
    if t.endswith("/status"):
        d = json.loads(p); d["meters"] = ["hot_water_meter"]; p = json.dumps(d, separators=(",", ":"))
    print(t, p)
PY
while read -r t p; do mosquitto_pub -p "$PORT" -r -t "$t" -m "$p"; done <"$T/fresh"

expect_rc 0 "--check fetches nothing and passes" "$SMOKE" --check --broker "$B"
expect_rc 0 "fresh snapshot with a meter passes" "$SMOKE" --broker "$B" --expect-meter
grep -q 'PASS  meter field order' "$T/out" && { pass=$((pass+1)); echo "ok   meter checked"; } || { fail=$((fail+1)); echo "FAIL meter not checked"; }

expect_rc 1 "unreachable /healthz fails" "$SMOKE" --broker "$B" --healthz "http://127.0.0.1:$HPORT/healthz"

mkdir -p "$T/www"
python3 -c 'import json,sys; print(json.dumps({"status":"ok","ready":True,"mqtt_connected":True,"meters":[{"device_id":"hot_water_meter","state":"failed","reason":"gpio_request_failed","publish_dropped":0}]}))' >"$T/www/healthz"
(cd "$T/www" && exec python3 -m http.server "$HPORT" --bind 127.0.0.1 >/dev/null 2>&1) & HPID=$!
for _ in $(seq 50); do curl -fs "http://127.0.0.1:$HPORT/healthz" >/dev/null 2>&1 && break; sleep 0.1; done
expect_rc 1 "/healthz meter failed is a FAIL" "$SMOKE" --broker "$B" --healthz "http://127.0.0.1:$HPORT/healthz" --expect-meter --save "$T/saved"
ls "$T/saved"/smoke-*.retained >/dev/null 2>&1 && ls "$T/saved"/smoke-*.healthz.json >/dev/null 2>&1 \
    && { pass=$((pass+1)); echo "ok   --save keeps the snapshot"; } || { fail=$((fail+1)); echo "FAIL --save"; }

mosquitto_pub -p "$PORT" -r -t water/hot/tanktemp/meter -m "$(python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); d["lost"]=2; print(json.dumps(d))' "$HERE/testdata/smoke/meter-pulse.golden")"
expect_rc 1 "a meter that lost edges fails" "$SMOKE" --broker "$B" --expect-meter

echo "smoke_sh_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
