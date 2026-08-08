#!/usr/bin/env bash
# Build deterministic proxyscene release archives. Xray is pinned by version and
# by a repository-reviewed SHA256 for every supported architecture.
set -euo pipefail
umask 022

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT"

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
VERSION="${VERSION#v}"
COMMIT="${COMMIT:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}"
DIST="${DIST:-dist}"
SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(git show -s --format=%ct HEAD 2>/dev/null || echo 0)}"
XRAY_VERSION="v26.3.27"
XRAY_COMMIT="d2758a023cd7f4174a5a5fa4ff66e487d4342ba0"
XRAY_RELEASE_BASE="${XRAY_RELEASE_BASE:-https://github.com/XTLS/Xray-core/releases/download/${XRAY_VERSION}}"
DOWNLOAD_TIMEOUT="${DOWNLOAD_TIMEOUT:-300}"

XRAY_SHA256_AMD64="23cd9af937744d97776ee35ecad4972cf4b2109d1e0fe6be9930467608f7c8ae"
XRAY_SHA256_ARM64="4d30283ae614e3057f730f67cd088a42be6fdf91f8639d82cb69e48cde80413c"
XRAY_SHA256_386="d1eeb0d9a9106eefd286fbb73595c2dfe1c48c56aa91ba1c9aefe04f188d0927"
XRAY_SHA256_ARMV7="c7265ae13c63ca0241a037df4ef960ad37938c8a67d984cc08834b2cfdf5654b"

LDFLAGS="-s -w -buildid= -X proxyscene/internal/manager.Version=${VERSION} -X proxyscene/internal/manager.Commit=${COMMIT}"

for tool in go curl unzip tar gzip sha256sum install mktemp git stat jq cmp; do
  command -v "$tool" >/dev/null 2>&1 || { echo "缺少必备工具：$tool" >&2; exit 1; }
done
[[ "$SOURCE_DATE_EPOCH" =~ ^[0-9]+$ ]] || { echo "SOURCE_DATE_EPOCH 必须是非负整数" >&2; exit 1; }
[[ "$DOWNLOAD_TIMEOUT" =~ ^[1-9][0-9]*$ ]] || { echo "DOWNLOAD_TIMEOUT 必须是正整数秒数" >&2; exit 1; }
[[ "$COMMIT" =~ ^[0-9A-Fa-f]{40}$ || "$COMMIT" == "unknown" ]] || { echo "COMMIT 必须是 40 位 Git 哈希或 unknown" >&2; exit 1; }
[[ "$XRAY_RELEASE_BASE" == https://* ]] || { echo "XRAY_RELEASE_BASE 必须是 HTTPS 地址" >&2; exit 1; }

mkdir -p "$DIST"
if find "$DIST" -mindepth 1 -maxdepth 1 -print -quit | grep -q .; then
  echo "输出目录必须为空：$DIST" >&2
  exit 1
fi

RELEASE_TAG="v${VERSION}"
[[ "$RELEASE_TAG" =~ ^v[0-9][0-9A-Za-z._-]*$ ]] || {
  echo "VERSION 无法生成安全 Release tag：$VERSION" >&2
  exit 1
}
RELEASE_INSTALLER="$(mktemp)"
trap 'rm -f "$RELEASE_INSTALLER"' EXIT
sed "s/^DEFAULT_MANAGER_VERSION=.*/DEFAULT_MANAGER_VERSION=\"${RELEASE_TAG}\"/" \
  install.sh > "$RELEASE_INSTALLER"
chmod 755 "$RELEASE_INSTALLER"
grep -qx "DEFAULT_MANAGER_VERSION=\"${RELEASE_TAG}\"" "$RELEASE_INSTALLER" || {
  echo "无法把 install.sh 绑定到 $RELEASE_TAG" >&2
  exit 1
}

xray_sha256() {
  case "$1" in
    amd64) printf '%s\n' "$XRAY_SHA256_AMD64" ;;
    arm64) printf '%s\n' "$XRAY_SHA256_ARM64" ;;
    386) printf '%s\n' "$XRAY_SHA256_386" ;;
    armv7) printf '%s\n' "$XRAY_SHA256_ARMV7" ;;
    *) echo "未知 Xray checksum target：$1" >&2; return 1 ;;
  esac
}

