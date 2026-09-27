#!/usr/bin/env python3
"""Prepare and verify byte-identical mirrors of immutable GitHub releases.

The mirror is a transport, not a release trust root. This module never executes
or extracts downloaded assets. GitHub metadata and SHA256 digests authenticate
the exact release set. Optional API authentication is never sent to asset hosts.

CLI prepare writes RECORD outside DIRECTORY. CLI verify downloads the public
version into OUTPUT (exactly eleven assets), and writes OUTPUT.result.json next
to that directory. --stable additionally verifies root/latest.json; there is no
mutable root/install.sh. All output paths must be new.
"""

import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import resource
import stat
import subprocess
import sys
import tempfile
import time
from urllib.parse import urlsplit


class MirrorError(Exception):
    """A release identity, integrity, transport, or local path check failed."""


REPOSITORY = "longlannet/proxyscene"
API_BASE = "https://api.github.com/repos/" + REPOSITORY
RELEASE_BASE = "https://github.com/" + REPOSITORY + "/releases/download"
MIRROR_BASE = "https://dl.ll.cd/proxyscene"
MAX_METADATA_BYTES = 1024 * 1024
MAX_ASSET_BYTES = 256 * 1024 * 1024
MAX_RELEASE_BYTES = 512 * 1024 * 1024
MAX_API_TOKEN_BYTES = 4096
NETWORK_TIMEOUT = 180
DOWNLOAD_BUDGET = 300
API_TIMEOUT = 40
ARCHITECTURES = ("amd64", "arm64", "386", "armv7")
FIXED_ASSETS = frozenset(
    ["install.sh", "checksums.txt"]
    + ["proxyscene_linux_" + arch + ".tar.gz" for arch in ARCHITECTURES]
    + ["proxyscene_bundle_linux_" + arch + ".tar.gz" for arch in ARCHITECTURES]
)
TAG_PATTERN = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\Z")
SOURCE_PATTERN = re.compile(r"xray_source_v[0-9]+(?:\.[0-9]+)+\.tar\.gz\Z")
SHA_PATTERN = re.compile(r"[0-9a-f]{64}\Z")
COMMIT_PATTERN = re.compile(r"[0-9a-f]{40}\Z")
INFO_KEYS = frozenset(("tag", "version", "commit", "release_id", "published_at", "assets"))


def validate_tag(tag):
    if not isinstance(tag, str) or len(tag) > 128 or not TAG_PATTERN.fullmatch(tag):
        raise MirrorError("release tag must be canonical stable vMAJOR.MINOR.PATCH (at most 128 bytes)")
    return tag


def version_key(tag):
    return tuple(int(part) for part in validate_tag(tag)[1:].split("."))


def _positive_integer(value, label, limit=None):
    if type(value) is not int or value <= 0 or (limit is not None and value > limit):
        raise MirrorError("invalid " + label)
    return value


def _timestamp(value):
    if not isinstance(value, str) or not re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z", value):
        raise MirrorError("invalid release publication timestamp")
    try:
        parsed = datetime.datetime.strptime(value, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=datetime.timezone.utc)
    except ValueError as exc:
        raise MirrorError("invalid release publication timestamp") from exc
    if parsed > datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(minutes=5):
        raise MirrorError("release publication timestamp is in the future")
    return value


def _asset_names(names):
    if len(names) != 11 or not FIXED_ASSETS.issubset(names):
        raise MirrorError("release must contain the exact eleven supported assets")
    extra = set(names) - FIXED_ASSETS
    if len(extra) != 1 or not SOURCE_PATTERN.fullmatch(next(iter(extra))):
        raise MirrorError("release must contain exactly one versioned Xray source archive")


def _asset_limit(name):
    return MAX_METADATA_BYTES if name in ("install.sh", "checksums.txt") else MAX_ASSET_BYTES


