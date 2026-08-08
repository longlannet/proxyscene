#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
export PROXYSCENE_INSTALL_TESTING=1
umask 000
# shellcheck disable=SC1091
source "$ROOT/install.sh"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_eq() {
  local expected="$1" actual="$2" label="$3"
  [[ "$actual" == "$expected" ]] || fail "$label: expected '$expected', got '$actual'"
}

assert_rejects_path() {
  local path="$1"
  if path_is_normalized_absolute "$path"; then
    fail "unsafe path accepted: $path"
  fi
}

assert_fails() {
  local label="$1"
  shift
  if ("$@") >/dev/null 2>&1; then
    fail "$label: command unexpectedly succeeded"
  fi
}

assert_input_fails() {
  local label="$1" input="$2"
  shift 2
  if printf '%s\n' "$input" | "$@" >/dev/null 2>&1; then
    fail "$label: input unexpectedly accepted"
  fi
}

assert_eq "$TRUSTED_ROOT_PATH" "$PATH" "trusted PATH after sourcing"
assert_eq 0077 "$(umask)" "installer umask after sourcing"
assert_eq "$TRUSTED_ROOT_PATH" "$(
  PATH="/tmp/proxyscene-attacker-bin:/usr/bin:/bin" PROXYSCENE_INSTALL_TESTING=1 \
    /bin/bash -c 'source "$1"; printf "%s" "$PATH"' _ "$ROOT/install.sh"
)" "caller PATH replacement"

assert_eq amd64 "$(arch_release_for_machine x86_64)" "x86_64 mapping"
assert_eq arm64 "$(arch_release_for_machine aarch64)" "aarch64 mapping"
assert_eq 386 "$(arch_release_for_machine i686)" "i686 mapping"
assert_eq armv7 "$(arch_release_for_machine armv7l)" "armv7 mapping"
if arch_release_for_machine riscv64 >/dev/null 2>&1; then
  fail "unsupported architecture accepted"
fi

assert_eq 64 "$(xray_asset_arch_for_release_arch amd64)" "amd64 Xray asset"
assert_eq arm64-v8a "$(xray_asset_arch_for_release_arch arm64)" "arm64 Xray asset"
assert_eq 32 "$(xray_asset_arch_for_release_arch 386)" "386 Xray asset"
assert_eq arm32-v7a "$(xray_asset_arch_for_release_arch armv7)" "armv7 Xray asset"
assert_eq "$XRAY_SHA256_ARM64" "$(xray_sha256_for_release_arch arm64)" "arm64 pinned SHA256"
assert_eq "$GO_SHA256_386" "$(go_tarball_sha256_for_arch 386)" "386 Go SHA256"
assert_eq "$GO_SHA256_AMD64" "$(go_tarball_sha256_for_arch amd64)" "amd64 Go SHA256"
assert_eq "$GO_SHA256_ARM64" "$(go_tarball_sha256_for_arch arm64)" "arm64 Go SHA256"
assert_eq "$GO_SHA256_ARMV6L" "$(go_tarball_sha256_for_arch armv6l)" "armv6l Go SHA256"
assert_eq 65203409 "$(go_tarball_size_for_arch 386)" "386 Go archive size"
assert_eq 66879095 "$(go_tarball_size_for_arch amd64)" "amd64 Go archive size"
assert_eq 63759990 "$(go_tarball_size_for_arch arm64)" "arm64 Go archive size"
assert_eq 65406756 "$(go_tarball_size_for_arch armv6l)" "armv6l Go archive size"
assert_fails "unsupported Go archive architecture" go_tarball_sha256_for_arch riscv64
assert_eq \
  "https://github.com/XTLS/Xray-core/releases/download/v26.3.27/Xray-linux-arm64-v8a.zip" \
  "$(xray_download_url_for_arch arm64)" \
  "fixed Xray URL"

upper_sha="$(printf '%s' "$XRAY_SHA256_AMD64" | tr '[:lower:]' '[:upper:]')"
assert_eq "$XRAY_SHA256_AMD64" "$(normalize_sha256 "$upper_sha")" "uppercase SHA256 normalization"
if normalize_sha256 not-a-digest >/dev/null 2>&1; then
  fail "invalid SHA256 accepted"
fi

