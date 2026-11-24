#!/usr/bin/env bash
# Tests for deploy/bootstrap.sh. The host's side runs against a fake root
# (WASHHOUSE_ROOT) with stubs for the commands that would change the
# system (useradd, chown, systemctl, docker, visudo, runuser); the dev
# machine's side runs with stubs for ssh and scp. No host, no root.
#
# Usage: deploy/bootstrap_test.sh
# shellcheck disable=SC2034 # S, RC and the like are read in expect's eval strings
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
WORK=$(cd "$(mktemp -d "${TMPDIR:-/tmp}/bootstrap-test.XXXXXX")" && pwd)
trap 'rm -rf "$WORK"' EXIT
STUBS="$WORK/stubs"
mkdir -p "$STUBS"

stub() { printf '#!/usr/bin/env bash\n%s\n' "$2" >"$STUBS/$1"; chmod +x "$STUBS/$1"; }
# Every stub logs its call; FAKE_STATE holds what the "system" has.
LOGIT='echo "$(basename "$0") $*" >>"$FAKE_STATE/log"'
stub id "$LOGIT"'
if [ "$1" = -u ] && [ $# -eq 1 ]; then echo "${FAKE_UID:-0}"; exit; fi
[ -e "$FAKE_STATE/user" ] && echo 999'
stub useradd "$LOGIT"'; touch "$FAKE_STATE/user"'
stub chown "$LOGIT"
stub stty ":"
stub journalctl "$LOGIT"
stub visudo "$LOGIT"'; grep -q NOPASSWD "${2:?}"'
stub runuser "$LOGIT"'; echo "check: ok"; exit "${FAKE_CHECK_RC:-0}"'
stub systemctl "$LOGIT"'
case "$1" in
    is-enabled) [ -e "$FAKE_STATE/enabled" ] ;;
    is-active) [ -e "$FAKE_STATE/active" ] ;;
    enable) touch "$FAKE_STATE/enabled" ;;
esac'
stub docker "$LOGIT"'
case "$1 $4 $5" in
    "inspect "*) echo "${FAKE_INFLUX_RUNNING:-true}" ;;
    "exec bucket list") [ -e "$FAKE_STATE/bucket" ] || { echo "Error: bucket not found" >&2; exit 1; }
        echo "[{\"id\": \"0a1b2c\", \"name\": \"washhouse\"}]" ;;
    "exec bucket create") touch "$FAKE_STATE/bucket"; echo "{\"id\": \"0a1b2c\"}" ;;
    "exec auth create") echo "{\"id\": \"9f\", \"token\": \"tok-123\"}" ;;
    *) exit 1 ;;
esac'

cat >"$WORK/config.yaml" <<'EOF'
mqtt:
  broker: tcp://broker.example:1883
  base_topic: test/water/hot
identity:
  base_url: https://id.example
  client_id: washhouse
  client_secret_file: /etc/washhouse/client-secret   # one line
site:
  id: home
influx:
  url: http://localhost:8086
  org: "home"
  bucket: washhouse
  token_file: /etc/washhouse/influx-token
heartbeat: 15m
EOF

# fresh makes an empty host: a fake root, a state, the uploaded files.
fresh() {
    export FAKE_STATE="$WORK/state-$RANDOM" WASHHOUSE_ROOT="$WORK/root-$RANDOM"
    UP="$WORK/up-$RANDOM"
    mkdir -p "$FAKE_STATE" "$WASHHOUSE_ROOT" "$UP"
    : >"$FAKE_STATE/log"
    cp "$HERE/washhouse.service" "$WORK/config.yaml" "$UP/"
    mv "$UP/config.yaml" "$UP/config.local.yaml"
    unset FAKE_CHECK_RC FAKE_INFLUX_RUNNING FAKE_UID || true
}
# host [ARGS]: run the host's side, as root through sudo.
host() {
    set +e
    OUT=$(PATH="$STUBS:$PATH" SUDO_USER="${SUDO_AS-deployer}" WASHHOUSE_CLIENT_SECRET="${SECRET-s3cret}" TMPDIR="$WORK" \
        bash "$HERE/bootstrap.sh" --on-host "$UP" "$@" 2>&1 <"${STDIN:-/dev/null}")
    RC=$?
    set -e
}
# changed: the calls that change the system, in the last run.
changed() { grep -E '^(useradd|chown|visudo|runuser) |^systemctl (enable|daemon-reload|restart)|^docker exec .*(create)' "$FAKE_STATE/log" || true; }

pass=0 fail=0
ok() { pass=$((pass + 1)); }
bad() { fail=$((fail + 1)); echo "FAIL: $1"; printf '%s\n' "$OUT" | sed 's/^/    /'; }
expect() { if eval "$2"; then ok; else bad "$1"; fi; }

