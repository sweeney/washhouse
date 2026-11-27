#!/usr/bin/env bash
# Open tools/live.html, the live view of tanktemp and washhouse, pointed at
# the broker and topics in a washhouse config. The page connects over
# MQTT-over-WebSockets, so the broker needs a websockets listener
# (mosquitto: `listener 9001` + `protocol websockets`). Read-only: it
# subscribes and never publishes. Built on tanktemp's tools/live.sh.
#
# Usage: ./tools/live.sh [--check] [--demo] [--config FILE] [--ws-port N] [--low L --margin L]
#   --check     print the URL and probe the websocket port; open nothing
#   --demo      open the page on its made-up demo, ignoring the config
#   --config    the washhouse config to read (default deploy/config.local.yaml)
#   --ws-port   the broker's websocket port (default 9001)
#   --low, --margin   the namespace's low_hot_water_l and
#               low_hot_water_margin_l, to draw the threshold (they aren't
#               on the wire, so the page can't know them)
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/.." && pwd)
PAGE="$HERE/live.html"
CONFIG="$ROOT/deploy/config.local.yaml"
WS_PORT=9001 CHECK=0 DEMO=0 LOW="" MARGIN=""

die() { echo "live.sh: $*" >&2; exit 1; }
usage() { sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//'; }
litres() { [[ "$2" =~ ^[0-9]+([.][0-9]+)?$ ]] || die "$1 must be a number of litres, got '$2'"; }

while [ $# -gt 0 ]; do
    case "$1" in
        --check) CHECK=1 ;;
        --demo) DEMO=1 ;;
        --config) [ $# -ge 2 ] || die "--config needs a file"; CONFIG="$2"; shift ;;
        --ws-port) [ $# -ge 2 ] || die "--ws-port needs a number"; WS_PORT="$2"; shift ;;
        --low) [ $# -ge 2 ] || die "--low needs a number"; litres --low "$2"; LOW="$2"; shift ;;
        --margin) [ $# -ge 2 ] || die "--margin needs a number"; litres --margin "$2"; MARGIN="$2"; shift ;;
        -h|--help) usage; exit 0 ;;
        *) die "unknown option $1 (try --help)" ;;
    esac
    shift
done

[[ "$WS_PORT" =~ ^[0-9]+$ ]] || die "--ws-port must be a number, got '$WS_PORT'"
[ -z "$MARGIN" ] || [ -n "$LOW" ] || die "--margin needs --low"
[ -f "$PAGE" ] || die "missing $PAGE"
[ -f "$HERE/live-core.js" ] || die "missing $HERE/live-core.js"

THRESHOLD="${LOW:+&low=$LOW}${MARGIN:+&margin=$MARGIN}"
if [ "$DEMO" -eq 1 ]; then
    URL="file://$PAGE#demo=1"
else
    [ -f "$CONFIG" ] || die "no config at $CONFIG (pass --config, or --demo)"
    # A key of the mqtt section, block style (washhouse's config is), with
    # comments and quotes stripped.
    mqtt_key() {
        awk -v k="$1" '
            /^[^[:space:]#]/ { sec = $0; sub(/:.*/, "", sec) }
            sec == "mqtt" && $0 ~ "^[[:space:]]+" k ":" {
                v = $0; sub(/^[^:]*:[[:space:]]*/, "", v); sub(/[[:space:]]+#.*$/, "", v)
                gsub(/^["\047]|["\047]$/, "", v); print v; exit
            }' "$CONFIG"
    }
    broker=$(mqtt_key broker)
    base=$(mqtt_key base_topic)
    tt=$(mqtt_key tanktemp_topic)
    [ -n "$broker" ] || die "no mqtt.broker in $CONFIG"
    [ -n "$base" ] || die "no mqtt.base_topic in $CONFIG"
    tt=${tt:-water/hot/tanktemp}
    host=$(sed -E 's#^[a-z]+://##; s#:[0-9]+$##' <<<"$broker")
    [ -n "$host" ] || die "can't read a host from broker '$broker'"
    URL="file://$PAGE#ws=ws://$host:$WS_PORT&base=$base&tt=$tt$THRESHOLD"
fi

if [ "$CHECK" -eq 1 ]; then
    echo "$URL"
    if [ "$DEMO" -eq 0 ]; then
        if nc -z -G 3 "$host" "$WS_PORT" >/dev/null 2>&1 || nc -z -w 3 "$host" "$WS_PORT" >/dev/null 2>&1; then
            echo "ok: $host:$WS_PORT is accepting connections"
        else
            die "$host:$WS_PORT isn't reachable; is the broker's websockets listener on?"
        fi
        [ -n "$LOW" ] || echo "note: no --low/--margin, so the page won't draw the low-water threshold"
    fi
    exit 0
fi

# macOS `open` drops the #fragment from a file:// URL, which carries the
# broker, so open a launcher page that redirects to the full URL.
LAUNCHER="${TMPDIR:-/tmp}"
LAUNCHER="${LAUNCHER%/}/washhouse-live-open.html"
printf '<!doctype html><meta charset="utf-8"><title>washhouse live</title><script>location.replace(%s)</script><a href="%s">Open washhouse live</a>\n' \
    "\"$URL\"" "$URL" >"$LAUNCHER"
echo "Opening $URL"
if command -v open >/dev/null 2>&1; then open "$LAUNCHER"
elif command -v xdg-open >/dev/null 2>&1; then xdg-open "$LAUNCHER"
else echo "Open this in a browser: $URL"; fi
