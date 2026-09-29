#!/usr/bin/env bash
# Stable mirror-only entry point. Keep installation in the release's installer.
# Sourcing is inert; download this file completely before executing it.
if [[ "${BASH_SOURCE[0]:-}" != "$0" ]]; then
  return 0
fi
set -euo pipefail
umask 077
PATH=/usr/sbin:/usr/bin:/sbin:/bin
export PATH
if ! command -v python3 >/dev/null 2>&1; then
  printf 'proxyscene: 缺少 python3；请先用系统包管理器安装 python3。\n' >&2
  exit 1
fi
# FD 3 retains the caller's terminal/input while Python reads this program.
exec python3 -I - "$@" 3<&0 <<'PY_BOOTSTRAP'
import argparse
import contextlib
import datetime
import hashlib
import json
import os
from pathlib import Path
import platform
import pwd
import re
import resource
import shutil
import signal
import stat
import subprocess
import sys
if sys.version_info < (3, 8):
    print("proxyscene: 需要 Python 3.8 或更新版本。", file=sys.stderr)
    raise SystemExit(1)
import tarfile
import tempfile
import time
import zlib

BASE = "https://dl.ll.cd/proxyscene"
MIN_VERSION = (0, 11, 0)
MAX_METADATA = 1024 * 1024
MAX_ASSET = 256 * 1024 * 1024
MAX_RELEASE = 512 * 1024 * 1024
VERSION = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\Z")
SHA = re.compile(r"[0-9a-f]{64}\Z")
SOURCE = re.compile(r"xray_source_v[0-9]+(?:\.[0-9]+)+\.tar\.gz\Z")
ARCHES = ("amd64", "arm64", "386", "armv7")
COMPONENTS = {"LICENSE", "LICENSE-Xray", "NOTICE", "SOURCE-Xray",
              "THIRD_PARTY_LICENSES", "THIRD_PARTY_LICENSES-Xray",
              "install.sh", "proxyscene", "xray", "xray-version.txt"}
ENV = {"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LANG": "C.UTF-8"}
ACTIVE_INSTALLER = None
ACTIVE_GROUP = None
INTERRUPTED = False


class Rejected(Exception):
    pass


def require(ok, message):
    if not ok:
        raise Rejected(message)


def version(tag):
    require(isinstance(tag, str) and len(tag) <= 128 and VERSION.fullmatch(tag), "版本号必须是 v 开头的正式版本，如 v0.11.0")
    return tuple(int(part) for part in tag[1:].split("."))


def object_pairs(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, "JSON 包含重复字段")
        result[key] = value
    return result


def read_json(path):
    data = path.read_bytes()
    require(len(data) <= MAX_METADATA, "版本元数据过大")
    try:
        return json.loads(data.decode("utf-8"), object_pairs_hook=object_pairs,
                          parse_constant=lambda value: (_ for _ in ()).throw(Rejected("JSON 非有限数值")))
    except (ValueError, UnicodeError, RecursionError) as exc:
        raise Rejected("版本元数据不是有效的 UTF-8 JSON") from exc


