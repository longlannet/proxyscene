#!/bin/bash
set -Eeuo pipefail
umask 077

TRUSTED_ROOT_PATH="/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
PATH="$TRUSTED_ROOT_PATH"
export PATH

SCRIPT_NAME="proxyscene 安装器"
DEFAULT_GO_VERSION="1.27.1"
DEFAULT_CORE_DIR="/opt/proxyscene"
DEFAULT_INSTALL_BIN="/usr/local/bin/proxyscene"
DEFAULT_SYSTEMD_SERVICE="proxyscene.service"
DEFAULT_RESTORE_SERVICE="proxyscene-restore.service"
DEFAULT_REPO="longlannet/proxyscene"
DEFAULT_MANAGER_VERSION="latest"
DEFAULT_XRAY_VERSION="v26.9.9"
DEFAULT_XRAY_RELEASE_BASE="https://github.com/XTLS/Xray-core/releases/download/${DEFAULT_XRAY_VERSION}"
DEFAULT_DOWNLOAD_TIMEOUT=300
INSTALL_LOCK_PATH="/run/proxyscene-install.lock"
TRANSACTION_TMP_ROOT="/run/proxyscene-install-tmp"
HOST_OWNERSHIP_PATH="/etc/proxyscene-host-ownership.json"
HOST_LOCK_PATH="/run/proxyscene-host-ownership.lock"
SYSTEMD_UNIT_DIR="/etc/systemd/system"

# Repository-reviewed digests for the four supported Xray release archives.
XRAY_SHA256_AMD64="1eb9175d0f0a8f8149c9230a7fc5ae66ce332ed20a53155ce61fe62e3f58b7df"
XRAY_SHA256_ARM64="3e38d72dfc5eb65c91df0e5583e9b6676c32232041da47de6ae73946b526d66c"
XRAY_SHA256_386="52a825a06d77f6e2d0d72d8d873b1fd277ae503ac9a2aedb112f4f5db0e347e0"
XRAY_SHA256_ARMV7="5b2a9e2767c0197f7bd41c2db133f91440e845771e621178e0fd2b8520228e5f"
XRAY_SIZE_AMD64=21356337
XRAY_SIZE_ARM64=19837074
XRAY_SIZE_386=20519149
XRAY_SIZE_ARMV7=20461728

# Repository-reviewed Go 1.27.1 archive digests and exact byte sizes. Go does
# not publish per-archive .sha256 URLs; custom Go versions therefore require an
# explicit GO_TARBALL_SHA256 instead of changing checksum provenance silently.
GO_SHA256_386="3b72028095439d2bc0ce84e271cc70328a878d879020c5721eaa46df5f72fbc0"
GO_SHA256_AMD64="63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445"
GO_SHA256_ARM64="3450b45a3f9ee8568792736a5c5e70a1f2e9b36c35a8f74958c03e51d7d92bec"
GO_SHA256_ARMV6L="44893f200fb034791d4188df9fc9b9e73eadbb5fceafd5166703f0b9bab73fc2"
GO_SIZE_386=68698442
GO_SIZE_AMD64=70553950
GO_SIZE_ARM64=67009954
GO_SIZE_ARMV6L=68968196

GO_VERSION="${GO_VERSION:-$DEFAULT_GO_VERSION}"
GO_TARBALL_SHA256="${GO_TARBALL_SHA256:-}"
GO_BIN=""
CORE_DIR="${PROXYSCENE_MANAGER_DIR:-$DEFAULT_CORE_DIR}"
INSTALL_BIN="${PROXYSCENE_SWITCH_BIN:-$DEFAULT_INSTALL_BIN}"
SYSTEMD_SERVICE="${PROXYSCENE_SYSTEMD_SERVICE_NAME:-$DEFAULT_SYSTEMD_SERVICE}"
RESTORE_SERVICE="${PROXYSCENE_BOOT_RESTORE_SERVICE_NAME:-$DEFAULT_RESTORE_SERVICE}"
XRAY_RELEASE_BASE="${XRAY_RELEASE_BASE:-$DEFAULT_XRAY_RELEASE_BASE}"
XRAY_ZIP_URL="${XRAY_ZIP_URL:-}"
XRAY_ZIP_SHA256="${XRAY_ZIP_SHA256:-}"
SKIP_GO_INSTALL="${SKIP_GO_INSTALL:-0}"
SKIP_XRAY_INSTALL="${SKIP_XRAY_INSTALL:-0}"
SKIP_MANAGER_INIT="${SKIP_MANAGER_INIT:-0}"
FORCE_GO_INSTALL="${FORCE_GO_INSTALL:-0}"
MANAGER_REPO="${PROXYSCENE_REPO:-$DEFAULT_REPO}"
MANAGER_VERSION="${PROXYSCENE_VERSION:-$DEFAULT_MANAGER_VERSION}"
MANAGER_BASE_URL="${PROXYSCENE_BASE_URL:-}"
BUILD_FROM_SOURCE="${PROXYSCENE_BUILD_FROM_SOURCE:-0}"

OFFLINE_REQUESTED=0
EXPECTED_MANAGER_SHA256=""
SHOW_HELP=0
RESOLVED_MANAGER_VERSION=""
TX_DIR=""
TX_ACTIVE=0
CORE_DIR_CREATED=0
GO_TOOLCHAIN_DIR=""
INSTALL_LOCK_FD=""
HOST_LOCK_FD=""
STORE_LOCK_FD=""
declare -a TX_DESTS=()
declare -a TX_BACKUPS=()
declare -a TX_EXISTED=()

log() { printf '[%s] %s\n' "$SCRIPT_NAME" "$*"; }
fatal() { printf '[%s] 错误：%s\n' "$SCRIPT_NAME" "$*" >&2; exit 1; }

run_quiet() {
  local desc="$1" err
  shift
  if ! err="$("$@" 2>&1 >/dev/null)"; then
    [[ -n "$err" ]] && printf '%s\n' "$err" >&2
    fatal "${desc}失败"
  fi
}

usage() {
  cat <<'EOF'
用法：
  sudo bash ./install.sh
  sudo bash ./install.sh --offline

联机安装：
  请从一个明确版本的 GitHub Release 下载 install.sh 和 checksums.txt，先按
  checksums.txt 校验 install.sh、审阅脚本，再设置同一 PROXYSCENE_VERSION 执行。
  latest 会先通过 GitHub API 解析为明确 tag，随后所有文件都从该 tag 下载。

离线安装：
  先用 Release 的 checksums.txt 校验完整 bundle tar，解压后必须显式执行：
    sudo ./install.sh --offline
  包内 bundle-manifest.sha256 会在任何文件复制前再次校验所有组件。

管理程序变量：
  PROXYSCENE_VERSION=latest                    明确 tag（推荐）或 latest
  PROXYSCENE_REPO=longlannet/proxyscene        GitHub 仓库 owner/name
  PROXYSCENE_BASE_URL=https://mirror/tag       固定 tag 的自定义下载基址；必须同时显式指定版本
  PROXYSCENE_BUILD_FROM_SOURCE=1                只从当前源码目录编译

常用变量：
  GO_VERSION=1.27.1                            源码编译需要的 Go 版本；改版本时必须显式提供 SHA256
  GO_TARBALL_SHA256=...                        非默认 Go 版本必填；默认版本使用仓库内四架构固定值
  SKIP_GO_INSTALL=1                            只使用 PATH 中已有的受支持 Go
  FORCE_GO_INSTALL=1                           强制使用事务目录中的临时指定 Go
  PROXYSCENE_MANAGER_DIR=/opt/proxyscene       Xray 与状态目录
  PROXYSCENE_SWITCH_BIN=/usr/local/bin/proxyscene
  XRAY_RELEASE_BASE=https://.../v26.9.9       固定版本 Xray 的下载基址
  XRAY_ZIP_URL=https://mirror/xray.zip         自定义当前架构 Xray zip；必须同时提供 SHA256
  XRAY_ZIP_SHA256=...                          自定义 Xray zip 的固定 SHA256
  SKIP_XRAY_INSTALL=1                          保留经文件类型、属主和架构检查的现有 Xray
  SKIP_MANAGER_INIT=1                          不执行管理器初始化

  自定义或保留现有 Xray 时无法从哈希证明上游版本，xray-version.txt 会记录
  custom-sha256:<摘要> 或 existing-sha256:<摘要>，不会误写固定官方版本。
EOF
}

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      -h|--help)
        SHOW_HELP=1
        ;;
      --offline)
        OFFLINE_REQUESTED=1
        ;;
      --expected-manager-sha256)
        [[ $# -ge 2 && -z "$EXPECTED_MANAGER_SHA256" && "$2" =~ ^[0-9a-f]{64}$ ]] \
          || fatal "--expected-manager-sha256 需要唯一的 64 位 SHA256"
        EXPECTED_MANAGER_SHA256="$2"
        shift
        ;;
      -*)
        fatal "未知选项：$1"
        ;;
      *)
        fatal "不接受位置参数；安装后请用 proxyscene install 交互录入，或使用 node add/import --stdin"
        ;;
    esac
    shift
  done
  [[ -z "$EXPECTED_MANAGER_SHA256" || "$OFFLINE_REQUESTED" == "1" ]] \
    || fatal "--expected-manager-sha256 仅用于 --offline 自更新"
}

