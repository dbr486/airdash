#!/usr/bin/env bash
# Refreshes the embedded reference data the service ships with: the airline
# list (VRS standing data, CC0) that names operators, and the airport list
# (OurAirports, public domain) that names route ends. Re-run occasionally and
# commit the result.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
URL="https://raw.githubusercontent.com/vradarserver/standing-data/main/airlines/schema-01/airlines.csv"
DEST="$ROOT/service/internal/standing/data/airlines.csv"
TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT

code="$(curl -sS -L -o "$TMP" -w '%{http_code}' "$URL")"
if [ "$code" != "200" ]; then
  echo "download failed: HTTP $code" >&2
  exit 1
fi
# A truncated or error-page download must never replace the embedded list.
size="$(wc -c < "$TMP" | tr -d ' ')"
if [ "$size" -lt 102400 ]; then
  echo "refusing suspiciously small file ($size bytes)" >&2
  exit 1
fi
mv "$TMP" "$DEST"
trap - EXIT
echo "updated $DEST ($size bytes)"

AIRPORTS_URL="https://davidmegginson.github.io/ourairports-data/airports.csv"
AIRPORTS_DEST="$ROOT/service/internal/standing/data/airports.csv"
RAW="$(mktemp)"
OUT="$(mktemp)"
trap 'rm -f "$RAW" "$OUT"' EXIT
code="$(curl -sS -L -o "$RAW" -w '%{http_code}' "$AIRPORTS_URL")"
if [ "$code" != "200" ]; then
  echo "airports download failed: HTTP $code" >&2
  exit 1
fi
# Only airports a scheduled route could name are kept (an IATA code, or a
# large/medium airport), which shrinks ~12 MB to ~180 KB of iata,icao,city.
python3 -I - "$RAW" "$OUT" <<'PY'
import csv, sys
src, dst = sys.argv[1], sys.argv[2]
with open(src, encoding="utf-8", newline="") as f, open(dst, "w", encoding="utf-8", newline="") as out:
    w = csv.writer(out, lineterminator="\n")
    w.writerow(["iata", "icao", "city"])
    for r in csv.DictReader(f):
        if r["type"] == "closed" or not r["municipality"]:
            continue
        if r["iata_code"] or r["type"] in ("large_airport", "medium_airport"):
            w.writerow([r["iata_code"], r["icao_code"] or r["ident"], r["municipality"]])
PY
rows="$(wc -l < "$OUT" | tr -d ' ')"
# A partial upstream file still parses; never let it shrink the embedded
# table by more than a tenth (airports rarely disappear in bulk).
old=0
if [ -f "$AIRPORTS_DEST" ]; then
  old="$(wc -l < "$AIRPORTS_DEST" | tr -d ' ')"
fi
if [ "$rows" -lt 5000 ] || [ "$rows" -lt $((old * 9 / 10)) ]; then
  echo "refusing suspiciously short airport list ($rows rows, had $old)" >&2
  exit 1
fi
mv "$OUT" "$AIRPORTS_DEST"
echo "updated $AIRPORTS_DEST ($rows rows)"
