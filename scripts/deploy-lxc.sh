#!/usr/bin/env bash
# Deploys the AirDash service + web UI to the Proxmox LXC created by
# scripts/pve/create-lxc.py: rsyncs the code and a generated .env, then runs
# docker compose there. Re-runnable. --sync-settings (first deploy only)
# overwrites the LXC's settings with the local service's, if it is running.
# The target is AIRDASH_LXC_IP (environment, else ~/.airdash-proxmox.env);
# --print-target prints the resolved address and exits.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
REMOTE_DIR="/opt/airdash"
SYNC_SETTINGS=0
PRINT_TARGET=0
for a in "$@"; do
  case "$a" in
    --sync-settings) SYNC_SETTINGS=1 ;;
    --print-target) PRINT_TARGET=1 ;;
    -h|--help) sed -n '2,7p' "$0"; exit 0 ;;
    *) echo "unknown argument: $a" >&2; exit 2 ;;
  esac
done

# Target: AIRDASH_LXC_IP from the environment, else the same key
# create-lxc.py reads from the Proxmox env file (CIDR form there; the
# prefix and any CR/whitespace are stripped).
ENV_FILE="${AIRDASH_PROXMOX_ENV:-$HOME/.airdash-proxmox.env}"
IP="${AIRDASH_LXC_IP:-}"
if [ -z "$IP" ] && [ -f "$ENV_FILE" ]; then
  IP="$(sed -n 's/^AIRDASH_LXC_IP=//p' "$ENV_FILE" | tail -n 1)"
fi
IP="$(printf '%s' "$IP" | tr -d '[:space:]')"
IP="${IP%%/*}"
[ -n "$IP" ] || { echo "no deploy target: set AIRDASH_LXC_IP in the environment or in $ENV_FILE" >&2; exit 1; }
[[ "$IP" =~ ^[0-9A-Fa-f:.]+$ ]] || { echo "AIRDASH_LXC_IP=$IP is not an IP address" >&2; exit 1; }
if [ "$PRINT_TARGET" = 1 ]; then echo "$IP"; exit 0; fi

KEY=""
for k in "$HOME/.ssh/id_ed25519" "$HOME/.ssh/airdash_ed25519"; do
  [ -f "$k" ] && { KEY="$k"; break; }
done
[ -n "$KEY" ] || { echo "no ssh key; run scripts/pve/create-lxc.py first" >&2; exit 1; }
SSH=(ssh -i "$KEY" -o StrictHostKeyChecking=accept-new -o ConnectTimeout=5 "root@$IP")

# rsync --delete runs as root on the target, so refuse anything that is not
# provably the container create-lxc.py built (it names it "airdash").
REMOTE_HOST="$("${SSH[@]}" hostname 2>/dev/null | tr -d '[:space:]')" || REMOTE_HOST=""
[ "$REMOTE_HOST" = "airdash" ] || { echo "$IP is not the AirDash container (hostname '${REMOTE_HOST:-unreachable}', expected 'airdash'); nothing deployed" >&2; exit 1; }
"${SSH[@]}" 'docker compose version >/dev/null 2>&1' || { echo "docker compose not available on $IP; run scripts/pve/create-lxc.py first" >&2; exit 1; }
[ -f "$ROOT/.env" ] || { echo ".env missing at $ROOT" >&2; exit 1; }

# Allow-list: only the service's own AIRDASH_* keys reach the LXC, so a
# stray secret in the local .env (Proxmox lines, anything else) never lands
# in the container environment. AIRDASH_UI_PORT is a host-only mapping and
# meaningless under host networking.
TMP_ENV="$(mktemp)"
trap 'rm -f "$TMP_ENV"' EXIT
grep -E '^AIRDASH_[A-Z0-9_]+=' "$ROOT/.env" | grep -v '^AIRDASH_UI_PORT=' > "$TMP_ENV" || true
grep -q '^AIRDASH_LAT=' "$TMP_ENV" || { echo "AIRDASH_LAT missing from $ROOT/.env" >&2; exit 1; }

# Validate the compose files with the env the LXC will get before rsync
# touches the remote tree, so a bad override never deletes a good release.
COMPOSE="docker compose -f docker-compose.yml -f docker-compose.lxc.yml"
if docker compose version >/dev/null 2>&1; then
  (cd "$ROOT" && $COMPOSE --env-file "$TMP_ENV" config --quiet) || { echo "compose config invalid locally; nothing deployed" >&2; exit 1; }
fi

"${SSH[@]}" "mkdir -p $REMOTE_DIR"
RSYNC_SSH="ssh -i '$KEY' -o StrictHostKeyChecking=accept-new -o ConnectTimeout=5"
rsync -az --delete -e "$RSYNC_SSH" \
  --exclude node_modules --exclude .pio --exclude bin \
  "$ROOT/service" "$ROOT/ui" "$ROOT/docker-compose.yml" "$ROOT/docker-compose.lxc.yml" "root@$IP:$REMOTE_DIR/"
rsync -az -e "$RSYNC_SSH" "$TMP_ENV" "root@$IP:$REMOTE_DIR/.env"

CONFIG_OUT="$("${SSH[@]}" "cd $REMOTE_DIR && $COMPOSE config --quiet" 2>&1)" || { echo "invalid compose config on $IP:" >&2; echo "$CONFIG_OUT" >&2; exit 1; }
"${SSH[@]}" "cd $REMOTE_DIR && $COMPOSE up --build -d --remove-orphans"
for _ in $(seq 1 30); do
  if curl -fsS -m 3 "http://$IP:8080/healthz" >/dev/null 2>&1; then break; fi
  sleep 2
done
curl -fsS -m 3 "http://$IP:8080/healthz" >/dev/null || { echo "service not healthy on $IP:8080" >&2; "${SSH[@]}" "cd $REMOTE_DIR && $COMPOSE logs --tail 20"; exit 1; }

if [ "$SYNC_SETTINGS" = 1 ]; then
  if SETTINGS="$(curl -fsS -m 3 http://localhost:8080/api/settings 2>/dev/null)"; then
    curl -fsS -m 5 -X PUT -H 'Content-Type: application/json' -d "$SETTINGS" "http://$IP:8080/api/settings" >/dev/null \
      || { echo "settings sync to $IP failed" >&2; exit 1; }
    echo "settings synced from the local service"
  else
    echo "local service not running; settings not synced"
  fi
fi

echo "service: http://$IP:8080/api/nearest"
echo "web UI:  http://$IP:3000"
