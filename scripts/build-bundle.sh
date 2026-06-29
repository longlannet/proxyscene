#!/usr/bin/env bash
# 构建 proxyscene 发布产物：每个架构生成「仅含管理程序的二进制包」与
# 「自包含离线整合包」（管理程序 + install.sh + Xray + geo 数据 + 许可证）。
#
# 由 .github/workflows/release.yml 调用，并在 CI 中以 dry-run 方式验证
# （见 .github/workflows/ci.yml 的 build-dry-run 任务）。
#
# 用法：
#   scripts/build-bundle.sh [target ...]
# 不带参数时构建全部架构；可指定子集，如 `scripts/build-bundle.sh amd64`。
#
# 可用 target：amd64 arm64 386 armv7
#
# 环境变量（均可覆盖）：
#   VERSION             版本号（默认取 git describe；自动去掉前导 v）
#   COMMIT              提交哈希（默认取 git rev-parse HEAD）
#   DIST                产物输出目录（默认 dist）
#   XRAY_RELEASE_BASE   Xray 发布下载基地址（默认 XTLS latest）
set -euo pipefail

# 切换到仓库根目录，使 install.sh / NOTICE / LICENSE / ./cmd 等相对路径稳定可解析。
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT"

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
VERSION="${VERSION#v}"
COMMIT="${COMMIT:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}"
DIST="${DIST:-dist}"
XRAY_RELEASE_BASE="${XRAY_RELEASE_BASE:-https://github.com/XTLS/Xray-core/releases/latest/download}"

LDFLAGS="-s -w -X proxyscene/internal/manager.Version=${VERSION} -X proxyscene/internal/manager.Commit=${COMMIT}"

# 必备工具检查（缺失时给出清晰报错，安装由调用方/工作流负责）。
for tool in go curl unzip tar sha256sum install mktemp; do
  command -v "$tool" >/dev/null 2>&1 || { echo "缺少必备工具：$tool" >&2; exit 1; }
done

# 下载并用官方 .dgst 校验 Xray，解压到 "$out/x"。
fetch_xray() {
  local xrayarch="$1" out="$2" exp
  curl -fsSL --proto '=https' --proto-redir '=https' -o "$out/xray.zip" \
    "${XRAY_RELEASE_BASE}/Xray-linux-${xrayarch}.zip"
  curl -fsSL --proto '=https' --proto-redir '=https' -o "$out/xray.zip.dgst" \
    "${XRAY_RELEASE_BASE}/Xray-linux-${xrayarch}.zip.dgst"
  exp="$(grep -iE 'sha2-?256' "$out/xray.zip.dgst" | grep -oiE '[0-9a-f]{64}' | head -n1)"
  [[ "$exp" =~ ^[0-9a-f]{64}$ ]] || { echo "bad Xray .dgst for ${xrayarch}"; exit 1; }
  echo "${exp}  ${out}/xray.zip" | sha256sum -c -
  unzip -oq "$out/xray.zip" -d "$out/x"
}

build_one() {
  local goarch="$1" goarm="$2" name="$3" xrayarch="$4" stage pkg
  echo "==> 构建 linux/${name} (GOARCH=${goarch} GOARM=${goarm:-none}, Xray=${xrayarch})"
  stage="$(mktemp -d)"
  CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" GOARM="$goarm" \
    go build -trimpath -ldflags="$LDFLAGS" -o "$stage/proxyscene" ./cmd/proxyscene
  # 仅含管理程序的二进制包。
  tar -C "$stage" -czf "${DIST}/proxyscene_linux_${name}.tar.gz" proxyscene
  # 自包含离线整合包：顶层目录内含 install.sh + 管理程序 + Xray + geo + NOTICE。
  # 解压后 `cd <目录> && sudo ./install.sh` 即可离线安装。
  pkg="proxyscene_bundle_linux_${name}"
  mkdir -p "$stage/$pkg"
  install -m 755 "$stage/proxyscene" "$stage/$pkg/proxyscene"
  install -m 755 install.sh "$stage/$pkg/install.sh"
  install -m 644 NOTICE "$stage/$pkg/NOTICE"
  install -m 644 LICENSE "$stage/$pkg/LICENSE"
  fetch_xray "$xrayarch" "$stage"
  install -m 755 "$stage/x/xray" "$stage/$pkg/xray"
  install -m 644 "$stage/x/geoip.dat" "$stage/$pkg/geoip.dat"
  install -m 644 "$stage/x/geosite.dat" "$stage/$pkg/geosite.dat"
  # XTLS 发布包内含 LICENSE 时一并打包，满足 MPL-2.0 的许可证随附要求。
  if [[ -f "$stage/x/LICENSE" ]]; then
    install -m 644 "$stage/x/LICENSE" "$stage/$pkg/LICENSE-Xray"
  fi
  tar -C "$stage" -czf "${DIST}/${pkg}.tar.gz" "$pkg"
  rm -rf "$stage"
}

build_target() {
  case "$1" in
    amd64) build_one amd64 ""  amd64 64 ;;
    arm64) build_one arm64 ""  arm64 arm64-v8a ;;
    386)   build_one 386   ""  386   32 ;;
    armv7) build_one arm   7   armv7 arm32-v7a ;;
    *) echo "未知 target：$1（可用：amd64 arm64 386 armv7）" >&2; exit 2 ;;
  esac
}

targets=("$@")
if [[ ${#targets[@]} -eq 0 ]]; then
  targets=(amd64 arm64 386 armv7)
fi

mkdir -p "$DIST"
echo "版本=${VERSION} 提交=${COMMIT} 输出=${DIST} 目标=${targets[*]}"
for t in "${targets[@]}"; do
  build_target "$t"
done

# 生成 checksums.txt（联机安装校验与发布签名使用）。
( cd "$DIST" && sha256sum proxyscene_*.tar.gz > checksums.txt )
echo "==> 完成，产物位于 ${DIST}/"
ls -1 "$DIST"
