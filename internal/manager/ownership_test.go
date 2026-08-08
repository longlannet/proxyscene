package manager

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "proxyscene-manager-test-locks-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create isolated manager test lock directory: %v\n", err)
		os.Exit(2)
	}
	oldInstallLockPath := installLockPath
	oldOwnershipPath := hostOwnershipPath
	oldHostLockPath := hostLockPath
	installLockPath = filepath.Join(root, "install.lock")
	hostOwnershipPath = filepath.Join(root, "host-ownership.json")
	hostLockPath = filepath.Join(root, "host-ownership.lock")

	code := m.Run()
	installLockPath = oldInstallLockPath
	hostOwnershipPath = oldOwnershipPath
	hostLockPath = oldHostLockPath
	if cleanupErr := os.RemoveAll(root); cleanupErr != nil && code == 0 {
		fmt.Fprintf(os.Stderr, "remove isolated manager test lock directory: %v\n", cleanupErr)
		code = 1
	}
	os.Exit(code)
}

func TestEnsureCoreDirsRefusesUnmarkedNonEmptyDirectory(t *testing.T) {
	core := t.TempDir()
	operatorFile := filepath.Join(core, "operator-data")
	if err := os.WriteFile(operatorFile, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.CoreDir = core
	a := NewApp(cfg)
	if err := a.ensureCoreDirs(); err == nil || !strings.Contains(err.Error(), "拒绝接管") {
		t.Fatalf("unmarked non-empty directory was not rejected: %v", err)
	}
	data, err := os.ReadFile(operatorFile)
	if err != nil || string(data) != "keep\n" {
		t.Fatalf("operator file changed: data=%q err=%v", data, err)
	}
	if _, err := os.Lstat(cfg.MarkerPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected directory gained a marker: %v", err)
	}
}

func TestInstallationOwnershipBindsEveryLocator(t *testing.T) {
	a := testApp(t)
	changed := a.cfg
	changed.SystemdService = "proxyscene-other.service"
	if err := NewApp(changed).validateInstallationOwnership(); err == nil || !strings.Contains(err.Error(), "locator") {
		t.Fatalf("locator mismatch was not rejected: %v", err)
	}
}

func TestHostOwnershipRejectsSecondInstallationAndReleasesExactly(t *testing.T) {
	root := t.TempDir()
	oldOwnershipPath := hostOwnershipPath
	oldLockPath := hostLockPath
	hostOwnershipPath = filepath.Join(root, "host-ownership.json")
	hostLockPath = filepath.Join(root, "host-ownership.lock")
	t.Cleanup(func() {
		hostOwnershipPath = oldOwnershipPath
		hostLockPath = oldLockPath
	})

	firstCfg := DefaultConfig()
	first := NewApp(firstCfg)
	called := false
	if err := first.withHostOwnership(func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("host ownership callback was not called")
	}
	info, err := os.Lstat(hostOwnershipPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("host ownership mode=%v, want regular 0600", info.Mode())
	}

	secondCfg := firstCfg
	secondCfg.CoreDir = "/var/lib/proxyscene-second"
	secondCfg.InstallBin = "/usr/local/bin/proxyscene-second/proxyscene"
	secondCfg.SystemdService = "proxyscene-second.service"
	secondCfg.RestoreService = "proxyscene-second-restore.service"
	second := NewApp(secondCfg)
	if err := second.withHostOwnership(func() error {
		t.Fatal("second installation reached protected callback")
		return nil
	}); err == nil || !strings.Contains(err.Error(), "另一套") {
		t.Fatalf("second installation was not rejected: %v", err)
	}
	if err := second.releaseHostOwnership(); err == nil {
		t.Fatal("second installation released first installation ownership")
	}
	if _, err := os.Lstat(hostOwnershipPath); err != nil {
		t.Fatalf("mismatched release changed host ownership: %v", err)
	}

	if err := first.releaseHostOwnership(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(hostOwnershipPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("host ownership remained after exact release: %v", err)
	}
}

func TestHostOwnershipCallbackFailureRetainsClaimForRetry(t *testing.T) {
	root := t.TempDir()
	oldOwnershipPath := hostOwnershipPath
	oldLockPath := hostLockPath
	hostOwnershipPath = filepath.Join(root, "host-ownership.json")
	hostLockPath = filepath.Join(root, "host-ownership.lock")
	t.Cleanup(func() {
		hostOwnershipPath = oldOwnershipPath
		hostLockPath = oldLockPath
	})

	owner := NewApp(DefaultConfig())
	wantErr := errors.New("injected callback failure")
	if err := owner.withHostOwnership(func() error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("callback error = %v, want %v", err, wantErr)
	}
	if _, err := loadOwnershipRecord(hostOwnershipPath, "test host ownership"); err != nil {
		t.Fatalf("failed callback lost durable host claim: %v", err)
	}
	retried := false
	if err := owner.withHostOwnership(func() error { retried = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !retried {
		t.Fatal("same owner could not retry after callback failure")
	}
}

func TestReleaseHostOwnershipConvergesAfterDirectorySyncFails(t *testing.T) {
	root := t.TempDir()
	oldOwnershipPath := hostOwnershipPath
	oldSync := hostOwnershipSync
	hostOwnershipPath = filepath.Join(root, "host-ownership.json")
	syncCalls := 0
	hostOwnershipSync = func(string) error {
		syncCalls++
		if syncCalls == 1 {
			return errors.New("injected directory sync failure")
		}
		return nil
	}
	t.Cleanup(func() {
		hostOwnershipPath = oldOwnershipPath
		hostOwnershipSync = oldSync
	})

	owner := NewApp(DefaultConfig())
	data, err := marshalInstallationOwnership(owner.expectedInstallationOwnership())
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(hostOwnershipPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := owner.releaseHostOwnership(); err == nil || !strings.Contains(err.Error(), "injected directory sync failure") {
		t.Fatalf("release error = %v", err)
	}
	if _, err := os.Lstat(hostOwnershipPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("release did not unlink ownership before sync failure: %v", err)
	}
	if err := owner.releaseHostOwnership(); err != nil {
		t.Fatalf("missing-record retry did not converge: %v", err)
	}
	if syncCalls != 2 {
		t.Fatalf("directory sync calls = %d, want 2", syncCalls)
	}
}

func TestEnsureInstallationOwnershipRequiresRunningConfiguredBinary(t *testing.T) {
	core := t.TempDir()
	binDir := t.TempDir()
	cfg := DefaultConfig()
	cfg.CoreDir = core
	cfg.InstallBin = filepath.Join(binDir, "proxyscene")
	if err := os.WriteFile(cfg.InstallBin, []byte("not this test executable\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	a := NewApp(cfg)
	if err := a.ensureCoreDirs(); err != nil {
		t.Fatal(err)
	}
	if err := a.ensureInstallationOwnership(); err == nil || !strings.Contains(err.Error(), "不是同一文件") {
		t.Fatalf("unrelated configured binary was accepted: %v", err)
	}
}

func TestUnitReplacementAndRemovalRequireOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxyscene-collision.service")
	original := []byte("[Service]\nExecStart=/usr/bin/true\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateUnitReplacementOwnership(path, "ExecStart=/opt/proxyscene/xray run -config /opt/proxyscene/config.json"); err == nil {
		t.Fatal("unmanaged existing unit was accepted for replacement")
	}
	if err := validateManagedUnitForRemoval(path); err == nil {
		t.Fatal("unmanaged existing unit was accepted for removal")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(original) {
		t.Fatalf("unmanaged unit changed: data=%q err=%v", data, err)
	}
}

func TestConfigRejectsSystemdLocatorOutsideNamespace(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SystemdService = "ssh.service"
	if err := cfg.ValidateLocators(); err == nil || !strings.Contains(err.Error(), "proxyscene") {
		t.Fatalf("arbitrary systemd locator was accepted: %v", err)
	}
}

func TestWithFileLockOrInheritedReusesExactHeldLock(t *testing.T) {
	root, err := os.MkdirTemp(".", ".proxyscene-inherited-lock-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "inherited.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROXYSCENE_TEST_INHERITED_LOCK_FD", strconv.Itoa(int(f.Fd())))

	called := false
	if err := withFileLockOrInherited(path, "PROXYSCENE_TEST_INHERITED_LOCK_FD", func() error {
		called = true
		competitor, openErr := os.OpenFile(path, os.O_RDWR, 0)
		if openErr != nil {
			return openErr
		}
		defer competitor.Close()
		if lockErr := syscall.Flock(int(competitor.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); lockErr == nil {
			return fmt.Errorf("independent open file description acquired inherited lock")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("inherited lock callback was not called")
	}
}

func TestWithFileLockOrInheritedRejectsWrongInodeAndMode(t *testing.T) {
	root, err := os.MkdirTemp(".", ".proxyscene-inherited-lock-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "expected.lock")
	other := filepath.Join(root, "other.lock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(other, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	t.Setenv("PROXYSCENE_TEST_INHERITED_LOCK_FD", strconv.Itoa(int(f.Fd())))
	if err := withFileLockOrInherited(path, "PROXYSCENE_TEST_INHERITED_LOCK_FD", func() error { return nil }); err == nil {
		t.Fatal("wrong inherited lock inode was accepted")
	}

	if err := os.Chmod(other, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := withFileLockOrInherited(other, "PROXYSCENE_TEST_INHERITED_LOCK_FD", func() error { return nil }); os.Geteuid() == 0 && err == nil {
		t.Fatal("non-root-only inherited lock mode was accepted")
	}
}

func TestInstallerLockHandoffCombinationTable(t *testing.T) {
	tests := []struct {
		name          string
		install, host bool
		store         bool
		wantHandoff   bool
		wantErr       bool
	}{
		{name: "direct", wantHandoff: false},
		{name: "fresh installer", install: true, host: true, wantHandoff: true},
		{name: "existing installer", install: true, host: true, store: true, wantHandoff: true},
		{name: "install only", install: true, wantErr: true},
		{name: "host only", host: true, wantErr: true},
		{name: "state only", store: true, wantErr: true},
		{name: "host state", host: true, store: true, wantErr: true},
		{name: "install state", install: true, store: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value := func(present bool) string {
				if present {
					return "9"
				}
				return ""
			}
			t.Setenv(inheritedInstallLockFDEnv, value(tt.install))
			t.Setenv(inheritedHostLockFDEnv, value(tt.host))
			t.Setenv(inheritedStoreLockFDEnv, value(tt.store))
			got, err := installerLockHandoffRequested()
			if (err != nil) != tt.wantErr {
				t.Fatalf("installerLockHandoffRequested() error = %v, wantErr=%v", err, tt.wantErr)
			}
			if got != tt.wantHandoff {
				t.Fatalf("installerLockHandoffRequested() = %v, want %v", got, tt.wantHandoff)
			}
		})
	}
}
