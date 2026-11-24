#!/usr/bin/env bash
# Tests for deploy/deploy.sh. ssh runs the "remote" command locally, with
# /opt/washhouse swapped for a fake host's directory and stubs for sudo,
# systemctl, journalctl, curl, uname and mv (GNU's -T); git and go are
# stubs too. A restart "runs" the current release: its fake build records
# the commit it was built from, and /healthz serves it. The fake
# -check-config models a config change: a build from c000002 on needs the
# key "newkey", and the builds before it refuse it. No host, no network.
#
# Usage: deploy/deploy_test.sh
# shellcheck disable=SC2034 # RC and the like are read in expect's eval strings
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
WORK=$(cd "$(mktemp -d "${TMPDIR:-/tmp}/deploy-test.XXXXXX")" && pwd)
trap 'rm -rf "$WORK"' EXIT
STUBS="$WORK/stubs" REMOTE="$WORK/remote"
mkdir -p "$STUBS" "$REMOTE"

stub() { printf '#!/usr/bin/env bash\n%s\n' "$3" >"$1/$2"; chmod +x "$1/$2"; }
LOGIT='echo "$(basename "$0") $*" >>"$FAKE_LOG"'

# The dev machine.
stub "$STUBS" git "$LOGIT"'
case "$1" in
    status) printf "%s" "${FAKE_DIRTY:-}" ;;
    fetch) ;;
    merge-base) exit "${FAKE_UNPUSHED:-0}" ;;
    rev-parse) echo "${FAKE_COMMIT:-abc1234}" ;;
    *) exit 1 ;;
esac'
stub "$STUBS" go "$LOGIT"'
v="" out=""
while [ $# -gt 0 ]; do
    case "$1" in -ldflags) v=${2##*=}; shift ;; -o) out=$2; shift ;; esac
    shift
done
printf "#!/bin/sh\n# version=%s\n" "$v" >"$out"'
stub "$STUBS" sleep ":"
stub "$STUBS" ssh "$LOGIT"'
while [ "${1#-}" != "$1" ]; do shift 2; done
shift
cmd="$*"
PATH="$FAKE_REMOTE:$PATH" bash -c "${cmd//\/opt\/washhouse/$FAKE_HOST/app}"'
stub "$STUBS" scp "$LOGIT"'
cp "$2" "$(sed "s|^[^:]*:/opt/washhouse|$FAKE_HOST/app|" <<<"$3")"'
# Both sides' curl: the host's /healthz serves what runs; the public URL
# serves FAKE_PUBLIC_VERSION if set (a stale route), else the same.
stub "$STUBS" curl "$LOGIT"'
url="${*: -1}"
v=$(cat "$FAKE_HOST/running" 2>/dev/null || true)
case "$url" in *127.0.0.1*) ;; *) v=${FAKE_PUBLIC_VERSION-$v} ;; esac
[ -n "$v" ] || exit 7
printf "{\n  \"status\": \"ok\",\n  \"version\": \"%s\"\n}\n" "$v"'

# The host.
stub "$REMOTE" uname 'echo "${FAKE_ARCH:-x86_64}"'
stub "$REMOTE" mv "$LOGIT"'
if [ "$1" = -T ]; then [ ! -d "$3" ] || [ -L "$3" ] || exit 1; rm -f "$3"; exec /bin/mv "$2" "$3"; fi
exec /bin/mv "$@"'
stub "$REMOTE" systemctl "$LOGIT"'
case "$1" in
    is-enabled) [ -e "$FAKE_HOST/enabled" ] ;;
    is-active) [ -s "$FAKE_HOST/running" ] ;;
    *) exit 1 ;;
esac'
stub "$REMOTE" journalctl "$LOGIT"'
case "$*" in *--show-cursor*) echo "old line"; echo "-- cursor: s=1" ;;
    *) echo "washhouse: the behaviour namespace: refusing to start" ;; esac'
