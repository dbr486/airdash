#!/usr/bin/env python3
"""Create and bootstrap the AirDash LXC on the Proxmox host.

Reads ~/.airdash-proxmox.env (never printed). Idempotent: an existing
container is left alone except for being started; Docker is installed only
when missing. --dry-run prints the container config and exits.

Keys in ~/.airdash-proxmox.env:
  PROXMOX_HOST, PROXMOX_PORT, PROXMOX_USER, PROXMOX_PASSWORD   required
  PROXMOX_FINGERPRINT       pins the PVE certificate (SHA-256); the first run prints it
  AIRDASH_LXC_IP            required, with prefix, e.g. 192.0.2.50/24
  AIRDASH_LXC_GATEWAY       required, e.g. 192.0.2.1
  AIRDASH_LXC_NAMESERVERS   optional, space-separated; default: host DNS
  AIRDASH_LXC_VMID          default 103
  PROXMOX_NODE              default pve
  AIRDASH_LXC_BRIDGE        default vmbr0
  AIRDASH_LXC_STORAGE       default local-lvm
  AIRDASH_LXC_TEMPLATE      default local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst
"""
import argparse, hashlib, http.client, ipaddress, json, os, secrets, ssl, subprocess, sys, time, urllib.parse

HOSTNAME = "airdash"
ENV_FILE = os.path.expanduser("~/.airdash-proxmox.env")

# Everything site-specific comes from ENV_FILE so the script runs unchanged
# on any Proxmox host. The VMID deliberately has its own AIRDASH_ key: a
# stray generic PROXMOX_VMID must never point the script at another guest.
DEFAULTS = {
    "AIRDASH_LXC_VMID": "103",
    "PROXMOX_NODE": "pve",
    "AIRDASH_LXC_BRIDGE": "vmbr0",
    "AIRDASH_LXC_STORAGE": "local-lvm",
    "AIRDASH_LXC_TEMPLATE": "local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst",
    "AIRDASH_LXC_NAMESERVERS": "",
}


def lxc_settings(env: dict) -> dict:
    def get(k):
        return (env.get(k) or DEFAULTS.get(k, "")).strip()

    for k in ("AIRDASH_LXC_IP", "AIRDASH_LXC_GATEWAY"):
        if not get(k):
            raise ValueError(f"{k} missing")
    ip_cidr = get("AIRDASH_LXC_IP")
    try:
        iface = ipaddress.ip_interface(ip_cidr)
    except ValueError:
        iface = None
    # A bare address would silently become a /32 and cut the LXC off the LAN.
    if iface is None or "/" not in ip_cidr:
        raise ValueError(f"AIRDASH_LXC_IP={ip_cidr!r} must be an address with its prefix, e.g. 192.0.2.50/24")
    gateway = get("AIRDASH_LXC_GATEWAY")
    try:
        ipaddress.ip_address(gateway)
    except ValueError:
        raise ValueError(f"AIRDASH_LXC_GATEWAY={gateway!r} is not an IP address") from None
    nameservers = get("AIRDASH_LXC_NAMESERVERS").split()
    # These end up in a shell command run as root in the container, so only
    # literal IP addresses are allowed through.
    for ns in nameservers:
        try:
            ipaddress.ip_address(ns)
        except ValueError:
            raise ValueError(f"AIRDASH_LXC_NAMESERVERS: {ns!r} is not an IP address") from None
    vmid = get("AIRDASH_LXC_VMID")
    if not vmid.isdigit():
        raise ValueError(f"AIRDASH_LXC_VMID={vmid!r} is not a number")
    return {
        "vmid": int(vmid), "node": get("PROXMOX_NODE"), "bridge": get("AIRDASH_LXC_BRIDGE"),
        "storage": get("AIRDASH_LXC_STORAGE"), "template": get("AIRDASH_LXC_TEMPLATE"),
        "ip": str(iface.ip), "ip_cidr": ip_cidr, "gateway": gateway,
        "nameservers": " ".join(nameservers),
    }