assert_eq 1.26.5 "$(parse_go_version_output 'go version go1.26.5 linux/amd64')" "Go version parsing"
assert_fails "development Go version" parse_go_version_output 'go version devel go1.27-deadbeef linux/amd64'
go_version_meets_requirement 1.27.0 1.26.5 || fail "newer Go did not meet explicit requirement"
assert_fails "Go below explicit requirement" go_version_meets_requirement 1.26.5 1.27.0
go_version_matches_exactly 1.26.5 1.26.5 || fail "exact downloaded Go version rejected"
assert_fails "downloaded Go version mismatch" go_version_matches_exactly 1.26.6 1.26.5

assert_eq "$DEFAULT_XRAY_VERSION" \
  "$(xray_marker_for_source official "$DEFAULT_XRAY_VERSION")" \
  "official Xray version marker"
assert_eq "custom-sha256:$XRAY_SHA256_AMD64" \
  "$(xray_marker_for_source custom "$upper_sha")" \
  "custom Xray provenance marker"
assert_eq "existing-sha256:$XRAY_SHA256_ARM64" \
  "$(xray_marker_for_source existing "$XRAY_SHA256_ARM64")" \
  "existing Xray provenance marker"
assert_fails "unknown Xray marker source" xray_marker_for_source unknown "$XRAY_SHA256_AMD64"

ownership_root="$(mktemp -d)"
ownership_bin="$ownership_root/custom/proxyscene"
ownership_default_bin="$ownership_root/default/proxyscene"
mkdir -p "$(dirname "$ownership_bin")" "$(dirname "$ownership_default_bin")"
printf 'binary\n' > "$ownership_bin"
printf 'binary\n' > "$ownership_default_bin"
chmod 700 "$ownership_bin"
chmod 700 "$ownership_default_bin"

validate_install_bin_for_test() (
  CORE_DIR="$1"
  DEFAULT_INSTALL_BIN="$2"
  INSTALL_BIN="$3"
  LEGACY_UNIT_MATCH_FOR_TEST="${4:-0}"
  validate_trusted_directory_chain() { :; }
  validate_root_regular_file() { :; }
  legacy_unit_pair_matches() { [[ "$LEGACY_UNIT_MATCH_FOR_TEST" == "1" ]]; }
  validate_install_bin
)

assert_fails "unclaimed custom install binary" install_bin_is_owned "$ownership_root" "$ownership_bin"
assert_fails "unclaimed default install binary" \
  validate_install_bin_for_test "$ownership_root" "$ownership_default_bin" "$ownership_default_bin"
assert_fails "unclaimed custom install binary through path validation" \
  validate_install_bin_for_test "$ownership_root" "$ownership_default_bin" "$ownership_bin"
printf '由 proxyscene 管理\n' > "$ownership_root/.managed-by-proxyscene"
chmod 600 "$ownership_root/.managed-by-proxyscene"
assert_fails "legacy CoreDir marker cannot claim a custom binary" \
  validate_install_bin_for_test "$ownership_root" "$ownership_default_bin" "$ownership_bin"
assert_fails "legacy CoreDir marker without matching units cannot claim default binary" \
  validate_install_bin_for_test "$ownership_root" "$ownership_default_bin" "$ownership_default_bin"
validate_install_bin_for_test "$ownership_root" "$ownership_default_bin" "$ownership_default_bin" 1 \
  || fail "legacy managed CoreDir with exact units did not establish default binary ownership"

legacy_unit_root="$ownership_root/units"
mkdir -p "$legacy_unit_root"
legacy_main="$legacy_unit_root/proxyscene-legacy.service"
legacy_restore="$legacy_unit_root/proxyscene-legacy-restore.service"
printf '[Service]\nExecStart=%s run -config %s\n' \
  "$(systemd_quote_for_installer "$ownership_root/xray")" \
  "$(systemd_quote_for_installer "$ownership_root/config.json")" > "$legacy_main"
printf '[Service]\nExecStart=%s boot-restore\n' \
  "$(systemd_quote_for_installer "$ownership_default_bin")" > "$legacy_restore"