require_root() {
  [[ "$(id -u)" == "0" ]] || fatal "请用 root 运行，例如：sudo bash ./install.sh"
}

need_cmd() { command -v "$1" >/dev/null 2>&1; }

validate_boolean_flag() {
  local name="$1" value="$2"
  [[ "$value" == "0" || "$value" == "1" ]] || fatal "$name 只能是 0 或 1"
}

url_has_userinfo() {
  local authority="${1#https://}"
  authority="${authority%%/*}"
  [[ "$authority" == *"@"* ]]
}

script_dir() {
  local source="${BASH_SOURCE[0]:-}"
  [[ -n "$source" && -f "$source" ]] || return 1
  (cd "$(dirname "$source")" && pwd -P)
}

repo_dir() { script_dir; }
bundle_dir() { script_dir; }

arch_release_for_machine() {
  case "$1" in
    x86_64|amd64) printf 'amd64\n' ;;
    aarch64|arm64) printf 'arm64\n' ;;
    i386|i686) printf '386\n' ;;
    armv7l|armhf) printf 'armv7\n' ;;
    *) return 1 ;;
  esac
}

arch_release() { arch_release_for_machine "$(uname -m)"; }

arch_go() {
  case "$(uname -m)" in
    x86_64|amd64) printf 'amd64\n' ;;
    aarch64|arm64) printf 'arm64\n' ;;
    i386|i686) printf '386\n' ;;
    armv6l|armv7l|armhf) printf 'armv6l\n' ;;
    *) fatal "不支持的 Go 架构：$(uname -m)" ;;
  esac
}

xray_asset_arch_for_release_arch() {
  case "$1" in
    amd64) printf '64\n' ;;
    arm64) printf 'arm64-v8a\n' ;;
    386) printf '32\n' ;;
    armv7) printf 'arm32-v7a\n' ;;
    *) return 1 ;;
  esac
}

xray_sha256_for_release_arch() {
  case "$1" in
    amd64) printf '%s\n' "$XRAY_SHA256_AMD64" ;;
    arm64) printf '%s\n' "$XRAY_SHA256_ARM64" ;;
    386) printf '%s\n' "$XRAY_SHA256_386" ;;
    armv7) printf '%s\n' "$XRAY_SHA256_ARMV7" ;;
    *) return 1 ;;
  esac
}

xray_archive_size_for_release_arch() {
  case "$1" in
    amd64) printf '%s\n' "$XRAY_SIZE_AMD64" ;;
    arm64) printf '%s\n' "$XRAY_SIZE_ARM64" ;;
    386) printf '%s\n' "$XRAY_SIZE_386" ;;
    armv7) printf '%s\n' "$XRAY_SIZE_ARMV7" ;;
    *) return 1 ;;
  esac
}

go_tarball_sha256_for_arch() {
  case "$1" in
    386) printf '%s\n' "$GO_SHA256_386" ;;
    amd64) printf '%s\n' "$GO_SHA256_AMD64" ;;
    arm64) printf '%s\n' "$GO_SHA256_ARM64" ;;
    armv6l) printf '%s\n' "$GO_SHA256_ARMV6L" ;;
    *) return 1 ;;
  esac
}

go_tarball_size_for_arch() {
  case "$1" in
    386) printf '%s\n' "$GO_SIZE_386" ;;
    amd64) printf '%s\n' "$GO_SIZE_AMD64" ;;
    arm64) printf '%s\n' "$GO_SIZE_ARM64" ;;
    armv6l) printf '%s\n' "$GO_SIZE_ARMV6L" ;;
    *) return 1 ;;
  esac
}

elf_machine_for_release_arch() {
  case "$1" in
    amd64) printf '62\n' ;;
    arm64) printf '183\n' ;;
    386) printf '3\n' ;;
    armv7) printf '40\n' ;;
    *) return 1 ;;
  esac
}

is_sha256_hex() { [[ "$1" =~ ^[0-9A-Fa-f]{64}$ ]]; }

normalize_sha256() {
  is_sha256_hex "$1" || return 1
  printf '%s' "$1" | tr '[:upper:]' '[:lower:]'
}

sha256_file() {
  local file="$1"
  if need_cmd sha256sum; then
    sha256sum "$file" | awk '{print $1}'
  elif need_cmd shasum; then
    shasum -a 256 "$file" | awk '{print $1}'
  elif need_cmd openssl; then
    openssl dgst -sha256 "$file" | awk '{print $NF}'
  else
    fatal "找不到 sha256sum、shasum 或 openssl，无法校验 SHA256"
  fi
}

verify_sha256_file() {
  local label="$1" file="$2" expected="$3" actual
  expected="$(normalize_sha256 "$expected")" || fatal "${label} SHA256 格式无效：$expected"
  actual="$(normalize_sha256 "$(sha256_file "$file")")" || fatal "无法读取 ${label} SHA256"
  [[ "$actual" == "$expected" ]] || fatal "${label} SHA256 不匹配：期望 $expected，实际 $actual"
}

manifest_sha_for_asset() {
  local manifest="$1" asset="$2" result
  result="$(awk -v a="$asset" '
    {
      name=$2
      sub(/^\*/, "", name)
      if (name == a) { count++; value=$1 }
    }
    END {
      if (count != 1) exit 1
      print value
    }
  ' "$manifest")" || return 1
  is_sha256_hex "$result" || return 1
  printf '%s\n' "$result"
}

version_ge() {
  local have="$1" want="$2" smallest
  smallest="$(printf '%s\n%s\n' "$want" "$have" | sort -V | head -n1)"
  [[ "$smallest" == "$want" ]]
}