def lxc_config(pubkey: str, password: str, s: dict) -> dict:
    cfg = {
        "vmid": s["vmid"], "hostname": HOSTNAME, "ostemplate": s["template"], "rootfs": f"{s['storage']}:8",
        "memory": 1024, "swap": 512, "cores": 2,
        "net0": f"name=eth0,bridge={s['bridge']},ip={s['ip_cidr']},gw={s['gateway']}",
        "unprivileged": 1, "features": "nesting=1,keyctl=1", "onboot": 1, "start": 0,
        "ssh-public-keys": pubkey, "password": password,
    }
    # Without explicit resolvers the container inherits the host's DNS.
    if s["nameservers"]:
        cfg["nameserver"] = s["nameservers"]
    return cfg


def encode_form(cfg: dict) -> str:
    return urllib.parse.urlencode({k: str(v) for k, v in cfg.items()})


def plan(exists: bool, running: bool) -> list:
    steps = []
    if not exists:
        steps.append("create")
    if not exists or not running:
        steps.append("start")
    return steps


def task_ok(status: dict) -> bool:
    return status.get("status") == "stopped" and status.get("exitstatus") == "OK"


def owned(config, s: dict) -> bool:
    # An existing VMID is only reused when it is provably the AirDash
    # container: same hostname and the same static address on net0.
    # Otherwise this script would start, reconfigure and bootstrap Docker
    # on somebody else's container.
    if not config:
        return False
    net0 = dict(kv.split("=", 1) for kv in config.get("net0", "").split(",") if "=" in kv)
    return config.get("hostname") == HOSTNAME and net0.get("ip") == s["ip_cidr"]


def random_password() -> str:
    return secrets.token_urlsafe(24)


def missing_ct_error(method: str, path: str, code: int) -> bool:
    # PVE answers 500 (not 404) for a missing CT's status/current — but only
    # that endpoint; a 500 anywhere else (including a task-status poll) is a
    # real failure and must not be swallowed behind an 8-minute timeout.
    return method == "GET" and code == 500 and path.endswith("/status/current")


def format_fingerprint(der: bytes) -> str:
    h = hashlib.sha256(der).hexdigest().upper()
    return ":".join(h[i:i + 2] for i in range(0, len(h), 2))


def fingerprint_matches(expected: str, der: bytes) -> bool:
    want = "".join(ch for ch in expected.strip().lower() if ch not in ":")
    return bool(want) and secrets.compare_digest(want, hashlib.sha256(der).hexdigest())


def certificate_problem(expected: str, der: bytes):
    # Fail closed: with no pinned fingerprint a spoofed host on the LAN would
    # receive the root password, so the first run only reports the
    # fingerprint to pin and stops before any credentials leave this machine.
    if not expected.strip():
        return ("PROXMOX_FINGERPRINT is not set, so credentials were not sent. Check this fingerprint "
                "against the Proxmox web UI (node > System > Certificates), add it to "
                f"{ENV_FILE} and re-run:\n  PROXMOX_FINGERPRINT={format_fingerprint(der)}")
    if not fingerprint_matches(expected, der):
        return (f"certificate fingerprint {format_fingerprint(der)} does not match "
                "PROXMOX_FINGERPRINT; refusing to send credentials")
    return None


class PinError(Exception):
    pass


class PinnedHTTPSConnection(http.client.HTTPSConnection):
    """HTTPS to a self-signed Proxmox host, authenticated by fingerprint.

    The CA check is off (PVE ships a self-signed certificate), so the pin is
    what authenticates the host. It is checked on this very connection right
    after the handshake, before any request bytes (and so the password) are
    written; a separate probe connection could be raced or rerouted.
    """

    def __init__(self, host, port, fingerprint, timeout=30):
        ctx = ssl.create_default_context()
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
        super().__init__(host, port, context=ctx, timeout=timeout)
        self.fingerprint = fingerprint

    def connect(self):
        super().connect()
        problem = certificate_problem(self.fingerprint, self.sock.getpeercert(binary_form=True))
        if problem:
            self.sock.close()
            self.sock = None
            raise PinError(problem)


