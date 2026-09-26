#!/usr/bin/env bash
# Scan the exact versioned source packages of the digest-bound Xray binary.
# govulncheck v1.6.0 cannot recover inline symbols from stripped ELF and falls back
# to every advisory in its modules. Use all imported source packages instead of
# an advisory allowlist; any affected imported package fails the release.
set -euo pipefail
umask 077
export LC_ALL=C GOENV=off GOWORK=off GOTOOLCHAIN=local GOFLAGS=''

readonly XRAY_COMMIT="52a412d9e2f5c2a5142b1b4e2ab3771dacb8b120"
readonly XRAY_MODULE_VERSION="v1.260327.1-0.20260908222543-52a412d9e2f5"
readonly XRAY_MODULE_SUM="h1:BsUC2sCXcdVCb09SUh1iWku0ci779t4bUIlKUor1ZRI="
readonly XRAY_GO_VERSION="go1.27.1"
readonly GOVULNCHECK_VERSION="v1.6.0"

fail() { printf 'Xray security verification failed: %s\n' "$*" >&2; exit 1; }
[[ $# -eq 1 ]] || fail "usage: $0 VERIFIED_XRAY_BINARY"
binary="$1"
[[ -f "$binary" && ! -L "$binary" ]] || fail "binary must be a regular non-symlink file"
for tool in go jq awk sort cmp mkdir mktemp sha256sum; do
  command -v "$tool" >/dev/null 2>&1 || fail "missing tool: $tool"
done
[[ "$(go env GOVERSION)" == "$XRAY_GO_VERSION" ]] \
  || fail "use the exact Xray compiler $XRAY_GO_VERSION; automatic toolchain switching is disabled"

work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
go version -m "$binary" > "$work/buildinfo.txt"
[[ "$(awk 'NR == 1 {print $NF}' "$work/buildinfo.txt")" == "$XRAY_GO_VERSION" ]] \
  || fail "binary compiler differs from $XRAY_GO_VERSION"
[[ "$(awk -F '\t' '$2 == "path" {print $3}' "$work/buildinfo.txt")" == github.com/xtls/xray-core/main ]] \
  || fail "binary main package is not Xray"
goos="$(awk -F '\t' '$2 == "build" && $3 ~ /^GOOS=/ {sub(/^GOOS=/, "", $3); print $3}' "$work/buildinfo.txt")"
goarch="$(awk -F '\t' '$2 == "build" && $3 ~ /^GOARCH=/ {sub(/^GOARCH=/, "", $3); print $3}' "$work/buildinfo.txt")"
[[ "$goos" == linux ]] || fail "only pinned Linux binaries are supported"
goarm=""
case "$goarch" in
  amd64) expected_sha=c4ae6798c38e0e5343b192406746333cd0ba7ff3eb984f8c4b9939dcb68c3f8a ;;
  arm64) expected_sha=c1defe42b6db958a97c5e049a02a00a4baaedaca7b51c1c229f0830e288acef5 ;;
  386) expected_sha=fefb0d8176949278f3428d62bf9c21bb1a038fc598cb147072a41d3932c195a6 ;;
  arm) expected_sha=ec346d24f45ebd28180d4342a24e0f27f98498777bcb1ab20250c78bf2c3e3a1; goarm=7 ;;
  *) fail "unsupported binary architecture: $goarch" ;;
esac
[[ "$(sha256sum "$binary" | awk '{print $1}')" == "$expected_sha" ]] \
  || fail "binary digest differs from the reviewed upstream asset"

metadata="$(go mod download -json "github.com/xtls/xray-core@$XRAY_COMMIT")" \
  || fail "cannot obtain exact corresponding source"
jq -e --arg version "$XRAY_MODULE_VERSION" --arg sum "$XRAY_MODULE_SUM" --arg commit "$XRAY_COMMIT" '
  .Path == "github.com/xtls/xray-core" and .Version == $version and .Sum == $sum and
  .Origin.Hash == $commit and (.Error == null or .Error == "")
' <<< "$metadata" >/dev/null || fail "source identity, module sum, or commit mismatch"
source_dir="$(jq -er '.Dir' <<< "$metadata")"
[[ -d "$source_dir" && ! -L "$source_dir" ]] || fail "unsafe source directory"
mkdir "$work/source"
# Keep Xray as a versioned dependency: govulncheck skips advisories for an
# unversioned main module. This also lets go mod verify hash Xray itself.
printf 'module proxyscene-xray-security-check\n\ngo 1.27.1\n\nrequire github.com/xtls/xray-core %s\n' \
  "$XRAY_MODULE_VERSION" > "$work/source/go.mod"
# go mod download trusts an existing extracted directory and cached ziphash.
# Recompute both ZIP and directory contents before loading any cached source.
go -C "$work/source" mod verify || fail "cached Xray source content mismatch"

# Re-resolve the exact build's complete imported-module set, including module
# checksums. An updated source graph must never stand in for an older binary.
awk -F '\t' '$2 == "dep" {print $3 "\t" $4 "\t" $5}' "$work/buildinfo.txt" \
  | sort -u > "$work/binary-dependencies.tsv"
[[ "$(wc -l < "$work/binary-dependencies.tsv")" -eq 47 ]] || fail "unexpected binary dependency count"
{ cat "$work/binary-dependencies.tsv"; printf '%s\t%s\t%s\n' github.com/xtls/xray-core "$XRAY_MODULE_VERSION" "$XRAY_MODULE_SUM"; } \
  | sort -u > "$work/expected-source-dependencies.tsv"
CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" GOARM="$goarm" GOFLAGS=-mod=mod \
  go -C "$work/source" list -deps \
    -f '{{with .Module}}{{if not .Main}}{{.Path}}{{"\t"}}{{.Version}}{{"\t"}}{{.Sum}}{{end}}{{end}}' github.com/xtls/xray-core/main \
  | awk 'NF' | sort -u > "$work/source-dependencies.tsv"
cmp -- "$work/expected-source-dependencies.tsv" "$work/source-dependencies.tsv" \
  || fail "source dependencies differ from the redistributed binary"
# The graph load may have downloaded additional packages. Verify every selected
# module's actual directory and ZIP against its checksum before analysis.
go -C "$work/source" mod verify || fail "cached dependency source content mismatch"

# Build the analyzer with the same Go language version it must type-check.
# Keep the analyzer native even when the scanned source targets another arch.
GOFLAGS='' GOOS="$(go env GOHOSTOS)" GOARCH="$(go env GOHOSTARCH)" GOARM='' \
  GOBIN="$work/tools" go install "golang.org/x/vuln/cmd/govulncheck@$GOVULNCHECK_VERSION"
printf 'Conservative stripped-binary advisory inventory (module precision):\n'
binary_status=0
"$work/tools/govulncheck" -mode=binary "$binary" || binary_status=$?
[[ "$binary_status" -eq 0 || "$binary_status" -eq 3 ]] \
  || fail "binary inventory failed with exit $binary_status"
printf 'Authoritative matching-source check: every imported package, no advisory exceptions.\n'
CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" GOARM="$goarm" GOFLAGS=-mod=readonly \
  "$work/tools/govulncheck" -C "$work/source" -scan=package github.com/xtls/xray-core/main
printf 'Xray exact-source imported-package security check passed: %s/%s\n' "$goos" "$goarch"
