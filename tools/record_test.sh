#!/usr/bin/env bash
# Tests for tools/record.sh against a throwaway local mosquitto.
# Usage: tools/record_test.sh
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
REC="$HERE/record.sh"
MOSQ=$(command -v mosquitto || ls /opt/homebrew/sbin/mosquitto 2>/dev/null || true)
[ -n "$MOSQ" ] || { echo "record_test: no mosquitto broker binary; skipping" >&2; exit 0; }

T=$(mktemp -d)
PORT=$(( 20000 + RANDOM % 20000 ))
BPID=""
cleanup() {
    "$REC" --dir "$T/rec" stop >/dev/null 2>&1 || true
    [ -z "$BPID" ] || kill "$BPID" 2>/dev/null || true
    rm -rf "$T"
}
trap cleanup EXIT

pass=0 fail=0
ok()  { pass=$((pass+1)); echo "ok   $*"; }
bad() { fail=$((fail+1)); echo "FAIL $*"; }
expect() { local what="$1"; shift; if "$@"; then ok "$what"; else bad "$what"; fi; }

# No config file: with none, mosquitto 2 listens on loopback only and allows
# anonymous clients. (Ubuntu's packaged broker can't read a config under /tmp.)
"$MOSQ" -p "$PORT" >"$T/mosq.log" 2>&1 &
BPID=$!
for _ in $(seq 50); do mosquitto_pub -h 127.0.0.1 -p "$PORT" -t ping -m x 2>/dev/null && break; sleep 0.1; done
mosquitto_pub -h 127.0.0.1 -p "$PORT" -t ping -m x 2>/dev/null || { echo "test broker never came up:" >&2; cat "$T/mosq.log" >&2; exit 1; }

pub() { mosquitto_pub -h 127.0.0.1 -p "$PORT" "$@"; }

# no broker anywhere: refuses, names the fixes
out=$(env -u WASHHOUSE_BROKER "$REC" --dir "$T/nobroker" start 2>&1) && bad "start with no broker refuses" || ok "start with no broker refuses"
expect "no-broker message names --broker" grep -q -- '--broker' <<<"$out"

# --check: says what, starts nothing
out=$("$REC" --check --dir "$T/rec" --broker "127.0.0.1:$PORT" start)
expect "--check names the topics" grep -q 'water/hot/#' <<<"$out"
expect "--check starts nothing" test ! -e "$T/rec/record.pid"

# retained message before start is captured (the day file is self-contained)
pub -r -t water/hot/tanktemp/meter -m '{"seq":1}'
"$REC" --dir "$T/rec" --broker "127.0.0.1:$PORT" start >/dev/null
expect "status says recording" "$REC" --dir "$T/rec" status
out=$("$REC" --dir "$T/rec" --broker "127.0.0.1:$PORT" start)
expect "second start is a no-op" grep -q 'already recording' <<<"$out"

pub -t energy/boiler/sensor/events -m '{"boiler":{"event":"HW_OFF"}}'
pub -t house/state/devices/hot_water -m '{"id":"hot_water"}'
pub -t unrelated/topic -m 'nope'
sleep 1
f="$T/rec/live-$(date -u +%Y%m%d).log"
expect "retained meter captured" grep -q ' water/hot/tanktemp/meter {"seq":1}$' "$f"
expect "boiler event captured" grep -q ' energy/boiler/sensor/events ' "$f"
expect "statehouse device captured" grep -q ' house/state/devices/hot_water ' "$f"
expect "unrelated topic not captured" bash -c "! grep -q unrelated '$f'"
expect "lines start with an ISO time" bash -c "awk '\$1 !~ /^20[0-9][0-9]-[0-9][0-9]-[0-9][0-9]T/ {exit 1}' '$f'"

# broker restart: the recorder reconnects and keeps the same file
kill "$BPID"; wait "$BPID" 2>/dev/null || true
sleep 1
"$MOSQ" -p "$PORT" >>"$T/mosq.log" 2>&1 &
BPID=$!
for _ in $(seq 50); do pub -t ping -m x 2>/dev/null && break; sleep 0.1; done
for _ in $(seq 40); do
    pub -t water/hot/tanktemp/status -m after-restart
    grep -q after-restart "$f" && break
    sleep 0.5
done
expect "records again after a broker restart" grep -q after-restart "$f"

"$REC" --dir "$T/rec" stop >/dev/null
expect "stop stops it" bash -c "! '$REC' --dir '$T/rec' status >/dev/null"
expect "no mosquitto_sub left behind" bash -c "! pgrep -f 'mosquitto_sub -h 127.0.0.1 -p $PORT' >/dev/null"

echo "record_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