def read_env_file(path: str) -> dict:
    env = {}
    with open(path) as f:
        for line in f:
            line = line.rstrip("\r\n")
            if "=" in line and not line.startswith("#"):
                k, v = line.split("=", 1)
                env[k.strip()] = v
    return env


def require_credentials(env: dict) -> None:
    for k in ("PROXMOX_HOST", "PROXMOX_PORT", "PROXMOX_USER", "PROXMOX_PASSWORD"):
        if not env.get(k):
            sys.exit(f"{ENV_FILE}: {k} missing")


class PVE:
    def __init__(self, env, node):
        self.node = node
        self.host, self.port = env["PROXMOX_HOST"], int(env["PROXMOX_PORT"])
        self.fingerprint = env.get("PROXMOX_FINGERPRINT", "")
        self.headers = {}
        data = self._call("POST", "/access/ticket", {"username": env["PROXMOX_USER"], "password": env["PROXMOX_PASSWORD"]}, auth=False)
        self.headers = {"Cookie": "PVEAuthCookie=" + data["ticket"], "CSRFPreventionToken": data["CSRFPreventionToken"]}

    def _call(self, method, path, form=None, auth=True):
        headers = dict(self.headers) if auth else {}
        body = None
        if form is not None:
            body = encode_form(form)
            headers["Content-Type"] = "application/x-www-form-urlencoded"
        conn = PinnedHTTPSConnection(self.host, self.port, self.fingerprint)
        try:
            conn.request(method, "/api2/json" + path, body=body, headers=headers)
            r = conn.getresponse()
            raw = r.read()
        except PinError as e:
            sys.exit(f"proxmox {self.host}:{self.port}: {e}")
        except (OSError, http.client.HTTPException) as e:
            sys.exit(f"proxmox {method} {path}: {e}")
        finally:
            conn.close()
        if r.status >= 400:
            if missing_ct_error(method, path, r.status):
                return None
            sys.exit(f"proxmox {method} {path}: HTTP {r.status} {raw.decode(errors='replace')[:200]}")
        return json.loads(raw)["data"]

    def get(self, path):
        return self._call("GET", path)

    def post(self, path, form=None):
        return self._call("POST", path, form or {})

    def put(self, path, form=None):
        return self._call("PUT", path, form or {})

    def wait_task(self, upid, what):
        for _ in range(240):
            st = self.get(f"/nodes/{self.node}/tasks/{urllib.parse.quote(upid, safe='')}/status")
            if st and st.get("status") == "stopped":
                if not task_ok(st):
                    sys.exit(f"{what} failed: {st.get('exitstatus')}")
                return
            time.sleep(2)
        sys.exit(f"{what}: timed out waiting for task")


def ssh_key() -> tuple:
    home = os.path.expanduser("~/.ssh")
    for name in ("id_ed25519", "airdash_ed25519"):
        if os.path.exists(f"{home}/{name}.pub"):
            return f"{home}/{name}", open(f"{home}/{name}.pub").read().strip()
    os.makedirs(home, mode=0o700, exist_ok=True)
    subprocess.run(["ssh-keygen", "-t", "ed25519", "-N", "", "-C", "airdash-deploy", "-f", f"{home}/airdash_ed25519", "-q"], check=True)
    return f"{home}/airdash_ed25519", open(f"{home}/airdash_ed25519.pub").read().strip()


def ssh(keyfile, ip, cmd, check=True, capture=False):
    return subprocess.run(["ssh", "-i", keyfile, "-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=5",
                           f"root@{ip}", cmd], check=check, capture_output=capture, text=True)


