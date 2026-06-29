#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_NAME="proxyscene 安装器"
DEFAULT_GO_VERSION="1.22.12"
DEFAULT_CORE_DIR="/opt/proxyscene"
DEFAULT_INSTALL_BIN="/usr/local/bin/proxyscene"
DEFAULT_XRAY_DOWNLOAD_SOURCE="official"
DEFAULT_XRAY_ZIP_URL=""
DEFAULT_XRAY_XXV_ZIP_URL="https://xxv.cc/7c9fxLN4nm4BFU8fjD.zip"
DEFAULT_XRAY_GITHUB_RELEASE_BASE="https://github.com/XTLS/Xray-core/releases/latest/download"
# 本项目 Release 的 minisign 公钥；离线安装默认用它验签离线整合包。
DEFAULT_MANAGER_MINISIGN_PUBKEY="RWSwCDZeUKUXxnGQfkQwePkJyg1uKh7LcKXgia4Lto4MeC6lKStdotYb"

GO_VERSION="${GO_VERSION:-$DEFAULT_GO_VERSION}"
GO_TARBALL_SHA256="${GO_TARBALL_SHA256:-}"
GO_INSTALL_DIR="${GO_INSTALL_DIR:-/usr/local}"
GO_ROOT="$GO_INSTALL_DIR/go"
CORE_DIR="${PROXYSCENE_MANAGER_DIR:-$DEFAULT_CORE_DIR}"
INSTALL_BIN="${PROXYSCENE_SWITCH_BIN:-$DEFAULT_INSTALL_BIN}"
XRAY_ZIP_URL="${XRAY_ZIP_URL:-$DEFAULT_XRAY_ZIP_URL}"
XRAY_XXV_ZIP_URL="${XRAY_XXV_ZIP_URL:-$DEFAULT_XRAY_XXV_ZIP_URL}"
XRAY_GITHUB_RELEASE_BASE="${XRAY_GITHUB_RELEASE_BASE:-$DEFAULT_XRAY_GITHUB_RELEASE_BASE}"
XRAY_DOWNLOAD_SOURCE="${XRAY_DOWNLOAD_SOURCE:-$DEFAULT_XRAY_DOWNLOAD_SOURCE}"
XRAY_ZIP_SHA256="${XRAY_ZIP_SHA256:-}"
SKIP_GO_INSTALL="${SKIP_GO_INSTALL:-0}"
SKIP_XRAY_INSTALL="${SKIP_XRAY_INSTALL:-0}"
SKIP_MANAGER_INIT="${SKIP_MANAGER_INIT:-0}"
FORCE_GO_INSTALL="${FORCE_GO_INSTALL:-0}"
DEFAULT_REPO="longlannet/proxyscene"
MANAGER_REPO="${PROXYSCENE_REPO:-$DEFAULT_REPO}"
MANAGER_VERSION="${PROXYSCENE_VERSION:-latest}"
MANAGER_BASE_URL="${PROXYSCENE_BASE_URL:-}"
# 默认用内置发布公钥；显式设置 PROXYSCENE_MINISIGN_PUBKEY 时则强制要求验签成功。
MANAGER_MINISIGN_PUBKEY="${PROXYSCENE_MINISIGN_PUBKEY:-$DEFAULT_MANAGER_MINISIGN_PUBKEY}"
MANAGER_MINISIGN_REQUIRED=0
[[ -n "${PROXYSCENE_MINISIGN_PUBKEY:-}" ]] && MANAGER_MINISIGN_REQUIRED=1
BUILD_FROM_SOURCE="${PROXYSCENE_BUILD_FROM_SOURCE:-0}"
FORCE_OFFLINE_LOCAL=0
NODE_URL=""

log() { printf '[%s] %s\n' "$SCRIPT_NAME" "$*"; }
fatal() { printf '[%s] 错误：%s\n' "$SCRIPT_NAME" "$*" >&2; exit 1; }
run_quiet() {
  local desc="$1"
  shift
  if ! "$@" >/dev/null 2>&1; then
    fatal "${desc}失败"
  fi
}

