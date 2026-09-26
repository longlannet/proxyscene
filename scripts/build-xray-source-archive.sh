#!/usr/bin/env bash
set -euo pipefail
export GOENV=off GOWORK=off GOTOOLCHAIN=local

umask 022
export LC_ALL=C TZ=UTC

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

readonly XRAY_VERSION="v26.9.9"
readonly XRAY_COMMIT="52a412d9e2f5c2a5142b1b4e2ab3771dacb8b120"
readonly XRAY_SOURCE_MODULE_VERSION="v1.260327.1-0.20260908222543-52a412d9e2f5"
readonly XRAY_SOURCE_MODULE_SUM="h1:BsUC2sCXcdVCb09SUh1iWku0ci779t4bUIlKUor1ZRI="

fail() {
  printf 'Xray source archive build failed: %s\n' "$*" >&2
  exit 1
}

[[ $# -eq 4 ]] || fail "usage: $0 XRAY_BINARY XRAY_LICENSE OUTPUT SOURCE_DATE_EPOCH"
xray_binary="$1"
xray_license="$2"
output="$3"
source_date_epoch="$4"

for tool in go jq find sort mktemp install stat awk sha256sum tar gzip cmp; do
  command -v "$tool" >/dev/null 2>&1 || fail "missing required tool: $tool"
done
[[ -f "$xray_binary" && ! -L "$xray_binary" ]] \
  || fail "Xray binary must be a regular non-symlink file"
[[ -f "$xray_license" && ! -L "$xray_license" ]] \
  || fail "Xray license must be a regular non-symlink file"
[[ "$source_date_epoch" =~ ^[0-9]+$ ]] \
  || fail "SOURCE_DATE_EPOCH must be a non-negative integer"
[[ ! -e "$output" && ! -L "$output" ]] || fail "output already exists: $output"

work_dir="$(mktemp -d)"
trap 'rm -rf -- "$work_dir"' EXIT
source_root="$work_dir/xray_source_${XRAY_VERSION}"
modules_root="$source_root/modules"
deps="$work_dir/dependencies.tsv"
install -d -m 0755 "$source_root" "$modules_root"

go version -m "$xray_binary" \
  | awk -F '\t' '$2 == "dep" { print $3 "\t" $4 "\t" $5 }' \
  | LC_ALL=C sort -u > "$deps"
[[ -s "$deps" ]] || fail "Xray binary contains no Go dependency build information"

while IFS=$'\t' read -r module version sum extra; do
  [[ -z "${extra:-}" ]] || fail "malformed dependency record for $module"
  [[ "$module" =~ ^[A-Za-z0-9][A-Za-z0-9._/+!~-]*$ ]] \
    || fail "unsafe module path: $module"
  [[ "$version" =~ ^v[0-9A-Za-z.+~-]+$ ]] \
    || fail "unsafe module version for $module: $version"
  [[ "$sum" =~ ^h1:[A-Za-z0-9+/=]+$ ]] \
    || fail "invalid module sum for $module@$version"
done < "$deps"

printf 'kind\tmodule\tversion\tmodule_sum\tzip_sha256\tsource_path\n' \
  > "$source_root/MODULES.tsv"
xray_source_dir=""

install_module_source() {
  local kind="$1" module="$2" query="$3" expected_version="$4" expected_sum="$5" index="$6"
  local metadata downloaded_path downloaded_version downloaded_sum zip_path mod_path info_path module_dir zip_sha module_output
  metadata="$(GOENV=off GOWORK=off go mod download -json "${module}@${query}")" \
    || fail "cannot download source for $module@$query"
  if jq -e '.Error != null and .Error != ""' <<< "$metadata" >/dev/null; then
    fail "module source download failed for $module@$query: $(jq -r '.Error' <<< "$metadata")"
  fi
  downloaded_path="$(jq -er '.Path' <<< "$metadata")" || fail "missing Path for $module@$query"
  downloaded_version="$(jq -er '.Version' <<< "$metadata")" || fail "missing Version for $module@$query"
  downloaded_sum="$(jq -er '.Sum' <<< "$metadata")" || fail "missing Sum for $module@$query"
  zip_path="$(jq -er '.Zip' <<< "$metadata")" || fail "missing Zip for $module@$query"
  mod_path="$(jq -er '.GoMod' <<< "$metadata")" || fail "missing GoMod for $module@$query"
  info_path="$(jq -er '.Info' <<< "$metadata")" || fail "missing Info for $module@$query"
  module_dir="$(jq -er '.Dir' <<< "$metadata")" || fail "missing Dir for $module@$query"
  [[ "$downloaded_path" == "$module" && "$downloaded_version" == "$expected_version" ]] \
    || fail "download identity mismatch for $module@$query"
  [[ "$downloaded_sum" == "$expected_sum" ]] \
    || fail "download sum mismatch for $module@$query"
  for path in "$zip_path" "$mod_path" "$info_path"; do
    [[ -f "$path" && ! -L "$path" ]] || fail "module source metadata is unsafe: $path"
  done
  [[ "$module_dir" == /* && -d "$module_dir" && ! -L "$module_dir" ]] \
    || fail "module source directory is unsafe for $module@$query"
  if [[ "$kind" == "xray" ]]; then
    xray_source_dir="$module_dir"
  fi

  module_output="$modules_root/$index"
  install -d -m 0755 "$module_output"
  install -m 0644 "$zip_path" "$module_output/source.zip"
  install -m 0644 "$mod_path" "$module_output/go.mod"
  install -m 0644 "$info_path" "$module_output/info.json"
  zip_sha="$(sha256sum "$module_output/source.zip" | awk '{print $1}')"
  printf '%s\t%s\t%s\t%s\t%s\tmodules/%s/source.zip\n' \
    "$kind" "$module" "$expected_version" "$expected_sum" "$zip_sha" "$index" \
    >> "$source_root/MODULES.tsv"
}

install_module_source xray github.com/xtls/xray-core "$XRAY_COMMIT" \
  "$XRAY_SOURCE_MODULE_VERSION" "$XRAY_SOURCE_MODULE_SUM" 0000
xray_origin="$(GOENV=off GOWORK=off go mod download -json \
  "github.com/xtls/xray-core@${XRAY_COMMIT}" | jq -er '.Origin.Hash')" \
  || fail "cannot verify Xray source origin"
[[ "$xray_origin" == "$XRAY_COMMIT" ]] || fail "Xray source origin commit mismatch"

index=1
while IFS=$'\t' read -r module version sum; do
  printf -v index_text '%04d' "$index"
  install_module_source dependency "$module" "$version" "$version" "$sum" "$index_text"
  index=$((index + 1))
done < "$deps"
[[ "$index" -eq 48 ]] || fail "expected 47 linked modules, got $((index - 1))"

install -m 0644 "$ROOT/SOURCE-Xray" "$source_root/README"
install -m 0644 "$ROOT/LICENSE-GPL-3.0" "$source_root/LICENSE-GPL-3.0"
install -m 0644 "$ROOT/THIRD_PARTY_LICENSES-Xray" "$source_root/THIRD_PARTY_LICENSES-Xray"
install -m 0644 "$xray_license" "$source_root/LICENSE-Xray"
xray_source_license="$xray_source_dir/LICENSE"
[[ -f "$xray_source_license" && ! -L "$xray_source_license" ]] \
  || fail "Xray source module has no regular LICENSE"
cmp -- "$xray_license" "$xray_source_license" \
  || fail "Xray release license differs from exact source module license"

manifest_tmp="$work_dir/source-manifest.sha256"
(
  cd "$source_root"
  find . -type f -printf '%P\0' \
    | LC_ALL=C sort -z \
    | while IFS= read -r -d '' path; do
        sha256sum "$path"
      done
) > "$manifest_tmp"
install -m 0644 "$manifest_tmp" "$source_root/source-manifest.sha256"

LC_ALL=C tar --sort=name --mtime="@${source_date_epoch}" \
  --owner=0 --group=0 --numeric-owner --format=gnu \
  -C "$work_dir" -cf - "$(basename "$source_root")" | gzip -n > "$output"
chmod 0644 "$output"
