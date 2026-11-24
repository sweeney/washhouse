#!/usr/bin/env bash
# Tests for deploy/bootstrap.sh. The host's side runs against a fake root
# (WASHHOUSE_ROOT) with stubs for the commands that would change the
# system (useradd, usermod, chown, systemctl, docker, visudo, runuser,
# sudo); the dev machine's side runs with stubs for ssh and scp. No host,
# no root.
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
case "$1" in
    -u) if [ $# -eq 1 ]; then echo "${FAKE_UID:-0}"; else [ -e "$FAKE_STATE/user" ] && echo 999; fi ;;
    -nG) echo "$2 $(cat "$FAKE_STATE/groups" 2>/dev/null)" ;;
esac'
stub useradd "$LOGIT"'; touch "$FAKE_STATE/user"'
stub usermod "$LOGIT"'; [ "$1 $2" = "-aG systemd-journal" ] && echo systemd-journal >>"$FAKE_STATE/groups"'
stub chown "$LOGIT"
stub stty ":"
stub visudo "$LOGIT"'; grep -q NOPASSWD "${2:?}"'
stub runuser "$LOGIT"'; while [ "$1" != -- ]; do shift; done; shift; "$@"'
stub sudo "$LOGIT"'; [ "$1 $2" = "-n -l" ] && exit "${FAKE_SUDO_L_RC:-0}"; exit 1'
stub systemctl "$LOGIT"'
case "$1" in
    is-enabled) [ -e "$FAKE_STATE/enabled" ] ;;
    is-active) [ -e "$FAKE_STATE/active" ] ;;
    enable) touch "$FAKE_STATE/enabled" ;;
esac'
stub docker "$LOGIT"'
case "$1 $4 $5" in
    "inspect "*) echo "${FAKE_INFLUX_RUNNING:-true}" ;;
    "exec org list") exit "${FAKE_INFLUX_AUTH_RC:-0}" ;;
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
    cp "$HERE/washhouse.service" "$UP/"
    cp "$WORK/config.yaml" "$UP/config.local.yaml"
    unset FAKE_INFLUX_RUNNING FAKE_INFLUX_AUTH_RC FAKE_SUDO_L_RC FAKE_UID || true
}
# host [ARGS]: run the host's side, as root through sudo.
host() {
    set +e
    OUT=$(PATH="$STUBS:$PATH" SUDO_USER="${SUDO_AS-deployer}" WASHHOUSE_CLIENT_SECRET="${SECRET-s3cret}" TMPDIR="$WORK" \
        bash "$HERE/bootstrap.sh" --on-host "$UP" "$@" 2>&1 <"${STDIN:-/dev/null}")
    RC=$?
    set -e
}
# changed: the calls that change the system, in the run.
changed() { grep -E '^(useradd|usermod|chown|visudo) |^systemctl (enable|daemon-reload|restart)|^docker exec .*(create)' "$FAKE_STATE/log" || true; }

pass=0 fail=0
ok() { pass=$((pass + 1)); }
bad() { fail=$((fail + 1)); echo "FAIL: $1"; printf '%s\n' "$OUT" | sed 's/^/    /'; }
expect() { if eval "$2"; then ok; else bad "$1"; fi; }

# --- --check on a bare host lists everything and changes nothing.
fresh
host --check
expect "check: exit 0" '[ $RC -eq 0 ]'
for want in "create the washhouse system user" "create /opt/washhouse, /var/lib/washhouse and /etc/washhouse" \
    "add deployer to systemd-journal" "write the identity client secret to /etc/washhouse/client-secret" \
    "create the Influx bucket washhouse in home" "mint a token that can only write to washhouse" \
    "install the unit" "enable washhouse" "install the deploy rule for deployer" "--check: nothing changed"; do
    expect "check lists: $want" '[[ "$OUT" == *"$want"* ]]'
done
expect "check: the config isn't installed" '[[ "$OUT" != *"config.yaml"* ]]'
expect "check changes nothing" '[ -z "$(changed)" ] && [ -z "$(find "$WASHHOUSE_ROOT" -mindepth 1)" ]'