# --- --check on a bare host lists everything and changes nothing.
fresh
host --check
expect "check: exit 0" '[ $RC -eq 0 ]'
for want in "create the washhouse system user" "create /opt/washhouse/bin" "write the identity client secret to /etc/washhouse/client-secret" \
    "create the Influx bucket washhouse in home" "mint a token that can only write to washhouse" "install /etc/washhouse/config.yaml" \
    "install the unit" "enable washhouse" "install the deploy rule for deployer" "--check: nothing changed"; do
    expect "check lists: $want" '[[ "$OUT" == *"$want"* ]]'
done
expect "check changes nothing" '[ -z "$(changed)" ] && [ -z "$(find "$WASHHOUSE_ROOT" -mindepth 1)" ]'

# --- --yes sets it all up.
: >"$FAKE_STATE/log"
host --yes
expect "setup: exit 0" '[ $RC -eq 0 ]'
R=$WASHHOUSE_ROOT
expect "setup: the user" '[ -e "$FAKE_STATE/user" ]'
expect "setup: the directories" '[ -d "$R/opt/washhouse/bin" ] && [ -d "$R/var/lib/washhouse" ] && [ -d "$R/etc/washhouse" ]'
expect "setup: bin owned by the deploy user" 'grep -q "^chown deployer:deployer $R/opt/washhouse/bin$" "$FAKE_STATE/log"'
expect "setup: the secret" '[ "$(cat "$R/etc/washhouse/client-secret")" = s3cret ]'
expect "setup: the secret is the service's, 640" 'grep -q "^chown root:washhouse $R/etc/washhouse/client-secret$" "$FAKE_STATE/log" && [ "$(stat -c %a "$R/etc/washhouse/client-secret" 2>/dev/null || stat -f %Lp "$R/etc/washhouse/client-secret")" = 640 ]'
expect "setup: the bucket" 'grep -q "^docker exec influxdb influx bucket create --org home --name washhouse" "$FAKE_STATE/log"'
expect "setup: a write-only token for that bucket" 'grep -q "^docker exec influxdb influx auth create --org home --write-bucket 0a1b2c " "$FAKE_STATE/log" && ! grep -q -- "--read-bucket\|--all-access" "$FAKE_STATE/log"'
expect "setup: the token" '[ "$(cat "$R/etc/washhouse/influx-token")" = tok-123 ]'
expect "setup: the config" 'cmp -s "$UP/config.local.yaml" "$R/etc/washhouse/config.yaml"'
expect "setup: the unit" 'cmp -s "$HERE/washhouse.service" "$R/etc/systemd/system/washhouse.service" && grep -q "^systemctl daemon-reload" "$FAKE_STATE/log"'
expect "setup: enabled, not started" 'grep -q "^systemctl enable washhouse" "$FAKE_STATE/log" && ! grep -q "^systemctl \(start\|restart\)" "$FAKE_STATE/log"'
S="$R/etc/sudoers.d/washhouse-deploy"
expect "setup: sudoers checked by visudo" 'grep -q "^visudo -cf " "$FAKE_STATE/log"'
expect "setup: sudoers restart and journal only" 'grep -q "^deployer ALL=(root) NOPASSWD: $STUBS/systemctl restart washhouse, $STUBS/journalctl -u washhouse \*$" "$S"'
expect "setup: sudoers -check-config as washhouse" 'grep -qF "deployer ALL=(washhouse) NOPASSWD: /opt/washhouse/bin/washhouse-* -config /etc/washhouse/config.yaml -check-config" "$S"'
expect "setup: says to deploy" '[[ "$OUT" == *"deploy/deploy.sh installs one"* ]]'

# --- A second run changes nothing.
: >"$FAKE_STATE/log"
host --yes
expect "again: nothing" '[ $RC -eq 0 ] && [[ "$OUT" == *"nothing: washhouse is set up"* ]] && [ -z "$(changed)" ]'

# --- A changed config, with a build installed and washhouse running: the
# build checks it first, then it is installed and washhouse restarted.
mkdir -p "$R/opt/washhouse/bin"; printf '#!/bin/sh\n' >"$R/opt/washhouse/bin/washhouse"; chmod +x "$R/opt/washhouse/bin/washhouse"
touch "$FAKE_STATE/active"
sed -i.bak 's|base_topic: test/water/hot|base_topic: water/hot|' "$UP/config.local.yaml"
: >"$FAKE_STATE/log"
host --check
expect "changed check: the diff" '[[ "$OUT" == *"-  base_topic: test/water/hot"* ]] && [[ "$OUT" == *"+  base_topic: water/hot"* ]]'
expect "changed check: the restart" '[[ "$OUT" == *"restart washhouse, so it runs the new config"* ]]'
expect "changed check: the build checks it" 'grep -q "^runuser -u washhouse -- $R/opt/washhouse/bin/washhouse -config .* -check-config$" "$FAKE_STATE/log"'
expect "changed check: nothing else" '[ -z "$(changed | grep -v ^runuser)" ] && grep -q "base_topic: test/water/hot" "$R/etc/washhouse/config.yaml"'

