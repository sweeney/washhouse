#!/usr/bin/env bash
# Run every tools/ test. Usage: tools/test.sh
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
cd "$HERE"
python3 -m unittest -q smoke_test meter_audit_test
# The Go meter consumer's parity scenarios must be what meter_audit.py says
# today: regenerate them and compare (python3 tools/meter_scenarios.py to fix).
SCEN=$(mktemp -d)
trap 'rm -rf "$SCEN"' EXIT
python3 meter_scenarios.py "$SCEN" >/dev/null
if ! diff -r "$SCEN" ../internal/meter/testdata/audit --exclude=README.md >/dev/null; then
  echo "internal/meter/testdata/audit is stale: run python3 tools/meter_scenarios.py" >&2
  exit 1
fi
# Likewise the recorded fixtures' audit reading (internal/meter's fixture parity).
python3 fixture_draws.py --check
./record_test.sh
./smoke_sh_test.sh
# The live page: its launcher, and its pure logic against the wire goldens.
./live_test.sh
node live_core_test.js "$(cd .. && go list -m -f '{{.Dir}}' github.com/sweeney/common/wire)"
./live_page_check.sh
shellcheck -S warning record.sh record_test.sh smoke.sh smoke_sh_test.sh test.sh live.sh live_test.sh live_page_check.sh
echo "tools: all tests passed"
