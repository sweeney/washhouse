#!/usr/bin/env bash
#
# Builds washhouse at HEAD and deploys it to its host:
#
#   deploy/deploy.sh --check      # build, and have the new build check the
#                                 # host's config; switches nothing
#   deploy/deploy.sh              # deploy, after one prompt
#   deploy/deploy.sh --yes        # without the prompt
#   deploy/deploy.sh --rollback   # back to the previous build, after a prompt
#
# The host is the first line of deploy/host, or DEPLOY_HOST. The port and
# the public URL come from deploy/config.local.yaml, the config
# deploy/bootstrap.sh installed (both files gitignored); run bootstrap.sh
# first.
#
# The rollout order is enforced: HEAD must be committed and on origin/main,
# so its hash (stamped into the build, and shown on /healthz) is settled.
# The new build is uploaded beside the running one, and checks the
# installed config as washhouse (-check-config: the config, the behaviour
# namespace, the Influx token) before anything is switched. A refused
# config leaves the running build as it was. --check uploads the build
# only for that check, then removes it.
#
# After the switch, washhouse must be active, its /healthz on the host
# must show the new commit, and so must the public URL. The last three
# builds are kept for --rollback.
#
# Tests: deploy/deploy_test.sh.
set -euo pipefail

SERVICE=washhouse
DEPLOY_DIR=/opt/$SERVICE/bin
KEEP=3

usage() { sed -n '3,9p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
die() { echo "deploy: $*" >&2; exit 1; }

CHECK="" YES="" ROLLBACK=""
while [ $# -gt 0 ]; do
    case "$1" in
        --check) CHECK=1 ;;
        --yes) YES=1 ;;
        --rollback) ROLLBACK=1 ;;
        -h | --help) usage ;;
        *) echo "deploy: unknown argument $1" >&2; usage ;;
    esac
    shift
done
[ -z "$CHECK" ] || [ -z "$ROLLBACK" ] || die "--check and --rollback don't go together"

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/.." && pwd)
HOST="${DEPLOY_HOST:-}"
if [ -z "$HOST" ]; then
    [ -s "$HERE/host" ] || die "no host: put user@host in deploy/host (gitignored), or set DEPLOY_HOST"
    HOST=$(head -n 1 "$HERE/host")
fi
CONFIG="$HERE/config.local.yaml"
[ -s "$CONFIG" ] || die "no deploy/config.local.yaml: it is the config bootstrap.sh installs"

