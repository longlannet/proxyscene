#!/usr/bin/env bash
set -euo pipefail
umask 077
export LC_ALL=C TZ=UTC

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT"

DIST="${DIST:-dist}"
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
VERSION="${VERSION#v}"
COMMIT="${COMMIT:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}"
SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(git show -s --format=%ct HEAD 2>/dev/null || echo 0)}"
XRAY_VERSION="v26.3.27"
XRAY_COMMIT="d2758a023cd7f4174a5a5fa4ff66e487d4342ba0"
XRAY_SOURCE_MODULE_VERSION="v1.260327.0"
XRAY_SOURCE_MODULE_SUM="h1:g4TzxMwyPrxslZh6uD+FiG3lXKTrnNO+b4ky2OhogHE="

fail() {
  printf 'release artifact verification failed: %s\n' "$*" >&2
  exit 1
}

for tool in awk cmp date diff find go grep mkdir mktemp od rm sha256sum sort stat strings tar tr; do
  command -v "$tool" >/dev/null 2>&1 || fail "missing required tool: $tool"
done
[[ -d "$DIST" ]] || fail "missing dist directory: $DIST"
[[ "$VERSION" =~ ^[0-9][0-9A-Za-z._-]*$ ]] || fail "unsafe VERSION: $VERSION"
[[ "$COMMIT" =~ ^[0-9A-Fa-f]{40}$ ]] || fail "COMMIT must be a 40-character Git hash"
[[ "$SOURCE_DATE_EPOCH" =~ ^[0-9]+$ ]] || fail "SOURCE_DATE_EPOCH must be a non-negative integer"
expected_mtime="$(date -u -d "@${SOURCE_DATE_EPOCH}" '+%Y-%m-%d %H:%M:%S')"

elf_machine() {
  case "$1" in
    amd64) printf '62\n' ;;
    arm64) printf '183\n' ;;
    386) printf '3\n' ;;
    armv7) printf '40\n' ;;
    *) return 1 ;;
  esac
}

elf_class() {
  case "$1" in
    amd64|arm64) printf '2\n' ;;
    386|armv7) printf '1\n' ;;
    *) return 1 ;;
  esac
}

validate_elf() {
  local path="$1" arch="$2" label="$3" magic class machine expected
  magic="$(od -An -N4 -t x1 "$path" | tr -d ' \n')"
  [[ "$magic" == "7f454c46" ]] || fail "$label is not ELF: $path"
  class="$(od -An -j4 -N1 -t u1 "$path" | awk 'NF {print $1; exit}')"
  expected="$(elf_class "$arch")" || fail "unsupported ELF architecture: $arch"
  [[ "$class" == "$expected" ]] \
    || fail "$label ELF class mismatch for $arch: expected $expected, got $class"
  machine="$(od -An -j18 -N2 -t u1 "$path" | awk 'NF >= 2 {print $1 + (256 * $2); exit}')"
  expected="$(elf_machine "$arch")" || fail "unsupported ELF architecture: $arch"
  [[ "$machine" == "$expected" ]] \
    || fail "$label ELF machine mismatch for $arch: expected $expected, got $machine"
}

assert_archive_members() {
  local archive="$1"
  shift
  local expected actual
  expected="$(printf '%s\n' "$@" | sort)"
  actual="$(tar -tzf "$archive" | sort)"
  [[ "$actual" == "$expected" ]] || {
    diff -u <(printf '%s\n' "$expected") <(printf '%s\n' "$actual") >&2 || true
    fail "unexpected archive members: $archive"
  }
}

