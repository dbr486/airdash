#!/usr/bin/env bash
# Refreshes the embedded airline list (VRS standing data, CC0) that the
# service uses to name operators. Re-run occasionally and commit the result.
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
