package manager

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

type removeAllNodesRuntimeFixture struct {
	core     *coreRuntimeFixture
	telegram *telegramJournalTestHarness
	profile  string
	apt      string
	devUser  string
	devOwned *devProxyBackup
	devLive  *devProxyBackup
	restarts int
}

func newRemoveAllNodesRuntimeFixture(t *testing.T) *removeAllNodesRuntimeFixture {
	t.Helper()
	requireDevGit(t)
	telegram := newTelegramJournalTestHarness(t)
	core := newCoreRuntimeFixture(t)
	telegram.app = core.app
	profile, apt := withGlobalProxyTestPaths(t, core.app)
	user, identity := realDevConfigFixture(t)
	core.app.cfg.DevTargetUser = user
	devCommandExists = func(name string) bool { return name == "git" || name == "npm" }
	devIOWrite(t, filepath.Join(identity.Home, ".gitconfig"), "[http]\nproxy = http://original.invalid:8080\n[https]\nproxy = http://original.invalid:8081\n")
	devIOWrite(t, filepath.Join(identity.Home, ".npmrc"), "proxy=http://original.invalid:8082\nregistry=https://registry.npmjs.org/\n")
	f := &removeAllNodesRuntimeFixture{core: core, telegram: telegram, profile: profile, apt: apt, devUser: user}
	coreRun := systemctlRun
	systemctlRun = func(label string, args ...string) error {
		if len(args) > 0 && args[0] == "try-restart" {
			if args[len(args)-1] != telegram.target().Service {
				t.Fatalf("Telegram restart escaped the fixture: %v", args)
			}
			f.restarts++
			return nil
		}
		return coreRun(label, args...)
	}
	manual := reconcileTestNode(t, "manual", false)
	managed := reconcileTestNode(t, "managed", true, reconcileSourceA)
	core.store = reconcileTestStore(manual, managed)
	for _, scene := range []Scene{SceneGlobal, SceneDev, SceneTelegram} {
		core.store.SceneEnabled[scene] = true
		core.store.SceneNodes[scene] = managed.ID
	}
	core.store.SpeedResults[manual.ID] = SpeedResult{NodeID: manual.ID, Success: true}
	core.store.SpeedResults[managed.ID] = SpeedResult{NodeID: managed.ID, Success: true}
	core.store.RuntimeConfig = core.app.cfg.runtimeConfig()
	config, err := core.app.renderXrayConfig(core.store)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(core.app.cfg.XrayConfig(), config, 0600); err != nil {
		t.Fatal(err)
	}
	if err := core.app.applyGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	if err := core.app.applyDev(); err != nil {
		t.Fatal(err)
	}
	if _, err := core.app.applyTelegram(core.store, []systemdTargetName{telegram.target()}); err != nil {
		t.Fatal(err)
	}
	f.devOwned, err = core.app.loadDevBackup()
	if err != nil {
		t.Fatal(err)
	}
	f.devLive, err = snapshotDevProxyConfig(user, f.devOwned, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.app.saveStore(core.store); err != nil {
		t.Fatal(err)
	}
	core.store, err = core.app.loadStore()
	if err != nil {
		t.Fatal(err)
	}
	core.writeReceipt(core.plan(), nil)
	core.commands = nil
	core.checked = nil
	f.restarts = 0
	return f
}

func (f *removeAllNodesRuntimeFixture) assertReleased(t *testing.T) {
	t.Helper()
	assertRuntimeNoGlobalOwnership(t, f.core.app, f.profile, f.apt)
	if _, err := f.core.app.loadDevBackup(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Dev ownership was not released: %v", err)
	}
	actual, err := snapshotDevProxyConfig(f.devUser, f.devOwned, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(actual.GitHTTPProxy, f.devOwned.GitHTTPProxy) || !slices.Equal(actual.GitHTTPSProxy, f.devOwned.GitHTTPSProxy) || !optionalStringsEqual(actual.NPMProxy, f.devOwned.NPMProxy) || !optionalStringsEqual(actual.NPMHTTPSProxy, f.devOwned.NPMHTTPSProxy) {
		t.Fatal("Dev proxy values were not restored to their original values")
	}
	if _, err := os.Stat(f.telegram.systemPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Telegram proxy drop-in was not removed: %v", err)
	}
	journal, err := f.core.app.loadTelegramProxyJournal()
	if err != nil || len(journal.Targets) != 0 {
		t.Fatalf("Telegram ownership was not released: journal=%+v err=%v", journal, err)
	}
}

func TestRuntimeRemoveAllNodesReleasesEveryScene(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		name := "enabled scenes"
		if !enabled {
			name = "disabled scenes with retained ownership"
		}
		t.Run(name, func(t *testing.T) {
			f := newRemoveAllNodesRuntimeFixture(t)
			if !enabled {
				for _, scene := range []Scene{SceneGlobal, SceneDev, SceneTelegram} {
					f.core.store.SceneEnabled[scene] = false
				}
				if err := f.core.app.saveStore(f.core.store); err != nil {
					t.Fatal(err)
				}
			}
			before := cloneStore(f.core.store)
			if err := f.core.app.removeAllNodes(f.core.store); err != nil {
				t.Fatal(err)
			}
			f.assertReleased(t)
			st := f.core.store
			if len(st.Nodes) != 0 || st.DefaultNodeID != "" || len(st.SceneNodes) != 0 || len(st.SpeedResults) != 0 || hasEnabledScene(st) || len(st.TelegramTargets) != 0 {
				t.Fatalf("bulk deletion retained nodes or active scene state: %+v", st)
			}
			if !reflect.DeepEqual(st.Subscriptions, before.Subscriptions) || !runtimeConfigsEqual(st.RuntimeConfig, before.RuntimeConfig) {
				t.Fatal("bulk deletion changed subscription URLs or runtime settings")
			}
			if st.Generation != before.Generation+1 || f.core.service.Active != "inactive" || f.core.service.Enabled != "disabled" || f.restarts != 1 {
				t.Fatalf("bulk deletion did not commit once and stop the runtime: generation=%d service=%+v telegram restarts=%d", st.Generation, f.core.service, f.restarts)
			}
			if _, err := os.Stat(f.core.app.cfg.XrayConfig()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("disabled core retained node configuration: %v", err)
			}
			persisted, err := f.core.app.loadStore()
			if err != nil || !reflect.DeepEqual(persisted, st) {
				t.Fatalf("persisted node deletion differs from memory: store=%+v err=%v", persisted, err)
			}
			if pending, err := f.core.app.hasRuntimeTransition(); err != nil || pending {
				t.Fatalf("completed bulk deletion retained a transaction: pending=%v err=%v", pending, err)
			}
		})
	}
}

func TestRuntimeRemoveAllNodesFailureRestoresNodesAndEveryScene(t *testing.T) {
	for _, failurePoint := range []string{"core stop", "store persistence"} {
		t.Run(failurePoint, func(t *testing.T) {
			f := newRemoveAllNodesRuntimeFixture(t)
			before := cloneStore(f.core.store)
			paths := []string{f.profile, f.apt, f.telegram.systemPath, f.core.app.cfg.XrayConfig(), f.core.unit, f.core.app.cfg.StorePath(), f.core.app.cfg.StoreBackupPath()}
			files := make(map[string]coreFileState, len(paths))
			for _, path := range paths {
				files[path] = f.core.file(path)
			}
			telegramBefore := f.telegram.journal(t)
			failure := errors.New("injected bulk deletion failure")
			failed := false
			if failurePoint == "core stop" {
				run := systemctlRun
				systemctlRun = func(label string, args ...string) error {
					if !failed && len(args) > 0 && args[0] == "stop" {
						f.assertReleased(t)
						failed = true
						return failure
					}
					return run(label, args...)
				}
			} else {
				persist := runtimePersistStore
				t.Cleanup(func() { runtimePersistStore = persist })
				runtimePersistStore = func(*App, *Store) error {
					f.assertReleased(t)
					if f.core.service.Active != "inactive" {
						t.Fatal("store failure injected before the core stopped")
					}
					failed = true
					return failure
				}
			}
			err := f.core.app.removeAllNodes(f.core.store)
			if !errors.Is(err, failure) || !failed {
				t.Fatalf("bulk deletion failure was lost: failed=%v err=%v", failed, err)
			}
			if !reflect.DeepEqual(f.core.store, before) {
				t.Fatal("failed deletion changed in-memory nodes or scene settings")
			}
			for _, path := range paths {
				if !coreFilesEqual(files[path], f.core.file(path)) {
					t.Fatalf("failed deletion did not restore file %s", path)
				}
			}
			assertSubscriptionStoreUnchanged(t, f.core.app, before)
			actual, err := snapshotDevProxyConfig(f.devUser, f.devOwned, true, true)
			if err != nil || !reflect.DeepEqual(actual, f.devLive) {
				t.Fatalf("failed deletion did not restore Dev proxy values: %+v %v", actual, err)
			}
			devOwned, err := f.core.app.loadDevBackup()
			if err != nil || !reflect.DeepEqual(devOwned, f.devOwned) {
				t.Fatalf("failed deletion lost Dev ownership: %+v %v", devOwned, err)
			}
			if !reflect.DeepEqual(f.telegram.journal(t).Targets, telegramBefore.Targets) {
				t.Fatal("failed deletion lost Telegram ownership")
			}
			if _, err := f.core.app.loadGlobalProxyJournal(); err != nil {
				t.Fatalf("failed deletion lost Global ownership: %v", err)
			}
			if f.core.service.Active != "active" || f.core.service.Enabled != "enabled" {
				t.Fatalf("failed deletion did not restore the running core: %+v", f.core.service)
			}
			if pending, err := f.core.app.hasRuntimeTransition(); err != nil || pending {
				t.Fatalf("successful rollback retained a transaction: pending=%v err=%v", pending, err)
			}
		})
	}
}