usage() {
  cat <<'EOF'
用法：
  sudo bash ./install.sh [节点链接]

离线安装（适合屏蔽 GitHub 的网络环境，全程不联网、不需要 Go）：
  从 Release 下载自包含整合包，解压后在其目录内运行本脚本即可：
    tar xzf proxyscene_bundle_linux_<arch>.tar.gz
    cd proxyscene_bundle_linux_<arch>
    sudo ./install.sh [节点链接]
  脚本检测到同目录的 proxyscene/xray 二进制即走离线本地安装（也可显式加 --offline）。
  自包含包无法验证它自身；如需校验，请在解压前用随包的 .minisig + 公钥验证整个 tar：
    minisign -Vm <包>.tar.gz -x <包>.tar.gz.minisig -P <公钥>

管理程序获取方式（默认优先下载预编译二进制，目标机无需 Go；失败时回退源码编译）：
  PROXYSCENE_VERSION=latest                    要下载的预编译版本，如 v0.4.0
  PROXYSCENE_REPO=longlannet/proxyscene     预编译二进制所在的 GitHub 仓库 owner/name
  PROXYSCENE_BASE_URL=https://mirror/dl         自定义预编译下载基址（必须 https），优先级最高
  PROXYSCENE_MINISIGN_PUBKEY=RWxxxx             用 minisign 校验签名（联机校验 checksums.txt，离线校验整合包）
  PROXYSCENE_BUILD_FROM_SOURCE=1                跳过预编译下载，强制本地源码编译

常用环境变量：
  GO_VERSION=1.22.12                         缺少 Go 或版本过低时准备的 Go 版本（仅源码编译用到）
  GO_TARBALL_SHA256=...                      Go 安装包 SHA256；留空时从 go.dev 官方 .sha256 文件获取并校验
  GO_INSTALL_DIR=/usr/local                   Go 安装父目录
  SKIP_GO_INSTALL=1                           不安装 Go，要求系统已有 go 命令
  FORCE_GO_INSTALL=1                          即使已有 Go 版本可用，也重新准备指定版本
  PROXYSCENE_MANAGER_DIR=/opt/proxyscene
                                             管理器核心目录
  PROXYSCENE_SWITCH_BIN=/usr/local/bin/proxyscene
                                             管理程序安装路径
  XRAY_DOWNLOAD_SOURCE=official                Xray 下载源，可选 official 或 xxv
  XRAY_ZIP_URL=https://example.com/xray.zip    自定义 Xray zip 下载地址，优先级高于预设下载源
  XRAY_ZIP_SHA256=...                          Xray zip SHA256；使用自定义或 xxv 源时建议设置
  SKIP_XRAY_INSTALL=1                         不安装 Xray，要求核心目录已有可执行 xray
  SKIP_MANAGER_INIT=1                         只安装依赖和程序，不执行管理器初始化
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    -h|--help)
      usage
      exit 0
      ;;
    --offline)
      FORCE_OFFLINE_LOCAL=1
      ;;
    -*)
      fatal "未知选项：$1"
      ;;
    *)
      [[ -z "$NODE_URL" ]] || fatal "只接受一个可选节点链接参数"
      NODE_URL="$1"
      ;;
  esac
  shift
done

require_root() {
  if [[ "$(id -u)" != "0" ]]; then
    fatal "请用 root 运行，例如：sudo bash ./install.sh"
  fi
}

repo_dir() {
  local source="${BASH_SOURCE[0]}"
  local dir
  dir="$(cd "$(dirname "$source")" && pwd -P)"
  printf '%s\n' "$dir"
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1
}

has_package() {
  local pkg="$1"
  if need_cmd dpkg-query; then
    dpkg-query -W -f='${Status}' "$pkg" 2>/dev/null | grep -q 'install ok installed'
  elif need_cmd rpm; then
    rpm -q "$pkg" >/dev/null 2>&1
  elif need_cmd apk; then
    apk info -e "$pkg" >/dev/null 2>&1
  elif need_cmd pacman; then
    pacman -Q "$pkg" >/dev/null 2>&1
  else
    return 1
  fi
}