repro_tar() {
  local output="$1" base="$2"
  shift 2
  LC_ALL=C tar --sort=name --mtime="@${SOURCE_DATE_EPOCH}" \
    --owner=0 --group=0 --numeric-owner --format=gnu \
    -C "$base" -cf - "$@" | gzip -n > "$output"
  chmod 644 "$output"
}

require_mode() {
  local path="$1" expected="$2" actual
  actual="$(stat -c '%a' "$path")"
  [[ "$actual" == "$expected" ]] || {
    echo "文件 mode 不符合可复现构建要求：$path（期望 $expected，实际 $actual）" >&2
    exit 1
  }
}

fetch_xray() {
  local target="$1" xrayarch="$2" out="$3" expected size
  expected="$(xray_sha256 "$target")"
  curl -q -fsSL --proto '=https' --proto-redir '=https' \
    --connect-timeout 15 --max-time "$DOWNLOAD_TIMEOUT" \
    --retry 3 --retry-delay 2 --max-filesize 104857600 \
    -o "$out/xray.zip" "${XRAY_RELEASE_BASE}/Xray-linux-${xrayarch}.zip"
  size="$(stat -c '%s' "$out/xray.zip")"
  [[ "$size" -le 104857600 ]] || { echo "Xray 归档超过 100 MiB 上限" >&2; exit 1; }
  printf '%s  %s\n' "$expected" "$out/xray.zip" | sha256sum -c -
  unzip -oq "$out/xray.zip" -d "$out/x"
  for file in xray LICENSE; do
    [[ -f "$out/x/$file" && ! -L "$out/x/$file" ]] || {
      echo "Xray 归档缺少常规文件：$file" >&2
      exit 1
    }
  done
}

