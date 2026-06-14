#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_NAME="xray-proxy-go 安装器"
DEFAULT_GO_VERSION="1.22.12"
DEFAULT_CORE_DIR="/opt/xray-proxy-manager"
DEFAULT_INSTALL_BIN="/usr/local/bin/xray-proxy"
DEFAULT_XRAY_DOWNLOAD_SOURCE="official"
DEFAULT_XRAY_ZIP_URL=""
DEFAULT_XRAY_XXV_ZIP_URL="https://xxv.cc/7c9fxLN4nm4BFU8fjD.zip"
DEFAULT_XRAY_GITHUB_RELEASE_BASE="https://github.com/XTLS/Xray-core/releases/latest/download"

GO_VERSION="${GO_VERSION:-$DEFAULT_GO_VERSION}"
GO_TARBALL_SHA256="${GO_TARBALL_SHA256:-}"
GO_INSTALL_DIR="${GO_INSTALL_DIR:-/usr/local}"
GO_ROOT="$GO_INSTALL_DIR/go"
CORE_DIR="${XRAY_PROXY_MANAGER_DIR:-$DEFAULT_CORE_DIR}"
INSTALL_BIN="${XRAY_PROXY_SWITCH_BIN:-$DEFAULT_INSTALL_BIN}"
XRAY_ZIP_URL="${XRAY_ZIP_URL:-$DEFAULT_XRAY_ZIP_URL}"
XRAY_XXV_ZIP_URL="${XRAY_XXV_ZIP_URL:-$DEFAULT_XRAY_XXV_ZIP_URL}"
XRAY_GITHUB_RELEASE_BASE="${XRAY_GITHUB_RELEASE_BASE:-$DEFAULT_XRAY_GITHUB_RELEASE_BASE}"
XRAY_DOWNLOAD_SOURCE="${XRAY_DOWNLOAD_SOURCE:-$DEFAULT_XRAY_DOWNLOAD_SOURCE}"
XRAY_ZIP_SHA256="${XRAY_ZIP_SHA256:-}"
SKIP_GO_INSTALL="${SKIP_GO_INSTALL:-0}"
SKIP_XRAY_INSTALL="${SKIP_XRAY_INSTALL:-0}"
SKIP_MANAGER_INIT="${SKIP_MANAGER_INIT:-0}"
FORCE_GO_INSTALL="${FORCE_GO_INSTALL:-0}"
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

