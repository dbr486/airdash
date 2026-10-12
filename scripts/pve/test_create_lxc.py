import hashlib, importlib.util, os, pathlib, shutil, socket, ssl, subprocess, tempfile, threading, unittest, urllib.parse

HERE = pathlib.Path(__file__).parent
spec = importlib.util.spec_from_file_location("create_lxc", HERE / "create-lxc.py")
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)


ENV = {"AIRDASH_LXC_IP": "192.0.2.50/24", "AIRDASH_LXC_GATEWAY": "192.0.2.1",
       "AIRDASH_LXC_NAMESERVERS": "192.0.2.53  198.51.100.53"}


def settings(**over):
    return m.lxc_settings(dict(ENV, **over))


class Tests(unittest.TestCase):
    def test_lxc_config(self):
        cfg = m.lxc_config("ssh-ed25519 AAAA+/== c", "pw", settings())
        self.assertEqual(cfg["vmid"], 103)
        self.assertEqual(cfg["hostname"], "airdash")
        self.assertEqual(cfg["ostemplate"], "local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst")
        self.assertEqual(cfg["rootfs"], "local-lvm:8")
        self.assertEqual((cfg["memory"], cfg["swap"], cfg["cores"]), (1024, 512, 2))
        self.assertEqual(cfg["net0"], "name=eth0,bridge=vmbr0,ip=192.0.2.50/24,gw=192.0.2.1")
        self.assertEqual(cfg["nameserver"], "192.0.2.53 198.51.100.53")
        self.assertEqual((cfg["unprivileged"], cfg["onboot"], cfg["start"]), (1, 1, 0))
        self.assertEqual(cfg["features"], "nesting=1,keyctl=1")
        self.assertEqual(cfg["ssh-public-keys"], "ssh-ed25519 AAAA+/== c")
        self.assertEqual(cfg["password"], "pw")

    def test_lxc_config_uses_overrides(self):
        s = settings(AIRDASH_LXC_VMID="210", AIRDASH_LXC_BRIDGE="vmbr1", AIRDASH_LXC_STORAGE="zfs",
                     AIRDASH_LXC_TEMPLATE="local:vztmpl/x.tar.zst", PROXMOX_NODE="node2")
        cfg = m.lxc_config("k", "pw", s)
        self.assertEqual((cfg["vmid"], cfg["rootfs"], cfg["ostemplate"]), (210, "zfs:8", "local:vztmpl/x.tar.zst"))
        self.assertIn("bridge=vmbr1", cfg["net0"])
        self.assertEqual(s["node"], "node2")

    def test_settings_require_ip_and_gateway(self):
        for key in ("AIRDASH_LXC_IP", "AIRDASH_LXC_GATEWAY"):
            env = dict(ENV)
            del env[key]
            with self.assertRaisesRegex(ValueError, key):
                m.lxc_settings(env)

    def test_settings_reject_ip_without_prefix(self):
        with self.assertRaisesRegex(ValueError, "/24"):
            settings(AIRDASH_LXC_IP="192.0.2.50")
        with self.assertRaisesRegex(ValueError, "AIRDASH_LXC_IP"):
            settings(AIRDASH_LXC_IP="not-an-ip/24")

    def test_settings_reject_bad_gateway_and_vmid(self):
        with self.assertRaisesRegex(ValueError, "AIRDASH_LXC_GATEWAY"):
            settings(AIRDASH_LXC_GATEWAY="gw.local")
        with self.assertRaisesRegex(ValueError, "AIRDASH_LXC_VMID"):
            settings(AIRDASH_LXC_VMID="abc")

    def test_settings_ignore_proxmox_vmid(self):
        self.assertEqual(settings(PROXMOX_VMID="150")["vmid"], 103)

    def test_settings_bare_ip_and_whitespace(self):
        s = settings(AIRDASH_LXC_IP=" 192.0.2.50/24\r")
        self.assertEqual((s["ip"], s["ip_cidr"]), ("192.0.2.50", "192.0.2.50/24"))

    def test_no_nameservers_leaves_dns_alone(self):
        s = settings(AIRDASH_LXC_NAMESERVERS="")
        self.assertEqual(s["nameservers"], "")
        self.assertNotIn("nameserver", m.lxc_config("k", "pw", s))
        self.assertNotIn("resolv.conf", m.docker_bootstrap(""))

    def test_settings_reject_non_ip_nameservers(self):
        for bad in ("1.1.1.1'; reboot; '", "dns.example", "192.0.2.53 $(id)"):
            with self.assertRaisesRegex(ValueError, "AIRDASH_LXC_NAMESERVERS"):
                settings(AIRDASH_LXC_NAMESERVERS=bad)

    def test_bootstrap_writes_nameservers(self):
        script = m.docker_bootstrap("192.0.2.53 198.51.100.53")
        self.assertIn("nameserver 192.0.2.53\\nnameserver 198.51.100.53\\n", script)
        self.assertIn("> /etc/resolv.conf", script)

    def test_read_env_file(self):
        with tempfile.NamedTemporaryFile("w", delete=False) as f:
            f.write("# comment\nA=1\nB=x=y\n\nC=\n")
        try:
            self.assertEqual(m.read_env_file(f.name), {"A": "1", "B": "x=y", "C": ""})
        finally:
            os.unlink(f.name)

    def test_pubkey_encoding(self):
        cfg = m.lxc_config("ssh-ed25519 AAAA+/== c", "pw", settings())
        body = m.encode_form(cfg)
        parsed = dict(urllib.parse.parse_qsl(body))
        self.assertEqual(parsed["ssh-public-keys"], "ssh-ed25519 AAAA+/== c")
        self.assertNotIn(" ", body)

    def test_owned_requires_hostname_and_address(self):
        s = settings()
        good = {"hostname": "airdash", "net0": "name=eth0,bridge=vmbr0,gw=192.0.2.1,hwaddr=AA:BB,ip=192.0.2.50/24,type=veth"}
        self.assertTrue(m.owned(good, s))
        self.assertFalse(m.owned(dict(good, hostname="homeassistant"), s))
        self.assertFalse(m.owned(dict(good, net0="name=eth0,bridge=vmbr0,ip=192.0.2.99/24"), s))
        self.assertFalse(m.owned({}, s))
        self.assertFalse(m.owned(None, s))

    def test_plan_skips_existing(self):
        self.assertEqual(m.plan(exists=True, running=True), [])
        self.assertEqual(m.plan(exists=True, running=False), ["start"])
        self.assertEqual(m.plan(exists=False, running=False), ["create", "start"])

    def test_task_failure_detected(self):
        self.assertTrue(m.task_ok({"status": "stopped", "exitstatus": "OK"}))
        self.assertFalse(m.task_ok({"status": "stopped", "exitstatus": "unable to create CT 103"}))
        self.assertFalse(m.task_ok({"status": "running"}))

    def test_random_password(self):
        p = m.random_password()
        self.assertGreaterEqual(len(p), 24)
        self.assertNotEqual(p, m.random_password())

    def test_missing_ct_error_scoped_to_status_current(self):
        self.assertTrue(m.missing_ct_error("GET", "/nodes/pve/lxc/103/status/current", 500))
        self.assertFalse(m.missing_ct_error("GET", "/nodes/pve/tasks/UPID/status", 500))
        self.assertFalse(m.missing_ct_error("POST", "/nodes/pve/lxc/103/status/current", 500))
        self.assertFalse(m.missing_ct_error("GET", "/nodes/pve/lxc/103/status/current", 403))

    def test_fingerprint_matches_ignores_colons_and_case(self):
        der = b"not-really-a-cert"
        hexd = hashlib.sha256(der).hexdigest()
        colon = ":".join(hexd[i:i + 2] for i in range(0, len(hexd), 2)).upper()
        self.assertTrue(m.fingerprint_matches(colon, der))
        self.assertTrue(m.fingerprint_matches(hexd, der))
        self.assertTrue(m.fingerprint_matches(" " + colon.lower() + "\n", der))
        self.assertFalse(m.fingerprint_matches(colon, der + b"x"))
        self.assertFalse(m.fingerprint_matches("", der))

    def test_unpinned_certificate_stops_before_credentials(self):
        der = b"cert"
        stop = m.certificate_problem("", der)
        self.assertIn("PROXMOX_FINGERPRINT=" + m.format_fingerprint(der), stop)
        self.assertIn("not sent", stop)
        self.assertIn("does not match", m.certificate_problem("AA:BB", der))
        self.assertIsNone(m.certificate_problem(m.format_fingerprint(der), der))

    def test_format_fingerprint_is_colon_separated_upper(self):
        der = b"abc"
        out = m.format_fingerprint(der)
        self.assertEqual(out, ":".join(hashlib.sha256(der).hexdigest()[i:i + 2] for i in range(0, 64, 2)).upper())