build_one() (
  local goarch="$1" goarm="$2" name="$3" xrayarch="$4" stage pkg source_asset
  stage="$(mktemp -d)"
  trap 'rm -rf "$stage"' EXIT
  echo "==> 构建 linux/${name} (GOARCH=${goarch} GOARM=${goarm:-none}, Xray=${xrayarch})"

  CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" GOARM="$goarm" \
    GOENV=off GOFLAGS='' GOTOOLCHAIN=local GOWORK=off \
    go build -trimpath -buildvcs=false -ldflags="$LDFLAGS" \
      -o "$stage/proxyscene" ./cmd/proxyscene
  chmod 755 "$stage/proxyscene"
  install -m 644 LICENSE "$stage/LICENSE"
  install -m 644 NOTICE "$stage/NOTICE"
  install -m 644 THIRD_PARTY_LICENSES "$stage/THIRD_PARTY_LICENSES"
  install -m 644 THIRD_PARTY_LICENSES-Xray "$stage/THIRD_PARTY_LICENSES-Xray"
  install -m 644 SOURCE-Xray "$stage/SOURCE-Xray"
  require_mode "$stage/proxyscene" 755
  require_mode "$stage/LICENSE" 644
  require_mode "$stage/NOTICE" 644
  require_mode "$stage/THIRD_PARTY_LICENSES" 644
  require_mode "$stage/THIRD_PARTY_LICENSES-Xray" 644
  require_mode "$stage/SOURCE-Xray" 644
  repro_tar "${DIST}/proxyscene_linux_${name}.tar.gz" "$stage" \
    proxyscene LICENSE NOTICE SOURCE-Xray THIRD_PARTY_LICENSES THIRD_PARTY_LICENSES-Xray

  pkg="proxyscene_bundle_linux_${name}"
  install -d -m 755 "$stage/$pkg"
  install -m 755 "$stage/proxyscene" "$stage/$pkg/proxyscene"
  install -m 755 "$RELEASE_INSTALLER" "$stage/$pkg/install.sh"
  install -m 644 NOTICE "$stage/$pkg/NOTICE"
  install -m 644 THIRD_PARTY_LICENSES "$stage/$pkg/THIRD_PARTY_LICENSES"
  install -m 644 THIRD_PARTY_LICENSES-Xray "$stage/$pkg/THIRD_PARTY_LICENSES-Xray"
  install -m 644 SOURCE-Xray "$stage/$pkg/SOURCE-Xray"
  install -m 644 LICENSE "$stage/$pkg/LICENSE"
  fetch_xray "$name" "$xrayarch" "$stage"
  "$SCRIPT_DIR/generate-xray-third-party-licenses.sh" \
    "$stage/x/xray" "$stage/generated-xray-licenses" "$XRAY_VERSION" "$XRAY_COMMIT"
  cmp -- THIRD_PARTY_LICENSES-Xray "$stage/generated-xray-licenses" || {
    echo "仓库内 Xray 第三方许可证与实际 $name 二进制依赖不一致" >&2
    exit 1
  }
  source_asset="${DIST}/xray_source_${XRAY_VERSION}.tar.gz"
  if [[ ! -e "$source_asset" && ! -L "$source_asset" ]]; then
    "$SCRIPT_DIR/build-xray-source-archive.sh" \
      "$stage/x/xray" "$stage/x/LICENSE" "$source_asset" "$SOURCE_DATE_EPOCH"
  fi
  install -m 755 "$stage/x/xray" "$stage/$pkg/xray"
  install -m 644 "$stage/x/LICENSE" "$stage/$pkg/LICENSE-Xray"
  printf '%s\n' "$XRAY_VERSION" > "$stage/$pkg/xray-version.txt"
  chmod 644 "$stage/$pkg/xray-version.txt"
  (
    cd "$stage/$pkg"
    sha256sum LICENSE LICENSE-Xray NOTICE SOURCE-Xray THIRD_PARTY_LICENSES \
      THIRD_PARTY_LICENSES-Xray install.sh proxyscene xray xray-version.txt \
      > bundle-manifest.sha256
    chmod 644 bundle-manifest.sha256
  )
  require_mode "$stage/$pkg" 755
  for file in proxyscene install.sh xray; do
    require_mode "$stage/$pkg/$file" 755
  done
  for file in LICENSE LICENSE-Xray NOTICE SOURCE-Xray THIRD_PARTY_LICENSES THIRD_PARTY_LICENSES-Xray xray-version.txt bundle-manifest.sha256; do
    require_mode "$stage/$pkg/$file" 644
  done
  repro_tar "${DIST}/${pkg}.tar.gz" "$stage" "$pkg"
  rm -rf "$stage"
  trap - EXIT
)

build_target() {
  case "$1" in
    amd64) build_one amd64 "" amd64 64 ;;
    arm64) build_one arm64 "" arm64 arm64-v8a ;;
    386) build_one 386 "" 386 32 ;;
    armv7) build_one arm 7 armv7 arm32-v7a ;;
    *) echo "未知 target：$1（可用：amd64 arm64 386 armv7）" >&2; exit 2 ;;
  esac
}

targets=("$@")
if [[ ${#targets[@]} -eq 0 ]]; then
  targets=(amd64 arm64 386 armv7)
fi

echo "版本=${VERSION} 提交=${COMMIT} 输出=${DIST} 目标=${targets[*]}"
for target in "${targets[@]}"; do
  build_target "$target"
done

install -m 755 "$RELEASE_INSTALLER" "$DIST/install.sh"
(
  cd "$DIST"
  mapfile -t release_files < <(find . -maxdepth 1 -type f ! -name checksums.txt -printf '%f\n' | LC_ALL=C sort)
  : > checksums.txt
  for file in "${release_files[@]}"; do
    sha256sum "$file" >> checksums.txt
  done
  chmod 644 checksums.txt
  sha256sum -c checksums.txt
)
require_mode "$DIST/install.sh" 755
while IFS= read -r file; do
  require_mode "$file" 644
done < <(find "$DIST" -maxdepth 1 -type f ! -name install.sh -print | LC_ALL=C sort)
echo "==> 完成，产物位于 ${DIST}/"
find "$DIST" -mindepth 1 -maxdepth 1 -type f -printf '%f\n' | LC_ALL=C sort
rm -f "$RELEASE_INSTALLER"
trap - EXIT