def _validate_info(info):
    if not isinstance(info, dict) or set(info) != INFO_KEYS:
        raise MirrorError("invalid release record fields")
    tag = validate_tag(info["tag"])
    if info["version"] != tag[1:]:
        raise MirrorError("release record version and tag disagree")
    if not isinstance(info["commit"], str) or not COMMIT_PATTERN.fullmatch(info["commit"]):
        raise MirrorError("invalid release commit")
    _positive_integer(info["release_id"], "release ID")
    _timestamp(info["published_at"])
    assets = info["assets"]
    if not isinstance(assets, dict) or not all(isinstance(name, str) for name in assets):
        raise MirrorError("invalid release asset map")
    _asset_names(assets)
    total = 0
    for name, asset in assets.items():
        if not isinstance(asset, dict) or set(asset) != {"sha256", "size", "url"}:
            raise MirrorError("invalid asset metadata for " + name)
        if not isinstance(asset["sha256"], str) or not SHA_PATTERN.fullmatch(asset["sha256"]):
            raise MirrorError("missing or invalid GitHub SHA256 digest for " + name)
        total += _positive_integer(asset["size"], "asset size for " + name, _asset_limit(name))
        if asset["url"] != RELEASE_BASE + "/" + tag + "/" + name:
            raise MirrorError("unexpected GitHub asset URL for " + name)
    if total > MAX_RELEASE_BYTES:
        raise MirrorError("release exceeds the total size limit")
    return info


def _json_object_pairs(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise MirrorError("JSON contains duplicate keys")
        result[key] = value
    return result


def _decode_json(data):
    def reject_constant(_value):
        raise MirrorError("non-finite JSON number")

    try:
        result = json.loads(data.decode("utf-8"), object_pairs_hook=_json_object_pairs,
                            parse_constant=reject_constant)
    except (UnicodeError, ValueError) as exc:
        raise MirrorError("invalid JSON response") from exc
    if not isinstance(result, dict):
        raise MirrorError("JSON response must be one object")
    return result


def _canonical_json(value):
    return (json.dumps(value, ensure_ascii=True, separators=(",", ":")) + "\n").encode("ascii")


def _plain_directory(path):
    path = Path(path).absolute()
    for component in [*reversed(path.parents), path]:
        try:
            metadata = component.lstat()
        except OSError as exc:
            raise MirrorError("directory does not exist: " + str(component)) from exc
        if not stat.S_ISDIR(metadata.st_mode) or stat.S_ISLNK(metadata.st_mode):
            raise MirrorError("directory path contains a symlink or non-directory: " + str(component))
    return path


def _new_path(path):
    path = Path(path).absolute()
    _plain_directory(path.parent)
    if os.path.lexists(path):
        raise MirrorError("output path already exists: " + str(path))
    return path


def _download_url(url, destination, max_bytes, *, api=False, token=None, mirror=False, deadline=None):
    """Download one bounded HTTPS object. API and mirror responses cannot redirect."""
    parsed = urlsplit(url)
    if parsed.scheme != "https" or parsed.username is not None or parsed.password is not None or parsed.fragment or parsed.query:
        raise MirrorError("download URL violates the HTTPS source policy")
    if api:
        if not url.startswith(API_BASE + "/") or parsed.netloc != "api.github.com":
            raise MirrorError("API authentication is restricted to the fixed GitHub repository")
    elif token is not None:
        raise MirrorError("authentication is forbidden for asset downloads")
    elif mirror:
        if not url.startswith(MIRROR_BASE + "/") or parsed.netloc != "dl.ll.cd":
            raise MirrorError("unexpected public mirror URL")
    elif not url.startswith(RELEASE_BASE + "/") or parsed.netloc != "github.com":
        raise MirrorError("unexpected GitHub download URL")
    # RFC 6750 section 2.1 permits opaque Bearer tokens, including installation
    # tokens with punctuation. Keep a bounded ASCII value without curl config
    # quoting/control characters; padding is allowed only at the end.
    if token is not None and (not isinstance(token, str) or len(token) > MAX_API_TOKEN_BYTES
                              or not re.fullmatch(r"[A-Za-z0-9._~+/-]+=*", token)):
        raise MirrorError("invalid GitHub API token syntax")
    timeout = API_TIMEOUT if api else NETWORK_TIMEOUT
    if deadline is not None:
        timeout = min(timeout, deadline - time.monotonic())
    if timeout < 1:
        raise MirrorError("release download exceeded its total time budget")
    curl_timeout = max(1, min(120, int(timeout) - 1))
    destination = _new_path(destination)
    command = ["curl", "-q", "--silent", "--show-error", "--fail", "--proto", "=https",
               "--proto-redir", "=https", "--connect-timeout", "10", "--max-time", str(curl_timeout),
               "--retry", "1", "--retry-delay", "1", "--retry-max-time", str(curl_timeout),
               "--max-filesize", str(max_bytes),
               "--write-out", "%{http_code}"]
    if api or mirror:
        command += ["--max-redirs", "0"]
    else:
        command += ["--location", "--max-redirs", "5"]
    config = ""
    if api:
        command += ["--header", "Accept: application/vnd.github+json", "--header", "X-GitHub-Api-Version: 2022-11-28"]
        if token is not None:
            command += ["--config", "-"]
            config = 'header = "Authorization: Bearer ' + token + '"\n'
    command += ["--url", url]
    child_env = {key: value for key, value in os.environ.items() if key not in ("GH_TOKEN", "GITHUB_TOKEN")}

    def bound_file_size():
        resource.setrlimit(resource.RLIMIT_FSIZE, (max_bytes, max_bytes))

    try:
        descriptor = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600)
        try:
            command += ["--output", "/proc/self/fd/" + str(descriptor)]
            completed = subprocess.run(command, input=config, text=True, stdout=subprocess.PIPE,
                                       stderr=subprocess.DEVNULL, timeout=timeout,
                                       env=child_env, preexec_fn=bound_file_size,
                                       pass_fds=(descriptor,), check=False)
            if completed.returncode != 0 or completed.stdout != "200":
                raise MirrorError("HTTPS download failed or redirected: " + url)
            metadata = os.fstat(descriptor)
            current = destination.lstat()
            if (not stat.S_ISREG(metadata.st_mode) or metadata.st_nlink != 1
                    or not 0 < metadata.st_size <= max_bytes
                    or (metadata.st_dev, metadata.st_ino) != (current.st_dev, current.st_ino)):
                raise MirrorError("download is not a nonempty bounded regular file")
            os.fchmod(descriptor, 0o644)
        finally:
            os.close(descriptor)
    except (OSError, subprocess.SubprocessError) as exc:
        raise MirrorError("HTTPS download failed: " + url) from exc


