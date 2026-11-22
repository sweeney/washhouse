#!/usr/bin/env bash
# Run every tools/ test. Usage: tools/test.sh
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
cd "$HERE"
python3 -m unittest -q smoke_test meter_audit_test
./record_test.sh
./smoke_sh_test.sh
shellcheck -S warning record.sh record_test.sh smoke.sh smoke_sh_test.sh test.sh
echo "tools: all tests passed"
