# Security

## Deployment model

AirDash is built for a trusted home network. The service API (port 8080)
and the web UI (port 3000) **have no authentication**: anyone who can
reach them can read the display and change the settings, including the
home position, with `PUT /api/settings`.

- Do not port-forward them or expose them to the internet. Use a VPN
  (WireGuard, Tailscale…) for remote access.
- API keys belong only in `.env`, which is gitignored and never served.
  WiFi credentials belong only in `firmware/include/config.h`, which is
  also gitignored. Never commit either file.
- `scripts/pve/create-lxc.py` reads Proxmox root credentials from
  `~/.airdash-proxmox.env`. Keep that file `chmod 600`, and set
  `PROXMOX_FINGERPRINT` so a spoofed host never receives the password.

## Reporting a vulnerability

Please use GitHub's **Report a vulnerability** button on the Security
tab of this repository (private vulnerability reporting). Do not open a
public issue. You can expect an answer within a couple of weeks. This is
a hobby project with no bounty.