def _api_json(endpoint, token):
    with tempfile.TemporaryDirectory(prefix="proxyscene-release-metadata-") as directory:
        target = Path(directory) / "response.json"
        _download_url(API_BASE + endpoint, target, MAX_METADATA_BYTES, api=True, token=token)
        return _decode_json(target.read_bytes())


def release_info(tag, token=None):
    requested = tag
    if tag != "latest":
        validate_tag(tag)
    release = _api_json("/releases/latest" if tag == "latest" else "/releases/tags/" + tag, token)
    tag = validate_tag(release.get("tag_name"))
    if requested != "latest" and requested != tag:
        raise MirrorError("GitHub returned a different release tag")
    if release.get("immutable") is not True or release.get("draft") is not False or release.get("prerelease") is not False:
        raise MirrorError("GitHub release must be immutable, public, and stable")
    _positive_integer(release.get("id"), "release ID")
    _timestamp(release.get("published_at"))
    raw_assets = release.get("assets")
    if not isinstance(raw_assets, list) or len(raw_assets) != 11:
        raise MirrorError("GitHub release must contain exactly eleven assets")
    assets = {}
    asset_ids = set()
    for asset in raw_assets:
        if not isinstance(asset, dict) or not isinstance(asset.get("name"), str):
            raise MirrorError("invalid GitHub asset entry")
        asset_id = _positive_integer(asset.get("id"), "GitHub asset ID")
        if asset_id in asset_ids:
            raise MirrorError("duplicate GitHub asset ID")
        asset_ids.add(asset_id)
        if asset.get("state") != "uploaded" or asset.get("url") != API_BASE + "/releases/assets/" + str(asset_id):
            raise MirrorError("GitHub asset must be uploaded and bound to its exact API URL")
        name = asset["name"]
        if name in assets:
            raise MirrorError("duplicate GitHub asset name")
        digest = asset.get("digest")
        if not isinstance(digest, str) or not digest.startswith("sha256:"):
            raise MirrorError("GitHub did not provide a SHA256 asset digest")
        assets[name] = {"sha256": digest[7:], "size": asset.get("size"), "url": asset.get("browser_download_url")}
    reference = _api_json("/git/ref/tags/" + tag, token)
    target = reference.get("object")
    if reference.get("ref") != "refs/tags/" + tag or not isinstance(target, dict) or target.get("type") != "commit":
        raise MirrorError("release tag must point directly to a commit")
    return _validate_info({"tag": tag, "version": tag[1:], "commit": target.get("sha"),
                           "release_id": release["id"], "published_at": release["published_at"], "assets": assets})