stub "$REMOTE" sudo "$LOGIT"'
[ "$1" = -n ] || { echo "sudo without -n would prompt" >&2; exit 1; }
shift
case "$1" in
    -u) next="$FAKE_HOST/app/next"
        [ "$2 $3 $4 $5 $6" = "washhouse $next/washhouse -config $next/config.yaml -check-config" ] || exit 1
        v=$(sed -n "s/^# version=//p" "$3")
        cp "$5" "$FAKE_HOST/checked"
        if [ "$v" \< c000002 ]; then grep -q newkey "$5" && { echo "field newkey not found" >&2; exit 1; }
        else grep -q newkey "$5" || { echo "newkey is required" >&2; exit 1; }; fi
        echo "$5: ok"; exit "${FAKE_CHECK_RC:-0}" ;;
    systemctl) [ "$2 $3" = "restart washhouse" ] || exit 1
        if [ -n "${FAKE_START_FAIL:-}" ]; then : >"$FAKE_HOST/running"; exit 0; fi
        [ -z "${FAKE_RESTART_NOOP:-}" ] || exit 0 # the old process carries on
        sed -n "s/^# version=//p" "$FAKE_HOST/app/current/washhouse" >"$FAKE_HOST/running" ;;
    *) exit 1 ;;
esac'

cat >"$WORK/config.yaml" <<'EOF'
http:
  listen: 127.0.0.1:8787
  public_url: https://washhouse.example   # the spec's server
  audience: warn
EOF

# fresh: a repo copy with the script, a host set up but running nothing.
fresh() {
    REPO="$WORK/repo-$RANDOM"
    mkdir -p "$REPO/deploy"
    cp "$HERE/deploy.sh" "$REPO/deploy/"
    cp "$WORK/config.yaml" "$REPO/deploy/config.local.yaml"
    echo "deployer@host.example" >"$REPO/deploy/host"
    export FAKE_HOST="$WORK/host-$RANDOM" FAKE_REMOTE="$REMOTE"
    mkdir -p "$FAKE_HOST/app"
    touch "$FAKE_HOST/enabled"
    export FAKE_LOG="$FAKE_HOST/log"
    : >"$FAKE_LOG"
    unset FAKE_DIRTY FAKE_UNPUSHED FAKE_COMMIT FAKE_CHECK_RC FAKE_START_FAIL FAKE_RESTART_NOOP FAKE_PUBLIC_VERSION FAKE_ARCH DEPLOY_HOST || true
}
run() { set +e; OUT=$(PATH="$STUBS:$PATH" bash "$REPO/deploy/deploy.sh" "$@" 2>&1 <"${STDIN:-/dev/null}"); RC=$?; set -e; }
releases() { (cd "$FAKE_HOST/app/releases" 2>/dev/null && ls) || true; }
current() { readlink "$FAKE_HOST/app/current" 2>/dev/null || true; }
running() { cat "$FAKE_HOST/running" 2>/dev/null || true; }
# line PATTERN: the log's first line matching PATTERN.
line() { grep -n -- "$1" "$FAKE_LOG" | head -n 1 | cut -d: -f1; }
newkey() { echo "newkey: 1" >>"$REPO/deploy/config.local.yaml"; }

pass=0 fail=0
ok() { pass=$((pass + 1)); }
bad() { fail=$((fail + 1)); echo "FAIL: $1"; printf '%s\n' "$OUT" | sed 's/^/    /'; }
expect() { if eval "$2"; then ok; else bad "$1"; fi; }

# --- --check: the new build checks the new config as washhouse, staged,
# and the stage goes.
fresh
run --check
expect "check: exit 0" '[ $RC -eq 0 ] && [[ "$OUT" == *"--check: abc1234 would deploy; nothing switched"* ]]'
expect "check: built with the commit" 'grep -q "^go build -trimpath -ldflags -X main.version=abc1234 -o bin/washhouse-linux-amd64 ./cmd/washhouse$" "$FAKE_LOG"'
expect "check: the staged build checks the staged config" 'grep -q "^sudo -n -u washhouse $FAKE_HOST/app/next/washhouse -config $FAKE_HOST/app/next/config.yaml -check-config$" "$FAKE_LOG"'
expect "check: the config checked is the local one" 'cmp -s "$FAKE_HOST/checked" "$REPO/deploy/config.local.yaml"'
expect "check: shows what it said" '[[ "$OUT" == *"/config.yaml: ok"* ]]'
expect "check: nothing left, nothing switched" '[ ! -e "$FAKE_HOST/app/next" ] && [ -z "$(releases)" ] && [ -z "$(current)" ] && ! grep -q "systemctl restart" "$FAKE_LOG"'

# --- A refused config: the stage goes, nothing switches.
FAKE_CHECK_RC=1 run --yes
expect "refused: exit 1" '[ $RC -eq 1 ] && [[ "$OUT" == *"the new build refuses the new config; washhouse still runs nothing"* ]]'
expect "refused: nothing left, nothing switched" '[ ! -e "$FAKE_HOST/app/next" ] && [ -z "$(releases)" ] && [ -z "$(current)" ] && ! grep -q "systemctl restart" "$FAKE_LOG"'

