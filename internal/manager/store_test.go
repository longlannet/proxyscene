package manager

import (
	"bytes"
	"debug/elf"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func testApp(t *testing.T) *App {
	t.Helper()
	cfg := DefaultConfig()
	cfg.CoreDir = t.TempDir()
	return claimTestApp(t, NewApp(cfg))
}

func claimTestApp(t *testing.T, a *App) *App {
	t.Helper()
	previousValidator := runtimeValidateConfig
	t.Cleanup(func() { runtimeValidateConfig = previousValidator })
	runtimeValidateConfig = func(cfg Config) error { return cfg.ValidateRuntime() }
	if err := a.ensureCoreDirs(); err != nil {
		t.Fatal(err)
	}
	if err := a.writeInstallationOwnership(a.expectedInstallationOwnership()); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestStoreSaveLoadRoundTrip(t *testing.T) {
	a := testApp(t)
	st := newStore()
	st.Nodes = append(st.Nodes, Node{ID: "node-1", Name: "n1", Protocol: "vless", RawURL: "vless://11111111-1111-1111-1111-111111111111@h:443?security=tls"})
	st.DefaultNodeID = "node-1"
	st.SceneEnabled[SceneGlobal] = true
	if err := a.saveStore(st); err != nil {
		t.Fatalf("saveStore: %v", err)
	}
	got, err := a.loadStore()
	if err != nil {
		t.Fatalf("loadStore: %v", err)
	}
	if len(got.Nodes) != 1 || got.Nodes[0].ID != "node-1" || !got.SceneEnabled[SceneGlobal] {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestRuntimeConfigJSONRoundTripExcludesLocatorsAndTestURL(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CoreDir = t.TempDir()
	cfg.InstallBin = "/usr/local/sbin/proxyscene-private"
	cfg.SystemdService = "proxyscene-private.service"
	cfg.RestoreService = "proxyscene-private-restore.service"
	cfg.ProxyHost = "127.0.0.9"
	cfg.DevHTTPPort = 18091
	cfg.TGHTTPPort = 18092
	cfg.TGSocksPort = 18093
	cfg.GlobalHTTPPort = 18090
	cfg.GlobalSocksPort = 18094
	cfg.XrayServiceUser = "proxycustom"
	cfg.TGTargetServices = []string{"hermes-gateway", "user:root:openclaw-gateway"}
	cfg.DevTargetUser = "root"
	cfg.ManageOpenClawConfig = false
	cfg.AllowPublicBind = false
	cfg.TestURL = "https://user:secret@example.invalid/probe?token=secret"
	a := claimTestApp(t, NewApp(cfg))
	st := newStore()
	st.RuntimeConfig = cfg.runtimeConfig()
	if err := a.saveStore(st); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(cfg.StorePath())
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{
		[]byte("core_dir"), []byte("install_bin"), []byte("systemd_service"),
		[]byte("restore_service"), []byte("test_url"), []byte("secret"),
	} {
		if bytes.Contains(data, forbidden) {
			t.Fatalf("state JSON leaked forbidden field/value %q:\n%s", forbidden, data)
		}
	}
	got, err := a.loadStoreForBoot()
	if err != nil {
		t.Fatal(err)
	}
	if !runtimeConfigsEqual(got.RuntimeConfig, cfg.runtimeConfig()) {
		t.Fatalf("runtime round-trip mismatch: got=%+v want=%+v", got.RuntimeConfig, cfg.runtimeConfig())
	}
}

func TestDecodeStoreRejectsInvalidRuntimeConfig(t *testing.T) {
	cases := map[string]func(*RuntimeConfig){
		"version":       func(cfg *RuntimeConfig) { cfg.Version++ },
		"port conflict": func(cfg *RuntimeConfig) { cfg.TGHTTPPort = cfg.DevHTTPPort },
		"public bind": func(cfg *RuntimeConfig) {
			cfg.ProxyHost = "0.0.0.0"
			cfg.AllowPublicBind = false
		},
		"target": func(cfg *RuntimeConfig) { cfg.TGTargetServices = []string{"bad//target"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			st := newStore()
			st.RuntimeConfig = DefaultConfig().runtimeConfig()
			mutate(st.RuntimeConfig)
			data, err := json.Marshal(st)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := decodeStore(data); err == nil {
				t.Fatal("invalid runtime config was accepted")
			}
		})
	}
}

func TestLoadStoreRuntimeResolutionAndBootPrecedence(t *testing.T) {
	dir := t.TempDir()
	storedCfg := DefaultConfig()
	storedCfg.CoreDir = dir
	storedCfg.ProxyHost = "127.0.0.8"
	storedCfg.DevHTTPPort = 18091
	storedCfg.DevTargetUser = "root"
	storedCfg.TGTargetServices = []string{"hermes-gateway"}
	stored := newStore()
	stored.RuntimeConfig = storedCfg.runtimeConfig()
	owner := claimTestApp(t, NewApp(storedCfg))
	if err := owner.saveStore(stored); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PROXYSCENE_DEV_HTTP_PORT", "28091")
	t.Setenv("PROXYSCENE_DEV_TARGET_USER", "")
	t.Setenv("PROXYSCENE_TG_SERVICES", "")
	processCfg := DefaultConfig()
	processCfg.CoreDir = dir
	normal := NewApp(processCfg)
	loaded, err := normal.loadStore()
	if err != nil {
		t.Fatal(err)
	}
	if normal.cfg.ProxyHost != storedCfg.ProxyHost || normal.cfg.DevHTTPPort != 28091 {
		t.Fatalf("ordinary resolution mismatch: %+v", normal.cfg)
	}
	if normal.cfg.DevTargetUser != "" || len(normal.cfg.TGTargetServices) != 0 {
		t.Fatalf("explicit empty overrides were lost: %+v", normal.cfg)
	}
	if !runtimeConfigsEqual(loaded.RuntimeConfig, stored.RuntimeConfig) {
		t.Fatal("load must not mutate the persisted baseline before a successful transaction")
	}

	boot := NewApp(processCfg)
	if _, err := boot.loadStoreForBoot(); err != nil {
		t.Fatal(err)
	}
	if boot.cfg.DevHTTPPort != storedCfg.DevHTTPPort || boot.cfg.DevTargetUser != storedCfg.DevTargetUser || len(boot.cfg.TGTargetServices) != 1 {
		t.Fatalf("boot did not prefer stored runtime config: %+v", boot.cfg)
	}
}

func TestInvalidRuntimeEnvironmentDoesNotOverrideStoredConfig(t *testing.T) {
	dir := t.TempDir()
	storedCfg := DefaultConfig()
	storedCfg.CoreDir = dir
	storedCfg.ProxyHost = "127.0.0.8"
	storedCfg.DevHTTPPort = 18091
	st := newStore()
	st.RuntimeConfig = storedCfg.runtimeConfig()
	owner := claimTestApp(t, NewApp(storedCfg))
	if err := owner.saveStore(st); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROXYSCENE_DEV_HTTP_PORT", "not-a-port")
	t.Setenv("PROXYSCENE_HOST", "not an ip")
	processCfg := DefaultConfig()
	processCfg.CoreDir = dir
	a := NewApp(processCfg)
	if _, err := a.loadStore(); err != nil {
		t.Fatal(err)
	}
	if a.cfg.DevHTTPPort != storedCfg.DevHTTPPort || a.cfg.ProxyHost != storedCfg.ProxyHost {
		t.Fatalf("invalid env replaced stored config: %+v", a.cfg)
	}
}

func TestBootAcceptsPersistedPublicBindPermission(t *testing.T) {
	dir := t.TempDir()
	storedCfg := DefaultConfig()
	storedCfg.CoreDir = dir
	storedCfg.ProxyHost = "0.0.0.0"
	storedCfg.AllowPublicBind = true
	st := newStore()
	st.RuntimeConfig = storedCfg.runtimeConfig()
	owner := claimTestApp(t, NewApp(storedCfg))
	if err := owner.saveStore(st); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PROXYSCENE_ALLOW_PUBLIC_BIND", "0")
	processCfg := DefaultConfig()
	processCfg.CoreDir = dir
	a := NewApp(processCfg)
	if _, err := a.loadStoreForBoot(); err != nil {
		t.Fatalf("boot rejected persisted public-bind opt-in: %v", err)
	}
	if !a.cfg.AllowPublicBind || a.cfg.ProxyHost != "0.0.0.0" {
		t.Fatalf("persisted public-bind policy was not restored: %+v", a.cfg)
	}
}

func TestEnabledDevAutoUserIsResolvedAndPersistedForBoot(t *testing.T) {
	stubStoreTransitionCore(t)
	stubDevCommands(t)
	devCommandExists = func(name string) bool { return name == "npm" }
	values := map[string]*string{}
	devReadNPMConfig = func(_ string, _ *persistedUserIdentity, key string) (*string, error) {
		return cloneStringPointer(values[key]), nil
	}
	devMutateNPMConfig = func(_ string, _ *persistedUserIdentity, key string, expected, desired *string) error {
		if !optionalStringsEqual(values[key], expected) {
			return errors.New("fixture npm compare-and-swap mismatch")
		}
		values[key] = cloneStringPointer(desired)
		return nil
	}
	devOutputAsUser = func(string, *persistedUserIdentity, string, ...string) (string, error) {
		t.Fatal("fixture must not query external tools")
		return "", nil
	}
	devRunAsUser = func(string, *persistedUserIdentity, string, ...string) error {
		t.Fatal("fixture must not mutate external tools")
		return nil
	}
	taskHome := t.TempDir()
	devLookupUserIdentity = func(name string) (localUserIdentity, error) {
		if name != "nobody" {
			return localUserIdentity{}, errors.New("unexpected user")
		}
		return localUserIdentity{Name: name, UID: os.Getuid(), GID: os.Getgid(), Home: taskHome}, nil
	}
	a := testApp(t)
	a.cfg.DevTargetUser = ""
	a.cfg.runtimeOverrides.DevTargetUser = false
	st := newStore()
	st.RuntimeConfig = a.cfg.runtimeConfig()
	st.Nodes = []Node{{ID: "node-one", Name: "one", Protocol: "trojan", RawURL: "trojan://secret@example.com:443"}}
	st.DefaultNodeID = "node-one"
	if err := a.saveStore(st); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUDO_USER", "nobody")
	t.Setenv("USER", "root")
	if err := a.commitStoreMutation(st, func(candidate *Store) error { candidate.SceneEnabled[SceneDev] = true; return nil }, storeRuntimeSyncAll); err != nil {
		t.Fatal(err)
	}
	if st.RuntimeConfig == nil || st.RuntimeConfig.DevTargetUser != "nobody" {
		t.Fatalf("automatic dev user not persisted: %+v", st.RuntimeConfig)
	}
	t.Setenv("SUDO_USER", "")
	processCfg := a.cfg
	processCfg.DevTargetUser = ""
	boot := NewApp(processCfg)
	if _, err := boot.loadStoreForBoot(); err != nil {
		t.Fatal(err)
	}
	if boot.cfg.DevTargetUser != "nobody" {
		t.Fatalf("boot re-selected another dev user: %+v", boot.cfg)
	}
}

func TestStoreCorruptMainRecoversFromBackup(t *testing.T) {
	a := testApp(t)
	st := newStore()
	st.DefaultNodeID = "node-keep"
	st.Nodes = append(st.Nodes, Node{ID: "node-keep", Name: "keep", Protocol: "ss", RawURL: "ss://aes-256-gcm:secret@h:8388"})
	if err := a.saveStore(st); err != nil {
		t.Fatalf("saveStore: %v", err)
	}
	if err := os.WriteFile(a.cfg.StorePath(), []byte("{ not valid json"), 0o600); err != nil {
		t.Fatalf("corrupt main: %v", err)
	}
	got, err := a.loadStore()
	if err != nil {
		t.Fatalf("loadStore should recover from backup, got error: %v", err)
	}
	if got.DefaultNodeID != "node-keep" || len(got.Nodes) != 1 {
		t.Fatalf("expected backup recovery, got %+v", got)
	}
}

func TestStoreCorruptMainAndBackupFails(t *testing.T) {
	a := testApp(t)
	if err := a.saveStore(newStore()); err != nil {
		t.Fatalf("saveStore: %v", err)
	}
	_ = os.WriteFile(a.cfg.StorePath(), []byte("{bad"), 0o600)
	_ = os.WriteFile(a.cfg.StoreBackupPath(), []byte("{bad"), 0o600)
	if _, err := a.loadStore(); err == nil {
		t.Fatalf("expected error when both main and backup are corrupt")
	}
}

func TestStoreEmptyMainRecoversFromBackup(t *testing.T) {
	a := testApp(t)
	st := newStore()
	st.DefaultNodeID = "node-keep"
	st.Nodes = append(st.Nodes, Node{ID: "node-keep", Name: "keep", Protocol: "ss", RawURL: "ss://aes-256-gcm:secret@h:8388"})
	if err := a.saveStore(st); err != nil {
		t.Fatalf("saveStore: %v", err)
	}
	// 模拟崩溃/掉电后主状态文件被截断为 0 字节。
	if err := os.WriteFile(a.cfg.StorePath(), []byte{}, 0o600); err != nil {
		t.Fatalf("truncate main: %v", err)
	}
	got, err := a.loadStore()
	if err != nil {
		t.Fatalf("loadStore 应从备份恢复，却报错：%v", err)
	}
	if got.DefaultNodeID != "node-keep" || len(got.Nodes) != 1 {
		t.Fatalf("期望从备份恢复，得到 %+v", got)
	}
}

func TestStoreEmptyMainNoBackupReturnsEmpty(t *testing.T) {
	a := testApp(t)
	if err := os.WriteFile(a.cfg.StorePath(), []byte{}, 0o600); err != nil {
		t.Fatalf("write empty main: %v", err)
	}
	if _, err := a.loadStore(); err == nil {
		t.Fatalf("空主文件且无备份必须 fail-closed，不能静默当作首次运行")
	}
}

func TestLoadStoreMissingReturnsEmpty(t *testing.T) {
	a := testApp(t)
	got, err := a.loadStore()
	if err != nil {
		t.Fatalf("loadStore on empty dir: %v", err)
	}
	if got == nil || len(got.Nodes) != 0 || got.SceneEnabled == nil {
		t.Fatalf("expected empty initialized store, got %+v", got)
	}
}

func TestStoreMissingMainRecoversFromBackup(t *testing.T) {
	a := testApp(t)
	st := newStore()
	st.Nodes = []Node{{ID: "node-backup", Name: "backup", Protocol: "trojan", RawURL: "trojan://secret@h:443"}}
	st.DefaultNodeID = "node-backup"
	if err := a.saveStore(st); err != nil {
		t.Fatalf("saveStore: %v", err)
	}
	if err := os.Remove(a.cfg.StorePath()); err != nil {
		t.Fatalf("remove main: %v", err)
	}
	got, err := a.loadStore()
	if err != nil {
		t.Fatalf("missing main should recover backup: %v", err)
	}
	if got.DefaultNodeID != "node-backup" || len(got.Nodes) != 1 {
		t.Fatalf("backup recovery mismatch: %+v", got)
	}
}

func TestLoadStoreRejectsNonRegularFiles(t *testing.T) {
	t.Run("main symlink", func(t *testing.T) {
		a := testApp(t)
		target := filepath.Join(t.TempDir(), "state-target.json")
		if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, a.cfg.StorePath()); err != nil {
			t.Fatal(err)
		}
		if _, err := a.loadStore(); err == nil {
			t.Fatalf("state.json symlink must be rejected")
		}
	})

	t.Run("main fifo", func(t *testing.T) {
		a := testApp(t)
		if err := syscall.Mkfifo(a.cfg.StorePath(), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := a.loadStore(); err == nil {
			t.Fatalf("state.json FIFO must be rejected without blocking")
		}
	})

	t.Run("backup symlink", func(t *testing.T) {
		a := testApp(t)
		if err := os.WriteFile(a.cfg.StorePath(), []byte("{bad"), 0o600); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "backup-target.json")
		if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, a.cfg.StoreBackupPath()); err != nil {
			t.Fatal(err)
		}
		if _, err := a.loadStore(); err == nil {
			t.Fatalf("state.json.bak symlink must be rejected")
		}
	})
}

func stubStoreTransitionCore(t *testing.T) {
	t.Helper()
	oldPlan, oldPersist := runtimePlanCore, runtimePersistStore
	oldSystem, oldUser := systemctlRun, userSystemctlRun
	t.Cleanup(func() {
		runtimePlanCore, runtimePersistStore = oldPlan, oldPersist
		systemctlRun, userSystemctlRun = oldSystem, oldUser
	})
	runtimePlanCore = func(*App, *Store) (*runtimeCorePlan, error) { return nil, nil }
	runtimePersistStore = func(app *App, st *Store) error { return app.saveStore(st) }
	systemctlRun = func(string, ...string) error { t.Fatal("isolated Store transition called systemctl"); return nil }
	userSystemctlRun = func(string, *persistedUserIdentity, string, ...string) error {
		t.Fatal("isolated Store transition called user systemctl")
		return nil
	}
}

func storeTransitionFixture(t *testing.T) (*App, *Store) {
	t.Helper()
	stubStoreTransitionCore(t)
	a := testApp(t)
	st := newStore()
	st.RuntimeConfig = a.cfg.runtimeConfig()
	st.Nodes = []Node{{ID: "old", Name: "old", Protocol: "trojan", RawURL: "trojan://secret@example.com:443"}}
	st.DefaultNodeID = "old"
	if err := a.saveStore(st); err != nil {
		t.Fatal(err)
	}
	return a, st
}

func TestCommitNodeStoreMutationPreflightFailureHasNoEffects(t *testing.T) {
	a, before := storeTransitionFixture(t)
	st := cloneStore(before)
	mainBefore, _ := os.ReadFile(a.cfg.StorePath())
	backupBefore, _ := os.ReadFile(a.cfg.StoreBackupPath())
	failure := errors.New("candidate core validation failed")
	runtimePlanCore = func(*App, *Store) (*runtimeCorePlan, error) { return nil, failure }
	err := a.commitStoreMutation(st, func(candidate *Store) error { candidate.Nodes[0].Name = "candidate"; return nil }, storeRuntimeSyncXray)
	if !errors.Is(err, failure) || !reflect.DeepEqual(st, before) {
		t.Fatalf("preflight changed in-memory state: %v", err)
	}
	mainAfter, _ := os.ReadFile(a.cfg.StorePath())
	backupAfter, _ := os.ReadFile(a.cfg.StoreBackupPath())
	if !bytes.Equal(mainBefore, mainAfter) || !bytes.Equal(backupBefore, backupAfter) {
		t.Fatal("preflight changed Store bytes")
	}
	if pending, err := a.hasRuntimeTransition(); err != nil || pending {
		t.Fatalf("preflight created a runtime receipt: %v", err)
	}
}

func TestMetadataCommitDoesNotMigrateLegacyRuntime(t *testing.T) {
	a, before := storeTransitionFixture(t)
	before.RuntimeConfig = nil
	before.SceneEnabled[SceneGlobal] = true
	if err := a.saveStore(before); err != nil {
		t.Fatal(err)
	}
	a.cfg.DevHTTPPort = 28091
	a.cfg.runtimeOverrides.DevHTTPPort = true
	runtimePlanCore = func(*App, *Store) (*runtimeCorePlan, error) {
		t.Fatal("metadata edit planned runtime")
		return nil, nil
	}
	if err := a.commitStoreMutation(before, func(candidate *Store) error { candidate.Nodes[0].Name = "metadata"; return nil }, storeRuntimeSyncNone); err != nil {
		t.Fatal(err)
	}
	loaded, err := a.loadStore()
	if err != nil || loaded.RuntimeConfig != nil || loaded.Nodes[0].Name != "metadata" || !loaded.SceneEnabled[SceneGlobal] {
		t.Fatalf("metadata edit invented historical runtime: %+v %v", loaded, err)
	}
	if pending, err := a.hasRuntimeTransition(); err != nil || pending {
		t.Fatalf("metadata edit created receipt: %v", err)
	}
}

func TestLegacyActiveRuntimeMutationFailsBeforeAnyCoordination(t *testing.T) {
	a, before := storeTransitionFixture(t)
	before.RuntimeConfig = nil
	before.SceneEnabled[SceneGlobal] = true
	if err := a.saveStore(before); err != nil {
		t.Fatal(err)
	}
	st := cloneStore(before)
	raw, _ := os.ReadFile(a.cfg.StorePath())
	runtimePlanCore = func(*App, *Store) (*runtimeCorePlan, error) {
		t.Fatal("unknown history reached core planning")
		return nil, nil
	}
	if err := a.commitStoreMutation(st, func(candidate *Store) error { candidate.Nodes[0].Name = "candidate"; return nil }, storeRuntimeSyncXray); err == nil {
		t.Fatal("unknown active history accepted")
	}
	after, _ := os.ReadFile(a.cfg.StorePath())
	if !bytes.Equal(raw, after) || !reflect.DeepEqual(st, before) {
		t.Fatal("rejected legacy migration changed state")
	}
}

func TestStoreTransitionBackupFailureKeepsPriorBytes(t *testing.T) {
	a, before := storeTransitionFixture(t)
	st := cloneStore(before)
	oldWrite := runtimeWriteStoreCAS
	t.Cleanup(func() { runtimeWriteStoreCAS = oldWrite })
	failure := errors.New("backup persistence failure")
	runtimeWriteStoreCAS = func(path string, expected runtimeStoreEvidence, data []byte) error {
		if path == a.cfg.StoreBackupPath() {
			return failure
		}
		return oldWrite(path, expected, data)
	}
	err := a.commitStoreMutation(st, func(candidate *Store) error { candidate.Nodes[0].Name = "candidate"; return nil }, storeRuntimeSyncXray)
	if !errors.Is(err, failure) {
		t.Fatalf("backup failure lost: %v", err)
	}
	loaded, loadErr := a.loadStore()
	if loadErr != nil || !reflect.DeepEqual(loaded, before) || !reflect.DeepEqual(st, before) {
		t.Fatalf("backup failure changed prior state: %+v %v", loaded, loadErr)
	}
	if pending, err := a.hasRuntimeTransition(); err != nil || pending {
		t.Fatalf("unmodified Store retained receipt: %v", err)
	}
}

func TestStoreTransitionMainFailureCompensatesWithNewGeneration(t *testing.T) {
	a, before := storeTransitionFixture(t)
	st := cloneStore(before)
	oldWrite := runtimeWriteStoreCAS
	t.Cleanup(func() { runtimeWriteStoreCAS = oldWrite })
	failure := errors.New("main persistence failure")
	failed := false
	runtimeWriteStoreCAS = func(path string, expected runtimeStoreEvidence, data []byte) error {
		if path == a.cfg.StorePath() && !failed {
			failed = true
			return failure
		}
		return oldWrite(path, expected, data)
	}
	err := a.commitStoreMutation(st, func(candidate *Store) error { candidate.Nodes[0].Name = "candidate"; return nil }, storeRuntimeSyncXray)
	if !errors.Is(err, failure) {
		t.Fatalf("main failure lost: %v", err)
	}
	loaded, loadErr := a.loadStore()
	if loadErr != nil || loaded.Nodes[0].Name != "old" || loaded.Generation <= before.Generation+1 {
		t.Fatalf("compensation did not supersede ahead backup: %+v %v", loaded, loadErr)
	}
	backup, backupErr := a.loadStoreBackup()
	if backupErr != nil || !reflect.DeepEqual(backup, loaded) {
		t.Fatalf("compensation copies mismatch: %+v %v", backup, backupErr)
	}
	if pending, err := a.hasRuntimeTransition(); err != nil || pending {
		t.Fatalf("completed compensation retained receipt: %v", err)
	}
}

func TestStoreTransitionPostCommitErrorRecoversFixedCommit(t *testing.T) {
	a, before := storeTransitionFixture(t)
	st := cloneStore(before)
	failure := errors.New("fsync failed after main commit")
	runtimePersistStore = func(app *App, candidate *Store) error {
		if err := app.saveStore(candidate); err != nil {
			return err
		}
		return failure
	}
	err := a.commitStoreMutation(st, func(candidate *Store) error { candidate.Nodes[0].Name = "committed"; return nil }, storeRuntimeSyncXray)
	if !errors.Is(err, failure) {
		t.Fatalf("post-commit failure lost: %v", err)
	}
	loaded, loadErr := a.loadStore()
	if loadErr != nil || loaded.Nodes[0].Name != "committed" || loaded.Generation != before.Generation+1 {
		t.Fatalf("committed main was rolled back: %+v %v", loaded, loadErr)
	}
	if pending, err := a.hasRuntimeTransition(); err != nil || !pending {
		t.Fatalf("uncertain commit did not retain receipt: %v", err)
	}
	runtimePlanCore = func(*App, *Store) (*runtimeCorePlan, error) { t.Fatal("recovery replanned core"); return nil, nil }
	runtimePersistStore = func(*App, *Store) error { t.Fatal("confirmed commit was persisted again"); return nil }
	oldInspect := telegramInspectPlanUnit
	t.Cleanup(func() { telegramInspectPlanUnit = oldInspect })
	telegramInspectPlanUnit = func(systemdTargetName, *persistedUserIdentity) (telegramPlanUnit, error) {
		t.Fatal("fixed recovery rediscovered a gateway")
		return telegramPlanUnit{}, nil
	}
	restarted := NewApp(a.cfg)
	if recovered, err := restarted.recoverRuntimeTransition(); err != nil || !recovered {
		t.Fatalf("fixed committed recovery failed: %v", err)
	}
	if pending, err := restarted.hasRuntimeTransition(); err != nil || pending {
		t.Fatalf("recovery did not retire receipt: %v", err)
	}
	current, err := restarted.loadStore()
	if err != nil || !reflect.DeepEqual(current, loaded) {
		t.Fatalf("recovery changed committed Store: %+v %v", current, err)
	}
}

func TestNodeCommitWithoutScenesRemovesStaleXrayConfig(t *testing.T) {
	fixture := newCoreRuntimeFixture(t)
	a := fixture.app
	st := fixture.store
	st.SceneEnabled[SceneGlobal] = false
	fixture.service.Active, fixture.service.Sub = "inactive", "dead"
	fixture.service.PID, fixture.service.Invocation = 0, ""
	if err := a.saveStore(st); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.cfg.XrayConfig(), []byte(`{"password":"deleted-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.commitNodeStoreMutation(st, func(candidate *Store) error {
		candidate.Nodes = append(candidate.Nodes, Node{ID: "new", Name: "new", Protocol: "trojan", RawURL: "trojan://secret@h:443"})
		candidate.DefaultNodeID = "new"
		return nil
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := os.Stat(a.cfg.XrayConfig()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale config still exists or stat failed: %v", err)
	}
	if fixture.service.Active != "inactive" || fixture.service.Enabled != "disabled" {
		t.Fatalf("disabled scene left core enabled: %+v", fixture.service)
	}
}

func TestNodeCommitWithoutScenesStopFailurePreservesConfigAndRollsBack(t *testing.T) {
	fixture := newCoreRuntimeFixture(t)
	a := fixture.app
	before := fixture.store
	before.SceneEnabled[SceneGlobal] = false
	if err := a.saveStore(before); err != nil {
		t.Fatal(err)
	}
	st := cloneStore(before)
	credentialConfig := []byte(`{"password":"credential-must-survive"}`)
	if err := os.WriteFile(a.cfg.XrayConfig(), credentialConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	fixtureRun := systemctlRun
	failure := errors.New("stop failed")
	stopCalls := 0
	systemctlRun = func(action string, args ...string) error {
		if len(args) != 0 && args[0] == "stop" {
			stopCalls++
			return failure
		}
		return fixtureRun(action, args...)
	}
	err := a.commitNodeStoreMutation(st, func(candidate *Store) error {
		candidate.Nodes[0].Name = "new"
		return nil
	})
	if !errors.Is(err, failure) || stopCalls == 0 {
		t.Fatalf("core stop failure was not exercised: calls=%d err=%v", stopCalls, err)
	}
	if !reflect.DeepEqual(st, before) {
		t.Fatalf("memory not rolled back: got=%+v want=%+v", st, before)
	}
	persisted, loadErr := a.loadStore()
	if loadErr != nil || !reflect.DeepEqual(persisted, before) {
		t.Fatalf("disk changed after stop failure: got=%+v err=%v", persisted, loadErr)
	}
	gotConfig, readErr := os.ReadFile(a.cfg.XrayConfig())
	if readErr != nil || !bytes.Equal(gotConfig, credentialConfig) {
		t.Fatalf("credential config changed while Xray may still be active: %q err=%v", gotConfig, readErr)
	}
}

func TestSaveStoreReportsBackupFailure(t *testing.T) {
	a := testApp(t)
	if err := os.Mkdir(a.cfg.StoreBackupPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	saveErr := a.saveStore(newStore())
	if saveErr == nil {
		t.Fatalf("backup-first failure must fail the transaction")
	}
	if _, err := os.Stat(a.cfg.StorePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("main commit point must remain absent after backup failure: %v", err)
	}
}

func TestSaveStoreBackupFirstFailureInjection(t *testing.T) {
	a := testApp(t)
	oldWrite := storeWriteFile
	t.Cleanup(func() { storeWriteFile = oldWrite })
	calls := []string{}
	storeWriteFile = func(path string, data []byte, mode os.FileMode) error {
		calls = append(calls, path)
		if path == a.cfg.StoreBackupPath() {
			return errors.New("injected backup failure")
		}
		return oldWrite(path, data, mode)
	}
	st := newStore()
	st.RuntimeConfig = a.cfg.runtimeConfig()
	if err := a.saveStore(st); err == nil {
		t.Fatal("backup failure must fail save")
	}
	if len(calls) != 1 || calls[0] != a.cfg.StoreBackupPath() {
		t.Fatalf("main was attempted before a durable backup: %v", calls)
	}
	if st.Generation != 0 {
		t.Fatalf("failed save mutated in-memory generation: %d", st.Generation)
	}
}

func TestSaveStoreMainFailureKeepsValidMainAndAheadBackupRecoverable(t *testing.T) {
	a := testApp(t)
	stable := newStore()
	stable.RuntimeConfig = a.cfg.runtimeConfig()
	stable.Nodes = []Node{{ID: "node-one", Name: "stable", Protocol: "trojan", RawURL: "trojan://secret@example.com:443"}}
	stable.DefaultNodeID = "node-one"
	if err := a.saveStore(stable); err != nil {
		t.Fatal(err)
	}
	candidate := cloneStore(stable)
	candidate.Nodes[0].Name = "ahead"

	oldWrite := storeWriteFile
	t.Cleanup(func() { storeWriteFile = oldWrite })
	storeWriteFile = func(path string, data []byte, mode os.FileMode) error {
		if path == a.cfg.StorePath() {
			return errors.New("injected main failure")
		}
		return oldWrite(path, data, mode)
	}
	if err := a.saveStore(candidate); err == nil {
		t.Fatal("main failure must fail save")
	}
	if candidate.Generation != stable.Generation {
		t.Fatalf("failed main commit mutated candidate generation: %d", candidate.Generation)
	}
	storeWriteFile = oldWrite

	loaded, err := a.loadStoreForBoot()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Nodes[0].Name != "stable" || loaded.Generation != stable.Generation {
		t.Fatalf("valid main must win over ahead backup: %+v", loaded)
	}
	if err := os.Remove(a.cfg.StorePath()); err != nil {
		t.Fatal(err)
	}
	recovered, err := a.loadStoreForBoot()
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Nodes[0].Name != "ahead" || recovered.Generation != stable.Generation+1 {
		t.Fatalf("latest backup was not recovered: %+v", recovered)
	}
}

func TestValidateStoreDataSize(t *testing.T) {
	if err := validateStoreDataSize(make([]byte, 10), 10); err != nil {
		t.Fatalf("exact limit rejected: %v", err)
	}
	if err := validateStoreDataSize(make([]byte, 11), 10); err == nil {
		t.Fatalf("oversized state data accepted")
	}
}

func TestEnsureXrayInstalledRejectsUnsafeFiles(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		a := testApp(t)
		if err := os.Symlink("/bin/true", a.cfg.XrayBin()); err != nil {
			t.Fatal(err)
		}
		if err := a.ensureXrayInstalled(); err == nil {
			t.Fatalf("symlinked Xray must be rejected")
		}
	})

	t.Run("fifo", func(t *testing.T) {
		a := testApp(t)
		if err := syscall.Mkfifo(a.cfg.XrayBin(), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := a.ensureXrayInstalled(); err == nil {
			t.Fatalf("FIFO Xray must be rejected")
		}
	})

	t.Run("group writable", func(t *testing.T) {
		a := testApp(t)
		binary, err := os.ReadFile("/bin/true")
		if err != nil {
			t.Skipf("cannot read /bin/true: %v", err)
		}
		if err := os.WriteFile(a.cfg.XrayBin(), binary, 0o775); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(a.cfg.XrayBin(), 0o775); err != nil {
			t.Fatal(err)
		}
		if err := a.ensureXrayInstalled(); err == nil {
			t.Fatalf("group-writable Xray must be rejected")
		}
	})
}

func TestEnsureXrayInstalledAcceptsPinnedTrustProperties(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root ownership assertion requires root test runner")
	}
	a := testApp(t)
	binary, err := os.ReadFile("/bin/true")
	if err != nil {
		t.Skipf("cannot read /bin/true: %v", err)
	}
	if err := os.WriteFile(a.cfg.XrayBin(), binary, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := a.ensureXrayInstalled(); err != nil {
		t.Fatalf("trusted ELF rejected: %v", err)
	}
}

func TestELFMachineMatchesRuntime(t *testing.T) {
	var current elf.Machine
	switch runtime.GOARCH {
	case "amd64":
		current = elf.EM_X86_64
	case "arm64":
		current = elf.EM_AARCH64
	case "386":
		current = elf.EM_386
	case "arm":
		current = elf.EM_ARM
	default:
		t.Skipf("unsupported test architecture %s", runtime.GOARCH)
	}
	if !elfMachineMatchesRuntime(current) {
		t.Fatalf("current ELF machine rejected: %v", current)
	}
	if elfMachineMatchesRuntime(elf.EM_NONE) {
		t.Fatalf("EM_NONE accepted")
	}
}

func TestWriteCheckedXrayConfigRemovesRejectedTemporaryFile(t *testing.T) {
	a := testApp(t)
	if err := os.WriteFile(a.cfg.XrayBin(), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	st := newStore()
	if _, err := a.addNode(st, "trojan://secret@example.com:443", "", "default"); err != nil {
		t.Fatal(err)
	}
	st.SceneEnabled[SceneGlobal] = true
	if err := a.writeCheckedXrayConfig(st); err == nil {
		t.Fatalf("expected Xray validation failure")
	}
	if _, err := os.Stat(a.cfg.XrayConfig() + ".new"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected temporary config remains: %v", err)
	}
}

func TestRemoveXrayTempConfigReportsCleanupFailure(t *testing.T) {
	a := testApp(t)
	path := a.cfg.XrayConfig() + ".new"
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeXrayTempConfig(path, a.cfg.CoreDir); err == nil {
		t.Fatalf("cleanup failure was swallowed")
	}
}

func TestLoadStoreSanitizesDisplayOnlyFields(t *testing.T) {
	a := testApp(t)
	rawURL := "trojan://secret@example.com:443"
	st := newStore()
	st.Nodes = []Node{{ID: "node-safe", Name: "safe\x1b\u202ename", Protocol: "trojan", RawURL: rawURL}}
	st.DefaultNodeID = "node-safe"
	st.SpeedResults["node-safe"] = SpeedResult{
		NodeID: "node-safe", Target: "target\x07", Error: "failed\x1b[2J", TestedAt: time.Now(),
	}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.cfg.StorePath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := a.loadStore()
	if err != nil {
		t.Fatal(err)
	}
	if got.Nodes[0].Name != "safename" || got.SpeedResults["node-safe"].Error != "failed[2J" {
		t.Fatalf("display fields not sanitized: %+v %+v", got.Nodes[0], got.SpeedResults["node-safe"])
	}
	if got.Nodes[0].RawURL != rawURL {
		t.Fatalf("secret/raw URL was modified: %q", got.Nodes[0].RawURL)
	}
}

func TestLoadStoreRejectsSemanticCorruption(t *testing.T) {
	validNode := Node{ID: "node-one", Name: "one", Protocol: "trojan", RawURL: "trojan://secret@example.com:443"}
	cases := map[string]func(*Store){
		"duplicate ID": func(st *Store) {
			st.Nodes = append(st.Nodes, Node{ID: "node-one", Name: "two", Protocol: "trojan", RawURL: "trojan://secret@other.example:443"})
		},
		"unknown scene": func(st *Store) { st.SceneEnabled[Scene("unknown")] = true },
		"invalid raw URL": func(st *Store) {
			st.Nodes[0].RawURL = "trojan://secret@bad host:443"
		},
		"protocol mismatch": func(st *Store) { st.Nodes[0].Protocol = "vless" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			a := testApp(t)
			st := newStore()
			st.Nodes = []Node{validNode}
			st.DefaultNodeID = validNode.ID
			mutate(st)
			data, err := json.Marshal(st)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(a.cfg.StorePath(), data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := a.loadStore(); err == nil {
				t.Fatalf("semantic corruption accepted")
			}
		})
	}
}

func TestSemanticInvalidMainRecoversValidBackup(t *testing.T) {
	a := testApp(t)
	valid := newStore()
	valid.Nodes = []Node{{ID: "node-safe", Name: "safe", Protocol: "trojan", RawURL: "trojan://secret@example.com:443"}}
	valid.DefaultNodeID = "node-safe"
	if err := a.saveStore(valid); err != nil {
		t.Fatal(err)
	}
	invalid := cloneStore(valid)
	invalid.SceneEnabled[Scene("unknown")] = true
	data, err := json.Marshal(invalid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.cfg.StorePath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := a.loadStore()
	if err != nil {
		t.Fatalf("semantic corruption should recover backup: %v", err)
	}
	if got.DefaultNodeID != "node-safe" || len(got.SceneEnabled) != 0 {
		t.Fatalf("did not recover valid backup: %+v", got)
	}
}

func TestStoreNullMainRecoversAndPreservesBackup(t *testing.T) {
	a := testApp(t)
	st := newStore()
	st.DefaultNodeID = "node-keep"
	st.Nodes = []Node{{ID: "node-keep", Name: "keep", Protocol: "ss", RawURL: "ss://aes-256-gcm:secret@h:8388"}}
	if err := a.saveStore(st); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.cfg.StorePath(), []byte(" \nnull\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := a.loadStore()
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultNodeID != st.DefaultNodeID || len(got.Nodes) != 1 {
		t.Fatalf("backup was ignored: %+v", got)
	}
	if err := a.saveStore(got); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(a.cfg.StoreBackupPath())
	if err != nil {
		t.Fatal(err)
	}
	recovered, _, err := decodeStore(backup)
	if err != nil || len(recovered.Nodes) != 1 {
		t.Fatalf("healthy backup was lost: %+v, %v", recovered, err)
	}
}

func TestStoreRejectsNonObjectMainAndBackup(t *testing.T) {
	for _, raw := range []string{"null", "[]", "42", "true", `"text"`} {
		t.Run(raw, func(t *testing.T) {
			a := testApp(t)
			if err := os.WriteFile(a.cfg.StorePath(), []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(a.cfg.StoreBackupPath(), []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := a.loadStore(); err == nil {
				t.Fatal("non-object main and backup accepted")
			}
		})
	}
	if st, _, err := decodeStore([]byte("{}")); err != nil || st == nil || st.SceneEnabled == nil {
		t.Fatalf("empty object should remain valid and initialized: %+v, %v", st, err)
	}
}