archive_mode_for_member() {
  local kind="$1" root_name="$2" member="$3"
  if [[ "$kind" == "source" ]]; then
    if [[ "$member" == */ ]]; then
      printf '%s\n' 'drwxr-xr-x'
    else
      printf '%s\n' '-rw-r--r--'
    fi
    return 0
  fi
  case "$kind:$member" in
    manager:proxyscene) printf '%s\n' '-rwxr-xr-x' ;;
    manager:LICENSE|manager:NOTICE|manager:SOURCE-Xray|manager:THIRD_PARTY_LICENSES|manager:THIRD_PARTY_LICENSES-Xray)
      printf '%s\n' '-rw-r--r--'
      ;;
    bundle:"${root_name}/") printf '%s\n' 'drwxr-xr-x' ;;
    bundle:"${root_name}/proxyscene"|bundle:"${root_name}/install.sh"|bundle:"${root_name}/xray")
      printf '%s\n' '-rwxr-xr-x'
      ;;
    bundle:*) printf '%s\n' '-rw-r--r--' ;;
    *) return 1 ;;
  esac
}

validate_archive_headers() {
  local archive="$1" kind="$2" root_name="$3"
  local mode owner size header_date header_time member extra expected_mode count=0
  while read -r mode owner size header_date header_time member extra; do
    [[ -z "${extra:-}" ]] || fail "archive member contains whitespace: $archive"
    [[ "$owner" == "0/0" ]] || fail "archive member is not owned by 0:0: $archive:$member ($owner)"
    [[ "$header_date $header_time" == "$expected_mtime" ]] \
      || fail "archive mtime mismatch: $archive:$member ($header_date $header_time)"
    [[ "$size" =~ ^[0-9]+$ ]] || fail "archive size field is invalid: $archive:$member"
    expected_mode="$(archive_mode_for_member "$kind" "$root_name" "$member")" \
      || fail "unexpected archive member while checking mode: $archive:$member"
    [[ "$mode" == "$expected_mode" ]] \
      || fail "archive mode mismatch: $archive:$member (expected $expected_mode, got $mode)"
    count=$((count + 1))
  done < <(tar --list --verbose --numeric-owner --full-time --gzip --file "$archive")
  (( count > 0 )) || fail "empty archive: $archive"
}

