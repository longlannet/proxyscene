#!/usr/bin/env python3
"""Offline adversarial tests for GitHub-bound mirror preparation/verification."""
import copy
import hashlib
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

import mirror_release as mirror


def sha(data):
    return hashlib.sha256(data).hexdigest()


class Fixture(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="proxyscene-mirror-test-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.names = sorted(mirror.FIXED_ASSETS | {"xray_source_v26.9.9.tar.gz"})
        self.files = {name: ("unexecuted, unextracted fixture: " + name + "\n").encode() for name in self.names if name != "checksums.txt"}
        self.files["checksums.txt"] = self.checksums()
        self.info = {
            "tag": "v0.9.0", "version": "0.9.0", "commit": "a" * 40,
            "release_id": 1234, "published_at": "2020-01-02T03:04:05Z", "notes": "修复与更新\n",
            "assets": {name: {"sha256": sha(data), "size": len(data),
                              "url": mirror.RELEASE_BASE + "/v0.9.0/" + name}
                       for name, data in self.files.items()},
        }
        self.release = {
            "id": self.info["release_id"], "tag_name": self.info["tag"], "immutable": True,
            "draft": False, "prerelease": False, "published_at": self.info["published_at"], "body": self.info["notes"],
            "assets": [{"name": name, "size": item["size"], "digest": "sha256:" + item["sha256"],
                        "browser_download_url": item["url"], "id": index, "state": "uploaded",
                        "url": mirror.API_BASE + "/releases/assets/" + str(index)}
                       for index, (name, item) in enumerate(self.info["assets"].items(), 1)],
        }
        self.reference = {"ref": "refs/tags/v0.9.0", "object": {"type": "commit", "sha": "a" * 40}}
        self.requests = []

    def checksums(self):
        return "".join(sha(data) + "  " + name + "\n" for name, data in sorted(self.files.items()) if name != "checksums.txt").encode()

    def change_checksums(self, data):
        self.files["checksums.txt"] = data
        self.info["assets"]["checksums.txt"].update(sha256=sha(data), size=len(data))

    def write_assets(self, name="prepared"):
        directory = self.root / name
        directory.mkdir()
        for asset, data in self.files.items():
            (directory / asset).write_bytes(data)
            (directory / asset).chmod(0o644)
        return directory

    def fake_download(self, url, destination, max_bytes, **kwargs):
        self.requests.append((url, max_bytes, kwargs))
        if url == mirror.MIRROR_BASE + "/latest.json":
            data = mirror.manifest_bytes(self.info)
        elif url == mirror.MIRROR_BASE + "/metadata/" + self.info["tag"] + ".json":
            data = mirror.metadata_bytes(self.info)
        else:
            data = self.files[url.rsplit("/", 1)[1]]
        self.assertLessEqual(len(data), max_bytes)
        Path(destination).write_bytes(data)
        Path(destination).chmod(0o644)

    def read_info(self, requested="v0.9.0"):
        with mock.patch.object(mirror, "_api_json", side_effect=[self.release, self.reference]) as fetch:
            info = mirror.release_info(requested, "github_pat_fixture")
        self.assertEqual(fetch.call_args_list[-1], mock.call("/git/ref/tags/v0.9.0", "github_pat_fixture"))
        return info


class IdentityTests(Fixture):
    def test_canonical_tags_and_numeric_order(self):
        self.assertEqual(mirror.version_key("v1.10.0"), (1, 10, 0))
        self.assertGreater(mirror.version_key("v1.10.0"), mirror.version_key("v1.9.0"))
        for bad in (None, "latest", "v01.2.3", "v1.02.3", "v1.2.03", "v1.2", "v1.2.3-rc1", "v1.2.3\n", "v1/2/3", "v" + "1" * 125 + ".0.0"):
            with self.subTest(tag=bad), self.assertRaises(mirror.MirrorError):
                mirror.validate_tag(bad)

    def test_release_and_latest_have_same_fixed_identity(self):
        self.assertEqual(self.read_info(), self.info)
        self.assertEqual(self.read_info("latest"), self.info)

    def test_rejects_untrusted_release_state(self):
        for field, value in (("immutable", False), ("immutable", "true"), ("draft", True), ("prerelease", True),
                             ("tag_name", "v0.8.0"), ("id", True), ("id", 0), ("id", 2**63),
                             ("published_at", "2099-01-01T00:00:00Z"), ("published_at", "2020-02-31T00:00:00Z")):
            with self.subTest(field=field, value=value):
                original = self.release[field]
                self.release[field] = value
                with self.assertRaises(mirror.MirrorError):
                    self.read_info()
                self.release[field] = original

    def test_tag_must_bind_direct_commit(self):
        for reference in ({"ref": "refs/tags/v0.8.0", "object": self.reference["object"]},
                          {"ref": "refs/tags/v0.9.0", "object": {"type": "tag", "sha": "a" * 40}},
                          {"ref": "refs/tags/v0.9.0", "object": {"type": "commit", "sha": "bad"}}):
            with self.subTest(reference=reference), mock.patch.object(mirror, "_api_json", side_effect=[self.release, reference]), self.assertRaises(mirror.MirrorError):
                mirror.release_info("v0.9.0")

    def test_asset_set_cannot_be_changed(self):
        original = copy.deepcopy(self.release["assets"])
        for kind in ("missing", "extra", "duplicate", "source-path", "second-source", "long-source"):
            self.release["assets"] = copy.deepcopy(original)
            if kind == "missing": self.release["assets"].pop()
            if kind == "extra": self.release["assets"].append(copy.deepcopy(original[0]))
            if kind == "duplicate": self.release["assets"][-1] = copy.deepcopy(original[0])
            if kind == "source-path": self.release["assets"][0]["name"] = "../install.sh"
            if kind == "second-source": self.release["assets"][0]["name"] = "xray_source_v26.9.8.tar.gz"
            if kind == "long-source": self.release["assets"][-1]["name"] = "xray_source_v" + "9" * 128 + ".9.tar.gz"
            with self.subTest(kind=kind), self.assertRaises(mirror.MirrorError): self.read_info()

    def test_asset_digest_size_and_origin_are_mandatory(self):
        asset = self.release["assets"][0]
        for field, bad in (("digest", None), ("digest", "sha256:" + "A" * 64), ("digest", "md5:" + "a" * 64),
                           ("size", True), ("size", 0), ("size", mirror.MAX_ASSET_BYTES + 1),
                           ("browser_download_url", "https://evil.example/file"),
                           ("browser_download_url", asset["browser_download_url"] + "?token=private"),
                           ("browser_download_url", asset["browser_download_url"].replace("v0.9.0", "v0.8.0"))):
            with self.subTest(field=field, bad=bad):
                old = asset[field]
                asset[field] = bad
                with self.assertRaises(mirror.MirrorError): self.read_info()
                asset[field] = old

    def test_asset_state_id_and_api_url_are_bound(self):
        asset = self.release["assets"][0]
        for field, bad in (("state", "new"), ("state", None), ("id", True), ("id", 0),
                           ("id", self.release["assets"][1]["id"]),
                           ("url", "https://api.github.com/repos/attacker/project/releases/assets/1"),
                           ("url", mirror.API_BASE + "/releases/assets/999")):
            with self.subTest(field=field, bad=bad):
                old = asset[field]
                asset[field] = bad
                with self.assertRaises(mirror.MirrorError): self.read_info()
                asset[field] = old

    def test_json_duplicate_keys_and_multiple_documents_fail(self):
        for raw in (b'{"immutable":true,"immutable":false}', b'{}\n{}', b'[]', b'{"x":NaN}', b'\xff'):
            with self.subTest(raw=raw), self.assertRaises(mirror.MirrorError): mirror._decode_json(raw)

    def test_public_metadata_is_exact_without_urls_and_includes_notes(self):
        raw = mirror.metadata_bytes(self.read_info())
        value = json.loads(raw)
        self.assertEqual(set(value), {"schema_version", "tag", "version", "commit", "release_id", "published_at", "notes", "assets"})
        self.assertEqual(value["schema_version"], 1)
        self.assertEqual(value["notes"], "修复与更新\n")
        self.assertLess(len(raw), mirror.MAX_METADATA_BYTES)
        self.assertEqual(len(value["assets"]), 11)
        self.assertTrue(all(set(asset) == {"sha256", "size"} for asset in value["assets"].values()))
        self.assertNotIn(b"github.com", raw)

    def test_release_notes_are_bounded_utf8_text(self):
        for notes in ([], 123, "x" * (mirror.MAX_NOTES_BYTES + 1), "中" * (mirror.MAX_NOTES_BYTES // 3 + 1), "\ud800"):
            with self.subTest(notes=repr(notes)[:40]):
                self.release["body"] = notes
                with self.assertRaises(mirror.MirrorError):
                    self.read_info()
        self.release["body"] = None
        self.assertEqual(self.read_info()["notes"], "")
        self.release["body"] = "x" * mirror.MAX_NOTES_BYTES
        self.assertEqual(self.read_info()["notes"], self.release["body"])

    def test_manifest_is_exact_and_does_not_trust_supplied_base_url(self):
        expected = (b'{"version":"0.9.0","tag":"v0.9.0","base_url":"https://dl.ll.cd/proxyscene/v0.9.0",'
                    b'"commit":"' + b'a' * 40 + b'","release_id":1234,"published_at":"2020-01-02T03:04:05Z"}\n')
        self.assertEqual(mirror.manifest_bytes(self.info), expected)
        self.info["base_url"] = "https://evil.example"
        with self.assertRaises(mirror.MirrorError): mirror.manifest_bytes(self.info)


class LocalAssetTests(Fixture):
    def test_exact_directory_and_downloaded_modes(self):
        directory = self.root / "downloaded"
        with mock.patch.object(mirror, "_download_url", side_effect=self.fake_download):
            mirror.download_release(self.info, directory)
        self.assertEqual(set(p.name for p in directory.iterdir()), set(self.names))
        self.assertTrue(all(stat.S_IMODE(p.stat().st_mode) == 0o644 for p in directory.iterdir()))
        self.assertTrue(all(set(options) == {"deadline"} for _, _, options in self.requests))
        self.assertEqual(len({options["deadline"] for _, _, options in self.requests}), 1)
        mirror.verify_directory(self.info, directory)
        with self.assertRaises(mirror.MirrorError): mirror.download_release(self.info, directory)

    def test_public_directory_modes_ignore_callers_umask(self):
        previous = os.umask(0o077)
        try:
            with mock.patch.object(mirror, "_download_url", side_effect=self.fake_download):
                directory = self.root / "private-umask-download"
                mirror.download_release(self.info, directory)
            self.assertEqual(stat.S_IMODE(directory.stat().st_mode), 0o755)
            self.assertTrue(all(stat.S_IMODE(p.stat().st_mode) == 0o644 for p in directory.iterdir()))
        finally:
            os.umask(previous)

    def test_extra_missing_modified_and_truncated_asset_fail(self):
        for fault in ("extra", "missing", "changed", "truncated"):
            directory = self.write_assets(fault)
            asset = directory / "install.sh"
            if fault == "extra": (directory / "extra").write_text("bad")
            if fault == "missing": asset.unlink()
            if fault == "changed": asset.write_bytes(b"x" * asset.stat().st_size)
            if fault == "truncated": asset.write_bytes(b"x")
            with self.subTest(fault=fault), self.assertRaises(mirror.MirrorError):
                mirror.verify_directory(self.info, directory)

    def test_symlink_hardlink_fifo_and_directory_assets_fail_without_blocking(self):
        for fault in ("symlink", "hardlink", "fifo", "directory"):
            directory = self.write_assets(fault)
            asset = directory / "install.sh"
            copy_path = self.root / (fault + ".original")
            asset.rename(copy_path)
            if fault == "symlink": asset.symlink_to(copy_path)
            if fault == "hardlink": os.link(copy_path, asset)
            if fault == "fifo": os.mkfifo(asset)
            if fault == "directory": asset.mkdir()
            with self.subTest(fault=fault), self.assertRaises(mirror.MirrorError):
                mirror.verify_directory(self.info, directory)

    def test_directory_symlink_and_symlink_ancestor_rejected(self):
        real = self.write_assets()
        link = self.root / "link"
        link.symlink_to(real, target_is_directory=True)
        with self.assertRaises(mirror.MirrorError): mirror.verify_directory(self.info, link)
        with self.assertRaises(mirror.MirrorError): mirror.download_release(self.info, link / "new")

    def test_authenticated_manifest_must_cover_exact_asset_hashes(self):
        valid = self.files["checksums.txt"]
        for fault, data in (
            ("duplicate", valid + valid.splitlines(keepends=True)[0]),
            ("missing", b"".join(valid.splitlines(keepends=True)[1:])),
            ("self", valid + b"0" * 64 + b"  checksums.txt\n"),
            ("wrong-digest", b"0" * 64 + valid[64:]),
            ("crlf", valid.replace(b"\n", b"\r\n")),
            ("no-newline", valid[:-1]),
            ("path", valid.replace(b"  install.sh\n", b"  ../install.sh\n")),
        ):
            self.change_checksums(data)
            directory = self.write_assets(fault)
            with self.subTest(fault=fault), self.assertRaises(mirror.MirrorError):
                mirror.verify_directory(self.info, directory)


class LifecycleTests(Fixture):
    def test_prepare_records_bound_info_outside_assets(self):
        directory, record = self.root / "download", self.root / "record.json"
        with mock.patch.object(mirror, "release_info", return_value=self.info) as release, mock.patch.object(mirror, "_download_url", side_effect=self.fake_download):
            mirror.prepare("v0.9.0", directory, record, "secret")
        self.assertEqual(json.loads(record.read_bytes()), self.info)
        self.assertEqual(release.call_count, 2)
        self.assertEqual(len(list(directory.iterdir())), 11)

    def test_prepare_rejects_identity_drift_and_internal_record(self):
        changed = copy.deepcopy(self.info)
        changed["commit"] = "b" * 40
        with mock.patch.object(mirror, "release_info", side_effect=[self.info, changed]), mock.patch.object(mirror, "_download_url", side_effect=self.fake_download), self.assertRaises(mirror.MirrorError):
            mirror.prepare("v0.9.0", self.root / "download", self.root / "record.json")
        self.assertFalse((self.root / "record.json").exists())
        with self.assertRaises(mirror.MirrorError):
            mirror.prepare("v0.9.0", self.root / "other", self.root / "other" / "record.json")

    def test_public_verify_includes_metadata_and_stable_index(self):
        source = self.write_assets()
        output = self.root / "public"
        with mock.patch.object(mirror, "release_info", return_value=self.info), mock.patch.object(mirror, "_download_url", side_effect=self.fake_download):
            result = mirror.verify_public("v0.9.0", source, output, True, "secret")
        self.assertEqual(result["asset_count"], 11)
        self.assertTrue(result["stable_verified"])
        self.assertEqual(len(list(output.iterdir())), 11)
        self.assertTrue((self.root / "public.result.json").is_file())
        self.assertEqual(len(self.requests), 13)
        self.assertTrue(result["metadata_verified"])
        self.assertTrue(all(options.get("mirror") is True and set(options) == {"mirror", "deadline"} for _, _, options in self.requests))
        self.assertNotIn(mirror.MIRROR_BASE + "/install.sh", [url for url, _, _ in self.requests])

    def test_mirror_changed_bytes_fail_and_leave_no_success_result(self):
        source = self.write_assets()
        def corrupt(url, destination, limit, **kwargs):
            self.fake_download(url, destination, limit, **kwargs)
            if url.endswith("/install.sh"): Path(destination).write_bytes(b"x" * limit)
        with mock.patch.object(mirror, "release_info", return_value=self.info), mock.patch.object(mirror, "_download_url", side_effect=corrupt), self.assertRaises(mirror.MirrorError):
            mirror.verify_public("v0.9.0", source, self.root / "public")
        self.assertFalse((self.root / "public.result.json").exists())

    def test_public_metadata_identity_notes_asset_or_encoding_drift_fails(self):
        source = self.write_assets()
        for fault in ("commit", "notes", "digest", "url", "extra", "encoding"):
            def corrupt_metadata(url, destination, limit, **kwargs):
                self.fake_download(url, destination, limit, **kwargs)
                if "/metadata/" in url:
                    data = json.loads(mirror.metadata_bytes(self.info))
                    if fault == "commit": data["commit"] = "b" * 40
                    if fault == "notes": data["notes"] = "changed"
                    if fault == "digest": data["assets"]["install.sh"]["sha256"] = "b" * 64
                    if fault == "url": data["assets"]["install.sh"]["url"] = "https://evil.example"
                    if fault == "extra": data["extra"] = True
                    raw = mirror._canonical_json(data)
                    if fault == "encoding": raw += b"\n"
                    Path(destination).write_bytes(raw)
            with self.subTest(fault=fault), mock.patch.object(mirror, "release_info", return_value=self.info), mock.patch.object(mirror, "_download_url", side_effect=corrupt_metadata), self.assertRaisesRegex(mirror.MirrorError, "public release metadata"):
                mirror.verify_public("v0.9.0", source, self.root / fault)
            self.assertFalse((self.root / (fault + ".result.json")).exists())

    def test_stable_manifest_semantic_or_byte_drift_fails(self):
        source = self.write_assets()
        def corrupt_index(url, destination, limit, **kwargs):
            self.fake_download(url, destination, limit, **kwargs)
            if url.endswith("/latest.json"):
                Path(destination).write_bytes(mirror.manifest_bytes(self.info) + b"\n")
        with mock.patch.object(mirror, "release_info", return_value=self.info), mock.patch.object(mirror, "_download_url", side_effect=corrupt_index), self.assertRaises(mirror.MirrorError):
            mirror.verify_public("v0.9.0", source, self.root / "public", True)

    def test_stable_latest_changed_rejected_before_download(self):
        source = self.write_assets()
        changed = copy.deepcopy(self.info)
        changed["release_id"] += 1
        with mock.patch.object(mirror, "release_info", side_effect=[self.info, changed]), mock.patch.object(mirror, "_download_url") as download, self.assertRaises(mirror.MirrorError):
            mirror.verify_public("v0.9.0", source, self.root / "public", True)
        download.assert_not_called()

    def test_explicit_byte_comparison_rejects_differences(self):
        first, second = self.root / "first", self.root / "second"
        first.write_bytes(b"same length")
        second.write_bytes(b"evil length")
        with self.assertRaises(mirror.MirrorError): mirror._same_file_bytes(first, second, 11)


class TransportTests(Fixture):
    def fake_run(self, command, **kwargs):
        os.write(kwargs["pass_fds"][0], b"data")
        self.command, self.options = command, kwargs
        return subprocess.CompletedProcess(command, 0, "200")

    def test_api_token_only_in_stdin_and_no_redirect(self):
        with mock.patch.dict(os.environ, {"GH_TOKEN": "secret", "GITHUB_TOKEN": "secret"}), mock.patch.object(mirror.subprocess, "run", side_effect=self.fake_run):
            mirror._download_url(mirror.API_BASE + "/releases/latest", self.root / "api", 100, api=True, token="secret")
        self.assertNotIn("secret", " ".join(self.command))
        self.assertIn("Authorization: Bearer secret", self.options["input"])
        self.assertNotIn("GH_TOKEN", self.options["env"])
        self.assertNotIn("GITHUB_TOKEN", self.options["env"])
        self.assertNotIn("--location", self.command)
        self.assertEqual(self.command[self.command.index("--max-redirs") + 1], "0")
        self.assertIn("--max-time", self.command)
        self.assertIn("--max-filesize", self.command)
        self.assertEqual(self.options["timeout"], mirror.API_TIMEOUT)
        self.assertTrue(callable(self.options["preexec_fn"]))
        self.assertEqual(stat.S_IMODE((self.root / "api").stat().st_mode), 0o644)

    def test_standard_bearer_tokens_only_pass_through_stdin_config(self):
        fixtures = (
            ("legacy", "ghs_fixture_123"),
            ("installation", "ghs_fixture.header-payload.signature"),
            ("alphabet", "AZaz09-._~+/"),
            ("padding", "AZaz09-._~+/=="),
            ("length-limit", "x" * (mirror.MAX_API_TOKEN_BYTES - 1) + "="),
        )
        for label, token in fixtures:
            with self.subTest(label=label), mock.patch.dict(os.environ, {"GH_TOKEN": token, "GITHUB_TOKEN": token}), mock.patch.object(mirror.subprocess, "run", side_effect=self.fake_run):
                mirror._download_url(mirror.API_BASE + "/releases/latest", self.root / label, 100, api=True, token=token)
                self.assertEqual(self.options["input"], 'header = "Authorization: Bearer ' + token + '"\n')
                self.assertEqual(self.command[self.command.index("--config") + 1], "-")
                self.assertNotIn(token, " ".join(self.command))
                self.assertNotIn("GH_TOKEN", self.options["env"])
                self.assertNotIn("GITHUB_TOKEN", self.options["env"])
                self.assertEqual(self.options["stderr"], subprocess.DEVNULL)

    def test_invalid_bearer_tokens_rejected_before_process_or_file_creation(self):
        invalid = (
            "", b"bytes", 123, "x" * (mirror.MAX_API_TOKEN_BYTES + 1),
            "=", "==", "=prefix", "middle=padding", "padding==suffix",
            "double\"quote", "single'quote", "back\\slash", "\"\nurl = \"https://evil.example",
            "carriage\rreturn", "line\nfeed", "trailing\n", "null\0byte", "delete\x7f",
            "space separated", " leading", "trailing ", "tab\there", "vertical\vtab", "form\ffeed",
            "nonascii-é", "unicode\u00a0space", "colon:header", "semicolon;value",
        )
        for token in invalid:
            with self.subTest(token=repr(token)[:80]), mock.patch.object(mirror.subprocess, "run") as run, self.assertRaisesRegex(mirror.MirrorError, "^invalid GitHub API token syntax$"):
                mirror._download_url(mirror.API_BASE + "/releases/latest", self.root / "invalid-token", 100, api=True, token=token)
            run.assert_not_called()
            self.assertFalse((self.root / "invalid-token").exists())

    def test_github_assets_are_anonymous_https_and_mirror_cannot_redirect(self):
        for origin, extra in ((mirror.RELEASE_BASE + "/v0.9.0/install.sh", {}), (mirror.MIRROR_BASE + "/v0.9.0/install.sh", {"mirror": True})):
            with self.subTest(origin=origin), mock.patch.object(mirror.subprocess, "run", side_effect=self.fake_run):
                destination = self.root / ("mirror" if extra else "github")
                mirror._download_url(origin, destination, 100, **extra)
                self.assertEqual(self.options["input"], "")
                self.assertNotIn("--config", self.command)
                self.assertEqual("--location" in self.command, not bool(extra))
                self.assertEqual(self.command[self.command.index("--proto-redir") + 1], "=https")

    def test_redirect_and_oversized_empty_or_failed_response_rejected(self):
        for fault in ("redirect", "too-large", "empty", "failure", "timeout"):
            def faulty(command, **kwargs):
                if fault == "timeout": raise subprocess.TimeoutExpired(command, 1)
                os.write(kwargs["pass_fds"][0], b"x" * (11 if fault == "too-large" else 0 if fault == "empty" else 4))
                return subprocess.CompletedProcess(command, 22 if fault == "failure" else 0, "302" if fault == "redirect" else "200")
            with self.subTest(fault=fault), mock.patch.object(mirror.subprocess, "run", side_effect=faulty), self.assertRaises(mirror.MirrorError):
                mirror._download_url(mirror.API_BASE + "/releases/latest", self.root / fault, 10, api=True)

    def test_total_deadline_bounds_each_transfer_and_expires_before_network(self):
        with mock.patch.object(mirror.time, "monotonic", return_value=100), mock.patch.object(mirror.subprocess, "run", side_effect=self.fake_run):
            mirror._download_url(mirror.RELEASE_BASE + "/v0.9.0/install.sh", self.root / "bounded", 100, deadline=103)
        self.assertEqual(self.options["timeout"], 3)
        self.assertEqual(self.command[self.command.index("--max-time") + 1], "2")
        with mock.patch.object(mirror.time, "monotonic", return_value=100), mock.patch.object(mirror.subprocess, "run") as run, self.assertRaises(mirror.MirrorError):
            mirror._download_url(mirror.RELEASE_BASE + "/v0.9.0/install.sh", self.root / "expired", 100, deadline=100)
        run.assert_not_called()
        self.assertFalse((self.root / "expired").exists())

    def test_kernel_file_limit_bounds_a_real_downloader_process(self):
        actual_run = subprocess.run
        def oversized_child(_command, **kwargs):
            descriptor = kwargs["pass_fds"][0]
            script = "import os; os.write(" + str(descriptor) + ", b'x' * 200); os.write(" + str(descriptor) + ", b'y')"
            return actual_run([sys.executable, "-B", "-c", script], **kwargs)
        destination = self.root / "kernel-bounded"
        with mock.patch.object(mirror.subprocess, "run", side_effect=oversized_child), self.assertRaises(mirror.MirrorError):
            mirror._download_url(mirror.RELEASE_BASE + "/v0.9.0/install.sh", destination, 100)
        self.assertLessEqual(destination.stat().st_size, 100)

    def test_credentials_and_source_confusion_rejected_before_network(self):
        cases = [
            ("https://evil.example/release", {"api": True, "token": "secret"}),
            (mirror.RELEASE_BASE + "/v0.9.0/install.sh", {"token": "secret"}),
            (mirror.MIRROR_BASE + "/v0.9.0/install.sh", {"mirror": True, "token": "AZaz09-._~+/=="}),
            (mirror.API_BASE + "/releases/latest", {"token": "AZaz09-._~+/=="}),
            ("https://api.github.com/repos/other/project/releases/latest", {"api": True, "token": "AZaz09-._~+/=="}),
            (mirror.API_BASE + "/releases/latest", {"api": True, "token": "bad\nheader"}),
            (mirror.MIRROR_BASE + "/v0.9.0/install.sh?token=secret", {"mirror": True}),
            ("https://dl.ll.cd.evil/proxyscene/v0.9.0/install.sh", {"mirror": True}),
            ("http://dl.ll.cd/proxyscene/v0.9.0/install.sh", {"mirror": True}),
            ("https://secret@dl.ll.cd/proxyscene/v0.9.0/install.sh", {"mirror": True}),
        ]
        for url, options in cases:
            with self.subTest(url=url), mock.patch.object(mirror.subprocess, "run") as run, self.assertRaises(mirror.MirrorError):
                mirror._download_url(url, self.root / "uncreated", 10, **options)
            run.assert_not_called()
        self.assertFalse((self.root / "uncreated").exists())


if __name__ == "__main__":
    unittest.main()
