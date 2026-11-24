#!/usr/bin/env bash
#
# Builds washhouse at HEAD and deploys it, with its config, to its host:
#
#   deploy/deploy.sh --check      # build, stage, and have the new build
#                                 # check the new config; switches nothing
#   deploy/deploy.sh              # deploy, after one prompt
#   deploy/deploy.sh --yes        # without the prompt
#   deploy/deploy.sh --rollback   # back one release, after a prompt
#
# The host is the first line of deploy/host, or DEPLOY_HOST; the config is
# deploy/config.local.yaml (both gitignored). Run deploy/bootstrap.sh
# first: it sets up the user, the secrets, the unit and the sudo rule.
#
# A release is a build and its config, together: /opt/washhouse/releases/
# <time>-<commit>/{washhouse,config.yaml}, and /opt/washhouse/current links
# to the one that runs. A config change is deployed like a code change, so
# the build that checks a config is always the build that runs it, and a
# rollback takes both back.
#
# The rollout order is enforced: HEAD must be committed and on origin/main,
# so its hash (stamped into the build, and shown on /healthz) is settled.
# The release is staged in /opt/washhouse/next, and the new build checks
# the new config there as washhouse (-check-config: the config, the
# behaviour namespace, the Influx token) before anything is switched. A
# refused one leaves the running release as it was. --check stops there,
# and removes what it staged.
#
# After the switch, washhouse must be active, and its /healthz on the host
# and at the public URL must show the new commit. Three releases are kept.
#
# Tests: deploy/deploy_test.sh.
set -euo pipefail

SERVICE=washhouse
APP=/opt/$SERVICE
NEXT=$APP/next
RELEASES=$APP/releases
KEEP=3
# Longer than washhouse's own wait for its behaviour namespace (bootWait,
# 30 s), plus a restart.
WAIT=45

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
[ -s "$CONFIG" ] || die "no deploy/config.local.yaml: the config each release carries"

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
# a bare --since is read in the host's local time. The deploy user reads
# it as a member of systemd-journal (bootstrap.sh), not through sudo.
CURSOR=""
mark_journal() {
    CURSOR=$(on_host "journalctl -u $SERVICE -n 1 --show-cursor --no-pager 2>/dev/null" | sed -n 's/^-- cursor: //p' || true)
}
journal() {
    if [ -n "$CURSOR" ]; then
        on_host "journalctl -u $SERVICE --after-cursor '$CURSOR' --no-pager" 2>/dev/null | tail -n 30 || true
    else
        on_host "journalctl -u $SERVICE -n 30 --no-pager" 2>/dev/null || true
    fi
}

# switch_to RELEASE COMMIT: point current at RELEASE (a new link renamed
# over the old, so there is never no link), restart, and check it serves
# COMMIT on the host and in public.
switch_to() {
    local release=$1 commit=$2 i body
    mark_journal
    on_host "ln -sfn releases/$release $APP/current.new && mv -T $APP/current.new $APP/current && sudo -n systemctl restart $SERVICE"
    echo "=== Checking ==="
    for i in $(seq 1 "$WAIT"); do
        if on_host "systemctl is-active --quiet $SERVICE" && [ "$(running_version)" = "$commit" ]; then
            break
        fi
        [ "$i" -lt "$WAIT" ] || {
            echo "  ✗ $SERVICE isn't serving $commit on the host after ${WAIT}s. Its journal:" >&2
            journal | sed 's/^/    /' >&2
            echo "  The release before is one command away: deploy/deploy.sh --rollback" >&2
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
on_host "systemctl is-enabled --quiet $SERVICE && test -w $APP" ||
    die "$SERVICE isn't set up on $HOST (or $APP isn't writable): run deploy/bootstrap.sh"
WAS=$(running_version)
CURRENT=$(on_host "readlink $APP/current" 2>/dev/null || true)
CURRENT=${CURRENT#releases/}
echo "  running: ${WAS:-nothing}${CURRENT:+ ($CURRENT)}"

# ---------------------------------------------------------------------------
if [ -n "$ROLLBACK" ]; then
    [ -n "$CURRENT" ] || die "no release is current on $HOST"
    # The release just older than the current one, so rollbacks step back.
    prev=$(on_host "cd $RELEASES && ls -t" | sed -n "/^$CURRENT\$/{n;p;q;}" || true)
    [ -n "$prev" ] || die "no release older than $CURRENT on $HOST to go back to"
    commit=${prev##*-}
    echo "  back to: $prev"
    confirm "Switch $SERVICE from ${WAS:-nothing} back to $commit, with its config, and restart it?"
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
RELEASE="$(date -u +%Y%m%dT%H%M%SZ)-$COMMIT"
echo "=== Building $COMMIT (linux/amd64) ==="
mkdir -p bin
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$COMMIT" -o "bin/$SERVICE-linux-amd64" ./cmd/$SERVICE
echo "  built bin/$SERVICE-linux-amd64"

echo "=== Staging it, with deploy/config.local.yaml ==="
on_host "rm -rf $NEXT && mkdir $NEXT"
scp -q "bin/$SERVICE-linux-amd64" "$HOST:$NEXT/$SERVICE"
scp -q "$CONFIG" "$HOST:$NEXT/config.yaml"
on_host "chmod 755 $NEXT $NEXT/$SERVICE && chmod 644 $NEXT/config.yaml"
if [ -n "$CURRENT" ]; then
    d=$(on_host "diff -u $APP/current/config.yaml $NEXT/config.yaml" || true)
    if [ -n "$d" ]; then
        echo "  the config changes:"
        tail -n +3 <<<"$d" | sed 's/^/    /'
    else
        echo "  the config is the running release's"
    fi
fi

echo "=== The new build checks the new config ==="
if ! out=$(on_host "cd / && sudo -n -u $SERVICE $NEXT/$SERVICE -config $NEXT/config.yaml -check-config" 2>&1); then
    on_host "rm -rf $NEXT" || true
    printf '%s\n' "$out" | sed 's/^/  /' >&2
    die "the new build refuses the new config; $SERVICE still runs ${WAS:-nothing}"
fi
printf '%s\n' "$out" | sed 's/^/  /'

if [ -n "$CHECK" ]; then
    on_host "rm -rf $NEXT"
    echo "--check: $COMMIT would deploy; nothing switched"
    exit 0
fi

confirm "Switch $SERVICE from ${WAS:-nothing} to $COMMIT and restart it?"
echo "=== Switching to $RELEASE ==="
on_host "mkdir -p $RELEASES && mv $NEXT $RELEASES/$RELEASE"
switch_to "$RELEASE" "$COMMIT"

echo "=== Keeping $KEEP releases ===" # the newest is the one just switched to
on_host "cd $RELEASES && ls -t | tail -n +$((KEEP + 1)) | xargs -r rm -rf --"
echo "=== Deployed $COMMIT ==="
