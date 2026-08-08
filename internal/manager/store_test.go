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
	if _, err := lookupLocalUserIdentity("nobody"); err != nil {
		t.Skipf("nobody user unavailable: %v", err)
	}
	cfg := DefaultConfig()
	cfg.CoreDir = t.TempDir()
	cfg.DevTargetUser = ""
	cfg.runtimeOverrides.DevTargetUser = false
	a := claimTestApp(t, NewApp(cfg))
	st := newStore()
	st.RuntimeConfig = cfg.runtimeConfig()
	st.Nodes = []Node{{ID: "node-one", Name: "one", Protocol: "trojan", RawURL: "trojan://secret@example.com:443"}}
	st.DefaultNodeID = "node-one"
	if err := a.saveStore(st); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUDO_USER", "nobody")
	t.Setenv("USER", "root")
	if err := a.commitStoreMutationWithRuntimeOps(st, func(candidate *Store) error {
		candidate.SceneEnabled[SceneDev] = true
		return nil
	}, storeRuntimeSyncAll,
		func(*App, *Store, storeRuntimeSyncMode) error { return nil },
		func(*App, *Store) error { return nil },
		func(app *App, state *Store) error { return app.saveStore(state) }); err != nil {
		t.Fatal(err)
	}
	if st.RuntimeConfig == nil || st.RuntimeConfig.DevTargetUser != "nobody" {
		t.Fatalf("automatic dev user was not persisted: %+v", st.RuntimeConfig)
	}

	t.Setenv("SUDO_USER", "")
	processCfg := DefaultConfig()
	processCfg.CoreDir = cfg.CoreDir
	processCfg.DevTargetUser = ""
	boot := NewApp(processCfg)
	if _, err := boot.loadStoreForBoot(); err != nil {
		t.Fatal(err)
	}
	if boot.cfg.DevTargetUser != "nobody" {
		t.Fatalf("boot re-selected a different dev user: %+v", boot.cfg)
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

func TestCommitNodeStoreMutationSyncFailureRollsBack(t *testing.T) {
	a := testApp(t)
	before := newStore()
	before.RuntimeConfig = a.cfg.runtimeConfig()
	before.Nodes = []Node{{ID: "old", Name: "old", Protocol: "trojan", RawURL: "trojan://secret@old:443"}}
	before.DefaultNodeID = "old"
	if err := a.saveStore(before); err != nil {
		t.Fatal(err)
	}
	st := cloneStore(before)
	syncCalls := 0
	err := a.commitStoreMutationWithRuntimeOps(st, func(candidate *Store) error {
		candidate.Nodes[0].Name = "new"
		return nil
	}, storeRuntimeSyncXray, func(_ *App, candidate *Store, _ storeRuntimeSyncMode) error {
		syncCalls++
		if syncCalls == 1 {
			return errors.New("xray rejected candidate")
		}
		if candidate.Nodes[0].Name != "old" {
			t.Fatalf("rollback runtime got %+v", candidate.Nodes[0])
		}
		return nil
	}, func(*App, *Store) error { return nil }, func(*App, *Store) error {
		t.Fatalf("persist must not run after sync failure")
		return nil
	})
	if err == nil {
		t.Fatalf("expected sync failure")
	}
	if !reflect.DeepEqual(st, before) {
		t.Fatalf("memory not rolled back: got %+v want %+v", st, before)
	}
	got, loadErr := a.loadStore()
	if loadErr != nil || !reflect.DeepEqual(got, before) {
		t.Fatalf("disk changed after sync failure: got=%+v err=%v", got, loadErr)
	}
}

func TestCommitNodeStoreMutationSaveFailureRestoresDisk(t *testing.T) {
	a := testApp(t)
	before := newStore()
	before.RuntimeConfig = a.cfg.runtimeConfig()
	before.Nodes = []Node{{ID: "old", Name: "old", Protocol: "trojan", RawURL: "trojan://secret@old:443"}}
	before.DefaultNodeID = "old"
	if err := a.saveStore(before); err != nil {
		t.Fatal(err)
	}
	st := cloneStore(before)
	persistCalls := 0
	err := a.commitStoreMutationWithRuntimeOps(st, func(candidate *Store) error {
		candidate.Nodes[0].Name = "new"
		return nil
	}, storeRuntimeSyncXray, func(*App, *Store, storeRuntimeSyncMode) error { return nil }, func(*App, *Store) error { return nil }, func(app *App, candidate *Store) error {
		persistCalls++
		if persistCalls == 1 {
			if err := app.saveStore(candidate); err != nil {
				return err
			}
			return errors.New("simulated fsync failure after rename")
		}
		return app.saveStore(candidate)
	})
	if err == nil {
		t.Fatalf("expected save failure")
	}
	if persistCalls != 2 {
		t.Fatalf("persist calls=%d, want candidate + rollback", persistCalls)
	}
	want := cloneStore(before)
	want.Generation++
	if !reflect.DeepEqual(st, want) {
		t.Fatalf("memory not rolled back: got %+v want %+v", st, want)
	}
	got, loadErr := a.loadStore()
	if loadErr != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("disk not rolled back: got=%+v want=%+v err=%v", got, want, loadErr)
	}
}