# --- Asked, and answered no.
echo n >"$WORK/no"
STDIN="$WORK/no" run
expect "no: nothing switched" '[ $RC -eq 1 ] && [[ "$OUT" == *"nothing changed"* ]] && [ -z "$(current)" ] && ! grep -q "systemctl restart" "$FAKE_LOG"'

# --- A deploy: staged, checked, released, switched, restarted, checked.
: >"$FAKE_LOG"
run --yes
R1=$(current)
expect "deploy: exit 0" '[ $RC -eq 0 ] && [[ "$OUT" == *"=== Deployed abc1234 ==="* ]]'
expect "deploy: a release, current, running" '[[ "$R1" == releases/2*Z-abc1234 ]] && [ "$(running)" = abc1234 ]'
expect "deploy: the release holds the build and its config" '[ -x "$FAKE_HOST/app/$R1/washhouse" ] && cmp -s "$FAKE_HOST/app/$R1/config.yaml" "$REPO/deploy/config.local.yaml"'
expect "deploy: the check before the switch" '[ "$(line -check-config)" -lt "$(line "^ssh .*ln -sfn")" ]'
expect "deploy: the switch is a rename" 'grep -q "^mv -T $FAKE_HOST/app/current.new $FAKE_HOST/app/current$" "$FAKE_LOG"'
expect "deploy: the switch before the restart" '[ "$(line "^mv -T")" -lt "$(line "^sudo -n systemctl restart washhouse")" ]'
expect "deploy: the host checked" '[[ "$OUT" == *"✓ active, and http://127.0.0.1:8787/healthz on the host serves abc1234"* ]]'
expect "deploy: the public URL checked" '[[ "$OUT" == *"✓ https://washhouse.example/healthz serves abc1234"* ]]'
expect "deploy: the journal without sudo" '! grep -q "^sudo -n journalctl" "$FAKE_LOG"'

# --- tanktemp's deadlock: a build that needs a new key, and a config with
# it, go together; each is refused by the other's partner.
fresh
FAKE_COMMIT=c000001 run --yes
expect "pair: c000001 deployed" '[ $RC -eq 0 ] && [ "$(running)" = c000001 ]'
FAKE_COMMIT=c000002 run --yes
expect "pair: c000002 refuses the old config" '[ $RC -eq 1 ] && [[ "$OUT" == *"newkey is required"* ]] && [ "$(running)" = c000001 ]'
newkey
FAKE_COMMIT=c000001 run --yes
expect "pair: c000001 refuses the new config" '[ $RC -eq 1 ] && [[ "$OUT" == *"field newkey not found"* ]] && [ "$(running)" = c000001 ]'
sleep 1
FAKE_COMMIT=c000002 run --yes
expect "pair: c000002 with the new config" '[ $RC -eq 0 ] && [ "$(running)" = c000002 ] && grep -q newkey "$FAKE_HOST/app/$(current)/config.yaml"'
expect "pair: the config diff shown" '[[ "$OUT" == *"the config changes:"* ]] && [[ "$OUT" == *"+newkey: 1"* ]]'
run --rollback --yes
expect "pair: rollback takes the config back too" '[ $RC -eq 0 ] && [ "$(running)" = c000001 ] && ! grep -q newkey "$FAKE_HOST/app/$(current)/config.yaml"'