def _read_asset(path, expected_size, collect=False):
    try:
        descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC)
        with os.fdopen(descriptor, "rb") as stream:
            before = os.fstat(stream.fileno())
            if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1 or before.st_size != expected_size:
                raise MirrorError("asset must be a single-link regular file with its exact GitHub size: " + path.name)
            digest = hashlib.sha256()
            chunks = []
            count = 0
            while chunk := stream.read(1024 * 1024):
                count += len(chunk)
                if count > expected_size:
                    raise MirrorError("asset grew during verification: " + path.name)
                digest.update(chunk)
                if collect:
                    chunks.append(chunk)
            after = os.fstat(stream.fileno())
        current = path.lstat()
        identity = lambda metadata: (metadata.st_dev, metadata.st_ino, metadata.st_size, metadata.st_mtime_ns, metadata.st_ctime_ns, metadata.st_nlink)
        if count != expected_size or identity(before) != identity(after) or identity(after) != identity(current):
            raise MirrorError("asset changed during verification: " + path.name)
        return digest.hexdigest(), b"".join(chunks)
    except OSError as exc:
        raise MirrorError("cannot safely read asset: " + path.name) from exc


def _verify_checksums(info, raw):
    try:
        text = raw.decode("ascii")
    except UnicodeError as exc:
        raise MirrorError("cannot read checksum manifest") from exc
    if not text.endswith("\n") or "\r" in text or "\x00" in text:
        raise MirrorError("checksum manifest is not canonical ASCII text")
    checksums = {}
    for line in text[:-1].split("\n"):
        match = re.fullmatch(r"([0-9a-f]{64})  ([A-Za-z0-9_.-]+)", line)
        if match is None or match[2] in checksums:
            raise MirrorError("checksum manifest has malformed or duplicate entries")
        checksums[match[2]] = match[1]
    expected = {name: asset["sha256"] for name, asset in info["assets"].items() if name != "checksums.txt"}
    if checksums != expected:
        raise MirrorError("checksum manifest must match all ten other GitHub asset digests exactly")


def verify_directory(info, directory):
    _validate_info(info)
    directory = _plain_directory(directory)
    if {entry.name for entry in directory.iterdir()} != set(info["assets"]):
        raise MirrorError("directory must contain exactly the eleven release assets")
    checksum_bytes = b""
    for name, asset in sorted(info["assets"].items()):
        digest, data = _read_asset(directory / name, asset["size"], name == "checksums.txt")
        if digest != asset["sha256"]:
            raise MirrorError("SHA256 mismatch for " + name)
        if name == "checksums.txt":
            checksum_bytes = data
    _verify_checksums(info, checksum_bytes)
    if {entry.name for entry in directory.iterdir()} != set(info["assets"]):
        raise MirrorError("release directory changed during verification")


def download_release(info, directory):
    _validate_info(info)
    directory = _new_path(directory)
    directory.mkdir(mode=0o755)
    directory.chmod(0o755)
    deadline = time.monotonic() + DOWNLOAD_BUDGET
    for name, asset in sorted(info["assets"].items()):
        _download_url(asset["url"], directory / name, asset["size"], deadline=deadline)
    verify_directory(info, directory)


def manifest_bytes(info):
    _validate_info(info)
    return _canonical_json({"version": info["version"], "tag": info["tag"],
                            "base_url": MIRROR_BASE + "/" + info["tag"], "commit": info["commit"],
                            "release_id": info["release_id"], "published_at": info["published_at"]})


def _same_release(expected, current):
    if expected != current:
        raise MirrorError("GitHub release identity or asset metadata changed")


def _write_new(path, data):
    path = _new_path(path)
    try:
        with path.open("xb") as output:
            output.write(data)
        path.chmod(0o644)
    except OSError as exc:
        raise MirrorError("cannot write new evidence file: " + str(path)) from exc


