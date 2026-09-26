#!/usr/bin/python3
"""Fixed-command, unprivileged receiver for the proxyscene release mirror."""

from __future__ import annotations

from contextlib import contextmanager
import ctypes
import datetime
import errno
import fcntl
import importlib.util
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import stat
import sys


MODULE_PATH = Path(__file__).absolute().with_name("mirror_release.py")
# Do not use sys.path, PYTHONPATH, user-site packages, or a caller-selected module.
# Refuse a swapped/linkable sibling before executing it. Deployment additionally
# requires root ownership and protected ancestors in require_runtime below.
_module_info = MODULE_PATH.lstat()
_code_info = Path(__file__).absolute().lstat()
if (not stat.S_ISREG(_module_info.st_mode) or _module_info.st_nlink != 1
        or _module_info.st_uid != _code_info.st_uid or _module_info.st_mode & 0o7022):
    raise RuntimeError("adjacent mirror release verifier has unsafe metadata")
_spec = importlib.util.spec_from_file_location("proxyscene_mirror_release", MODULE_PATH)
if _spec is None or _spec.loader is None:
    raise RuntimeError("cannot load the adjacent mirror release verifier")
release = importlib.util.module_from_spec(_spec)
sys.modules[_spec.name] = release
_spec.loader.exec_module(release)

PROJECT_ROOT = Path("/www/wwwroot/dl.ll.cd/proxyscene")
INCOMING_ROOT = Path("/var/lib/proxyscene-mirror")
OPERATION_TIMEOUT_SECONDS = 600
MAX_METADATA_BYTES = 1024 * 1024
MAX_INCOMING_BYTES = 4 * 1024**3
MAX_PROJECT_BYTES = 32 * 1024**3
MAX_PROJECT_VERSIONS = 128
MAX_INCOMING_ENTRIES = 128
MIN_AVAILABLE_BYTES = 1024**3
STAGE_PATTERN = re.compile(r"stage-[0-9a-f]{32}")
ASSET_PATTERN = re.compile(
    r"(?:checksums[.]txt|install[.]sh|"
    r"proxyscene(?:_bundle)?_linux_(?:amd64|arm64|386|armv7)[.]tar[.]gz|"
    r"xray_source_v[0-9]+(?:[.][0-9]+)+[.]tar[.]gz)"
)
LIBC = ctypes.CDLL(None, use_errno=True)
RENAMEAT2 = getattr(LIBC, "renameat2", None)
if RENAMEAT2 is not None:
    RENAMEAT2.argtypes = (ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint)
    RENAMEAT2.restype = ctypes.c_int


class ReceiverError(release.MirrorError):
    pass


class ReceiverInterrupted(BaseException):
    """Must unwind even when a download helper catches ordinary exceptions."""


def fail(message: str) -> None:
    raise ReceiverError(message)


def parse_request(command: str) -> tuple[str, str]:
    if not isinstance(command, str) or len(command) > 160:
        fail("invalid SSH command length")
    match = re.fullmatch(r"proxyscene-mirror (sync|promote) (v[0-9]+[.][0-9]+[.][0-9]+)", command)
    if match is None:
        fail("only proxyscene-mirror sync/promote followed by one stable tag is allowed")
    return match[1], release.validate_tag(match[2])


def require_directory(path: Path, owner: int, mode: int) -> os.stat_result:
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != owner or stat.S_IMODE(info.st_mode) != mode:
        fail(f"unsafe directory owner, type, or mode: {path}")
    return info


def require_file(path: Path, owner: int, modes: tuple[int, ...], maximum: int) -> os.stat_result:
    info = path.lstat()
    if (not stat.S_ISREG(info.st_mode) or info.st_uid != owner or info.st_nlink != 1
            or stat.S_IMODE(info.st_mode) not in modes or info.st_size > maximum):
        fail(f"unsafe file owner, type, links, mode, or size: {path}")
    return info


def require_ancestry(path: Path, owner: int, mode: int) -> None:
    if not path.is_absolute() or path.resolve(strict=True) != path:
        fail(f"noncanonical deployment path: {path}")
    current = Path("/")
    for part in path.parts[1:]:
        current /= part
        info = current.lstat()
        if (not stat.S_ISDIR(info.st_mode) or info.st_uid != (owner if current == path else 0)
                or info.st_mode & 0o7022):
            fail(f"unsafe deployment ancestor: {current}")
    require_directory(path, owner, mode)