install_packages() {
  local packages=("curl" "ca-certificates" "tar" "unzip")
  local commands=("curl" "" "tar" "unzip")
  local missing=()
  local i pkg cmd
  for i in "${!packages[@]}"; do
    pkg="${packages[$i]}"
    cmd="${commands[$i]}"
    if [[ -n "$cmd" ]]; then
      need_cmd "$cmd" || missing+=("$pkg")
    elif ! has_package "$pkg"; then
      missing+=("$pkg")
    fi
  done
  if [[ ${#missing[@]} -eq 0 ]]; then
    return 0
  fi

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

arch_go() {
  case "$(uname -m)" in
    x86_64|amd64) printf 'amd64\n' ;;
    aarch64|arm64) printf 'arm64\n' ;;
    i386|i686) printf '386\n' ;;
    armv6l) printf 'armv6l\n' ;;
    armv7l|armhf) printf 'armv6l\n' ;;
    *) fatal "不支持的 Go 架构：$(uname -m)" ;;
  esac
}

arch_xray() {
  case "$(uname -m)" in
    x86_64|amd64) printf '64\n' ;;
    aarch64|arm64) printf 'arm64-v8a\n' ;;
    i386|i686) printf '32\n' ;;
    armv7l|armhf) printf 'arm32-v7a\n' ;;
    armv6l) printf 'arm32-v6\n' ;;
    *) fatal "不支持的 Xray 架构：$(uname -m)" ;;
  esac
}

version_ge() {
  local have="$1"
  local want="$2"
  local smallest
  smallest="$(printf '%s\n%s\n' "$want" "$have" | sort -V | head -n1)"
  [[ "$smallest" == "$want" ]]
}

current_go_version() {
  if ! need_cmd go; then
    return 1
  fi
  go version | awk '{print $3}' | sed 's/^go//'
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

is_sha256_hex() {
  [[ "$1" =~ ^[0-9A-Fa-f]{64}$ ]]
}

verify_sha256_file() {
  local label="$1"
  local file="$2"
  local expected="$3"
  local actual
  is_sha256_hex "$expected" || fatal "${label} SHA256 格式无效：$expected"
  actual="$(sha256_file "$file")"
  [[ "$actual" == "$expected" ]] || fatal "${label} SHA256 不匹配：期望 $expected，实际 $actual"
}

ensure_go() {
  if [[ "$SKIP_GO_INSTALL" == "1" ]]; then
    need_cmd go || fatal "SKIP_GO_INSTALL=1 但找不到 go 命令"
    log "使用已有 Go：版本 $(current_go_version)"
    return 0
  fi

  local have=""
  if have="$(current_go_version 2>/dev/null)" && [[ -n "$have" && "$FORCE_GO_INSTALL" != "1" ]]; then
    if version_ge "$have" "1.22"; then
      log "使用已有 Go：版本 $have"
      return 0
    fi
    log "已有 Go 版本过低：$have，将安装 Go $GO_VERSION"
  fi

  local arch url tmp archive checksum checksum_url checksum_text
  arch="$(arch_go)"
  url="https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz"
  tmp="$(mktemp -d)"
  archive="$tmp/go.tar.gz"
  trap 'rm -rf "$tmp"' EXIT

  log "下载 Go：$url"
  run_quiet "下载 Go" curl -fL --connect-timeout 15 --retry 3 --retry-delay 2 -o "$archive" "$url"
  if [[ -n "$GO_TARBALL_SHA256" ]]; then
    checksum="$GO_TARBALL_SHA256"
  else
    checksum_url="${url}.sha256"
    log "下载 Go SHA256：$checksum_url"
    if ! checksum_text="$(curl -fL --connect-timeout 15 --retry 3 --retry-delay 2 "$checksum_url" 2>/dev/null)"; then
      fatal "下载 Go SHA256 失败"
    fi
    checksum="$(printf '%s\n' "$checksum_text" | awk 'NF {print $1; exit}')"
  fi
  verify_sha256_file "Go" "$archive" "$checksum"
  rm -rf "$GO_ROOT"
  run_quiet "解压 Go" tar -C "$GO_INSTALL_DIR" -xzf "$archive"
  mkdir -p /usr/local/bin
  ln -sf "$GO_ROOT/bin/go" /usr/local/bin/go
  ln -sf "$GO_ROOT/bin/gofmt" /usr/local/bin/gofmt
  log "Go 安装完成：版本 $(/usr/local/bin/go version | awk '{print $3}' | sed 's/^go//')"
  rm -rf "$tmp"
  trap - EXIT
}

xray_download_url() {
  if [[ -n "$XRAY_ZIP_URL" ]]; then
    printf '%s\n' "$XRAY_ZIP_URL"
    return 0
  fi
  case "${XRAY_DOWNLOAD_SOURCE,,}" in
    official|github|xtls)
      printf '%s/Xray-linux-%s.zip\n' "$XRAY_GITHUB_RELEASE_BASE" "$(arch_xray)"
      ;;
    xxv|xxv.cc|mirror)
      printf '%s\n' "$XRAY_XXV_ZIP_URL"
      ;;
    *)
      fatal "未知 Xray 下载源：$XRAY_DOWNLOAD_SOURCE，可选 official 或 xxv"
      ;;
  esac
}