targets=("$@")
if [[ ${#targets[@]} -eq 0 ]]; then
  targets=(amd64 arm64 386 armv7)
fi
declare -A seen=()
for arch in "${targets[@]}"; do
  elf_machine "$arch" >/dev/null || fail "unsupported target: $arch"
  [[ -z "${seen[$arch]:-}" ]] || fail "duplicate target: $arch"
  seen[$arch]=1
done

expected_assets=(install.sh checksums.txt "xray_source_${XRAY_VERSION}.tar.gz")
for arch in "${targets[@]}"; do
  expected_assets+=("proxyscene_bundle_linux_${arch}.tar.gz" "proxyscene_linux_${arch}.tar.gz")
done
expected_names="$(printf '%s\n' "${expected_assets[@]}" | sort)"
actual_names="$(find "$DIST" -mindepth 1 -maxdepth 1 -type f -printf '%f\n' | sort)"
[[ -z "$(find "$DIST" -mindepth 1 -maxdepth 1 ! -type f -print -quit)" ]] \
  || fail "dist contains a directory, symlink, or special file"
[[ "$actual_names" == "$expected_names" ]] || {
  diff -u <(printf '%s\n' "$expected_names") <(printf '%s\n' "$actual_names") >&2 || true
  fail "dist asset set is not exact"
}

manifest_names="$(awk '
  NF != 2 || length($1) != 64 || $1 !~ /^[0-9a-f]+$/ { exit 1 }
  { print $2 }
' "$DIST/checksums.txt" | sort)" || fail "checksums.txt syntax is invalid"
expected_manifest_names="$(printf '%s\n' "${expected_assets[@]}" | grep -v '^checksums[.]txt$' | sort)"
[[ "$manifest_names" == "$expected_manifest_names" ]] \
  || fail "checksums.txt does not name the exact non-manifest asset set"
(cd "$DIST" && sha256sum -c checksums.txt) >/dev/null \
  || fail "checksums.txt verification failed"

for asset in "${expected_assets[@]}"; do
  expected_asset_mode=644
  [[ "$asset" == "install.sh" ]] && expected_asset_mode=755
  [[ "$(stat -c '%a' "$DIST/$asset")" == "$expected_asset_mode" ]] \
    || fail "release asset mode is not 0$expected_asset_mode: $asset"
done
grep -Fxq "DEFAULT_MANAGER_VERSION=\"v${VERSION}\"" "$DIST/install.sh" \
  || fail "release install.sh is not bound to v${VERSION}"
grep -Fxq "DEFAULT_XRAY_VERSION=\"${XRAY_VERSION}\"" "$DIST/install.sh" \
  || fail "release install.sh is not bound to Xray ${XRAY_VERSION}"

work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
reference_xray=""
reference_xray_license=""
for arch in "${targets[@]}"; do
  manager_archive="$DIST/proxyscene_linux_${arch}.tar.gz"
  bundle_archive="$DIST/proxyscene_bundle_linux_${arch}.tar.gz"
  bundle_root="proxyscene_bundle_linux_${arch}"
  arch_dir="$work/$arch"
  manager_dir="$arch_dir/manager"
  bundle_dir="$arch_dir/bundle"
  mkdir -p "$manager_dir" "$bundle_dir"

  assert_archive_members "$manager_archive" \
    LICENSE NOTICE SOURCE-Xray THIRD_PARTY_LICENSES THIRD_PARTY_LICENSES-Xray proxyscene
  validate_archive_headers "$manager_archive" manager ""
  assert_archive_members "$bundle_archive" \
    "$bundle_root/" \
    "$bundle_root/LICENSE" \
    "$bundle_root/LICENSE-Xray" \
    "$bundle_root/NOTICE" \
    "$bundle_root/SOURCE-Xray" \
    "$bundle_root/THIRD_PARTY_LICENSES" \
    "$bundle_root/THIRD_PARTY_LICENSES-Xray" \
    "$bundle_root/bundle-manifest.sha256" \
    "$bundle_root/install.sh" \
    "$bundle_root/proxyscene" \
    "$bundle_root/xray" \
    "$bundle_root/xray-version.txt"
  validate_archive_headers "$bundle_archive" bundle "$bundle_root"

  tar --extract --gzip --file "$manager_archive" --directory "$manager_dir" \
    --no-same-owner --no-same-permissions
  tar --extract --gzip --file "$bundle_archive" --directory "$bundle_dir" \
    --no-same-owner --no-same-permissions
  bundle_dir="$bundle_dir/$bundle_root"

  validate_elf "$manager_dir/proxyscene" "$arch" "$arch manager"
  validate_elf "$bundle_dir/xray" "$arch" "$arch Xray"
  cmp "$manager_dir/proxyscene" "$bundle_dir/proxyscene" \
    || fail "$arch manager differs between standalone and bundle archives"
  cmp "$manager_dir/LICENSE" "$bundle_dir/LICENSE" \
    || fail "$arch license differs between standalone and bundle archives"
  cmp "$manager_dir/NOTICE" "$bundle_dir/NOTICE" \
    || fail "$arch notice differs between standalone and bundle archives"
  cmp "$manager_dir/SOURCE-Xray" "$bundle_dir/SOURCE-Xray" \
    || fail "$arch Xray source notice differs between standalone and bundle archives"
  cmp "$manager_dir/THIRD_PARTY_LICENSES" "$bundle_dir/THIRD_PARTY_LICENSES" \
    || fail "$arch third-party licenses differ between standalone and bundle archives"
  cmp "$manager_dir/THIRD_PARTY_LICENSES-Xray" "$bundle_dir/THIRD_PARTY_LICENSES-Xray" \
    || fail "$arch Xray third-party licenses differ between standalone and bundle archives"
  cmp "$DIST/install.sh" "$bundle_dir/install.sh" \
    || fail "$arch bundle installer differs from the release installer"
  if [[ -z "$reference_xray" ]]; then
    reference_xray="$bundle_dir/xray"
    reference_xray_license="$bundle_dir/LICENSE-Xray"
  fi

  embedded="$(strings -a "$manager_dir/proxyscene")"
  grep -Fq -- "$VERSION" <<< "$embedded" \
    || fail "$arch manager does not contain the release version"
  grep -Fq -- "$COMMIT" <<< "$embedded" \
    || fail "$arch manager does not contain the full source commit"
  if [[ "$arch" == "amd64" ]]; then
    expected_version_output="proxyscene ${VERSION} (${COMMIT:0:12})"
    actual_version_output="$("$manager_dir/proxyscene" version)" \
      || fail "amd64 manager could not execute its version command"
    [[ "$actual_version_output" == "$expected_version_output" ]] \
      || fail "amd64 version output mismatch: expected '$expected_version_output', got '$actual_version_output'"
  fi

  [[ "$(tr -d '\r\n' < "$bundle_dir/xray-version.txt")" == "$XRAY_VERSION" ]] \
    || fail "$arch bundle Xray marker is not $XRAY_VERSION"
  bundle_manifest_names="$(awk '
    NF != 2 || length($1) != 64 || $1 !~ /^[0-9a-f]+$/ { exit 1 }
    { print $2 }
  ' "$bundle_dir/bundle-manifest.sha256" | sort)" \
    || fail "$arch bundle manifest syntax is invalid"
  expected_bundle_manifest_names="$(printf '%s\n' \
    LICENSE LICENSE-Xray NOTICE SOURCE-Xray THIRD_PARTY_LICENSES THIRD_PARTY_LICENSES-Xray \
    install.sh proxyscene xray xray-version.txt | sort)"
  [[ "$bundle_manifest_names" == "$expected_bundle_manifest_names" ]] \
    || fail "$arch bundle manifest does not have the exact component set"
  (cd "$bundle_dir" && sha256sum -c bundle-manifest.sha256) >/dev/null \
    || fail "$arch bundle manifest verification failed"
done

source_archive="$DIST/xray_source_${XRAY_VERSION}.tar.gz"
source_root_name="xray_source_${XRAY_VERSION}"
while IFS= read -r entry; do
  normalized="${entry#./}"
  [[ -n "$normalized" ]] || fail "Xray source archive contains an empty member name"
  case "$normalized" in
    /*|..|../*|*/..|*/../*)
      fail "Xray source archive contains an unsafe member name: $entry"
      ;;
  esac
  case "$normalized" in
    "$source_root_name"|"$source_root_name/"|"$source_root_name/"*) ;;
    *) fail "Xray source archive has a member outside its root: $entry" ;;
  esac
done < <(tar -tzf "$source_archive")
validate_archive_headers "$source_archive" source "$source_root_name"

source_extract="$work/source"
mkdir "$source_extract"
tar --extract --gzip --file "$source_archive" --directory "$source_extract" \
  --no-same-owner --no-same-permissions
source_root="$source_extract/$source_root_name"
[[ -d "$source_root" && ! -L "$source_root" ]] \
  || fail "Xray source archive root is missing or unsafe"
if find "$source_root" -mindepth 1 \( ! -type d -a ! -type f \) -print -quit | grep -q .; then
  fail "Xray source archive contains a symlink or special file"
fi

expected_source_top="$(printf '%s\n' \
  LICENSE-GPL-3.0 LICENSE-Xray MODULES.tsv README THIRD_PARTY_LICENSES-Xray \
  modules source-manifest.sha256 | sort)"
actual_source_top="$(find "$source_root" -mindepth 1 -maxdepth 1 -printf '%f\n' | sort)"
[[ "$actual_source_top" == "$expected_source_top" ]] \
  || fail "Xray source archive top-level member set is not exact"

source_manifest_names="$(awk '
  NF != 2 || length($1) != 64 || $1 !~ /^[0-9a-f]+$/ { exit 1 }
  { print $2 }
' "$source_root/source-manifest.sha256" | sort)" \
  || fail "Xray source manifest syntax is invalid"
actual_source_files="$(find "$source_root" -type f ! -name source-manifest.sha256 -printf '%P\n' | sort)"
[[ "$source_manifest_names" == "$actual_source_files" ]] \
  || fail "Xray source manifest does not cover the exact file set"
(cd "$source_root" && sha256sum -c source-manifest.sha256) >/dev/null \
  || fail "Xray source manifest verification failed"

expected_module_dirs="$(awk 'BEGIN { for (i = 0; i < 35; i++) printf "%04d\n", i }')"
actual_module_dirs="$(find "$source_root/modules" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' | sort)"
[[ "$actual_module_dirs" == "$expected_module_dirs" ]] \
  || fail "Xray source archive does not contain exactly 35 numbered module directories"
while IFS= read -r module_index; do
  module_files="$(find "$source_root/modules/$module_index" -mindepth 1 -maxdepth 1 -type f -printf '%f\n' | sort)"
  [[ "$module_files" == $'go.mod\ninfo.json\nsource.zip' ]] \
    || fail "Xray source module $module_index has an unexpected file set"
done <<< "$expected_module_dirs"

awk -F '\t' \
  -v xray_version="$XRAY_SOURCE_MODULE_VERSION" \
  -v xray_sum="$XRAY_SOURCE_MODULE_SUM" '
  NR == 1 {
    if ($0 != "kind\tmodule\tversion\tmodule_sum\tzip_sha256\tsource_path") exit 1
    next
  }
  {
    if (NF != 6 || $4 !~ /^h1:[A-Za-z0-9+\/=]+$/ ||
        length($5) != 64 || $5 !~ /^[0-9a-f]+$/) exit 1
    expected_path = sprintf("modules/%04d/source.zip", NR - 2)
    if ($6 != expected_path) exit 1
  }
  NR == 2 {
    if ($1 != "xray" || $2 != "github.com/xtls/xray-core" ||
        $3 != xray_version || $4 != xray_sum) exit 1
    next
  }
  NR > 2 {
    if ($1 != "dependency") exit 1
  }
  END { if (NR != 36) exit 1 }
' "$source_root/MODULES.tsv" \
  || fail "Xray source module inventory is invalid"

binary_deps="$work/xray-binary-dependencies.tsv"
source_deps="$work/xray-source-dependencies.tsv"
go version -m "$reference_xray" \
  | awk -F '\t' '$2 == "dep" { print $3 "\t" $4 "\t" $5 }' \
  | sort -u > "$binary_deps"
awk -F '\t' 'NR > 2 { print $2 "\t" $3 "\t" $4 }' \
  "$source_root/MODULES.tsv" | sort -u > "$source_deps"
cmp -- "$binary_deps" "$source_deps" \
  || fail "Xray source inventory does not match the bundled ELF dependencies"

{
  IFS= read -r _header
  while IFS=$'\t' read -r _kind _module _version _sum zip_sha source_path; do
    [[ "$(sha256sum "$source_root/$source_path" | awk '{print $1}')" == "$zip_sha" ]] \
      || fail "Xray source zip digest mismatch: $source_path"
  done
} < "$source_root/MODULES.tsv"

cmp -- SOURCE-Xray "$source_root/README" \
  || fail "Xray source archive README differs from SOURCE-Xray"
grep -Fq -- "$XRAY_COMMIT" "$source_root/README" \
  || fail "Xray source archive README is not bound to the reviewed commit"
cmp -- LICENSE-GPL-3.0 "$source_root/LICENSE-GPL-3.0" \
  || fail "Xray source archive GPL-3.0 text differs from the reviewed copy"
cmp -- THIRD_PARTY_LICENSES-Xray "$source_root/THIRD_PARTY_LICENSES-Xray" \
  || fail "Xray source archive license collection differs from the reviewed copy"
cmp -- "$reference_xray_license" "$source_root/LICENSE-Xray" \
  || fail "Xray source archive project license differs from the bundled Xray license"

rm -rf -- "$work"
trap - EXIT
printf 'release artifact verification passed for: %s\n' "${targets[*]}"