常用环境变量：
  GO_VERSION=1.22.12                         缺少 Go 或版本过低时准备的 Go 版本
  GO_TARBALL_SHA256=...                      Go 安装包 SHA256；留空时从 go.dev 官方 .sha256 文件获取并校验
  GO_INSTALL_DIR=/usr/local                   Go 安装父目录
  SKIP_GO_INSTALL=1                           不安装 Go，要求系统已有 go 命令
  FORCE_GO_INSTALL=1                          即使已有 Go 版本可用，也重新准备指定版本
  XRAY_PROXY_MANAGER_DIR=/opt/xray-proxy-manager
                                             管理器核心目录
  XRAY_PROXY_SWITCH_BIN=/usr/local/bin/xray-proxy
                                             管理程序安装路径
  XRAY_DOWNLOAD_SOURCE=official                Xray 下载源，可选 official 或 xxv
  XRAY_ZIP_URL=https://example.com/xray.zip    自定义 Xray zip 下载地址，优先级高于预设下载源
  XRAY_ZIP_SHA256=...                          Xray zip SHA256；使用自定义或 xxv 源时建议设置
  SKIP_XRAY_INSTALL=1                         不安装 Xray，要求核心目录已有可执行 xray
  SKIP_MANAGER_INIT=1                         只安装依赖和程序，不执行管理器初始化
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi
if [[ $# -gt 1 ]]; then
  fatal "只接受一个可选节点链接参数"
fi
NODE_URL="${1:-}"

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

validate_core_dir() {
  if [[ -z "$CORE_DIR" || "$CORE_DIR" != /* ]]; then
    fatal "XRAY_PROXY_MANAGER_DIR 必须是绝对路径：$CORE_DIR"
  fi
  if [[ "$CORE_DIR" == *[$' \t\r\n']* || "$CORE_DIR" == *//* ]]; then
    fatal "XRAY_PROXY_MANAGER_DIR 不能包含空白字符或重复斜杠：$CORE_DIR"
  fi
  if [[ "$CORE_DIR" == *'/../'* || "$CORE_DIR" == */.. || "$CORE_DIR" == *'/./'* || "$CORE_DIR" == */. ]]; then
    fatal "XRAY_PROXY_MANAGER_DIR 必须使用规范化路径：$CORE_DIR"
  fi
  case "$CORE_DIR" in
    /|/bin|/boot|/dev|/etc|/home|/lib|/lib64|/media|/mnt|/opt|/proc|/root|/run|/sbin|/srv|/sys|/tmp|/usr|/var|/var/lib|/var/opt|/var/tmp)
      fatal "XRAY_PROXY_MANAGER_DIR 不能使用系统目录本身：$CORE_DIR"
      ;;
  esac
  case "$CORE_DIR/" in
    /etc/*|/usr/*|/bin/*|/sbin/*|/lib/*|/lib64/*|/proc/*|/sys/*|/dev/*|/run/*|/home/*|/root/*|/tmp/*|/var/tmp/*)
      fatal "XRAY_PROXY_MANAGER_DIR 不能位于敏感系统目录下：$CORE_DIR"
      ;;
  esac
  case "$CORE_DIR/" in
    /opt/*|/var/lib/*|/var/opt/*) ;;
    *) fatal "XRAY_PROXY_MANAGER_DIR 必须位于 /opt、/var/lib 或 /var/opt 下的专用目录：$CORE_DIR" ;;
  esac
  if [[ -L "$CORE_DIR" ]]; then
    fatal "XRAY_PROXY_MANAGER_DIR 不能是符号链接：$CORE_DIR"
  fi
  if [[ -e "$CORE_DIR" && ! -d "$CORE_DIR" ]]; then
    fatal "XRAY_PROXY_MANAGER_DIR 已存在但不是目录：$CORE_DIR"
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
    fatal "XRAY_PROXY_MANAGER_DIR 不可用：$CORE_DIR"
  fi
  if [[ "$created" == "1" ]]; then
    chmod 700 "$CORE_DIR"
  fi
  printf '由 xray-proxy-go 安装器管理\n' > "$CORE_DIR/.managed-by-xray-proxy-go"
  chmod 600 "$CORE_DIR/.managed-by-xray-proxy-go"
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

  if [[ -n "$XRAY_ZIP_SHA256" ]]; then
    verify_sha256_file "Xray" "$zip" "$XRAY_ZIP_SHA256"
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

build_manager() {
  local dir out
  dir="$(repo_dir)"
  out="$(mktemp "$dir/.xray-proxy-build.XXXXXX")"
  trap 'rm -f "$out"' EXIT
  [[ -f "$dir/go.mod" ]] || fatal "未找到 go.mod：$dir/go.mod"

  log "编译本地 Go 管理程序"
  run_quiet "编译 Go 管理程序" env CGO_ENABLED=0 go build -C "$dir" -trimpath -ldflags "-s -w" -o "$out" ./cmd/xray-proxy
  run_quiet "安装 Go 管理程序" install -m 755 "$out" "$INSTALL_BIN"
  rm -f "$out"
  trap - EXIT
  log "Go 管理程序已安装：$INSTALL_BIN"
}

init_manager() {
  if [[ "$SKIP_MANAGER_INIT" == "1" ]]; then
    log "跳过管理服务初始化"
    return 0
  fi

  if [[ -n "$NODE_URL" ]]; then
    log "初始化管理服务并导入节点"
    XRAY_PROXY_MANAGER_DIR="$CORE_DIR" XRAY_PROXY_SWITCH_BIN="$INSTALL_BIN" "$INSTALL_BIN" install "$NODE_URL"
  else
    log "初始化管理服务，不导入节点"
    XRAY_PROXY_MANAGER_DIR="$CORE_DIR" XRAY_PROXY_SWITCH_BIN="$INSTALL_BIN" "$INSTALL_BIN" install --skip-node
  fi
}

main() {
  require_root
  install_packages
  ensure_go
  install_xray
  build_manager
  init_manager
  log "安装完成。运行：sudo $INSTALL_BIN"
}

main "$@"