# xray_expected_sha256 解析期望的 Xray zip SHA256：
#   1) 显式设置的 XRAY_ZIP_SHA256 优先；
#   2) 否则在使用官方源（未自定义 XRAY_ZIP_URL）时，尝试拉取官方 .dgst 校验文件并提取 SHA256；
#   3) 其余情况返回空（由调用方决定是否仅警告）。
xray_expected_sha256() {
  local url="$1"
  if [[ -n "$XRAY_ZIP_SHA256" ]]; then
    printf '%s\n' "$XRAY_ZIP_SHA256"
    return 0
  fi
  if [[ -n "$XRAY_ZIP_URL" ]]; then
    return 0
  fi
  case "${XRAY_DOWNLOAD_SOURCE,,}" in
    official|github|xtls)
      local dgst_text checksum
      if dgst_text="$(curl -fL --connect-timeout 15 --retry 3 --retry-delay 2 "${url}.dgst" 2>/dev/null)"; then
        # XTLS 官方 .dgst 把 SHA-256 标注为 `SHA2-256= <hex>`（不是 `SHA256=`）。
        # 用 sha2-?256 精确匹配 SHA2-256 行（不会误中 SHA2-512 / SHA3-256），再取 64 位十六进制。
        checksum="$(printf '%s\n' "$dgst_text" | grep -iE 'sha2-?256' | grep -oiE '[0-9a-f]{64}' | head -n1)"
        if is_sha256_hex "$checksum"; then
          printf '%s\n' "$checksum"
        fi
      fi
      ;;
  esac
  return 0
}