def identity(doc):
    require(isinstance(doc, dict), "发行元数据必须是对象")
    tag = doc.get("tag")
    version(tag)
    require(doc.get("version") == tag[1:], "发行 version/tag 不一致")
    require(isinstance(doc.get("commit"), str) and re.fullmatch(r"[0-9a-f]{40}", doc["commit"]), "发行 commit 无效")
    require(type(doc.get("release_id")) is int and 0 < doc["release_id"] <= 2**63 - 1, "发行 ID 无效")
    published = doc.get("published_at")
    require(isinstance(published, str) and re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z", published), "发行时间无效")
    try:
        stamp = datetime.datetime.strptime(published, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=datetime.timezone.utc)
        require(stamp <= datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(minutes=5), "发行时间超出允许的时钟偏差")
    except ValueError as exc:
        raise Rejected("发行时间无效") from exc
    return tag


def validate_latest(doc):
    require(isinstance(doc, dict) and set(doc) == {"version", "tag", "base_url", "commit", "release_id", "published_at"}, "Latest 字段无效")
    tag = identity(doc)
    require(doc["base_url"] == BASE + "/" + tag, "Latest 下载位置不是固定镜像路径")
    return tag


def validate_metadata(doc, tag, latest=None):
    require(isinstance(doc, dict) and set(doc) == {"schema_version", "tag", "version", "commit", "release_id", "published_at", "notes", "assets"}, "发行元数据字段无效")
    require(type(doc["schema_version"]) is int and doc["schema_version"] == 1, "不支持的发行元数据格式")
    require(identity(doc) == tag, "发行元数据版本与请求不一致")
    require(isinstance(doc["notes"], str), "发行说明格式无效")
    try:
        require(len(doc["notes"].encode("utf-8")) <= 64 * 1024, "发行说明超过 64 KiB")
    except UnicodeError as exc:
        raise Rejected("发行说明包含无效 Unicode") from exc
    if latest is not None:
        for key in ("version", "tag", "commit", "release_id", "published_at"):
            require(doc[key] == latest[key], "Latest 与固定版本身份不一致")
    assets = doc["assets"]
    require(isinstance(assets, dict) and len(assets) == 11, "发行必须包含 11 项资产")
    expected = {"install.sh", "checksums.txt"}
    for arch in ARCHES:
        expected.update(("proxyscene_linux_" + arch + ".tar.gz", "proxyscene_bundle_linux_" + arch + ".tar.gz"))
    sources = set(assets) - expected
    require(expected <= set(assets) and len(sources) == 1 and all(len(name) <= 128 and SOURCE.fullmatch(name) for name in sources), "发行资产白名单不匹配")
    total = 0
    for name, asset in assets.items():
        limit = MAX_METADATA if name in ("install.sh", "checksums.txt") else MAX_ASSET
        require(isinstance(asset, dict) and set(asset) == {"sha256", "size"}, "资产字段无效")
        require(isinstance(asset["sha256"], str) and SHA.fullmatch(asset["sha256"]), "资产 SHA256 无效")
        require(type(asset["size"]) is int and 0 < asset["size"] <= limit, "资产大小无效")
        total += asset["size"]
    require(total <= MAX_RELEASE, "发行资产总大小超限")
    return assets


def download_limit(limit):
    def apply():
        resource.setrlimit(resource.RLIMIT_FSIZE, (limit, limit))
    return apply


def download(relative, destination, limit):
    # Construct URLs here only; metadata never supplies a URL. No redirects,
    # curlrc, proxy environment, alternate host, or network fallback is used.
    require(re.fullmatch(r"(?:latest\.json|metadata/v[0-9]+\.[0-9]+\.[0-9]+\.json|v[0-9]+\.[0-9]+\.[0-9]+/[A-Za-z0-9_.-]+)", relative), "下载路径无效")
    result = subprocess.run(["curl", "-q", "--fail", "--silent", "--show-error",
                             "--proto", "=https", "--max-redirs", "0",
                             "--connect-timeout", "15", "--max-time", "300",
                             "--max-filesize", str(limit), "--output", str(destination),
                             "--write-out", "%{http_code}", "--", BASE + "/" + relative],
                            env=ENV, stdout=subprocess.PIPE, text=True, check=False,
                            preexec_fn=download_limit(limit))
    require(result.returncode == 0 and result.stdout == "200", "镜像下载失败或发生重定向：" + relative)
    require(destination.is_file() and destination.stat().st_size <= limit, "镜像下载文件大小超限")


def digest(path):
    value = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            value.update(chunk)
    return value.hexdigest()


def verify_asset(path, asset):
    require(path.stat().st_size == asset["size"] and digest(path) == asset["sha256"], "文件大小或 SHA256 不匹配：" + path.name)


def checksums(path, expected):
    require(path.stat().st_size <= MAX_METADATA, "校验清单过大")
    try:
        text = path.read_bytes().decode("ascii")
        require(text.endswith("\n") and "\r" not in text, "校验清单必须使用 LF 并以换行结束")
        lines = text[:-1].split("\n")
    except UnicodeError as exc:
        raise Rejected("校验清单不是 ASCII 文本") from exc
    actual = {}
    for line in lines:
        match = re.fullmatch(r"([0-9a-f]{64})  ([A-Za-z0-9_.-]+)", line)
        require(match is not None and match[2] not in actual, "校验清单格式无效或条目重复")
        actual[match[2]] = match[1]
    require(actual == expected, "校验清单与发行元数据不一致")


def unpack_gzip(archive, output):
    # tarfile can stop before gzip verifies its footer. Decode the entire one
    # permitted gzip stream with a hard bound before parsing any tar header.
    decoder = zlib.decompressobj(16 + zlib.MAX_WBITS)
    size = 0
    with archive.open("rb") as source, output.open("xb") as target:
        while True:
            chunk = source.read(64 * 1024)
            if not chunk:
                break
            require(not decoder.eof, "bundle gzip 包含额外流或尾部数据")
            while chunk:
                data = decoder.decompress(chunk, 1024 * 1024)
                chunk = decoder.unconsumed_tail
                size += len(data)
                require(size <= MAX_RELEASE, "bundle 解压大小超限")
                target.write(data)
                require(not decoder.unused_data, "bundle gzip 包含额外流或尾部数据")
        require(decoder.eof, "bundle gzip 截断或缺少有效尾部")
    return size


def extract_bundle(archive, destination, arch, assets):
    package = "proxyscene_bundle_linux_" + arch
    names = COMPONENTS | {"bundle-manifest.sha256"}
    seen = set()
    total = 0
    # Validate all headers before creating component files. Never extractall:
    # only known regular files are copied into a newly private directory.
    raw_tar = destination.with_name("bundle.tar")
    unpack_gzip(archive, raw_tar)
    with tarfile.open(raw_tar, "r:") as tar:
        members = []
        for member in tar:
            require(len(seen) < len(names) + 1, "bundle 包含多余条目")
            require(member.name not in seen and not member.pax_headers and not member.sparse and member.offset_data == member.offset + 512, "bundle 包含重复或扩展条目")
            seen.add(member.name)
            if member.name == package:
                require(member.isdir() and member.mode == 0o755, "bundle 根目录类型或权限无效")
                continue
            require(member.name.startswith(package + "/") and member.name[len(package)+1:] in names, "bundle 包含非白名单路径")
            name = member.name[len(package)+1:]
            require(member.type in (tarfile.REGTYPE, tarfile.AREGTYPE) and not member.linkname, "bundle 组件必须是普通文件")
            wanted_mode = 0o755 if name in ("proxyscene", "xray", "install.sh") else 0o644
            require(member.mode == wanted_mode and 0 < member.size <= MAX_ASSET, "bundle 组件权限或大小无效")
            total += member.size
            require(total <= MAX_RELEASE, "bundle 解压大小超限")
            members.append((member, name))
        require(seen == {package} | {package + "/" + name for name in names}, "bundle 缺少必要组件")
        with raw_tar.open("rb") as raw:
            raw.seek(tar.offset)
            ending = raw.read(10241)
        require(1024 <= len(ending) <= 10240 and len(ending) % 512 == 0 and not any(ending), "bundle tar 尾部无效或包含隐藏条目")
        destination.mkdir(mode=0o700)
        for member, name in members:
            with tar.extractfile(member) as source, (destination / name).open("xb") as target:
                shutil.copyfileobj(source, target, 1024 * 1024)
            os.chmod(destination / name, 0o700 if name in ("proxyscene", "xray", "install.sh") else 0o600)
    raw_tar.unlink()
    checksums(destination / "bundle-manifest.sha256", {name: digest(destination / name) for name in COMPONENTS})
    verify_asset(destination / "install.sh", assets["install.sh"])


def trusted_chain(path):
    require(path.is_absolute(), "安装路径必须是绝对路径")
    for item in [Path("/")] + list(reversed(path.parents[:-1])) + [path]:
        info = item.lstat()
        require(stat.S_ISDIR(info.st_mode) and info.st_uid == 0 and not info.st_mode & 0o022, "安装路径不是 root 可信目录：" + str(item))


def verify_default_installation(path=Path("/etc/proxyscene-host-ownership.json")):
    trusted_chain(path.parent)
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    except FileNotFoundError:
        return
    with os.fdopen(fd, "rb") as source:
        info = os.fstat(source.fileno())
        require(stat.S_ISREG(info.st_mode) and info.st_uid == 0 and stat.S_IMODE(info.st_mode) == 0o600,
                "主机安装记录不是可信的 root 0600 普通文件")
        data = source.read(MAX_METADATA + 1)
    require(len(data) <= MAX_METADATA, "主机安装记录过大")
    try:
        record = json.loads(data.decode("utf-8"), object_pairs_hook=object_pairs)
    except (ValueError, UnicodeError, RecursionError) as exc:
        raise Rejected("主机安装记录损坏") from exc
    expected = {"version": 1, "core_dir": "/opt/proxyscene", "install_bin": "/usr/local/bin/proxyscene",
                "systemd_service": "proxyscene.service", "restore_service": "proxyscene-restore.service"}
    require(isinstance(record, dict) and type(record.get("version")) is int and record == expected,
            "检测到自定义安装位置或安装记录不匹配；请使用原安装的 proxyscene update 入口")


def installer_env():
    env = dict(ENV)
    root = pwd.getpwuid(0)
    env.update(HOME=root.pw_dir, USER=root.pw_name, LOGNAME=root.pw_name)
    sudo_user = os.environ.get("SUDO_USER")
    if sudo_user:
        try:
            account = pwd.getpwnam(sudo_user)
        except KeyError as exc:
            raise Rejected("SUDO_USER 不是有效的系统用户") from exc
        for name, wanted in (("SUDO_UID", account.pw_uid), ("SUDO_GID", account.pw_gid)):
            require(name not in os.environ or os.environ[name] == str(wanted), "sudo 用户身份不一致")
            env[name] = str(wanted)
        env["SUDO_USER"] = sudo_user
    return env


def installed_state(path, target, commit):
    trusted_chain(path.parent)
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    except FileNotFoundError:
        return ["--expected-manager-absent"]
    with os.fdopen(fd, "rb") as source:
        info = os.fstat(source.fileno())
        require(stat.S_ISREG(info.st_mode) and info.st_uid == 0 and not info.st_mode & 0o7022 and info.st_mode & 0o100, "现有 proxyscene 不是可信的 root 可执行普通文件")
        current_digest = hashlib.sha256()
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            current_digest.update(chunk)
        # The descriptor pins the exact checked inode across rename races.
        result = subprocess.run(["/proc/self/fd/" + str(source.fileno()), "version"],
                                pass_fds=(source.fileno(),), env=installer_env(),
                                stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, text=True, timeout=15, check=False)
    match = re.fullmatch(r"proxyscene (v?(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)) \(([0-9a-f]{12})\)\n?", result.stdout)
    require(result.returncode == 0 and match is not None, "现有程序未报告正式版本，请先人工核实安装来源")
    current = version("v" + match[1].lstrip("v"))
    wanted = version(target)
    require(wanted >= current, "镜像版本低于当前版本，拒绝降级")
    require(wanted != current or commit.startswith(match[2]), "同一版本的 commit 不一致，拒绝安装")
    return ["--expected-manager-sha256", current_digest.hexdigest()]


def select_arch(machine):
    names = {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64",
             "i386": "386", "i686": "386", "armv7l": "armv7", "armhf": "armv7"}
    require(machine in names, "不支持的机器架构：" + machine)
    return names[machine]


def group_alive(group):
    try:
        os.killpg(group, 0)
    except ProcessLookupError:
        return False
    # Zombies cannot use the archive and may await reaping by PID 1.
    for entry in Path("/proc").iterdir():
        if not entry.name.isdigit():
            continue
        try:
            fields = (entry / "stat").read_text().rsplit(")", 1)[1].split()
        except FileNotFoundError:
            continue
        if int(fields[2]) == group and fields[0] not in ("Z", "X"):
            return True
    return False


def signal_group(group, signum):
    try:
        os.killpg(group, signum)
    except ProcessLookupError:
        pass


def finish_group(group):
    for signum in (signal.SIGTERM, signal.SIGKILL):
        if not group_alive(group):
            return
        if signum != signal.SIGTERM or not INTERRUPTED:
            signal_group(group, signum)
        deadline = time.monotonic() + 5
        while group_alive(group) and time.monotonic() < deadline:
            time.sleep(0.05)
    require(not group_alive(group), "安装子进程尚未退出；保留临时安装包供恢复")


@contextlib.contextmanager
def private_workspace():
    path = Path(tempfile.mkdtemp(prefix="proxyscene-bootstrap.", dir="/var/lib"))
    try:
        yield path
    finally:
        if ACTIVE_GROUP is not None and group_alive(ACTIVE_GROUP):
            print("proxyscene: 安装子进程仍在运行，保留临时目录：" + str(path), file=sys.stderr)
        else:
            shutil.rmtree(path)


def run_installer(path, precondition, env, stdin):
    global ACTIVE_INSTALLER, ACTIVE_GROUP
    # Close the spawn/registration signal window. The child restores the
    # caller's signal mask before executing the installer.
    previous_mask = signal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGINT, signal.SIGTERM, signal.SIGHUP})
    try:
        process = subprocess.Popen(["/bin/bash", str(path), "--offline", *precondition],
                                   env=env, stdin=stdin, start_new_session=True,
                                   preexec_fn=lambda: signal.pthread_sigmask(signal.SIG_SETMASK, previous_mask))
        ACTIVE_INSTALLER, ACTIVE_GROUP = process, process.pid
    finally:
        signal.pthread_sigmask(signal.SIG_SETMASK, previous_mask)
    deadline = None
    try:
        while True:
            try:
                return process.wait(timeout=0.2)
            except subprocess.TimeoutExpired:
                if INTERRUPTED:
                    deadline = deadline or time.monotonic() + 5
                    if time.monotonic() >= deadline:
                        signal_group(process.pid, signal.SIGKILL)
    finally:
        try:
            finish_group(process.pid)
            process.wait()
        finally:
            ACTIVE_INSTALLER = None
            if not group_alive(process.pid):
                ACTIVE_GROUP = None