func TestRuntimeConfigChangeUsesFullTransitionAndOldConfigRollback(t *testing.T) {
	oldCfg := DefaultConfig()
	oldCfg.CoreDir = t.TempDir()
	oldCfg.DevTargetUser = "olduser"
	oldCfg.DevHTTPPort = 18091
	newCfg := oldCfg
	newCfg.DevTargetUser = "newuser"
	newCfg.DevHTTPPort = 28091
	a := NewApp(newCfg)
	before := newStore()
	before.RuntimeConfig = oldCfg.runtimeConfig()
	before.SceneEnabled[SceneDev] = true
	st := cloneStore(before)
	events := []string{}
	syncCalls := 0
	err := a.commitStoreMutationWithRuntimeOps(st, func(*Store) error { return nil }, storeRuntimeSyncNone,
		func(app *App, state *Store, mode storeRuntimeSyncMode) error {
			syncCalls++
			events = append(events, "sync:"+app.cfg.DevTargetUser)
			wantMode := storeRuntimeSyncTransition
			if syncCalls > 1 {
				wantMode = storeRuntimeSyncAll
			}
			if mode != wantMode {
				t.Fatalf("runtime transition call %d used mode %d, want %d", syncCalls, mode, wantMode)
			}
			if syncCalls == 1 {
				if app.cfg.DevTargetUser != "newuser" || state.RuntimeConfig.DevTargetUser != "newuser" {
					t.Fatalf("candidate sync used wrong config: app=%+v state=%+v", app.cfg, state.RuntimeConfig)
				}
				return errors.New("candidate sync failed")
			}
			if app.cfg.DevTargetUser != "olduser" || state.RuntimeConfig.DevTargetUser != "olduser" {
				t.Fatalf("rollback sync did not use old config: app=%+v state=%+v", app.cfg, state.RuntimeConfig)
			}
			return nil
		},
		func(app *App, _ *Store) error {
			events = append(events, "cleanup:"+app.cfg.DevTargetUser)
			return nil
		},
		func(*App, *Store) error {
			t.Fatal("failed sync without cleanup journal must not persist")
			return nil
		})
	if err == nil {
		t.Fatal("candidate sync failure was lost")
	}
	wantEvents := []string{"cleanup:olduser", "sync:newuser", "cleanup:newuser", "sync:olduser"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("transition order = %v, want %v", events, wantEvents)
	}
	if !reflect.DeepEqual(st, before) {
		t.Fatalf("old Store was not restored: got=%+v want=%+v", st, before)
	}
}

func TestRuntimeConfigSaveFailurePersistsOldConfig(t *testing.T) {
	oldCfg := DefaultConfig()
	oldCfg.CoreDir = t.TempDir()
	oldCfg.DevTargetUser = "olduser"
	newCfg := oldCfg
	newCfg.DevTargetUser = "newuser"
	a := NewApp(newCfg)
	before := newStore()
	before.RuntimeConfig = oldCfg.runtimeConfig()
	st := cloneStore(before)
	persistCalls := 0
	err := a.commitStoreMutationWithRuntimeOps(st, func(*Store) error { return nil }, storeRuntimeSyncNone,
		func(*App, *Store, storeRuntimeSyncMode) error { return nil },
		func(*App, *Store) error { return nil },
		func(app *App, state *Store) error {
			persistCalls++
			if persistCalls == 1 {
				if app.cfg.DevTargetUser != "newuser" || state.RuntimeConfig.DevTargetUser != "newuser" {
					t.Fatalf("candidate persistence used wrong config")
				}
				return errors.New("candidate save failed")
			}
			if app.cfg.DevTargetUser != "olduser" || state.RuntimeConfig.DevTargetUser != "olduser" {
				t.Fatalf("rollback persistence used candidate config: app=%+v state=%+v", app.cfg, state.RuntimeConfig)
			}
			return nil
		})
	if err == nil || persistCalls != 2 {
		t.Fatalf("save failure transaction mismatch: calls=%d err=%v", persistCalls, err)
	}
	if !reflect.DeepEqual(st, before) {
		t.Fatalf("save failure did not restore old config: got=%+v want=%+v", st, before)
	}
}