FAKE_CHECK_RC=1 host --yes
expect "refused: exit 1" '[ $RC -eq 1 ] && [[ "$OUT" == *"the installed build refuses the new config; nothing changed"* ]]'
expect "refused: the config kept" 'grep -q "base_topic: test/water/hot" "$R/etc/washhouse/config.yaml"'
expect "refused: no restart" '! grep -q "^systemctl restart" "$FAKE_STATE/log"'

# Asked, and answered no, then yes.
echo n >"$WORK/no"; echo y >"$WORK/yes"
STDIN="$WORK/no" host
expect "no: nothing changed" '[ $RC -eq 1 ] && grep -q "base_topic: test/water/hot" "$R/etc/washhouse/config.yaml"'
: >"$FAKE_STATE/log"
STDIN="$WORK/yes" host
expect "yes: installed" '[ $RC -eq 0 ] && cmp -s "$UP/config.local.yaml" "$R/etc/washhouse/config.yaml"'
expect "yes: restarted" 'grep -q "^systemctl restart washhouse" "$FAKE_STATE/log"'

# --- Refusals before anything changes.
fresh
FAKE_UID=1000 host --yes
expect "not root" '[ $RC -eq 1 ] && [[ "$OUT" == *"run as root"* ]] && [ -z "$(changed)" ]'
SUDO_AS="" host --yes
expect "not through sudo" '[ $RC -eq 1 ] && [[ "$OUT" == *"run through sudo"* ]] && [ -z "$(changed)" ]'
FAKE_INFLUX_RUNNING=false host --yes
expect "influx down" '[ $RC -eq 1 ] && [[ "$OUT" == *"the influxdb container isn'"'"'t running"* ]] && [ -z "$(changed)" ]'
sed -i.bak '/client_secret_file/d' "$UP/config.local.yaml"
host --yes
expect "no secret file in the config" '[ $RC -eq 1 ] && [[ "$OUT" == *"no identity.client_secret_file"* ]]'

# --- No secret given: the steps before it are done, and it says to rerun.
fresh
SECRET="" host --yes
expect "no secret: exit 1" '[ $RC -eq 1 ] && [[ "$OUT" == *"no client secret given"* ]]'
expect "no secret: no file" '[ ! -e "$WASHHOUSE_ROOT/etc/washhouse/client-secret" ]'
SECRET=again host --yes
expect "no secret: a rerun finishes" '[ $RC -eq 0 ] && [ "$(cat "$WASHHOUSE_ROOT/etc/washhouse/client-secret")" = again ]'

# --- The dev machine's side: it uploads the three files and runs the
# host's side as root there, with the flags, then cleans up.
LOCAL="$WORK/local/deploy"
mkdir -p "$LOCAL"
cp "$HERE/bootstrap.sh" "$HERE/washhouse.service" "$LOCAL/"
stub ssh "$LOGIT"'
case "$*" in *mktemp*) echo /tmp/washhouse-bootstrap.abc ;; esac
exit "${FAKE_SSH_RC:-0}"'
stub scp "$LOGIT"
local_run() { set +e; OUT=$(PATH="$STUBS:$PATH" bash "$LOCAL/bootstrap.sh" "$@" 2>&1 </dev/null); RC=$?; set -e; }
fresh
local_run --check
expect "local: needs a host" '[ $RC -eq 1 ] && [[ "$OUT" == *"put user@host in deploy/host"* ]] && [ ! -s "$FAKE_STATE/log" ]'
echo "deployer@host.example" >"$LOCAL/host"
local_run --check
expect "local: needs the config" '[ $RC -eq 1 ] && [[ "$OUT" == *"no deploy/config.local.yaml"* ]] && [ ! -s "$FAKE_STATE/log" ]'
cp "$WORK/config.yaml" "$LOCAL/config.local.yaml"
local_run --check --yes
expect "local: exit 0" '[ $RC -eq 0 ]'
expect "local: uploads" 'grep -q "^scp -q $LOCAL/bootstrap.sh $LOCAL/washhouse.service $LOCAL/config.local.yaml deployer@host.example:/tmp/washhouse-bootstrap.abc/$" "$FAKE_STATE/log"'
expect "local: runs it as root" 'grep -q "^ssh -t deployer@host.example sudo bash /tmp/washhouse-bootstrap.abc/bootstrap.sh --on-host /tmp/washhouse-bootstrap.abc --check --yes$" "$FAKE_STATE/log"'
expect "local: cleans up" '[ "$(tail -n 1 "$FAKE_STATE/log")" = "ssh deployer@host.example rm -rf /tmp/washhouse-bootstrap.abc" ]'
: >"$FAKE_STATE/log"
DEPLOY_HOST=other@host.example local_run
expect "local: DEPLOY_HOST, no flags" 'grep -q "^ssh -t other@host.example sudo bash /tmp/washhouse-bootstrap.abc/bootstrap.sh --on-host /tmp/washhouse-bootstrap.abc $" "$FAKE_STATE/log"'

echo "bootstrap_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
