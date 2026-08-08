#!/usr/bin/env bash
set -euo pipefail

umask 077
export LC_ALL=C

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

fail() {
  printf 'Xray license generation failed: %s\n' "$*" >&2
  exit 1
}

[[ $# -eq 4 ]] || fail "usage: $0 XRAY_BINARY OUTPUT XRAY_VERSION XRAY_COMMIT"

xray_binary="$1"
output="$2"
xray_version="$3"
xray_commit="$4"

for tool in go jq find sort mktemp install stat; do
  command -v "$tool" >/dev/null 2>&1 || fail "missing required tool: $tool"
done

[[ -f "$xray_binary" && ! -L "$xray_binary" ]] \
  || fail "Xray binary must be a regular non-symlink file: $xray_binary"
[[ "$xray_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] \
  || fail "invalid Xray version: $xray_version"
[[ "$xray_commit" =~ ^[0-9a-f]{40}$ ]] \
  || fail "invalid Xray commit: $xray_commit"

work_dir="$(mktemp -d)"
trap 'rm -rf -- "$work_dir"' EXIT
deps="$work_dir/dependencies.tsv"
generated="$work_dir/THIRD_PARTY_LICENSES-Xray"
normalized="$work_dir/THIRD_PARTY_LICENSES-Xray.normalized"

go version -m "$xray_binary" \
  | awk -F '\t' '$2 == "dep" { print $3 "\t" $4 "\t" $5 }' \
  | LC_ALL=C sort -u > "$deps"

[[ -s "$deps" ]] || fail "Xray binary contains no Go dependency build information"
while IFS=$'\t' read -r module version sum extra; do
  [[ -z "${extra:-}" ]] || fail "malformed dependency record for $module"
  [[ "$module" =~ ^[A-Za-z0-9][A-Za-z0-9._/+!~-]*$ ]] \
    || fail "unsafe module path in Xray build information: $module"
  [[ "$version" =~ ^v[0-9A-Za-z.+~-]+$ ]] \
    || fail "unsafe module version for $module: $version"
  [[ "$sum" =~ ^h1:[A-Za-z0-9+/=]+$ ]] \
    || fail "missing or invalid module sum for $module@$version"
done < "$deps"

{
  printf 'Third-Party Licenses for Xray-core %s\n' "$xray_version"
  printf '%s\n\n' '==============================================='
  printf 'Xray source: https://github.com/XTLS/Xray-core/tree/%s\n' "$xray_version"
  printf 'Xray commit: %s\n' "$xray_commit"
  printf '%s\n' 'The Xray-core project license is distributed separately as LICENSE-Xray.'
  printf '%s\n\n' 'This file covers Go modules linked into the redistributed Xray executable.'
} > "$generated"

while IFS=$'\t' read -r module version expected_sum; do
  metadata="$(GOENV=off GOWORK=off go mod download -json "${module}@${version}")" \
    || fail "cannot download $module@$version"
  downloaded_path="$(jq -er '.Path' <<< "$metadata")" \
    || fail "download metadata has no Path for $module@$version"
  downloaded_version="$(jq -er '.Version' <<< "$metadata")" \
    || fail "download metadata has no Version for $module@$version"
  downloaded_sum="$(jq -er '.Sum' <<< "$metadata")" \
    || fail "download metadata has no Sum for $module@$version"
  module_dir="$(jq -er '.Dir' <<< "$metadata")" \
    || fail "download metadata has no Dir for $module@$version"
  [[ "$downloaded_path" == "$module" && "$downloaded_version" == "$version" ]] \
    || fail "download identity mismatch for $module@$version"
  [[ "$downloaded_sum" == "$expected_sum" ]] \
    || fail "download sum mismatch for $module@$version"
  [[ "$module_dir" == /* && -d "$module_dir" && ! -L "$module_dir" ]] \
    || fail "downloaded module directory is unsafe for $module@$version"

  mapfile -d '' -t license_files < <(
    find "$module_dir" -mindepth 1 -type f \
      \( -iname 'LICENSE' -o -iname 'LICENSE.*' -o -iname 'LICENSE-*' \
         -o -iname 'COPYING' -o -iname 'COPYING.*' -o -iname 'COPYING-*' \
         -o -iname 'NOTICE' -o -iname 'NOTICE.*' -o -iname 'NOTICE-*' \
         -o -iname 'PATENTS' -o -iname 'PATENTS.*' -o -iname 'PATENTS-*' \
         -o -iname 'COPYRIGHT' -o -iname 'COPYRIGHT.*' -o -iname 'COPYRIGHT-*' \
         -o -iname 'AUTHORS' -o -iname 'AUTHORS.*' -o -iname 'AUTHORS-*' \
         -o -iname 'CONTRIBUTORS' -o -iname 'CONTRIBUTORS.*' -o -iname 'CONTRIBUTORS-*' \) \
      -print0 | LC_ALL=C sort -z
  )
  (( ${#license_files[@]} > 0 )) \
    || fail "no top-level license or notice file found for $module@$version"

  {
    printf '\n\n%s %s\n' "$module" "$version"
    printf 'Module sum: %s\n' "$expected_sum"
    printf 'Source: https://pkg.go.dev/%s@%s\n' "$module" "$version"
    printf '%s\n' '-----------------------------------------------'
    for license_file in "${license_files[@]}"; do
      [[ -f "$license_file" && ! -L "$license_file" ]] \
        || fail "license path changed while reading: $license_file"
      printf '\n[%s]\n\n' "${license_file#"$module_dir"/}"
      command cat -- "$license_file"
      printf '\n'
    done
  } >> "$generated"
done < "$deps"

if grep -Fq $'github.com/sagernet/sing\t' "$deps" || \
  grep -Fq $'github.com/sagernet/sing-shadowsocks\t' "$deps"; then
  [[ -f "$ROOT/LICENSE-GPL-3.0" && ! -L "$ROOT/LICENSE-GPL-3.0" ]] \
    || fail "missing canonical GPL-3.0 license text"
  {
    printf '\n\nGNU General Public License v3.0\n'
    printf '%s\n\n' '-----------------------------------------------'
    command cat -- "$ROOT/LICENSE-GPL-3.0"
    printf '\n'
  } >> "$generated"
fi

# Upstream license files may use CRLF or contain trailing horizontal whitespace.
# Normalize only line endings and trailing whitespace so repository and release
# checks stay reproducible without changing the license text.
awk '
  {
    sub(/\r$/, "")
    sub(/[ \t]+$/, "")
    lines[NR] = $0
  }
  END {
    last = NR
    while (last > 0 && lines[last] == "") {
      last--
    }
    for (i = 1; i <= last; i++) {
      print lines[i]
    }
  }
' "$generated" > "$normalized"

install -m 0644 "$normalized" "$output"
[[ "$(stat -c '%a' "$output")" == 644 ]] || fail "output mode is not 0644"