# --- --yes sets it all up.
: >"$FAKE_STATE/log"
host --yes
expect "setup: exit 0" '[ $RC -eq 0 ]'
R=$WASHHOUSE_ROOT
expect "setup: the user" '[ -e "$FAKE_STATE/user" ]'
expect "setup: the directories" '[ -d "$R/opt/washhouse" ] && [ -d "$R/var/lib/washhouse" ] && [ -d "$R/etc/washhouse" ]'
expect "setup: /opt/washhouse is the deploy user's" 'grep -q "^chown deployer:deployer $R/opt/washhouse$" "$FAKE_STATE/log"'
expect "setup: the journal group" 'grep -q "^usermod -aG systemd-journal deployer$" "$FAKE_STATE/log"'
expect "setup: the secret" '[ "$(cat "$R/etc/washhouse/client-secret")" = s3cret ]'
expect "setup: the secret is the service's, 640" 'grep -q "^chown root:washhouse $R/etc/washhouse/client-secret$" "$FAKE_STATE/log" && [ "$(stat -c %a "$R/etc/washhouse/client-secret" 2>/dev/null || stat -f %Lp "$R/etc/washhouse/client-secret")" = 640 ]'
expect "setup: the bucket" 'grep -q "^docker exec influxdb influx bucket create --org home --name washhouse" "$FAKE_STATE/log"'
expect "setup: a write-only token for that bucket" 'grep -q "^docker exec influxdb influx auth create --org home --write-bucket 0a1b2c " "$FAKE_STATE/log" && ! grep -q -- "--read-bucket\|--all-access\|--write-buckets" "$FAKE_STATE/log"'
expect "setup: the token" '[ "$(cat "$R/etc/washhouse/influx-token")" = tok-123 ]'
expect "setup: no config installed" '[ ! -e "$R/etc/washhouse/config.yaml" ] && [ -z "$(find "$R/opt" -name config.yaml)" ]'
expect "setup: the unit" 'cmp -s "$HERE/washhouse.service" "$R/etc/systemd/system/washhouse.service" && grep -q "^systemctl daemon-reload" "$FAKE_STATE/log"'
expect "setup: the unit runs the current release" 'grep -q "^ExecStart=/opt/washhouse/current/washhouse -config /opt/washhouse/current/config.yaml$" "$R/etc/systemd/system/washhouse.service"'
expect "setup: enabled, not started" 'grep -q "^systemctl enable washhouse" "$FAKE_STATE/log" && ! grep -q "^systemctl \(start\|restart\)" "$FAKE_STATE/log"'
S="$R/etc/sudoers.d/washhouse-deploy"
expect "setup: sudoers checked by visudo" 'grep -q "^visudo -cf " "$FAKE_STATE/log"'
expect "setup: sudoers, the restart" 'grep -qxF "deployer ALL=(root) NOPASSWD: $STUBS/systemctl restart washhouse" "$S"'
expect "setup: sudoers, the staged check" 'grep -qxF "deployer ALL=(washhouse) NOPASSWD: /opt/washhouse/next/washhouse -config /opt/washhouse/next/config.yaml -check-config" "$S"'
expect "setup: sudoers, two rules, no wildcard, no journal" '[ "$(grep -c NOPASSWD "$S")" = 2 ] && ! grep -v "^#" "$S" | grep -q "[*?]" && ! grep -q journalctl "$S"'
expect "setup: sudo grants it, checked" 'grep -q "^sudo -n -l $STUBS/systemctl restart washhouse$" "$FAKE_STATE/log" && [[ "$OUT" == *"sudo grants deployer the restart"* ]]'
expect "setup: says to deploy" '[[ "$OUT" == *"deploy/deploy.sh installs one, with its config"* ]]'

# --- A second run changes nothing.
: >"$FAKE_STATE/log"
host --yes
expect "again: nothing" '[ $RC -eq 0 ] && [[ "$OUT" == *"nothing: washhouse is set up"* ]] && [ -z "$(changed)" ]'

# --- A changed unit, washhouse running: diffed, then installed and
# washhouse restarted; asked, and answered no first.
touch "$FAKE_STATE/active"
printf '# a change\n' >>"$UP/washhouse.service"
: >"$FAKE_STATE/log"
host --check
expect "unit check: the diff and the restart" '[[ "$OUT" == *"+# a change"* ]] && [[ "$OUT" == *"restart washhouse, so it runs the new unit"* ]] && [ -z "$(changed)" ]'
echo n >"$WORK/no"; echo y >"$WORK/yes"
STDIN="$WORK/no" host
expect "unit no: nothing changed" '[ $RC -eq 1 ] && [[ "$OUT" == *"nothing changed"* ]] && [ -z "$(changed)" ]'
STDIN="$WORK/yes" host
expect "unit yes: installed, restarted" '[ $RC -eq 0 ] && cmp -s "$UP/washhouse.service" "$R/etc/systemd/system/washhouse.service" && grep -q "^systemctl restart washhouse" "$FAKE_STATE/log"'

# --- sudo doesn't grant the rule (the host's policy overrides it).
fresh
FAKE_SUDO_L_RC=1 host --yes
expect "sudo -l refuses: exit 1" '[ $RC -eq 1 ] && [[ "$OUT" == *"sudo doesn'"'"'t grant deployer"* ]]'

# --- Refusals before anything changes.
fresh
FAKE_UID=1000 host --yes
expect "not root" '[ $RC -eq 1 ] && [[ "$OUT" == *"run as root"* ]] && [ -z "$(changed)" ]'
SUDO_AS="" host --yes
expect "not through sudo" '[ $RC -eq 1 ] && [[ "$OUT" == *"run through sudo"* ]] && [ -z "$(changed)" ]'
FAKE_INFLUX_RUNNING=false host --yes
expect "influx down" '[ $RC -eq 1 ] && [[ "$OUT" == *"the influxdb container isn'"'"'t running"* ]] && [ -z "$(changed)" ]'
FAKE_INFLUX_AUTH_RC=1 host --yes
expect "influx CLI not an operator" '[ $RC -eq 1 ] && [[ "$OUT" == *"can'"'"'t see the org home"* ]] && [ -z "$(changed)" ]'
sed -i.bak '/client_secret_file/d' "$UP/config.local.yaml"
host --yes
expect "no secret file in the config" '[ $RC -eq 1 ] && [[ "$OUT" == *"no identity.client_secret_file"* ]] && [ -z "$(changed)" ]'

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
case "$*" in *mktemp*) echo /tmp/washhouse-bootstrap.abc ;; esac'
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
