#!/usr/bin/env bash
#
# Sets washhouse up on its host, the first time and whenever the config or
# the unit changes. Run it from the dev machine:
#
#   deploy/bootstrap.sh --check   # what it would change; changes nothing
#   deploy/bootstrap.sh           # change it, after one prompt
#   deploy/bootstrap.sh --yes     # without the prompt
#
# The host is the first line of deploy/host, and the config is
# deploy/config.local.yaml (both gitignored). The script copies itself, the
# unit and the config to a temporary directory on the host and runs there
# as root (sudo asks for your password). On the host it:
#   - creates the washhouse user and its directories;
#   - writes the identity client secret to identity.client_secret_file,
#     if it isn't there (asked for, hidden, or WASHHOUSE_CLIENT_SECRET);
#   - creates influx.bucket in the influxdb container and mints a token
#     that can only write to it, into influx.token_file, if not there;
#   - installs the config, showing the diff. When a build is installed,
#     that build checks the new config first, and a refused config changes
#     nothing;
#   - installs and enables the unit, but doesn't start it: deploy.sh does,
#     with a binary;
#   - installs the deploy user's sudoers rule: restarting washhouse,
#     reading its journal, and running -check-config as washhouse;
#   - restarts washhouse, if it is running and its config or unit changed.
# A second run changes nothing.
#
# Tests: deploy/bootstrap_test.sh.
set -euo pipefail

SERVICE=washhouse
INFLUX_CONTAINER=influxdb

usage() { sed -n '3,9p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
die() { echo "bootstrap: $*" >&2; exit 1; }

CHECK="" YES="" ON_HOST=""
while [ $# -gt 0 ]; do
    case "$1" in
        --check) CHECK=1 ;;
        --yes) YES=1 ;;
        --on-host) ON_HOST="${2:?--on-host needs a directory}"; shift ;;
        -h | --help) usage ;;
        *) echo "bootstrap: unknown argument $1" >&2; usage ;;
    esac
    shift
done

# ---------------------------------------------------------------------------
# On the dev machine: copy what the host needs, and run there as root.
if [ -z "$ON_HOST" ]; then
    HERE=$(cd "$(dirname "$0")" && pwd)
    HOST="${DEPLOY_HOST:-}"
    if [ -z "$HOST" ]; then
        [ -s "$HERE/host" ] || die "no host: put user@host in deploy/host (gitignored), or set DEPLOY_HOST"
        HOST=$(head -n 1 "$HERE/host")
    fi
    [ -s "$HERE/config.local.yaml" ] || die "no deploy/config.local.yaml (gitignored): copy deploy/config.example.yaml and fill it in"
    [ -s "$HERE/$SERVICE.service" ] || die "no deploy/$SERVICE.service"
    echo "=== $HOST ==="
    DIR=$(ssh "$HOST" "mktemp -d /tmp/$SERVICE-bootstrap.XXXXXX") || die "can't reach $HOST"
    trap 'ssh "$HOST" "rm -rf $DIR" || true' EXIT
    scp -q "$HERE/bootstrap.sh" "$HERE/$SERVICE.service" "$HERE/config.local.yaml" "$HOST:$DIR/"
    flags=()
    [ -z "$CHECK" ] || flags+=(--check)
    [ -z "$YES" ] || flags+=(--yes)
    ssh -t "$HOST" "sudo bash $DIR/bootstrap.sh --on-host $DIR ${flags[*]:-}"
    exit
fi

