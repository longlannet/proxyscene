#!/usr/bin/env bash
set -Eeuo pipefail

umask 077

readonly CONTAINER_IMAGE="debian:13@sha256:fac46bff2e02f51425b6e33b0e1169f55dfb053d83511ca28aa50c09fd5ed7a4"
readonly CURRENT_CONTAINER_BUNDLE="/artifacts/current-amd64-bundle.tar.gz"
readonly BASELINE_CONTAINER_BUNDLE="/artifacts/baseline-amd64-bundle.tar.gz"
readonly V071_BUNDLE_SHA256="08a12e5716166c76095c54f6ea8227db8f623a719051479ceb7d6720c5c0da38"
readonly V080_BUNDLE_SHA256="6f650d09dcb67d1f745b2e381e6358b2821253edea8b365415f9922e0bbe171a"
readonly V092_BUNDLE_SHA256="0464e87cb15fbcfd6f967202d805f2d699952a5c12053b9392d1025d34368cdc"
readonly V0110_BUNDLE_SHA256="2301d6fad5db8bf33d10de1ad8aead70e790b8b32cacab94643c0bcb4389a195"

log() {
  printf '\n==> %s\n' "$*"
}

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

# Bind both the baseline release and its installer contract to reviewed bytes.
# The outer and inner phases call this independently before extracting a bundle.
upgrade_baseline_identity() {
  case "$1" in
    "$V071_BUNDLE_SHA256") printf '%s\n' 'v0.7.1 1.22.12' ;;
    "$V080_BUNDLE_SHA256") printf '%s\n' 'v0.8.0 1.26.5' ;;
    "$V092_BUNDLE_SHA256") printf '%s\n' 'v0.9.2 1.27.1' ;;
    "$V0110_BUNDLE_SHA256") printf '%s\n' 'v0.11.0 1.27.1' ;;
    *) fail "unsupported upgrade baseline bundle SHA256: $1" ;;
  esac
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"
}

assert_file() {
  [[ -f "$1" && ! -L "$1" ]] || fail "expected regular file: $1"
}

assert_no_path() {
  [[ ! -e "$1" && ! -L "$1" ]] || fail "expected path to be absent: $1"
}

