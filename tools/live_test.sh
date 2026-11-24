#!/usr/bin/env bash
# shellcheck disable=SC2016 # the stubs below expand their own $ variables
# Tests for tools/live.sh, against fixture configs and stub nc/open (after
# tanktemp's tools/live_test.sh). No network, no browser. The page's own
# logic is tested by tools/live_core_test.js.
#
# Usage: ./tools/live_test.sh
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
WORK=$(cd "$(mktemp -d "${TMPDIR:-/tmp}/live-test.XXXXXX")" && pwd)
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/bin"
# nc succeeds unless NC_FAIL is set; open records its argument.
printf '#!/usr/bin/env bash\n[ -z "${NC_FAIL:-}" ]\n' >"$WORK/bin/nc"
printf '#!/usr/bin/env bash\necho "$1" >"%s/opened"\n' "$WORK" >"$WORK/bin/open"
chmod +x "$WORK/bin/"*

pass=0 fail=0
check() {
    local name="$1"; shift
    if "$@"; then pass=$((pass + 1)); else fail=$((fail + 1)); echo "FAIL: $name"; echo "  output: $OUT"; fi
}
run() { set +e; OUT=$(PATH="$WORK/bin:$PATH" "$HERE/live.sh" "$@" 2>&1); RC=$?; set -e; }
says() { grep -qE -- "$1" <<<"$OUT"; }
rc_is() { [ "$RC" -eq "$1" ]; }
not() { ! "$@"; }

cat >"$WORK/config.yaml" <<'EOF'
mqtt:
  broker: tcp://broker.example:1883
  client_id: washhouse   # optional
  base_topic: test/water/hot
  tanktemp_topic: "water/hot/tanktemp"
  boiler_topic: energy/boiler/sensor
http:
  listen: 127.0.0.1:8787
EOF
cat >"$WORK/commented.yaml" <<'EOF'
mqtt:
  broker: tcp://192.0.2.9:1883
  # base_topic: test/water/hot
  base_topic: water/hot   # the live tree
EOF
cat >"$WORK/notopic.yaml" <<'EOF'
mqtt:
  broker: tcp://broker.example:1883
http:
  base_topic: not/this/one
EOF
printf 'http:\n  listen: x\n' >"$WORK/nobroker.yaml"

run --check --config "$WORK/config.yaml"
check "config: ok" rc_is 0
check "config: the url" says "live.html#ws=ws://broker.example:9001&base=test/water/hot&tt=water/hot/tanktemp$"
check "config: probe ok" says "^ok: broker.example:9001"
check "config: says the threshold isn't drawn" says "won't draw the low-water threshold"

run --check --config "$WORK/config.yaml" --low 30 --margin 10
check "the threshold goes in the hash" says "&tt=water/hot/tanktemp&low=30&margin=10$"
check "the threshold: no note" not says "won't draw"
run --check --config "$WORK/config.yaml" --low 22.5
check "a threshold alone, a decimal" says "&low=22.5$"

run --check --config "$WORK/commented.yaml" --ws-port 8083
check "a commented-out key is ignored, a trailing comment stripped" says "#ws=ws://192.0.2.9:8083&base=water/hot&tt=water/hot/tanktemp$"

run --check --config "$WORK/notopic.yaml"
check "base_topic only from the mqtt section" rc_is 1
check "no base_topic says so" says "no mqtt.base_topic"

NC_FAIL=1 run --check --config "$WORK/config.yaml"
check "unreachable port fails" rc_is 1
check "unreachable port says why" says "websockets listener"

run --check --config "$WORK/nobroker.yaml"
check "no broker fails" rc_is 1
check "no broker says so" says "no mqtt.broker"

run --check --config "$WORK/missing.yaml"
check "missing config fails" rc_is 1

run --ws-port abc --config "$WORK/config.yaml"
check "bad port refused" rc_is 1
run --check --config "$WORK/config.yaml" --low lots
check "a bad threshold refused" rc_is 1
run --check --config "$WORK/config.yaml" --margin 10
check "a margin without a threshold refused" rc_is 1

run --bogus
check "unknown option refused" rc_is 1

run --check --demo --config "$WORK/missing.yaml"
check "demo needs no config" rc_is 0
check "demo url" says "live.html#demo=1$"

# macOS `open` drops the #fragment from a file:// URL, so live.sh opens a
# small launcher page that redirects to the full URL.
rm -f "$WORK/opened"
TMPDIR="$WORK" run --config "$WORK/config.yaml" --low 30 --margin 10
check "opens a launcher, not the bare URL" not grep -q "#" "$WORK/opened"
check "the launcher redirects to the full URL" grep -q "live.html#ws=ws://broker.example:9001&base=test/water/hot&tt=water/hot/tanktemp&low=30&margin=10" "$(cat "$WORK/opened" 2>/dev/null)"

rm -f "$WORK/opened"
run --check --config "$WORK/config.yaml"
check "--check opens nothing" test ! -e "$WORK/opened"

run --help
check "help" says "Usage: ./tools/live.sh"

echo "live_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