func TestLegacyActiveStoreWithExplicitRuntimeOverrideUsesFullReconcile(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CoreDir = t.TempDir()
	cfg.DevHTTPPort = 28091
	cfg.runtimeOverrides.DevHTTPPort = true
	a := NewApp(cfg)
	st := newStore()
	st.SceneEnabled[SceneGlobal] = true
	st.Nodes = []Node{{ID: "node-one", Name: "one", Protocol: "trojan", RawURL: "trojan://secret@example.com:443"}}
	st.DefaultNodeID = "node-one"
	seenMode := storeRuntimeSyncNone
	if err := a.commitStoreMutationWithRuntimeOps(st, func(*Store) error { return nil }, storeRuntimeSyncNone,
		func(_ *App, _ *Store, mode storeRuntimeSyncMode) error {
			seenMode = mode
			return nil
		},
		func(*App, *Store) error { return nil },
		func(*App, *Store) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if seenMode != storeRuntimeSyncTransition || st.RuntimeConfig == nil || st.RuntimeConfig.DevHTTPPort != 28091 {
		t.Fatalf("legacy active migration skipped full reconcile: mode=%d state=%+v", seenMode, st.RuntimeConfig)
	}
}

func TestRuntimeConfigRollbackRetainsCandidateTelegramCleanupWhileOldSceneEnabled(t *testing.T) {
	oldCfg := DefaultConfig()
	oldCfg.CoreDir = t.TempDir()
	oldCfg.TGHTTPPort = 18092
	newCfg := oldCfg
	newCfg.TGHTTPPort = 28092
	a := NewApp(newCfg)

	before := newStore()
	before.RuntimeConfig = oldCfg.runtimeConfig()
	before.SceneEnabled[SceneTelegram] = true
	before.TelegramTargets = []string{"old-telegram.service"}
	st := cloneStore(before)

	syncCalls := 0
	cleanupCalls := 0
	persistCalls := 0
	err := a.commitStoreMutationWithRuntimeOps(st, func(*Store) error { return nil }, storeRuntimeSyncAll,
		func(app *App, state *Store, mode storeRuntimeSyncMode) error {
			syncCalls++
			switch syncCalls {
			case 1:
				if mode != storeRuntimeSyncTransition || app.cfg.TGHTTPPort != newCfg.TGHTTPPort {
					t.Fatalf("candidate sync used app=%+v mode=%d", app.cfg, mode)
				}
				state.TelegramTargets = []string{"new-telegram.service"}
				return errors.New("candidate reload failed")
			case 2:
				if mode != storeRuntimeSyncAll || app.cfg.TGHTTPPort != oldCfg.TGHTTPPort {
					t.Fatalf("rollback sync used app=%+v mode=%d", app.cfg, mode)
				}
				want := []string{"old-telegram.service", "new-telegram.service"}
				if !reflect.DeepEqual(state.TelegramTargets, want) {
					t.Fatalf("rollback lost candidate cleanup target: got=%v want=%v", state.TelegramTargets, want)
				}
				return nil
			default:
				t.Fatalf("unexpected sync call %d", syncCalls)
				return nil
			}
		},
		func(app *App, state *Store) error {
			cleanupCalls++
			if cleanupCalls == 1 {
				if app.cfg.TGHTTPPort != oldCfg.TGHTTPPort {
					t.Fatalf("old cleanup used candidate config: %+v", app.cfg)
				}
				state.TelegramTargets = nil
				return nil
			}
			if state.SceneEnabled[SceneTelegram] {
				t.Fatal("candidate rollback cleanup must be ownership-driven, not discovery-driven")
			}
			if !reflect.DeepEqual(state.TelegramTargets, []string{"new-telegram.service"}) {
				t.Fatalf("candidate cleanup state=%+v", state)
			}
			return errors.New("candidate cleanup failed")
		},
		func(app *App, state *Store) error {
			persistCalls++
			if app.cfg.TGHTTPPort != oldCfg.TGHTTPPort {
				t.Fatalf("cleanup journal used candidate config: %+v", app.cfg)
			}
			want := []string{"old-telegram.service", "new-telegram.service"}
			if !reflect.DeepEqual(state.TelegramTargets, want) {
				t.Fatalf("persisted cleanup journal=%v want=%v", state.TelegramTargets, want)
			}
			return nil
		})
	if err == nil || syncCalls != 2 || cleanupCalls != 2 || persistCalls != 1 {
		t.Fatalf("transaction calls sync=%d cleanup=%d persist=%d err=%v", syncCalls, cleanupCalls, persistCalls, err)
	}
	wantTargets := []string{"old-telegram.service", "new-telegram.service"}
	if !st.SceneEnabled[SceneTelegram] || !reflect.DeepEqual(st.TelegramTargets, wantTargets) {
		t.Fatalf("in-memory cleanup journal was lost: %+v", st)
	}
}

func TestRuntimeConfigRollbackSkipsUnownedCandidateDevCleanup(t *testing.T) {
	oldCfg := DefaultConfig()
	oldCfg.CoreDir = t.TempDir()
	oldCfg.DevTargetUser = "root"
	newCfg := oldCfg
	newCfg.DevTargetUser = "nobody"
	a := NewApp(newCfg)

	before := newStore()
	before.RuntimeConfig = oldCfg.runtimeConfig()
	before.SceneEnabled[SceneDev] = true
	st := cloneStore(before)
	cleanupCalls := 0
	syncCalls := 0
	err := a.commitStoreMutationWithRuntimeOps(st, func(*Store) error { return nil }, storeRuntimeSyncAll,
		func(_ *App, _ *Store, mode storeRuntimeSyncMode) error {
			syncCalls++
			if syncCalls == 1 {
				if mode != storeRuntimeSyncTransition {
					t.Fatalf("candidate mode=%d", mode)
				}
				return errors.New("failed before candidate scenes were applied")
			}
			if mode != storeRuntimeSyncAll {
				t.Fatalf("rollback mode=%d", mode)
			}
			return nil
		},
		func(_ *App, state *Store) error {
			cleanupCalls++
			if cleanupCalls == 2 && state.SceneEnabled[SceneDev] {
				t.Fatal("candidate Dev cleanup ran without a backup ownership record")
			}
			return nil
		},
		func(*App, *Store) error { return nil })
	if err == nil || cleanupCalls != 2 || syncCalls != 2 {
		t.Fatalf("transaction calls sync=%d cleanup=%d err=%v", syncCalls, cleanupCalls, err)
	}
	if !reflect.DeepEqual(st, before) {
		t.Fatalf("rollback state mismatch: got=%+v want=%+v", st, before)
	}
}

func TestLegacyRollbackDoesNotInventCandidateRuntimeConfig(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CoreDir = t.TempDir()
	cfg.DevHTTPPort = 28091
	cfg.runtimeOverrides.DevHTTPPort = true
	a := NewApp(cfg)
	st := newStore()
	persistCalls := 0
	err := a.commitStoreMutationWithRuntimeOps(st, func(*Store) error { return nil }, storeRuntimeSyncNone,
		func(*App, *Store, storeRuntimeSyncMode) error { return nil },
		func(*App, *Store) error { return nil },
		func(_ *App, state *Store) error {
			persistCalls++
			if persistCalls == 1 {
				return errors.New("candidate save failed")
			}
			if state.RuntimeConfig != nil {
				t.Fatalf("legacy rollback invented candidate runtime: %+v", state.RuntimeConfig)
			}
			return nil
		})
	if err == nil || persistCalls != 2 {
		t.Fatalf("legacy rollback mismatch: calls=%d err=%v", persistCalls, err)
	}
	if st.RuntimeConfig != nil {
		t.Fatalf("legacy in-memory rollback invented runtime: %+v", st.RuntimeConfig)
	}
}

func TestTelegramRollbackCleanupJournalSurvivesNewAppAndReplays(t *testing.T) {
	a := testApp(t)
	before := newStore()
	before.RuntimeConfig = a.cfg.runtimeConfig()
	if err := a.saveStore(before); err != nil {
		t.Fatal(err)
	}
	st := cloneStore(before)
	syncCalls := 0
	err := a.commitStoreMutationWithRuntimeOps(st, func(candidate *Store) error {
		candidate.SceneEnabled[SceneTelegram] = true
		return nil
	}, storeRuntimeSyncAll,
		func(_ *App, state *Store, _ storeRuntimeSyncMode) error {
			syncCalls++
			if syncCalls == 1 {
				state.TelegramTargets = []string{"hermes-journal-replay.service"}
				return errors.New("candidate daemon-reload failed")
			}
			if state.SceneEnabled[SceneTelegram] {
				t.Fatal("rollback cleanup did not restore scene-off state")
			}
			return errors.New("rollback daemon-reload failed")
		},
		func(*App, *Store) error { return nil },
		func(app *App, state *Store) error { return app.saveStore(state) })
	if err == nil || syncCalls != 2 {
		t.Fatalf("expected apply+rollback reload failures: calls=%d err=%v", syncCalls, err)
	}
	if st.SceneEnabled[SceneTelegram] || !reflect.DeepEqual(st.TelegramTargets, []string{"hermes-journal-replay.service"}) {
		t.Fatalf("pending cleanup ownership was lost in memory: %+v", st)
	}

	restarted := NewApp(a.cfg)
	loaded, err := restarted.loadStoreForBoot()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SceneEnabled[SceneTelegram] || !reflect.DeepEqual(loaded.TelegramTargets, st.TelegramTargets) {
		t.Fatalf("pending cleanup ownership was lost across process restart: %+v", loaded)
	}
	if err := restarted.commitStoreMutationWithRuntimeOps(loaded, func(*Store) error { return nil }, storeRuntimeSyncAll,
		func(_ *App, state *Store, _ storeRuntimeSyncMode) error {
			if state.SceneEnabled[SceneTelegram] || !reflect.DeepEqual(state.TelegramTargets, []string{"hermes-journal-replay.service"}) {
				t.Fatalf("retry did not receive persisted scene-off ownership: %+v", state)
			}
			state.TelegramTargets = nil
			return nil
		},
		func(*App, *Store) error { return nil },
		func(app *App, state *Store) error { return app.saveStore(state) }); err != nil {
		t.Fatal(err)
	}
	verified, err := NewApp(a.cfg).loadStoreForBoot()
	if err != nil {
		t.Fatal(err)
	}
	if len(verified.TelegramTargets) != 0 {
		t.Fatalf("successful replay did not clear ownership: %+v", verified.TelegramTargets)
	}
}

func TestNodeCommitWithoutScenesRemovesStaleXrayConfig(t *testing.T) {
	a := testApp(t)
	st := newStore()
	oldRun := systemctlRun
	oldOutput := systemctlOutput
	t.Cleanup(func() {
		systemctlRun = oldRun
		systemctlOutput = oldOutput
	})
	systemctlRun = func(string, ...string) error { return nil }
	systemctlOutput = func(string, ...string) (string, error) { return "inactive\n", nil }
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
}

func TestNodeCommitWithoutScenesStopFailurePreservesConfigAndRollsBack(t *testing.T) {
	a := testApp(t)
	before := newStore()
	before.Nodes = []Node{{ID: "old", Name: "old", Protocol: "trojan", RawURL: "trojan://secret@old:443"}}
	before.DefaultNodeID = "old"
	if err := a.saveStore(before); err != nil {
		t.Fatal(err)
	}
	st := cloneStore(before)
	credentialConfig := []byte(`{"password":"credential-must-survive"}`)
	if err := os.WriteFile(a.cfg.XrayConfig(), credentialConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	oldRun := systemctlRun
	oldOutput := systemctlOutput
	t.Cleanup(func() {
		systemctlRun = oldRun
		systemctlOutput = oldOutput
	})
	systemctlRun = func(string, ...string) error { return errors.New("stop failed") }
	systemctlOutput = func(string, ...string) (string, error) { return "active\n", nil }

	err := a.commitNodeStoreMutation(st, func(candidate *Store) error {
		candidate.Nodes[0].Name = "new"
		return nil
	})
	if err == nil {
		t.Fatal("stop failure must reject the node mutation")
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
