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
   (positions)    (routes, owners) (logos)       (optional)     (optional)   (address search)

   VRS airline list + type names + OurAirports city names: embedded in the
   service binary, no network
```

The displays never talk to the internet directly. Keys, caches and rate
limits all live in the service, so a display only needs WiFi and one URL.

## Service (`service/`)

Wiring is in `cmd/airdash/main.go`; each package has one job.

1. **`provider`**: polls a readsb-style point query
   (`AIRDASH_PROVIDER`, every `AIRDASH_POLL_SECONDS`) for aircraft within
   the radius of home. Altitude below 6,000 ft is QNH-corrected when the
   feed's barometric altitude is known; an aircraft without its own
   `nav_qnh` borrows the median of the airborne aircraft below 6,000 ft. The tracker also adds `type_name`
   (e.g. "737-800") from the embedded ICAO type table, and `/api/nearest`
   exposes it next to `type`.
2. **`tracker`**: keeps the latest snapshot, drops aircraft on the ground or
   without a position, applies the airport **`filter`**, sorts by distance
   (**`geo`**), and answers "nearest", "follow this hex" and "next after
   this hex".
3. **`routes`**: callsign → airline, origin and destination from adsbdb,
   cached for `AIRDASH_ROUTE_CACHE_HOURS`, with a cooldown after errors.
   City names come from the same answer. adsbdb routes have no date and go
   stale when an airline reuses a flight number, so before the filter the
   tracker settles each aircraft's route in order: a board or AeroAPI route
   already found this pass (so it cannot flap); the airport board's one row
   for the same callsign whose leg is flying now (departed within 45 min or
   up to 3 h behind schedule, or due to land within an hour and not landed,
   not cancelled: today's schedule, free); then adsbdb's route, only if
   the aircraft's position could lie on it (detour via its position at most
   1.25 × the direct distance plus 100 km). Otherwise the airports are
   dropped and the airline stays. A route still missing is asked of hexdb
   (free, no key): its record must be under 30 days old, a single leg, and
   pass the same position check; it carries no city names, so the snapshot
   names its ends (and board routes) from the embedded airport table. Only then may
   step 5 ask AeroAPI. hexdb errors never mark route lookup as failing.
   **`operators`** then names the operating airline for each aircraft that
   survives the filter: the callsign prefix is looked up in the embedded
   VRS airline list; failing that (and when the route gave no airline) the
   adsbdb registered owner by hex is mapped through the same list. Owner
   answers are cached 7 days when known, 24 hours when unknown and 60
   seconds after an error. An operator found this way replaces the route's
   airline name, and its IATA code too when the list has one (so the logo
   matches the name shown). It also covers aircraft with no
   callsign or no known route.
4. **Delay status** is a chain, cheapest first:
   - **`board`** (with `AIRDASH_AIRLABS_KEY`): downloads the departures and
     arrivals board of one airport every `AIRDASH_BOARD_REFRESH_MINUTES`
     and matches flights locally. A monthly call counter is persisted so
     redeploys cannot overrun the quota.
   - **`flightstatus`** (with `AIRDASH_AEROAPI_KEY`): a per-flight AeroAPI
     lookup, only for the aircraft on screen. Results are cached for
     15 minutes and capped per UTC day.
5. **Route fallback** (tracker): when step 3 leaves the nearest aircraft
   with no route and `paid_routes` is on, the tracker makes one AeroAPI
   `/flights/{ident}` call (ident = callsign, else the registration without
   its dash; light aircraft are skipped). The call goes through the same
   15-minute cache as the delay lookup, so route and delay never cost two
   calls, and it spends the same daily cap. A found route or an "unknown"
   answer is remembered for the rest of that aircraft's pass, so one
   aircraft costs at most one route-only call per pass. Filters run before
   this call; a paid route changes what is shown, not what is listed.
6. **Route source log** (`sourcelog`, with `AIRDASH_SOURCE_LOG_FILE`): every
   tick the tracker records, per aircraft pass (hex + first sighting), what
   the board, adsbdb, hexdb and AeroAPI answered (hit, implausible, unknown,
   miss, error, skipped, off, none, not asked) and which route was shown. To
   make the comparison complete, the board and hexdb are asked for every
   aircraft with a callsign (the board has no network cost, hexdb is cached),
   while the display order above is unchanged; AeroAPI is never asked just for
   the log. Records go through a non-blocking queue to one SQLite writer (WAL,
   one transaction per tick) that merges them into one row per pass (a hit
   is never replaced by a later miss) and prunes rows older than 90 days.
   `GET /api/sources/summary`, `/passes` and `/disagreements` serve it.

   The same database also keeps the raw answers: the HTTP clients of adsbdb,
   hexdb, AeroAPI and AirLabs are wrapped by `capture`, which stores each
   response body (gzip, in `sources.db`) with its status and fetch time. The
   body is copied as the client reads it, so capture never delays a lookup;
   bodies over 4 MiB or cut by a read error are not kept. The
   API key is stripped from the stored URL and headers are never stored (only
   the response's Content-Type is kept). AirLabs bodies are stored without
   their `request` echo (which carries the key): only `response` and `error`
   are kept, and a body that is not a JSON object is stored empty, since the
   echo cannot be removed from it. AirLabs responses are pruned after 7 days (the board
   refreshes constantly), the others after 90.
   `GET /api/sources/responses?source=&subject=&before=&limit=` lists them
   (newest first), `GET /api/sources/responses/{id}` returns one with its
   metadata and `GET /api/sources/responses/{id}/raw` the stored body.

7. **`logos`**: fetches the airline's square logo from the CDN, crops the
   wordmark out of its padding and fits it (`fit.go`). It serves RGB565 for
   the displays and PNG for the page, cached for a week.
8. **`settings`**: runtime settings (home, filters, radius, toggles,
   caps). Env vars are the defaults, `PUT /api/settings` changes them live,
   and `AIRDASH_SETTINGS_FILE` persists them.
9. **`geocode`**: address → coordinates for the settings page, via
   Nominatim, throttled to one request per second and cached for a day.
10. **`api`**: the HTTP surface (`/api/nearest`, `/api/settings`,
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
| hexdb | none published | asked only when board and adsbdb miss; same route cache as adsbdb, airports cached for the process |
| AirLabs | plan quota (free: 1,000/month) | `AIRDASH_BOARD_CALLS_PER_MONTH` (default 900), persisted counter |
| AeroAPI | paid per call | `delay_lookups_per_day` setting (default 65, shared by delay and paid routes), 15-min cache |