chmod 600 "$legacy_main" "$legacy_restore"
legacy_unit_pair_for_test() (
  # shellcheck disable=SC2030 # isolated locator overrides for the sourced helper
  local SYSTEMD_UNIT_DIR="$1" SYSTEMD_SERVICE=proxyscene-legacy.service RESTORE_SERVICE=proxyscene-legacy-restore.service
  validate_root_regular_file() {
    [[ ! -L "$1" && -f "$1" ]] || fatal "$2 必须是非符号链接常规文件：$1"
  }
  legacy_unit_pair_matches "$2" "$3"
)
legacy_unit_pair_for_test "$legacy_unit_root" "$ownership_root" "$ownership_default_bin" \
  || fail "exact legacy unit pair was rejected"
assert_fails "legacy unit pair bound to another CoreDir" legacy_unit_pair_for_test \
  "$legacy_unit_root" /opt/other "$ownership_default_bin"
rm -f "$legacy_restore"
ln -s "$legacy_main" "$legacy_restore"
assert_fails "legacy symlink unit" legacy_unit_pair_for_test \
  "$legacy_unit_root" "$ownership_root" "$ownership_default_bin"
# shellcheck disable=SC2031 # the earlier locator overrides were confined to a subshell
printf '{"version":1,"core_dir":"%s","install_bin":"%s","systemd_service":"%s","restore_service":"%s"}\n' \
  "$ownership_root" "$ownership_default_bin" "$SYSTEMD_SERVICE" "$RESTORE_SERVICE" \
  > "$ownership_root/installation-ownership.json"
chmod 600 "$ownership_root/installation-ownership.json"
install_bin_is_owned "$ownership_root" "$ownership_default_bin" \
  || fail "matching default installation ownership was rejected"
validate_install_bin_for_test "$ownership_root" "$ownership_default_bin" "$ownership_default_bin" \
  || fail "owned default install binary was rejected by path validation"
assert_fails "mismatched custom binary ownership" install_bin_is_owned "$ownership_root" "$ownership_bin"
assert_fails "mismatched custom install binary through path validation" \
  validate_install_bin_for_test "$ownership_root" "$ownership_default_bin" "$ownership_bin"
# shellcheck disable=SC2031 # the earlier locator overrides were confined to a subshell
printf '{"version":1,"core_dir":"%s","install_bin":"%s","systemd_service":"%s","restore_service":"%s"}\n' \
  "$ownership_root" "$ownership_bin" "$SYSTEMD_SERVICE" "$RESTORE_SERVICE" \
  > "$ownership_root/installation-ownership.json"
install_bin_is_owned "$ownership_root" "$ownership_bin" \
  || fail "matching custom installation ownership was rejected"
validate_install_bin_for_test "$ownership_root" "$ownership_default_bin" "$ownership_bin" \
  || fail "owned custom install binary was rejected by path validation"

host_ownership_for_test() (
  # shellcheck disable=SC2030 # isolated override for the sourced installer helper
  local HOST_OWNERSHIP_PATH="$1"
  CORE_DIR="$2"
  INSTALL_BIN="$3"
  SYSTEMD_SERVICE="$4"
  RESTORE_SERVICE="$5"
  validate_root_regular_file() {
    [[ ! -L "$1" && -f "$1" ]] || fatal "$2 必须是非符号链接常规文件：$1"
  }
  validate_host_ownership_for_install
)

host_record="$ownership_root/host-ownership.json"
printf '{"version":1,"core_dir":"%s","install_bin":"%s","systemd_service":"%s","restore_service":"%s"}\n' \
  "$ownership_root" "$ownership_bin" "proxyscene-custom.service" "proxyscene-custom-restore.service" \
  > "$host_record"
chmod 600 "$host_record"
host_ownership_for_test \
  "$host_record" "$ownership_root" "$ownership_bin" \
  proxyscene-custom.service proxyscene-custom-restore.service \
  || fail "matching host ownership was rejected"
assert_fails "host ownership core mismatch" host_ownership_for_test \
  "$host_record" /opt/other "$ownership_bin" \
  proxyscene-custom.service proxyscene-custom-restore.service
assert_fails "host ownership unit mismatch" host_ownership_for_test \
  "$host_record" "$ownership_root" "$ownership_bin" \
  proxyscene-other.service proxyscene-custom-restore.service
