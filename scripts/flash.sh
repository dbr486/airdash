#!/usr/bin/env bash
# Builds and flashes the display firmware, then opens the serial monitor.
#   scripts/flash.sh                      build + upload + monitor
#   scripts/flash.sh --build-only
#   scripts/flash.sh --monitor-only
#   scripts/flash.sh --port=/dev/cu.usbserial-XXXX
#   scripts/flash.sh --board=sticks3          M5Stack StickS3 (default: ttgo, the T-Display)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FW="$ROOT/firmware"
PORT=""
MODE=full
PORT_GIVEN=0
BOARD=ttgo
for a in "$@"; do
  case "$a" in
    --port=*) PORT="${a#--port=}"; PORT_GIVEN=1 ;;
    --monitor-only) MODE=monitor ;;
    --build-only) MODE=build ;;
    --board=*) BOARD="${a#--board=}" ;;
    -h|--help) sed -n '2,7p' "$0"; exit 0 ;;
    *) echo "unknown argument: $a" >&2; exit 2 ;;
  esac
done
case "$BOARD" in
  ttgo|sticks3) ;;
  *) echo "unknown board: $BOARD (ttgo or sticks3)" >&2; exit 2 ;;
esac

# pio resolution: $PIO, then PATH, then the pipx default location.
PIO="${PIO:-$(command -v pio || true)}"
if [ -z "$PIO" ] && [ -x "$HOME/.local/bin/pio" ]; then PIO="$HOME/.local/bin/pio"; fi
if [ -z "$PIO" ]; then
  echo "pio not found. Install with: pipx install platformio" >&2
  exit 1
fi

# config.h is compiled in, so only build/upload need it; monitoring an
# already flashed board does not.
if [ "$MODE" != monitor ] && [ ! -f "$FW/include/config.h" ]; then
  echo "firmware/include/config.h is missing." >&2
  echo "  cp firmware/include/config.h.example firmware/include/config.h" >&2
  echo "then fill in WIFI_SSID, WIFI_PASS and AIRDASH_URL." >&2
  exit 1
fi

# The T-Display talks through a CP2104 bridge; the StickS3 is the ESP32-S3's
# own USB port.
detect_port() {
  case "$BOARD" in
    # Any native-USB board shows up as usbmodem, so a second one plugged in
    # makes the choice ambiguous: refuse rather than flash the wrong device.
    sticks3)
      CANDS="$(ls /dev/cu.usbmodem* 2>/dev/null || true)"
      if [ "$(printf '%s' "$CANDS" | grep -c . || true)" -gt 1 ]; then
        echo "several usbmodem ports; pass --port=:" >&2
        echo "$CANDS" >&2
        return 1
      fi
      printf '%s' "$CANDS" ;;
    *) ls /dev/cu.usbserial-* /dev/cu.SLAB_USBtoUART* /dev/cu.wchusbserial* 2>/dev/null | head -1 || true ;;
  esac
}

if [ "$MODE" != build ]; then
  [ -z "$PORT" ] && { PORT="$(detect_port)" || exit 1; }
  if [ -z "$PORT" ]; then
    echo "no serial port found; plug the display in or pass --port=/dev/cu.xxx" >&2
    exit 1
  fi
  echo "flash: port $PORT"
fi

cd "$FW"
case "$MODE" in
  build)   exec "$PIO" run -e "$BOARD" ;;
  monitor) exec "$PIO" device monitor -p "$PORT" -b 115200 ;;
  full)
    "$PIO" run -e "$BOARD" -t upload --upload-port "$PORT"
    # Native USB drops off the bus while the S3 resets after flashing; wait
    # for it to come back before monitoring. An explicit --port is waited
    # for as given; otherwise exactly one usbmodem port must reappear, so
    # the monitor never attaches to some other native-USB device.
    if [ "$BOARD" = sticks3 ]; then
      FOUND=""
      for _ in $(seq 1 20); do
        sleep 0.5
        if [ "$PORT_GIVEN" = 1 ]; then
          if [ -e "$PORT" ]; then FOUND="$PORT"; break; fi
          continue
        fi
        CANDS="$(ls /dev/cu.usbmodem* 2>/dev/null || true)"
        N="$(printf '%s' "$CANDS" | grep -c . || true)"
        if [ "$N" -gt 1 ]; then
          echo "several usbmodem ports after upload; pass --port=:" >&2
          echo "$CANDS" >&2
          exit 1
        fi
        if [ "$N" = 1 ]; then FOUND="$CANDS"; break; fi
      done
      if [ -z "$FOUND" ]; then
        echo "upload done, but the StickS3 port did not come back; try scripts/flash.sh --board=sticks3 --monitor-only" >&2
        exit 1
      fi
      PORT="$FOUND"
    fi
    exec "$PIO" device monitor -p "$PORT" -b 115200 ;;
esac