def install(argv, stdin=3):
    parser = argparse.ArgumentParser(description="从 dl.ll.cd 安装或升级 proxyscene；安装端不访问 GitHub。需要 root、Python 3.8+、curl、jq、flock、coreutils 和 systemd。")
    parser.add_argument("--version", metavar="vX.Y.Z", help="指定正式版本（至少 v0.11.0）；默认使用镜像已完整发布的最新版本")
    args = parser.parse_args(argv)
    if args.version:
        require(version(args.version) >= MIN_VERSION, "固定安装入口支持 v0.11.0 起；旧版请使用 README 的固定版本安装流程")
    require(sys.platform == "linux" and os.geteuid() == 0, "请在 Linux 上用 sudo bash 安装脚本运行")
    for command in ("curl", "jq", "flock", "sha256sum", "stat", "awk", "od", "tr", "install", "cp", "mv", "rm", "mkdir", "mktemp", "chmod", "dirname", "basename", "readlink", "systemctl", "id", "grep"):
        require(shutil.which(command, path=ENV["PATH"]), "缺少系统工具 " + command + "；请先用系统包管理器安装所需依赖")
    arch = select_arch(platform.machine())
    trusted_chain(Path("/var/lib"))
    verify_default_installation()
    env = installer_env()
    with private_workspace() as work:
        work = Path(work)
        os.chmod(work, 0o700)
        latest = None
        tag = args.version
        if tag is None:
            download("latest.json", work / "latest.json", MAX_METADATA)
            latest = read_json(work / "latest.json")
            tag = validate_latest(latest)
        require(version(tag) >= MIN_VERSION, "固定安装入口支持 v0.11.0 起；镜像尚未提供支持版本，或请使用 README 的旧版安装流程")
        download("metadata/" + tag + ".json", work / "metadata.json", MAX_METADATA)
        release = read_json(work / "metadata.json")
        assets = validate_metadata(release, tag, latest)
        precondition = installed_state(Path("/usr/local/bin/proxyscene"), tag, release["commit"])
        print("proxyscene: 已锁定镜像版本 " + tag + "，正在校验完整安装包。", flush=True)
        download(tag + "/checksums.txt", work / "checksums.txt", assets["checksums.txt"]["size"])
        verify_asset(work / "checksums.txt", assets["checksums.txt"])
        checksums(work / "checksums.txt", {name: asset["sha256"] for name, asset in assets.items() if name != "checksums.txt"})
        bundle = "proxyscene_bundle_linux_" + arch + ".tar.gz"
        download(tag + "/" + bundle, work / bundle, assets[bundle]["size"])
        verify_asset(work / bundle, assets[bundle])
        unpacked = work / "bundle"
        extract_bundle(work / bundle, unpacked, arch, assets)
        download("metadata/" + tag + ".json", work / "metadata-confirm.json", MAX_METADATA)
        confirmed = read_json(work / "metadata-confirm.json")
        validate_metadata(confirmed, tag, latest)
        require(confirmed == release, "下载期间固定版本元数据发生变化，拒绝安装")
        print("proxyscene: 校验通过，开始离线安装 " + tag + "。", flush=True)
        # No arbitrary arguments or caller-controlled installer override variables.
        # A separate process group receives cancellation as a whole; retain
        # the archive until every live installer descendant has exited.
        result = run_installer(unpacked / "install.sh", precondition, env, stdin)
        require(result == 0 and not INTERRUPTED, "离线安装未成功完成；文件可能已经提交，请按上方安装器提示处理")
    return 0


def interrupted(signum, frame):
    global INTERRUPTED
    INTERRUPTED = True
    if ACTIVE_INSTALLER is not None:
        signal_group(ACTIVE_INSTALLER.pid, signum)
        return
    raise Rejected("安装入口收到中断信号")


if __name__ == "__main__":
    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGHUP, interrupted)
    try:
        sys.exit(install(sys.argv[1:]))
    except (Rejected, OSError, tarfile.TarError, zlib.error, subprocess.SubprocessError, KeyboardInterrupt) as error:
        print("proxyscene: 错误：" + str(error), file=sys.stderr)
        sys.exit(1)
PY_BOOTSTRAP