printf '{"version":1,"core_dir":"%s","install_bin":"%s","systemd_service":"%s","restore_service":"%s","extra":true}\n' \
  "$ownership_root" "$ownership_bin" "proxyscene-custom.service" "proxyscene-custom-restore.service" \
  > "$host_record"
assert_fails "host ownership unknown field" host_ownership_for_test \
  "$host_record" "$ownership_root" "$ownership_bin" \
  proxyscene-custom.service proxyscene-custom-restore.service
printf '{}\n{}\n' > "$host_record"
assert_fails "host ownership multiple JSON values" host_ownership_for_test \
  "$host_record" "$ownership_root" "$ownership_bin" \
  proxyscene-custom.service proxyscene-custom-restore.service
printf '{broken\n' > "$host_record"
assert_fails "host ownership malformed JSON" host_ownership_for_test \
  "$host_record" "$ownership_root" "$ownership_bin" \
  proxyscene-custom.service proxyscene-custom-restore.service
printf '{"version":1,"core_dir":"%s","install_bin":"%s","systemd_service":"%s","restore_service":"%s"}\n' \
  "$ownership_root" "$ownership_bin" "proxyscene-custom.service" "proxyscene-custom-restore.service" \
  > "$host_record"
chmod 644 "$host_record"
assert_fails "host ownership non-root-only mode" host_ownership_for_test \
  "$host_record" "$ownership_root" "$ownership_bin" \
  proxyscene-custom.service proxyscene-custom-restore.service
chmod 600 "$host_record"
mv "$host_record" "$host_record.real"
ln -s "$host_record.real" "$host_record"
assert_fails "host ownership symlink" host_ownership_for_test \
  "$host_record" "$ownership_root" "$ownership_bin" \
  proxyscene-custom.service proxyscene-custom-restore.service
rm -f "$host_record" "$host_record.real"
host_ownership_for_test \
  "$host_record" "$ownership_root" "$ownership_bin" \
  proxyscene-custom.service proxyscene-custom-restore.service \
  || fail "missing host ownership should allow first claim"

rendered_ownership="$ownership_root/rendered-installation-ownership.json"
(
  CORE_DIR="$ownership_root"
  INSTALL_BIN="$ownership_bin"
  SYSTEMD_SERVICE=proxyscene-rendered.service
  RESTORE_SERVICE=proxyscene-rendered-restore.service
  render_installation_ownership
) > "$rendered_ownership"
ownership_record_matches \
  "$rendered_ownership" "$ownership_root" "$ownership_bin" \
  proxyscene-rendered.service proxyscene-rendered-restore.service \
  || fail "rendered installation ownership did not bind all locators"

installation_ownership_transaction_for_test() (
  local root="$1" outcome="$2" go_toolchain_dir
  CORE_DIR="$root/core"
  INSTALL_BIN="$root/bin/proxyscene"
  TRANSACTION_TMP_ROOT="$root/transaction-tmp"
  SYSTEMD_SERVICE=proxyscene-transaction.service
  RESTORE_SERVICE=proxyscene-transaction-restore.service
  SKIP_MANAGER_INIT=1
  mkdir -p "$CORE_DIR" "$(dirname "$INSTALL_BIN")"
  printf 'manager\n' > "$INSTALL_BIN"
  chmod 700 "$INSTALL_BIN"
  validate_trusted_directory_chain() { :; }
  validate_root_regular_file() { [[ ! -L "$1" && -f "$1" ]]; }
  chown() { :; }

  if [[ "$outcome" == "restore" ]]; then
    printf 'prior ownership record\n' > "$CORE_DIR/installation-ownership.json"
    chmod 600 "$CORE_DIR/installation-ownership.json"
  fi
  begin_transaction
  stage_installation_ownership
  prepare_executable_go_toolchain_dir
  go_toolchain_dir="$GO_TOOLCHAIN_DIR"
  [[ "${go_toolchain_dir%/*}" == "$CORE_DIR" ]] \
    || fail "executable Go toolchain was not staged directly under CoreDir"
  [[ "$go_toolchain_dir" != "$TX_DIR" && "$go_toolchain_dir" != "$TX_DIR/"* ]] \
    || fail "executable Go toolchain was staged below the potentially noexec transaction root"
  assert_eq 700 "$(stat -c '%a' "$go_toolchain_dir")" \
    "executable Go toolchain directory mode"
  mkdir "$go_toolchain_dir/go"
  ownership_record_matches \
    "$CORE_DIR/installation-ownership.json" "$CORE_DIR" "$INSTALL_BIN" \
    "$SYSTEMD_SERVICE" "$RESTORE_SERVICE" \
    || fail "staged installation ownership did not bind all locators"
  assert_eq 600 "$(stat -c '%a' "$CORE_DIR/installation-ownership.json")" \
    "staged installation ownership mode"

  case "$outcome" in
    commit)
      commit_files_then_init
      install_bin_is_owned "$CORE_DIR" "$INSTALL_BIN" \
        || fail "SKIP_MANAGER_INIT commit was not accepted on installer retry"
      ;;
    restore)
      rollback_transaction
      assert_eq "prior ownership record" \
        "$(tr -d '\r\n' < "$CORE_DIR/installation-ownership.json")" \
        "installation ownership rollback restore"
      ;;
    remove)
      rollback_transaction
      [[ ! -e "$CORE_DIR/installation-ownership.json" ]] \
        || fail "installation ownership rollback left a newly created record"
      ;;
    *)
      fail "unknown ownership transaction outcome: $outcome"
      ;;
  esac
  [[ ! -e "$go_toolchain_dir" && ! -L "$go_toolchain_dir" ]] \
    || fail "temporary Go toolchain remained after $outcome"
  [[ -z "$GO_TOOLCHAIN_DIR" ]] || fail "temporary Go toolchain locator remained after $outcome"
)

