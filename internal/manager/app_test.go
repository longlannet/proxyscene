package manager

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestInstallWithStoreCreatesUnitsBeforeFirstNodeCommit(t *testing.T) {
	a := testApp(t)
	st := newStore()
	events := []string{}
	step := func(name string) func() error {
		return func() error {
			events = append(events, name)
			return nil
		}
	}
	commit := func(state *Store, mutate func(*Store) error, mode storeRuntimeSyncMode) error {
		events = append(events, "commit")
		if !reflect.DeepEqual(events, []string{"xray", "main-unit", "restore-unit", "commit"}) {
			t.Fatalf("unsafe install order: %v", events)
		}
		if mode != storeRuntimeSyncXray {
			t.Fatalf("fresh idle install mode=%d, want Xray-only sync", mode)
		}
		return mutate(state)
	}
	raw := "trojan://secret@example.com:443"
	if err := a.installWithStore(st, raw, step("xray"), step("main-unit"), step("restore-unit"), commit); err != nil {
		t.Fatal(err)
	}
	if len(st.Nodes) != 1 || st.Nodes[0].RawURL != raw {
		t.Fatalf("first node was not committed: %+v", st.Nodes)
	}
}

func TestInstallPreparedCreatesOwnershipOnlyInsideAllLocks(t *testing.T) {
	root := t.TempDir()
	oldInstallLockPath := installLockPath
	oldOwnershipPath := hostOwnershipPath
	oldHostLockPath := hostLockPath
	installLockPath = filepath.Join(root, "install.lock")
	hostOwnershipPath = filepath.Join(root, "host-ownership.json")
	hostLockPath = filepath.Join(root, "host-ownership.lock")
	t.Cleanup(func() {
		installLockPath = oldInstallLockPath
		hostOwnershipPath = oldOwnershipPath
		hostLockPath = oldHostLockPath
	})

	running, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.CoreDir = filepath.Join(root, "core")
	cfg.InstallBin = running
	a := NewApp(cfg)

	installLock, err := os.OpenFile(installLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer installLock.Close()
	if err := syscall.Flock(int(installLock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	hostLock, err := os.OpenFile(hostLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer hostLock.Close()
	if err := syscall.Flock(int(hostLock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.installPrepared("") }()

	select {
	case err := <-done:
		t.Fatalf("install returned before held install lock was released: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := os.Lstat(cfg.CoreDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("install changed CoreDir before acquiring install lock: %v", err)
	}
	if err := syscall.Flock(int(installLock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	waitForPath := func(path string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, statErr := os.Lstat(path); statErr == nil {
				return
			} else if !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("waiting for %s: %v", path, statErr)
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", path)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitForPath(cfg.CoreDir)
	if _, err := os.Lstat(cfg.InstallationOwnershipPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("install created ownership before acquiring host lock: %v", err)
	}
	storeLock, err := os.OpenFile(cfg.StoreLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer storeLock.Close()
	if err := syscall.Flock(int(storeLock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(hostLock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	waitForPath(hostOwnershipPath)
	if _, err := os.Lstat(cfg.InstallationOwnershipPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("install created ownership before acquiring state lock: %v", err)
	}
	if err := syscall.Flock(int(storeLock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "Xray") {
			t.Fatalf("install after lock release error = %v, want missing Xray", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("install did not continue after install lock release")
	}
	if _, err := os.Lstat(cfg.InstallationOwnershipPath()); err != nil {
		t.Fatalf("install did not create ownership inside all three locks: %v", err)
	}
}

func TestInstallPreparedRejectsPartialInstallerLockHandoffBeforeChanges(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CoreDir = filepath.Join(t.TempDir(), "core")
	a := NewApp(cfg)
	t.Setenv(inheritedInstallLockFDEnv, "9")
	t.Setenv(inheritedHostLockFDEnv, "")
	t.Setenv(inheritedStoreLockFDEnv, "")

	err := a.installPrepared("")
	if err == nil || !strings.Contains(err.Error(), "安装器锁交接组合无效") {
		t.Fatalf("partial installer lock handoff error = %v", err)
	}
	if _, statErr := os.Lstat(cfg.CoreDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("partial installer lock handoff changed CoreDir: %v", statErr)
	}
}

func TestInstallPreparedHostOwnershipConflictDoesNotCreateInstallationOwnership(t *testing.T) {
	root := t.TempDir()
	oldInstallLockPath := installLockPath
	oldOwnershipPath := hostOwnershipPath
	oldHostLockPath := hostLockPath
	installLockPath = filepath.Join(root, "install.lock")
	hostOwnershipPath = filepath.Join(root, "host-ownership.json")
	hostLockPath = filepath.Join(root, "host-ownership.lock")
	t.Cleanup(func() {
		installLockPath = oldInstallLockPath
		hostOwnershipPath = oldOwnershipPath
		hostLockPath = oldHostLockPath
	})

	running, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.CoreDir = filepath.Join(root, "core")
	cfg.InstallBin = running
	a := NewApp(cfg)
	conflicting := a.expectedInstallationOwnership()
	conflicting.CoreDir = filepath.Join(root, "another-core")
	data, err := marshalInstallationOwnership(conflicting)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(hostOwnershipPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	err = a.installPrepared("")
	if err == nil || !strings.Contains(err.Error(), "主机已由另一套 proxyscene 安装接管") {
		t.Fatalf("host ownership conflict error = %v", err)
	}
	if _, statErr := os.Lstat(cfg.InstallationOwnershipPath()); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("host ownership conflict wrote installation ownership: %v", statErr)
	}
}

func TestInstallerLockHandoffRequiresPrecommittedInstallationOwnership(t *testing.T) {
	root, err := os.MkdirTemp(".", ".proxyscene-installer-handoff-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	oldInstallLockPath := installLockPath
	oldOwnershipPath := hostOwnershipPath
	oldHostLockPath := hostLockPath
	installLockPath = filepath.Join(root, "install.lock")
	hostOwnershipPath = filepath.Join(root, "host-ownership.json")
	hostLockPath = filepath.Join(root, "host-ownership.lock")
	t.Cleanup(func() {
		installLockPath = oldInstallLockPath
		hostOwnershipPath = oldOwnershipPath
		hostLockPath = oldHostLockPath
	})

	running, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.CoreDir = filepath.Join(root, "core")
	cfg.InstallBin = running
	a := NewApp(cfg)
	if err := a.ensureCoreDirs(); err != nil {
		t.Fatal(err)
	}

	lockPaths := []string{installLockPath, hostLockPath, cfg.StoreLockPath()}
	lockEnvs := []string{inheritedInstallLockFDEnv, inheritedHostLockFDEnv, inheritedStoreLockFDEnv}
	for i, path := range lockPaths {
		lock, openErr := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if openErr != nil {
			t.Fatal(openErr)
		}
		defer lock.Close()
		if lockErr := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); lockErr != nil {
			t.Fatal(lockErr)
		}
		t.Setenv(lockEnvs[i], strconv.Itoa(int(lock.Fd())))
	}

	err = a.installPrepared("")
	if err == nil || !strings.Contains(err.Error(), "缺少安装 ownership 记录") {
		t.Fatalf("installer handoff without precommitted ownership error = %v", err)
	}
	if _, statErr := os.Lstat(cfg.InstallationOwnershipPath()); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("installer handoff synthesized missing ownership: %v", statErr)
	}
	if err := a.writeInstallationOwnership(a.expectedInstallationOwnership()); err != nil {
		t.Fatal(err)
	}
	err = a.installPrepared("")
	if err == nil || !strings.Contains(err.Error(), "Xray") {
		t.Fatalf("installer handoff did not accept precommitted ownership: %v", err)
	}
}

func TestFreshInstallerLockHandoffAcquiresStateLockInManager(t *testing.T) {
	root, err := os.MkdirTemp(".", ".proxyscene-fresh-handoff-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	oldInstallLockPath := installLockPath
	oldOwnershipPath := hostOwnershipPath
	oldHostLockPath := hostLockPath
	installLockPath = filepath.Join(root, "install.lock")
	hostOwnershipPath = filepath.Join(root, "host-ownership.json")
	hostLockPath = filepath.Join(root, "host-ownership.lock")
	t.Cleanup(func() {
		installLockPath = oldInstallLockPath
		hostOwnershipPath = oldOwnershipPath
		hostLockPath = oldHostLockPath
	})

	running, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.CoreDir = filepath.Join(root, "core")
	cfg.InstallBin = running
	a := NewApp(cfg)
	if err := a.ensureCoreDirs(); err != nil {
		t.Fatal(err)
	}
	if err := a.writeInstallationOwnership(a.expectedInstallationOwnership()); err != nil {
		t.Fatal(err)
	}

	lockPaths := []string{installLockPath, hostLockPath}
	lockEnvs := []string{inheritedInstallLockFDEnv, inheritedHostLockFDEnv}
	for i, path := range lockPaths {
		lock, openErr := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if openErr != nil {
			t.Fatal(openErr)
		}
		defer lock.Close()
		if lockErr := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); lockErr != nil {
			t.Fatal(lockErr)
		}
		t.Setenv(lockEnvs[i], strconv.Itoa(int(lock.Fd())))
	}
	t.Setenv(inheritedStoreLockFDEnv, "")

	err = a.installPrepared("")
	if err == nil || !strings.Contains(err.Error(), "Xray") {
		t.Fatalf("fresh installer handoff did not reach manager initialization: %v", err)
	}
	info, statErr := os.Lstat(cfg.StoreLockPath())
	if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("manager did not create a regular 0600 state lock: info=%v err=%v", info, statErr)
	}
}

func TestDisableAndRemoveSystemdUnitPreservesUnitWhenDisableFails(t *testing.T) {
	originalSystemctlRun := systemctlRun
	t.Cleanup(func() { systemctlRun = originalSystemctlRun })

	unitPath := filepath.Join(t.TempDir(), "proxyscene.service")
	if err := os.WriteFile(unitPath, []byte(managedSystemdUnitHeader+"unit"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("disable failed")
	var gotArgs []string
	systemctlRun = func(_ string, args ...string) error {
		gotArgs = append([]string(nil), args...)
		return wantErr
	}

	err := disableAndRemoveSystemdUnit("proxyscene.service", unitPath, "Xray 主服务")
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "已保留 unit 文件") {
		t.Fatalf("disableAndRemoveSystemdUnit() error = %v", err)
	}
	if _, statErr := os.Stat(unitPath); statErr != nil {
		t.Fatalf("unit should remain after disable failure: %v", statErr)
	}
	wantArgs := []string{"disable", "--now", "--", "proxyscene.service"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("systemctl args = %#v, want %#v", gotArgs, wantArgs)
	}
}

func TestDisableAndRemoveSystemdUnitRemovesUnitAfterDisable(t *testing.T) {
	originalSystemctlRun := systemctlRun
	t.Cleanup(func() { systemctlRun = originalSystemctlRun })

	unitPath := filepath.Join(t.TempDir(), "proxyscene-restore.service")
	if err := os.WriteFile(unitPath, []byte(managedSystemdUnitHeader+"unit"), 0o600); err != nil {
		t.Fatal(err)
	}
	systemctlRun = func(_ string, _ ...string) error { return nil }

	if err := disableAndRemoveSystemdUnit("proxyscene-restore.service", unitPath, "开机恢复服务"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(unitPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unit should be removed, stat error = %v", err)
	}
}

func TestDisableAndRemoveSystemdUnitMissingNotFoundIsIdempotent(t *testing.T) {
	oldRun := systemctlRun
	oldOutput := systemctlOutput
	t.Cleanup(func() {
		systemctlRun = oldRun
		systemctlOutput = oldOutput
	})
	systemctlRun = func(string, ...string) error {
		t.Fatal("already absent/not-found unit must not be mutated")
		return nil
	}
	systemctlOutput = func(string, ...string) (string, error) {
		return "LoadState=not-found\nActiveState=inactive\n", nil
	}
	if err := disableAndRemoveSystemdUnit("proxyscene-missing.service", filepath.Join(t.TempDir(), "proxyscene-missing.service"), "缺失服务"); err != nil {
		t.Fatal(err)
	}
}

func TestDisableAndRemoveSystemdUnitReconcilesUnlinkedLoadedUnit(t *testing.T) {
	oldRun := systemctlRun
	oldOutput := systemctlOutput
	t.Cleanup(func() {
		systemctlRun = oldRun
		systemctlOutput = oldOutput
	})
	queries := 0
	systemctlOutput = func(string, ...string) (string, error) {
		queries++
		if queries == 1 {
			return "LoadState=loaded\nActiveState=inactive\n", nil
		}
		return "LoadState=not-found\nActiveState=inactive\n", nil
	}
	calls := []string{}
	systemctlRun = func(_ string, args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	if err := disableAndRemoveSystemdUnit("proxyscene-stale.service", filepath.Join(t.TempDir(), "proxyscene-stale.service"), "残留服务"); err != nil {
		t.Fatal(err)
	}
	want := []string{"disable --now -- proxyscene-stale.service", "daemon-reload"}
	if !reflect.DeepEqual(calls, want) || queries != 2 {
		t.Fatalf("reconcile calls=%v queries=%d, want=%v/2", calls, queries, want)
	}
}

func TestDisableAndRemoveSystemdUnitRejectsUnconfirmedMissingUnit(t *testing.T) {
	oldOutput := systemctlOutput
	t.Cleanup(func() { systemctlOutput = oldOutput })
	systemctlOutput = func(string, ...string) (string, error) {
		return "garbled", nil
	}
	if err := disableAndRemoveSystemdUnit("proxyscene-unknown.service", filepath.Join(t.TempDir(), "proxyscene-unknown.service"), "未知服务"); err == nil {
		t.Fatal("missing unit with unknown systemd state must fail closed")
	}
}

func TestDisableScenesForUninstallStopsAtFirstFailure(t *testing.T) {
	wantErr := errors.New("dev cleanup failed")
	var calls []Scene
	err := disableScenesForUninstall(newStore(), func(_ *Store, scene Scene, enabled bool) error {
		if enabled {
			t.Fatalf("uninstall attempted to enable %s", scene)
		}
		calls = append(calls, scene)
		if scene == SceneDev {
			return wantErr
		}
		return nil
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("disableScenesForUninstall() error = %v", err)
	}
	wantCalls := []Scene{SceneTelegram, SceneDev}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("scene calls = %#v, want %#v", calls, wantCalls)
	}
}

func TestRemoveSystemdUnitsForUninstallStopsWhenRestoreFails(t *testing.T) {
	wantErr := errors.New("restore disable failed")
	cfg := Config{SystemdService: "proxyscene.service", RestoreService: "proxyscene-restore.service"}
	var calls []string
	reloaded := false
	err := removeSystemdUnitsForUninstall(cfg, func(service, _, _ string) error {
		calls = append(calls, service)
		return wantErr
	}, func() error {
		reloaded = true
		return nil
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("removeSystemdUnitsForUninstall() error = %v", err)
	}
	if !reflect.DeepEqual(calls, []string{cfg.RestoreService}) {
		t.Fatalf("unit calls = %#v", calls)
	}
	if reloaded {
		t.Fatalf("daemon-reload ran even though restore unit was preserved")
	}
}

func TestRemoveSystemdUnitsForUninstallOrdersUnitsAndJoinsReloadFailure(t *testing.T) {
	wantMainErr := errors.New("main disable failed")
	wantReloadErr := errors.New("daemon reload failed")
	cfg := Config{SystemdService: "proxyscene.service", RestoreService: "proxyscene-restore.service"}
	type removal struct {
		service string
		path    string
		label   string
	}
	var calls []removal
	err := removeSystemdUnitsForUninstall(cfg, func(service, path, label string) error {
		calls = append(calls, removal{service: service, path: path, label: label})
		if service == cfg.SystemdService {
			return wantMainErr
		}
		return nil
	}, func() error { return wantReloadErr })
	if !errors.Is(err, wantMainErr) || !errors.Is(err, wantReloadErr) {
		t.Fatalf("joined error = %v", err)
	}
	wantCalls := []removal{
		{service: cfg.RestoreService, path: "/etc/systemd/system/proxyscene-restore.service", label: "开机恢复服务"},
		{service: cfg.SystemdService, path: "/etc/systemd/system/proxyscene.service", label: "Xray 主服务"},
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("unit calls = %#v, want %#v", calls, wantCalls)
	}
}

func TestRemoveSystemdUnitsForUninstallRetriesAfterRestoreWasRemoved(t *testing.T) {
	cfg := Config{SystemdService: "proxyscene.service", RestoreService: "proxyscene-restore.service"}
	removed := map[string]bool{}
	mainFailures := 0
	remove := func(service, _, _ string) error {
		if removed[service] {
			return nil
		}
		if service == cfg.SystemdService && mainFailures == 0 {
			mainFailures++
			return errors.New("injected main removal failure")
		}
		removed[service] = true
		return nil
	}
	reloads := 0
	reload := func() error { reloads++; return nil }
	if err := removeSystemdUnitsForUninstall(cfg, remove, reload); err == nil {
		t.Fatal("first partial uninstall should report main failure")
	}
	if !removed[cfg.RestoreService] || removed[cfg.SystemdService] {
		t.Fatalf("unexpected first-pass state: %v", removed)
	}
	if err := removeSystemdUnitsForUninstall(cfg, remove, reload); err != nil {
		t.Fatalf("second uninstall did not converge: %v", err)
	}
	if !removed[cfg.SystemdService] || reloads != 2 {
		t.Fatalf("retry final state=%v reloads=%d", removed, reloads)
	}
}

func TestBootRestoreWithStoreRollsBackAfterPersistenceFailure(t *testing.T) {
	a := testApp(t)
	before := newStore()
	before.RuntimeConfig = a.cfg.runtimeConfig()
	before.SceneEnabled[SceneGlobal] = true
	st := cloneStore(before)

	syncCalls := 0
	persistCalls := 0
	commit := func(state *Store, mutate func(*Store) error, mode storeRuntimeSyncMode) error {
		if mode != storeRuntimeSyncAll {
			t.Fatalf("boot restore mode=%d, want full reconciliation", mode)
		}
		return a.commitStoreMutationWithRuntimeOps(state, mutate, mode,
			func(_ *App, candidate *Store, gotMode storeRuntimeSyncMode) error {
				syncCalls++
				if gotMode != storeRuntimeSyncAll || !candidate.SceneEnabled[SceneGlobal] {
					t.Fatalf("boot sync call %d got mode=%d state=%+v", syncCalls, gotMode, candidate)
				}
				return nil
			},
			func(*App, *Store) error { return nil },
			func(_ *App, candidate *Store) error {
				persistCalls++
				if persistCalls == 1 {
					return errors.New("injected boot state save failure")
				}
				if !reflect.DeepEqual(candidate, before) {
					t.Fatalf("boot rollback persisted wrong state: got=%+v want=%+v", candidate, before)
				}
				return nil
			})
	}
	err := a.bootRestoreWithStore(st, commit)
	if err == nil || !strings.Contains(err.Error(), "injected boot state save failure") {
		t.Fatalf("boot persistence failure was not returned: %v", err)
	}
	if syncCalls != 2 || persistCalls != 2 {
		t.Fatalf("boot transaction calls sync=%d persist=%d, want 2/2", syncCalls, persistCalls)
	}
	if !reflect.DeepEqual(st, before) {
		t.Fatalf("boot persistence failure left candidate state: got=%+v want=%+v", st, before)
	}
}
