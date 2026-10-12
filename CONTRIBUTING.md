# Contributing

Issues and pull requests are welcome. AirDash is a small hobby project,
so please open an issue before large changes.

## Layout

- `service/`: Go service (no third-party modules)
- `ui/`: Node (Express) server with a plain-JS web UI and Home Assistant widget
- `firmware/`: PlatformIO firmware, one env per board (`ttgo`, `sticks3`) plus `native` tests
- `scripts/`: flashing, bench fake service, Proxmox provisioning and deploy

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for how the pieces fit.

## Checks (what CI runs)

    (cd service && go vet ./... && go test -race ./...)
    (cd ui && npm ci && npm test)
    python3 -m unittest discover -s scripts/pve -p 'test_*.py'
    for f in scripts/*.sh; do bash -n "$f"; done
    (cd firmware && pio test -e native && pio run -e ttgo && pio run -e sticks3)

The firmware builds need `firmware/include/config.h`; copy it from
`config.h.example`.

## Conventions

- Tests first for behaviour changes. Pure logic (parsing, formatting,
  filters) is kept out of hardware/network code so it can be unit-tested.
- [Conventional Commits](https://www.conventionalcommits.org):
  `type(scope): description`, scopes `service`, `ui`, `firmware`,
  `scripts`, `deploy`, `ci`, `docs`.
- Comments explain *why*, not *what*.
- Never commit `.env`, `config.h`, API keys or your own coordinates.

By contributing you agree that your contribution is licensed under the
MIT License.