# ---------------------------------------------------------------------------
# On the host, as root. WASHHOUSE_ROOT prefixes every path, for the tests.
trap 'echo "bootstrap: failed at line $LINENO: $BASH_COMMAND" >&2' ERR
PATH="$PATH:/usr/sbin:/sbin" # useradd, visudo, runuser, whatever sudo's path
R="${WASHHOUSE_ROOT:-}"
[ "$(id -u)" -eq 0 ] || die "run as root (deploy/bootstrap.sh does, through sudo)"
DEPLOY_USER="${SUDO_USER:-}"
[ -n "$DEPLOY_USER" ] && [ "$DEPLOY_USER" != root ] || die "run through sudo as the deploy user, so the rule names them"
CONFIG="$ON_HOST/config.local.yaml"
UNIT="$ON_HOST/$SERVICE.service"
[ -s "$CONFIG" ] || die "no config at $CONFIG"
[ -s "$UNIT" ] || die "no unit at $UNIT"
for c in systemctl journalctl useradd visudo runuser; do
    command -v "$c" >/dev/null || die "$c is missing"
done

# yaml_get SECTION KEY: a key of a block-style section of the config.
yaml_get() {
    awk -v s="$1" -v k="$2" '
        /^[^[:space:]#]/ { sec = $0; sub(/:.*/, "", sec) }
        sec == s && $0 ~ "^[[:space:]]+" k ":" {
            v = $0; sub(/^[^:]*:[[:space:]]*/, "", v); sub(/[[:space:]]+#.*$/, "", v)
            gsub(/^["\047]|["\047]$/, "", v); print v; exit
        }' "$CONFIG"
}
SECRET_FILE=$(yaml_get identity client_secret_file)
[ -n "$SECRET_FILE" ] || die "the config has no identity.client_secret_file"
INFLUX_ORG=$(yaml_get influx org)
INFLUX_BUCKET=$(yaml_get influx bucket)
TOKEN_FILE=$(yaml_get influx token_file)
if [ -n "$INFLUX_BUCKET$TOKEN_FILE" ]; then
    [ -n "$INFLUX_ORG" ] && [ -n "$INFLUX_BUCKET" ] && [ -n "$TOKEN_FILE" ] ||
        die "the config's influx section needs org, bucket and token_file"
    command -v docker >/dev/null || die "docker is missing (Influx runs in the $INFLUX_CONTAINER container)"
    [ "$(docker inspect -f '{{.State.Running}}' "$INFLUX_CONTAINER" 2>/dev/null)" = true ] ||
        die "the $INFLUX_CONTAINER container isn't running"
fi

ETC="$R/etc/$SERVICE"
BIN_DIR="$R/opt/$SERVICE/bin"
STATE_DIR="$R/var/lib/$SERVICE"
UNIT_FILE="$R/etc/systemd/system/$SERVICE.service"
SUDOERS_FILE="$R/etc/sudoers.d/$SERVICE-deploy"
SYSTEMCTL=$(command -v systemctl)
JOURNALCTL=$(command -v journalctl)

sudoers() {
    cat <<EOF
# washhouse's deploy (deploy/deploy.sh): restart it, read its journal, and
# check a new build's config as washhouse before switching to it.
$DEPLOY_USER ALL=(root) NOPASSWD: $SYSTEMCTL restart $SERVICE, $JOURNALCTL -u $SERVICE *
$DEPLOY_USER ALL=($SERVICE) NOPASSWD: /opt/$SERVICE/bin/$SERVICE-* -config /etc/$SERVICE/config.yaml -check-config
EOF
}

bucket_id() {
    docker exec "$INFLUX_CONTAINER" influx bucket list --org "$INFLUX_ORG" --name "$INFLUX_BUCKET" --json 2>/dev/null |
        sed -nE 's/.*"id": *"([a-f0-9]+)".*/\1/p' | head -n 1 || true
}

# ---------------------------------------------------------------------------
# The plan: each step that would change something, and why.
PLAN=()
plan() { PLAN+=("$1"); echo "  - $2"; }

echo "=== What would change ==="
id -u "$SERVICE" >/dev/null 2>&1 || plan user "create the $SERVICE system user"
for d in "$BIN_DIR" "$STATE_DIR" "$ETC"; do
    if [ ! -d "$d" ]; then plan dirs "create /opt/$SERVICE/bin, /var/lib/$SERVICE and /etc/$SERVICE"; break; fi
done
[ -s "$R$SECRET_FILE" ] || plan secret "write the identity client secret to $SECRET_FILE"
if [ -n "$INFLUX_BUCKET" ]; then
    [ -n "$(bucket_id)" ] || plan bucket "create the Influx bucket $INFLUX_BUCKET in $INFLUX_ORG"
    [ -s "$R$TOKEN_FILE" ] || plan token "mint a token that can only write to $INFLUX_BUCKET, into $TOKEN_FILE"
fi
CONFIG_CHANGED=""
if ! cmp -s "$CONFIG" "$ETC/config.yaml"; then
    CONFIG_CHANGED=1
    if [ -f "$ETC/config.yaml" ]; then
        plan config "replace /etc/$SERVICE/config.yaml:"
        diff -u "$ETC/config.yaml" "$CONFIG" | tail -n +3 | sed 's/^/      /' || true
    else
        plan config "install /etc/$SERVICE/config.yaml"
    fi
fi
UNIT_CHANGED=""
if ! cmp -s "$UNIT" "$UNIT_FILE"; then
    UNIT_CHANGED=1
    if [ -f "$UNIT_FILE" ]; then
        plan unit "replace the unit:"
        diff -u "$UNIT_FILE" "$UNIT" | tail -n +3 | sed 's/^/      /' || true
    else
        plan unit "install the unit"
    fi
fi
systemctl is-enabled --quiet "$SERVICE" 2>/dev/null || plan enable "enable $SERVICE (deploy.sh starts it)"
sudoers | cmp -s - "$SUDOERS_FILE" || plan sudoers "install the deploy rule for $DEPLOY_USER in /etc/sudoers.d/$SERVICE-deploy"
RUNNING=""
if systemctl is-active --quiet "$SERVICE" 2>/dev/null; then
    RUNNING=1
    [ -z "$CONFIG_CHANGED$UNIT_CHANGED" ] || plan restart "restart $SERVICE, so it runs the new config and unit"
fi

if [ ${#PLAN[@]} -eq 0 ]; then
    echo "  nothing: $SERVICE is set up"
    exit 0
fi

# The installed build checks a changed config before anything changes,
# as washhouse, with the secrets it would run with.
if [ -n "$CONFIG_CHANGED" ] && [ -x "$BIN_DIR/$SERVICE" ]; then
    if [ -s "$R$SECRET_FILE" ] && { [ -z "$INFLUX_BUCKET" ] || [ -s "$R$TOKEN_FILE" ]; }; then
        echo "=== The installed build checks the new config ==="
        tmp=$(mktemp "${TMPDIR:-/tmp}/$SERVICE-config.XXXXXX")
        cp "$CONFIG" "$tmp"
        chmod 644 "$tmp"
        if ! out=$(runuser -u "$SERVICE" -- "$BIN_DIR/$SERVICE" -config "$tmp" -check-config 2>&1); then
            rm -f "$tmp"
            printf '%s\n' "$out" | sed 's/^/  /'
            die "the installed build refuses the new config; nothing changed"
        fi
        rm -f "$tmp"
        printf '%s\n' "$out" | sed "s|$tmp|the new config|; s/^/  /"
    else
        echo "  (the installed build checks the config once its secrets are in place: run this again)"
    fi
fi

if [ -n "$CHECK" ]; then
    echo "--check: nothing changed"
    exit 0
fi
if [ -z "$YES" ]; then
    printf 'Make these changes? [y/N] '
    read -r answer
    case "$answer" in y | Y | yes) ;; *) die "nothing changed" ;; esac
fi

# ---------------------------------------------------------------------------
# Apply, in order.
has() { local s; for s in "${PLAN[@]}"; do [ "$s" != "$1" ] || return 0; done; return 1; }
secure() { chown "root:$SERVICE" "$1"; chmod 640 "$1"; }

if has user; then
    useradd --system --shell /usr/sbin/nologin --home-dir "/var/lib/$SERVICE" --no-create-home "$SERVICE"
    echo "  created the $SERVICE user"
fi
if has dirs; then
    mkdir -p "$BIN_DIR" "$STATE_DIR" "$ETC"
    chown "$DEPLOY_USER:$DEPLOY_USER" "$BIN_DIR"
    chown "$SERVICE:$SERVICE" "$STATE_DIR"; chmod 700 "$STATE_DIR"
    chown "root:$SERVICE" "$ETC"; chmod 750 "$ETC"
    echo "  created the directories"
fi
if has secret; then
    secret="${WASHHOUSE_CLIENT_SECRET:-}"
    if [ -z "$secret" ]; then
        printf 'identity client_secret for %s: ' "$(yaml_get identity client_id)"
        stty -echo 2>/dev/null || true
        read -r secret || true
        stty echo 2>/dev/null || true
        echo
    fi
    [ -n "$secret" ] || die "no client secret given; the steps before this one are done, so run this again"
    (umask 077; mkdir -p "$(dirname "$R$SECRET_FILE")"; printf '%s\n' "$secret" >"$R$SECRET_FILE")
    secure "$R$SECRET_FILE"
    echo "  wrote $SECRET_FILE"
fi
if has bucket; then
    docker exec "$INFLUX_CONTAINER" influx bucket create --org "$INFLUX_ORG" --name "$INFLUX_BUCKET" --json >/dev/null
    [ -n "$(bucket_id)" ] || die "created the bucket $INFLUX_BUCKET, but can't find it"
    echo "  created the bucket $INFLUX_BUCKET"
fi
if has token; then
    bid=$(bucket_id)
    [ -n "$bid" ] || die "no bucket $INFLUX_BUCKET in $INFLUX_ORG"
    tok=$(docker exec "$INFLUX_CONTAINER" influx auth create --org "$INFLUX_ORG" --write-bucket "$bid" \
        --description "$SERVICE: write $INFLUX_BUCKET" --json | sed -nE 's/.*"token": *"([^"]+)".*/\1/p' | head -n 1)
    [ -n "$tok" ] || die "minting the Influx token failed"
    (umask 077; mkdir -p "$(dirname "$R$TOKEN_FILE")"; printf '%s\n' "$tok" >"$R$TOKEN_FILE")
    secure "$R$TOKEN_FILE"
    echo "  minted a write-only token for $INFLUX_BUCKET into $TOKEN_FILE"
fi
if has config; then
    cp "$CONFIG" "$ETC/config.yaml.new"
    secure "$ETC/config.yaml.new"
    mv "$ETC/config.yaml.new" "$ETC/config.yaml"
    echo "  installed /etc/$SERVICE/config.yaml"
fi
if has unit; then
    mkdir -p "$(dirname "$UNIT_FILE")"
    cp "$UNIT" "$UNIT_FILE"
    chmod 644 "$UNIT_FILE"
    systemctl daemon-reload
    echo "  installed the unit"
fi
if has enable; then
    systemctl enable "$SERVICE" >/dev/null 2>&1
    echo "  enabled $SERVICE"
fi
if has sudoers; then
    tmp=$(mktemp "${TMPDIR:-/tmp}/$SERVICE-sudoers.XXXXXX")
    sudoers >"$tmp"
    visudo -cf "$tmp" >/dev/null || { rm -f "$tmp"; die "visudo refuses the rule; nothing installed"; }
    mkdir -p "$(dirname "$SUDOERS_FILE")"
    chmod 440 "$tmp"
    mv "$tmp" "$SUDOERS_FILE"
    echo "  installed /etc/sudoers.d/$SERVICE-deploy"
fi
if has restart; then
    systemctl restart "$SERVICE"
    echo "  restarted $SERVICE"
fi

echo "=== Done ==="
if [ -z "$RUNNING" ] && [ ! -x "$BIN_DIR/$SERVICE" ]; then
    echo "  No build is installed yet: deploy/deploy.sh installs one and starts it."
fi