def require_runtime() -> int:
    owner = os.getuid()
    if owner == 0 or os.geteuid() != owner:
        fail("mirror receiver must run as an ordinary, non-root deployment account")
    for code in (Path(__file__).absolute(), MODULE_PATH):
        require_ancestry(code.parent, 0, 0o755)
        require_file(code, 0, (0o644, 0o755), MAX_METADATA_BYTES)
    require_ancestry(PROJECT_ROOT, owner, 0o755)
    require_ancestry(INCOMING_ROOT, owner, 0o700)
    if (PROJECT_ROOT == INCOMING_ROOT or PROJECT_ROOT in INCOMING_ROOT.parents
            or INCOMING_ROOT in PROJECT_ROOT.parents):
        fail("public and private roots must be separate")
    if PROJECT_ROOT.stat().st_dev != INCOMING_ROOT.stat().st_dev:
        fail("public and private roots must be on the same filesystem")
    return owner


@contextmanager
def exact_umask(mask: int):
    previous = os.umask(mask)
    try:
        yield
    finally:
        os.umask(previous)


@contextmanager
def deployment_lock(owner: int):
    path = INCOMING_ROOT / ".deploy.lock"
    with exact_umask(0):
        descriptor = os.open(path, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC, 0o600)
    try:
        info = os.fstat(descriptor)
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != owner or info.st_nlink != 1
                or stat.S_IMODE(info.st_mode) != 0o600 or info.st_size != 0):
            fail("deployment lock has unsafe metadata")
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            fail("another mirror deployment is active")
        yield
    finally:
        # Keep the same lock inode between invocations; unlinking permits split locks.
        os.close(descriptor)


def fsync_directory(path: Path) -> None:
    descriptor = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def rename_noreplace(source: Path, destination: Path) -> None:
    if RENAMEAT2 is None:
        fail("renameat2(RENAME_NOREPLACE) is required")
    ctypes.set_errno(0)
    if RENAMEAT2(-100, os.fsencode(source), -100, os.fsencode(destination), 1) != 0:
        error = ctypes.get_errno()
        if error == errno.EEXIST:
            fail(f"immutable destination appeared concurrently: {destination}")
        fail(f"atomic no-replace publication failed: {os.strerror(error)}")


def allocated_bytes(info: os.stat_result) -> int:
    return max(info.st_size, info.st_blocks * 512)


def release_usage(directory: Path, owner: int) -> int:
    total = allocated_bytes(require_directory(directory, owner, 0o755))
    names = []
    with os.scandir(directory) as entries:
        for entry in entries:
            names.append(entry.name)
            if len(names) > 11 or ASSET_PATTERN.fullmatch(entry.name) is None:
                fail(f"unexpected release path: {directory / entry.name}")
            total += allocated_bytes(require_file(directory / entry.name, owner, (0o644,), MAX_INCOMING_BYTES))
    if len(names) != 11:
        fail(f"published release is incomplete: {directory}")
    return total


