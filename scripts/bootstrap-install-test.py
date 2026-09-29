#!/usr/bin/env python3
"""Mirror bootstrap tests: synthetic releases and fake installers, no host services."""
import copy
import gzip
import hashlib
import io
import json
import os
import pty
from pathlib import Path
import subprocess
import tarfile
import tempfile
import time
import signal
import zlib
import types
import unittest
from unittest.mock import patch

SCRIPT = Path(__file__).resolve().with_name("bootstrap-install.sh")
SOURCE = SCRIPT.read_text().split("<<'PY_BOOTSTRAP'\n", 1)[1].rsplit("\nPY_BOOTSTRAP", 1)[0]
BOOT = types.ModuleType("bootstrap_test_module")
exec(compile(SOURCE, str(SCRIPT), "exec"), BOOT.__dict__)


def sha(data):
    return hashlib.sha256(data).hexdigest()


def archive(files, mutate=None):
    stream = io.BytesIO()
    package = "proxyscene_bundle_linux_amd64"
    entries = []
    root = tarfile.TarInfo(package)
    root.type, root.mode = tarfile.DIRTYPE, 0o755
    entries.append((root, b""))
    for name, data in files.items():
        item = tarfile.TarInfo(package + "/" + name)
        item.size = len(data)
        item.mode = 0o755 if name in ("install.sh", "proxyscene", "xray") else 0o644
        entries.append((item, data))
    if mutate:
        mutate(entries)
    with tarfile.open(fileobj=stream, mode="w:gz", format=tarfile.USTAR_FORMAT) as tar:
        for item, data in entries:
            tar.addfile(item, io.BytesIO(data) if item.isfile() else None)
    return stream.getvalue()


class BootstrapTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="proxyscene-bootstrap-test.")
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name)
        self.marker = self.path / "called"
        self.stdin = tempfile.TemporaryFile()
        self.stdin.write(b"retained stdin\n")
        self.stdin.seek(0)
        self.addCleanup(self.stdin.close)
        self.files = {name: (name + "\n").encode() for name in BOOT.COMPONENTS}
        self.files["install.sh"] = ("#!/bin/bash\nset -eu\nread -r input\nprintf '%s\\n' \"$input\" \"$@\" \"${SUDO_USER-unset}\" \"${SKIP_MANAGER_INIT-unset}\" > '" + str(self.marker) + "'\n").encode()
        self.regenerate_manifest()
        self.payload = archive(self.files)
        self.metadata = {"schema_version": 1, "version": "0.11.0", "tag": "v0.11.0", "commit": "a" * 40,
                         "release_id": 42, "published_at": "2026-09-29T00:00:00Z", "notes": "Test release.", "assets": {}}
        names = ["install.sh", "checksums.txt", "xray_source_v26.9.9.tar.gz"]
        for arch in BOOT.ARCHES:
            names += ["proxyscene_linux_" + arch + ".tar.gz", "proxyscene_bundle_linux_" + arch + ".tar.gz"]
        self.metadata["assets"] = {name: {"size": 1, "sha256": "b" * 64} for name in names}
        self.refresh_assets()
        self.latest = {key: self.metadata[key] for key in ("version", "tag", "commit", "release_id", "published_at")}
        self.latest["base_url"] = BOOT.BASE + "/v0.11.0"
        self.requests = []

    def regenerate_manifest(self):
        self.files["bundle-manifest.sha256"] = "".join(sha(self.files[name]) + "  " + name + "\n" for name in sorted(BOOT.COMPONENTS)).encode()

    def refresh_assets(self):
        self.metadata["assets"]["install.sh"] = {"size": len(self.files["install.sh"]), "sha256": sha(self.files["install.sh"])}
        self.metadata["assets"]["proxyscene_bundle_linux_amd64.tar.gz"] = {"size": len(self.payload), "sha256": sha(self.payload)}
        self.checksums = "".join(asset["sha256"] + "  " + name + "\n" for name, asset in sorted(self.metadata["assets"].items()) if name != "checksums.txt").encode()
        self.metadata["assets"]["checksums.txt"] = {"size": len(self.checksums), "sha256": sha(self.checksums)}

    def fetch(self, relative, target, limit):
        self.requests.append(relative)
        data = {"latest.json": json.dumps(self.latest).encode(),
                "metadata/v0.11.0.json": json.dumps(self.metadata).encode(),
                "v0.11.0/checksums.txt": self.checksums,
                "v0.11.0/proxyscene_bundle_linux_amd64.tar.gz": self.payload}[relative]
        target.write_bytes(data)
        BOOT.require(len(data) <= limit, "mock download too large")

    def run_install(self, args=None, precondition=None, fetch=None):
        temp_factory = tempfile.TemporaryDirectory
        def private_temp():
            return temp_factory(prefix="proxyscene-bootstrap.", dir=self.path)
        with patch.object(BOOT.os, "geteuid", return_value=0), \
             patch.object(BOOT.shutil, "which", return_value="/usr/bin/mock-tool"), \
             patch.object(BOOT.platform, "machine", return_value="x86_64"), \
             patch.object(BOOT, "trusted_chain"), \
             patch.object(BOOT, "verify_default_installation"), \
             patch.object(BOOT, "installed_state", return_value=precondition or ["--expected-manager-absent"]), \
             patch.object(BOOT, "download", side_effect=fetch or self.fetch), \
             patch.object(BOOT, "private_workspace", side_effect=private_temp):
            return BOOT.install(args or [], stdin=self.stdin)

    def assert_not_installed(self):
        self.assertFalse(self.marker.exists())
        self.assertEqual(list(self.path.glob("proxyscene-bootstrap.*")), [])

    def test_source_is_inert(self):
        result = subprocess.run(["/bin/bash", "-c", 'source "$1"; printf "source-safe\\n"', "_", str(SCRIPT)], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "source-safe\n")

    def test_help_does_not_need_root_or_network(self):
        result = subprocess.run(["/bin/bash", str(SCRIPT), "--help"], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("dl.ll.cd", result.stdout)

    def test_full_install_keeps_stdin_and_arguments_and_cleans(self):
        with patch.dict(os.environ, {"SKIP_MANAGER_INIT": "1", "PROXYSCENE_BASE_URL": "https://evil.invalid"}):
            self.assertEqual(self.run_install(), 0)
        lines = self.marker.read_text().splitlines()
        self.assertEqual(lines[:3], ["retained stdin", "--offline", "--expected-manager-absent"])
        self.assertEqual(lines[-1], "unset")
        self.assertEqual(self.requests, ["latest.json", "metadata/v0.11.0.json", "v0.11.0/checksums.txt", "v0.11.0/proxyscene_bundle_linux_amd64.tar.gz", "metadata/v0.11.0.json"])
        self.assertEqual(list(self.path.glob("proxyscene-bootstrap.*")), [])

    def shell_fixture(self):
        # Execute the real Bash/FD-3 wrapper with a test-local copy whose Python
        # network and host paths are replaced. The production script has no
        # environment-based test hooks or configurable download endpoints.
        mirror = self.path / "mirror"
        mirror.mkdir()
        payloads = {"latest.json": json.dumps(self.latest).encode(),
                    "metadata/v0.11.0.json": json.dumps(self.metadata).encode(),
                    "v0.11.0/checksums.txt": self.checksums,
                    "v0.11.0/proxyscene_bundle_linux_amd64.tar.gz": self.payload}
        for name, data in payloads.items():
            target = mirror / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(data)
        prelude = f"""
TEST_ROOT = Path({str(self.path)!r})
def test_download(relative, destination, limit):
    data = (TEST_ROOT / 'mirror' / relative).read_bytes()
    require(len(data) <= limit, 'oversize fixture')
    destination.write_bytes(data)
download = test_download
trusted_chain = lambda path: None
verify_default_installation = lambda: None
installed_state = lambda *args: ['--expected-manager-absent']
shutil.which = lambda *args, **kwargs: '/usr/bin/test-command'
os.geteuid = lambda: 0
platform.machine = lambda: 'x86_64'
test_original_temp = tempfile.TemporaryDirectory
private_workspace = lambda: test_original_temp(prefix='proxyscene-bootstrap.', dir=TEST_ROOT)
"""
        executable = self.path / "test-bootstrap.sh"
        executable.write_text(SCRIPT.read_text().replace('if __name__ == "__main__":', prelude + '\nif __name__ == "__main__":'))
        return executable

    def test_shell_wrapper_preserves_input_for_offline_installer(self):
        executable = self.shell_fixture()
        result = subprocess.run(["/bin/bash", str(executable)], input="retained stdin\n", text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.marker.read_text().splitlines()[:3], ["retained stdin", "--offline", "--expected-manager-absent"])
        self.assertEqual(list(self.path.glob("proxyscene-bootstrap.*")), [])

    def test_cancellation_waits_for_real_installer_child_before_cleanup(self):
        done = self.path / "child-done"
        ready = self.path / "child-ready"
        self.files["install.sh"] = ("#!/bin/bash\ntrap 'exit 143' TERM\n"
            "/bin/bash -c 'trap '\"'\"'sleep .3; if [[ -f $1 ]]; then printf retained > \"$2\"; fi; exit 0'\"'\"' TERM; printf ready > \"$3\"; while :; do sleep 30; done' _ \"$0\" '" + str(done) + "' '" + str(ready) + "' &\nwait\n").encode()
        self.regenerate_manifest()
        self.payload = archive(self.files)
        self.refresh_assets()
        executable = self.shell_fixture()
        process = subprocess.Popen(["/bin/bash", str(executable)], stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            deadline = time.monotonic() + 5
            while not ready.exists() and process.poll() is None and time.monotonic() < deadline:
                time.sleep(.02)
            self.assertTrue(ready.exists(), "fake installer child did not start")
            process.send_signal(signal.SIGTERM)
            stdout, stderr = process.communicate(timeout=15)
            self.assertNotEqual(process.returncode, 0, stdout + stderr)
            self.assertEqual(done.read_text(), "retained")
            self.assertEqual(list(self.path.glob("proxyscene-bootstrap.*")), [])
        finally:
            if process.poll() is None:
                process.kill()
                process.wait()

    def test_metadata_changes_during_download_prevent_installer(self):
        seen = 0
        def fetch(relative, destination, limit):
            nonlocal seen
            if relative == "metadata/v0.11.0.json":
                seen += 1
                if seen == 2:
                    self.metadata["notes"] = "Changed during download"
            self.fetch(relative, destination, limit)
        with self.assertRaisesRegex(BOOT.Rejected, "元数据发生变化"):
            self.run_install(fetch=fetch)
        self.assertEqual(seen, 2)
        self.assert_not_installed()

    def test_gzip_crc_trailing_stream_hidden_tar_data_rejected(self):
        original = self.payload
        raw = gzip.decompress(original)
        tails = [original[:-8] + bytes([original[-8] ^ 255]) + original[-7:],
                 original + gzip.compress(b"second stream"), original + b"trailing bytes",
                 gzip.compress(raw + b"hidden tar data"), original[:-8]]
        for payload in tails:
            self.payload = payload
            self.refresh_assets()
            with self.subTest(size=len(payload)), self.assertRaises((BOOT.Rejected, zlib.error)):
                self.run_install()
            self.assert_not_installed()

    def test_os_file_size_limit_stops_writes_without_content_length(self):
        output = self.path / "bounded-download"
        result = subprocess.run(["/usr/bin/python3", "-c", "import sys; open(sys.argv[1], 'wb').write(b'x' * 100000)", str(output)],
                                preexec_fn=BOOT.download_limit(100), capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertLessEqual(output.stat().st_size, 100)

    def test_real_terminal_stdin_is_preserved(self):
        self.files["install.sh"] = ("#!/bin/bash\nset -eu\n[[ -t 0 ]]\nread -r input\nprintf '%s' \"$input\" > '" + str(self.marker) + "'\n").encode()
        self.regenerate_manifest()
        self.payload = archive(self.files)
        self.refresh_assets()
        executable = self.shell_fixture()
        master, slave = pty.openpty()
        try:
            process = subprocess.Popen(["/bin/bash", str(executable)], stdin=slave, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            os.write(master, b"terminal retained\n")
            stdout, stderr = process.communicate(timeout=10)
            self.assertEqual(process.returncode, 0, stdout + stderr)
            self.assertEqual(self.marker.read_text(), "terminal retained")
        finally:
            os.close(master)
            os.close(slave)

    def test_pinned_install_skips_latest_and_keeps_digest_cas(self):
        self.run_install(["--version", "v0.11.0"], ["--expected-manager-sha256", "c" * 64])
        self.assertEqual(self.requests[0], "metadata/v0.11.0.json")
        self.assertEqual(self.marker.read_text().splitlines()[1:4], ["--offline", "--expected-manager-sha256", "c" * 64])

    def test_disallows_old_versions_and_arbitrary_flags(self):
        for args in (["--version", "v0.10.0"], ["--version", "latest"], ["--offline"], ["--url", "https://evil.invalid"], ["--version", "v01.2.3"]):
            with self.subTest(args=args), self.assertRaises((BOOT.Rejected, SystemExit)):
                self.run_install(args)
        self.assertEqual(self.requests, [])
        self.assert_not_installed()

    def test_metadata_rejections_do_not_run_installer(self):
        changes = [lambda d: d.update(schema_version=True), lambda d: d.update(commit="G" * 40),
                   lambda d: d.update(release_id=True), lambda d: d.update(published_at="2026-02-31T00:00:00Z"),
                   lambda d: d.update(version="0.12.0"), lambda d: d.update(tag="v0.12.0"),
                   lambda d: d.update(url="https://evil.invalid"), lambda d: d.update(notes=[]),
                   lambda d: d.update(notes="汉"*24000), lambda d: d.update(published_at="2999-01-01T00:00:00Z"),
                   lambda d: d["assets"].pop("install.sh"),
                   lambda d: d["assets"]["install.sh"].update(url="https://evil.invalid"),
                   lambda d: d["assets"]["install.sh"].update(size=True),
                   lambda d: d["assets"]["install.sh"].update(size=BOOT.MAX_METADATA+1),
                   lambda d: d["assets"]["install.sh"].update(sha256="C"*64)]
        original = copy.deepcopy(self.metadata)
        for mutate in changes:
            self.metadata = copy.deepcopy(original)
            mutate(self.metadata)
            with self.subTest(metadata=self.metadata), self.assertRaises(BOOT.Rejected):
                self.run_install()
            self.assert_not_installed()

    def test_latest_redirect_and_identity_mismatch_rejected(self):
        original = copy.deepcopy(self.latest)
        for name, value in (("base_url", "https://github.com/evil"), ("commit", "b"*40), ("release_id", 43), ("published_at", "2026-09-28T00:00:00Z")):
            self.latest = dict(original, **{name: value})
            with self.subTest(name=name), self.assertRaises(BOOT.Rejected):
                self.run_install()
            self.assert_not_installed()

    def test_duplicate_json_and_invalid_utf8_rejected(self):
        data = self.path / "bad.json"
        for body in (b'{"tag":"v0.11.0","tag":"v0.12.0"}', b'\xff', b'{"assets":{"a":1,"a":2}}', b'{"bad":NaN}'):
            data.write_bytes(body)
            with self.subTest(body=body), self.assertRaises(BOOT.Rejected):
                BOOT.read_json(data)

    def test_download_failure_and_partial_cleanup(self):
        for failed in ("latest.json", "metadata/v0.11.0.json", "v0.11.0/checksums.txt", "v0.11.0/proxyscene_bundle_linux_amd64.tar.gz"):
            def fetch(relative, destination, limit):
                self.fetch(relative, destination, limit)
                if relative == failed:
                    raise BOOT.Rejected("truncated download")
            with self.subTest(failed=failed), self.assertRaises(BOOT.Rejected):
                self.run_install(fetch=fetch)
            self.assert_not_installed()

    def test_wrong_asset_hash_size_and_checksums_rejected(self):
        original = copy.deepcopy(self.metadata)
        for asset, field, value in (("checksums.txt", "sha256", "f"*64), ("checksums.txt", "size", len(self.checksums)+1),
                                    ("proxyscene_bundle_linux_amd64.tar.gz", "sha256", "f"*64)):
            self.metadata = copy.deepcopy(original)
            self.metadata["assets"][asset][field] = value
            with self.subTest(asset=asset, field=field), self.assertRaises(BOOT.Rejected):
                self.run_install()
            self.assert_not_installed()
        self.metadata = original
        self.checksums += self.checksums.splitlines()[0] + b"\n"
        self.metadata["assets"]["checksums.txt"] = {"size": len(self.checksums), "sha256": sha(self.checksums)}
        with self.assertRaises(BOOT.Rejected):
            self.run_install()
        self.assert_not_installed()

    def test_archive_attacks_rejected_before_installation(self):
        def member_attr(entries, **kwargs):
            for key, value in kwargs.items():
                setattr(entries[1][0], key, value)
        mutations = [lambda e: member_attr(e, name="/absolute"), lambda e: member_attr(e, name="../escape"),
                     lambda e: member_attr(e, name="proxyscene_bundle_linux_amd64/../escape"),
                     lambda e: member_attr(e, type=tarfile.SYMTYPE, linkname="/etc/shadow"),
                     lambda e: member_attr(e, type=tarfile.LNKTYPE, linkname="/etc/shadow"),
                     lambda e: member_attr(e, type=tarfile.FIFOTYPE), lambda e: member_attr(e, mode=0o4777),
                     lambda e: e.append(copy.deepcopy(e[1])), lambda e: e.pop(),
                     lambda e: member_attr(e, name="proxyscene_bundle_linux_amd64/unexpected")]
        for mutate in mutations:
            self.payload = archive(self.files, mutate)
            self.refresh_assets()
            with self.subTest(mutate=mutate), self.assertRaises(BOOT.Rejected):
                self.run_install()
            self.assert_not_installed()

    def test_bundle_manifest_corruption_and_installer_release_mismatch(self):
        self.files["bundle-manifest.sha256"] = b"f"*64 + b"  install.sh\n"
        self.payload = archive(self.files)
        self.refresh_assets()
        with self.assertRaises(BOOT.Rejected):
            self.run_install()
        self.assert_not_installed()
        self.regenerate_manifest()
        self.payload = archive(self.files)
        self.refresh_assets()
        self.metadata["assets"]["install.sh"]["sha256"] = "f"*64
        self.checksums = "".join(a["sha256"] + "  " + n + "\n" for n, a in sorted(self.metadata["assets"].items()) if n != "checksums.txt").encode()
        self.metadata["assets"]["checksums.txt"] = {"size": len(self.checksums), "sha256": sha(self.checksums)}
        with self.assertRaises(BOOT.Rejected):
            self.run_install()
        self.assert_not_installed()

    def test_curl_is_fixed_https_without_redirects_config_or_environment(self):
        output = self.path / "download"
        def curl(command, **kwargs):
            self.assertEqual(command[0:2], ["curl", "-q"])
            self.assertNotIn("-L", command)
            self.assertNotIn("--location", command)
            self.assertEqual(command[command.index("--proto")+1], "=https")
            self.assertEqual(command[command.index("--max-redirs")+1], "0")
            self.assertEqual(command[-1], BOOT.BASE + "/metadata/v0.11.0.json")
            self.assertEqual(kwargs["env"], BOOT.ENV)
            output.write_bytes(b"ok")
            return types.SimpleNamespace(returncode=0, stdout="200")
        with patch.object(BOOT.subprocess, "run", side_effect=curl):
            BOOT.download("metadata/v0.11.0.json", output, 100)
        for relative in ("https://github.com/a", "//evil.invalid/a", "../latest.json", "metadata/v0.11.0.json?x=1"):
            with self.subTest(relative=relative), patch.object(BOOT.subprocess, "run") as run, self.assertRaises(BOOT.Rejected):
                BOOT.download(relative, output, 100)
            run.assert_not_called()

    def test_redirect_partial_and_oversize_downloads_rejected(self):
        output = self.path / "download"
        for status, code, data in (("302", 0, b""), ("200", 18, b"partial"), ("200", 0, b"large")):
            output.write_bytes(data)
            with self.subTest(status=status, code=code), patch.object(BOOT.subprocess, "run", return_value=types.SimpleNamespace(returncode=code, stdout=status)), self.assertRaises(BOOT.Rejected):
                BOOT.download("latest.json", output, 3)

    def test_installer_failure_cleans_downloads(self):
        self.files["install.sh"] = b"#!/bin/bash\nexit 42\n"
        self.regenerate_manifest()
        self.payload = archive(self.files)
        self.refresh_assets()
        with self.assertRaisesRegex(BOOT.Rejected, "文件可能已经提交"):
            self.run_install()
        self.assert_not_installed()

    def test_supported_architecture_detection(self):
        for machine, arch in (("x86_64", "amd64"), ("aarch64", "arm64"), ("i686", "386"), ("armv7l", "armv7")):
            self.assertEqual(BOOT.select_arch(machine), arch)
        with self.assertRaises(BOOT.Rejected):
            BOOT.select_arch("riscv64")

    def test_installed_version_downgrade_same_version_identity_and_cas(self):
        manager = self.path / "proxyscene"
        manager.write_text("#!/bin/sh\nprintf 'proxyscene 0.12.0 (aaaaaaaaaaaa)\\n'\n")
        manager.chmod(0o700)
        original_fstat = os.fstat
        def root_fstat(fd):
            info = list(original_fstat(fd))
            info[4] = 0
            return os.stat_result(info)
        with patch.object(BOOT, "trusted_chain"), patch.object(BOOT.os, "fstat", side_effect=root_fstat):
            with self.assertRaisesRegex(BOOT.Rejected, "拒绝降级"):
                BOOT.installed_state(manager, "v0.11.0", "a"*40)
            with self.assertRaisesRegex(BOOT.Rejected, "commit 不一致"):
                BOOT.installed_state(manager, "v0.12.0", "b"*40)
            self.assertEqual(BOOT.installed_state(manager, "v0.13.0", "b"*40), ["--expected-manager-sha256", sha(manager.read_bytes())])
            self.assertEqual(BOOT.installed_state(self.path / "absent", "v0.11.0", "a"*40), ["--expected-manager-absent"])

    def test_untrusted_installed_file_is_never_executed(self):
        manager = self.path / "proxyscene"
        manager.write_text("#!/bin/sh\nexit 99\n")
        manager.chmod(0o777)
        with patch.object(BOOT, "trusted_chain"), patch.object(BOOT.subprocess, "run") as run, self.assertRaises(BOOT.Rejected):
            BOOT.installed_state(manager, "v0.11.0", "a"*40)
        run.assert_not_called()
        manager.unlink()
        manager.symlink_to("missing")
        with patch.object(BOOT, "trusted_chain"), patch.object(BOOT.subprocess, "run") as run, self.assertRaises(OSError):
            BOOT.installed_state(manager, "v0.11.0", "a"*40)
        run.assert_not_called()
        with self.assertRaises(BOOT.Rejected):
            BOOT.trusted_chain(self.path)

    def test_custom_or_untrusted_installation_ownership_is_rejected(self):
        ownership = self.path / "ownership.json"
        expected = {"version": 1, "core_dir": "/opt/proxyscene", "install_bin": "/usr/local/bin/proxyscene",
                    "systemd_service": "proxyscene.service", "restore_service": "proxyscene-restore.service"}
        original_fstat = os.fstat
        def root_fstat(fd):
            info = list(original_fstat(fd))
            info[4] = 0
            return os.stat_result(info)
        with patch.object(BOOT, "trusted_chain"), patch.object(BOOT.os, "fstat", side_effect=root_fstat):
            BOOT.verify_default_installation(ownership)
            ownership.write_text(json.dumps(expected))
            ownership.chmod(0o600)
            BOOT.verify_default_installation(ownership)
            for key, value in (("core_dir", "/opt/custom-proxyscene"), ("version", True), ("install_bin", "/opt/bin/proxyscene")):
                ownership.write_text(json.dumps(dict(expected, **{key: value})))
                with self.subTest(key=key), self.assertRaises(BOOT.Rejected):
                    BOOT.verify_default_installation(ownership)
            ownership.write_text(json.dumps(expected))
            ownership.chmod(0o666)
            with self.assertRaises(BOOT.Rejected):
                BOOT.verify_default_installation(ownership)
            ownership.unlink()
            ownership.symlink_to("missing")
            with self.assertRaises(OSError):
                BOOT.verify_default_installation(ownership)

    def test_signal_is_forwarded_until_installer_exits(self):
        from unittest.mock import Mock
        process = Mock()
        with patch.object(BOOT, "ACTIVE_INSTALLER", process), patch.object(BOOT, "INTERRUPTED", False), patch.object(BOOT, "signal_group") as forward:
            BOOT.interrupted(15, None)
            forward.assert_called_once_with(process.pid, 15)
            self.assertTrue(BOOT.INTERRUPTED)
        with patch.object(BOOT, "ACTIVE_INSTALLER", None), patch.object(BOOT, "INTERRUPTED", False), self.assertRaises(BOOT.Rejected):
            BOOT.interrupted(15, None)

    def test_environment_keeps_verified_sudo_identity_and_drops_overrides(self):
        user = BOOT.pwd.getpwuid(os.getuid())
        with patch.dict(os.environ, {"SUDO_USER": user.pw_name, "SUDO_UID": str(user.pw_uid), "SUDO_GID": str(user.pw_gid), "BASH_ENV": "/evil", "SKIP_MANAGER_INIT": "1"}, clear=True):
            env = BOOT.installer_env()
            self.assertEqual(env["SUDO_USER"], user.pw_name)
            self.assertEqual(env["SUDO_UID"], str(user.pw_uid))
            self.assertNotIn("BASH_ENV", env)
            self.assertNotIn("SKIP_MANAGER_INIT", env)
            os.environ["SUDO_UID"] = "-1"
            with self.assertRaises(BOOT.Rejected):
                BOOT.installer_env()


if __name__ == "__main__":
    unittest.main()
