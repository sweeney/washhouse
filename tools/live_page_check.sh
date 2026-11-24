#!/usr/bin/env bash
# Loads tools/live.html in headless Chrome on its demo and checks the page
# works as a whole: no uncaught error, and every panel filled (the water,
# both cylinders, the services, a draw, the log). tools/live_core_test.js
# tests the logic; this catches what only a browser runs (it caught a
# demo that threw on its first line).
#
# Usage: tools/live_page_check.sh   (CHROME=path to use a given browser)
# Skips, saying so, when no Chrome or Chromium is installed.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
CHROME="${CHROME:-}"
if [ -z "$CHROME" ]; then
    for c in "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" google-chrome google-chrome-stable chromium chromium-browser; do
        if command -v "$c" >/dev/null 2>&1 || [ -x "$c" ]; then CHROME="$c"; break; fi
    done
fi
if [ -z "$CHROME" ]; then echo "live_page_check: skipped (no Chrome or Chromium)"; exit 0; fi

WORK=$(cd "$(mktemp -d "${TMPDIR:-/tmp}/live-page.XXXXXX")" && pwd)
trap 'rm -rf "$WORK"' EXIT
cp "$HERE/live-core.js" "$WORK/"
# The page, with an uncaught error written into its title, where
# --dump-dom shows it.
sed 's#<script src="live-core.js"></script>#<script>window.onerror=(m,s,l)=>{document.title="ERROR "+m+" at line "+l}</script><script src="live-core.js"></script>#' \
    "$HERE/live.html" >"$WORK/live.html"
grep -q 'window.onerror' "$WORK/live.html" || { echo "live_page_check: couldn't hook the page's errors" >&2; exit 1; }

# 30 s of virtual time: the demo's first draw (at 4 s) ends and its state
# goes out; flags keep Chrome quiet and usable in CI.
DOM=$("$CHROME" --headless=new --disable-gpu --no-sandbox --no-first-run --disable-extensions \
    --virtual-time-budget=30000 --dump-dom "file://$WORK/live.html#demo=1" 2>/dev/null)

pass=0 fail=0
check() { if grep -qE -- "$2" <<<"$DOM"; then pass=$((pass + 1)); else fail=$((fail + 1)); echo "FAIL: $1"; fi; }
count() { local n; n=$(grep -oE -- "$2" <<<"$DOM" | wc -l | tr -d ' '); if [ "$n" -ge "$3" ]; then pass=$((pass + 1)); else fail=$((fail + 1)); echo "FAIL: $1 ($n, want $3 or more)"; fi; }
check_not() { if grep -qE -- "$2" <<<"$DOM"; then fail=$((fail + 1)); echo "FAIL: $1"; else pass=$((pass + 1)); fi; }

check_not "no uncaught error" '<title>ERROR'
check "the demo is on" 'id="link"[^>]*>demo<'
check "the water to plan on is a number" 'id="uLow">[0-9]+<small>'
check "the low-water explanation is filled" 'id="lowWhy"><b>(Above|Below|Between)'
check "the threshold is drawn" 'low below 30 L'
check "the left cylinder is drawn" 'id="tank-hot_water_cylinder_left"'
check "the right cylinder is drawn" 'id="tank-hot_water_cylinder_right"'
count "each cylinder has its probes" '<circle cx="140"' 10
check "a cylinder has washhouse's state" 'class="pill (info|warn|copper|off)">(cooling|drawing|heating|unknown)'
check "washhouse's status" 'id="whVersion">demo123<'
check "the meter" 'id="meterCount">[0-9]+ L this run'
check "a draw ended, in the table" '<td>tap</td>'
count "the log has rows" 'class="row"' 10
check "draw events are explained" 'The meter.s final account'

echo "live_page_check: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