def require_project_budget(
    owner: int, additional: int = 0, new_version: bool = False, already_allocated: bool = False
) -> None:
    require_directory(PROJECT_ROOT, owner, 0o755)
    total, versions = additional, int(new_version)
    with os.scandir(PROJECT_ROOT) as entries:
        for entry in entries:
            path = PROJECT_ROOT / entry.name
            if entry.name == "latest.json":
                total += allocated_bytes(require_file(path, owner, (0o644,), MAX_METADATA_BYTES))
            else:
                release.validate_tag(entry.name)
                versions += 1
                if versions > MAX_PROJECT_VERSIONS:
                    fail("mirror release count limit exceeded")
                total += release_usage(path, owner)
            if total > MAX_PROJECT_BYTES:
                fail("mirror project byte limit exceeded")
    if versions > MAX_PROJECT_VERSIONS or total > MAX_PROJECT_BYTES:
        fail("projected mirror storage limit exceeded")
    space = os.statvfs(PROJECT_ROOT)
    reserve = max(MIN_AVAILABLE_BYTES, (space.f_blocks * space.f_frsize + 19) // 20)
    if space.f_bavail * space.f_frsize < reserve + (0 if already_allocated else additional):
        fail("mirror filesystem would fall below its free-space reserve")


def require_incoming_budget(owner: int, additional: int = 0) -> None:
    total, entries = additional, 13 if additional else 0
    if total > MAX_INCOMING_BYTES:
        fail("projected private staging byte limit exceeded")

    def inspect(directory: Path, depth: int) -> None:
        nonlocal total, entries
        if depth > 2:
            fail("unexpected private staging depth")
        with os.scandir(directory) as children:
            for child in children:
                entries += 1
                if entries > MAX_INCOMING_ENTRIES:
                    fail("private staging entry limit exceeded")
                path = directory / child.name
                info = path.lstat()
                if stat.S_ISDIR(info.st_mode):
                    if depth == 0 and STAGE_PATTERN.fullmatch(child.name) is None:
                        fail("unexpected private staging directory")
                    modes = (0o700,) if depth == 0 else (0o700, 0o755)
                    if info.st_uid != owner or stat.S_IMODE(info.st_mode) not in modes:
                        fail(f"unsafe private staging directory: {path}")
                    inspect(path, depth + 1)
                else:
                    require_file(path, owner, (0o600, 0o644), MAX_INCOMING_BYTES)
                    if depth == 0 and child.name != ".deploy.lock":
                        fail("unexpected private staging file")
                total += allocated_bytes(info)
                if total > MAX_INCOMING_BYTES:
                    fail("aggregate private staging byte limit exceeded")

    require_directory(INCOMING_ROOT, owner, 0o700)
    inspect(INCOMING_ROOT, 0)


@contextmanager
def private_stage(owner: int):
    stage = INCOMING_ROOT / ("stage-" + secrets.token_hex(16))
    with exact_umask(0):
        stage.mkdir(mode=0o700)

    def cleanup() -> None:
        require_directory(stage, owner, 0o700)
        shutil.rmtree(stage)
        fsync_directory(INCOMING_ROOT)
        require_incoming_budget(owner)

    try:
        yield stage
    except BaseException as original:
        try:
            cleanup()
        except BaseException as cleanup_error:
            raise original from cleanup_error
        raise
    else:
        cleanup()


def verify_published(info: dict, owner: int) -> Path:
    directory = PROJECT_ROOT / info["tag"]
    release_usage(directory, owner)
    release.verify_directory(info, directory)
    return directory


def sync_release(tag: str, owner: int) -> dict:
    info = release.release_info(tag)
    destination = PROJECT_ROOT / tag
    if destination.exists() or destination.is_symlink():
        verify_published(info, owner)
        # Also repair durability after a previous rename succeeded but fsync failed.
        fsync_directory(PROJECT_ROOT)
        return info
    size = sum(asset["size"] for asset in info["assets"].values()) + MAX_METADATA_BYTES
    require_project_budget(owner, size, new_version=True)
    require_incoming_budget(owner, size)
    with private_stage(owner) as stage:
        payload = stage / "release"
        with exact_umask(0o077):
            release.download_release(info, payload)
        require_incoming_budget(owner)
        release.verify_directory(info, payload)
        for name in info["assets"]:
            path = payload / name
            require_file(path, owner, (0o600, 0o644), info["assets"][name]["size"])
            descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC)
            try:
                os.fchmod(descriptor, 0o644)
                os.fsync(descriptor)
            finally:
                os.close(descriptor)
        os.chmod(payload, 0o755, follow_symlinks=False)
        fsync_directory(payload)
        if release.release_info(tag) != info:
            fail("GitHub release identity changed during synchronization")
        # The download already occupies this filesystem; do not reserve it twice.
        require_project_budget(owner, size, new_version=True, already_allocated=True)
        rename_noreplace(payload, destination)
        fsync_directory(PROJECT_ROOT)
        fsync_directory(stage)
    return info