@unittest.skipUnless(shutil.which("openssl"), "needs openssl to mint a test certificate")
class PinnedConnection(unittest.TestCase):
    """A real TLS server with a throwaway self-signed certificate: the pin is
    checked on the very connection that carries the request."""

    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        cert, key = (os.path.join(self.dir.name, n) for n in ("c.pem", "k.pem"))
        subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-subj", "/CN=pve",
                        "-keyout", key, "-out", cert], check=True, capture_output=True)
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        ctx.load_cert_chain(cert, key)
        self.der = ssl.PEM_cert_to_DER_cert(pathlib.Path(cert).read_text())
        self.listener = socket.socket()
        self.listener.bind(("127.0.0.1", 0))
        self.listener.listen()
        self.port = self.listener.getsockname()[1]
        self.received = b""

        def serve():
            try:
                conn, _ = self.listener.accept()
                with ctx.wrap_socket(conn, server_side=True) as tls:
                    tls.settimeout(2)
                    try:
                        # Headers and body may arrive in separate writes.
                        while b"\r\n\r\n" not in self.received:
                            chunk = tls.recv(65536)
                            if not chunk:
                                return
                            self.received += chunk
                        head, _, body = self.received.partition(b"\r\n\r\n")
                        length = int(head.lower().split(b"content-length:")[1].split(b"\r\n")[0]) if b"content-length:" in head.lower() else 0
                        while len(body) < length:
                            chunk = tls.recv(65536)
                            if not chunk:
                                break
                            body += chunk
                            self.received += chunk
                    except (OSError, ssl.SSLError):
                        return
                    if self.received:
                        tls.sendall(b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 13\r\n\r\n{\"data\":\"ok\"}")
            except (OSError, ssl.SSLError):
                pass

        self.thread = threading.Thread(target=serve, daemon=True)
        self.thread.start()

    def tearDown(self):
        self.listener.close()
        self.thread.join(3)
        self.dir.cleanup()

    def request(self, pin):
        conn = m.PinnedHTTPSConnection("127.0.0.1", self.port, pin, timeout=5)
        try:
            conn.request("POST", "/api2/json/access/ticket", body="password=secret")
            return conn.getresponse().read()
        finally:
            conn.close()

    def test_wrong_pin_sends_nothing(self):
        with self.assertRaises(m.PinError):
            self.request("AA:BB")
        self.thread.join(3)
        self.assertNotIn(b"secret", self.received)

    def test_empty_pin_sends_nothing(self):
        with self.assertRaisesRegex(m.PinError, "credentials were not sent"):
            self.request("")
        self.thread.join(3)
        self.assertNotIn(b"secret", self.received)

    def test_matching_pin_completes_request(self):
        self.assertEqual(self.request(m.format_fingerprint(self.der)), b'{"data":"ok"}')
        self.assertIn(b"password=secret", self.received)

if __name__ == "__main__":
    unittest.main()
