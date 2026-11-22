#!/usr/bin/env bash
# Record everything washhouse reads from MQTT, one file per UTC day, for
# replay and post-hoc analysis. Read-only on the broker: it only subscribes.
#
# Each day's file starts with the broker's retained messages, so a day can be
# replayed on its own. Lines are mosquitto_sub's '%I %t %p': ISO time, topic,
# payload. Files go to $WASHHOUSE_RECORDINGS (default ~/washhouse-recordings)
# as live-YYYYMMDD.log; mosquitto_sub's errors go to the same name + .err.
#
# Usage: tools/record.sh [--check] [--broker HOST] [--dir DIR] start|stop|status|run
#   start     start recording in the background (no-op if already running)
#   stop      stop the background recorder
#   status    say whether it's running, where, and how fresh its file is
#   run       record in the foreground (what start runs)
#   --check   say what would happen; start or stop nothing
#   --broker  MQTT broker HOST or HOST:PORT (default $WASHHOUSE_BROKER, else the broker in
#             $WASHHOUSE_RECORDINGS/broker, one line). No host is named here.
#   --dir     where recordings go
set -euo pipefail

TOPICS=(
    'water/hot/#'                                   # tanktemp (probes, status, meter); washhouse later
    'energy/boiler/#'                               # programmer HW/CH: events + system heartbeat
    'zigbee2mqtt/climate_groundfloor_boiler-room'   # ambient for UA
    'house/state/devices/climate_groundfloor_boiler-room'
    'house/state/devices/hot_water'                 # statehouse's view, before/after its changes
    'house/state/devices/central_heating'
)

DIR="${WASHHOUSE_RECORDINGS:-$HOME/washhouse-recordings}"
BROKER="${WASHHOUSE_BROKER:-}" CHECK=0 CMD=""

die() { echo "record.sh: $*" >&2; exit 1; }
usage() { sed -n '2,/^set -euo/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'; }

while [ $# -gt 0 ]; do
    case "$1" in
        --check) CHECK=1 ;;
        --broker) [ $# -ge 2 ] || die "--broker needs a host"; BROKER="$2"; shift ;;
        --dir) [ $# -ge 2 ] || die "--dir needs a directory"; DIR="$2"; shift ;;
        -h|--help) usage; exit 0 ;;
        start|stop|status|run) [ -z "$CMD" ] || die "one command at a time"; CMD="$1" ;;
        *) die "unknown argument $1 (try --help)" ;;
    esac
    shift
done
[ -n "$CMD" ] || { usage >&2; exit 2; }

PIDFILE="$DIR/record.pid"
LOG="$DIR/record.log"

running_pid() {
    local p
    [ -f "$PIDFILE" ] || return 1
    p=$(cat "$PIDFILE")
    [[ "$p" =~ ^[0-9]+$ ]] && kill -0 "$p" 2>/dev/null && ps -o command= -p "$p" | grep -q 'record.sh' || return 1
    echo "$p"
}

need_broker() {
    if [ -z "$BROKER" ] && [ -f "$DIR/broker" ]; then
        BROKER=$(sed -e 's/#.*//' -e 's/[[:space:]]//g' "$DIR/broker" | grep -m1 . || true)
    fi
    [ -n "$BROKER" ] || die "no broker: give --broker HOST, set WASHHOUSE_BROKER, or put the host in $DIR/broker"
    PORT=1883
    case "$BROKER" in *:*) PORT="${BROKER##*:}" ;; esac
    [[ "$PORT" =~ ^[0-9]+$ ]] || die "bad port in --broker $BROKER"
    command -v mosquitto_sub >/dev/null || die "mosquitto_sub not found (brew install mosquitto)"
}

# mtime FILE: modification time in epoch seconds, GNU stat (Linux) or BSD (macOS).
mtime() { stat -c %Y "$1" 2>/dev/null || stat -f %m "$1"; }

# secs_to_utc_midnight: seconds left in today's UTC day (at least 1).
secs_to_utc_midnight() {
    local now; now=$(date -u +%s)
    echo $(( 86400 - now % 86400 ))
}

record_forever() {
    local args=() t f left rc
    SUB=""
    for t in "${TOPICS[@]}"; do args+=(-t "$t"); done
    trap '[ -z "$SUB" ] || kill "$SUB" 2>/dev/null; echo "$(date -u +%FT%TZ) stopping" >>"$LOG"; exit 0' TERM INT
    while :; do
        f="$DIR/live-$(date -u +%Y%m%d).log"
        left=$(secs_to_utc_midnight)
        echo "$(date -u +%FT%TZ) recording to $f for ${left}s" >>"$LOG"
        set +e
        mosquitto_sub -h "${BROKER%%:*}" -p "$PORT" -F '%I %t %p' -W "$left" "${args[@]}" >>"$f" 2>>"$f.err" &
        SUB=$!; wait "$SUB"; rc=$?; SUB=
        set -e
        # 27 = -W timeout: the day is over, start the next file at once.
        if [ "$rc" -ne 27 ]; then
            echo "$(date -u +%FT%TZ) mosquitto_sub exited $rc; again in 10s" >>"$LOG"
            sleep 10
        fi
    done
}

case "$CMD" in
    status)
        if p=$(running_pid); then
            f=$(ls -t "$DIR"/live-*.log 2>/dev/null | head -1)
            age=$(( $(date +%s) - $(mtime "$f") ))
            echo "recording (pid $p) to $f: $(wc -l <"$f" | tr -d ' ') lines, last write ${age}s ago"
            [ "$age" -lt 120 ] || { echo "record.sh: nothing written for ${age}s" >&2; exit 1; }
        else
            echo "not recording"; exit 3
        fi
        ;;
    stop)
        if p=$(running_pid); then
            [ "$CHECK" -eq 1 ] && { echo "would stop pid $p"; exit 0; }
            pkill -TERM -P "$p" mosquitto_sub 2>/dev/null || true
            kill -TERM "$p"; rm -f "$PIDFILE"; echo "stopped pid $p"
        else
            echo "not recording"
        fi
        ;;
    start)
        need_broker
        if p=$(running_pid); then echo "already recording (pid $p)"; exit 0; fi
        if [ "$CHECK" -eq 1 ]; then
            echo "would record ${#TOPICS[@]} topic filters from $BROKER to $DIR/live-$(date -u +%Y%m%d).log"
            printf '  %s\n' "${TOPICS[@]}"
            exit 0
        fi
        mkdir -p "$DIR"
        nohup "$0" --dir "$DIR" --broker "$BROKER" run </dev/null >>"$LOG" 2>&1 &
        echo $! >"$PIDFILE"
        sleep 2
        running_pid >/dev/null || die "recorder died at once; see $LOG"
        echo "recording (pid $(cat "$PIDFILE")) to $DIR"
        ;;
    run)
        need_broker
        [ "$CHECK" -eq 1 ] && { echo "would record from $BROKER to $DIR"; exit 0; }
        mkdir -p "$DIR"
        record_forever
        ;;
esac