ownership_tx_root="$(mktemp -d)"
trap 'rm -rf "$ownership_tx_root"' EXIT
installation_ownership_transaction_for_test "$ownership_tx_root/commit" commit
installation_ownership_transaction_for_test "$ownership_tx_root/restore" restore
installation_ownership_transaction_for_test "$ownership_tx_root/remove" remove
rm -rf "$ownership_tx_root"
trap - EXIT

downloaded_go_toolchain_for_test() (
  local root="$1" fixture_root saved_toolchain
  local CORE_DIR TX_DIR GO_VERSION GO_BIN GO_TOOLCHAIN_DIR GO_FIXTURE_ARCHIVE GO_TARBALL_SHA256
  CORE_DIR="$root/core"
  # shellcheck disable=SC2030 # isolated transaction locator for the sourced installer helper
  TX_DIR="$root/noexec-transaction"
  GO_VERSION="$DEFAULT_GO_VERSION"
  GO_BIN=""
  GO_TOOLCHAIN_DIR=""
  fixture_root="$root/archive-root"
  GO_FIXTURE_ARCHIVE="$root/fake-go.tar.gz"
  mkdir -p "$CORE_DIR" "$TX_DIR" "$fixture_root/go/bin"
  install -m 0700 /dev/stdin "$fixture_root/go/bin/go" <<'FAKE_GO'
#!/bin/sh
printf 'go version go1.26.5 linux/amd64\n'
FAKE_GO
  tar -czf "$GO_FIXTURE_ARCHIVE" -C "$fixture_root" go
  GO_TARBALL_SHA256="$(sha256_file "$GO_FIXTURE_ARCHIVE")"
  validate_trusted_directory_chain() { :; }
  fetch_https() { cp "$GO_FIXTURE_ARCHIVE" "$2"; }

  install_go_toolchain
  saved_toolchain="$GO_TOOLCHAIN_DIR"
  [[ "${GO_BIN%/go/bin/go}" == "$CORE_DIR"/.proxyscene-go-toolchain.* ]] \
    || fail "downloaded Go executable was not installed below CoreDir"
  [[ "$GO_BIN" != "$TX_DIR/"* ]] \
    || fail "downloaded Go executable remained below the potentially noexec transaction root"
  assert_eq "$DEFAULT_GO_VERSION" "$(current_go_version "$GO_BIN")" \
    "downloaded temporary Go execution"
  cleanup_executable_go_toolchain || fail "downloaded Go cleanup failed"
  [[ ! -e "$saved_toolchain" && ! -L "$saved_toolchain" ]] \
    || fail "downloaded Go toolchain remained after cleanup"
)