# --- Refusals before anything is built or staged.
for c in "FAKE_DIRTY=' M x.go'|uncommitted changes" "FAKE_UNPUSHED=1|HEAD isn't on origin/main" \
    "FAKE_ARCH=aarch64|this builds linux/amd64"; do
    fresh
    eval "export ${c%%|*}"
    run --yes
    want=${c#*|}
    expect "refused ($want)" '[ $RC -eq 1 ] && [[ "$OUT" == *"$want"* ]] && ! grep -q "^\(go\|scp\) " "$FAKE_LOG" && [ ! -e "$FAKE_HOST/app/next" ]'
done
fresh
rm "$FAKE_HOST/enabled"
run --yes
expect "refused: not bootstrapped" '[ $RC -eq 1 ] && [[ "$OUT" == *"run deploy/bootstrap.sh"* ]] && ! grep -q "^go " "$FAKE_LOG"'
fresh
rm "$REPO/deploy/host"
run --yes
expect "refused: no host" '[ $RC -eq 1 ] && [[ "$OUT" == *"put user@host in deploy/host"* ]]'
DEPLOY_HOST=deployer@host.example run --check
expect "DEPLOY_HOST instead" '[ $RC -eq 0 ] && grep -q "^ssh -o BatchMode=yes deployer@host.example uname -m$" "$FAKE_LOG"'
fresh
sed -i.bak 's/listen: .*/listen: nowhere/' "$REPO/deploy/config.local.yaml"
run --check
expect "refused: no port" '[ $RC -eq 1 ] && [[ "$OUT" == *"can'"'"'t read the port"* ]]'

# --- A build that doesn't start: the journal from the switch, and the
# way back.
fresh
FAKE_START_FAIL=1 run --yes
expect "won't start: exit 1" '[ $RC -eq 1 ] && [[ "$OUT" == *"isn'"'"'t serving abc1234 on the host after 45s"* ]]'
expect "won't start: this boot's journal" '[[ "$OUT" == *"refusing to start"* ]] && [[ "$OUT" != *"old line"* ]] && grep -q "^journalctl -u washhouse --after-cursor s=1" "$FAKE_LOG"'
expect "won't start: waits out washhouse's boot" '[ "$(grep -c "systemctl is-active" "$FAKE_LOG")" -ge 45 ]'
expect "won't start: says --rollback" '[[ "$OUT" == *"deploy/deploy.sh --rollback"* ]]'

# --- Active, but the old build still answers.
fresh
FAKE_COMMIT=c000001 run --yes
FAKE_COMMIT=c000001 FAKE_RESTART_NOOP=1 run --yes
expect "old process answers: still c000001, so ok" '[ $RC -eq 0 ]'
fresh
FAKE_COMMIT=c000001 run --yes
newkey
FAKE_COMMIT=c000002 FAKE_RESTART_NOOP=1 run --yes
expect "old build answers: exit 1" '[ $RC -eq 1 ] && [[ "$OUT" == *"isn'"'"'t serving c000002 on the host"* ]]'

# --- The route serves another build.
fresh
FAKE_PUBLIC_VERSION=0ld0ld0 run --yes
expect "stale route: exit 1" '[ $RC -eq 1 ] && [[ "$OUT" == *"doesn'"'"'t serve abc1234 (it said:"* ]] && [[ "$OUT" == *"it'"'"'s the route"* ]]'
# No public URL: no public check.
fresh
sed -i.bak '/public_url/d' "$REPO/deploy/config.local.yaml"
run --yes
expect "no public URL" '[ $RC -eq 0 ] && [[ "$OUT" == *"no http.public_url in the config: no public check"* ]]'

# --- Four deploys keep three releases; --rollback steps back through
# them, one each time, and stops at the oldest.
fresh
newkey
for c in c000002 c000003 c000004 c000005; do
    FAKE_COMMIT=$c run --yes
    [ $RC -eq 0 ] || bad "deploy $c"
    sleep 1 # distinct times for ls -t
done
expect "keeps three" '[ "$(releases | wc -l | tr -d " ")" = 3 ] && ! releases | grep -q c000002 && [[ "$(current)" == *-c000005 ]]'
: >"$FAKE_LOG"
run --rollback --yes
expect "rollback: to the one before" '[ $RC -eq 0 ] && [[ "$(current)" == *-c000004 ]] && [ "$(running)" = c000004 ] && [[ "$OUT" == *"=== Rolled back to c000004 ==="* ]]'
expect "rollback: builds nothing" '! grep -q "^go " "$FAKE_LOG"'
run --rollback --yes
expect "rollback again: one further back" '[ $RC -eq 0 ] && [[ "$(current)" == *-c000003 ]] && [ "$(running)" = c000003 ]'
run --rollback --yes
expect "rollback: nothing older" '[ $RC -eq 1 ] && [[ "$OUT" == *"no release older than"* ]] && [[ "$(current)" == *-c000003 ]]'
# A deploy after rollbacks keeps the current release.
FAKE_COMMIT=c000006 run --yes
expect "deploy after rollback" '[ $RC -eq 0 ] && [[ "$(current)" == *-c000006 ]] && [ "$(releases | wc -l | tr -d " ")" = 3 ]'
fresh
run --rollback --yes
expect "rollback: nothing current" '[ $RC -eq 1 ] && [[ "$OUT" == *"no release is current"* ]]'
run --check --rollback
expect "--check --rollback refused" '[ $RC -eq 1 ] && [[ "$OUT" == *"don'"'"'t go together"* ]]'

echo "deploy_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