validate_core_dir() {
  if [[ -z "$CORE_DIR" || "$CORE_DIR" != /* ]]; then
    fatal "PROXYSCENE_MANAGER_DIR 必须是绝对路径：$CORE_DIR"
  fi
  if [[ "$CORE_DIR" == *[$' \t\r\n']* || "$CORE_DIR" == *//* ]]; then
    fatal "PROXYSCENE_MANAGER_DIR 不能包含空白字符或重复斜杠：$CORE_DIR"
  fi
  if [[ "$CORE_DIR" == *'/../'* || "$CORE_DIR" == */.. || "$CORE_DIR" == *'/./'* || "$CORE_DIR" == */. ]]; then
    fatal "PROXYSCENE_MANAGER_DIR 必须使用规范化路径：$CORE_DIR"
  fi
  case "$CORE_DIR" in
    /|/bin|/boot|/dev|/etc|/home|/lib|/lib64|/media|/mnt|/opt|/proc|/root|/run|/sbin|/srv|/sys|/tmp|/usr|/var|/var/lib|/var/opt|/var/tmp)
      fatal "PROXYSCENE_MANAGER_DIR 不能使用系统目录本身：$CORE_DIR"
      ;;
  esac
  case "$CORE_DIR/" in
    /etc/*|/usr/*|/bin/*|/sbin/*|/lib/*|/lib64/*|/proc/*|/sys/*|/dev/*|/run/*|/home/*|/root/*|/tmp/*|/var/tmp/*)
      fatal "PROXYSCENE_MANAGER_DIR 不能位于敏感系统目录下：$CORE_DIR"
      ;;
  esac
  case "$CORE_DIR/" in
    /opt/*|/var/lib/*|/var/opt/*) ;;
    *) fatal "PROXYSCENE_MANAGER_DIR 必须位于 /opt、/var/lib 或 /var/opt 下的专用目录：$CORE_DIR" ;;
  esac
  if [[ -L "$CORE_DIR" ]]; then
    fatal "PROXYSCENE_MANAGER_DIR 不能是符号链接：$CORE_DIR"
  fi
  if [[ -e "$CORE_DIR" && ! -d "$CORE_DIR" ]]; then
    fatal "PROXYSCENE_MANAGER_DIR 已存在但不是目录：$CORE_DIR"
  fi
}

ensure_core_dir() {
  validate_core_dir
  local created=0
  if [[ ! -e "$CORE_DIR" ]]; then
    mkdir -p "$CORE_DIR"
    created=1
  fi
  if [[ -L "$CORE_DIR" || ! -d "$CORE_DIR" ]]; then
    fatal "PROXYSCENE_MANAGER_DIR 不可用：$CORE_DIR"
  fi
  if [[ "$created" == "1" ]]; then
    chmod 700 "$CORE_DIR"
  fi
  printf '由 proxyscene 安装器管理\n' > "$CORE_DIR/.managed-by-proxyscene"
  chmod 600 "$CORE_DIR/.managed-by-proxyscene"
}

install_xray() {
  ensure_core_dir

  if [[ "$SKIP_XRAY_INSTALL" == "1" ]]; then
    [[ -x "$CORE_DIR/xray" ]] || fatal "SKIP_XRAY_INSTALL=1 但 $CORE_DIR/xray 不存在或不可执行"
    log "跳过 Xray 安装，使用已有文件：$CORE_DIR/xray"
    return 0
  fi

  if [[ -x "$CORE_DIR/xray" ]]; then
    log "Xray 已存在：$CORE_DIR/xray"
    return 0
  fi

  local tmp zip url checksum
  tmp="$(mktemp -d)"
  zip="$tmp/xray.zip"
  url="$(xray_download_url)"
  trap 'rm -rf "$tmp"' EXIT

  log "下载 Xray：$url"
  run_quiet "下载 Xray" curl -fL --connect-timeout 15 --retry 3 --retry-delay 2 -o "$zip" "$url"

  local expected
  expected="$(xray_expected_sha256 "$url")"
  if [[ -n "$expected" ]]; then
    verify_sha256_file "Xray" "$zip" "$expected"
    log "Xray SHA256 校验通过"
  else
    log "警告：未校验 Xray 完整性（未提供 XRAY_ZIP_SHA256，且无法自动获取官方校验和）；建议设置 XRAY_ZIP_SHA256"
  fi

  run_quiet "解压 Xray" unzip -oq "$zip" -d "$tmp/xray"
  if [[ -f "$tmp/xray/xray" ]]; then
    install -m 700 "$tmp/xray/xray" "$CORE_DIR/xray"
  fi
  local name
  for name in geoip.dat geosite.dat; do
    if [[ -f "$tmp/xray/$name" ]]; then
      install -m 600 "$tmp/xray/$name" "$CORE_DIR/$name"
    fi
  done
  [[ -x "$CORE_DIR/xray" ]] || fatal "Xray 解压后未找到可执行文件"
  log "Xray 安装完成：$CORE_DIR/xray"
  rm -rf "$tmp"
  trap - EXIT
}

arch_release() {
  case "$(uname -m)" in
    x86_64|amd64) printf 'amd64\n' ;;
    aarch64|arm64) printf 'arm64\n' ;;
    i386|i686) printf '386\n' ;;
    armv7l|armhf) printf 'armv7\n' ;;
    *) return 1 ;;
  esac
}

validate_release_inputs() {
  case "$MANAGER_REPO" in
    ""|/*|*/*/*|*" "*|*..*) fatal "PROXYSCENE_REPO 格式无效，应为 owner/name：$MANAGER_REPO" ;;
  esac
  case "$MANAGER_VERSION" in
    ""|*/*|*" "*|*"?"*|*"#"*) fatal "PROXYSCENE_VERSION 无效：$MANAGER_VERSION" ;;
  esac
  if [[ -n "$MANAGER_BASE_URL" ]]; then
    case "$MANAGER_BASE_URL" in
      https://*) ;;
      *) fatal "PROXYSCENE_BASE_URL 必须是 https 地址：$MANAGER_BASE_URL" ;;
    esac
    case "$MANAGER_BASE_URL" in
      *"?"*|*"#"*) fatal "PROXYSCENE_BASE_URL 不能包含 ? 或 #" ;;
    esac
  fi
}

