#!/usr/bin/env bash
# net-sim installer.
#
# Run as root on a Debian/Ubuntu host:
#
#   curl -fsSL https://raw.githubusercontent.com/packethacking/net-sim/main/install.sh | sudo bash
#
# Or, with overrides:
#
#   curl -fsSL https://raw.githubusercontent.com/packethacking/net-sim/main/install.sh | \
#     sudo SIM_DIR=/opt/sim SAMOYED_DIR=/opt/samoyed bash
#
# Idempotent — safe to re-run. Doesn't install pulseaudio / pipewire /
# jackd / any sound daemon (that's the whole point of net-sim).
set -euo pipefail

SIM_REPO="${SIM_REPO:-https://github.com/packethacking/net-sim.git}"
SIM_REF="${SIM_REF:-main}"
SIM_DIR="${SIM_DIR:-/opt/sim}"

SAMOYED_REPO="${SAMOYED_REPO:-https://github.com/doismellburning/samoyed.git}"
SAMOYED_REF="${SAMOYED_REF:-main}"
SAMOYED_DIR="${SAMOYED_DIR:-/opt/samoyed}"

WEB_PORT="${WEB_PORT:-8080}"
NETWORK_YAML="${NETWORK_YAML:-/etc/sim/network.yaml}"
SYSTEMD="${SYSTEMD:-1}"   # set to 0 to skip the sim-web systemd unit

step() { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }
note() { printf '   %s\n' "$*"; }
fail() { printf '\n\033[1;31m!! %s\033[0m\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || fail "run as root (sudo)."

if ! command -v apt-get >/dev/null 2>&1; then
    fail "this installer assumes apt (Debian/Ubuntu). Adapt manually for other distros."
fi

# sync_repo <dir> <repo_url> <ref> [extra git clone args...]
# Idempotent: clones into a fresh dir, fetch+reset on an existing checkout,
# bails out cleanly if the directory exists but isn't a checkout (likely
# someone hand-provisioned it — don't clobber their work).
sync_repo() {
    local dir="$1" url="$2" ref="$3"; shift 3
    if [ -d "$dir/.git" ]; then
        git -C "$dir" remote set-url origin "$url" 2>/dev/null || true
        git -C "$dir" fetch "$@" origin "$ref"
        git -C "$dir" reset --hard FETCH_HEAD
    elif [ -d "$dir" ] && [ -n "$(ls -A "$dir" 2>/dev/null)" ]; then
        note "$dir exists and is not a git checkout — leaving it as-is."
        note "Remove it (or set SIM_DIR / SAMOYED_DIR to a different path) to let the installer manage it."
    else
        git clone "$@" --branch "$ref" "$url" "$dir"
    fi
}

step "Installing apt dependencies"
DEBIAN_FRONTEND=noninteractive apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
    build-essential git make pkg-config ca-certificates curl \
    libudev-dev libhamlib-dev portaudio19-dev libavahi-client-dev \
    libbsd-dev libgps-dev libasound2-dev \
    socat tcpdump yamllint \
    golang
note "go: $(go version 2>&1 | head -1)"

step "Cloning / updating samoyed → $SAMOYED_DIR"
sync_repo "$SAMOYED_DIR" "$SAMOYED_REPO" "$SAMOYED_REF" --depth 1
note "samoyed: $(git -C "$SAMOYED_DIR" rev-parse --short HEAD)"

step "Building samoyed"
make -C "$SAMOYED_DIR" cmds >/dev/null
note "built: $SAMOYED_DIR/dist/samoyed-direwolf"

step "Cloning / updating net-sim → $SIM_DIR"
sync_repo "$SIM_DIR" "$SIM_REPO" "$SIM_REF"
note "net-sim: $(git -C "$SIM_DIR" rev-parse --short HEAD)"

step "Building libpa_stub.so"
gcc -shared -fPIC -o /usr/local/lib/libpa_stub.so "$SIM_DIR/preload/pa_stub.c"
ldconfig
note "installed: /usr/local/lib/libpa_stub.so"

step "Building sim-router and sim-web"
( cd "$SIM_DIR" && go build -o sim-router ./cmd/sim-router && go build -o sim-web ./cmd/sim-web )
install -m 0755 "$SIM_DIR/sim-router" /usr/local/bin/sim-router
install -m 0755 "$SIM_DIR/sim-web"    /usr/local/bin/sim-web
note "installed: /usr/local/bin/sim-router, /usr/local/bin/sim-web"

step "Bootstrapping default network at $NETWORK_YAML"
mkdir -p "$(dirname "$NETWORK_YAML")"
if [ ! -e "$NETWORK_YAML" ]; then
    cp "$SIM_DIR/configs/two-node.yaml" "$NETWORK_YAML"
    note "wrote default two-node config (existing file would have been preserved)"
else
    note "existing config preserved"
fi

if [ "$SYSTEMD" = "1" ] && [ -d /run/systemd/system ]; then
    step "Installing sim-web systemd service"
    cat > /etc/systemd/system/sim-web.service <<UNIT
[Unit]
Description=net-sim web UI and embedded router
Documentation=https://github.com/packethacking/net-sim
After=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/sim-web -addr :${WEB_PORT} -config ${NETWORK_YAML}
Restart=on-failure
RestartSec=2
# sim-web's children (samoyed-direwolf) need the LD_PRELOAD shim to
# bypass PortAudio init in headless environments.
Environment=LD_PRELOAD=/usr/local/lib/libpa_stub.so

[Install]
WantedBy=multi-user.target
UNIT
    systemctl daemon-reload
    systemctl enable --now sim-web.service
    sleep 1
    if systemctl is-active --quiet sim-web.service; then
        note "sim-web running on http://0.0.0.0:${WEB_PORT}/"
    else
        note "sim-web service installed but not active — check 'journalctl -u sim-web'"
    fi
else
    note "skipping systemd unit (SYSTEMD=0 or systemd not detected)"
fi

printf '\n\033[1;32mDone.\033[0m\n\n'
cat <<DONE
Next steps:
  • Open the web UI:        http://<this-host>:${WEB_PORT}/
  • Edit the network:       $NETWORK_YAML
  • Or run the CLI directly: sim-router -config $NETWORK_YAML
  • Stop the web service:   systemctl stop sim-web
  • Logs:                   journalctl -u sim-web -f

Read $SIM_DIR/README.md and $SIM_DIR/NOTES-audio-io.md for the full story.
DONE
