#!/usr/bin/python3
"""Receiver policy tests; all release transport is replaced by local fixtures."""

import copy
import hashlib
import importlib.util
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location("mirror_receiver", Path(__file__).with_name("mirror-receiver.py"))
receiver = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(receiver)
release = receiver.release


def fixture(tag="v0.9.0"):
    blobs = {name: (name + " " + tag + "\n").encode() for name in release.FIXED_ASSETS if name != "checksums.txt"}
    blobs["xray_source_v26.9.9.tar.gz"] = b"source archive fixture\n"
    blobs["checksums.txt"] = b"".join(
        f"{hashlib.sha256(blob).hexdigest()}  {name}\n".encode()
        for name, blob in sorted(blobs.items())
    )
    info = {"tag": tag, "version": tag[1:], "commit": "a" * 40, "release_id": 123,
            "published_at": "2026-01-01T00:00:00Z", "notes": "Fixture release notes\n", "assets": {
                name: {"sha256": hashlib.sha256(blob).hexdigest(), "size": len(blob),
                       "url": f"{release.RELEASE_BASE}/{tag}/{name}"}
                for name, blob in blobs.items()}}
    return info, blobs


class CommandTests(unittest.TestCase):
    def test_exact_command_and_numeric_versions(self):
        for verb in ("sync", "promote"):
            self.assertEqual(receiver.parse_request(f"proxyscene-mirror {verb} v0.9.0"), (verb, "v0.9.0"))
        self.assertGreater(release.version_key("v0.10.0"), release.version_key("v0.9.0"))

    def test_rejects_other_commands_options_paths_and_ambiguous_whitespace(self):
        for command in ("", "sh", "proxyscene-mirror sync latest", "proxyscene-mirror sync v00.9.0",
                        "proxyscene-mirror sync v0.9.0-rc.1", "proxyscene-mirror sync ../v0.9.0",
                        "proxyscene-mirror sync v0.9.0;id", "proxyscene-mirror sync $(id)",
                        "proxyscene-mirror sync v0.9.0\n", "proxyscene-mirror\tsync v0.9.0",
                        "proxyscene-mirror  sync v0.9.0", "proxyscene-mirror sync 'v0.9.0'",
                        "proxyscene-mirror sync v0.9.0 extra", "proxyscene-mirror delete v0.9.0",
                        "proxyscene-mirror sync v" + "9" * 128 + ".0.0"):
            with self.subTest(command=command), self.assertRaises(release.MirrorError):
                receiver.parse_request(command)

    def test_root_and_setuid_execution_are_rejected(self):
        for uid, euid in ((0, 0), (1000, 0)):
            with mock.patch.object(receiver.os, "getuid", return_value=uid), \
                 mock.patch.object(receiver.os, "geteuid", return_value=euid), \
                 self.assertRaisesRegex(receiver.ReceiverError, "non-root"):
                receiver.require_runtime()

    def test_isolated_entrypoint_ignores_pythonpath_module(self):
        with tempfile.TemporaryDirectory(prefix="receiver-import-") as temporary:
            root = Path(temporary)
            marker = root / "executed"
            (root / "mirror_release.py").write_text(f"from pathlib import Path\nPath({str(marker)!r}).touch()\n")
            environment = dict(os.environ, PYTHONPATH=str(root), SSH_ORIGINAL_COMMAND="invalid")
            result = subprocess.run([sys.executable, "-B", "-I", str(Path(receiver.__file__).absolute())],
                                    env=environment, capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 1)
            self.assertFalse(marker.exists())
            self.assertIn("only proxyscene-mirror", result.stderr)


    def test_adjacent_module_is_checked_before_execution(self):
        with tempfile.TemporaryDirectory(prefix="receiver-code-trust-") as temporary:
            root = Path(temporary)
            script = root / "mirror-receiver.py"
            script.write_bytes(Path(receiver.__file__).read_bytes())
            module = root / "mirror_release.py"
            target = root / "target.py"
            marker = root / "executed"
            target.write_text(f"from pathlib import Path\nPath({str(marker)!r}).touch()\n")
            for kind in ("symlink", "hardlink", "writable", "fifo"):
                with self.subTest(kind=kind):
                    if kind == "symlink":
                        module.symlink_to(target)
                    elif kind == "hardlink":
                        os.link(target, module)
                    elif kind == "fifo":
                        os.mkfifo(module)
                    else:
                        module.write_bytes(target.read_bytes())
                        module.chmod(0o666)
                    result = subprocess.run([sys.executable, "-B", "-I", str(script)],
                                            capture_output=True, text=True, timeout=5)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("unsafe metadata", result.stderr)
                    self.assertFalse(marker.exists())
                    module.unlink()


class ReceiverTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="proxyscene-receiver-test-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.project = self.root / "public"
        self.incoming = self.root / "private"
        self.project.mkdir(mode=0o755)
        self.project.chmod(0o755)
        self.incoming.mkdir(mode=0o700)
        self.incoming.chmod(0o700)
        self.owner = os.getuid()
        self.info, self.blobs = fixture()
        self.records = {self.info["tag"]: self.info}
        self.payloads = {self.info["tag"]: self.blobs}
        self.latest = self.info["tag"]
        self.downloads = []
        self.start_patch(receiver, "PROJECT_ROOT", self.project)
        self.start_patch(receiver, "INCOMING_ROOT", self.incoming)
        self.start_patch(receiver, "require_runtime", mock.Mock(return_value=self.owner))
        self.start_patch(release, "release_info", mock.Mock(side_effect=self.fetch_info))
        self.start_patch(release, "download_release", mock.Mock(side_effect=self.download))

    def start_patch(self, target, name, replacement):
        patch = mock.patch.object(target, name, replacement)
        patch.start()
        self.addCleanup(patch.stop)

    def fetch_info(self, tag, *args, **kwargs):
        # The receiver must never consult/forward CI or environment credentials.
        self.assertEqual(args, ())
        self.assertEqual(kwargs, {})
        self.assertEqual(dict(os.environ), {"PATH": "/usr/bin:/bin", "LC_ALL": "C"})
        return copy.deepcopy(self.records[self.latest if tag == "latest" else tag])

    def download(self, info, directory):
        self.assertFalse(directory.exists())
        self.downloads.append(info["tag"])
        directory.mkdir(mode=0o755)
        for name, blob in self.payloads[info["tag"]].items():
            (directory / name).write_bytes(blob)
        release.verify_directory(info, directory)

    def call(self, verb="sync", tag="v0.9.0"):
        with mock.patch.dict(os.environ, {"SSH_ORIGINAL_COMMAND": f"proxyscene-mirror {verb} {tag}",
                                          "GH_TOKEN": "not-a-credential", "GITHUB_TOKEN": "not-a-credential"}):
            return receiver.receive()

    def add_version(self, tag):
        info, blobs = fixture(tag)
        self.records[tag], self.payloads[tag] = info, blobs
        return info

    def assert_clean(self):
        self.assertEqual(sorted(p.name for p in self.incoming.iterdir()), [".deploy.lock"])

    def test_sync_publishes_complete_exact_files_and_idempotent_retry(self):
        result = self.call()
        self.assertEqual(result, {"operation": "sync", "tag": "v0.9.0", "release_id": 123})
        directory = self.project / "v0.9.0"
        self.assertEqual(directory.stat().st_mode & 0o7777, 0o755)
        for name, blob in self.blobs.items():
            self.assertEqual((directory / name).read_bytes(), blob)
            self.assertEqual((directory / name).stat().st_mode & 0o7777, 0o644)
        self.assertFalse((self.project / "latest.json").exists())
        before = {p.name: p.stat().st_ino for p in directory.iterdir()}
        self.call()
        self.assertEqual(self.downloads, ["v0.9.0"])
        self.assertEqual(before, {p.name: p.stat().st_ino for p in directory.iterdir()})
        self.assert_clean()

    def test_existing_bytes_are_never_overwritten_or_partially_repaired(self):
        self.call()
        binary = self.project / "v0.9.0/install.sh"
        binary.write_bytes(b"changed")
        with self.assertRaises(release.MirrorError):
            self.call()
        self.assertEqual(binary.read_bytes(), b"changed")
        binary.unlink()
        with self.assertRaisesRegex(receiver.ReceiverError, "incomplete"):
            self.call()
        self.assertFalse(binary.exists())
        self.assertEqual(len(self.downloads), 1)

    def test_metadata_publishes_once_outside_exact_asset_directory(self):
        self.call()
        sidecar = self.project / "metadata/v0.9.0.json"
        self.assertEqual(sidecar.read_bytes(), release.metadata_bytes(self.info))
        self.assertEqual(sidecar.stat().st_mode & 0o7777, 0o644)
        self.assertEqual(sidecar.parent.stat().st_mode & 0o7777, 0o755)
        self.assertEqual(len(list((self.project / "v0.9.0").iterdir())), 11)
        before = sidecar.stat().st_ino
        self.call()
        self.assertEqual(sidecar.stat().st_ino, before)
        self.assert_clean()

    def test_existing_legacy_release_can_fill_missing_metadata_without_rewriting_assets(self):
        self.call()
        sidecar = self.project / "metadata/v0.9.0.json"
        sidecar.unlink()
        sidecar.parent.rmdir()
        before = {p.name: p.stat().st_ino for p in (self.project / "v0.9.0").iterdir()}
        self.call()
        self.assertEqual(sidecar.read_bytes(), release.metadata_bytes(self.info))
        self.assertEqual(before, {p.name: p.stat().st_ino for p in (self.project / "v0.9.0").iterdir()})
        self.assertEqual(self.downloads, ["v0.9.0"])
        self.assert_clean()

    def test_missing_or_modified_metadata_cannot_promote_or_overwrite(self):
        self.call()
        sidecar = self.project / "metadata/v0.9.0.json"
        original = sidecar.read_bytes()
        sidecar.unlink()
        with self.assertRaises(FileNotFoundError):
            self.call("promote")
        self.assertFalse((self.project / "latest.json").exists())
        sidecar.write_bytes(original.replace(b"Fixture", b"Changed"))
        sidecar.chmod(0o644)
        changed = sidecar.read_bytes()
        for verb in ("sync", "promote"):
            with self.subTest(verb=verb), self.assertRaises(release.MirrorError):
                self.call(verb)
            self.assertEqual(sidecar.read_bytes(), changed)
            self.assertFalse((self.project / "latest.json").exists())
        self.assert_clean()

    def test_metadata_rejects_symlink_hardlink_fifo_and_unexpected_paths(self):
        self.call()
        sidecar = self.project / "metadata/v0.9.0.json"
        external = self.root / "metadata-original"
        sidecar.rename(external)
        for kind in ("symlink", "hardlink", "fifo", "mode"):
            with self.subTest(kind=kind):
                if kind == "symlink": sidecar.symlink_to(external)
                elif kind == "hardlink": os.link(external, sidecar)
                elif kind == "fifo": os.mkfifo(sidecar, 0o644)
                else:
                    sidecar.write_bytes(external.read_bytes())
                    sidecar.chmod(0o666)
                with self.assertRaises((receiver.ReceiverError, OSError)):
                    self.call()
                sidecar.unlink()
        external.rename(sidecar)
        for name in ("v01.2.3.json", "v99.0.0.json", "extra", "install.sh"):
            path = sidecar.parent / name
            path.write_bytes(b"{}")
            path.chmod(0o644)
            with self.subTest(name=name), self.assertRaises((release.MirrorError, OSError)):
                self.call()
            path.unlink()
        self.call()

    def test_metadata_cannot_publish_after_upstream_changes(self):
        original = receiver.publish_metadata
        def changed(info, owner):
            self.info["release_id"] += 1
            return original(info, owner)
        with mock.patch.object(receiver, "publish_metadata", side_effect=changed), self.assertRaisesRegex(receiver.ReceiverError, "identity changed before metadata"):
            self.call()
        self.assertTrue((self.project / "v0.9.0").is_dir())
        self.assertFalse((self.project / "metadata/v0.9.0.json").exists())
        self.assertFalse((self.project / "latest.json").exists())
        self.assert_clean()

    def test_metadata_commit_before_fsync_retry_preserves_inode(self):
        original = receiver.fsync_directory
        sidecar = self.project / "metadata/v0.9.0.json"
        def interrupted(path):
            if path == sidecar.parent and sidecar.exists():
                raise OSError("metadata power loss")
            original(path)
        with mock.patch.object(receiver, "fsync_directory", side_effect=interrupted), self.assertRaisesRegex(OSError, "metadata power loss"):
            self.call()
        before = sidecar.stat().st_ino
        self.call()
        self.assertEqual(sidecar.stat().st_ino, before)
        self.assertEqual(self.downloads, ["v0.9.0"])
        self.assert_clean()

    def test_download_failure_leaves_no_public_tree_or_staging(self):
        def fail_download(info, directory):
            directory.mkdir(mode=0o700)
            (directory / "install.sh").write_bytes(b"partial")
            raise release.MirrorError("download failed")
        release.download_release.side_effect = fail_download
        with self.assertRaisesRegex(release.MirrorError, "download failed"):
            self.call()
        self.assertEqual(list(self.project.iterdir()), [])
        self.assert_clean()

    def test_corrupt_download_cannot_publish(self):
        def corrupt(info, directory):
            self.download(info, directory)
            (directory / "install.sh").write_bytes(b"corrupt")
        release.download_release.side_effect = corrupt
        with self.assertRaises(release.MirrorError):
            self.call()
        self.assertEqual(list(self.project.iterdir()), [])
        self.assert_clean()

    def test_changed_upstream_identity_cannot_publish(self):
        first = copy.deepcopy(self.info)
        changed = copy.deepcopy(self.info)
        changed["release_id"] += 1
        release.release_info.side_effect = [first, changed]
        with self.assertRaisesRegex(receiver.ReceiverError, "identity changed"):
            self.call()
        self.assertEqual(list(self.project.iterdir()), [])
        self.assert_clean()

    def test_no_replace_refuses_existing_destination(self):
        first, second = self.root / "first", self.root / "second"
        first.mkdir()
        second.mkdir()
        (second / "existing").write_bytes(b"keep")
        with self.assertRaisesRegex(receiver.ReceiverError, "concurrently"):
            receiver.rename_noreplace(first, second)
        self.assertEqual((second / "existing").read_bytes(), b"keep")
        self.assertTrue(first.is_dir())

    def test_missing_no_replace_primitive_fails_closed(self):
        with mock.patch.object(receiver, "RENAMEAT2", None), \
             self.assertRaisesRegex(receiver.ReceiverError, "required"):
            self.call()
        self.assertEqual(list(self.project.iterdir()), [])
        self.assert_clean()

    def test_retry_after_directory_commit_before_fsync(self):
        original = receiver.fsync_directory
        failed = False
        def interrupted(path):
            nonlocal failed
            if path == self.project and (self.project / "v0.9.0").exists() and not failed:
                failed = True
                raise OSError("simulated power loss after commit")
            original(path)
        with mock.patch.object(receiver, "fsync_directory", side_effect=interrupted), \
             self.assertRaisesRegex(OSError, "power loss"):
            self.call()
        release.verify_directory(self.info, self.project / "v0.9.0")
        self.call()
        self.assertEqual(self.downloads, ["v0.9.0"])
        self.assert_clean()

    def test_exact_creation_modes_ignore_hostile_umask(self):
        with receiver.exact_umask(0o777):
            self.call()
            self.call("promote")
        self.assertEqual((self.incoming / ".deploy.lock").stat().st_mode & 0o7777, 0o600)
        self.assertEqual((self.project / "latest.json").stat().st_mode & 0o7777, 0o644)
        self.assert_clean()

    def test_only_one_receiver_can_hold_lock_and_inode_is_preserved(self):
        with receiver.deployment_lock(self.owner):
            inode = (self.incoming / ".deploy.lock").stat().st_ino
            with self.assertRaisesRegex(receiver.ReceiverError, "active"):
                self.call()
        self.call()
        self.assertEqual((self.incoming / ".deploy.lock").stat().st_ino, inode)

    def test_lock_rejects_symlink_hardlink_and_wrong_mode(self):
        lock = self.incoming / ".deploy.lock"
        external = self.root / "external"
        external.touch(mode=0o600)
        for kind in ("symlink", "hardlink", "mode"):
            with self.subTest(kind=kind):
                if kind == "symlink":
                    lock.symlink_to(external)
                elif kind == "hardlink":
                    os.link(external, lock)
                else:
                    lock.touch(mode=0o644)
                with self.assertRaises((receiver.ReceiverError, OSError)):
                    self.call()
                lock.unlink()
        self.assertEqual(external.read_bytes(), b"")

    def test_public_assets_reject_links_fifo_permissions_and_wrong_owner(self):
        self.call()
        asset = self.project / "v0.9.0/install.sh"
        external = self.root / "external"
        external.write_bytes(asset.read_bytes())
        external.chmod(0o644)
        for kind in ("symlink", "hardlink", "fifo", "mode"):
            with self.subTest(kind=kind):
                asset.unlink()
                if kind == "symlink":
                    asset.symlink_to(external)
                elif kind == "hardlink":
                    os.link(external, asset)
                elif kind == "fifo":
                    os.mkfifo(asset, 0o644)
                else:
                    asset.write_bytes(external.read_bytes())
                    asset.chmod(0o666)
                with self.assertRaises(receiver.ReceiverError):
                    self.call()
        asset.unlink()
        asset.write_bytes(external.read_bytes())
        asset.chmod(0o644)
        with self.assertRaises(receiver.ReceiverError):
            receiver.require_file(asset, self.owner + 1, (0o644,), 1000)

    def test_private_root_symlink_and_public_directory_mode_rejected(self):
        self.incoming.rmdir()
        other = self.root / "other"
        other.mkdir(mode=0o700)
        self.incoming.symlink_to(other)
        with self.assertRaises(receiver.ReceiverError):
            self.call()
        self.incoming.unlink()
        self.incoming.mkdir(mode=0o700)
        self.project.chmod(0o775)
        with self.assertRaises(receiver.ReceiverError):
            self.call()

    def test_storage_limits_are_checked_before_download(self):
        for constant, value in (("MAX_PROJECT_BYTES", 1), ("MAX_PROJECT_VERSIONS", 0),
                                ("MAX_INCOMING_BYTES", 1)):
            with self.subTest(constant=constant), mock.patch.object(receiver, constant, value), \
                 self.assertRaises(receiver.ReceiverError):
                self.call()
        release.download_release.assert_not_called()
        self.assertEqual(list(self.project.iterdir()), [])

    def test_projected_growth_cannot_exceed_project_limit(self):
        self.call()
        self.add_version("v0.10.0")
        current = receiver.release_usage(self.project / "v0.9.0", self.owner)
        with mock.patch.object(receiver, "MAX_PROJECT_BYTES", current + 1), \
             self.assertRaises(receiver.ReceiverError):
            self.call(tag="v0.10.0")
        self.assertFalse((self.project / "v0.10.0").exists())

    def test_space_reserve_blocks_publication(self):
        space = os.statvfs(self.project)
        values = list(space)
        values[4] = 0  # f_bavail
        with mock.patch.object(receiver.os, "statvfs", return_value=os.statvfs_result(values)), \
             self.assertRaisesRegex(receiver.ReceiverError, "reserve"):
            self.call()
        release.download_release.assert_not_called()

    def test_staged_bytes_count_toward_project_limit_without_double_reserving_disk(self):
        space = os.statvfs(self.project)
        reserve = max(receiver.MIN_AVAILABLE_BYTES, (space.f_blocks * space.f_frsize + 19) // 20)
        values = list(space)
        values[4] = (reserve + space.f_frsize - 1) // space.f_frsize
        with mock.patch.object(receiver.os, "statvfs", return_value=os.statvfs_result(values)):
            with self.assertRaisesRegex(receiver.ReceiverError, "reserve"):
                receiver.require_project_budget(self.owner, additional=1024 * 1024)
            receiver.require_project_budget(self.owner, additional=1024 * 1024, already_allocated=True)
        with mock.patch.object(receiver, "MAX_PROJECT_BYTES", 1), self.assertRaises(receiver.ReceiverError):
            receiver.require_project_budget(self.owner, additional=2, already_allocated=True)

    def test_residual_staging_is_counted_and_never_deleted_by_another_run(self):
        residual = self.incoming / ("stage-" + "0" * 32)
        residual.mkdir(mode=0o700)
        (residual / "leftover").write_bytes(b"leftover")
        with mock.patch.object(receiver, "MAX_INCOMING_BYTES", 5000), \
             self.assertRaisesRegex(receiver.ReceiverError, "byte limit"):
            self.call()
        self.assertEqual((residual / "leftover").read_bytes(), b"leftover")
        release.download_release.assert_not_called()

    def test_cleanup_failure_is_reported_and_preserves_download_error(self):
        def broken(info, directory):
            raise release.MirrorError("original download failure")
        release.download_release.side_effect = broken
        with mock.patch.object(receiver.shutil, "rmtree", side_effect=OSError("cleanup failure")), \
             self.assertRaisesRegex(release.MirrorError, "original download failure") as caught:
            self.call()
        self.assertIn("cleanup failure", str(caught.exception.__cause__))
        self.assertEqual(list(self.project.iterdir()), [])

    def test_interrupt_unwinds_transfer_before_unlock_and_leaves_no_public_tree(self):
        def interrupted(info, directory):
            directory.mkdir(mode=0o700)
            (directory / "install.sh").write_bytes(b"partial")
            signal.raise_signal(signal.SIGTERM)
        release.download_release.side_effect = interrupted
        with receiver.controlled_operation(), self.assertRaises(receiver.ReceiverInterrupted):
            self.call()
        self.assertEqual(list(self.project.iterdir()), [])
        self.assert_clean()
        with receiver.deployment_lock(self.owner):
            pass

    def test_deadline_cannot_be_swallowed_by_ordinary_download_exception_handler(self):
        def interrupted(info, directory):
            try:
                signal.raise_signal(signal.SIGALRM)
            except Exception:
                self.fail("deadline was caught as an ordinary download failure")
        release.download_release.side_effect = interrupted
        with receiver.controlled_operation(), self.assertRaises(receiver.ReceiverInterrupted):
            self.call()
        self.assert_clean()

    def test_promote_requires_complete_matching_release_and_github_latest(self):
        with self.assertRaises(FileNotFoundError):
            self.call("promote")
        self.call()
        self.add_version("v0.10.0")
        self.latest = "v0.10.0"
        with self.assertRaisesRegex(receiver.ReceiverError, "exact GitHub Latest"):
            self.call("promote")
        self.assertFalse((self.project / "latest.json").exists())

    def test_promote_writes_single_canonical_pointer_and_idempotent_retry(self):
        self.call()
        self.call("promote")
        pointer = self.project / "latest.json"
        self.assertEqual(pointer.read_bytes(), release.manifest_bytes(self.info))
        inode = pointer.stat().st_ino
        self.call("promote")
        self.assertEqual(pointer.stat().st_ino, inode)
        self.assertFalse((self.project / "install.sh").exists())
        self.assert_clean()

    def test_promote_refuses_local_downgrade_even_if_github_latest_regresses(self):
        self.call()
        new = self.add_version("v0.10.0")
        self.call(tag=new["tag"])
        self.latest = new["tag"]
        self.call("promote", new["tag"])
        before = (self.project / "latest.json").read_bytes()
        self.latest = "v0.9.0"
        with self.assertRaisesRegex(receiver.ReceiverError, "roll back"):
            self.call("promote")
        self.assertEqual((self.project / "latest.json").read_bytes(), before)

    def test_same_version_metadata_change_cannot_mutate_latest(self):
        self.call()
        self.call("promote")
        before = (self.project / "latest.json").read_bytes()
        self.info["release_id"] += 1
        with self.assertRaisesRegex(receiver.ReceiverError, "mutate metadata"):
            self.call("promote")
        self.assertEqual((self.project / "latest.json").read_bytes(), before)

    def test_latest_change_immediately_before_commit_preserves_old_pointer(self):
        self.call()
        self.call("promote")
        new = self.add_version("v0.10.0")
        self.call(tag=new["tag"])
        old_pointer = (self.project / "latest.json").read_bytes()
        release.release_info.side_effect = [copy.deepcopy(new), copy.deepcopy(new), copy.deepcopy(self.info)]
        with self.assertRaisesRegex(receiver.ReceiverError, "Latest changed"):
            self.call("promote", new["tag"])
        self.assertEqual((self.project / "latest.json").read_bytes(), old_pointer)
        self.assert_clean()

    def test_malformed_current_latest_is_not_treated_as_first_publication(self):
        self.call()
        pointer = self.project / "latest.json"
        valid = release.manifest_bytes(self.info)
        bad_values = [b"", b"[]\n", b"{}\n", valid + b"\n",
                      valid.replace(b'"release_id":123', b'"release_id":true'),
                      valid.replace(b'"tag":"v0.9.0"', b'"tag":"v0.9.0","tag":"v0.9.0"'),
                      valid.replace(b"https://dl.ll.cd", b"https://evil.example"),
                      valid.replace(b"2026-01-01", b"2026-02-30")]
        for data in bad_values:
            with self.subTest(data=data):
                pointer.write_bytes(data)
                pointer.chmod(0o644)
                with self.assertRaises(release.MirrorError):
                    self.call("promote")
                self.assertEqual(pointer.read_bytes(), data)

    def test_promotion_commit_crash_is_idempotently_recoverable(self):
        self.call()
        original = receiver.fsync_directory
        failed = False
        def interrupted(path):
            nonlocal failed
            if path == self.project and (self.project / "latest.json").exists() and not failed:
                failed = True
                raise OSError("simulated latest fsync failure")
            original(path)
        with mock.patch.object(receiver, "fsync_directory", side_effect=interrupted), \
             self.assertRaisesRegex(OSError, "latest fsync"):
            self.call("promote")
        self.call("promote")
        self.assertEqual((self.project / "latest.json").read_bytes(), release.manifest_bytes(self.info))
        self.assert_clean()


if __name__ == "__main__":
    unittest.main()
