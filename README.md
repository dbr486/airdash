# AirDash

A tiny desk display that shows the aircraft flying closest to your home:
callsign, airline logo, route, altitude, distance and whether it's on time.
The same live screen also works as a **Home Assistant dashboard widget**,
with or without the hardware.
The same live screen also works as a **Home Assistant dashboard widget**,
with or without the hardware.

[![ci](https://github.com/dbr486/airdash/actions/workflows/ci.yml/badge.svg)](https://github.com/dbr486/airdash/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

<!-- photo: docs/img/airdash.jpg -->

## Features

- The nearest aircraft within a radius of your home, refreshed every few seconds
- Step through every aircraft in range with a button
- Filters: everything nearby, departures from an airport, arrivals to one, or anything touching one
- Route as airport codes or city names, with the airline's logo
- Optional on-time / delayed status (FlightAware AeroAPI and/or an AirLabs airport board)
- A web page that mirrors the display and edits every setting live, including an address lookup for home
- A Home Assistant widget: the live screen in a dashboard card (optionally
  with Refresh / Next buttons), plus a REST sensor for automations
- Two boards from one codebase: LILYGO T-Display and M5Stack StickS3

## Hardware

| Part | Notes |
|---|---|
| [LILYGO TTGO T-Display V1.1](https://lilygo.cc) (ESP32, 1.14" 135×240) | Two buttons beside the USB port |
| or [M5Stack StickS3](https://docs.m5stack.com) (ESP32-S3, same panel size) | Battery-powered; flashing replaces the factory UiFlow2 firmware (restore it with M5Burner) |
| An always-on machine for the service | Anything that runs Docker: a Raspberry Pi, NAS, mini PC, Proxmox LXC or a laptop |

The ESP32 only joins **2.4 GHz** WiFi.

## How it works

A small Go service polls a public ADS-B feed for aircraft around your
home, looks up their routes, logos and (optionally) delay status, and
serves the result as JSON. The display polls that service every 3 seconds
over your LAN and draws it; a web UI shows the same screen and edits the
settings. Details: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Quick start

**1. Run the service and web UI** (on the always-on machine):

    git clone https://github.com/dbr486/airdash.git && cd airdash
    cp .env.example .env          # set AIRDASH_LAT / AIRDASH_LON to your home
    docker compose up --build -d
    curl localhost:8080/api/nearest

Open `http://<server>:3000`. The page shows what the display will show,
and lets you set home (with an address search), radius and filters.

**2. Give the server a fixed LAN address.** Use a DHCP reservation on
your router (or a static IP) so the display's URL keeps working.

**3. Flash the display** (on a computer with USB):

    pipx install platformio
    cp firmware/include/config.h.example firmware/include/config.h
    # edit config.h: WIFI_SSID, WIFI_PASS, and AIRDASH_URL → http://<server-ip>:8080/api/nearest
    scripts/flash.sh                    # T-Display
    scripts/flash.sh --board=sticks3    # StickS3

`flash.sh` builds, uploads and opens the serial monitor. Port
autodetection works on macOS; on Linux or Windows pass the port
(`--port=/dev/ttyUSB0`, `--port=/dev/ttyACM0` or `--port=COM3`), or use
PlatformIO directly: `cd firmware && pio run -e ttgo -t upload && pio device monitor`
(`-e sticks3` for the StickS3).

## Using it

| | T-Display | StickS3 |
|---|---|---|
| Next aircraft in range | GPIO 0 button, short press | front key (BtnA), short press |
| Backlight on/off | GPIO 0 button, hold 0.7 s | front key, hold 0.7 s |
| Refresh now | GPIO 35 button | side key (BtnB) |

Bottom-line marks: `stale` means the data is older than 30 s or
unparsable; `x` means WiFi or the service is unreachable. The StickS3
also shows a battery gauge (`+` while charging).

Settings changed on the web page apply to the running service, so the
display follows within a few seconds; in Docker they persist across
restarts in the `airdash-data` volume.

## Configuration

All service configuration is environment variables, set in `.env` (see
[.env.example](.env.example)). Filters, radius, home and the display
toggles are only *defaults*: the web page (or `PUT /api/settings`)
overrides them at runtime.

| Variable | Default | Meaning |
|---|---|---|
| `AIRDASH_LAT`, `AIRDASH_LON` | *required* | Home position, decimal degrees |
| `AIRDASH_RADIUS_KM` | `10` | Search radius |
| `AIRDASH_POLL_SECONDS` | `5` | Feed poll interval (min 2; use ≥ 10 with adsb.lol) |
| `AIRDASH_PROVIDER` | `adsbfi` | `adsbfi` or `adsblol` |
| `AIRDASH_ROUTE_CACHE_HOURS` | `6` | How long a callsign's route is cached |
| `AIRDASH_ORIGIN`, `AIRDASH_DEST`, `AIRDASH_AIRPORT` | empty | Airport filters: comma-separated IATA/ICAO codes; codes in one variable are OR-ed, variables are AND-ed |
| `AIRDASH_AEROAPI_KEY` | empty | Enables per-flight delay status (see below) |
| `AIRDASH_AIRLABS_KEY` | empty | Enables the airport-board delay source (see below) |
| `AIRDASH_BOARD_AIRPORT` | empty | Board airport (also editable on the page) |
| `AIRDASH_BOARD_REFRESH_MINUTES` | `100` | Board refresh interval (min 5) |
| `AIRDASH_BOARD_CALLS_PER_MONTH` | `900` | Monthly AirLabs call cap the service enforces |
| `AIRDASH_PORT` | `8080` | Host port for the service (docker compose) |
| `AIRDASH_UI_PORT` | `3000` | Host port for the web UI (docker compose) |
| `AIRDASH_LISTEN` | `:8080` | Listen address, standalone binary only |
| `AIRDASH_SETTINGS_FILE` | empty | Where runtime settings persist, standalone binary only (compose sets `/data/settings.json`) |
| `AIRDASH_SOURCE_LOG_FILE` | empty | SQLite file recording what each route source answered per aircraft pass, kept 90 days; empty turns it off (compose sets `/data/sources.db`) |

The display's settings live in `firmware/include/config.h`: `WIFI_SSID`,
`WIFI_PASS`, `AIRDASH_URL` and `POLL_MS` (default 3000).

## Delay status (optional)

**AeroAPI.** Get a key at
https://www.flightaware.com/commercial/aeroapi/ (Personal tier, $5 free
credit ≈ 1000 lookups/month) and set `AIRDASH_AEROAPI_KEY`. The service
looks up on-time / delayed status only for the aircraft currently on
screen, caches each result for 15 minutes, and caps lookups per UTC day
via `delay_lookups_per_day` (default 65, editable on the page). The same
key and cap also fill a missing route: when the airport board does not
list the flight and adsbdb has no route the aircraft could be flying and `paid_routes` is on (default
on with a key), one AeroAPI call names its origin and destination; that
call also answers the delay, so one aircraft never costs two. Light
aircraft are skipped, and an unknown answer is not retried while the
aircraft stays in range. Even in the worst case (the cap hit every day)
the default cap of 65/day stays near $10 a month (65 × 31 × $0.005), about
$5 a month beyond the free credit; the cache keeps real usage far lower. The counter
resets on restart. A `show_delay:false` or `paid_routes:false` saved to
the settings file before a key existed stays off after the key is added,
until you tick the box on the page.

**AirLabs airport board.** With `AIRDASH_AIRLABS_KEY` set (free tier:
1,000 calls/month, https://airlabs.co), the service downloads the
departures and arrivals board of `AIRDASH_BOARD_AIRPORT` (or the
`board_airport` setting) every 100 minutes and answers delay status for
every aircraft departing from or arriving at it, including the ones you
step through with the button, with no per-flight calls. AeroAPI is then
only asked for the nearest aircraft when the board has no match. A
departure the board has not yet recorded is dated from when the display
first saw it airborne (about 10 minutes after leaving the gate), so a
late departure reads late even between board refreshes. `/api/stats`
reports the board's airport, age, size and this month's call count
against the cap.

You are responsible for any charges on your keys; see
[DISCLAIMER.md](DISCLAIMER.md).

## Home Assistant

AirDash can show the same screen on a Home Assistant dashboard. You only
need the service and web UI running; the physical display is optional.

**Widget card.** `http://<server>:3000/widget` is the screen on its own,
scaled to fit whatever box it is given. Open the settings page, scroll to
its "Home Assistant" section and press **Copy card YAML**. Then in Home
Assistant go to Edit dashboard → Add card → Manual and paste it. The
snippet uses your deployment's address and looks like this:

    type: iframe
    url: http://<server>:3000/widget
    aspect_ratio: 56.25%
    title: AirDash

Add `?controls=1` to the URL for Refresh and Next buttons under the
screen. Give the card some extra height if you do (for example
`aspect_ratio: 70%`), because 56.25% only fits the screen.

**REST sensor.** The same section has a **Copy sensor YAML** button for a
`rest:` sensor in `configuration.yaml`. It polls `/api/nearest` every 10 s.
Its state is the nearest aircraft's callsign (or `none`) and its
attributes are airline, origin, dest, alt_ft, gs_kt, dist_km, bearing,
nearby and delay. Use it in automations, templates or a Markdown card.

**HTTPS.** A Home Assistant served over HTTPS can't embed the HTTP widget
because the browser blocks mixed content. Use the REST sensor there
instead, or serve AirDash behind an HTTPS reverse proxy.

## Running without Docker

    cd service && go build -o bin/airdash ./cmd/airdash
    AIRDASH_LAT=51.47 AIRDASH_LON=-0.45 ./bin/airdash

    cd ui && npm install && AIRDASH_API=http://localhost:8080 npm start

## Deploy to a Proxmox LXC (optional)

Two scripts create an unprivileged Debian 12 container with Docker and
deploy AirDash into it. Create `~/.airdash-proxmox.env` (`chmod 600`, kept
outside the repo). Comments go on their own lines; everything after `=` is
the value:

    PROXMOX_HOST=pve.example.com
    PROXMOX_PORT=8006
    PROXMOX_USER=root@pam
    PROXMOX_PASSWORD=changeme
    # SHA-256 of the PVE certificate, colon-separated; leave empty and the first run prints it.
    PROXMOX_FINGERPRINT=
    #PROXMOX_NODE=pve

    # The container's static address, with prefix.
    AIRDASH_LXC_IP=192.0.2.50/24
    AIRDASH_LXC_GATEWAY=192.0.2.1
    # Default: inherit the host's DNS.
    #AIRDASH_LXC_NAMESERVERS=192.0.2.53
    #AIRDASH_LXC_VMID=103
    #AIRDASH_LXC_BRIDGE=vmbr0
    #AIRDASH_LXC_STORAGE=local-lvm
    #AIRDASH_LXC_TEMPLATE=local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst

Then:

    scripts/pve/create-lxc.py --dry-run      # shows the container config (generates ~/.ssh/airdash_ed25519 if you have no key)
    scripts/pve/create-lxc.py                # creates + starts the LXC (1 GiB, 2 cores) and installs Docker; safe to re-run
    scripts/deploy-lxc.sh --sync-settings    # rsyncs the code and .env, builds and starts both containers there

Download the template first if your host lacks it (`pveam update && pveam
download local debian-12-standard_12.7-1_amd64.tar.zst`). Proxmox uses a
self-signed certificate, so the script cannot verify it against a CA.
Leave `PROXMOX_FINGERPRINT` empty on the first run: the script prints the
fingerprint it saw and stops without sending the password. Compare it with
the one in the Proxmox web UI (node → System → Certificates), pin it and
re-run. From then on a certificate mismatch also aborts before the
password is sent. An existing container with the same
VMID is only touched if its hostname is `airdash` and its address matches.

`--sync-settings` copies your local service's current settings to the
LXC; use it on the first deploy only, since it overwrites whatever was set
through the LXC's web UI. Re-run `scripts/deploy-lxc.sh` (without the
flag) after any change; `AIRDASH_LXC_IP` in the environment overrides the
env file. Point the display at `http://<AIRDASH_LXC_IP without /24>:8080/api/nearest`.

The LXC is unprivileged, and Docker's default bridge networking can't set
the ports sysctl inside it, so the deploy uses `docker-compose.lxc.yml`
(an override on top of `docker-compose.yml`) to run both containers with
`network_mode: host`: the service binds `:8080` and the UI `:3000`
directly on the LXC.

## API

### `GET /api/nearest`

    {"found":true,"callsign":"BAW466","hex":"400877","index":0,"type":"A319","reg":"G-EUPG",
     "airline":"British Airways","airline_iata":"BA","origin":"LHR","dest":"BCN",
     "origin_label":"LHR","dest_label":"BCN","logo":"BA",
     "alt_ft":3525,"gs_kt":267,"track":216,"dist_km":3.4,"bearing":112,
     "age_s":2,"nearby":3,"radius_km":10,
     "delay":{"kind":"late","min":7,"basis":"arrival"}}

`?hex=<icao>` follows that aircraft while it is in range (else the
nearest); `?after=<icao>` returns the next one in the distance-sorted
list, wrapping. `index` is the position (0 = nearest). `found:false`
carries only `age_s`, `nearby` and `radius_km`. `age_s` is seconds since
the last successful upstream fetch. `delay` is omitted when status is
unknown or the delay feature is off.

### `GET /api/settings`, `PUT /api/settings`

    PUT /api/settings   {"lat":51.47,"lon":-0.45,
                          "origin":"","dest":"","airport":"LHR","radius_km":10,
                          "show_delay":true,"delay_lookups_per_day":65,"paid_routes":true,
                          "show_names":false,"show_logos":true,
                          "board_airport":"LHR"}

All keys are optional on PUT; codes are comma-separated IATA or ICAO. 400
on a bad code or radius. `show_delay` toggles the delay feature (no effect
without `AIRDASH_AEROAPI_KEY`); `paid_routes` lets AeroAPI fill a route
the free sources lack; `delay_lookups_per_day` caps all AeroAPI calls, delay
and route together, per UTC day (0–1000; 0 turns both off). `show_names` makes `origin_label`/`dest_label`
carry city names instead of airport codes where adsbdb knows the
municipality. `show_logos` turns airline logos on or off. `lat`/`lon`
move the home position (defaults from `AIRDASH_LAT`/`AIRDASH_LON`).

### Other endpoints

- `GET /api/logo/{iata}`: the airline logo, cached in the service for a week.
- `GET /api/logo/{iata}/104x32.rgb565`: the display bitmap (RGB565,
  big-endian; the wordmark is cropped out of the CDN's square padding and
  fitted). `WxH.png` gives the same image for the page; the older square
  `N.rgb565` forms are still served.
- `GET /api/geocode?q=<address>` → `{"lat","lon","name"}`, backed by
  OpenStreetMap Nominatim (one lookup per second, cached a day).
- `GET /api/stats` → `{"delay_enabled":true,"delay_lookups_today":12,"delay_lookups_cap":65,"delay_source":"aeroapi","paid_routes_enabled":true}`,
  plus `board_*` fields when the AirLabs board is enabled.
- `GET /healthz`: liveness.

The API has no authentication; keep it on your LAN
([SECURITY.md](SECURITY.md)).

## Development

Bench-test the display without live traffic:

    scripts/fake-service.sh            # a sample flight; --empty for "No aircraft"; --stale for age_s 99; --logo for a test bitmap

It listens on 8080 by default, the same port as the real service; pass
`--port=8081` (and point `config.h` at it) if the service is running.
`scripts/flash.sh --build-only` compiles without a board and
`--monitor-only` just opens the serial log.

Tests and conventions: [CONTRIBUTING.md](CONTRIBUTING.md).

## Troubleshooting

- **Display never connects**: the ESP32 needs a 2.4 GHz network; check
  `WIFI_SSID`/`WIFI_PASS` in `config.h` and watch the serial log
  (`scripts/flash.sh --monitor-only`).
- **`x` on the display**: the service is unreachable. Check `AIRDASH_URL`,
  that the server's address hasn't changed, and that its firewall allows
  port 8080 from the LAN (`curl http://<server-ip>:8080/healthz` from
  another machine).
- **`stale` on the display**: the feed isn't answering, usually rate
  limiting. Raise `AIRDASH_POLL_SECONDS`; adsb.lol needs 10 or more.
  `docker compose logs airdash` shows the upstream errors.
- **StickS3 upload fails or picks the wrong port**: pass `--port=`
  explicitly. The script refuses to guess when several `usbmodem` ports
  are present.
- **Home Assistant card is blank**: an HTTPS Home Assistant can't embed
  an HTTP page; use the REST sensor snippet instead.

## Data sources and credits

- Aircraft positions: [adsb.fi](https://adsb.fi) open data (personal,
  non-commercial use) or [adsb.lol](https://adsb.lol)
- Routes and airlines: [adsbdb](https://www.adsbdb.com), with [hexdb](https://hexdb.io) as a free fallback for routes
- Airline names: [VRS standing data](https://github.com/vradarserver/standing-data) (CC0), refreshed with scripts/update-standing-data.sh
- Airport boards: [AirLabs](https://airlabs.co) schedules API
- Flight status: [FlightAware AeroAPI](https://www.flightaware.com/commercial/aeroapi/)
- Address lookup: [OpenStreetMap Nominatim](https://nominatim.org), © OpenStreetMap contributors
- Airline logos: Travelpayouts airline logo CDN (https://pics.avs.io),
  fetched by IATA code and cached in memory

See [PRIVACY.md](PRIVACY.md) for what is sent where, and
[DISCLAIMER.md](DISCLAIMER.md) for the terms you accept when you run it.

## License

[MIT](LICENSE) © 2026 Alexey Popov
