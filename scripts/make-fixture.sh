#!/usr/bin/env bash
# Cut a privacy-shifted washhouse test fixture from a live recording.
#
#   scripts/make-fixture.sh [--check] [--yes] --from ISO --to ISO \
#       --out testdata/fixtures/NAME.log.gz INPUT.log...
#   scripts/make-fixture.sh [--check] [--yes] --slices FILE.tsv [--out-dir DIR]
#
# Keeps only the topics washhouse reads, de-duplicates retained replays, and
# shifts every timestamp so --from is 2001-01-01T00:00:00Z. Refuses to write
# if a network detail or a real date survives. --check reports what it would
# write and writes nothing; an unchanged fixture is never rewritten; replacing
# one that differs asks first (--yes skips the question).
# The TSV has real times in it: keep it in ~/washhouse-recordings, never here.
# The logic is in scripts/fixture.py (Python 3 stdlib only).
set -euo pipefail

case "$0" in
    */*) HERE=$(cd "${0%/*}" && pwd) ;;
    *) HERE=$(pwd) ;;
esac
PY="$HERE/fixture.py"

die() { echo "make-fixture: $*" >&2; exit 1; }

command -v python3 >/dev/null 2>&1 || die "python3 not found on PATH (needs Python 3.8 or later)"
python3 -c 'import sys; sys.exit(sys.version_info < (3, 8))' \
    || die "needs Python 3.8 or later, found $(python3 -V 2>&1)"
[ -f "$PY" ] || die "missing $PY"

exec python3 -I "$PY" "$@"