def wait_ssh(keyfile, ip):
    for _ in range(60):
        r = ssh(keyfile, ip, "true", check=False, capture=True)
        if r.returncode == 0:
            return
        time.sleep(3)
    hint = ""
    if "HOST IDENTIFICATION HAS CHANGED" in r.stderr:
        hint = f"; the container was rebuilt, run: ssh-keygen -R {ip}"
    sys.exit(f"ssh root@{ip} not reachable after 3 minutes{hint}\n{r.stderr.strip()}")


# Proxmox regenerates /etc/resolv.conf from the container's pct-level
# `nameserver` config on each start, so the PUT reconciliation in main() sets
# the authoritative value when nameservers are configured. This just writes
# the same resolvers into the file now, so DNS works immediately without
# waiting for a restart. Overwriting (not appending) keeps it idempotent.
def docker_bootstrap(nameservers: str) -> str:
    resolv = ""
    if nameservers:
        lines = "".join(f"nameserver {ns}\\n" for ns in nameservers.split())
        resolv = f"printf '{lines}' > /etc/resolv.conf; "
    return (
        "set -e; export DEBIAN_FRONTEND=noninteractive; " + resolv +
        "apt-get update -qq && apt-get install -y -qq ca-certificates curl rsync >/dev/null; "
        "if ! docker --version >/dev/null 2>&1; then curl -fsSL https://get.docker.com | sh >/dev/null; fi; "
        "systemctl enable --now docker >/dev/null 2>&1 || true; docker compose version"
    )


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--dry-run", action="store_true", help="print the container config and exit")
    args = ap.parse_args()
    try:
        env = read_env_file(ENV_FILE)
        s = lxc_settings(env)
    except FileNotFoundError:
        sys.exit(f"{ENV_FILE} not found; see README, Deploy to Proxmox")
    except ValueError as e:
        sys.exit(f"{ENV_FILE}: {e}")
    keyfile, pubkey = ssh_key()
    cfg = lxc_config(pubkey, random_password(), s)
    if args.dry_run:
        shown = dict(cfg, password="<random>", **{"ssh-public-keys": pubkey[:20] + "..."})
        print(json.dumps(shown, indent=2))
        return
    require_credentials(env)
    node, vmid, ip = s["node"], s["vmid"], s["ip"]
    pve = PVE(env, node)
    status = pve.get(f"/nodes/{node}/lxc/{vmid}/status/current")
    exists = status is not None
    if exists and not owned(pve.get(f"/nodes/{node}/lxc/{vmid}/config"), s):
        sys.exit(f"LXC {vmid} exists but is not the AirDash container (hostname {HOSTNAME!r}, ip {s['ip_cidr']}); refusing to touch it")
    running = bool(status and status.get("status") == "running")
    steps = plan(exists, running)
    print(f"LXC {vmid}: exists={exists} running={running} -> {steps or ['nothing to do']}")
    if "create" in steps:
        pve.wait_task(pve.post(f"/nodes/{node}/lxc", cfg), "create")
        print("created")
    if "start" in steps:
        pve.wait_task(pve.post(f"/nodes/{node}/lxc/{vmid}/status/start"), "start")
        print("started")
    # Reconciled every run, not just on create, so a hand-edited container
    # converges back to the configured resolvers.
    if s["nameservers"]:
        pve.put(f"/nodes/{node}/lxc/{vmid}/config", {"nameserver": s["nameservers"]})
        print("nameserver set")
    wait_ssh(keyfile, ip)
    print("bootstrapping docker (idempotent) ...")
    try:
        ssh(keyfile, ip, docker_bootstrap(s["nameservers"]))
    except subprocess.CalledProcessError as e:
        sys.exit(f"docker bootstrap failed on {ip} (exit {e.returncode}); ssh in and check apt/docker")
    print(f"ready: ssh -i {keyfile} root@{ip}")


if __name__ == "__main__":
    main()
