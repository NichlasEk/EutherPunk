#!/usr/bin/env sh
set -eu

HOST="${EUTHERPUNK_DEPLOY_HOST:-nichlas@192.168.32.186}"
SSH_CONFIG="${EUTHERPUNK_SSH_CONFIG:--F /dev/null}"
KEY="${EUTHERPUNK_SSH_KEY:-/home/nichlas/.ssh/euther_server}"
REMOTE_ROOT="${EUTHERPUNK_REMOTE_ROOT:-/home/nichlas/EutherPunk}"
REMOTE_CONFIG_DIR="${EUTHERPUNK_REMOTE_CONFIG_DIR:-/home/nichlas/.config/eutherpunk}"
SERVICE_DIR="${EUTHERPUNK_SERVICE_DIR:-/home/nichlas/.config/systemd/user}"

ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"

ssh_cmd() {
  # shellcheck disable=SC2086
  ssh $SSH_CONFIG -o IdentitiesOnly=yes -i "$KEY" "$HOST" "$@"
}

scp_to() {
  # shellcheck disable=SC2086
  scp $SSH_CONFIG -o IdentitiesOnly=yes -i "$KEY" "$1" "$HOST:$2"
}

if [ ! -x "$ROOT/dist/cli/eutherpunkd-linux-amd64" ]; then
  "$ROOT/scripts/build.sh"
fi

ssh_cmd "mkdir -p '$REMOTE_ROOT/bin' '$REMOTE_ROOT/dist/cli' '$REMOTE_CONFIG_DIR' '$SERVICE_DIR'"

scp_to "$ROOT/dist/cli/eutherpunk-linux-amd64" "$REMOTE_ROOT/dist/cli/eutherpunk-linux-amd64"
scp_to "$ROOT/dist/cli/eutherpunkd-linux-amd64" "$REMOTE_ROOT/bin/eutherpunkd"
scp_to "$ROOT/deploy/eutherpunk.server.toml" "$REMOTE_CONFIG_DIR/config.toml"
scp_to "$ROOT/deploy/eutherpunkd.service" "$SERVICE_DIR/eutherpunkd.service"

ssh_cmd "chmod +x '$REMOTE_ROOT/bin/eutherpunkd' '$REMOTE_ROOT/dist/cli/eutherpunk-linux-amd64'"
ssh_cmd "systemctl --user daemon-reload && systemctl --user enable --now eutherpunkd.service"
ssh_cmd "systemctl --user --no-pager status eutherpunkd.service"
ssh_cmd "curl -fsS http://127.0.0.1:8787/api/eutherpunk/status"
