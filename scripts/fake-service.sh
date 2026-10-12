#!/usr/bin/env bash
# Serves a canned /api/nearest so the display can be tested with no live
# traffic. Point firmware/include/config.h at the URL it prints.
set -euo pipefail

PORT=8080
BODY=found
for a in "$@"; do
  case "$a" in
    --empty) BODY=empty ;;
    --stale) BODY=stale ;;
    --early) BODY=early ;;
    --long) BODY=long ;;
    --names) BODY=names ;;
    --logo) BODY=logo ;;
    --port=*) PORT="${a#--port=}" ;;
    -h|--help) echo "usage: $0 [--empty|--stale|--early|--long|--names|--logo] [--port=N]"; exit 0 ;;
    *) echo "unknown argument: $a" >&2; exit 2 ;;
  esac
done

DIR="$(mktemp -d)"
mkdir -p "$DIR/api"
case "$BODY" in
  found)
    printf '%s' '{"found":true,"callsign":"BAW466","hex":"400877","index":0,"type":"A319","reg":"G-EUPG","airline":"British Airways","origin":"LHR","dest":"BCN","logo":"","alt_ft":3525,"gs_kt":267,"track":216,"dist_km":3.4,"bearing":112,"age_s":2,"nearby":3,"radius_km":10,"delay":{"kind":"late","min":14,"basis":"arrival"}}' > "$DIR/api/nearest" ;;
  empty)
    printf '%s' '{"found":false,"age_s":1,"nearby":0,"radius_km":12.5}' > "$DIR/api/nearest" ;;
  stale)
    printf '%s' '{"found":true,"callsign":"PRIV1","hex":"4ca5e1","index":0,"type":"C172","reg":"G-ABCD","airline":"","origin":"","dest":"","logo":"","alt_ft":1500,"gs_kt":95,"track":10,"dist_km":0.8,"bearing":300,"age_s":99,"nearby":1,"radius_km":10}' > "$DIR/api/nearest" ;;
  early)
    printf '%s' '{"found":true,"callsign":"RYR12CD","hex":"3c6444","index":0,"type":"B738","reg":"EI-DWX","airline":"Ryanair","origin":"STN","dest":"DUB","logo":"","alt_ft":4100,"gs_kt":280,"track":270,"dist_km":2.1,"bearing":90,"age_s":3,"nearby":2,"radius_km":10,"delay":{"kind":"early","min":-7,"basis":"arrival"}}' > "$DIR/api/nearest" ;;
  long)
    printf '%s' '{"found":true,"callsign":"LONG1","hex":"344567","index":0,"type":"A21N","reg":"EC-LONG","airline":"An Airline With A Very Long Name","origin":"","dest":"","logo":"","alt_ft":2500,"gs_kt":210,"track":90,"dist_km":3.1,"bearing":100,"age_s":2,"nearby":1,"radius_km":10,"delay":{"kind":"cancelled","min":0}}' > "$DIR/api/nearest" ;;
  names)
    printf '%s' '{"found":true,"callsign":"VLG1234","hex":"3444a1","index":0,"type":"A320","reg":"EC-MJB","airline":"Vueling","origin":"PMI","dest":"BCN","origin_label":"Palma de Mallorca","dest_label":"Barcelona","logo":"","alt_ft":3800,"gs_kt":240,"track":45,"dist_km":2.6,"bearing":200,"age_s":2,"nearby":2,"radius_km":10,"delay":{"kind":"on_time","min":2,"basis":"arrival"}}' > "$DIR/api/nearest" ;;
  logo)
    printf '%s' '{"found":true,"callsign":"VLG8VK","hex":"34515e","index":0,"type":"A320","reg":"EC-NAJ","airline":"Vueling","airline_iata":"VY","origin":"AGP","dest":"BCN","origin_label":"Malaga","dest_label":"Barcelona","logo":"VY","alt_ft":3200,"gs_kt":230,"track":60,"dist_km":4.4,"bearing":150,"age_s":2,"nearby":1,"radius_km":10}' > "$DIR/api/nearest"
    mkdir -p "$DIR/api/logo/VY"
    # 104x32 RGB565 big-endian: yellow (0xFFE0) with a 2 px red (0xF800) border,
    # so a byte-order mistake shows up as the wrong colours.
    python3 - "$DIR/api/logo/VY/104x32.rgb565" <<'PYEOF'
import sys
out = bytearray()
for y in range(32):
    for x in range(104):
        v = 0xF800 if (x < 2 or y < 2 or x >= 102 or y >= 30) else 0xFFE0
        out += bytes([v >> 8, v & 0xFF])
open(sys.argv[1], "wb").write(out)
PYEOF
    ;;
esac

IP="$(ipconfig getifaddr en0 2>/dev/null || hostname -I 2>/dev/null | awk '{print $1}' || echo 127.0.0.1)"
echo "fake-service: '$BODY' body at http://$IP:$PORT/api/nearest  (Ctrl-C to stop)"
cd "$DIR"
exec python3 -m http.server "$PORT" --bind 0.0.0.0
