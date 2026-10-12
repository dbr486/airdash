import os, pathlib, subprocess, tempfile, unittest

SCRIPT = pathlib.Path(__file__).resolve().parent.parent / "deploy-lxc.sh"


def deploy(remote_hostname, ip="192.0.2.50"):
    # Fake ssh/rsync on PATH: ssh answers `hostname` with remote_hostname and
    # succeeds otherwise; rsync leaves a marker so the test sees whether the
    # script got as far as copying anything.
    with tempfile.TemporaryDirectory() as d:
        bindir = pathlib.Path(d, "bin"); bindir.mkdir()
        pathlib.Path(d, ".ssh").mkdir()
        pathlib.Path(d, ".ssh", "id_ed25519").write_text("k")
        marker = pathlib.Path(d, "rsync-called")
        (bindir / "ssh").write_text('#!/bin/sh\ncase "$*" in *hostname*) echo "$FAKE_HOSTNAME";; esac\nexit 0\n')
        (bindir / "rsync").write_text(f'#!/bin/sh\ntouch "{marker}"\n')
        # docker/curl/sleep fail or return at once so a script that gets past
        # the guard finishes quickly instead of polling a fake address.
        for f in ("docker", "curl"):
            (bindir / f).write_text("#!/bin/sh\nexit 1\n")
        (bindir / "sleep").write_text("#!/bin/sh\nexit 0\n")
        for f in ("ssh", "rsync", "docker", "curl", "sleep"):
            (bindir / f).chmod(0o755)
        env = dict(os.environ, HOME=d, AIRDASH_LXC_IP=ip, FAKE_HOSTNAME=remote_hostname,
                   PATH=f"{bindir}:{os.environ['PATH']}")
        r = subprocess.run(["bash", str(SCRIPT)], env=env, capture_output=True, text=True)
        return r, marker.exists()


def target(env_file_text=None, ip_env=None):
    with tempfile.TemporaryDirectory() as d:
        env = {k: v for k, v in os.environ.items() if k != "AIRDASH_LXC_IP"}
        env["HOME"] = d
        env["AIRDASH_PROXMOX_ENV"] = os.path.join(d, "pve.env")
        if env_file_text is not None:
            pathlib.Path(env["AIRDASH_PROXMOX_ENV"]).write_text(env_file_text)
        if ip_env is not None:
            env["AIRDASH_LXC_IP"] = ip_env
        return subprocess.run(["bash", str(SCRIPT), "--print-target"], env=env, capture_output=True, text=True)


class DeployTarget(unittest.TestCase):
    def test_deploy_target_strips_prefix_and_whitespace(self):
        r = target("PROXMOX_HOST=x\r\nAIRDASH_LXC_IP=192.0.2.50/24 \r\n")
        self.assertEqual((r.returncode, r.stdout), (0, "192.0.2.50\n"))

    def test_environment_wins_over_file(self):
        r = target("AIRDASH_LXC_IP=192.0.2.50/24\n", ip_env="192.0.2.77")
        self.assertEqual(r.stdout, "192.0.2.77\n")

    def test_missing_target_fails_with_hint(self):
        r = target()
        self.assertEqual(r.returncode, 1)
        self.assertIn("AIRDASH_LXC_IP", r.stderr)


    def test_rejects_non_ip_target(self):
        r = target(ip_env="nas.local;rm")
        self.assertEqual(r.returncode, 1)
        self.assertIn("not an IP address", r.stderr)

    def test_refuses_host_that_is_not_the_airdash_container(self):
        r, rsynced = deploy("nas")
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("not the AirDash container", r.stderr)
        self.assertFalse(rsynced)


if __name__ == "__main__":
    unittest.main()