def current_manifest(owner: int) -> tuple[dict, bytes] | None:
    path = PROJECT_ROOT / "latest.json"
    if not path.exists() and not path.is_symlink():
        return None
    require_file(path, owner, (0o644,), MAX_METADATA_BYTES)
    raw = path.read_bytes()
    try:
        value = json.loads(raw.decode("ascii"))
    except (UnicodeError, ValueError, RecursionError) as error:
        raise ReceiverError("invalid existing latest.json") from error
    fields = {"version", "tag", "base_url", "commit", "release_id", "published_at"}
    if not isinstance(value, dict) or set(value) != fields:
        fail("unexpected latest.json fields")
    if not all(isinstance(value[name], str) for name in fields - {"release_id"}):
        fail("invalid latest.json field types")
    tag = release.validate_tag(value["tag"])
    if (value["version"] != tag[1:] or value["base_url"] != f"{release.MIRROR_BASE}/{tag}"
            or re.fullmatch(r"[0-9a-f]{40}", value["commit"]) is None
            or type(value["release_id"]) is not int or value["release_id"] <= 0):
        fail("inconsistent latest.json release identity")
    if re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z", value["published_at"]) is None:
        fail("invalid latest.json timestamp")
    try:
        datetime.datetime.fromisoformat(value["published_at"][:-1] + "+00:00")
    except ValueError as error:
        raise ReceiverError("invalid latest.json timestamp") from error
    canonical = {key: value[key] for key in ("version", "tag", "base_url", "commit", "release_id", "published_at")}
    if (json.dumps(canonical, ensure_ascii=True, separators=(",", ":")) + "\n").encode("ascii") != raw:
        fail("noncanonical latest.json")
    release_usage(PROJECT_ROOT / tag, owner)
    return value, raw


def promote_release(tag: str, owner: int) -> dict:
    info = release.release_info(tag)
    if release.release_info("latest") != info:
        fail("requested release is not the exact GitHub Latest")
    verify_published(info, owner)
    candidate = release.manifest_bytes(info)
    current = current_manifest(owner)
    if current is not None:
        value, raw = current
        if release.version_key(tag) < release.version_key(value["tag"]):
            fail("refusing to roll back mirror Latest")
        if tag == value["tag"]:
            if candidate != raw:
                fail("refusing to mutate metadata for the current version")
            fsync_directory(PROJECT_ROOT)
            return info
    require_project_budget(owner, len(candidate))
    with private_stage(owner) as stage:
        temporary = stage / "latest.json"
        with exact_umask(0):
            descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600)
        with os.fdopen(descriptor, "wb") as stream:
            stream.write(candidate)
            stream.flush()
            os.fchmod(stream.fileno(), 0o644)
            os.fsync(stream.fileno())
        if release.release_info("latest") != info:
            fail("GitHub Latest changed before promotion")
        if current_manifest(owner) != current:
            fail("local Latest changed during promotion")
        if current is None:
            rename_noreplace(temporary, PROJECT_ROOT / "latest.json")
        else:
            os.replace(temporary, PROJECT_ROOT / "latest.json")
        fsync_directory(PROJECT_ROOT)
        fsync_directory(stage)
    return info


def receive() -> dict:
    operation, tag = parse_request(os.environ.get("SSH_ORIGINAL_COMMAND", ""))
    # SSH callers may not choose curl, a proxy, a CA bundle, or API credentials.
    # The sole caller-controlled value was parsed above; nothing else is needed.
    os.environ.clear()
    os.environ.update({"PATH": "/usr/bin:/bin", "LC_ALL": "C"})
    owner = require_runtime()
    with deployment_lock(owner):
        require_incoming_budget(owner)
        require_project_budget(owner)
        info = sync_release(tag, owner) if operation == "sync" else promote_release(tag, owner)
        require_incoming_budget(owner)
        require_project_budget(owner)
    return {"operation": operation, "tag": info["tag"], "release_id": info["release_id"]}


@contextmanager
def controlled_operation():
    signals = (signal.SIGALRM, signal.SIGTERM, signal.SIGHUP, signal.SIGINT)
    previous = {number: signal.getsignal(number) for number in signals}
    interrupted = False

    def stop(number, _frame):
        nonlocal interrupted
        if not interrupted:
            interrupted = True
            signal.setitimer(signal.ITIMER_REAL, 0)
            raise ReceiverInterrupted(f"receiver interrupted by {signal.Signals(number).name}")

    try:
        for number in signals:
            signal.signal(number, stop)
        signal.setitimer(signal.ITIMER_REAL, OPERATION_TIMEOUT_SECONDS)
        yield
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        for number, handler in previous.items():
            signal.signal(number, handler)


def main() -> int:
    try:
        if len(sys.argv) != 1:
            fail("receiver takes no command-line arguments")
        with controlled_operation():
            result = receive()
        print(json.dumps(result, separators=(",", ":")))
        return 0
    except (release.MirrorError, OSError, ReceiverInterrupted) as error:
        print(f"mirror receiver: {error}", file=sys.stderr)
        if error.__cause__ is not None:
            print(f"mirror receiver detail: {error.__cause__}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
