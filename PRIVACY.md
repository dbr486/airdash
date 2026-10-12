# Privacy

AirDash is software you run yourself. The project has no servers, no
accounts, no analytics and no telemetry: nothing is ever sent to the
AirDash authors.

Your own AirDash service does talk to third-party services to do its
job. Every request carries your public IP address, plus the data below.

| Service | When | What it receives |
|---|---|---|
| [adsb.fi](https://adsb.fi) (default) or [adsb.lol](https://adsb.lol) | every few seconds | your home latitude/longitude and search radius |
| [adsbdb](https://www.adsbdb.com) | each new aircraft | its callsign |
| [Travelpayouts logo CDN](https://pics.avs.io) | each new airline, if logos are on | the airline's IATA code |
| [FlightAware AeroAPI](https://www.flightaware.com/commercial/aeroapi/) | only with `AIRDASH_AEROAPI_KEY` set, capped per day | the flight identifier, and your API key |
| [AirLabs](https://airlabs.co) | only with `AIRDASH_AIRLABS_KEY` set, every 100 min by default | the board airport code, and your API key |
| [OpenStreetMap Nominatim](https://nominatim.org) | only when you search an address on the settings page | the address you typed |

Each provider handles that data under its own privacy policy.

Your home position is therefore shared with the ADS-B provider. If that
matters to you, set a position a few kilometres away and enlarge the
radius to compensate.

## What stays on your machines

- `.env`: position, filters and API keys, on the machine running the service.
- The settings file (`/data/settings.json` in the `airdash-data` Docker
  volume): runtime changes made from the web page.
- `firmware/include/config.h`: WiFi name and password and the service URL,
  compiled into the display's flash.
- In-memory caches of routes, logos and flight status, lost on restart.

The web UI and the Home Assistant widget load nothing from third-party
hosts; logos are proxied through your own service.