go_fixture_root="$(mktemp -d "$ROOT/.proxyscene-build.go-toolchain-test.XXXXXX")"
trap 'rm -rf "$go_fixture_root"' EXIT
downloaded_go_toolchain_for_test "$go_fixture_root"
rm -rf "$go_fixture_root"
trap - EXIT

hostile_tmp_root="$(mktemp -d)"
trap 'rm -rf "$hostile_tmp_root"' EXIT
# shellcheck disable=SC2031 # the downloaded-toolchain locator override was confined to a subshell
hostile_tmp_for_test() (
  local root="$1"
  local attacker_parent="$root/attacker-parent"
  local attacker_moved="$root/attacker-parent-moved"
  local fixed_root="$root/fixed-transaction-root"
  mkdir -m 0700 "$attacker_parent"
  chmod 0777 "$attacker_parent"
  TMPDIR="$attacker_parent"
  TRANSACTION_TMP_ROOT="$fixed_root"

  validate_trusted_directory_chain() {
    case "$1" in
      "$root"|"$fixed_root"|"$fixed_root"/transaction.*) ;;
      *) fail "transaction used an unexpected directory chain: $1" ;;
    esac
    [[ ! -L "$1" ]] || fail "transaction directory chain contains a symlink: $1"
  }

  begin_transaction
  [[ "$TX_DIR" == "$fixed_root"/transaction.* ]] \
    || fail "transaction inherited hostile TMPDIR: $TX_DIR"
  assert_eq 700 "$(stat -c '%a' "$TX_DIR")" "fixed transaction directory mode"

  mv -- "$attacker_parent" "$attacker_moved"
  ln -s -- "$attacker_moved" "$attacker_parent"
  [[ -d "$TX_DIR" && ! -L "$TX_DIR" ]] \
    || fail "replacing hostile TMPDIR affected the fixed transaction directory"
  commit_transaction
)
hostile_tmp_for_test "$hostile_tmp_root"
rm -rf "$hostile_tmp_root"
trap - EXIT

rm -rf "$ownership_root"
manifest_dir="$(mktemp -d)"
trap 'rm -rf "$manifest_dir"' EXIT
printf '%s  manager.tar.gz\n' "$XRAY_SHA256_AMD64" > "$manifest_dir/checksums.txt"
assert_eq "$XRAY_SHA256_AMD64" \
  "$(manifest_sha_for_asset "$manifest_dir/checksums.txt" manager.tar.gz)" \
  "single manifest entry"
printf '%s  manager.tar.gz\n' "$XRAY_SHA256_ARM64" >> "$manifest_dir/checksums.txt"
if manifest_sha_for_asset "$manifest_dir/checksums.txt" manager.tar.gz >/dev/null 2>&1; then
  fail "duplicate manifest entry accepted"
fi
rm -rf "$manifest_dir"
trap - EXIT

path_is_normalized_absolute /opt/proxyscene || fail "safe path rejected"
assert_rejects_path opt/proxyscene
assert_rejects_path /opt/proxyscene/
assert_rejects_path /opt//proxyscene
assert_rejects_path /opt/a/../proxyscene
assert_rejects_path $'/opt/proxy scene'
# shellcheck disable=SC2016 # $1 is intentionally expanded by the child shell.
assert_fails "arbitrary install target" bash -c \
  'export PROXYSCENE_INSTALL_TESTING=1 PROXYSCENE_SWITCH_BIN=/etc/passwd; source "$1"; validate_install_bin' \
  _ "$ROOT/install.sh"

assert_eq online "$(install_mode_for_flag 0)" "default install mode"
assert_eq offline "$(install_mode_for_flag 1)" "explicit offline mode"
# shellcheck disable=SC2016 # $1 is intentionally expanded by the child shell.
assert_fails "positional secret argument" bash -c \
  'export PROXYSCENE_INSTALL_TESTING=1; source "$1"; parse_args "vless://secret"' \
  _ "$ROOT/install.sh"
validate_boolean_flag TEST_FLAG 0
validate_boolean_flag TEST_FLAG 1
# shellcheck disable=SC2016 # $1 is intentionally expanded by the child shell.
assert_fails "invalid boolean" bash -c \
  'export PROXYSCENE_INSTALL_TESTING=1; source "$1"; validate_boolean_flag TEST_FLAG yes' \
  _ "$ROOT/install.sh"