def _same_file_bytes(first, second, size):
    descriptors = []
    try:
        for path in (first, second):
            descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC)
            descriptors.append(descriptor)
            metadata = os.fstat(descriptor)
            if not stat.S_ISREG(metadata.st_mode) or metadata.st_nlink != 1 or metadata.st_size != size:
                raise MirrorError("asset changed before public byte comparison")
        remaining = size
        while remaining:
            amount = min(remaining, 1024 * 1024)
            left = os.read(descriptors[0], amount)
            right = os.read(descriptors[1], amount)
            if not left or left != right:
                raise MirrorError("public mirror bytes differ for " + first.name)
            remaining -= len(left)
        if os.read(descriptors[0], 1) or os.read(descriptors[1], 1):
            raise MirrorError("asset grew during public byte comparison")
    finally:
        for descriptor in descriptors:
            os.close(descriptor)


def prepare(tag, directory, record, token=None):
    validate_tag(tag)
    directory = _new_path(directory)
    record = Path(record).absolute()
    if directory == record or directory in record.parents:
        raise MirrorError("release record must be outside the asset directory")
    _new_path(record)
    info = release_info(tag, token)
    download_release(info, directory)
    _same_release(info, release_info(tag, token))
    verify_directory(info, directory)
    _write_new(record, _canonical_json(info))
    return info


def verify_public(tag, directory, output, stable=False, token=None):
    validate_tag(tag)
    directory = _plain_directory(directory)
    output = _new_path(output)
    if directory == output or directory in output.parents or output in directory.parents:
        raise MirrorError("public verification output must be separate from the prepared directory")
    result_path = _new_path(output.with_name(output.name + ".result.json"))
    info = release_info(tag, token)
    verify_directory(info, directory)
    if stable:
        _same_release(info, release_info("latest", token))
    output.mkdir(mode=0o755)
    output.chmod(0o755)
    deadline = time.monotonic() + DOWNLOAD_BUDGET
    for name, asset in sorted(info["assets"].items()):
        _download_url(MIRROR_BASE + "/" + tag + "/" + name, output / name, asset["size"], mirror=True, deadline=deadline)
    verify_directory(info, output)
    for name in info["assets"]:
        _same_file_bytes(directory / name, output / name, info["assets"][name]["size"])
    if stable:
        with tempfile.TemporaryDirectory(prefix="proxyscene-mirror-latest-") as temporary:
            latest = Path(temporary) / "latest.json"
            _download_url(MIRROR_BASE + "/latest.json", latest, MAX_METADATA_BYTES, mirror=True, deadline=deadline)
            if latest.read_bytes() != manifest_bytes(info):
                raise MirrorError("public latest.json does not match the exact canonical release manifest")
        _same_release(info, release_info("latest", token))
    _same_release(info, release_info(tag, token))
    verify_directory(info, directory)
    verify_directory(info, output)
    result = {"tag": tag, "commit": info["commit"], "release_id": info["release_id"],
              "asset_count": len(info["assets"]), "all_bytes_match": True, "stable_verified": bool(stable)}
    _write_new(result_path, _canonical_json(result))
    return result


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    subcommands = parser.add_subparsers(dest="command", required=True)
    prep = subcommands.add_parser("prepare", help="download one immutable GitHub release")
    prep.add_argument("--tag", required=True)
    prep.add_argument("--directory", required=True, type=Path)
    prep.add_argument("--record", required=True, type=Path)
    verify = subcommands.add_parser("verify", help="verify anonymous public mirror bytes against GitHub and prepared files")
    verify.add_argument("--tag", required=True)
    verify.add_argument("--directory", required=True, type=Path)
    verify.add_argument("--output", required=True, type=Path)
    verify.add_argument("--stable", action="store_true")
    args = parser.parse_args(argv)
    token = os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN") or None
    try:
        if args.command == "prepare":
            info = prepare(args.tag, args.directory, args.record, token)
            print("prepared " + info["tag"] + " with eleven verified assets")
        else:
            result = verify_public(args.tag, args.directory, args.output, args.stable, token)
            print("verified public mirror " + result["tag"] + (" and stable index" if args.stable else ""))
        return 0
    except (MirrorError, OSError) as exc:
        print("mirror release: " + str(exc), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
