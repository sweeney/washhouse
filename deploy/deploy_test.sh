#!/usr/bin/env bash
# Tests for deploy/deploy.sh. ssh runs the "remote" command locally, with
# /opt/washhouse/bin swapped for a fake host's directory and stubs for
# sudo, systemctl, journalctl, curl and uname; git and go are stubs too.
# A restart "runs" whatever the link points at: the fake build records the
# commit it was built from, and /healthz serves it. No host, no network.
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
PATH="$FAKE_REMOTE:$PATH" bash -c "${cmd//\/opt\/washhouse\/bin/$FAKE_HOST/bin}"'
stub "$STUBS" scp "$LOGIT"'
cp "$2" "$(sed "s|^[^:]*:/opt/washhouse/bin|$FAKE_HOST/bin|" <<<"$3")"'
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
stub "$REMOTE" systemctl "$LOGIT"'
case "$1" in
    is-enabled) [ -e "$FAKE_HOST/enabled" ] ;;
    is-active) [ -s "$FAKE_HOST/running" ] ;;
    *) exit 1 ;;
esac'
stub "$REMOTE" sudo "$LOGIT"'
[ "$1" = -n ] || { echo "sudo without -n would prompt" >&2; exit 1; }
shift
case "$1" in
    -u) [ "$2" = washhouse ] && [ -x "$3" ] || exit 1
        [ "$4 $5 $6" = "-config /etc/washhouse/config.yaml -check-config" ] || exit 1
        echo "/etc/washhouse/config.yaml: ok"; exit "${FAKE_CHECK_RC:-0}" ;;
    systemctl) [ "$2 $3" = "restart washhouse" ] || exit 1
        if [ -n "${FAKE_START_FAIL:-}" ]; then : >"$FAKE_HOST/running"; exit 0; fi
        [ -z "${FAKE_RESTART_NOOP:-}" ] || exit 0 # the old process carries on
        sed -n "s/^# version=//p" "$FAKE_HOST/bin/washhouse" >"$FAKE_HOST/running" ;;
    journalctl) case "$*" in *--show-cursor*) echo "old line"; echo "-- cursor: s=1" ;;
        *) echo "washhouse: the behaviour namespace: refusing to start" ;; esac ;;
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
    mkdir -p "$FAKE_HOST/bin"
    touch "$FAKE_HOST/enabled"
    export FAKE_LOG="$FAKE_HOST/log"
    : >"$FAKE_LOG"
    unset FAKE_DIRTY FAKE_UNPUSHED FAKE_COMMIT FAKE_CHECK_RC FAKE_START_FAIL FAKE_RESTART_NOOP FAKE_PUBLIC_VERSION FAKE_ARCH DEPLOY_HOST || true
}
run() { set +e; OUT=$(PATH="$STUBS:$PATH" bash "$REPO/deploy/deploy.sh" "$@" 2>&1 <"${STDIN:-/dev/null}"); RC=$?; set -e; }
builds() { (cd "$FAKE_HOST/bin" && ls washhouse-2* 2>/dev/null) || true; }
linked() { readlink "$FAKE_HOST/bin/washhouse" 2>/dev/null || true; }
running() { cat "$FAKE_HOST/running" 2>/dev/null || true; }
# line N PATTERN: the log's first line matching PATTERN.
line() { grep -n -- "$1" "$FAKE_LOG" | head -n 1 | cut -d: -f1; }

pass=0 fail=0
ok() { pass=$((pass + 1)); }
bad() { fail=$((fail + 1)); echo "FAIL: $1"; printf '%s\n' "$OUT" | sed 's/^/    /'; }
expect() { if eval "$2"; then ok; else bad "$1"; fi; }

# --- --check: the new build checks the config as washhouse, then goes.
fresh
run --check
expect "check: exit 0" '[ $RC -eq 0 ] && [[ "$OUT" == *"--check: abc1234 would deploy; nothing switched"* ]]'
expect "check: built with the commit" 'grep -q "^go build -trimpath -ldflags -X main.version=abc1234 -o bin/washhouse-linux-amd64 ./cmd/washhouse$" "$FAKE_LOG"'
expect "check: the build checks the config as washhouse" 'grep -q "^sudo -n -u washhouse $FAKE_HOST/bin/washhouse-2[0-9T]*Z-abc1234 -config /etc/washhouse/config.yaml -check-config$" "$FAKE_LOG"'
expect "check: shows what it said" '[[ "$OUT" == *"/etc/washhouse/config.yaml: ok"* ]]'
expect "check: nothing left, nothing switched" '[ -z "$(builds)" ] && [ -z "$(linked)" ] && ! grep -q "systemctl restart" "$FAKE_LOG"'