# shellcheck disable=SC2016 # $1 is intentionally expanded by the child shell.
assert_fails "invalid Go checksum" bash -c \
  'export PROXYSCENE_INSTALL_TESTING=1 GO_TARBALL_SHA256=bad; source "$1"; validate_common_inputs' \
  _ "$ROOT/install.sh"
# shellcheck disable=SC2016 # child-shell variables are intentionally literal here.
assert_fails "custom Go version without checksum" bash -c \
  'export PROXYSCENE_INSTALL_TESTING=1 GO_VERSION=1.26.6; source "$1"; validate_common_inputs' \
  _ "$ROOT/install.sh"
GO_VERSION=1.26.6 GO_TARBALL_SHA256="$GO_SHA256_AMD64" validate_common_inputs

permission_root="$(mktemp -d)"
core_mode_for_test="$({
  umask 000
  CORE_DIR="$permission_root/core"
  TX_DIR="$permission_root/tx"
  mkdir -m 0700 "$TX_DIR"
  validate_core_dir() { :; }
  validate_trusted_directory_chain() { :; }
  transactional_replace() { :; }
  ensure_core_dir
  stat -c '%a' "$CORE_DIR"
})"
assert_eq 700 "$core_mode_for_test" "CoreDir creation mode under caller umask 000"
rm -rf "$permission_root"

assert_eq /run/proxyscene-install.lock "$INSTALL_LOCK_PATH" "trusted install lock path"
assert_eq /run/proxyscene-host-ownership.lock "$HOST_LOCK_PATH" "shared host runtime lock path"
# shellcheck disable=SC2031 # the earlier override was confined to a subshell
assert_eq /etc/proxyscene-host-ownership.json "$HOST_OWNERSHIP_PATH" "fixed host ownership path"

lock_handoff_root="$(mktemp -d)"
chmod 700 "$lock_handoff_root"
lock_handoff_for_test() (
  umask 000
  INSTALL_LOCK_PATH="$1/install.lock"
  HOST_LOCK_PATH="$1/host.lock"
  CORE_DIR="$1/core"
  mkdir -m 700 "$CORE_DIR"
  validate_trusted_directory_chain() { :; }
  acquire_install_lock
  acquire_host_runtime_lock
  acquire_store_runtime_lock
  assert_eq 600 "$(stat -c '%a' "$INSTALL_LOCK_PATH")" "install lock creation mode"
  assert_eq 600 "$(stat -c '%a' "$HOST_LOCK_PATH")" "host lock creation mode"
  assert_eq 600 "$(stat -c '%a' "$CORE_DIR/.state.lock")" "state lock creation mode"
  PROXYSCENE_INHERITED_INSTALL_LOCK_FD="$INSTALL_LOCK_FD" \
    PROXYSCENE_INHERITED_HOST_LOCK_FD="$HOST_LOCK_FD" \
    PROXYSCENE_INHERITED_STORE_LOCK_FD="$STORE_LOCK_FD" \
    bash -c '
      flock -n "$PROXYSCENE_INHERITED_INSTALL_LOCK_FD"
      flock -n "$PROXYSCENE_INHERITED_HOST_LOCK_FD"
      flock -n "$PROXYSCENE_INHERITED_STORE_LOCK_FD"
    '
  release_runtime_locks
)
lock_handoff_for_test "$lock_handoff_root" || fail "installer lock fds were not inherited across exec"

fresh_lock_handoff_for_test() (
  INSTALL_LOCK_PATH="$1/fresh-install.lock"
  HOST_LOCK_PATH="$1/fresh-host.lock"
  CORE_DIR="$1/fresh-core"
  validate_trusted_directory_chain() { :; }
  acquire_install_lock
  acquire_host_runtime_lock
  acquire_store_runtime_lock
  [[ -z "$STORE_LOCK_FD" ]] || fail "fresh CoreDir unexpectedly acquired a state lock in install.sh"
  PROXYSCENE_INHERITED_INSTALL_LOCK_FD="$INSTALL_LOCK_FD" \
    PROXYSCENE_INHERITED_HOST_LOCK_FD="$HOST_LOCK_FD" \
    PROXYSCENE_INHERITED_STORE_LOCK_FD="$STORE_LOCK_FD" \
    bash -c '
      [[ -z "$PROXYSCENE_INHERITED_STORE_LOCK_FD" ]]
      flock -n "$PROXYSCENE_INHERITED_INSTALL_LOCK_FD"
      flock -n "$PROXYSCENE_INHERITED_HOST_LOCK_FD"
    '
  release_runtime_locks
)
fresh_lock_handoff_for_test "$lock_handoff_root" \
  || fail "fresh installer did not hand off exactly the install and host locks"
