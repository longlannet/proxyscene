#!/usr/bin/env bash
# Exercise the real gate against copied, corrupted Go module caches, offline.
set -euo pipefail
umask 077
export LC_ALL=C GOENV=off GOWORK=off GOTOOLCHAIN=local GOFLAGS=''

gate="${1:?usage: $0 ABSOLUTE_GATE_SCRIPT ABSOLUTE_XRAY_BINARY}"
binary="${2:?usage: $0 ABSOLUTE_GATE_SCRIPT ABSOLUTE_XRAY_BINARY}"
module=github.com/xtls/xray-core
version=v1.260327.1-0.20260908222543-52a412d9e2f5
commit=52a412d9e2f5c2a5142b1b4e2ab3771dacb8b120
expected_sum='h1:BsUC2sCXcdVCb09SUh1iWku0ci779t4bUIlKUor1ZRI='
reference_cache="$(go env GOMODCACHE)"
[[ "$reference_cache" == /* && "$gate" == /* && "$binary" == /* ]] \
  || { printf 'absolute paths are required\n' >&2; exit 1; }
reference_source="$reference_cache/$module@$version"
reference_download="$reference_cache/cache/download/$module/@v"
[[ -d "$reference_source" && ! -L "$reference_source" ]]
work="$(mktemp -d)"
evidence="${TEST_LOG_DIR:-$work/evidence}"
mkdir -p -- "$evidence"
cleanup() {
  chmod -R u+w -- "$work"
  rm -rf -- "$work"
}
trap cleanup EXIT

# Commit lookup must resolve through a file proxy, even though the versioned
# module and checksum metadata are already in each copied cache. All fallback
# requests also use the original cache as a read-only file proxy; no network.
proxy="$work/proxy"
mkdir -p "$proxy/$module/@v"
cp -- "$reference_download/$version.info" "$proxy/$module/@v/$commit.info"
export GOPROXY="file://$proxy,file://$reference_cache/cache/download" GOSUMDB=off

for kind in directory zip; do
  cache="$work/$kind-cache"
  download="$cache/cache/download/$module/@v"
  mkdir -p "$download" "$cache/github.com/xtls"
  cp -R -- "$reference_source" "$cache/$module@$version"
  for suffix in info mod zip ziphash; do
    cp -- "$reference_download/$version.$suffix" "$download/$version.$suffix"
  done
  if [[ "$kind" == directory ]]; then
    chmod u+w "$cache/$module@$version/main/main.go"
    printf '\n// copied-cache integrity negative test\n' >> "$cache/$module@$version/main/main.go"
  else
    # A trailing ZIP comment does not change Go's canonical module hash.
    # Modify an actual member, preserving the cached .ziphash and extracted Dir.
    python3 - "$download/$version.zip" "$module@$version/main/main.go" <<'PY'
import os
import sys
import zipfile

path, target = sys.argv[1:]
temporary = path + ".modified"
changed = False
with zipfile.ZipFile(path) as original, zipfile.ZipFile(temporary, "w") as modified:
    for member in original.infolist():
        content = original.read(member.filename)
        if member.filename == target:
            content += b"\n// copied-cache integrity negative test\n"
            changed = True
        modified.writestr(member, content)
if not changed:
    raise SystemExit("expected Go source member absent from fixture ZIP")
os.replace(temporary, path)
PY
  fi

  # Demonstrate the old metadata-only checks still pass with corrupted content.
  GOMODCACHE="$cache" go mod download -json "$module@$commit" > "$evidence/$kind-download.json"
  jq -e --arg module "$module" --arg version "$version" --arg sum "$expected_sum" --arg commit "$commit" '
    .Path == $module and .Version == $version and .Sum == $sum and
    .Origin.Hash == $commit and (.Error == null or .Error == "")
  ' "$evidence/$kind-download.json" >/dev/null
  cmp -- "$reference_download/$version.ziphash" "$download/$version.ziphash"

  status=0
  GOMODCACHE="$cache" bash "$gate" "$binary" > "$evidence/$kind-gate.log" 2>&1 || status=$?
  [[ "$status" -ne 0 ]] || { printf 'gate accepted corrupted %s content\n' "$kind" >&2; exit 1; }
  if ! grep -Fq 'cached Xray source content mismatch' "$evidence/$kind-gate.log"; then
    cat "$evidence/$kind-gate.log" >&2
    printf 'cache regression failed for an unexpected reason\n' >&2
    exit 1
  fi
  if [[ "$kind" == directory ]]; then
    grep -Fq 'dir has been modified' "$evidence/$kind-gate.log"
  else
    grep -Fq 'zip has been modified' "$evidence/$kind-gate.log"
  fi
  printf '%s corruption: unchanged pinned metadata; actual gate rejected content, exit %s\n' "$kind" "$status"
done

cleanup
trap - EXIT
[[ ! -e "$work" ]]
printf 'Xray copied-cache corruption regressions passed; fixture cache cleanup verified\n'
