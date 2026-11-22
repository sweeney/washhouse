#!/usr/bin/env bash
# Smoke-test the live hot-water inputs: tanktemp's status, tank states and
# meter, boiler-sensor's heartbeat, and (with --healthz) tanktemp's /healthz.
# Read-only: it subscribes for retained messages and GETs /healthz, nothing
# else. Prints one PASS/WARN/FAIL line per check (see tools/smoke.py).
#
# Usage: tools/smoke.sh [--check] [--broker HOST[:PORT]] [--healthz URL] [--expect-meter] [--save DIR]
#   --check         say what it would fetch, fetch nothing
#   --broker        default $WASHHOUSE_BROKER, else ~/washhouse-recordings/broker
#   --healthz       tanktemp's /healthz URL (default $TANKTEMP_HEALTHZ, else
#                   ~/washhouse-recordings/tanktemp-healthz, else skipped)
#   --expect-meter  the meter is deployed: its absence is a FAIL
#   --save DIR      keep the fetched snapshot in DIR (for later analysis)
# Exit: 0 all pass, 1 a FAIL, 2 only WARNs, 3 couldn't fetch.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
CONF="${WASHHOUSE_RECORDINGS:-$HOME/washhouse-recordings}"
BROKER="${WASHHOUSE_BROKER:-}" HEALTHZ="${TANKTEMP_HEALTHZ:-}" CHECK=0 EXPECT=() SAVE=""

die() { echo "smoke.sh: $*" >&2; exit 3; }
usage() { sed -n '2,/^set -euo/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'; }
first_line() { sed -e 's/#.*//' -e 's/[[:space:]]//g' "$1" | grep -m1 . || true; }

while [ $# -gt 0 ]; do
    case "$1" in
        --check) CHECK=1 ;;
        --broker) [ $# -ge 2 ] || die "--broker needs a host"; BROKER="$2"; shift ;;
        --healthz) [ $# -ge 2 ] || die "--healthz needs a URL"; HEALTHZ="$2"; shift ;;
        --expect-meter) EXPECT=(--expect-meter) ;;
        --save) [ $# -ge 2 ] || die "--save needs a directory"; SAVE="$2"; shift ;;
        -h|--help) usage; exit 0 ;;
        *) die "unknown argument $1 (try --help)" ;;
    esac
    shift
done

[ -n "$BROKER" ] || { [ -f "$CONF/broker" ] && BROKER=$(first_line "$CONF/broker"); }
[ -n "$BROKER" ] || die "no broker: give --broker, set WASHHOUSE_BROKER, or write $CONF/broker"
[ -n "$HEALTHZ" ] || { [ -f "$CONF/tanktemp-healthz" ] && HEALTHZ=$(first_line "$CONF/tanktemp-healthz"); }
PORT=1883; case "$BROKER" in *:*) PORT="${BROKER##*:}" ;; esac
command -v mosquitto_sub >/dev/null || die "mosquitto_sub not found"
command -v python3 >/dev/null || die "python3 not found"

TOPICS=(water/hot/tanktemp/status 'water/hot/tanktemp/+/state' water/hot/tanktemp/meter energy/boiler/sensor/system)

if [ "$CHECK" -eq 1 ]; then
    echo "would read retained messages from ${BROKER%%:*}:$PORT:"; printf '  %s\n' "${TOPICS[@]}"
    echo "would GET ${HEALTHZ:-(no /healthz URL: skipped)}"
    exit 0
fi

T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
args=(); for t in "${TOPICS[@]}"; do args+=(-t "$t"); done
# --retained-only stops at the first non-retained message; -W bounds the wait.
rc=0
mosquitto_sub -h "${BROKER%%:*}" -p "$PORT" --retained-only -W 3 -F '%t %p' "${args[@]}" >"$T/retained" 2>"$T/err" || rc=$?
[ "$rc" -eq 0 ] || [ "$rc" -eq 27 ] || die "mosquitto_sub failed ($rc): $(cat "$T/err")"
[ -s "$T/retained" ] || die "no retained messages from ${BROKER%%:*}:$PORT"

hz=()
if [ -n "$HEALTHZ" ]; then
    if curl -fsS -m 5 "$HEALTHZ" >"$T/healthz" 2>"$T/curl.err"; then
        hz=(--healthz "$T/healthz")
    else
        echo "FAIL  /healthz: $HEALTHZ did not answer ($(tr -d '\n' <"$T/curl.err"))"
        echo '{}' >"$T/healthz"; hz=(--healthz "$T/healthz")
    fi
fi

rc=0
python3 "$HERE/smoke.py" --retained "$T/retained" "${hz[@]+"${hz[@]}"}" "${EXPECT[@]+"${EXPECT[@]}"}" || rc=$?
if [ -n "$SAVE" ]; then
    mkdir -p "$SAVE"; stamp=$(date -u +%Y%m%dT%H%M%SZ)
    cp "$T/retained" "$SAVE/smoke-$stamp.retained"
    [ -z "$HEALTHZ" ] || cp "$T/healthz" "$SAVE/smoke-$stamp.healthz.json"
fi
exit "$rc"