rm -rf "$lock_handoff_root"

url_has_userinfo 'https://user:secret@example.com/assets' || fail "URL userinfo not detected"
if url_has_userinfo 'https://example.com/assets/@scope'; then
  fail "path @ incorrectly treated as URL userinfo"
fi

release_json='{"id":1,"tag_name":"v1.2.3","draft":false,"immutable":true}'
assert_eq v1.2.3 "$(printf '%s\n' "$release_json" | parse_release_tag_json)" "release tag parsing"
printf '%s\n' "$release_json" | release_json_is_immutable \
  || fail "immutable release metadata rejected"
assert_eq v1.2.3 \
  "$(printf '%s\n' "$release_json" | verified_release_tag_json v1.2.3)" \
  "explicit immutable release metadata"
assert_eq v1.2.3 \
  "$(printf '%s\n' "$release_json" | verified_release_tag_json latest)" \
  "latest immutable release metadata"
assert_input_fails "mismatched release tag" "$release_json" verified_release_tag_json v9.9.9
assert_input_fails "mutable release metadata" \
  '{"tag_name":"v1.2.3","immutable":false}' verified_release_tag_json v1.2.3
assert_input_fails "missing immutable field" \
  '{"tag_name":"v1.2.3"}' verified_release_tag_json v1.2.3
assert_input_fails "malformed release metadata" '{not-json' verified_release_tag_json v1.2.3
validate_release_tag v1.2.3 || fail "valid release tag rejected"
if validate_release_tag 'v1/bad'; then
  fail "release tag with slash accepted"
fi
if validate_release_tag latest; then
  fail "literal latest accepted as resolved tag"
fi

saved_manager_repo="$MANAGER_REPO"
saved_manager_base_url="$MANAGER_BASE_URL"
saved_resolved_manager_version="$RESOLVED_MANAGER_VERSION"
MANAGER_REPO=example/project
MANAGER_BASE_URL=https://mirror.example/project/v1.2.3/
RESOLVED_MANAGER_VERSION=v1.2.3
assert_eq https://mirror.example/project/v1.2.3 "$(release_base_url)" "manager mirror base"
assert_eq \
  https://github.com/example/project/releases/download/v1.2.3/checksums.txt \
  "$(release_checksums_url)" \
  "canonical checksum URL"
MANAGER_REPO="$saved_manager_repo"
MANAGER_BASE_URL="$saved_manager_base_url"
RESOLVED_MANAGER_VERSION="$saved_resolved_manager_version"

init_test_dir="$(mktemp -d)"
trap 'rm -rf "$init_test_dir"' EXIT
# shellcheck disable=SC2016 # Variables are intentionally expanded by the child shell.
if PROXYSCENE_INIT_MARKER="$init_test_dir/installed" bash -c '
  export PROXYSCENE_INSTALL_TESTING=1
  source "$1"
  TX_ACTIVE=1
  commit_transaction() {
    : > "$PROXYSCENE_INIT_MARKER"
    TX_ACTIVE=0
  }
  init_manager() {
    [[ "$TX_ACTIVE" == "0" && -f "$PROXYSCENE_INIT_MARKER" ]] || return 99
    return 42
  }
  commit_files_then_init
' _ "$ROOT/install.sh" >/dev/null 2>&1; then
  fail "init failure unexpectedly succeeded"
fi
[[ -f "$init_test_dir/installed" ]] \
  || fail "init failure rolled back the committed file dependency"
rm -rf "$init_test_dir"
trap - EXIT

[[ "$TX_ACTIVE" == "0" ]] || fail "sourcing install.sh unexpectedly started a transaction"
[[ -z "$(PROXYSCENE_INSTALL_TESTING=1 bash "$ROOT/install.sh")" ]] \
  || fail "testing guard produced unexpected output"
printf 'installer pure-function tests passed\n'