release_base_url() {
  if [[ -n "$MANAGER_BASE_URL" ]]; then
    printf '%s\n' "${MANAGER_BASE_URL%/}"
  elif [[ "$MANAGER_VERSION" == "latest" ]]; then
    printf 'https://github.com/%s/releases/latest/download\n' "$MANAGER_REPO"
  else
    printf 'https://github.com/%s/releases/download/%s\n' "$MANAGER_REPO" "$MANAGER_VERSION"
  fi
}

# fetch_https 仅允许 https（含重定向），并限制下载体积。
fetch_https() {
  local url="$1" out="$2" maxsize="$3"
  curl -fL --proto '=https' --proto-redir '=https' \
    --connect-timeout 15 --retry 3 --retry-delay 2 \
    --max-filesize "$maxsize" -o "$out" "$url"
}

# install_manager_prebuilt 下载并安装预编译二进制；成功返回 0，否则返回 1 由调用方回退源码编译。
install_manager_prebuilt() {
  local arch base asset tmp expected
  arch="$(arch_release)" || { log "当前架构 $(uname -m) 无预编译二进制"; return 1; }
  validate_release_inputs
  base="$(release_base_url)"
  asset="proxyscene_linux_${arch}.tar.gz"
  tmp="$(mktemp -d)"

  log "下载预编译管理程序：$base/$asset"
  if ! fetch_https "$base/$asset" "$tmp/$asset" 104857600; then
    log "预编译二进制下载失败"
    rm -rf "$tmp"
    return 1
  fi
  if ! fetch_https "$base/checksums.txt" "$tmp/checksums.txt" 1048576; then
    log "校验和文件下载失败"
    rm -rf "$tmp"
    return 1
  fi

  if [[ -n "$MANAGER_MINISIGN_PUBKEY" ]] && need_cmd minisign; then
    if fetch_https "$base/checksums.txt.minisig" "$tmp/checksums.txt.minisig" 1048576; then
      run_quiet "校验 checksums 签名" minisign -Vm "$tmp/checksums.txt" -x "$tmp/checksums.txt.minisig" -P "$MANAGER_MINISIGN_PUBKEY"
      log "minisign 签名校验通过"
    elif [[ "$MANAGER_MINISIGN_REQUIRED" == "1" ]]; then
      rm -rf "$tmp"
      fatal "签名文件下载失败，无法按要求校验签名"
    else
      log "未获取到签名文件，跳过签名校验（仍会校验 SHA256）"
    fi
  elif [[ "$MANAGER_MINISIGN_REQUIRED" == "1" ]]; then
    rm -rf "$tmp"
    fatal "设置了 PROXYSCENE_MINISIGN_PUBKEY 但未找到 minisign"
  else
    log "警告：未安装 minisign，跳过签名校验，仅校验 SHA256。镜像等不可信源可能同源篡改 SHA256 与二进制；如需密码学保证，请安装 minisign 或设置 PROXYSCENE_MINISIGN_PUBKEY 强制验签。"
  fi

  # checksums.txt 每行形如 "<64hex>  <asset>"（二进制模式为 "<hex> *<asset>"）。
  expected="$(awk -v a="$asset" '{name=$2; sub(/^\*/,"",name); if (name==a) {print $1; exit}}' "$tmp/checksums.txt")"
  if [[ -z "$expected" ]]; then
    fatal "checksums.txt 中找不到 $asset 的校验和"
  fi
  verify_sha256_file "管理程序" "$tmp/$asset" "$expected"
  log "管理程序 SHA256 校验通过"

  run_quiet "解压管理程序" tar -xzf "$tmp/$asset" -C "$tmp" proxyscene
  [[ -f "$tmp/proxyscene" ]] || fatal "压缩包中未找到 proxyscene"
  run_quiet "安装管理程序" install -D -m 755 "$tmp/proxyscene" "$INSTALL_BIN"
  log "预编译管理程序已安装：$INSTALL_BIN（版本 $("$INSTALL_BIN" version 2>/dev/null || echo 未知)）"
  rm -rf "$tmp"
  return 0
}