# yaml_get SECTION KEY, as bootstrap.sh's.
yaml_get() {
    awk -v s="$1" -v k="$2" '
        /^[^[:space:]#]/ { sec = $0; sub(/:.*/, "", sec) }
        sec == s && $0 ~ "^[[:space:]]+" k ":" {
            v = $0; sub(/^[^:]*:[[:space:]]*/, "", v); sub(/[[:space:]]+#.*$/, "", v)
            gsub(/^["\047]|["\047]$/, "", v); print v; exit
        }' "$CONFIG"
}
LISTEN=$(yaml_get http listen)
PORT=${LISTEN##*:}
[[ "$PORT" =~ ^[0-9]+$ ]] || die "can't read the port from http.listen ($LISTEN) in deploy/config.local.yaml"
LOCAL_HEALTH="http://127.0.0.1:$PORT/healthz"
PUBLIC_URL=$(yaml_get http public_url)
PUBLIC_HEALTH="${PUBLIC_URL:+${PUBLIC_URL%/}/healthz}"

on_host() { ssh -o BatchMode=yes "$HOST" "$@"; }
# version_of BODY: the version a /healthz body names.
version_of() { sed -nE 's/.*"version": *"([^"]*)".*/\1/p' <<<"$1" | head -n 1; }
running_version() { version_of "$(on_host "curl -fsS --max-time 5 $LOCAL_HEALTH" 2>/dev/null || true)"; }

# The journal from just before the switch, for a failure; a cursor, since
# a bare --since is read in the host's local time.
CURSOR=""
mark_journal() {
    CURSOR=$(on_host "sudo -n journalctl -u $SERVICE -n 1 --show-cursor --no-pager 2>/dev/null" | sed -n 's/^-- cursor: //p' || true)
}
journal() {
    if [ -n "$CURSOR" ]; then
        on_host "sudo -n journalctl -u $SERVICE --after-cursor '$CURSOR' --no-pager" 2>/dev/null | tail -n 30 || true
    else
        on_host "sudo -n journalctl -u $SERVICE -n 30 --no-pager" 2>/dev/null || true
    fi
}

# switch_to BUILD COMMIT: point the link at BUILD, restart, and check it
# serves COMMIT on the host and in public.
switch_to() {
    local build=$1 commit=$2 i body
    mark_journal
    on_host "ln -sfn $build $DEPLOY_DIR/$SERVICE && sudo -n systemctl restart $SERVICE"
    echo "=== Checking ==="
    for i in $(seq 1 15); do
        if on_host "systemctl is-active --quiet $SERVICE" && [ "$(running_version)" = "$commit" ]; then
            break
        fi
        [ "$i" -lt 15 ] || {
            echo "  ✗ $SERVICE isn't serving $commit on the host. Its journal:" >&2
            journal | sed 's/^/    /' >&2
            echo "  The previous build is one command away: deploy/deploy.sh --rollback" >&2
            exit 1
        }
        sleep 1
    done
    echo "  ✓ active, and $LOCAL_HEALTH on the host serves $commit"
    if [ -z "$PUBLIC_HEALTH" ]; then
        echo "  (no http.public_url in the config: no public check)"
        return
    fi
    for i in 1 2 3 4 5; do
        body=$(curl -fsS --max-time 8 "$PUBLIC_HEALTH" 2>/dev/null || true)
        if [ "$(version_of "$body")" = "$commit" ]; then
            echo "  ✓ $PUBLIC_HEALTH serves $commit"
            return
        fi
        [ "$i" -eq 5 ] || sleep 2
    done
    echo "  ✗ $PUBLIC_HEALTH doesn't serve $commit (it said: ${body:-nothing})." >&2
    echo "    The host does, so it's the route: DNS, the tunnel or the proxy." >&2
    exit 1
}

confirm() {
    [ -z "$YES" ] || return 0
    printf '%s [y/N] ' "$1"
    local answer
    read -r answer || true
    case "$answer" in y | Y | yes) ;; *) die "nothing changed" ;; esac
}

# The host has been bootstrapped, and is the platform the build is for.
echo "=== $HOST ==="
arch=$(on_host "uname -m") || die "can't reach $HOST"
[ "$arch" = x86_64 ] || die "$HOST is $arch; this builds linux/amd64"
on_host "systemctl is-enabled --quiet $SERVICE && test -w $DEPLOY_DIR" ||
    die "$SERVICE isn't set up on $HOST (or $DEPLOY_DIR isn't writable): run deploy/bootstrap.sh"
WAS=$(running_version)
echo "  running: ${WAS:-nothing}"

# ---------------------------------------------------------------------------
if [ -n "$ROLLBACK" ]; then
    current=$(on_host "readlink $DEPLOY_DIR/$SERVICE" || true)
    builds=$(on_host "cd $DEPLOY_DIR && ls -t $SERVICE-2* 2>/dev/null" || true)
    prev=$(grep -vxF "${current:-none}" <<<"$builds" | head -n 1 || true)
    [ -n "$prev" ] || die "no earlier build on $HOST to go back to"
    commit=${prev##*-}
    echo "  back to: $prev"
    confirm "Switch $SERVICE from ${WAS:-nothing} back to $commit and restart it?"
    switch_to "$prev" "$commit"
    echo "=== Rolled back to $commit ==="
    exit 0
fi

# ---------------------------------------------------------------------------
# HEAD, settled: committed and on origin/main.
cd "$REPO"
[ -z "$(git status --porcelain)" ] || die "the tree has uncommitted changes: deploy a commit"
git fetch --quiet origin main || die "can't fetch origin/main"
git merge-base --is-ancestor HEAD origin/main ||
    die "HEAD isn't on origin/main: merge and push it first, so the hash the build carries is settled"
COMMIT=$(git rev-parse --short HEAD)
BUILD="$SERVICE-$(date -u +%Y%m%dT%H%M%SZ)-$COMMIT"
echo "=== Building $COMMIT (linux/amd64) ==="
mkdir -p bin
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$COMMIT" -o "bin/$SERVICE-linux-amd64" ./cmd/$SERVICE
echo "  built bin/$SERVICE-linux-amd64"

echo "=== The new build checks the host's config ==="
scp -q "bin/$SERVICE-linux-amd64" "$HOST:$DEPLOY_DIR/$BUILD"
on_host "chmod 755 $DEPLOY_DIR/$BUILD"
if ! out=$(on_host "cd / && sudo -n -u $SERVICE $DEPLOY_DIR/$BUILD -config /etc/$SERVICE/config.yaml -check-config" 2>&1); then
    on_host "rm -f $DEPLOY_DIR/$BUILD" || true
    printf '%s\n' "$out" | sed 's/^/  /' >&2
    die "the new build refuses the host's config; $SERVICE still runs ${WAS:-nothing}"
fi
printf '%s\n' "$out" | sed 's/^/  /'

if [ -n "$CHECK" ]; then
    on_host "rm -f $DEPLOY_DIR/$BUILD"
    echo "--check: $COMMIT would deploy; nothing switched"
    exit 0
fi

confirm "Switch $SERVICE from ${WAS:-nothing} to $COMMIT and restart it?"
echo "=== Switching to $BUILD ==="
switch_to "$BUILD" "$COMMIT"

echo "=== Keeping the last $KEEP builds ==="
on_host "cd $DEPLOY_DIR && ls -t $SERVICE-2* | tail -n +$((KEEP + 1)) | xargs -r rm -f --"
echo "=== Deployed $COMMIT ==="