# --- A refused config: the upload goes, nothing switches.
FAKE_CHECK_RC=1 run --yes
expect "refused: exit 1" '[ $RC -eq 1 ] && [[ "$OUT" == *"the new build refuses the host'"'"'s config; washhouse still runs nothing"* ]]'
expect "refused: nothing left, nothing switched" '[ -z "$(builds)" ] && [ -z "$(linked)" ] && ! grep -q "systemctl restart" "$FAKE_LOG"'

# --- Asked, and answered no.
echo n >"$WORK/no"
STDIN="$WORK/no" run
expect "no: nothing switched" '[ $RC -eq 1 ] && [[ "$OUT" == *"nothing changed"* ]] && [ -z "$(linked)" ] && ! grep -q "systemctl restart" "$FAKE_LOG"'

# --- A deploy: checked, then switched, restarted, and checked serving.
: >"$FAKE_LOG"
run --yes
expect "deploy: exit 0" '[ $RC -eq 0 ] && [[ "$OUT" == *"=== Deployed abc1234 ==="* ]]'
expect "deploy: linked and running" '[[ "$(linked)" == washhouse-2*Z-abc1234 ]] && [ "$(running)" = abc1234 ]'
expect "deploy: the check before the switch" '[ "$(line -check-config)" -lt "$(line "ln -sfn")" ]'
expect "deploy: the switch before the restart" '[ "$(line "^ssh .*ln -sfn")" -lt "$(line "^sudo -n systemctl restart washhouse")" ]'
expect "deploy: the host checked" '[[ "$OUT" == *"✓ active, and http://127.0.0.1:8787/healthz on the host serves abc1234"* ]]'
expect "deploy: the public URL checked" '[[ "$OUT" == *"✓ https://washhouse.example/healthz serves abc1234"* ]]'
expect "deploy: the build is executable" '[ -x "$FAKE_HOST/bin/$(linked)" ]'

# --- Refusals before anything is built or uploaded.
for c in "FAKE_DIRTY=' M x.go'|uncommitted changes" "FAKE_UNPUSHED=1|HEAD isn't on origin/main" \
    "FAKE_ARCH=aarch64|this builds linux/amd64"; do
    fresh
    : >"$FAKE_LOG"
    eval "export ${c%%|*}"
    run --yes
    want=${c#*|}
    expect "refused ($want)" '[ $RC -eq 1 ] && [[ "$OUT" == *"$want"* ]] && ! grep -q "^\(go\|scp\) " "$FAKE_LOG" && [ -z "$(builds)" ]'
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
expect "won't start: exit 1" '[ $RC -eq 1 ] && [[ "$OUT" == *"isn'"'"'t serving abc1234 on the host"* ]]'
expect "won't start: this boot's journal" '[[ "$OUT" == *"refusing to start"* ]] && [[ "$OUT" != *"old line"* ]] && grep -q "journalctl -u washhouse --after-cursor .s=1." "$FAKE_LOG"'
expect "won't start: says --rollback" '[[ "$OUT" == *"deploy/deploy.sh --rollback"* ]]'

# --- Active, but the old build still answers.
fresh
FAKE_COMMIT=c000001 run --yes
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

# --- Four deploys keep three builds; --rollback goes to the one before.
fresh
for c in c000001 c000002 c000003 c000004; do
    FAKE_COMMIT=$c run --yes
    [ $RC -eq 0 ] || bad "deploy $c"
    sleep 1 # distinct times for ls -t
done
: >"$FAKE_LOG"
expect "keeps three" '[ "$(builds | wc -l | tr -d " ")" = 3 ] && ! builds | grep -q c000001 && [[ "$(linked)" == *-c000004 ]]'
run --rollback --yes
expect "rollback: to the one before" '[ $RC -eq 0 ] && [[ "$(linked)" == *-c000003 ]] && [ "$(running)" = c000003 ] && [[ "$OUT" == *"=== Rolled back to c000003 ==="* ]]'
expect "rollback: builds nothing" '! grep -q "^go " "$FAKE_LOG"'
fresh
FAKE_COMMIT=c000001 run --yes
run --rollback --yes
expect "rollback: nothing earlier" '[ $RC -eq 1 ] && [[ "$OUT" == *"no earlier build"* ]] && [[ "$(linked)" == *-c000001 ]]'
run --check --rollback
expect "--check --rollback refused" '[ $RC -eq 1 ] && [[ "$OUT" == *"don'"'"'t go together"* ]]'

echo "deploy_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