assert_trusted_input_file() {
  local path="$1"
  local owner mode
  assert_file "$path"
  owner="$(stat -c '%u' "$path")"
  mode="$(stat -c '%a' "$path")"
  [[ "$owner" == "$EUID" ]] || fail "test input is not owned by uid $EUID: $path"
  (( (8#$mode & 0022) == 0 )) || fail "test input is group/other writable: $path (mode $mode)"
}

assert_eq() {
  local expected="$1"
  local actual="$2"
  local label="$3"
  [[ "$actual" == "$expected" ]] || fail "$label: expected '$expected', got '$actual'"
}

assert_npm_proxy_eq() {
  local expected="${1%/}"
  local actual="${2%/}"
  local label="$3"
  assert_eq "$expected" "$actual" "$label"
}

assert_contains() {
  local path="$1"
  local expected="$2"
  local label="$3"
  grep -Fq -- "$expected" "$path" || fail "$label: '$expected' not found in $path"
}

assert_not_contains() {
  local path="$1"
  local unexpected="$2"
  local label="$3"
  if grep -Fq -- "$unexpected" "$path"; then
    fail "$label: unexpected '$unexpected' found in $path"
  fi
}

assert_json() {
  local path="$1"
  local expression="$2"
  local label="$3"
  jq -e "$expression" "$path" >/dev/null || fail "$label: jq expression failed: $expression"
}

sha256_file() {
  sha256sum "$1" | awk '{print $1}'
}

wait_for() {
  local label="$1"
  shift
  local attempt
  for ((attempt = 0; attempt < 60; attempt++)); do
    if "$@" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
  done
  fail "timed out waiting for $label"
}

expect_failure() {
  local label="$1"
  local output="$2"
  shift 2
  if "$@" >"$output" 2>&1; then
    fail "$label unexpectedly succeeded"
  fi
}

validate_archive_names() {
  local archive="$1"
  local entry normalized
  while IFS= read -r entry; do
    normalized="${entry#./}"
    [[ -n "$normalized" ]] || fail "archive contains an empty member name: $archive"
    case "$normalized" in
      /*|..|../*|*/..|*/../*)
        fail "archive contains an unsafe member name '$entry': $archive"
        ;;
    esac
  done < <(tar -tzf "$archive")
}

extract_single_root_bundle() {
  local archive="$1"
  local destination="$2"
  local label="$3"
  local -a roots=()

  validate_archive_names "$archive"
  install -d -m 0700 "$destination"
  tar --extract --gzip --file "$archive" --directory "$destination" \
    --no-same-owner --no-same-permissions --delay-directory-restore
  if find "$destination" -mindepth 1 \( ! -type d -a ! -type f \) -print -quit | grep -q .; then
    fail "$label bundle contains a symlink or special file"
  fi
  mapfile -t roots < <(find "$destination" -mindepth 1 -maxdepth 1 -type d -print)
  [[ ${#roots[@]} -eq 1 ]] || fail "$label bundle must contain exactly one top-level directory"
  assert_file "${roots[0]}/install.sh"
  assert_file "${roots[0]}/proxyscene"
  assert_file "${roots[0]}/xray"
  printf '%s\n' "${roots[0]}"
}

service_is_active() {
  systemctl is-active --quiet -- "$1"
}

service_is_inactive() {
  local state
  state="$(systemctl show --property=ActiveState --value -- "$1" 2>/dev/null || true)"
  [[ "$state" == "inactive" || "$state" == "failed" || -z "$state" ]]
}

port_is_listening() {
  local port="$1"
  ss -H -ltn | awk '{print $4}' | grep -Eq "(^|:)${port}$"
}

port_is_not_listening() {
  ! port_is_listening "$1"
}

unit_is_fully_removed() {
  local service="$1"
  [[ "$(systemctl show --property=LoadState --value -- "$service" 2>/dev/null || true)" == "not-found" ]] \
    && ! systemctl is-active --quiet -- "$service" \
    && ! systemctl is-enabled --quiet -- "$service"
}

user_systemctl() {
  local user_name="$1"
  local uid="$2"
  shift 2
  runuser -u "$user_name" -- env \
    "XDG_RUNTIME_DIR=/run/user/${uid}" \
    "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/${uid}/bus" \
    systemctl --user "$@"
}

target_start_count() {
  local target="$1"
  local path="/run/proxyscene-integration-targets/${target}.starts"
  if [[ ! -f "$path" ]]; then
    printf '0\n'
    return 0
  fi
  wc -l < "$path" | tr -d '[:space:]'
}

target_start_count_greater_than() {
  local target="$1"
  local previous="$2"
  local current
  current="$(target_start_count "$target")"
  (( current > previous ))
}

assert_git_values() {
  local key="$1"
  shift
  local value
  local -a actual=()
  mapfile -t actual < <(git config --global --get-all "$key" || true)
  assert_eq "$#" "${#actual[@]}" "$key value count"
  for value in "$@"; do
    printf '%s\n' "${actual[@]}" | grep -Fxq -- "$value" \
      || fail "$key is missing expected value '$value'"
  done
}

# A real Python venv with editable source metadata exercises -m imports under
# PYTHONSAFEPATH=1 and the usual venv/bin/python -> /usr/bin/python3 link.
# It deliberately contains no Hermes credentials or network clients.
install_hermes_python_fixture() {
  local project="$1" site_packages
  install -d -m 0755 "$project" "$project/hermes_cli" "$project/gateway"
  python3 -m venv --without-pip "$project/venv"
  site_packages="$("$project/venv/bin/python" -c 'import sysconfig; print(sysconfig.get_path("purelib"))')"
  printf '%s\n' "$project" > "$site_packages/hermes_agent_fixture.pth"
  install -d -m 0755 "$site_packages/hermes_agent_fixture-0.0.0.dist-info"
  printf '{"url":"file://%s","dir_info":{"editable":true}}\n' "$project" \
    > "$site_packages/hermes_agent_fixture-0.0.0.dist-info/direct_url.json"
  install -m 0644 /dev/null "$project/hermes_cli/__init__.py"
  install -m 0644 /dev/null "$project/gateway/__init__.py"
  install -m 0644 /dev/stdin "$project/hermes_cli/main.py" <<'HERMES_PYTHON'
import json
import os
from pathlib import Path
import sys
import time

if sys.argv[1:] != ["gateway", "run"]:
    raise SystemExit(2)
prefix = Path("/run/proxyscene-integration-targets/hermes-layout")
with prefix.with_suffix(".starts").open("a") as output:
    output.write(f"{os.getpid()}\n")
prefix.with_suffix(".runtime").write_text(json.dumps({
    "project": str(Path(__file__).resolve().parent.parent),
    "uid": os.getuid(),
    "home": os.environ.get("HOME"),
    "hermes_home": os.environ.get("HERMES_HOME"),
    "proxy": os.environ.get("TELEGRAM_PROXY", ""),
    "safe_path": sys.flags.safe_path,
    "prefix": sys.prefix,
    "base_prefix": sys.base_prefix,
}))
while True:
    time.sleep(3600)
HERMES_PYTHON
  install -m 0644 /dev/stdin "$project/gateway/systemd_stop_mark.py" <<'HERMES_HOOK'
from pathlib import Path
with Path("/run/proxyscene-integration-targets/hermes-layout.hooks").open("a") as output:
    output.write(__name__ + "\n")
HERMES_HOOK
  cp -- "$project/gateway/systemd_stop_mark.py" "$project/gateway/cgroup_cleanup.py"
  # venv creation inherits the script's restrictive umask; root-owned system
  # installations must still be readable/executable by the service user.
  find "$project" -type d -exec chmod 0755 {} +
  find "$project" -type f -exec chmod go+r {} +
}

wire_hermes_uv_interpreter_fixture() {
  local project="$1" runtime alias version
  runtime="$project/.hermes-runtime/python"
  alias="$runtime/cpython-current-linux-x86_64-gnu"
  version="$runtime/cpython-fixture-linux-x86_64-gnu"
  install -d -m 0755 "$project/.hermes-runtime" "$runtime" "$version" "$version/bin"
  ln -s -- /usr/bin/python3 "$version/bin/python3"
  ln -s -- "${version##*/}" "$alias"
  # Preserve pyvenv.cfg and editable imports while exercising an intermediate
  # directory link in the real interpreter path used by systemd and both hooks.
  ln -sfn -- "$alias/bin/python3" "$project/venv/bin/python"
  [[ -L "$alias" && -d "$alias" ]] || fail "uv fixture lacks directory alias"
  assert_eq "$(readlink -e -- /usr/bin/python3)" \
    "$(readlink -e -- "$project/venv/bin/python")" "uv fixture interpreter target"
}

write_hermes_layout_unit() {
  local project="$1" service_user="$2" service_home="$3" config_home="$4"
  install -m 0644 /dev/stdin /etc/systemd/system/hermes-gateway.service <<HERMES_LAYOUT_UNIT
[Unit]
Description=proxyscene integration Hermes installation layout
# Lifecycle tests deliberately restart fixtures faster than normal gateways.
StartLimitIntervalSec=0

[Service]
Type=simple
User=$service_user
Environment=HOME=$service_home
Environment=HERMES_HOME=$config_home
ExecStart=$project/venv/bin/python -m hermes_cli.main gateway run
ExecStop=-$project/venv/bin/python -m gateway.systemd_stop_mark
ExecStopPost=-$project/venv/bin/python -m gateway.cgroup_cleanup

[Install]
WantedBy=multi-user.target
HERMES_LAYOUT_UNIT
  systemctl daemon-reload
}

assert_hermes_layout_runtime() {
  local project="$1" service_uid="$2" config_home="$3" expected_proxy="$4"
  jq -e --arg project "$project" --argjson uid "$service_uid" \
    --arg config "$config_home" --arg home "${config_home%/*}" --arg proxy "$expected_proxy" \
    '.project == $project and .uid == $uid and .home == $home and .hermes_home == $config and .proxy == $proxy
     and .prefix == ($project + "/venv") and .prefix != .base_prefix
     and (if $proxy == "" then true else .safe_path == true end)' \
    /run/proxyscene-integration-targets/hermes-layout.runtime >/dev/null
}

assert_hermes_layout_rejected() {
  local label="$1" evidence_dir="$2" state_path="$3" expected_reason="$4"
  local prior_count before after output
  prior_count="$(target_start_count hermes-layout)"
  before="$(sha256sum "$state_path" /opt/proxyscene/config.json \
    /opt/proxyscene/telegram-proxy-journal.json \
    /opt/proxyscene/telegram-proxy-journal.json.bak)"
  output="$evidence_dir/hermes-layout-${label}.log"
  expect_failure "Hermes unsafe $label" "$output" \
    env PROXYSCENE_TG_SERVICES=hermes-gateway /usr/local/bin/proxyscene tg on
  assert_contains "$output" "$expected_reason" "Hermes unsafe $label rejection reason"
  assert_not_contains "$output" 'canary-secret-token' "Hermes rejection redacted dotenv value"
  after="$(sha256sum "$state_path" /opt/proxyscene/config.json \
    /opt/proxyscene/telegram-proxy-journal.json \
    /opt/proxyscene/telegram-proxy-journal.json.bak)"
  assert_eq "$before" "$after" "Hermes unsafe $label preserved state/config/journal bytes"
  assert_eq "$prior_count" "$(target_start_count hermes-layout)" \
    "Hermes unsafe $label did not restart service"
  assert_no_path /etc/systemd/system/hermes-gateway.service.d/90-proxyscene-telegram-proxy.conf
  service_is_active hermes-gateway.service || fail "Hermes unsafe $label stopped service"
  printf 'HERMES_LAYOUT_REJECTION_OK case=%s no_state_write no_ownership no_restart\n' "$label"
}

hermes_installation_layout_canary() {
  local evidence_dir="$1" state_path="$2"
  local service_user=proxyscene-hermes-test service_home=/home/proxyscene-hermes-test
  local config_home="$service_home/.hermes-layout" user_project system_project service_uid count_before
  local managed_drop_in=/etc/systemd/system/hermes-gateway.service.d/90-proxyscene-telegram-proxy.conf
  user_project="$config_home/hermes-agent"
  system_project=/usr/local/lib/hermes-agent

  log "Hermes auto-detects user and system installations with independent configuration"
  systemctl stop -- hermes-gateway.service
  useradd --create-home --shell /bin/bash "$service_user"
  service_uid="$(id -u "$service_user")"
  install -d -o "$service_user" -g "$service_user" -m 0700 "$config_home"
  install -o "$service_user" -g "$service_user" -m 0600 /dev/stdin "$config_home/config.yaml" <<'HERMES_LAYOUT_CONFIG'
platforms:
  telegram:
    extra:
      drop_pending_on_cold_boot: false
HERMES_LAYOUT_CONFIG
  install_hermes_python_fixture "$user_project"
  chown -R "$service_user:$service_user" "$user_project"
  install_hermes_python_fixture "$system_project"
  wire_hermes_uv_interpreter_fixture "$system_project"
  assert_eq 0 "$(stat -c '%u' "$system_project")" "system Hermes project owned by root"
  [[ -L "$system_project/venv/bin/python" ]] || fail "system Hermes venv fixture lacks interpreter link"

  write_hermes_layout_unit "$user_project" "$service_user" "$service_home" "$config_home"
  systemctl start -- hermes-gateway.service
  wait_for "user-owned Hermes editable Python source" assert_hermes_layout_runtime \
    "$user_project" "$service_uid" "$config_home" ''
  env PROXYSCENE_TG_SERVICES=hermes-gateway /usr/local/bin/proxyscene tg on
  wait_for "user Hermes proxy injection" assert_hermes_layout_runtime \
    "$user_project" "$service_uid" "$config_home" http://127.0.0.1:7892

  # The journal owns only the proxy configuration. A changed service program
  # must be re-detected during restore, without persisting the old install path.
  write_hermes_layout_unit "$system_project" "$service_user" "$service_home" "$config_home"
  /usr/local/bin/proxyscene tg off
  wait_for "restore after migration to root-owned system Hermes" assert_hermes_layout_runtime \
    "$system_project" "$service_uid" "$config_home" ''
  assert_no_path "$managed_drop_in"
  printf 'HERMES_LAYOUT_MIGRATION_OK user_to_system tg_off actual_python_source\n'

  install -m 0600 /dev/stdin "$system_project/.env" <<'HERMES_UNSAFE_ENV'
TELEGRAM_PROXY=http://canary-secret-token.invalid:9999
HERMES_UNSAFE_ENV
  assert_hermes_layout_rejected dotenv "$evidence_dir" "$state_path" TELEGRAM_PROXY
  rm -- "$system_project/.env"
  chmod 0777 "$system_project"
  assert_hermes_layout_rejected writable-project "$evidence_dir" "$state_path" "$system_project"
  chmod 0755 "$system_project"
  chown "$service_user:$service_user" "$system_project"
  assert_hermes_layout_rejected wrong-owner "$evidence_dir" "$state_path" "$system_project"
  chown root:root "$system_project"
  mv -- "$system_project" "${system_project}.canary-hidden"
  assert_hermes_layout_rejected missing-project "$evidence_dir" "$state_path" "$system_project"
  mv -- "${system_project}.canary-hidden" "$system_project"
  ln -s -- "$config_home/.env" "$system_project/.env"
  assert_hermes_layout_rejected linked-dotenv "$evidence_dir" "$state_path" '.env'
  rm -- "$system_project/.env"
  mkfifo -m 0600 "$system_project/.env"
  assert_hermes_layout_rejected fifo-dotenv "$evidence_dir" "$state_path" '.env'
  rm -- "$system_project/.env"
  chmod 0775 "$system_project/.hermes-runtime/python"
  assert_hermes_layout_rejected writable-uv-alias-parent "$evidence_dir" "$state_path" \
    "$system_project/venv/bin/python"
  chmod 0755 "$system_project/.hermes-runtime/python"
  chown -h "$service_user:$service_user" \
    "$system_project/.hermes-runtime/python/cpython-current-linux-x86_64-gnu"
  assert_hermes_layout_rejected foreign-uv-alias "$evidence_dir" "$state_path" \
    "$system_project/venv/bin/python"
  chown -h root:root "$system_project/.hermes-runtime/python/cpython-current-linux-x86_64-gnu"

  env PROXYSCENE_TG_SERVICES=hermes-gateway /usr/local/bin/proxyscene tg on
  wait_for "non-root service uses trusted root-owned Hermes and safe Python import" \
    assert_hermes_layout_runtime "$system_project" "$service_uid" "$config_home" http://127.0.0.1:7892
  printf 'HERMES_UV_INTERPRETER_OK trusted_directory_alias actual_python_venv systemd_nonroot\n'
  count_before="$(target_start_count hermes-layout)"
  /usr/local/bin/proxyscene tg on
  assert_eq "$count_before" "$(target_start_count hermes-layout)" \
    "unchanged system installation did not restart Hermes"

  write_hermes_layout_unit "$user_project" "$service_user" "$service_home" "$config_home"
  # Program relocation can be validated anew; disappearance of owned proxy
  # bytes is an administrator conflict and must not be silently reclaimed.
  cp -p -- "$managed_drop_in" "$evidence_dir/hermes-layout-owned-drop-in"
  rm -- "$managed_drop_in"
  systemctl daemon-reload
  expect_failure "boot reconciliation with missing owned Hermes drop-in" \
    "$evidence_dir/hermes-layout-missing-owned.log" /usr/local/bin/proxyscene boot-restore
  assert_no_path "$managed_drop_in"
  assert_no_path /opt/proxyscene/runtime-transition.json
  cp -p -- "$evidence_dir/hermes-layout-owned-drop-in" "$managed_drop_in"
  systemctl daemon-reload
  # The operator changed the gateway program. Start that program explicitly,
  # as a normal boot would, before asking proxyscene to reconcile its proxy.
  systemctl restart -- hermes-gateway.service
  systemctl start -- proxyscene-restore.service
  wait_for "boot reconciliation after return to user Hermes" assert_hermes_layout_runtime \
    "$user_project" "$service_uid" "$config_home" http://127.0.0.1:7892
  assert_file "$managed_drop_in"
  /usr/local/bin/proxyscene tg off
  wait_for "migrated user Hermes restored without proxy" assert_hermes_layout_runtime \
    "$user_project" "$service_uid" "$config_home" ''
  assert_no_path "$managed_drop_in"
  assert_json /opt/proxyscene/telegram-proxy-journal.json '.targets | length == 0' \
    "migration cleanup cleared ownership"
  assert_json "$state_path" '.scene_enabled.telegram == false' "layout canary disabled Telegram"
  printf 'HERMES_LAYOUT_MIGRATION_OK system_to_user boot_restore tg_off actual_python_source\n'
}

assert_core_process_loaded() {
  local label="$1" pid actual
  pid="$(systemctl show --property=MainPID --value proxyscene.service)"
  [[ "$pid" =~ ^[1-9][0-9]*$ ]] || fail "$label has no MainPID"
  assert_eq "$(stat -Lc '%d:%i' /opt/proxyscene/xray)" \
    "$(stat -Lc '%d:%i' "/proc/$pid/exe")" "$label executable identity"
  actual="$(tr '\0' '\n' < "/proc/$pid/cmdline")"
  assert_eq $'/opt/proxyscene/xray\nrun\n-config\n/opt/proxyscene/config.json' "$actual" "$label argv"
  wait_for "$label active service" service_is_active proxyscene.service
}

assert_core_loaded_receipt() {
  local receipt=/opt/proxyscene/core-loaded.json
  assert_file "$receipt"
  assert_eq "$(sha256_file /opt/proxyscene/xray)" "$(jq -r .binary "$receipt")" "loaded binary digest"
  assert_eq "$(sha256_file /opt/proxyscene/config.json)" "$(jq -r .config "$receipt")" "loaded config digest"
  assert_eq "$(sha256_file /etc/systemd/system/proxyscene.service)" "$(jq -r .unit "$receipt")" "loaded unit digest"
  assert_eq "$(systemctl show --property=InvocationID --value proxyscene.service)" \
    "$(jq -r .invocation "$receipt")" "loaded service invocation"
}

runtime_transaction_canary() {
  local test_root="$1" fault_bin="$2" old_port="$3"
  local fault_mode marker before_store before_generation before_config before_profile before_apt
  local candidate_port=$((old_port + 1)) pid invocation store_sha
  local state=/opt/proxyscene/state.json receipt=/opt/proxyscene/runtime-transition.json
  local trusted_fault_bin=/usr/local/sbin/systemctl

  for fault_mode in core-restart-once core-kill-after-restart; do
    log "real systemd runtime transaction recovery: $fault_mode"
    before_store="$(jq -cS 'del(.generation)' "$state")"
    before_generation="$(jq -r .generation "$state")"
    before_config="$(sha256_file /opt/proxyscene/config.json)"
    before_profile="$(sha256_file /etc/profile.d/proxyscene-global-proxy.sh)"
    before_apt="$(sha256_file /etc/apt/apt.conf.d/99proxyscene-global-proxy)"
    marker="/run/proxyscene-$fault_mode"
    assert_no_path "$trusted_fault_bin"
    assert_no_path "$marker"
    install -m 0755 "$fault_bin" "$trusted_fault_bin"
    expect_failure "$fault_mode" "$test_root/$fault_mode.log" \
      env "PROXYSCENE_GLOBAL_HTTP_PORT=$candidate_port" \
        "PROXYSCENE_FAULT_MODE=$fault_mode" "PROXYSCENE_FAULT_MARKER=$marker" \
        /usr/local/bin/proxyscene global on
    rm -f -- "$trusted_fault_bin"
    assert_file "$marker"
    if [[ "$fault_mode" == core-kill-after-restart ]]; then
      assert_file "$receipt"
      assert_json "$receipt" '.phase == "applying" and any(.steps[]; .name == "core" and .started == true)' \
        "crash retained a started core plan"
      assert_eq "$before_generation" "$(jq -r .generation "$state")" "crash did not commit Store"
      wait_for "candidate listener before recovery" port_is_listening "$candidate_port"
      expect_failure "pending transaction blocks mutations" "$test_root/pending-mutation.log" \
        /usr/local/bin/proxyscene global off
      assert_contains "$test_root/pending-mutation.log" 'proxyscene recover' "pending mutation guidance"
      /usr/local/bin/proxyscene recover
    fi
    assert_no_path "$receipt"
    assert_eq "$before_store" "$(jq -cS 'del(.generation)' "$state")" "$fault_mode restored Store semantics"
    assert_eq "$before_generation" "$(jq -r .generation "$state")" "$fault_mode retained uncommitted Store generation"
    cmp -- "$state" "$state.bak" || fail "$fault_mode Store backup differs"
    assert_eq "$before_config" "$(sha256_file /opt/proxyscene/config.json)" "$fault_mode restored config"
    assert_eq "$before_profile" "$(sha256_file /etc/profile.d/proxyscene-global-proxy.sh)" "$fault_mode retained profile"
    assert_eq "$before_apt" "$(sha256_file /etc/apt/apt.conf.d/99proxyscene-global-proxy)" "$fault_mode retained apt proxy"
    assert_core_process_loaded "$fault_mode compensated core"
    wait_for "restored original listener" port_is_listening "$old_port"
    wait_for "retired candidate listener" port_is_not_listening "$candidate_port"
    /usr/local/bin/proxyscene global on
    assert_core_loaded_receipt
    printf 'RUNTIME_TRANSACTION_RECOVERY_VERIFIED mode=%s real_systemd=1\n' "$fault_mode"
  done

  log "unloaded core drop-in fails before Store, runtime files or process change"
  pid="$(systemctl show --property=MainPID --value proxyscene.service)"
  invocation="$(systemctl show --property=InvocationID --value proxyscene.service)"
  store_sha="$(sha256_file "$state")"
  before_config="$(sha256_file /opt/proxyscene/config.json)"
  assert_no_path /etc/systemd/system/proxyscene.service.d
  install -d -m 0755 /etc/systemd/system/proxyscene.service.d
  printf '[Service]\nEnvironment=PROXYSCENE_CANARY_UNLOADED=1\n' \
    > /etc/systemd/system/proxyscene.service.d/99-canary-unloaded.conf
  chmod 0644 /etc/systemd/system/proxyscene.service.d/99-canary-unloaded.conf
  expect_failure "unloaded core drop-in" "$test_root/core-drop-in.log" \
    /usr/local/bin/proxyscene global on
  assert_contains "$test_root/core-drop-in.log" 'drop-in' "unloaded core drop-in refusal"
  assert_eq "$store_sha" "$(sha256_file "$state")" "unloaded drop-in preserved Store"
  assert_eq "$before_config" "$(sha256_file /opt/proxyscene/config.json)" "unloaded drop-in preserved core config"
  assert_eq "$pid" "$(systemctl show --property=MainPID --value proxyscene.service)" "unloaded drop-in preserved process"
  assert_eq "$invocation" "$(systemctl show --property=InvocationID --value proxyscene.service)" "unloaded drop-in preserved invocation"
  assert_no_path "$receipt"
  rm -f -- /etc/systemd/system/proxyscene.service.d/99-canary-unloaded.conf
  rmdir /etc/systemd/system/proxyscene.service.d
  systemctl daemon-reload
  printf 'CORE_UNLOADED_OVERRIDE_REFUSAL_VERIFIED real_systemd=1\n'
}

# Exercise the installed binary while its production global proxy stays active.
# The reserved documentation node cannot carry traffic; a recorded HTTPS request
# failure proves that the isolated core actually started, without a public node.
subscription_and_probe_canary() {
  local test_root="$1" state="$2" snapshot path snapshot_name service
  local pid invocation executable fixture_pid fixture_invocation
  snapshot="$test_root/subscription-probe-runtime"
  install -d -m 0700 "$snapshot"
  pid="$(systemctl show --property=MainPID --value proxyscene.service)"
  invocation="$(systemctl show --property=InvocationID --value proxyscene.service)"
  executable="$(stat -Lc '%d:%i' "/proc/$pid/exe")"
  fixture_pid="$(systemctl show --property=MainPID --value proxyscene-subscription-fixture.service)"
  fixture_invocation="$(systemctl show --property=InvocationID --value proxyscene-subscription-fixture.service)"
  for path in /opt/proxyscene/config.json /opt/proxyscene/core-loaded.json \
    /etc/systemd/system/proxyscene.service /etc/systemd/system/proxyscene-restore.service \
    /etc/profile.d/proxyscene-global-proxy.sh /etc/apt/apt.conf.d/99proxyscene-global-proxy \
    /opt/proxyscene/global-proxy-journal.json /opt/proxyscene/global-proxy-journal.json.bak; do
    snapshot_name="${path//\//_}"
    cp -p -- "$path" "$snapshot/$snapshot_name"
  done
  jq -S 'del(.generation, .speed_results) | .nodes |= map(del(.subscription_ids))' \
    "$state" > "$snapshot/state.json"
  log "saved subscription refresh and isolated node probe preserve the active global service"
  # Even poisoned environment proxies must not affect the direct-first fetch.
  env PROXYSCENE_ALLOW_HTTP_SUBSCRIPTION=1 PROXYSCENE_ALLOW_PRIVATE_SUBSCRIPTION=1 \
    HTTP_PROXY=http://127.0.0.1:1 HTTPS_PROXY=http://127.0.0.1:1 ALL_PROXY=http://127.0.0.1:1 \
    http_proxy=http://127.0.0.1:1 https_proxy=http://127.0.0.1:1 all_proxy=http://127.0.0.1:1 \
    NO_PROXY= no_proxy= PROXYSCENE_GLOBAL_HTTP_PORT=17999 \
    /usr/local/bin/proxyscene subscription update --all
  env PROXYSCENE_TEST_URL=https://example.com/ PROXYSCENE_GLOBAL_HTTP_PORT=17999 \
    /usr/local/bin/proxyscene node test
  assert_json "$state" \
    '(.speed_results | length == 1) and all(.speed_results[]; .success == false and .target == "节点实际 HTTPS 代理请求" and (.error | startswith("节点 HTTPS 代理请求失败")))' \
    "isolated core reached the real HTTPS request phase"
  # A legacy node remains unmanaged until explicit adoption, but its matching
  # source membership is recorded during the first refresh.
  assert_json "$state" \
    'all(.nodes[]; (.subscription_managed // false) == false and (.subscription_ids | length) == 1)' \
    "legacy refresh records source without taking ownership"
  jq -S 'del(.generation, .speed_results) | .nodes |= map(del(.subscription_ids))' \
    "$state" > "$snapshot/state-after.json"
  cmp -- "$snapshot/state.json" "$snapshot/state-after.json" \
    || fail "subscription refresh or probe changed saved node, subscription, or runtime settings"
  for path in /opt/proxyscene/config.json /opt/proxyscene/core-loaded.json \
    /etc/systemd/system/proxyscene.service /etc/systemd/system/proxyscene-restore.service \
    /etc/profile.d/proxyscene-global-proxy.sh /etc/apt/apt.conf.d/99proxyscene-global-proxy \
    /opt/proxyscene/global-proxy-journal.json /opt/proxyscene/global-proxy-journal.json.bak; do
    snapshot_name="${path//\//_}"
    cmp -- "$snapshot/$snapshot_name" "$path" || fail "probe or refresh changed runtime file: $path"
    assert_eq "$(stat -c '%u:%g:%a' "$snapshot/$snapshot_name")" \
      "$(stat -c '%u:%g:%a' "$path")" "probe or refresh retained metadata: $path"
  done
  assert_eq "$pid" "$(systemctl show --property=MainPID --value proxyscene.service)" "probe preserved global PID"
  assert_eq "$invocation" "$(systemctl show --property=InvocationID --value proxyscene.service)" "probe preserved global invocation"
  assert_eq "$executable" "$(stat -Lc '%d:%i' "/proc/$pid/exe")" "probe preserved global executable"
  assert_eq "$fixture_pid" "$(systemctl show --property=MainPID --value proxyscene-subscription-fixture.service)" "probe preserved unrelated PID"
  assert_eq "$fixture_invocation" "$(systemctl show --property=InvocationID --value proxyscene-subscription-fixture.service)" "probe preserved unrelated invocation"
  for service in proxyscene.service proxyscene-subscription-fixture.service; do
    service_is_active "$service" || fail "probe stopped active service: $service"
  done
  if find /tmp -maxdepth 1 -name 'proxyscene-node-probe-*' -print -quit | grep -q .; then
    fail "node probe left private temporary state"
  fi
  if pgrep -f '^/proc/self/fd/3 run -format json -config stdin:$' >/dev/null; then
    fail "node probe left an isolated core process"
  fi
  assert_no_path /opt/proxyscene/runtime-transition.json
  printf 'NODE_PROBE_ACTIVE_SERVICE_ISOLATION_VERIFIED global_unchanged=1 unrelated_unchanged=1 cleanup=1\n'
  subscription_replacement_canary "$test_root" "$state" "$snapshot"
}

subscription_replacement_canary() {
  local test_root="$1" state="$2" snapshot="$3"
  local original_url original_id source_id first_url second_url replacement_id
  local pid invocation fixture_pid fixture_invocation path snapshot_name
  local config_sha receipt_sha global_port
  original_url="$(jq -r '.nodes[0].raw_url' "$state")"
  original_id="$(jq -r '.nodes[0].id' "$state")"
  source_id="$(jq -r '.nodes[0].subscription_ids[0]' "$state")"
  [[ "$original_url" == *@198.51.100.10:443* ]] || fail "unexpected subscription fixture endpoint"
  first_url="${original_url/198.51.100.10/198.51.100.11}"
  second_url="${original_url/198.51.100.10/198.51.100.12}"
  fixture_pid="$(systemctl show --property=MainPID --value proxyscene-subscription-fixture.service)"
  fixture_invocation="$(systemctl show --property=InvocationID --value proxyscene-subscription-fixture.service)"
  jq -S '{runtime_config, scene_enabled, subscriptions}' "$state" > "$snapshot/replacement-settings.json"
  global_port="$(jq -r '.runtime_config.global_http_port' "$state")"

  log "withdraw selected subscription nodes and replace the running core with stored settings"
  /usr/local/bin/proxyscene subscription adopt 1 "$original_id"
  /usr/local/bin/proxyscene node use "$original_id" all
  pid="$(systemctl show --property=MainPID --value proxyscene.service)"
  invocation="$(systemctl show --property=InvocationID --value proxyscene.service)"
  printf '%s\n' "$first_url" "$second_url" > "$test_root/subscription-fixture/nodes.txt"
  env PROXYSCENE_ALLOW_HTTP_SUBSCRIPTION=1 PROXYSCENE_ALLOW_PRIVATE_SUBSCRIPTION=1 \
    PROXYSCENE_HOST=127.0.0.2 PROXYSCENE_GLOBAL_HTTP_PORT=17999 \
    PROXYSCENE_GLOBAL_SOCKS_PORT=17998 PROXYSCENE_SERVICE_USER=unexpected-subscription-user \
    /usr/local/bin/proxyscene subscription update --all
  jq -e --arg first "$first_url" --arg second "$second_url" --arg source "$source_id" \
    --arg old "$original_id" \
    '(.nodes | length == 2) and ([.nodes[].raw_url] == [$first, $second]) and
     all(.nodes[]; .id != $old and .subscription_managed == true and .subscription_ids == [$source]) and
     ((.speed_results | has($old)) | not)' "$state" >/dev/null \
    || fail "subscription replacement retained withdrawn nodes or lost source ownership"
  replacement_id="$(jq -r '.nodes[0].id' "$state")"
  jq -e --arg id "$replacement_id" \
    '.default_node_id == $id and .scene_nodes == {global: $id, dev: $id, telegram: $id}' \
    "$state" >/dev/null || fail "subscription replacement did not migrate every selected binding"
  [[ "$(systemctl show --property=MainPID --value proxyscene.service)" != "$pid" ]] \
    || fail "subscription replacement did not replace the core process"
  [[ "$(systemctl show --property=InvocationID --value proxyscene.service)" != "$invocation" ]] \
    || fail "subscription replacement did not restart the active core"
  assert_core_process_loaded "subscription replacement core"
  assert_core_loaded_receipt
  assert_json /opt/proxyscene/config.json \
    '([.outbounds[] | select(.tag == "proxy-global") | .settings.vnext[0].address] == ["198.51.100.11"])' \
    "running core uses the replacement subscription endpoint"
  wait_for "subscription replacement retained custom listener" port_is_listening "$global_port"
  wait_for "subscription replacement ignored caller port override" port_is_not_listening 17999
  assert_eq "$fixture_pid" "$(systemctl show --property=MainPID --value proxyscene-subscription-fixture.service)" \
    "subscription replacement preserved unrelated PID"
  assert_eq "$fixture_invocation" "$(systemctl show --property=InvocationID --value proxyscene-subscription-fixture.service)" \
    "subscription replacement preserved unrelated invocation"
  for path in /etc/systemd/system/proxyscene.service /etc/systemd/system/proxyscene-restore.service \
    /etc/profile.d/proxyscene-global-proxy.sh /etc/apt/apt.conf.d/99proxyscene-global-proxy \
    /opt/proxyscene/global-proxy-journal.json /opt/proxyscene/global-proxy-journal.json.bak; do
    snapshot_name="${path//\//_}"
    cmp -- "$snapshot/$snapshot_name" "$path" || fail "subscription replacement changed unrelated runtime file: $path"
    assert_eq "$(stat -c '%u:%g:%a' "$snapshot/$snapshot_name")" \
      "$(stat -c '%u:%g:%a' "$path")" "subscription replacement retained metadata: $path"
  done
  assert_no_path /opt/proxyscene/runtime-transition.json

  log "an identical subscription refresh preserves the active core and replacement nodes"
  pid="$(systemctl show --property=MainPID --value proxyscene.service)"
  invocation="$(systemctl show --property=InvocationID --value proxyscene.service)"
  config_sha="$(sha256_file /opt/proxyscene/config.json)"
  receipt_sha="$(sha256_file /opt/proxyscene/core-loaded.json)"
  jq -S 'del(.generation)' "$state" > "$snapshot/replacement-state.json"
  env PROXYSCENE_ALLOW_HTTP_SUBSCRIPTION=1 PROXYSCENE_ALLOW_PRIVATE_SUBSCRIPTION=1 \
    PROXYSCENE_GLOBAL_HTTP_PORT=17999 /usr/local/bin/proxyscene subscription update --all
  jq -S 'del(.generation)' "$state" > "$snapshot/replacement-state-after.json"
  cmp -- "$snapshot/replacement-state.json" "$snapshot/replacement-state-after.json" \
    || fail "identical subscription refresh changed replacement nodes or settings"
  assert_eq "$pid" "$(systemctl show --property=MainPID --value proxyscene.service)" "identical refresh retained core PID"
  assert_eq "$invocation" "$(systemctl show --property=InvocationID --value proxyscene.service)" "identical refresh retained core invocation"
  assert_eq "$config_sha" "$(sha256_file /opt/proxyscene/config.json)" "identical refresh retained core config"
  assert_eq "$receipt_sha" "$(sha256_file /opt/proxyscene/core-loaded.json)" "identical refresh retained loaded receipt"
  assert_no_path /opt/proxyscene/runtime-transition.json

  # Restore the original single endpoint through the public update path so all
  # later lifecycle canaries retain their existing network and scene fixture.
  printf '%s\n' "$original_url" > "$test_root/subscription-fixture/nodes.txt"
  env PROXYSCENE_ALLOW_HTTP_SUBSCRIPTION=1 PROXYSCENE_ALLOW_PRIVATE_SUBSCRIPTION=1 \
    /usr/local/bin/proxyscene subscription update --all
  jq -e --arg raw "$original_url" \
    '.nodes[0].id as $id | (.nodes | length == 1) and .nodes[0].raw_url == $raw and
     .default_node_id == $id and .scene_nodes == {global: $id, dev: $id, telegram: $id}' "$state" >/dev/null \
    || fail "subscription canary did not restore the original selected endpoint"
  cmp -- "$snapshot/_opt_proxyscene_config.json" /opt/proxyscene/config.json \
    || fail "subscription canary did not restore the original core config"
  jq -S '{runtime_config, scene_enabled, subscriptions}' "$state" > "$snapshot/replacement-settings-after.json"
  cmp -- "$snapshot/replacement-settings.json" "$snapshot/replacement-settings-after.json" \
    || fail "subscription replacement changed saved runtime parameters or scene activation"
  assert_core_process_loaded "restored subscription core"
  assert_core_loaded_receipt
  assert_eq "$fixture_pid" "$(systemctl show --property=MainPID --value proxyscene-subscription-fixture.service)" \
    "subscription canary retained unrelated PID"
  assert_eq "$fixture_invocation" "$(systemctl show --property=InvocationID --value proxyscene-subscription-fixture.service)" \
    "subscription canary retained unrelated invocation"
  service_is_active proxyscene-subscription-fixture.service || fail "subscription canary stopped the unrelated fixture"
  assert_no_path /opt/proxyscene/runtime-transition.json
  printf 'SUBSCRIPTION_SELECTED_REPLACEMENT_VERIFIED real_systemd=1 replaced=1 bindings_migrated=1 settings_preserved=1 unrelated_unchanged=1 identical_no_restart=1\n'
}

# Reuse the running Hermes/OpenClaw fixtures after their individual lifecycle
# checks. All paths and service mutations remain inside the disposable container.
remove_all_nodes_canary() {
  local test_root="$1" state="$2" original_url="$3" oc_user="$4" oc_uid="$5"
  local openclaw_config="$6" original_profile="$7" original_apt="$8"
  local snapshot second_url generation global_port global_socks_port restored_id port hermes_before
  snapshot="$test_root/remove-all-nodes"
  install -d -m 0700 "$snapshot"
  assert_json "$state" \
    '(.nodes | length == 1) and .scene_enabled.global == true and .scene_enabled.dev == false and .scene_enabled.telegram == false' \
    "bulk-delete canary starts with the established single-node fixture"
  jq -S '{runtime_config, subscriptions, scene_enabled}' "$state" > "$snapshot/settings-before.json"
  jq -S '.subscriptions' "$state" > "$snapshot/subscriptions-before.json"
  cp -p -- /opt/proxyscene/config.json "$snapshot/config-before.json"
  global_port="$(jq -r '.runtime_config.global_http_port' "$state")"
  global_socks_port="$(jq -r '.runtime_config.global_socks_port' "$state")"
  [[ "$original_url" == *@198.51.100.10:443* ]] || fail "unexpected bulk-delete fixture endpoint"
  second_url="${original_url/198.51.100.10/198.51.100.13}"

  log "delete all nodes in one transaction while all three scenes are active"
  printf '%s\n' "$second_url" | /usr/local/bin/proxyscene node add --stdin
  env PROXYSCENE_DEV_TARGET_USER=root /usr/local/bin/proxyscene dev on
  env "PROXYSCENE_TG_SERVICES=hermes-gateway user:${oc_user}:openclaw-gateway" \
    /usr/local/bin/proxyscene tg on
  assert_json "$state" \
    '(.nodes | length == 2) and .scene_enabled.global == true and .scene_enabled.dev == true and .scene_enabled.telegram == true' \
    "bulk deletion has two nodes and every scene enabled"
  assert_core_process_loaded "bulk-delete active core"
  wait_for "bulk-delete development listener" port_is_listening 7891
  wait_for "bulk-delete Telegram listener" port_is_listening 7892
  wait_for "bulk-delete Hermes proxy settings" grep -Fxq 1 \
    /run/proxyscene-integration-targets/hermes.fallback-ips-disabled
  hermes_before="$(target_start_count hermes)"
  generation="$(jq -r .generation "$state")"

  /usr/local/bin/proxyscene node remove --all
  assert_json "$state" \
    '(.nodes | length == 0) and .default_node_id == "" and (.scene_nodes | length == 0) and
     (.speed_results | length == 0) and .scene_enabled.global == false and
     .scene_enabled.dev == false and .scene_enabled.telegram == false' \
    "bulk deletion cleared every node, selection, speed result and enabled scene"
  assert_eq "$((generation + 1))" "$(jq -r .generation "$state")" "bulk deletion committed exactly once"
  cmp -- "$state" "$state.bak" || fail "bulk deletion Store backup differs"
  jq -S '.subscriptions' "$state" > "$snapshot/subscriptions-after.json"
  cmp -- "$snapshot/subscriptions-before.json" "$snapshot/subscriptions-after.json" \
    || fail "bulk deletion removed saved subscriptions"
  wait_for "bulk-delete stopped core" service_is_inactive proxyscene.service
  assert_eq inactive "$(systemctl show --property=ActiveState --value proxyscene.service)" \
    "bulk deletion cleanly stopped the core"
  assert_eq disabled "$(systemctl show --property=UnitFileState --value proxyscene.service)" \
    "bulk deletion disabled the core"
  for port in "$global_port" "$global_socks_port" 7891 7892 7893; do
    wait_for "bulk-delete closed listener $port" port_is_not_listening "$port"
  done
  assert_no_path /opt/proxyscene/config.json
  assert_no_path /opt/proxyscene/runtime-transition.json
  assert_no_path /opt/proxyscene/global-proxy-journal.json
  assert_no_path /opt/proxyscene/global-proxy-journal.json.bak
  assert_no_path /opt/proxyscene/dev-proxy-backup.json
  assert_eq "$original_profile" \
    "$(tr -d '\r\n' < /etc/profile.d/proxyscene-global-proxy.sh)" "bulk deletion restored original global profile"
  assert_eq "$original_apt" \
    "$(tr -d '\r\n' < /etc/apt/apt.conf.d/99proxyscene-global-proxy)" "bulk deletion restored original apt proxy"
  assert_eq 640 "$(stat -c '%a' /etc/profile.d/proxyscene-global-proxy.sh)" \
    "bulk deletion restored original global profile mode"
  assert_eq 600 "$(stat -c '%a' /etc/apt/apt.conf.d/99proxyscene-global-proxy)" \
    "bulk deletion restored original apt proxy mode"
  assert_git_values http.proxy \
    http://original-http-a.invalid:8001 http://original-http-b.invalid:8002 http://concurrent-http.invalid:8031
  assert_git_values https.proxy \
    http://original-https-a.invalid:8011 http://original-https-b.invalid:8012 http://concurrent-https.invalid:8032
  assert_npm_proxy_eq http://original-npm.invalid:8021 "$(npm config get proxy | tail -n1)" \
    "bulk deletion restored npm proxy"
  assert_npm_proxy_eq http://original-npm-https.invalid:8022 "$(npm config get https-proxy | tail -n1)" \
    "bulk deletion restored npm HTTPS proxy"
  assert_no_path /etc/systemd/system/hermes-gateway.service.d/90-proxyscene-telegram-proxy.conf
  assert_json /opt/proxyscene/telegram-proxy-journal.json '.targets | length == 0' \
    "bulk deletion released Hermes ownership"
  assert_json /opt/proxyscene/telegram-proxy-journal.json.bak '.targets | length == 0' \
    "bulk deletion released Hermes backup ownership"
  assert_json "$openclaw_config" \
    '(.channels.telegram | has("proxy")) and .channels.telegram.proxy == null and .preserved.value == 7 and .channels.telegram.preserved == true' \
    "bulk deletion restored OpenClaw settings"
  jq -e --arg user "$oc_user" '.users | has($user) | not' \
    /opt/proxyscene/openclaw-proxy-journal.json >/dev/null \
    || fail "bulk deletion retained OpenClaw ownership"
  service_is_active hermes-gateway.service || fail "bulk deletion stopped Hermes"
  user_systemctl "$oc_user" "$oc_uid" is-active --quiet -- openclaw-gateway.service \
    || fail "bulk deletion stopped OpenClaw"
  wait_for "bulk-delete Hermes restart" target_start_count_greater_than hermes "$hermes_before"
  wait_for "bulk-delete Hermes proxy cleanup" grep -Fxq '' \
    /run/proxyscene-integration-targets/hermes.fallback-ips-disabled

  # Restore the existing fixture through public commands for the later layout
  # and uninstall canaries; reimport does not silently enable any proxy scene.
  printf '%s\n' "$original_url" | /usr/local/bin/proxyscene node add --stdin
  restored_id="$(jq -r '.nodes[0].id' "$state")"
  /usr/local/bin/proxyscene node use "$restored_id" all
  assert_json "$state" \
    '(.nodes | length == 1) and .scene_enabled.global == false and .scene_enabled.dev == false and .scene_enabled.telegram == false' \
    "reimport retained disabled scenes"
  /usr/local/bin/proxyscene global on
  jq -S '{runtime_config, subscriptions, scene_enabled}' "$state" > "$snapshot/settings-after.json"
  cmp -- "$snapshot/settings-before.json" "$snapshot/settings-after.json" \
    || fail "bulk-delete canary did not restore runtime settings, subscriptions and enabled scenes"
  cmp -- "$snapshot/config-before.json" /opt/proxyscene/config.json \
    || fail "bulk-delete canary did not restore the original core config"
  assert_core_process_loaded "restored bulk-delete core"
  assert_core_loaded_receipt
  assert_no_path /opt/proxyscene/runtime-transition.json
  printf 'NODE_REMOVE_ALL_VERIFIED real_systemd=1 two_nodes=1 single_commit=1 subscriptions_retained=1 all_scenes_closed=1 ownership_restored=1\n'
}

inside_container() {
  local current_archive="$1"
  local old_archive="$2"
  local test_root current_dir old_dir current_manager_sha current_xray_sha
  local fault_dir fault_bin trusted_fault_bin fault_core fault_manager fault_state fault_output
  local rollback_test_root rollback_output tampered_dir tamper_output
  local old_manager_sha old_state old_version custom_port node_url upgraded_global_port
  local baseline_identity baseline_version baseline_go_version
  local upgrade_pid upgrade_invocation upgrade_executable legacy_snapshot path snapshot_name
  local oc_user oc_uid oc_group openclaw_config hermes_before openclaw_before count_after telegram_before
  local failure_output global_profile_original global_profile_concurrent global_apt_original
  local legacy_drop_in legacy_env legacy_state_tmp subscription_url
  local -a fault_env

  [[ "${PROXYSCENE_CONTAINER_TEST:-0}" == "1" ]] \
    || fail "container phase requires PROXYSCENE_CONTAINER_TEST=1"
  [[ -e /.dockerenv ]] || fail "container phase refused outside Docker"
  assert_eq systemd "$(tr -d '[:space:]' < /proc/1/comm)" "container PID 1"
  # shellcheck disable=SC1091
  source /etc/os-release
  assert_eq debian "${ID:-}" "container distribution"
  assert_eq 13 "${VERSION_ID:-}" "container Debian version"
  case "$(uname -m)" in
    x86_64|amd64) ;;
    *) fail "amd64 integration bundle requires an amd64 container" ;;
  esac

  for command_name in systemctl jq tar sha256sum git npm node ss runuser flock; do
    require_command "$command_name"
  done
  assert_file "$current_archive"
  assert_file "$old_archive"
  baseline_identity="$(upgrade_baseline_identity "$(sha256_file "$old_archive")")"
  read -r baseline_version baseline_go_version <<< "$baseline_identity"

	test_root="$(mktemp -d /root/proxyscene-systemd-integration.XXXXXX)"
	cleanup_inside() {
		local status=$?
		if (( status != 0 )); then
			systemctl --no-pager --full status hermes-gateway.service proxyscene.service >&2 || true
			journalctl --no-pager -n 120 -u hermes-gateway.service -u proxyscene.service >&2 || true
		fi
		if [[ -n "${test_root:-}" && "$test_root" == /root/proxyscene-systemd-integration.* ]]; then
			rm -rf -- "$test_root" || return 1
		fi
		return "$status"
	}
  trap cleanup_inside EXIT

  current_dir="$(extract_single_root_bundle "$current_archive" "$test_root/current" current)"
  old_dir="$(extract_single_root_bundle "$old_archive" "$test_root/baseline" "$baseline_version")"
  assert_file "$current_dir/bundle-manifest.sha256"
  current_manager_sha="$(sha256_file "$current_dir/proxyscene")"
  current_xray_sha="$(sha256_file "$current_dir/xray")"
  old_manager_sha="$(sha256_file "$old_dir/proxyscene")"
  [[ "$current_manager_sha" != "$old_manager_sha" ]] \
    || fail "current and $baseline_version manager binaries are byte-identical"
  grep -Fxq "DEFAULT_GO_VERSION=\"$baseline_go_version\"" "$old_dir/install.sh" \
    || fail "$baseline_version installer default Go version differs from $baseline_go_version"
  old_version="$("$old_dir/proxyscene" version)"
  [[ "$old_version" == "proxyscene ${baseline_version#v}" || \
    "$old_version" == "proxyscene ${baseline_version#v} ("*")" ]] \
    || fail "$baseline_version bundle binary reported an unexpected version: $old_version"

  node_url='vless://00000000-0000-4000-8000-000000000001@198.51.100.10:443?encryption=none&security=tls&sni=example.com&type=tcp#systemd-integration'
  custom_port=17890

  log "offline manifest rejection happens before any installation change"
  tampered_dir="$test_root/tampered-bundle"
  cp -a -- "$current_dir" "$tampered_dir"
  printf 'tampered\n' >> "$tampered_dir/xray"
  tamper_output="$test_root/tampered-bundle.log"
  # shellcheck disable=SC2016 # $1 is expanded by the isolated child shell.
  expect_failure "tampered offline bundle" "$tamper_output" \
    bash -c 'cd "$1" && exec ./install.sh --offline' _ "$tampered_dir"
  assert_contains "$tamper_output" 'SHA256' "tampered bundle checksum failure"
  assert_no_path /usr/local/bin/proxyscene
  assert_no_path /opt/proxyscene

  log "real multi-file installer rollback restores prior bytes"
  rollback_test_root=/opt/proxyscene-installer-rollback-test
  rollback_output="$test_root/file-rollback.log"
  assert_no_path "$rollback_test_root"
  install -d -m 0700 "$rollback_test_root"
  printf 'old-one\n' > "$rollback_test_root/dest-one"
  printf 'old-two\n' > "$rollback_test_root/dest-two"
  printf 'new-one\n' > "$rollback_test_root/source-one"
  printf 'new-two\n' > "$rollback_test_root/source-two"
  chmod 0600 "$rollback_test_root"/*
  # shellcheck disable=SC2016 # $1 and ROLLBACK_TEST_ROOT are expanded by the child shell.
  expect_failure "third replacement failure" "$rollback_output" \
    env PROXYSCENE_INSTALL_TESTING=1 ROLLBACK_TEST_ROOT="$rollback_test_root" \
      /bin/bash -c '
        source "$1/install.sh"
        begin_transaction
        transactional_replace "$ROLLBACK_TEST_ROOT/source-one" "$ROLLBACK_TEST_ROOT/dest-one" 600 one
        transactional_replace "$ROLLBACK_TEST_ROOT/source-two" "$ROLLBACK_TEST_ROOT/dest-two" 600 two
        transactional_replace "$ROLLBACK_TEST_ROOT/missing" "$ROLLBACK_TEST_ROOT/dest-three" 600 three
      ' _ "$current_dir"
  assert_eq old-one "$(tr -d '\r\n' < "$rollback_test_root/dest-one")" \
    "first file rollback"
  assert_eq old-two "$(tr -d '\r\n' < "$rollback_test_root/dest-two")" \
    "second file rollback"
  assert_no_path "$rollback_test_root/dest-three"
  assert_contains "$rollback_output" '已回滚本次二进制和数据文件替换' \
    "rollback completion report"
  rm -rf -- "$rollback_test_root"

  log "manager initialization failure retains verified files; fresh interactive retry"
  fault_dir="$test_root/fault-bin"
  fault_core="/opt/proxyscene-init-fault"
  fault_manager="/usr/local/lib/proxyscene-init-fault/proxyscene"
  fault_state="$fault_core/state.json"
  fault_output="$test_root/manager-init-failure.log"
  install -d -m 0755 "$fault_dir" "$(dirname "$fault_manager")"
  install -m 0755 /dev/stdin "$fault_dir/systemctl" <<'FAULT_SYSTEMCTL'
#!/usr/bin/env bash
set -euo pipefail
mode="${PROXYSCENE_FAULT_MODE:-}"
marker="${PROXYSCENE_FAULT_MARKER:-/run/proxyscene-integration-fault}"
if [[ ! -e "$marker" ]]; then
  case "$mode" in
    observe-upgrade)
      if [[ "${1:-}" == show && "${*: -1}" == proxyscene.service ]]; then
        pid="$(/usr/bin/systemctl show --property=MainPID --value proxyscene.service)"
        if [[ "$pid" =~ ^[1-9][0-9]*$ ]] && \
          [[ "$(stat -Lc '%d:%i' "/proc/$pid/exe")" != "$(stat -Lc '%d:%i' /opt/proxyscene/xray)" ]]; then
          : > "$marker"
        fi
      fi
      ;;
    core-restart-once)
      if [[ "$*" == "restart -- proxyscene.service" ]]; then
        : > "$marker"
        exit 95
      fi
      ;;
    core-kill-after-restart)
      if [[ "$*" == "restart -- proxyscene.service" ]]; then
        /usr/bin/systemctl "$@"
        : > "$marker"
        kill -KILL "$PPID"
        exit 96
      fi
      ;;
    manager-init)
      if [[ "$#" -eq 1 && "$1" == "daemon-reload" ]]; then
        : > "$marker"
        exit 91
      fi
      ;;
    main-disable)
      if [[ "$*" == "disable --now -- proxyscene.service" ]]; then
        : > "$marker"
        exit 92
      fi
      ;;
    daemon-reload)
      if [[ "$#" -eq 1 && "$1" == "daemon-reload" ]] && \
        [[ ! -e /etc/systemd/system/proxyscene.service ]] && \
        [[ ! -e /etc/systemd/system/proxyscene-restore.service ]]; then
        : > "$marker"
        exit 93
      fi
      ;;
    kill-daemon-reload)
      if [[ "$#" -eq 1 && "$1" == "daemon-reload" ]] && \
        [[ ! -e /etc/systemd/system/hermes-gateway.service.d/90-proxyscene-telegram-proxy.conf ]] && \
        /usr/bin/jq -e \
          '.targets["hermes-gateway.service"].phase == "restoring"' \
          /opt/proxyscene/telegram-proxy-journal.json >/dev/null 2>&1; then
        : > "$marker"
        kill -KILL "$PPID"
        exit 94
      fi
      ;;
  esac
fi
exec /usr/bin/systemctl "$@"
FAULT_SYSTEMCTL
  fault_bin="$fault_dir/systemctl"
  assert_file "$fault_bin"
  trusted_fault_bin=/usr/local/sbin/systemctl
  assert_no_path "$trusted_fault_bin"
  install -m 0755 "$fault_bin" "$trusted_fault_bin"
  fault_env=(
    "PROXYSCENE_MANAGER_DIR=$fault_core"
    "PROXYSCENE_SWITCH_BIN=$fault_manager"
    "PROXYSCENE_SYSTEMD_SERVICE_NAME=proxyscene-init-fault.service"
    "PROXYSCENE_BOOT_RESTORE_SERVICE_NAME=proxyscene-init-fault-restore.service"
  )
  rm -f /run/proxyscene-manager-init-fault
  # shellcheck disable=SC2016 # $1 is expanded by the isolated child shell.
  expect_failure "fault-injected offline install" "$fault_output" \
    env "${fault_env[@]}" \
      PROXYSCENE_FAULT_MODE=manager-init \
      PROXYSCENE_FAULT_MARKER=/run/proxyscene-manager-init-fault \
      bash -c 'cd "$1" && exec ./install.sh --offline' _ "$current_dir"
  rm -f -- "$trusted_fault_bin"
  assert_file /run/proxyscene-manager-init-fault
  assert_file "$fault_manager"
  assert_file "$fault_core/xray"
  assert_eq "$current_manager_sha" "$(sha256_file "$fault_manager")" "retained manager SHA256"
  assert_eq "$current_xray_sha" "$(sha256_file "$fault_core/xray")" "retained Xray SHA256"

  rm -f \
    /etc/systemd/system/proxyscene-init-fault.service \
    /etc/systemd/system/proxyscene-init-fault-restore.service
  /usr/bin/systemctl daemon-reload
  assert_no_path "$fault_state"
  printf '%s\n' "$node_url" | env "${fault_env[@]}" "$fault_manager" install
  assert_json "$fault_state" '.nodes | length == 1' "fresh interactive node persisted"
  assert_eq "$node_url" "$(jq -r '.nodes[0].raw_url' "$fault_state")" \
    "fresh interactive node URL"
  assert_file /etc/systemd/system/proxyscene-init-fault.service
  assert_file /etc/systemd/system/proxyscene-init-fault-restore.service
  assert_eq enabled "$(systemctl is-enabled proxyscene-init-fault-restore.service)" \
    "fresh interactive restore unit"
  wait_for "idle fault-injection Xray service" service_is_inactive \
    proxyscene-init-fault.service
  assert_no_path "$fault_core/config.json"
  env "${fault_env[@]}" "$fault_manager" uninstall
  assert_no_path /etc/systemd/system/proxyscene-init-fault.service
  assert_no_path /etc/systemd/system/proxyscene-init-fault-restore.service

  log "$baseline_version offline install with node and global scene"
  (
    cd "$old_dir"
    case "$baseline_version" in
      v0.7.1) ./install.sh --offline "$node_url" ;;
      v0.8.0|v0.9.2|v0.11.0)
        ./install.sh --offline
        printf '%s\n' "$node_url" | /usr/local/bin/proxyscene node add --stdin
        ;;
    esac
  )
  old_state=/opt/proxyscene/state.json
  assert_eq "$old_manager_sha" "$(sha256_file /usr/local/bin/proxyscene)" \
    "installed $baseline_version manager SHA256"
  assert_json "$old_state" '.nodes | length == 1' "$baseline_version node count"
  assert_eq "$node_url" "$(jq -r '.nodes[0].raw_url' "$old_state")" \
    "$baseline_version node URL"
  env PROXYSCENE_GLOBAL_HTTP_PORT="$custom_port" \
    PROXYSCENE_GLOBAL_SOCKS_PORT=17894 \
    /usr/local/bin/proxyscene global on
  assert_json "$old_state" '.scene_enabled.global == true' "$baseline_version global scene"
  wait_for "$baseline_version Xray service" service_is_active proxyscene.service
  assert_file /etc/profile.d/proxyscene-global-proxy.sh
  assert_file /etc/apt/apt.conf.d/99proxyscene-global-proxy
  assert_contains /etc/profile.d/proxyscene-global-proxy.sh ":${custom_port}" \
    "$baseline_version custom global port"

  case "$baseline_version" in
    v0.7.1)
      assert_json "$old_state" '.runtime_config == null' "v0.7.1 has no persisted runtime configuration"
      upgraded_global_port="$custom_port"
      ;;
    v0.8.0|v0.9.2|v0.11.0)
      assert_json "$old_state" ".runtime_config.global_http_port == $custom_port" \
        "$baseline_version custom global port persisted before upgrade"
      upgraded_global_port="$custom_port"
      ;;
  esac

  if [[ "$baseline_version" == v0.11.0 ]]; then
    log "save a subscription through the official v0.11.0 manager before upgrade"
    install -d -m 0700 "$test_root/subscription-fixture"
    printf '%s\n' "$node_url" > "$test_root/subscription-fixture/nodes.txt"
    systemd-run --unit=proxyscene-subscription-fixture.service --property=Type=exec \
      /usr/bin/python3 -m http.server --bind 127.0.0.1 --directory "$test_root/subscription-fixture" 17897
    wait_for "local subscription fixture" port_is_listening 17897
    subscription_url='http://127.0.0.1:17897/nodes.txt'
    printf '%s\n' "$subscription_url" | env PROXYSCENE_ALLOW_HTTP_SUBSCRIPTION=1 \
      PROXYSCENE_ALLOW_PRIVATE_SUBSCRIPTION=1 /usr/local/bin/proxyscene node import --stdin
    assert_json "$old_state" '.nodes | length == 1' "baseline subscription deduplicates the existing node"
    assert_json "$old_state" '.subscriptions == ["http://127.0.0.1:17897/nodes.txt"]' "baseline saved subscription"
    jq -S '{nodes, subscriptions, default_node_id, scene_nodes, scene_enabled, runtime_config}' \
      "$old_state" > "$test_root/subscription-before-upgrade.json"
  fi

  upgrade_pid="$(systemctl show --property=MainPID --value proxyscene.service)"
  upgrade_invocation="$(systemctl show --property=InvocationID --value proxyscene.service)"
  upgrade_executable="$(stat -Lc '%d:%i' "/proc/$upgrade_pid/exe")"
  if [[ "$baseline_version" == v0.7.1 ]]; then
    log "legacy state without RuntimeConfig refuses migration and retains active runtime"
    legacy_snapshot="$test_root/legacy-before"
    install -d -m 0700 "$legacy_snapshot"
    for path in /opt/proxyscene/state.json /opt/proxyscene/state.json.bak \
      /opt/proxyscene/config.json /etc/systemd/system/proxyscene.service \
      /etc/systemd/system/proxyscene-restore.service \
      /etc/profile.d/proxyscene-global-proxy.sh /etc/apt/apt.conf.d/99proxyscene-global-proxy; do
      snapshot_name="${path//\//_}"
      if [[ -e "$path" ]]; then
        cp -p -- "$path" "$legacy_snapshot/$snapshot_name"
      fi
    done
    # Binary installation commits independently; missing historical runtime
    # evidence must stop manager initialization before touching live resources.
    # shellcheck disable=SC2016
    expect_failure "unsupported v0.7.1 runtime migration" "$test_root/legacy-refusal.log" \
      bash -c 'cd "$1" && exec ./install.sh --offline' _ "$current_dir"
    assert_contains "$test_root/legacy-refusal.log" '缺少 RuntimeConfig' "legacy refusal reason"
    assert_eq "$current_manager_sha" "$(sha256_file /usr/local/bin/proxyscene)" "verified manager retained after refusal"
    assert_eq "$current_xray_sha" "$(sha256_file /opt/proxyscene/xray)" "verified Xray retained after refusal"
    for path in /opt/proxyscene/state.json /opt/proxyscene/state.json.bak \
      /opt/proxyscene/config.json /etc/systemd/system/proxyscene.service \
      /etc/systemd/system/proxyscene-restore.service \
      /etc/profile.d/proxyscene-global-proxy.sh /etc/apt/apt.conf.d/99proxyscene-global-proxy; do
      snapshot_name="${path//\//_}"
      if [[ -e "$legacy_snapshot/$snapshot_name" ]]; then
        cmp -- "$legacy_snapshot/$snapshot_name" "$path" || fail "legacy runtime changed: $path"
        assert_eq "$(stat -c '%u:%g:%a' "$legacy_snapshot/$snapshot_name")" \
          "$(stat -c '%u:%g:%a' "$path")" "legacy runtime metadata $path"
      else
        assert_no_path "$path"
      fi
    done
    assert_eq "$upgrade_pid" "$(systemctl show --property=MainPID --value proxyscene.service)" "legacy process retained"
    assert_eq "$upgrade_invocation" "$(systemctl show --property=InvocationID --value proxyscene.service)" "legacy invocation retained"
    assert_eq "$upgrade_executable" "$(stat -Lc '%d:%i' "/proc/$upgrade_pid/exe")" "legacy executable retained"
    wait_for "legacy custom listener after refusal" port_is_listening "$custom_port"
    assert_no_path /opt/proxyscene/runtime-transition.json
    assert_no_path /opt/proxyscene/global-proxy-journal.json
    printf 'LEGACY_MIGRATION_REFUSAL_VERIFIED baseline=%s runtime_unchanged=1 binaries_committed=1\n' "$baseline_version"
    cleanup_inside
    trap - EXIT
    return 0
  fi

  log "upgrade $baseline_version in place with an active old executable"
  install -m 0755 "$fault_bin" "$trusted_fault_bin"
  (
    cd "$current_dir"
    PROXYSCENE_FAULT_MODE=observe-upgrade \
      PROXYSCENE_FAULT_MARKER=/run/proxyscene-active-upgrade-observed \
      ./install.sh --offline
  )
  rm -f -- "$trusted_fault_bin"
  assert_file /run/proxyscene-active-upgrade-observed
  [[ "$(systemctl show --property=InvocationID --value proxyscene.service)" != "$upgrade_invocation" ]] \
    || fail "upgrade did not replace the old service invocation"
  assert_core_process_loaded "upgraded core"
  assert_core_loaded_receipt
  printf 'ACTIVE_BINARY_UPGRADE_VERIFIED baseline=%s\n' "$baseline_version"
  assert_eq "$current_manager_sha" "$(sha256_file /usr/local/bin/proxyscene)" \
    "upgraded manager SHA256"
  assert_eq "$current_xray_sha" "$(sha256_file /opt/proxyscene/xray)" \
    "upgraded Xray SHA256"
  assert_file /etc/proxyscene-host-ownership.json
  assert_eq 600 "$(stat -c '%a' /etc/proxyscene-host-ownership.json)" \
    "host ownership mode"
  assert_json /etc/proxyscene-host-ownership.json \
    '.core_dir == "/opt/proxyscene" and .install_bin == "/usr/local/bin/proxyscene" and .systemd_service == "proxyscene.service" and .restore_service == "proxyscene-restore.service"' \
    "host ownership locator binding"
  assert_json "$old_state" '.nodes | length == 1' "upgraded node count"
  assert_eq "$node_url" "$(jq -r '.nodes[0].raw_url' "$old_state")" \
    "upgraded node URL"
  assert_json "$old_state" '.scene_enabled.global == true' "upgraded global scene"
  assert_json "$old_state" '.runtime_config.version == 1' "runtime configuration migration"
  wait_for "upgraded Xray service" service_is_active proxyscene.service
  for journal_path in \
    /opt/proxyscene/global-proxy-journal.json \
    /opt/proxyscene/global-proxy-journal.json.bak; do
    assert_file "$journal_path"
    assert_eq 600 "$(stat -c '%a' "$journal_path")" "global ownership journal mode"
    assert_json "$journal_path" \
      '.phase == "active" and all(.artifacts[]; .original_present == false)' \
      "global files retain or migrate into durable ownership"
  done
  assert_eq enabled "$(systemctl is-enabled proxyscene-restore.service)" \
    "upgraded restore unit"
  assert_json /opt/proxyscene/config.json \
    ".inbounds[] | select(.tag == \"global-http\" and .port == $upgraded_global_port)" \
    "upgraded global inbound"

  if [[ "$baseline_version" == v0.11.0 ]]; then
    jq -S '{nodes, subscriptions, default_node_id, scene_nodes, scene_enabled, runtime_config}' \
      "$old_state" > "$test_root/subscription-after-upgrade.json"
    cmp -- "$test_root/subscription-before-upgrade.json" "$test_root/subscription-after-upgrade.json" \
      || fail "v0.11.0 upgrade changed saved nodes, subscriptions or active runtime settings"
    subscription_and_probe_canary "$test_root" "$old_state"
    systemctl stop -- proxyscene-subscription-fixture.service
    wait_for "stopped subscription fixture" port_is_not_listening 17897
    printf 'SUBSCRIPTION_ACTIVE_GLOBAL_UPGRADE_VERIFIED baseline=%s\n' "$baseline_version"
  fi

  runtime_transaction_canary "$test_root" "$fault_bin" "$upgraded_global_port"

  log "custom runtime port survives a restore-unit start without caller environment"
  env "PROXYSCENE_GLOBAL_HTTP_PORT=$custom_port" /usr/local/bin/proxyscene global on
  assert_json "$old_state" ".runtime_config.global_http_port == $custom_port" \
    "custom global port persisted"
  assert_contains /etc/profile.d/proxyscene-global-proxy.sh ":${custom_port}" \
    "custom global profile proxy"
  for locator in \
    PROXYSCENE_MANAGER_DIR \
    PROXYSCENE_SWITCH_BIN \
    PROXYSCENE_SYSTEMD_SERVICE_NAME \
    PROXYSCENE_BOOT_RESTORE_SERVICE_NAME; do
    assert_contains /etc/systemd/system/proxyscene-restore.service \
      "Environment=\"${locator}=" "restore unit locator $locator"
  done
  for runtime_name in \
    PROXYSCENE_HOST \
    PROXYSCENE_GLOBAL_HTTP_PORT \
    PROXYSCENE_DEV_HTTP_PORT \
    PROXYSCENE_TG_HTTP_PORT \
    PROXYSCENE_TG_SOCKS_PORT \
    PROXYSCENE_GLOBAL_SOCKS_PORT \
    PROXYSCENE_SERVICE_USER \
    PROXYSCENE_TG_SERVICES \
    PROXYSCENE_DEV_TARGET_USER \
    PROXYSCENE_MANAGE_OPENCLAW_CONFIG \
    PROXYSCENE_ALLOW_PUBLIC_BIND; do
    assert_not_contains /etc/systemd/system/proxyscene-restore.service \
      "${runtime_name}=" "restore unit excludes runtime setting $runtime_name"
  done
  systemctl stop -- proxyscene.service
  wait_for "stopped Xray before boot restore" service_is_inactive proxyscene.service
  systemctl reset-failed -- proxyscene.service proxyscene-restore.service || true
  if ! systemctl start -- proxyscene-restore.service; then
    systemctl --no-pager --full status proxyscene-restore.service >&2 || true
    journalctl --no-pager -n 200 -u proxyscene-restore.service >&2 || true
    fail "boot restore service failed"
  fi
  wait_for "boot-restored Xray service" service_is_active proxyscene.service
  wait_for "custom global listener" port_is_listening "$custom_port"
  assert_json /opt/proxyscene/config.json \
    ".inbounds[] | select(.tag == \"global-http\" and .port == $custom_port)" \
    "boot restore used persisted custom port"
  assert_json "$old_state" ".runtime_config.global_http_port == $custom_port" \
    "boot restore retained custom runtime port"

  log "global scene lifecycle"
  /usr/local/bin/proxyscene global off
  assert_json "$old_state" '.scene_enabled.global == false' "global scene disabled"
  assert_no_path /etc/profile.d/proxyscene-global-proxy.sh
  assert_no_path /etc/apt/apt.conf.d/99proxyscene-global-proxy
  assert_no_path /opt/proxyscene/global-proxy-journal.json
  assert_no_path /opt/proxyscene/global-proxy-journal.json.bak
  wait_for "idle Xray after global off" service_is_inactive proxyscene.service
  global_profile_original='# operator profile content survives proxyscene'
  global_apt_original='// operator apt content survives proxyscene'
  printf '%s\n' "$global_profile_original" > /etc/profile.d/proxyscene-global-proxy.sh
  printf '%s\n' "$global_apt_original" > /etc/apt/apt.conf.d/99proxyscene-global-proxy
  chmod 0640 /etc/profile.d/proxyscene-global-proxy.sh
  chmod 0600 /etc/apt/apt.conf.d/99proxyscene-global-proxy
  /usr/local/bin/proxyscene global on
  assert_json "$old_state" '.scene_enabled.global == true' "global scene re-enabled"
  assert_contains /etc/profile.d/proxyscene-global-proxy.sh ":${custom_port}" \
    "global scene reused persisted port"
  assert_json /opt/proxyscene/global-proxy-journal.json \
    '.phase == "active" and all(.artifacts[]; .original_present == true)' \
    "operator global files captured before replacement"
  wait_for "Xray after global on" service_is_active proxyscene.service

  log "development scene lifecycle preserves original and concurrent Git/npm values"
  git config --global --unset-all http.proxy >/dev/null 2>&1 || true
  git config --global --unset-all https.proxy >/dev/null 2>&1 || true
  git config --global --add http.proxy http://original-http-a.invalid:8001
  git config --global --add http.proxy http://original-http-b.invalid:8002
  git config --global --add https.proxy http://original-https-a.invalid:8011
  git config --global --add https.proxy http://original-https-b.invalid:8012
  npm config set proxy http://original-npm.invalid:8021
  npm config set https-proxy http://original-npm-https.invalid:8022
  install -d -m 0700 /root/.config/git
  install -m 0600 /dev/stdin /root/.config/git/config <<'SECOND_GLOBAL_CONFIG'
[user]
	name = topology-conflict
SECOND_GLOBAL_CONFIG
  failure_output="$test_root/dev-dual-global.log"
  expect_failure "development scene with two Git global files" "$failure_output" \
    env PROXYSCENE_DEV_TARGET_USER=root /usr/local/bin/proxyscene dev on
  assert_contains "$failure_output" '多个 Git global 配置文件' \
    "dual Git global topology rejection"
  assert_git_values http.proxy \
    http://original-http-a.invalid:8001 \
    http://original-http-b.invalid:8002
  assert_git_values https.proxy \
    http://original-https-a.invalid:8011 \
    http://original-https-b.invalid:8012
  assert_npm_proxy_eq http://original-npm.invalid:8021 "$(npm config get proxy | tail -n1)" \
    "npm proxy unchanged after Git topology rejection"
  assert_no_path /opt/proxyscene/dev-proxy-backup.json
  assert_json "$old_state" '(.scene_enabled.dev // false) == false' \
    "development scene stayed disabled after Git topology rejection"
  rm -f -- /root/.config/git/config
  env PROXYSCENE_DEV_TARGET_USER=root /usr/local/bin/proxyscene dev on
  assert_json "$old_state" '.scene_enabled.dev == true' "development scene enabled"
  assert_json "$old_state" '.runtime_config.dev_target_user == "root"' \
    "development target user persisted"
  assert_git_values http.proxy http://127.0.0.1:7891
  assert_git_values https.proxy http://127.0.0.1:7891
  assert_npm_proxy_eq http://127.0.0.1:7891 "$(npm config get proxy | tail -n1)" \
    "managed npm proxy"
  assert_npm_proxy_eq http://127.0.0.1:7891 "$(npm config get https-proxy | tail -n1)" \
    "managed npm HTTPS proxy"
  assert_json /opt/proxyscene/config.json \
    '.inbounds[] | select(.tag == "dev-http" and .port == 7891)' \
    "development Xray inbound"
  wait_for "development HTTP listener" port_is_listening 7891
  if ! systemctl start -- proxyscene-restore.service; then
    systemctl --no-pager --full status proxyscene-restore.service >&2 || true
    journalctl --no-pager -n 200 -u proxyscene-restore.service >&2 || true
    fail "boot restore failed after development scene apply"
  fi
  assert_git_values http.proxy http://127.0.0.1:7891
  assert_git_values https.proxy http://127.0.0.1:7891
  assert_npm_proxy_eq http://127.0.0.1:7891 "$(npm config get proxy | tail -n1)" \
    "development proxy after clean-environment restore"
  git config --global --add http.proxy http://concurrent-http.invalid:8031
  git config --global --add https.proxy http://concurrent-https.invalid:8032
  /usr/local/bin/proxyscene dev off
  assert_json "$old_state" '.scene_enabled.dev == false' "development scene disabled"
  assert_git_values http.proxy \
    http://original-http-a.invalid:8001 \
    http://original-http-b.invalid:8002 \
    http://concurrent-http.invalid:8031
  assert_git_values https.proxy \
    http://original-https-a.invalid:8011 \
    http://original-https-b.invalid:8012 \
    http://concurrent-https.invalid:8032
  assert_npm_proxy_eq http://original-npm.invalid:8021 "$(npm config get proxy | tail -n1)" \
    "restored npm proxy"
  assert_npm_proxy_eq http://original-npm-https.invalid:8022 "$(npm config get https-proxy | tail -n1)" \
    "restored npm HTTPS proxy"
  assert_no_path /opt/proxyscene/dev-proxy-backup.json
  assert_json /opt/proxyscene/config.json \
    '[.inbounds[] | select(.tag == "dev-http")] | length == 0' \
    "development inbound removed"
  wait_for "removed development HTTP listener" port_is_not_listening 7891

  log "generic Hermes and OpenClaw lifecycle with crash-safe Telegram cleanup"
  install -d -m 0777 /run/proxyscene-integration-targets
  install -d -m 0755 \
    /root/.hermes-integration \
    /root/.hermes-integration/hermes-agent \
    /root/.hermes-integration/hermes-agent/venv \
    /root/.hermes-integration/hermes-agent/venv/bin
  install -m 0755 /dev/stdin \
    /root/.hermes-integration/hermes-agent/venv/bin/python <<'HERMES_HELPER'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  '-m gateway.systemd_stop_mark'|'-m gateway.cgroup_cleanup')
    printf '%s\n' "$2" >> /run/proxyscene-integration-targets/hermes.hooks
    exit 0
    ;;
  '-m hermes_cli.main gateway run') ;;
  *) exit 2 ;;
esac
printf '%s\n' "$$" >> /run/proxyscene-integration-targets/hermes.starts
printf '%s\n' "${HERMES_TELEGRAM_DISABLE_FALLBACK_IPS-}" \
  > /run/proxyscene-integration-targets/hermes.fallback-ips-disabled
exec /usr/bin/sleep infinity
HERMES_HELPER
  install -m 0644 /dev/stdin /etc/systemd/system/hermes-gateway.service <<'HERMES_UNIT'
[Unit]
Description=proxyscene integration Hermes gateway
# Lifecycle tests deliberately restart fixtures faster than normal gateways.
StartLimitIntervalSec=0

[Service]
Type=simple
User=root
Environment=HOME=/root
Environment=HERMES_HOME=/root/.hermes-integration
ExecStart=/root/.hermes-integration/hermes-agent/venv/bin/python -m hermes_cli.main gateway run
ExecStop=-/root/.hermes-integration/hermes-agent/venv/bin/python -m gateway.systemd_stop_mark
ExecStopPost=-/root/.hermes-integration/hermes-agent/venv/bin/python -m gateway.cgroup_cleanup

[Install]
WantedBy=multi-user.target
HERMES_UNIT

  systemctl daemon-reload
  systemctl enable --now -- hermes-gateway.service
  wait_for "Hermes fixture" service_is_active hermes-gateway.service
  wait_for "Hermes fixture process start" target_start_count_greater_than hermes 0
  log "running Hermes default queue policy refuses ownership before any restart"
  hermes_before="$(target_start_count hermes)"
  telegram_before="$(jq -c '(.scene_enabled.telegram // false)' "$old_state")"
  failure_output="$test_root/hermes-queue-policy.log"
  expect_failure "Hermes default drop-pending queue policy" "$failure_output" \
    env PROXYSCENE_TG_SERVICES=hermes-gateway /usr/local/bin/proxyscene tg on
  assert_contains "$failure_output" 'drop_pending_on_cold_boot' "Hermes queue-policy rejection reason"
  assert_no_path /root/.hermes-integration/config.yaml
  assert_no_path /etc/systemd/system/hermes-gateway.service.d/90-proxyscene-telegram-proxy.conf
  for journal_path in \
    /opt/proxyscene/telegram-proxy-journal.json \
    /opt/proxyscene/telegram-proxy-journal.json.bak; do
    if [[ -e "$journal_path" ]]; then
      assert_json "$journal_path" '(.targets // {}) | has("hermes-gateway.service") | not' \
        "queue-policy rejection did not claim Hermes ownership"
    fi
  done
  assert_eq "$telegram_before" "$(jq -c '(.scene_enabled.telegram // false)' "$old_state")" \
    "queue-policy rejection preserved the prior Telegram scene state"
  assert_eq "$hermes_before" "$(target_start_count hermes)" "queue-policy rejection did not restart Hermes"
  service_is_active hermes-gateway.service || fail "queue-policy rejection stopped Hermes"
  printf 'HERMES_QUEUE_GUARD_OK no_config_write no_ownership no_restart\n'
  install -m 0600 /dev/stdin /root/.hermes-integration/config.yaml <<'HERMES_CONFIG'
platforms:
  telegram:
    extra:
      drop_pending_on_cold_boot: false
HERMES_CONFIG


  oc_user=proxyscene-oc-test
  useradd --create-home --shell /bin/bash "$oc_user"
  oc_uid="$(id -u "$oc_user")"
  oc_group="$(id -gn "$oc_user")"
  install -d -m 0755 /opt/openclaw /opt/openclaw/dist
  install -m 0644 /dev/stdin /opt/openclaw/dist/index.js <<'OPENCLAW_HELPER'
'use strict';

if (process.argv[2] !== 'gateway') {
  process.exit(2);
}
const fs = require('node:fs');
if (process.argv[3] === 'call') {
  fs.appendFileSync('/run/proxyscene-integration-targets/openclaw.rpc-calls', `${process.argv[4]}\n`);
  process.stderr.write('unsupported fixture RPC\n');
  process.exit(2);
}
fs.appendFileSync('/run/proxyscene-integration-targets/openclaw.starts', `${process.pid}\n`);
setInterval(() => {}, 2147483647);
OPENCLAW_HELPER
  install -d -o "$oc_user" -g "$oc_group" -m 0700 \
    "/home/$oc_user/.config" \
    "/home/$oc_user/.config/systemd" \
    "/home/$oc_user/.config/systemd/user" \
    "/home/$oc_user/.openclaw"
  install -o "$oc_user" -g "$oc_group" -m 0644 /dev/stdin \
    "/home/$oc_user/.config/systemd/user/openclaw-gateway.service" <<OPENCLAW_UNIT
[Unit]
Description=proxyscene integration OpenClaw gateway
# Lifecycle tests deliberately restart fixtures faster than normal gateways.
StartLimitIntervalSec=0

[Service]
Type=simple
Environment=OPENCLAW_SERVICE_MARKER=openclaw
Environment=OPENCLAW_SERVICE_KIND=gateway
Environment=HOME=/home/$oc_user
ExecStart=/usr/bin/node --max-old-space-size=256 /opt/openclaw/dist/index.js gateway

[Install]
WantedBy=default.target
OPENCLAW_UNIT
  openclaw_config="/home/$oc_user/.openclaw/openclaw.json"
  install -o "$oc_user" -g "$oc_group" -m 0600 /dev/stdin "$openclaw_config" <<'OPENCLAW_CONFIG'
{
  "preserved": {"value": 7},
  "channels": {
    "telegram": {
      "proxy": null,
      "preserved": true
    }
  }
}
OPENCLAW_CONFIG

  loginctl enable-linger "$oc_user"
  systemctl start -- "user-runtime-dir@${oc_uid}.service"
  systemctl start -- "user@${oc_uid}.service"
  wait_for "OpenClaw test user bus" test -S "/run/user/${oc_uid}/bus"
  user_systemctl "$oc_user" "$oc_uid" daemon-reload
  user_systemctl "$oc_user" "$oc_uid" enable --now -- openclaw-gateway.service
  wait_for "OpenClaw fixture" user_systemctl "$oc_user" "$oc_uid" \
    is-active --quiet -- openclaw-gateway.service
  wait_for "OpenClaw fixture process start" target_start_count_greater_than openclaw 0

  log "legacy Telegram files require exact Store ownership evidence"
  legacy_drop_in=/etc/systemd/system/hermes-gateway.service.d/10-openclaw-hermes-telegram-proxy.conf
  legacy_env=/etc/openclaw-hermes-tg-proxy.env
  install -d -m 0755 "$(dirname "$legacy_drop_in")"
  install -m 0644 /dev/stdin "$legacy_drop_in" <<'LEGACY_DROP_IN'
[Service]
EnvironmentFile=-/etc/openclaw-hermes-tg-proxy.env
LEGACY_DROP_IN
  install -m 0600 /dev/stdin "$legacy_env" <<'LEGACY_ENV'
# 由 proxyscene 管理
TELEGRAM_PROXY=http://127.0.0.1:7892
LEGACY_ENV

  /usr/local/bin/proxyscene tg off
  assert_file "$legacy_drop_in"
  assert_file "$legacy_env"
  assert_json "$old_state" '.telegram_targets | length == 0' \
    "legacy Telegram files retained without ownership evidence"

  legacy_state_tmp="$test_root/state-with-legacy-target.json"
  jq '.telegram_targets = ["hermes-gateway.service"]' "$old_state" > "$legacy_state_tmp"
  install -o root -g root -m 0600 "$legacy_state_tmp" "$old_state"
  /usr/local/bin/proxyscene tg off
  assert_no_path "$legacy_drop_in"
  assert_contains "$legacy_env" 'TELEGRAM_PROXY=http://127.0.0.1:7892' \
    "shared legacy Telegram env retained after exact migration"
  assert_json "$old_state" '.telegram_targets | length == 0' \
    "exact legacy Telegram ownership evidence cleared"
  rm -f -- "$legacy_env"

  hermes_before="$(target_start_count hermes)"
  openclaw_before="$(target_start_count openclaw)"

  env "PROXYSCENE_TG_SERVICES=hermes-gateway user:${oc_user}:openclaw-gateway" \
    /usr/local/bin/proxyscene tg on
  assert_json "$old_state" '.scene_enabled.telegram == true' "Telegram scene enabled"
  assert_json "$old_state" '.telegram_targets | length == 0' \
    "new Telegram ownership is not stored in legacy state"
  assert_no_path /etc/openclaw-hermes-tg-proxy.env
  assert_file \
    /etc/systemd/system/hermes-gateway.service.d/90-proxyscene-telegram-proxy.conf
  assert_contains \
    /etc/systemd/system/hermes-gateway.service.d/90-proxyscene-telegram-proxy.conf \
    'Environment="TELEGRAM_PROXY=http://127.0.0.1:7892"' \
    "Hermes direct Telegram proxy"
  assert_contains \
    /etc/systemd/system/hermes-gateway.service.d/90-proxyscene-telegram-proxy.conf \
    'Environment="HERMES_TELEGRAM_DISABLE_FALLBACK_IPS=1"' \
    "Hermes fallback IP discovery disabled"
  assert_no_path \
    /etc/systemd/system/hermes-gateway.service.d/10-openclaw-hermes-telegram-proxy.conf
  assert_no_path \
    "/home/$oc_user/.config/systemd/user/openclaw-gateway.service.d/10-openclaw-hermes-telegram-proxy.conf"
  assert_no_path \
    "/home/$oc_user/.config/systemd/user/openclaw-gateway.service.d/90-proxyscene-telegram-proxy.conf"
  for journal_path in \
    /opt/proxyscene/telegram-proxy-journal.json \
    /opt/proxyscene/telegram-proxy-journal.json.bak; do
    assert_file "$journal_path"
    assert_eq 600 "$(stat -c '%a' "$journal_path")" "Telegram journal mode"
    assert_json "$journal_path" \
      '.targets["hermes-gateway.service"].phase == "active"' \
      "Hermes ownership journal active"
  done
  assert_json "$openclaw_config" \
    '.channels.telegram.proxy == "http://127.0.0.1:7892" and .preserved.value == 7 and .channels.telegram.preserved == true' \
    "OpenClaw proxy applied without collateral changes"
  assert_file /opt/proxyscene/openclaw-proxy-journal.json
  assert_eq 600 "$(stat -c '%a' /opt/proxyscene/openclaw-proxy-journal.json)" \
    "OpenClaw journal mode"
  jq -e --arg user "$oc_user" '.users[$user].phase == "active"' \
    /opt/proxyscene/openclaw-proxy-journal.json >/dev/null \
    || fail "OpenClaw journal was not committed active"
  assert_json /opt/proxyscene/config.json \
    '.inbounds[] | select(.tag == "telegram-http" and .port == 7892)' \
    "Telegram HTTP inbound"
  assert_json /opt/proxyscene/config.json \
    '.inbounds[] | select(.tag == "telegram-socks" and .port == 7893)' \
    "Telegram SOCKS inbound"
  wait_for "Telegram HTTP listener" port_is_listening 7892
  wait_for "Telegram SOCKS listener" port_is_listening 7893
  wait_for "Hermes restart after proxy injection" \
    target_start_count_greater_than hermes "$hermes_before"
  wait_for "OpenClaw restart after proxy injection" \
    target_start_count_greater_than openclaw "$openclaw_before"
  assert_eq 1 "$(cat /run/proxyscene-integration-targets/hermes.fallback-ips-disabled)" \
    "running Hermes received the fallback-discovery guard"
  assert_contains /run/proxyscene-integration-targets/hermes.hooks 'gateway.systemd_stop_mark' \
    "official Hermes planned-stop hook executed"
  assert_contains /run/proxyscene-integration-targets/hermes.hooks 'gateway.cgroup_cleanup' \
    "official Hermes cleanup hook executed"
  assert_contains /run/proxyscene-integration-targets/openclaw.rpc-calls 'config.get' \
    "OpenClaw attempted the read-only RPC before falling back"
  assert_eq "$((openclaw_before + 1))" "$(target_start_count openclaw)" \
    "unsupported RPC caused exactly one OpenClaw restart"
  hermes_before="$(target_start_count hermes)"
  openclaw_before="$(target_start_count openclaw)"
  /usr/local/bin/proxyscene tg on
  count_after="$(target_start_count hermes)"
  assert_eq "$hermes_before" "$count_after" \
    "unchanged Hermes reconciliation did not restart the service"
  count_after="$(target_start_count openclaw)"
  assert_eq "$openclaw_before" "$count_after" \
    "unchanged OpenClaw reconciliation did not restart the service"

  hermes_before="$(target_start_count hermes)"
  openclaw_before="$(target_start_count openclaw)"
  systemctl start -- proxyscene-restore.service
  count_after="$(target_start_count hermes)"
  assert_eq "$hermes_before" "$count_after" \
    "unchanged boot restore did not restart Hermes"
  count_after="$(target_start_count openclaw)"
  assert_eq "$openclaw_before" "$count_after" \
    "unchanged boot restore did not restart OpenClaw"

  hermes_before="$(target_start_count hermes)"
  rm -f /run/proxyscene-telegram-reload-crash
  failure_output="$test_root/telegram-reload-crash.log"
  # User systemd commands run as their persisted UID; the shared helper must
  # be traversable by that UID instead of living below the root-only fixture.
  assert_no_path "$trusted_fault_bin"
  install -m 0755 "$fault_bin" "$trusted_fault_bin"
  expect_failure "Telegram cleanup crash after unlink" "$failure_output" \
    env \
      PROXYSCENE_FAULT_MODE=kill-daemon-reload \
      PROXYSCENE_FAULT_MARKER=/run/proxyscene-telegram-reload-crash \
      /usr/local/bin/proxyscene tg off
  rm -f -- "$trusted_fault_bin"
  if [[ ! -f /run/proxyscene-telegram-reload-crash ]]; then
    cat -- "$failure_output" >&2
    fail "Telegram cleanup did not reach the crash injection point"
  fi
  assert_json "$old_state" '.scene_enabled.telegram == true' \
    "crashed Telegram cleanup left durable enabled state"
  assert_no_path /etc/openclaw-hermes-tg-proxy.env
  assert_no_path \
    /etc/systemd/system/hermes-gateway.service.d/90-proxyscene-telegram-proxy.conf
  assert_json /opt/proxyscene/telegram-proxy-journal.json \
    '.targets["hermes-gateway.service"].phase == "restoring"' \
    "crashed Hermes cleanup retained restoring ownership"
  assert_json "$openclaw_config" \
    '.channels.telegram.proxy == "http://127.0.0.1:7892"' \
    "OpenClaw ownership survived earlier Hermes cleanup crash"

  assert_file /opt/proxyscene/runtime-transition.json
  expect_failure "Telegram mutation while recovery is pending" "$test_root/telegram-pending.log" \
    /usr/local/bin/proxyscene tg off
  /usr/local/bin/proxyscene recover
  assert_no_path /opt/proxyscene/runtime-transition.json
  assert_json "$old_state" '.scene_enabled.telegram == true' "Telegram recovery compensated to enabled scene"
  assert_file /etc/systemd/system/hermes-gateway.service.d/90-proxyscene-telegram-proxy.conf
  wait_for "Hermes compensation restart" target_start_count_greater_than hermes "$hermes_before"
  /usr/local/bin/proxyscene tg off
  assert_json "$old_state" '.scene_enabled.telegram == false' "Telegram retry disabled scene"
  assert_json "$old_state" '.telegram_targets | length == 0' \
    "Telegram retry retained an empty legacy target list"
  assert_no_path /etc/openclaw-hermes-tg-proxy.env
  assert_no_path \
    /etc/systemd/system/hermes-gateway.service.d/90-proxyscene-telegram-proxy.conf
  assert_json /opt/proxyscene/telegram-proxy-journal.json \
    '.targets | length == 0' "Telegram retry cleared Hermes ownership"
  assert_json /opt/proxyscene/telegram-proxy-journal.json.bak \
    '.targets | length == 0' "Telegram retry cleared Hermes backup ownership"
  assert_json "$openclaw_config" \
    '(.channels.telegram | has("proxy")) and .channels.telegram.proxy == null and .preserved.value == 7 and .channels.telegram.preserved == true' \
    "OpenClaw null proxy restored"
  jq -e --arg user "$oc_user" '.users | has($user) | not' \
    /opt/proxyscene/openclaw-proxy-journal.json >/dev/null \
    || fail "OpenClaw journal ownership remained after successful restore"
  wait_for "Hermes reconciliation restart after missing drop-in" \
    target_start_count_greater_than hermes "$hermes_before"
  assert_json /opt/proxyscene/config.json \
    '[.inbounds[] | select(.tag == "telegram-http" or .tag == "telegram-socks")] | length == 0' \
    "Telegram inbounds removed"
  wait_for "removed Telegram HTTP listener" port_is_not_listening 7892
  wait_for "removed Telegram SOCKS listener" port_is_not_listening 7893

  remove_all_nodes_canary "$test_root" "$old_state" "$node_url" "$oc_user" "$oc_uid" \
    "$openclaw_config" "$global_profile_original" "$global_apt_original"

  hermes_installation_layout_canary "$test_root" "$old_state"

  log "uninstall retries after restore-unit/main-unit partial completion"
  global_profile_concurrent='# operator replaced the profile while proxyscene was active'
  cp -p -- /etc/profile.d/proxyscene-global-proxy.sh "$test_root/global-owned-profile"
  cp -p -- "$old_state" "$test_root/global-before-conflict-state"
  printf '%s\n' "$global_profile_concurrent" > /etc/profile.d/proxyscene-global-proxy.sh
  chmod 0644 /etc/profile.d/proxyscene-global-proxy.sh
  expect_failure "global ownership divergence" "$test_root/global-ownership-conflict.log" \
    /usr/local/bin/proxyscene global off
  assert_contains "$test_root/global-ownership-conflict.log" 'ownership' "global conflict reason"
  cmp -- "$old_state" "$test_root/global-before-conflict-state" || fail "global conflict changed Store"
  assert_eq "$global_profile_concurrent" \
    "$(tr -d '\r\n' < /etc/profile.d/proxyscene-global-proxy.sh)" "global conflict retained administrator content"
  assert_no_path /opt/proxyscene/runtime-transition.json
  # Explicit fixture repair represents the operator resolving the ownership
  # conflict; no manager command is allowed to silently claim the replacement.
  cp -p -- "$test_root/global-owned-profile" /etc/profile.d/proxyscene-global-proxy.sh
  /usr/local/bin/proxyscene global off
  assert_eq "$global_profile_original" \
    "$(tr -d '\r\n' < /etc/profile.d/proxyscene-global-proxy.sh)" "global off restored original profile"
  printf '%s\n' "$global_profile_concurrent" > /etc/profile.d/proxyscene-global-proxy.sh
  chmod 0644 /etc/profile.d/proxyscene-global-proxy.sh
  assert_json "$old_state" \
    '(.scene_enabled.global == false) and (.scene_enabled.dev == false) and (.scene_enabled.telegram == false)' \
    "all scenes disabled before uninstall"
  assert_eq "$global_profile_concurrent" \
    "$(tr -d '\r\n' < /etc/profile.d/proxyscene-global-proxy.sh)" \
    "concurrent operator global profile preserved"
  assert_eq "$global_apt_original" \
    "$(tr -d '\r\n' < /etc/apt/apt.conf.d/99proxyscene-global-proxy)" \
    "operator apt proxy file restored"
  assert_eq 644 "$(stat -c '%a' /etc/profile.d/proxyscene-global-proxy.sh)" \
    "concurrent operator global profile mode preserved"
  assert_eq 600 "$(stat -c '%a' /etc/apt/apt.conf.d/99proxyscene-global-proxy)" \
    "operator apt proxy mode restored"
  assert_no_path /opt/proxyscene/global-proxy-journal.json
  assert_no_path /opt/proxyscene/global-proxy-journal.json.bak
  wait_for "idle Xray before uninstall" service_is_inactive proxyscene.service
  assert_file /etc/systemd/system/proxyscene.service
  assert_file /etc/systemd/system/proxyscene-restore.service
  rm -f /run/proxyscene-main-disable-fault
  failure_output="$test_root/uninstall-main-disable.log"
  expect_failure "uninstall with main-unit disable failure" "$failure_output" \
    env "PATH=$fault_dir:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
      PROXYSCENE_FAULT_MODE=main-disable \
      PROXYSCENE_FAULT_MARKER=/run/proxyscene-main-disable-fault \
      /usr/local/bin/proxyscene uninstall
  assert_file /run/proxyscene-main-disable-fault
  assert_no_path /etc/systemd/system/proxyscene-restore.service
  assert_file /etc/systemd/system/proxyscene.service
  /usr/local/bin/proxyscene uninstall
  assert_no_path /etc/systemd/system/proxyscene.service
  assert_no_path /etc/systemd/system/proxyscene-restore.service
  assert_no_path /etc/proxyscene-host-ownership.json
  wait_for "fully removed main service" unit_is_fully_removed proxyscene.service
  wait_for "fully removed restore service" unit_is_fully_removed proxyscene-restore.service

  log "uninstall retries after final daemon-reload failure"
  /usr/local/bin/proxyscene install --skip-node
  assert_file /etc/systemd/system/proxyscene.service
  assert_file /etc/systemd/system/proxyscene-restore.service
  rm -f /run/proxyscene-uninstall-reload-fault
  failure_output="$test_root/uninstall-daemon-reload.log"
  expect_failure "uninstall with daemon-reload failure" "$failure_output" \
    env "PATH=$fault_dir:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
      PROXYSCENE_FAULT_MODE=daemon-reload \
      PROXYSCENE_FAULT_MARKER=/run/proxyscene-uninstall-reload-fault \
      /usr/local/bin/proxyscene uninstall
  assert_file /run/proxyscene-uninstall-reload-fault
  assert_no_path /etc/systemd/system/proxyscene.service
  assert_no_path /etc/systemd/system/proxyscene-restore.service
  /usr/local/bin/proxyscene uninstall
  assert_no_path /etc/systemd/system/proxyscene.service
  assert_no_path /etc/systemd/system/proxyscene-restore.service
  assert_no_path /etc/proxyscene-host-ownership.json
  assert_json "$old_state" '.nodes | length == 1' "uninstall retained node state"
  assert_json "$old_state" \
    '(.scene_enabled.global == false) and (.scene_enabled.dev == false) and (.scene_enabled.telegram == false)' \
    "uninstall retained disabled scene state"
  assert_eq "$node_url" "$(jq -r '.nodes[0].raw_url' "$old_state")" \
    "uninstall retained original upgraded node"
  assert_eq "$global_profile_concurrent" \
    "$(tr -d '\r\n' < /etc/profile.d/proxyscene-global-proxy.sh)" \
    "uninstall preserved unowned global profile"
  assert_eq "$global_apt_original" \
    "$(tr -d '\r\n' < /etc/apt/apt.conf.d/99proxyscene-global-proxy)" \
    "uninstall preserved unowned apt proxy file"

	printf '\ncontainer integration assertions passed\n'
	cleanup_inside
	trap - EXIT
}

outer_cleanup() {
  local status=$?
  local cleanup_failed=0
  local cleanup_container_id="${TEST_CONTAINER_ID:-}" cleanup_label="" cleanup_name="" cleanup_attempt
  if [[ -n "$cleanup_container_id" ]] && \
    ! docker container inspect "$cleanup_container_id" >/dev/null 2>&1; then
    cleanup_container_id=""
  fi
  if [[ -z "$cleanup_container_id" && "${TEST_CONTAINER_ATTEMPTED:-0}" == "1" && -n "${TEST_CONTAINER_NAME:-}" ]]; then
    # A detached create can outlive an interrupted client before its ID reaches
    # this shell. Resolve only the exact random name and verify its run label.
    for ((cleanup_attempt = 0; cleanup_attempt < 10; cleanup_attempt++)); do
      cleanup_container_id="$(docker container inspect \
        --format '{{.Id}}' "$TEST_CONTAINER_NAME" 2>/dev/null || true)"
      [[ -z "$cleanup_container_id" ]] || break
      sleep 0.2
    done
  fi
  if [[ -n "$cleanup_container_id" ]]; then
    cleanup_name="$(docker container inspect --format '{{.Name}}' \
      "$cleanup_container_id" 2>/dev/null || true)"
    cleanup_label="$(docker container inspect \
      --format '{{index .Config.Labels "io.proxyscene.integration-run"}}' \
      "$cleanup_container_id" 2>/dev/null || true)"
    if [[ "$cleanup_name" != "/${TEST_CONTAINER_NAME:-}" || "$cleanup_label" != "${TEST_RUN_TOKEN:-}" ]]; then
      printf 'FAIL: 拒绝删除无法证明属于本轮的容器：%s（%s）\n' \
        "${TEST_CONTAINER_NAME:-unknown}" "$cleanup_container_id" >&2
      cleanup_failed=1
    elif ! docker rm --force -- "$cleanup_container_id" >/dev/null; then
      printf 'FAIL: 无法删除特权测试容器：%s（%s）\n' "${TEST_CONTAINER_NAME:-unknown}" "$cleanup_container_id" >&2
      cleanup_failed=1
    elif docker inspect "$cleanup_container_id" >/dev/null 2>&1; then
      printf 'FAIL: 特权测试容器在删除后仍然存在：%s（%s）\n' "${TEST_CONTAINER_NAME:-unknown}" "$cleanup_container_id" >&2
      cleanup_failed=1
    else
      printf 'CLEANUP_VERIFIED container=%s id=%s absent\n' "$TEST_CONTAINER_NAME" "$cleanup_container_id"
    fi
  fi
  if [[ "${TEST_IMAGE_PREEXISTED:-1}" == "0" ]] && \
    docker image inspect "$CONTAINER_IMAGE" >/dev/null 2>&1; then
    if ! docker image rm -- "$CONTAINER_IMAGE" >/dev/null; then
      printf 'FAIL: 无法删除本轮新拉取的镜像引用：%s\n' "$CONTAINER_IMAGE" >&2
      cleanup_failed=1
    elif docker image inspect "$CONTAINER_IMAGE" >/dev/null 2>&1; then
      printf 'FAIL: 本轮新拉取的镜像引用在删除后仍然存在：%s\n' "$CONTAINER_IMAGE" >&2
      cleanup_failed=1
    fi
  fi
  if [[ -n "${OUTER_TEST_ROOT:-}" && "$OUTER_TEST_ROOT" == /tmp/proxyscene-systemd-integration.* ]]; then
    if ! rm -rf -- "$OUTER_TEST_ROOT" || [[ -e "$OUTER_TEST_ROOT" || -L "$OUTER_TEST_ROOT" ]]; then
      printf 'FAIL: temporary test staging remained: %s\n' "$OUTER_TEST_ROOT" >&2
      cleanup_failed=1
    else
      printf 'CLEANUP_VERIFIED staging=%s absent\n' "$OUTER_TEST_ROOT"
    fi
  fi
  if (( status == 0 && cleanup_failed != 0 )); then
    return 1
  fi
  return "$status"
}

outer_main() {
  local current_bundle="$1"
  local old_bundle="$2"
  local script_path bootstrap_state container_id baseline_identity

  [[ "${PROXYSCENE_CONTAINER_TEST:-0}" == "1" ]] \
    || fail "refusing to run: set PROXYSCENE_CONTAINER_TEST=1 explicitly"
  [[ ! -e /.dockerenv ]] || fail "outer phase must run on the Docker host"
  require_command docker
  require_command readlink
  require_command install
  require_command mktemp

  current_bundle="$(readlink -e -- "$current_bundle")" \
    || fail "current amd64 bundle does not exist"
  old_bundle="$(readlink -e -- "$old_bundle")" \
    || fail "baseline amd64 bundle does not exist"
  assert_trusted_input_file "$current_bundle"
  assert_trusted_input_file "$old_bundle"
  baseline_identity="$(upgrade_baseline_identity "$(sha256_file "$old_bundle")")"
  log "verified upgrade baseline ${baseline_identity%% *}"
  script_path="$(readlink -e -- "${BASH_SOURCE[0]}")"
  assert_trusted_input_file "$script_path"

  OUTER_TEST_ROOT="$(mktemp -d /tmp/proxyscene-systemd-integration.XXXXXX)"
  TEST_CONTAINER_NAME="proxyscene-systemd-integration-$$-${RANDOM}"
  TEST_RUN_TOKEN="$TEST_CONTAINER_NAME"
  TEST_CONTAINER_ID=""
  TEST_CONTAINER_ATTEMPTED=0
  if docker image inspect "$CONTAINER_IMAGE" >/dev/null 2>&1; then
    TEST_IMAGE_PREEXISTED=1
  else
    TEST_IMAGE_PREEXISTED=0
  fi
  trap outer_cleanup EXIT
  trap 'exit 130' INT TERM HUP
  install -m 0444 "$current_bundle" "$OUTER_TEST_ROOT/current-amd64-bundle.tar.gz"
  install -m 0444 "$old_bundle" "$OUTER_TEST_ROOT/baseline-amd64-bundle.tar.gz"
  install -m 0555 "$script_path" "$OUTER_TEST_ROOT/systemd-integration-test.sh"

  log "starting isolated privileged Debian 13 systemd container"
  if docker container inspect "$TEST_CONTAINER_NAME" >/dev/null 2>&1; then
    fail "refusing to reuse an existing container name: $TEST_CONTAINER_NAME"
  fi
  TEST_CONTAINER_ATTEMPTED=1
  if ! container_id="$(docker run --detach \
    --name "$TEST_CONTAINER_NAME" \
    --label "io.proxyscene.integration-run=$TEST_RUN_TOKEN" \
    --hostname proxyscene-integration \
    --platform linux/amd64 \
    --privileged \
    --cgroupns private \
    --stop-signal SIGRTMIN+3 \
    --tmpfs /run:rw,nosuid,nodev,noexec,mode=0755 \
    --tmpfs /run/lock:rw,nosuid,nodev,noexec,mode=0755 \
    --tmpfs /tmp:rw,nosuid,nodev,mode=1777 \
    --mount "type=bind,source=$OUTER_TEST_ROOT,target=/artifacts,readonly" \
    "$CONTAINER_IMAGE" \
    bash -ceu '
      export DEBIAN_FRONTEND=noninteractive
      apt-get update
      apt-get install -y --no-install-recommends \
        bash ca-certificates coreutils dbus dbus-user-session git iproute2 jq \
        libpam-systemd npm passwd procps python3-venv systemd systemd-sysv tar unzip util-linux
      apt-get clean
      exec /sbin/init
    ')"; then
    fail "failed to create isolated Debian 13 container: $TEST_CONTAINER_NAME"
  fi
  TEST_CONTAINER_ID="$container_id"
  [[ "$container_id" =~ ^[0-9a-f]{64}$ ]] || fail "docker returned an invalid container id"
  printf 'CANARY_IDENTITY container=%s id=%s label=%s staging=%s image_preexisted=%s\n' \
    "$TEST_CONTAINER_NAME" "$TEST_CONTAINER_ID" "$TEST_RUN_TOKEN" "$OUTER_TEST_ROOT" "$TEST_IMAGE_PREEXISTED"

  bootstrap_state=""
  for _ in $(seq 1 180); do
    if ! docker inspect --format '{{.State.Running}}' "$TEST_CONTAINER_ID" 2>/dev/null \
      | grep -qx true; then
      docker logs "$TEST_CONTAINER_ID" >&2 || true
      fail "Debian 13 container exited during bootstrap"
    fi
    bootstrap_state="$(docker exec "$TEST_CONTAINER_ID" \
      systemctl is-system-running 2>/dev/null || true)"
    if [[ "$bootstrap_state" == "running" || "$bootstrap_state" == "degraded" ]]; then
      break
    fi
    sleep 1
  done
  if [[ "$bootstrap_state" != "running" && "$bootstrap_state" != "degraded" ]]; then
    docker logs "$TEST_CONTAINER_ID" >&2 || true
    fail "systemd did not become ready (state: ${bootstrap_state:-unknown})"
  fi

  docker exec \
    --env PROXYSCENE_CONTAINER_TEST=1 \
    "$TEST_CONTAINER_ID" \
    bash /artifacts/systemd-integration-test.sh --inside \
      "$CURRENT_CONTAINER_BUNDLE" "$BASELINE_CONTAINER_BUNDLE"
  log "all isolated systemd integration tests passed"
}

if [[ "${1:-}" == "--inside" ]]; then
  [[ $# -eq 3 ]] || fail "internal usage: --inside CURRENT_BUNDLE VERIFIED_BASELINE_BUNDLE"
  inside_container "$2" "$3"
else
  [[ $# -eq 2 ]] || fail "usage: PROXYSCENE_CONTAINER_TEST=1 $0 CURRENT_AMD64_BUNDLE VERIFIED_BASELINE_AMD64_BUNDLE"
  outer_main "$1" "$2"
fi
