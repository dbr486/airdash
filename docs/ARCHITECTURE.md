# Architecture

AirDash has three parts: a Go **service** that does all the work, small
**displays** that poll it, and a **web UI** that mirrors the display and
edits the settings.

```
  T-Display / StickS3 ──GET /api/nearest, /api/logo/…──┐
                                                       ▼
  browser ──► ui (Node, :3000) ──proxies /api/*──► service (Go, :8080)
                                                       │
         ┌──────────────┬──────────────┬───────────────┼──────────────┬───────────────┐
         ▼              ▼              ▼               ▼              ▼               ▼
   adsb.fi/adsb.lol   adsbdb     pics.avs.io     AirLabs board   AeroAPI      Nominatim
   (positions)       (routes)    (logos)         (optional)     (optional)   (address search)
```

The displays never talk to the internet directly. Keys, caches and rate
limits all live in the service, so a display only needs WiFi and one URL.

## Service (`service/`)

Wiring is in `cmd/airdash/main.go`; each package has one job.

1. **`provider`**: polls a readsb-style point query
   (`AIRDASH_PROVIDER`, every `AIRDASH_POLL_SECONDS`) for aircraft within
   the radius of home.
2. **`tracker`**: keeps the latest snapshot, drops aircraft on the ground or
   without a position, applies the airport **`filter`**, sorts by distance
   (**`geo`**), and answers "nearest", "follow this hex" and "next after
   this hex".
3. **`routes`**: callsign → airline, origin and destination from adsbdb,
   cached for `AIRDASH_ROUTE_CACHE_HOURS`, with a cooldown after errors.
   City names come from the same answer.
4. **Delay status** is a chain, cheapest first:
   - **`board`** (with `AIRDASH_AIRLABS_KEY`): downloads the departures and
     arrivals board of one airport every `AIRDASH_BOARD_REFRESH_MINUTES`
     and matches flights locally. A monthly call counter is persisted so
     redeploys cannot overrun the quota.
   - **`flightstatus`** (with `AIRDASH_AEROAPI_KEY`): a per-flight AeroAPI
     lookup, only for the aircraft on screen. Results are cached for
     15 minutes and capped per UTC day.
5. **`logos`**: fetches the airline's square logo from the CDN, crops the
   wordmark out of its padding and fits it (`fit.go`). It serves RGB565 for
   the displays and PNG for the page, cached for a week.
6. **`settings`**: runtime settings (home, filters, radius, toggles,
   caps). Env vars are the defaults, `PUT /api/settings` changes them live,
   and `AIRDASH_SETTINGS_FILE` persists them.
7. **`geocode`**: address → coordinates for the settings page, via
   Nominatim, throttled to one request per second and cached for a day.
8. **`api`**: the HTTP surface (`/api/nearest`, `/api/settings`,
   `/api/logo/…`, `/api/geocode`, `/api/stats`, `/healthz`).

The service has no third-party Go modules.

## Web UI (`ui/`)

`server.js` (Express) serves `public/` and proxies `/api/*` to the
service (`AIRDASH_API`). `index.html` with `mirror.js`/`mirror.css`
draws the same rows as the display and holds the settings form.
`widget.html` is the screen alone, for a Home Assistant iframe card. The
browser only ever talks to the UI server.

## Firmware (`firmware/`)

One codebase with a PlatformIO env per board:

- `ttgo`: LILYGO T-Display (ESP32), drawn with TFT_eSPI configured
  entirely from build flags.
- `sticks3`: M5Stack StickS3 (ESP32-S3), drawn with M5Unified/M5GFX,
  which also own the power chip and buttons.

`board.h`/`board.cpp` hide the difference (display, buttons, backlight,
battery). Everything else is shared. `main.cpp` runs the loop: poll
`AIRDASH_URL` every `POLL_MS`, fetch a new logo bitmap only when the
airline changes, redraw.

Logic that needs no hardware lives in small headers and modules
(`parse`, `format`, `logourl`, `pollurl`, `logosync`, `stale`,
`reconnect`, `battery`). These are what `pio test -e native` covers on
the host.

## Rate limits and budgets

| Upstream | Limit | Where it is enforced |
|---|---|---|
| adsb.fi | 1 request/s | default 5 s poll; minimum 2 s |
| adsb.lol | ~8 requests/min | use `AIRDASH_POLL_SECONDS` ≥ 10 |
| Nominatim | 1 request/s | `geocode` throttle and 1-day cache |
| AirLabs | plan quota (free: 1,000/month) | `AIRDASH_BOARD_CALLS_PER_MONTH` (default 900), persisted counter |
| AeroAPI | paid per call | `delay_lookups_per_day` setting (default 30), 15-min cache |