build_manager() {
  local dir out
  dir="$(repo_dir)"
  out="$(mktemp "$dir/.proxyscene-build.XXXXXX")"
  trap 'rm -f "$out"' EXIT
  [[ -f "$dir/go.mod" ]] || fatal "未找到 go.mod：$dir/go.mod"

  log "编译本地 Go 管理程序"
  run_quiet "编译 Go 管理程序" env CGO_ENABLED=0 go build -C "$dir" -trimpath -ldflags "-s -w" -o "$out" ./cmd/proxyscene
  run_quiet "安装 Go 管理程序" install -D -m 755 "$out" "$INSTALL_BIN"
  rm -f "$out"
  trap - EXIT
  log "Go 管理程序已安装：$INSTALL_BIN"
}

# install_manager 默认优先下载预编译二进制（目标机无需 Go），失败时回退本地源码编译。
install_manager() {
  if [[ "$BUILD_FROM_SOURCE" == "1" ]]; then
    log "按 PROXYSCENE_BUILD_FROM_SOURCE=1 从源码编译管理程序"
    ensure_go
    build_manager
    return 0
  fi
  if install_manager_prebuilt; then
    return 0
  fi
  # 预编译失败时才回退源码编译，但这只有在源码目录（有 go.mod）里才可能。
  # 通过 `curl | sudo bash` 在任意目录运行时拿不到仓库，给出清晰指引而不是含糊的 go.mod 报错。
  if [[ ! -f "$(repo_dir)/go.mod" ]]; then
    fatal "预编译二进制下载失败，且当前不在源码目录（找不到 go.mod），无法回退编译。请：① 确认仓库已发布对应架构的 Release（检查 PROXYSCENE_VERSION/PROXYSCENE_REPO）；或 ② 克隆仓库后在源码目录运行 ./install.sh；或 ③ 用 --offline 离线整合包安装。"
  fi
  log "改用本地源码编译管理程序"
  ensure_go
  build_manager
}

# bundle_dir 返回 install.sh 物理所在目录；通过管道（curl|bash）运行时 BASH_SOURCE 为空，返回空。
bundle_dir() {
  local src="${BASH_SOURCE[0]:-}"
  [[ -n "$src" && -f "$src" ]] || return 0
  (cd "$(dirname "$src")" && pwd -P)
}

# is_self_contained_bundle 判断 install.sh 同目录是否为自包含整合包（解压后形态：
# install.sh 与 proxyscene / xray 二进制同处一目录）。
is_self_contained_bundle() {
  local bdir="$1"
  [[ -n "$bdir" && -f "$bdir/proxyscene" && -f "$bdir/xray" && ! -f "$bdir/go.mod" ]]
}

