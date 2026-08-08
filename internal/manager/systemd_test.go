package manager

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestRestoreServiceEnvironmentContainsOnlyLocators(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CoreDir = "/var/lib/proxyscene-custom"
	cfg.ProxyHost = "127.0.0.9"
	cfg.DevHTTPPort = 18091
	cfg.TGHTTPPort = 18092
	cfg.TGSocksPort = 18093
	cfg.GlobalHTTPPort = 18090
	cfg.GlobalSocksPort = 18094
	cfg.InstallBin = "/usr/local/sbin/proxyscene-custom"
	cfg.SystemdService = "proxyscene-custom.service"
	cfg.RestoreService = "proxyscene-custom-restore.service"
	cfg.XrayServiceUser = "proxycustom"
	cfg.TGTargetServices = []string{"hermes-gateway", "user:root:openclaw-gateway"}
	cfg.DevTargetUser = "root"
	cfg.TestURL = "https://user:secret@example.invalid/probe?token=secret"

	lines := restoreServiceEnvironmentLines(cfg)
	for _, want := range []string{
		"PROXYSCENE_MANAGER_DIR=/var/lib/proxyscene-custom",
		"PROXYSCENE_SWITCH_BIN=/usr/local/sbin/proxyscene-custom",
		"PROXYSCENE_SYSTEMD_SERVICE_NAME=proxyscene-custom.service",
		"PROXYSCENE_BOOT_RESTORE_SERVICE_NAME=proxyscene-custom-restore.service",
	} {
		if !strings.Contains(lines, want) {
			t.Errorf("restore environment missing %q:\n%s", want, lines)
		}
	}
	for _, forbidden := range []string{
		"PROXYSCENE_HOST", "PROXYSCENE_DEV_HTTP_PORT", "PROXYSCENE_TG_HTTP_PORT",
		"PROXYSCENE_TG_SOCKS_PORT", "PROXYSCENE_GLOBAL_HTTP_PORT", "PROXYSCENE_GLOBAL_SOCKS_PORT",
		"PROXYSCENE_SERVICE_USER", "PROXYSCENE_TG_SERVICES", "PROXYSCENE_DEV_TARGET_USER",
		"PROXYSCENE_ALLOW_PUBLIC_BIND", "PROXYSCENE_MANAGE_OPENCLAW_CONFIG", "PROXYSCENE_TEST_URL", "secret",
	} {
		if strings.Contains(lines, forbidden) {
			t.Fatalf("runtime or secret value %q leaked into restore unit:\n%s", forbidden, lines)
		}
	}
}

func TestStopXrayServiceAggregatesFailuresAndVerifiesInactive(t *testing.T) {
	oldRun := systemctlRun
	oldOutput := systemctlOutput
	t.Cleanup(func() {
		systemctlRun = oldRun
		systemctlOutput = oldOutput
	})
	calls := []string{}
	systemctlRun = func(_ string, args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return errors.New("command failed")
	}
	systemctlOutput = func(_ string, args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		return "active\n", nil
	}

	err := testApp(t).stopXrayService()
	if err == nil || !strings.Contains(err.Error(), "仍处于 \"active\"") {
		t.Fatalf("stop/disable/state failures should be aggregated: %v", err)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{"stop --", "disable --", "show --property=ActiveState"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing systemctl call %q:\n%s", want, joined)
		}
	}
}

func TestRestartXrayServiceResetsRateLimitBeforeRestart(t *testing.T) {
	oldRun := systemctlRun
	oldOutput := systemctlOutput
	t.Cleanup(func() {
		systemctlRun = oldRun
		systemctlOutput = oldOutput
	})

	var calls []string
	systemctlRun = func(_ string, args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	systemctlOutput = func(_ string, args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		return "active\n", nil
	}
	a := testApp(t)
	if err := a.restartXrayService(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"reset-failed -- " + a.cfg.SystemdService,
		"restart -- " + a.cfg.SystemdService,
		"show --property=ActiveState --value -- " + a.cfg.SystemdService,
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("systemctl calls=%v, want %v", calls, want)
	}

	calls = nil
	systemctlRun = func(_ string, args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return errors.New("injected reset failure")
	}
	if err := a.restartXrayService(); err == nil {
		t.Fatal("reset-failed error was ignored")
	}
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "reset-failed -- ") {
		t.Fatalf("restart proceeded after reset failure: %v", calls)
	}

	calls = nil
	systemctlRun = func(_ string, args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	systemctlOutput = func(_ string, args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		return "failed\n", nil
	}
	if err := a.restartXrayService(); err == nil || !strings.Contains(err.Error(), `"failed"`) {
		t.Fatalf("non-active state was accepted: %v", err)
	}
}

func TestPrepareXrayServiceRuntimeIgnoresUnusedGeoData(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to verify ownership remains unchanged")
	}
	identity, err := lookupLocalUserIdentity("nobody")
	if err != nil || identity.GID == 0 {
		t.Skip("requires an existing unprivileged nobody account")
	}
	cfg := DefaultConfig()
	cfg.CoreDir = t.TempDir()
	cfg.XrayServiceUser = identity.Name
	for path, content := range map[string]string{
		cfg.XrayBin():                             "xray",
		cfg.XrayConfig():                          "{}",
		filepath.Join(cfg.CoreDir, "geoip.dat"):   "unused geoip",
		filepath.Join(cfg.CoreDir, "geosite.dat"): "unused geosite",
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	type metadata struct {
		mode os.FileMode
		uid  uint32
		gid  uint32
	}
	readMetadata := func(path string) metadata {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		stat := info.Sys().(*syscall.Stat_t)
		return metadata{mode: info.Mode().Perm(), uid: stat.Uid, gid: stat.Gid}
	}
	geoPaths := []string{filepath.Join(cfg.CoreDir, "geoip.dat"), filepath.Join(cfg.CoreDir, "geosite.dat")}
	before := map[string]metadata{}
	for _, path := range geoPaths {
		before[path] = readMetadata(path)
	}

	if err := NewApp(cfg).prepareXrayServiceRuntime(); err != nil {
		t.Fatal(err)
	}
	for _, path := range geoPaths {
		if after := readMetadata(path); after != before[path] {
			t.Errorf("unused geodata metadata changed: path=%s before=%+v after=%+v", path, before[path], after)
		}
	}
}

func TestValidatePrivilegedExecutable(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root ownership to exercise privileged executable validation")
	}
	dir, err := os.MkdirTemp("/root", ".proxyscene-exec-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "proxyscene")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := validatePrivilegedExecutable(path, "test"); err != nil {
		t.Fatalf("trusted executable rejected: %v", err)
	}
	if err := os.Chmod(path, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := validatePrivilegedExecutable(path, "test"); err == nil {
		t.Fatal("group-writable privileged executable must be rejected")
	}
}