is_stable_go_version() { [[ "$1" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; }

parse_go_version_output() {
  local output="$1"
  if [[ "$output" =~ ^go[[:space:]]+version[[:space:]]+go([0-9]+\.[0-9]+\.[0-9]+)([[:space:]]|$) ]]; then
    printf '%s\n' "${BASH_REMATCH[1]}"
    return 0
  fi
  return 1
}

go_version_meets_requirement() {
  local have="$1" want="$2"
  is_stable_go_version "$have" && is_stable_go_version "$want" && version_ge "$have" "$want"
}

go_version_matches_exactly() {
  local have="$1" want="$2"
  is_stable_go_version "$have" && is_stable_go_version "$want" && [[ "$have" == "$want" ]]
}

validate_release_tag() { [[ "$1" =~ ^v[0-9][0-9A-Za-z._-]*$ ]]; }

parse_release_tag_json() {
  jq -er 'select(type == "object") | .tag_name | select(type == "string" and length > 0)'
}

release_json_is_immutable() {
  jq -e 'type == "object" and .immutable == true' >/dev/null
}

verified_release_tag_json() {
  local expected="$1" json tag
  json="$(cat)" || return 1
  tag="$(printf '%s\n' "$json" | parse_release_tag_json)" || return 1
  validate_release_tag "$tag" || return 1
  [[ "$expected" == "latest" || "$tag" == "$expected" ]] || return 1
  printf '%s\n' "$json" | release_json_is_immutable || return 1
  printf '%s\n' "$tag"
}

path_is_normalized_absolute() {
  local path="$1"
  [[ -n "$path" && "$path" == /* && "$path" != "/" && "$path" != */ ]] || return 1
  [[ "$path" != *[$' \t\r\n']* && "$path" != *//* ]] || return 1
  [[ "$path" != *'/../'* && "$path" != */.. && "$path" != *'/./'* && "$path" != */. ]] || return 1
}

install_mode_for_flag() {
  [[ "$1" == "1" ]] && printf 'offline\n' || printf 'online\n'
}

validate_common_inputs() {
  validate_boolean_flag SKIP_GO_INSTALL "$SKIP_GO_INSTALL"
  validate_boolean_flag SKIP_XRAY_INSTALL "$SKIP_XRAY_INSTALL"
  validate_boolean_flag SKIP_MANAGER_INIT "$SKIP_MANAGER_INIT"
  validate_boolean_flag FORCE_GO_INSTALL "$FORCE_GO_INSTALL"
  validate_boolean_flag PROXYSCENE_BUILD_FROM_SOURCE "$BUILD_FROM_SOURCE"
  is_stable_go_version "$GO_VERSION" || fatal "GO_VERSION 必须是完整稳定版本号：$GO_VERSION"
  version_ge "$GO_VERSION" "$DEFAULT_GO_VERSION" || fatal "GO_VERSION 不能低于 $DEFAULT_GO_VERSION"
  [[ -z "$GO_TARBALL_SHA256" ]] || normalize_sha256 "$GO_TARBALL_SHA256" >/dev/null \
    || fatal "GO_TARBALL_SHA256 格式无效"
  if [[ "$GO_VERSION" != "$DEFAULT_GO_VERSION" && -z "$GO_TARBALL_SHA256" ]]; then
    fatal "非默认 GO_VERSION=$GO_VERSION 必须显式设置 GO_TARBALL_SHA256"
  fi
  validate_installer_service_name "$SYSTEMD_SERVICE" "PROXYSCENE_SYSTEMD_SERVICE_NAME"
  validate_installer_service_name "$RESTORE_SERVICE" "PROXYSCENE_BOOT_RESTORE_SERVICE_NAME"
  [[ "$SYSTEMD_SERVICE" != "$RESTORE_SERVICE" ]] \
    || fatal "Xray 主服务和开机恢复服务不能使用同一个 systemd unit：$SYSTEMD_SERVICE"
}

validate_installer_service_name() {
  local name="$1" field="$2" stem
  [[ "$name" =~ ^[A-Za-z0-9_.@:-]+\.service$ && "$name" != *".."* ]] \
    || fatal "$field 必须是安全的 .service 名称：$name"
  stem="${name%.service}"
  [[ "$stem" == "proxyscene" || "$stem" == proxyscene-* || "$stem" == proxyscene@* ]] \
    || fatal "$field 必须位于 proxyscene 命名空间：$name"
}

validate_online_inputs() {
  local release_arch
  release_arch="$(arch_release)" || fatal "当前架构没有受支持的安装产物：$(uname -m)"
  if [[ "$BUILD_FROM_SOURCE" != "1" ]]; then
    validate_release_inputs
  fi
  if [[ -n "$XRAY_ZIP_URL" ]]; then
    [[ "$XRAY_ZIP_URL" == https://* ]] || fatal "XRAY_ZIP_URL 必须是 https 地址"
    url_has_userinfo "$XRAY_ZIP_URL" && fatal "XRAY_ZIP_URL 不能包含 URL userinfo 凭据"
    [[ -n "$XRAY_ZIP_SHA256" ]] || fatal "自定义 XRAY_ZIP_URL 必须同时设置 XRAY_ZIP_SHA256"
    normalize_sha256 "$XRAY_ZIP_SHA256" >/dev/null || fatal "XRAY_ZIP_SHA256 格式无效"
  else
    [[ "$XRAY_RELEASE_BASE" == https://* ]] || fatal "XRAY_RELEASE_BASE 必须是 https 地址"
    url_has_userinfo "$XRAY_RELEASE_BASE" && fatal "XRAY_RELEASE_BASE 不能包含 URL userinfo 凭据"
    xray_sha256_for_release_arch "$release_arch" >/dev/null || fatal "缺少 Xray 固定 SHA256：$release_arch"
  fi
}

has_package() {
  local pkg="$1"
  if need_cmd dpkg-query; then
    dpkg-query -W -f='${Status}' "$pkg" 2>/dev/null | grep -q 'install ok installed'
  elif need_cmd rpm; then
    rpm -q "$pkg" >/dev/null 2>&1
  elif need_cmd apk; then
    apk info -e "$pkg" >/dev/null 2>&1
  else
    return 1
  fi
}

install_packages() {
  local packages=(curl ca-certificates tar unzip coreutils jq)
  local commands=(curl "" tar unzip od jq)
  local missing=() i pkg cmd
  for i in "${!packages[@]}"; do
    pkg="${packages[$i]}"
    cmd="${commands[$i]}"
    if [[ -n "$cmd" ]]; then
      need_cmd "$cmd" || missing+=("$pkg")
    elif ! has_package "$pkg"; then
      missing+=("$pkg")
    fi
  done
  [[ ${#missing[@]} -gt 0 ]] || return 0

  log "安装基础依赖：${missing[*]}"
  if need_cmd apt-get; then
    run_quiet "更新软件包索引" env DEBIAN_FRONTEND=noninteractive apt-get update
    run_quiet "安装基础依赖" env DEBIAN_FRONTEND=noninteractive apt-get install -y "${missing[@]}"
  elif need_cmd dnf; then
    run_quiet "安装基础依赖" dnf install -y "${missing[@]}"
  elif need_cmd yum; then
    run_quiet "安装基础依赖" yum install -y "${missing[@]}"
  elif need_cmd apk; then
    run_quiet "安装基础依赖" apk add --no-cache "${missing[@]}"
  elif need_cmd zypper; then
    run_quiet "安装基础依赖" zypper --non-interactive install "${missing[@]}"
  else
    fatal "无法自动安装基础依赖，请手动安装：${missing[*]}"
  fi
}

fetch_https() {
  local url="$1" out="$2" maxsize="$3" size
  curl -q -fL --proto '=https' --proto-redir '=https' \
    --connect-timeout 15 --max-time "$DEFAULT_DOWNLOAD_TIMEOUT" \
    --retry 3 --retry-delay 2 \
    --max-filesize "$maxsize" -o "$out" "$url" || return 1
  size="$(stat -c '%s' "$out" 2>/dev/null || wc -c < "$out")"
  if [[ "$size" -gt "$maxsize" ]]; then
    log "下载内容超过大小上限（${size} > ${maxsize} 字节）"
    rm -f "$out"
    return 1
  fi
}

validate_trusted_directory_chain() {
  local path="$1" current="" component owner mode mode_value
  local -a components
  path_is_normalized_absolute "$path" || fatal "路径必须是规范化绝对路径：$path"
  IFS='/' read -r -a components <<< "${path#/}"
  for component in "${components[@]}"; do
    current="${current}/${component}"
    [[ ! -L "$current" ]] || fatal "路径祖先不能是符号链接：$current"
    if [[ -e "$current" ]]; then
      [[ -d "$current" ]] || fatal "路径祖先不是目录：$current"
      owner="$(stat -c '%u' "$current" 2>/dev/null || true)"
      mode="$(stat -c '%a' "$current" 2>/dev/null || true)"
      [[ "$owner" == "0" && "$mode" =~ ^[0-7]+$ ]] || fatal "路径祖先必须属于 root：$current"
      mode_value=$((8#$mode))
      (( (mode_value & 0022) == 0 )) || fatal "路径祖先不能由组或其他用户写入：$current"
    else
      return 0
    fi
  done
}

validate_root_regular_file() {
  local path="$1" label="$2" owner mode mode_value
  [[ -e "$path" || -L "$path" ]] || fatal "$label 不存在：$path"
  [[ ! -L "$path" && -f "$path" ]] || fatal "$label 必须是非符号链接常规文件：$path"
  owner="$(stat -c '%u' "$path" 2>/dev/null || true)"
  mode="$(stat -c '%a' "$path" 2>/dev/null || true)"
  [[ "$owner" == "0" && "$mode" =~ ^[0-7]+$ ]] || fatal "$label 必须属于 root：$path"
  mode_value=$((8#$mode))
  (( (mode_value & 0022) == 0 )) || fatal "$label 不能由组或其他用户写入：$path"
  (( (mode_value & 07000) == 0 )) || fatal "$label 不能带 setuid、setgid 或 sticky 位：$path"
}

validate_source_regular_file() {
  local path="$1" label="$2"
  [[ -f "$path" && ! -L "$path" ]] || fatal "$label 必须是非符号链接常规文件：$path"
}

validate_elf_arch() {
  local path="$1" expected_arch="$2" label="$3" magic machine expected_machine
  magic="$(od -An -N4 -t x1 "$path" | tr -d ' \n')"
  [[ "$magic" == "7f454c46" ]] || fatal "$label 不是 ELF 文件：$path"
  machine="$(od -An -j18 -N2 -t u1 "$path" | awk 'NF >= 2 { print $1 + (256 * $2); exit }')"
  expected_machine="$(elf_machine_for_release_arch "$expected_arch")" || fatal "不支持的 ELF 架构：$expected_arch"
  [[ "$machine" == "$expected_machine" ]] || fatal "$label 架构不匹配：需要 $expected_arch（ELF machine $expected_machine），实际 $machine"
}

validate_core_dir() {
  path_is_normalized_absolute "$CORE_DIR" || fatal "PROXYSCENE_MANAGER_DIR 必须是无尾斜杠的规范化绝对路径：$CORE_DIR"
  case "$CORE_DIR" in
    /opt/*|/var/lib/*|/var/opt/*) ;;
    *) fatal "PROXYSCENE_MANAGER_DIR 必须位于 /opt、/var/lib 或 /var/opt 下：$CORE_DIR" ;;
  esac
  local parent
  parent="$(dirname "$CORE_DIR")"
  [[ -d "$parent" ]] || fatal "PROXYSCENE_MANAGER_DIR 的父目录必须预先存在：$parent"
  validate_trusted_directory_chain "$parent"
  [[ ! -L "$CORE_DIR" ]] || fatal "PROXYSCENE_MANAGER_DIR 不能是符号链接：$CORE_DIR"
  [[ ! -e "$CORE_DIR" || -d "$CORE_DIR" ]] || fatal "PROXYSCENE_MANAGER_DIR 已存在但不是目录：$CORE_DIR"
  [[ ! -e "$CORE_DIR" ]] || validate_trusted_directory_chain "$CORE_DIR"
}

validate_install_bin() {
  path_is_normalized_absolute "$INSTALL_BIN" || fatal "PROXYSCENE_SWITCH_BIN 必须是无尾斜杠的规范化绝对路径：$INSTALL_BIN"
  [[ "$(basename "$INSTALL_BIN")" == "proxyscene" ]] \
    || fatal "PROXYSCENE_SWITCH_BIN 的文件名必须是 proxyscene：$INSTALL_BIN"
  local parent
  parent="$(dirname "$INSTALL_BIN")"
  [[ -d "$parent" ]] || fatal "PROXYSCENE_SWITCH_BIN 的父目录必须预先存在：$parent"
  validate_trusted_directory_chain "$parent"
  if [[ -e "$INSTALL_BIN" || -L "$INSTALL_BIN" ]]; then
    validate_root_regular_file "$INSTALL_BIN" "现有管理程序"
    if ! install_bin_is_owned "$CORE_DIR" "$INSTALL_BIN"; then
      fatal "PROXYSCENE_SWITCH_BIN 已存在但没有匹配的 proxyscene ownership：$INSTALL_BIN"
    fi
  fi
}

install_bin_is_owned() {
  local core_dir="$1" install_bin="$2"
  local ownership="$core_dir/installation-ownership.json" marker="$core_dir/.managed-by-proxyscene" marker_text
  if [[ -f "$ownership" && ! -L "$ownership" ]]; then
    validate_root_regular_file "$ownership" "安装 ownership 记录"
    [[ "$(stat -c '%a' "$ownership" 2>/dev/null || true)" == "600" ]] || return 1
    ownership_record_matches "$ownership" "$core_dir" "$install_bin" "$SYSTEMD_SERVICE" "$RESTORE_SERVICE"
    return
  fi
  # Historical markers identify only the CoreDir. Require the exact legacy unit
  # pair to bind that directory to the historical default binary; a copied or
  # stale marker alone must not replace another installation's shared binary.
  [[ "$install_bin" == "$DEFAULT_INSTALL_BIN" ]] || return 1
  [[ -f "$marker" && ! -L "$marker" ]] || return 1
  validate_root_regular_file "$marker" "管理目录 marker"
  marker_text="$(tr -d '\r\n' < "$marker")"
  [[ "$marker_text" == "由 proxyscene 安装器管理" || "$marker_text" == "由 proxyscene 管理" ]] \
    || return 1
  legacy_unit_pair_matches "$core_dir" "$install_bin"
}

systemd_quote_for_installer() {
  local value="$1"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  value="${value//%/%%}"
  printf '"%s"\n' "$value"
}

legacy_unit_pair_matches() {
  local core_dir="$1" install_bin="$2"
  local main_unit="$SYSTEMD_UNIT_DIR/$SYSTEMD_SERVICE"
  local restore_unit="$SYSTEMD_UNIT_DIR/$RESTORE_SERVICE"
  local main_exec restore_exec
  [[ -f "$main_unit" && ! -L "$main_unit" && -f "$restore_unit" && ! -L "$restore_unit" ]] || return 1
  validate_root_regular_file "$main_unit" "旧版 Xray systemd unit"
  validate_root_regular_file "$restore_unit" "旧版恢复 systemd unit"
  main_exec="ExecStart=$(systemd_quote_for_installer "$core_dir/xray") run -config $(systemd_quote_for_installer "$core_dir/config.json")"
  restore_exec="ExecStart=$(systemd_quote_for_installer "$install_bin") boot-restore"
  grep -Fxq -- "$main_exec" "$main_unit" && grep -Fxq -- "$restore_exec" "$restore_unit"
}

ownership_record_matches() {
  local path="$1" core_dir="$2" install_bin="$3" systemd_service="$4" restore_service="$5"
  local size
  size="$(stat -c '%s' "$path" 2>/dev/null || true)"
  [[ "$size" =~ ^[0-9]+$ && "$size" -le 65536 ]] || return 1
  jq -e -s \
    --arg core "$core_dir" \
    --arg bin "$install_bin" \
    --arg systemd "$systemd_service" \
    --arg restore "$restore_service" '
      length == 1 and
      (.[0] |
        type == "object" and
        keys == ["core_dir", "install_bin", "restore_service", "systemd_service", "version"] and
        .version == 1 and
        .core_dir == $core and
        .install_bin == $bin and
        .systemd_service == $systemd and
        .restore_service == $restore)
    ' "$path" >/dev/null 2>&1
}

render_installation_ownership() {
  jq -n \
    --arg core "$CORE_DIR" \
    --arg bin "$INSTALL_BIN" \
    --arg systemd "$SYSTEMD_SERVICE" \
    --arg restore "$RESTORE_SERVICE" '{
      version: 1,
      core_dir: $core,
      install_bin: $bin,
      systemd_service: $systemd,
      restore_service: $restore
    }'
}

stage_installation_ownership() {
  local source="$TX_DIR/installation-ownership.json"
  render_installation_ownership > "$source" || fatal "无法生成安装 ownership 记录"
  ownership_record_matches "$source" "$CORE_DIR" "$INSTALL_BIN" "$SYSTEMD_SERVICE" "$RESTORE_SERVICE" \
    || fatal "生成的安装 ownership 记录无效"
  transactional_replace "$source" "$CORE_DIR/installation-ownership.json" 600 "安装 ownership 记录"
}

validate_host_ownership_for_install() {
  if [[ ! -e "$HOST_OWNERSHIP_PATH" && ! -L "$HOST_OWNERSHIP_PATH" ]]; then
    return 0
  fi
  validate_root_regular_file "$HOST_OWNERSHIP_PATH" "主机 ownership 记录"
  [[ "$(stat -c '%a' "$HOST_OWNERSHIP_PATH" 2>/dev/null || true)" == "600" ]] \
    || fatal "主机 ownership 记录权限必须为 0600：$HOST_OWNERSHIP_PATH"
  ownership_record_matches \
    "$HOST_OWNERSHIP_PATH" "$CORE_DIR" "$INSTALL_BIN" "$SYSTEMD_SERVICE" "$RESTORE_SERVICE" \
    || fatal "主机已由另一套 proxyscene 安装接管，或 ownership 记录损坏：$HOST_OWNERSHIP_PATH"
}

validate_managed_core_dir() {
  local marker="$CORE_DIR/.managed-by-proxyscene" marker_text
  [[ -d "$CORE_DIR" ]] || return 0
  if ! find "$CORE_DIR" -mindepth 1 -maxdepth 1 -print -quit | grep -q .; then
    return 0
  fi
  validate_root_regular_file "$marker" "现有管理目录标记"
  marker_text="$(tr -d '\r\n' < "$marker")"
  [[ "$marker_text" == "由 proxyscene 安装器管理" || "$marker_text" == "由 proxyscene 管理" ]] \
    || fatal "现有目录不是可识别的 proxyscene 管理目录：$CORE_DIR"
}

acquire_install_lock() {
  local lock_dir
  umask 077
  need_cmd flock || fatal "缺少 flock（通常由 util-linux 提供），无法取得安装锁"
  lock_dir="$(dirname "$INSTALL_LOCK_PATH")"
  [[ -d "$lock_dir" ]] || fatal "安装锁目录不存在：$lock_dir"
  validate_trusted_directory_chain "$lock_dir"
  if [[ -e "$INSTALL_LOCK_PATH" || -L "$INSTALL_LOCK_PATH" ]]; then
    validate_root_regular_file "$INSTALL_LOCK_PATH" "现有安装锁"
  fi
  if ! exec {INSTALL_LOCK_FD}>>"$INSTALL_LOCK_PATH"; then
    fatal "无法打开安装锁：$INSTALL_LOCK_PATH"
  fi
  chmod 600 "$INSTALL_LOCK_PATH" || fatal "无法收紧安装锁权限：$INSTALL_LOCK_PATH"
  validate_root_regular_file "$INSTALL_LOCK_PATH" "安装锁"
  flock -n "$INSTALL_LOCK_FD" || fatal "另一个 proxyscene 安装器正在运行"
}

# The updater has not held the lock while downloading. Recheck its exact
# executable under our installation lock before touching any installed file.
# A CLI option (rather than an environment hint) makes older installers fail
# closed if they do not implement this precondition.
verify_expected_manager() {
  [[ -n "$EXPECTED_MANAGER_SHA256" ]] || return 0
  validate_root_regular_file "$INSTALL_BIN" "自更新前的管理程序"
  local actual
  actual="$(sha256sum -- "$INSTALL_BIN" | awk '{print $1}')" \
    || fatal "无法校验当前管理程序"
  [[ "$actual" == "$EXPECTED_MANAGER_SHA256" ]] \
    || fatal "当前程序在准备升级期间已被替换，本次升级未执行；请重新运行 proxyscene"
}

acquire_host_runtime_lock() {
  local lock_dir
  umask 077
  lock_dir="$(dirname "$HOST_LOCK_PATH")"
  [[ -d "$lock_dir" ]] || fatal "主机 ownership 锁目录不存在：$lock_dir"
  validate_trusted_directory_chain "$lock_dir"
  if [[ -e "$HOST_LOCK_PATH" || -L "$HOST_LOCK_PATH" ]]; then
    validate_root_regular_file "$HOST_LOCK_PATH" "现有主机 ownership 锁"
  fi
  if ! exec {HOST_LOCK_FD}>>"$HOST_LOCK_PATH"; then
    fatal "无法打开主机 ownership 锁：$HOST_LOCK_PATH"
  fi
  chmod 600 "$HOST_LOCK_PATH" || fatal "无法收紧主机 ownership 锁权限：$HOST_LOCK_PATH"
  validate_root_regular_file "$HOST_LOCK_PATH" "主机 ownership 锁"
  flock -n "$HOST_LOCK_FD" || fatal "另一个 proxyscene 状态事务正在运行，请稍后重试"
}

acquire_store_runtime_lock() {
  local lock_path
  umask 077
  [[ -d "$CORE_DIR" ]] || return 0
  validate_trusted_directory_chain "$CORE_DIR"
  lock_path="$CORE_DIR/.state.lock"
  if [[ -e "$lock_path" || -L "$lock_path" ]]; then
    validate_root_regular_file "$lock_path" "现有状态锁"
  fi
  if ! exec {STORE_LOCK_FD}>>"$lock_path"; then
    fatal "无法打开状态锁：$lock_path"
  fi
  chmod 600 "$lock_path" || fatal "无法收紧状态锁权限：$lock_path"
  validate_root_regular_file "$lock_path" "状态锁"
  flock -n "$STORE_LOCK_FD" || fatal "另一个 proxyscene 状态事务正在运行，请稍后重试"
}

release_runtime_locks() {
  if [[ -n "$STORE_LOCK_FD" ]]; then
    flock -u "$STORE_LOCK_FD" || fatal "无法释放状态锁"
    exec {STORE_LOCK_FD}>&-
    STORE_LOCK_FD=""
  fi
  if [[ -n "$HOST_LOCK_FD" ]]; then
    flock -u "$HOST_LOCK_FD" || fatal "无法释放主机 ownership 锁"
    exec {HOST_LOCK_FD}>&-
    HOST_LOCK_FD=""
  fi
}

cleanup_executable_go_toolchain() {
  local path="${GO_TOOLCHAIN_DIR:-}" parent base
  [[ -n "$path" ]] || return 0
  parent="${path%/*}"
  base="${path##*/}"
  [[ "$parent" == "$CORE_DIR" && "$base" == .proxyscene-go-toolchain.* && \
    "$base" != ".proxyscene-go-toolchain." ]] || return 1
  if [[ ! -e "$path" && ! -L "$path" ]]; then
    GO_TOOLCHAIN_DIR=""
    GO_BIN=""
    return 0
  fi
  [[ -d "$path" && ! -L "$path" ]] || return 1
  rm -rf -- "$path" || return 1
  GO_TOOLCHAIN_DIR=""
  GO_BIN=""
}

rollback_transaction() {
  local i dest backup existed restore status=0
  set +e
  for ((i=${#TX_DESTS[@]}-1; i>=0; i--)); do
    dest="${TX_DESTS[$i]}"
    backup="${TX_BACKUPS[$i]}"
    existed="${TX_EXISTED[$i]}"
    if [[ "$existed" == "1" ]]; then
      restore="$(mktemp "$(dirname "$dest")/.proxyscene-restore.XXXXXX")" || { status=1; continue; }
      cp -p "$backup" "$restore" && mv -f "$restore" "$dest" || status=1
      rm -f "$restore"
    else
      rm -f "$dest" || status=1
    fi
  done
  cleanup_executable_go_toolchain || status=1
  if [[ "$CORE_DIR_CREATED" == "1" ]]; then
    rmdir "$CORE_DIR" 2>/dev/null || true
  fi
  [[ -z "$TX_DIR" ]] || rm -rf "$TX_DIR"
  TX_ACTIVE=0
  set -e
  if [[ "$status" != "0" ]]; then
    printf '[%s] 警告：安装失败，且至少一个文件未能自动回滚。\n' "$SCRIPT_NAME" >&2
  else
    printf '[%s] 安装失败，已回滚本次二进制和数据文件替换。\n' "$SCRIPT_NAME" >&2
  fi
}

transaction_exit_handler() {
  local status=$?
  if [[ "$TX_ACTIVE" == "1" ]]; then
    rollback_transaction
  fi
  return "$status"
}

prepare_transaction_tmp_root() {
  local parent
  parent="$(dirname "$TRANSACTION_TMP_ROOT")"
  [[ -d "$parent" ]] || fatal "事务临时目录的父目录不存在：$parent"
  validate_trusted_directory_chain "$parent"
  if [[ -e "$TRANSACTION_TMP_ROOT" || -L "$TRANSACTION_TMP_ROOT" ]]; then
    [[ ! -L "$TRANSACTION_TMP_ROOT" && -d "$TRANSACTION_TMP_ROOT" ]] \
      || fatal "事务临时根必须是非符号链接目录：$TRANSACTION_TMP_ROOT"
  else
    mkdir -m 0700 -- "$TRANSACTION_TMP_ROOT" \
      || fatal "无法创建事务临时根：$TRANSACTION_TMP_ROOT"
  fi
  chmod 0700 -- "$TRANSACTION_TMP_ROOT" \
    || fatal "无法收紧事务临时根权限：$TRANSACTION_TMP_ROOT"
  validate_trusted_directory_chain "$TRANSACTION_TMP_ROOT"
}

begin_transaction() {
  prepare_transaction_tmp_root
  TX_DIR="$(mktemp -d "$TRANSACTION_TMP_ROOT/transaction.XXXXXX")" \
    || fatal "无法创建事务临时目录"
  TX_ACTIVE=1
  trap transaction_exit_handler EXIT
  chmod 0700 -- "$TX_DIR" || fatal "无法收紧事务临时目录权限：$TX_DIR"
  validate_trusted_directory_chain "$TX_DIR"
}

commit_transaction() {
  cleanup_executable_go_toolchain || fatal "无法删除临时 Go 工具链"
  rm -rf -- "$TX_DIR" || fatal "无法删除安装事务临时目录"
  TX_DIR=""
  TX_ACTIVE=0
  trap - EXIT
}

transactional_replace() {
  local source="$1" dest="$2" mode="$3" label="$4"
  local parent stage backup existed index
  validate_source_regular_file "$source" "$label"
  parent="$(dirname "$dest")"
  [[ -d "$parent" ]] || fatal "$label 目标父目录不存在：$parent"
  validate_trusted_directory_chain "$parent"
  existed=0
  backup=""
  if [[ -e "$dest" || -L "$dest" ]]; then
    validate_root_regular_file "$dest" "现有 $label"
    existed=1
    index="${#TX_DESTS[@]}"
    backup="$TX_DIR/backup-$index"
    cp -p "$dest" "$backup"
  fi
  # Register the rollback record before rename so every visible replacement is journaled.
  TX_DESTS+=("$dest")
  TX_BACKUPS+=("$backup")
  TX_EXISTED+=("$existed")
  stage="$(mktemp "$parent/.proxyscene-new.XXXXXX")" || fatal "无法为 $label 创建同目录暂存文件"
  if ! install -m "$mode" "$source" "$stage"; then
    rm -f "$stage"
    fatal "无法暂存 $label"
  fi
  if ! chown 0:0 "$stage"; then
    rm -f "$stage"
    fatal "无法设置 $label 的属主"
  fi
  if ! mv -f "$stage" "$dest"; then
    rm -f "$stage"
    fatal "无法原子替换 $label"
  fi
}

ensure_core_dir() {
  validate_core_dir
  if [[ ! -e "$CORE_DIR" ]]; then
    mkdir -m 0700 -- "$CORE_DIR"
    CORE_DIR_CREATED=1
  fi
  validate_trusted_directory_chain "$CORE_DIR"
  printf '由 proxyscene 安装器管理\n' > "$TX_DIR/managed-marker"
  transactional_replace "$TX_DIR/managed-marker" "$CORE_DIR/.managed-by-proxyscene" 600 "管理目录标记"
}

current_go_version() {
  local command_path="$1" output
  output="$("$command_path" version 2>/dev/null)" || return 1
  parse_go_version_output "$output"
}

prepare_executable_go_toolchain_dir() {
  [[ -d "$CORE_DIR" ]] || fatal "临时 Go 工具链要求核心目录已创建：$CORE_DIR"
  [[ -z "$GO_TOOLCHAIN_DIR" ]] || fatal "临时 Go 工具链目录已存在：$GO_TOOLCHAIN_DIR"
  validate_trusted_directory_chain "$CORE_DIR"
  GO_TOOLCHAIN_DIR="$(mktemp -d "$CORE_DIR/.proxyscene-go-toolchain.XXXXXX")" \
    || fatal "无法在核心目录创建临时 Go 工具链目录"
  chmod 0700 -- "$GO_TOOLCHAIN_DIR" || fatal "无法收紧临时 Go 工具链目录权限"
  validate_trusted_directory_chain "$GO_TOOLCHAIN_DIR"
}

install_go_toolchain() {
  local arch url archive checksum expected_size actual_size stage actual
  arch="$(arch_go)"
  url="https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz"
  archive="$TX_DIR/go.tar.gz"
  log "下载 Go：$url"
  fetch_https "$url" "$archive" 209715200 || fatal "下载 Go 失败"
  if [[ -n "$GO_TARBALL_SHA256" ]]; then
    checksum="$GO_TARBALL_SHA256"
    expected_size=""
  else
    [[ "$GO_VERSION" == "$DEFAULT_GO_VERSION" ]] \
      || fatal "非默认 GO_VERSION=$GO_VERSION 必须显式设置 GO_TARBALL_SHA256"
    checksum="$(go_tarball_sha256_for_arch "$arch")" \
      || fatal "缺少 Go $GO_VERSION 的固定 SHA256：$arch"
    expected_size="$(go_tarball_size_for_arch "$arch")" \
      || fatal "缺少 Go $GO_VERSION 的固定归档大小：$arch"
    actual_size="$(stat -c '%s' "$archive")"
    [[ "$actual_size" == "$expected_size" ]] \
      || fatal "Go 归档大小不匹配：期望 $expected_size，实际 $actual_size"
  fi
  verify_sha256_file "Go" "$archive" "$checksum"

  # /run is commonly mounted noexec. Keep downloads, backups and caches in the
  # fixed transaction root, but place the executable toolchain on CoreDir's
  # filesystem, which must also execute the managed Xray binary.
  prepare_executable_go_toolchain_dir
  stage="$GO_TOOLCHAIN_DIR"
  tar --no-same-owner --no-same-permissions -C "$stage" -xzf "$archive"
  validate_source_regular_file "$stage/go/bin/go" "Go 工具链"
  [[ -x "$stage/go/bin/go" ]] || fatal "Go 归档中的 go/bin/go 不可执行"
  GO_BIN="$stage/go/bin/go"
  actual="$(current_go_version "$GO_BIN")" || fatal "下载的 Go 无法报告稳定版本"
  go_version_matches_exactly "$actual" "$GO_VERSION" \
    || fatal "下载的 Go 版本不匹配：期望 $GO_VERSION，实际 $actual"
  log "临时 Go 已准备：版本 $actual；安装结束后自动删除"
}

ensure_go() {
  local existing="" have=""
  if need_cmd go; then
    existing="$(command -v go)"
    have="$(current_go_version "$existing" 2>/dev/null || true)"
  fi
  if [[ "$SKIP_GO_INSTALL" == "1" ]]; then
    [[ -n "$existing" && -n "$have" ]] || fatal "SKIP_GO_INSTALL=1 但找不到可用 go"
    go_version_meets_requirement "$have" "$GO_VERSION" \
      || fatal "现有 Go $have 低于显式要求 $GO_VERSION"
    GO_BIN="$existing"
    return 0
  fi
  if [[ -n "$have" && "$FORCE_GO_INSTALL" != "1" ]] && go_version_meets_requirement "$have" "$GO_VERSION"; then
    GO_BIN="$existing"
    log "使用已有 Go：版本 $have"
    return 0
  fi
  install_go_toolchain
}

xray_download_url_for_arch() {
  local release_arch="$1" asset_arch
  if [[ -n "$XRAY_ZIP_URL" ]]; then
    [[ "$XRAY_ZIP_URL" == https://* ]] || fatal "XRAY_ZIP_URL 必须是 https 地址"
    [[ -n "$XRAY_ZIP_SHA256" ]] || fatal "自定义 XRAY_ZIP_URL 必须同时设置 XRAY_ZIP_SHA256"
    printf '%s\n' "$XRAY_ZIP_URL"
    return 0
  fi
  asset_arch="$(xray_asset_arch_for_release_arch "$release_arch")" || fatal "不支持的 Xray 架构：$release_arch"
  printf '%s/Xray-linux-%s.zip\n' "${XRAY_RELEASE_BASE%/}" "$asset_arch"
}

xray_expected_sha256_for_arch() {
  local release_arch="$1"
  if [[ -n "$XRAY_ZIP_URL" ]]; then
    normalize_sha256 "$XRAY_ZIP_SHA256" || fatal "XRAY_ZIP_SHA256 格式无效"
  else
    xray_sha256_for_release_arch "$release_arch" || fatal "缺少 Xray 固定 SHA256：$release_arch"
  fi
}

xray_marker_for_source() {
  local source="$1" value="$2" digest
  case "$source" in
    official)
      validate_release_tag "$value" || return 1
      printf '%s\n' "$value"
      ;;
    custom|existing)
      digest="$(normalize_sha256 "$value")" || return 1
      printf '%s-sha256:%s\n' "$source" "$digest"
      ;;
    *)
      return 1
      ;;
  esac
}

install_xray() {
  local release_arch url expected unpack file marker marker_source digest expected_size
  release_arch="$(arch_release)" || fatal "当前架构没有受支持的 Xray 产物：$(uname -m)"
  if [[ "$SKIP_XRAY_INSTALL" == "1" ]]; then
    validate_root_regular_file "$CORE_DIR/xray" "现有 Xray"
    [[ -x "$CORE_DIR/xray" ]] || fatal "现有 Xray 不可执行：$CORE_DIR/xray"
    validate_elf_arch "$CORE_DIR/xray" "$release_arch" "现有 Xray"
    digest="$(sha256_file "$CORE_DIR/xray")" || fatal "无法计算现有 Xray SHA256"
    marker="$(xray_marker_for_source existing "$digest")" || fatal "无法生成现有 Xray 来源标记"
    printf '%s\n' "$marker" > "$TX_DIR/xray-version.txt"
    transactional_replace "$TX_DIR/xray-version.txt" "$CORE_DIR/xray-version.txt" 600 "Xray 来源标记"
    log "按 SKIP_XRAY_INSTALL=1 保留现有 Xray；版本未知，已记录二进制 SHA256"
    return 0
  fi

  url="$(xray_download_url_for_arch "$release_arch")"
  expected="$(xray_expected_sha256_for_arch "$release_arch")"
  marker_source=official
  if [[ -n "$XRAY_ZIP_URL" ]]; then
    marker_source=custom
    log "下载自定义 Xray 归档（架构 $release_arch，版本未知）"
  else
    log "下载固定 Xray ${DEFAULT_XRAY_VERSION}（架构 $release_arch）"
  fi
  fetch_https "$url" "$TX_DIR/xray.zip" 104857600 || fatal "下载 Xray 失败"
  verify_sha256_file "Xray" "$TX_DIR/xray.zip" "$expected"
  if [[ "$marker_source" == "official" ]]; then
    expected_size="$(xray_archive_size_for_release_arch "$release_arch")" || fatal "缺少 Xray 归档精确大小"
    [[ "$(stat -c '%s' "$TX_DIR/xray.zip")" == "$expected_size" ]] || fatal "Xray 归档大小与固定官方版本不匹配"
  fi
  unpack="$TX_DIR/xray-unpack"
  mkdir "$unpack"
  unzip -oq "$TX_DIR/xray.zip" -d "$unpack"
  for file in xray LICENSE; do
    validate_source_regular_file "$unpack/$file" "Xray 归档中的 $file"
  done
  validate_elf_arch "$unpack/xray" "$release_arch" "Xray"

  transactional_replace "$unpack/xray" "$CORE_DIR/xray" 700 "Xray"
  transactional_replace "$unpack/LICENSE" "$CORE_DIR/LICENSE-Xray" 600 "Xray 许可证"
  if [[ "$marker_source" == "official" ]]; then
    marker="$(xray_marker_for_source official "$DEFAULT_XRAY_VERSION")" || fatal "无法生成官方 Xray 版本标记"
  else
    marker="$(xray_marker_for_source custom "$expected")" || fatal "无法生成自定义 Xray 来源标记"
  fi
  printf '%s\n' "$marker" > "$TX_DIR/xray-version.txt"
  transactional_replace "$TX_DIR/xray-version.txt" "$CORE_DIR/xray-version.txt" 600 "Xray 来源标记"
}

validate_release_inputs() {
  [[ "$MANAGER_REPO" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || fatal "PROXYSCENE_REPO 格式无效：$MANAGER_REPO"
  [[ "$MANAGER_VERSION" == "latest" ]] || validate_release_tag "$MANAGER_VERSION" || fatal "PROXYSCENE_VERSION 必须是 v 开头的安全 tag：$MANAGER_VERSION"
  if [[ -n "$MANAGER_BASE_URL" ]]; then
    [[ "$MANAGER_BASE_URL" == https://* && "$MANAGER_BASE_URL" != *"?"* && "$MANAGER_BASE_URL" != *"#"* ]] \
      || fatal "PROXYSCENE_BASE_URL 必须是无查询参数的 https 地址"
    [[ "$MANAGER_VERSION" != "latest" ]] || fatal "自定义 PROXYSCENE_BASE_URL 时必须显式指定 PROXYSCENE_VERSION"
    url_has_userinfo "$MANAGER_BASE_URL" && fatal "PROXYSCENE_BASE_URL 不能包含 URL userinfo 凭据"
  fi
}

resolve_manager_version() {
  local metadata metadata_url tag
  metadata="$TX_DIR/release-metadata.json"
  if [[ "$MANAGER_VERSION" == "latest" ]]; then
    log "解析 GitHub latest 为明确且不可变的 tag"
    metadata_url="https://api.github.com/repos/${MANAGER_REPO}/releases/latest"
  else
    metadata_url="https://api.github.com/repos/${MANAGER_REPO}/releases/tags/${MANAGER_VERSION}"
  fi
  fetch_https "$metadata_url" "$metadata" 1048576 || fatal "下载 GitHub Release 元数据失败"
  tag="$(verified_release_tag_json "$MANAGER_VERSION" < "$metadata")" \
    || fatal "GitHub Release 元数据未同时满足安全 tag 精确匹配和 immutable=true"
  RESOLVED_MANAGER_VERSION="$tag"
  log "目标 Release 已固定并确认 immutable：$RESOLVED_MANAGER_VERSION"
}

release_base_url() {
  if [[ -n "$MANAGER_BASE_URL" ]]; then
    printf '%s\n' "${MANAGER_BASE_URL%/}"
  else
    printf 'https://github.com/%s/releases/download/%s\n' "$MANAGER_REPO" "$RESOLVED_MANAGER_VERSION"
  fi
}

release_checksums_url() {
  printf 'https://github.com/%s/releases/download/%s/checksums.txt\n' \
    "$MANAGER_REPO" "$RESOLVED_MANAGER_VERSION"
}

install_manager_prebuilt() {
  local arch base checksums_url asset manifest expected unpack
  arch="$(arch_release)" || fatal "当前架构没有受支持的预编译管理程序：$(uname -m)"
  validate_release_inputs
  resolve_manager_version
  base="$(release_base_url)"
  checksums_url="$(release_checksums_url)"
  asset="proxyscene_linux_${arch}.tar.gz"
  manifest="$TX_DIR/release-checksums.txt"

  log "从官方不可变 Release 下载 $RESOLVED_MANAGER_VERSION 的 checksums.txt"
  fetch_https "$checksums_url" "$manifest" 1048576 || fatal "下载 Release checksums.txt 失败"
  expected="$(manifest_sha_for_asset "$manifest" "$asset")" || fatal "checksums.txt 中必须且只能包含一个 $asset"
  log "下载预编译管理程序：$RESOLVED_MANAGER_VERSION / $asset"
  fetch_https "$base/$asset" "$TX_DIR/$asset" 104857600 || fatal "下载预编译管理程序失败"
  verify_sha256_file "管理程序归档" "$TX_DIR/$asset" "$expected"

  unpack="$TX_DIR/manager-unpack"
  mkdir "$unpack"
  tar -xzf "$TX_DIR/$asset" -C "$unpack" \
    proxyscene LICENSE NOTICE SOURCE-Xray THIRD_PARTY_LICENSES THIRD_PARTY_LICENSES-Xray
  validate_source_regular_file "$unpack/proxyscene" "管理程序"
  validate_source_regular_file "$unpack/LICENSE" "项目许可证"
  validate_source_regular_file "$unpack/NOTICE" "第三方声明"
  validate_source_regular_file "$unpack/SOURCE-Xray" "Xray 对应源码说明"
  validate_source_regular_file "$unpack/THIRD_PARTY_LICENSES" "第三方许可证"
  validate_source_regular_file "$unpack/THIRD_PARTY_LICENSES-Xray" "Xray 第三方许可证"
  validate_elf_arch "$unpack/proxyscene" "$arch" "管理程序"
  transactional_replace "$unpack/proxyscene" "$INSTALL_BIN" 755 "管理程序"
  transactional_replace "$unpack/LICENSE" "$CORE_DIR/LICENSE-proxyscene" 600 "项目许可证"
  transactional_replace "$unpack/NOTICE" "$CORE_DIR/NOTICE" 600 "第三方声明"
  transactional_replace "$unpack/SOURCE-Xray" "$CORE_DIR/SOURCE-Xray" 600 "Xray 对应源码说明"
  transactional_replace "$unpack/THIRD_PARTY_LICENSES" "$CORE_DIR/THIRD_PARTY_LICENSES" 600 "第三方许可证"
  transactional_replace "$unpack/THIRD_PARTY_LICENSES-Xray" "$CORE_DIR/THIRD_PARTY_LICENSES-Xray" 600 "Xray 第三方许可证"
  log "已暂存管理程序 $RESOLVED_MANAGER_VERSION，全部依赖验证后统一提交"
}

build_manager() {
  local dir out arch
  dir="$(repo_dir)" || fatal "无法确定源码目录"
  [[ -f "$dir/go.mod" ]] || fatal "未找到 go.mod：$dir/go.mod"
  out="$TX_DIR/proxyscene-source-build"
  arch="$(arch_release)" || fatal "不支持的管理程序架构：$(uname -m)"
  log "编译本地 Go 管理程序"
  run_quiet "编译 Go 管理程序" env \
    CGO_ENABLED=0 GOENV=off GOFLAGS= GOTOOLCHAIN=local GOWORK=off \
    GOCACHE="$TX_DIR/go-cache" GOMODCACHE="$TX_DIR/go-mod-cache" GOPATH="$TX_DIR/go-path" \
    "$GO_BIN" build -C "$dir" \
    -trimpath -buildvcs=false -ldflags "-s -w -buildid=" -o "$out" ./cmd/proxyscene
  validate_elf_arch "$out" "$arch" "本地构建管理程序"
  transactional_replace "$out" "$INSTALL_BIN" 755 "管理程序"
  transactional_replace "$dir/LICENSE" "$CORE_DIR/LICENSE-proxyscene" 600 "项目许可证"
  transactional_replace "$dir/NOTICE" "$CORE_DIR/NOTICE" 600 "第三方声明"
  transactional_replace "$dir/SOURCE-Xray" "$CORE_DIR/SOURCE-Xray" 600 "Xray 对应源码说明"
  transactional_replace "$dir/THIRD_PARTY_LICENSES" "$CORE_DIR/THIRD_PARTY_LICENSES" 600 "第三方许可证"
  transactional_replace "$dir/THIRD_PARTY_LICENSES-Xray" "$CORE_DIR/THIRD_PARTY_LICENSES-Xray" 600 "Xray 第三方许可证"
}

install_manager() {
  if [[ "$BUILD_FROM_SOURCE" == "1" ]]; then
    ensure_go
    build_manager
    return 0
  fi
  install_manager_prebuilt
}

verify_bundle_manifest() {
  local bdir="$1" manifest="$1/bundle-manifest.sha256" file expected count
  local -a required=(LICENSE LICENSE-Xray NOTICE SOURCE-Xray THIRD_PARTY_LICENSES THIRD_PARTY_LICENSES-Xray install.sh proxyscene xray xray-version.txt)
  validate_trusted_directory_chain "$bdir"
  validate_root_regular_file "$manifest" "bundle manifest"
  count="$(awk 'NF {count++} END {print count+0}' "$manifest")"
  [[ "$count" == "${#required[@]}" ]] || fatal "bundle manifest 条目数错误：$count"
  for file in "${required[@]}"; do
    validate_root_regular_file "$bdir/$file" "bundle 组件 $file"
    expected="$(manifest_sha_for_asset "$manifest" "$file")" || fatal "bundle manifest 缺少或重复：$file"
    verify_sha256_file "bundle 组件 $file" "$bdir/$file" "$expected"
  done
  [[ "$(tr -d '\r\n' < "$bdir/xray-version.txt")" == "$DEFAULT_XRAY_VERSION" ]] \
    || fatal "bundle Xray 版本与安装器固定版本不一致"
}

install_offline_local() {
  local bdir="$1" arch
  arch="$(arch_release)" || fatal "当前架构没有受支持的离线包：$(uname -m)"
  validate_elf_arch "$bdir/proxyscene" "$arch" "bundle 管理程序"
  validate_elf_arch "$bdir/xray" "$arch" "bundle Xray"
  log "bundle manifest 校验通过，开始离线替换"
  transactional_replace "$bdir/proxyscene" "$INSTALL_BIN" 755 "管理程序"
  transactional_replace "$bdir/xray" "$CORE_DIR/xray" 700 "Xray"
  transactional_replace "$bdir/LICENSE-Xray" "$CORE_DIR/LICENSE-Xray" 600 "Xray 许可证"
  transactional_replace "$bdir/LICENSE" "$CORE_DIR/LICENSE-proxyscene" 600 "项目许可证"
  transactional_replace "$bdir/NOTICE" "$CORE_DIR/NOTICE" 600 "第三方声明"
  transactional_replace "$bdir/SOURCE-Xray" "$CORE_DIR/SOURCE-Xray" 600 "Xray 对应源码说明"
  transactional_replace "$bdir/THIRD_PARTY_LICENSES" "$CORE_DIR/THIRD_PARTY_LICENSES" 600 "第三方许可证"
  transactional_replace "$bdir/THIRD_PARTY_LICENSES-Xray" "$CORE_DIR/THIRD_PARTY_LICENSES-Xray" 600 "Xray 第三方许可证"
  transactional_replace "$bdir/xray-version.txt" "$CORE_DIR/xray-version.txt" 600 "Xray 版本标记"
}

init_manager() {
  if [[ "$SKIP_MANAGER_INIT" == "1" ]]; then
    log "跳过管理服务初始化"
    return 0
  fi
  if ! PROXYSCENE_MANAGER_DIR="$CORE_DIR" PROXYSCENE_SWITCH_BIN="$INSTALL_BIN" \
    PROXYSCENE_SYSTEMD_SERVICE_NAME="$SYSTEMD_SERVICE" \
    PROXYSCENE_BOOT_RESTORE_SERVICE_NAME="$RESTORE_SERVICE" \
    PROXYSCENE_INHERITED_INSTALL_LOCK_FD="$INSTALL_LOCK_FD" \
    PROXYSCENE_INHERITED_HOST_LOCK_FD="$HOST_LOCK_FD" \
    PROXYSCENE_INHERITED_STORE_LOCK_FD="$STORE_LOCK_FD" \
    "$INSTALL_BIN" install --skip-node; then
    return 1
  fi
}

commit_files_then_init() {
  commit_transaction
  # The manager validates and reuses these inherited flock file descriptions.
  # Keep all three locks held until initialization has committed the units and
  # runtime state; releasing here would leave a file-commit/init race window.
  if ! init_manager; then
    release_runtime_locks
    fatal "管理器初始化失败；已验证的程序和数据文件已保留，避免 systemd 指向被回滚的文件。修复原因后运行 sudo proxyscene install --skip-node 重试"
  fi
  release_runtime_locks
}

main() {
  parse_args "$@"
  if [[ "$SHOW_HELP" == "1" ]]; then
    usage
    return 0
  fi
  require_root
  validate_common_inputs
  validate_core_dir
  validate_managed_core_dir
  validate_install_bin

  if [[ "$(install_mode_for_flag "$OFFLINE_REQUESTED")" == "offline" ]]; then
    local bdir
    bdir="$(bundle_dir)" || fatal "--offline 必须从解压后的 bundle 内执行"
    verify_bundle_manifest "$bdir"
    acquire_install_lock
    verify_expected_manager
    acquire_host_runtime_lock
    acquire_store_runtime_lock
    validate_install_bin
    validate_host_ownership_for_install
    begin_transaction
    ensure_core_dir
    install_offline_local "$bdir"
    stage_installation_ownership
    commit_files_then_init
    log "离线安装完成。请运行 sudo proxyscene install 交互录入节点。"
    return 0
  fi

  validate_online_inputs
  acquire_install_lock
  install_packages
  acquire_host_runtime_lock
  acquire_store_runtime_lock
  validate_install_bin
  validate_host_ownership_for_install
  begin_transaction
  ensure_core_dir
  install_xray
  install_manager
  stage_installation_ownership
  commit_files_then_init
  log "安装完成。请运行 sudo proxyscene install 交互录入节点。"
}

# Sourcing this file is always inert. Tests additionally set PROXYSCENE_INSTALL_TESTING=1.
if [[ "${BASH_SOURCE[0]:-}" == "$0" && "${PROXYSCENE_INSTALL_TESTING:-0}" != "1" ]]; then
  main "$@"
fi