# install_offline_local 从 install.sh 同目录的整合包文件离线安装（不联网、不需要 Go）。
# 自包含整合包里 install.sh 与二进制同处一目录，解压后直接运行本脚本即走这里。
# 注意：自包含包无法验证它自身（脚本与二进制都在包内）；如需密码学保证，请在解压前用随包的
# .minisig + 公钥验证整个 tar。安装前对每个文件强制"常规文件且非符号链接"检查。
install_offline_local() {
  local bdir="$1" f
  require_root
  for f in proxyscene xray; do
    [[ -f "$bdir/$f" && ! -L "$bdir/$f" ]] || fatal "整合包缺少 $f 或不是常规文件：$bdir/$f"
  done
  log "离线本地安装（来自 $bdir，不联网、不需要 Go）"
  log "提示：未对整合包做验签；如需校验，请在解压前执行：minisign -Vm <包>.tar.gz -x <包>.tar.gz.minisig -P <公钥>"
  ensure_core_dir
  run_quiet "安装管理程序" install -D -m 755 "$bdir/proxyscene" "$INSTALL_BIN"
  run_quiet "安装 Xray" install -m 700 "$bdir/xray" "$CORE_DIR/xray"
  for f in geoip.dat geosite.dat; do
    if [[ -f "$bdir/$f" && ! -L "$bdir/$f" ]]; then
      install -m 600 "$bdir/$f" "$CORE_DIR/$f"
    fi
  done
  log "离线安装完成：$INSTALL_BIN（版本 $("$INSTALL_BIN" version 2>/dev/null || echo 未知)）、$CORE_DIR/xray"
}

init_manager() {
  if [[ "$SKIP_MANAGER_INIT" == "1" ]]; then
    log "跳过管理服务初始化"
    return 0
  fi

  if [[ -n "$NODE_URL" ]]; then
    log "初始化管理服务并导入节点"
    PROXYSCENE_MANAGER_DIR="$CORE_DIR" PROXYSCENE_SWITCH_BIN="$INSTALL_BIN" "$INSTALL_BIN" install "$NODE_URL"
  else
    log "初始化管理服务，不导入节点"
    PROXYSCENE_MANAGER_DIR="$CORE_DIR" PROXYSCENE_SWITCH_BIN="$INSTALL_BIN" "$INSTALL_BIN" install --skip-node
  fi
}

validate_install_bin() {
  [[ -n "$INSTALL_BIN" ]] || fatal "PROXYSCENE_SWITCH_BIN 不能为空"
  [[ "$INSTALL_BIN" == /* ]] || fatal "PROXYSCENE_SWITCH_BIN 必须是绝对路径：$INSTALL_BIN"
  if [[ "$INSTALL_BIN" == *[$' \t\r\n']* ]]; then
    fatal "PROXYSCENE_SWITCH_BIN 不能包含空白字符：$INSTALL_BIN"
  fi
  if [[ -L "$INSTALL_BIN" ]]; then
    fatal "PROXYSCENE_SWITCH_BIN 不能是符号链接：$INSTALL_BIN"
  fi
}

main() {
  validate_install_bin
  local bdir
  bdir="$(bundle_dir)"
  # 自包含整合包：install.sh 与二进制同处一目录（解压后形态），或显式 --offline。
  if [[ "$FORCE_OFFLINE_LOCAL" == "1" ]] || is_self_contained_bundle "$bdir"; then
    if ! is_self_contained_bundle "$bdir"; then
      fatal "--offline 需要在解压后的整合包目录内运行（同目录应有 proxyscene 与 xray 二进制）"
    fi
    install_offline_local "$bdir"
    init_manager
    log "离线安装完成。运行：sudo $INSTALL_BIN"
    return 0
  fi
  require_root
  install_packages
  install_xray
  install_manager
  init_manager
  log "安装完成。运行：sudo $INSTALL_BIN"
}

main "$@"
