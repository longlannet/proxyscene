package manager

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"testing"
)

func prepareSubscriptionRuntimeFixture(t *testing.T, f *coreRuntimeFixture) {
	t.Helper()
	withGlobalProxyTestPaths(t, f.app)
	f.app.cfg.DevTargetUser = "root"
	f.store.RuntimeConfig = f.app.cfg.runtimeConfig()
	config, err := f.app.renderXrayConfig(f.store)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.app.cfg.XrayConfig(), config, 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.app.saveStore(f.store); err != nil {
		t.Fatal(err)
	}
	f.writeReceipt(f.plan(), nil)
	f.checked = nil
}

func TestSubscriptionReplacementUpdatesEnabledScenesWithStoredRuntimeConfig(t *testing.T) {
	f := newCoreRuntimeFixture(t)
	manual := reconcileTestNode(t, "manual", false)
	oldA := reconcileTestNode(t, "old-a", true, reconcileSourceA)
	oldB := reconcileTestNode(t, "old-b", true, reconcileSourceB)
	f.store = reconcileTestStore(manual, oldA, oldB)
	f.store.DefaultNodeID = oldA.ID
	f.store.SceneNodes[SceneDev] = oldB.ID
	f.store.SceneNodes[SceneTelegram] = manual.ID
	for _, scene := range []Scene{SceneGlobal, SceneDev, SceneTelegram} {
		f.store.SceneEnabled[scene] = true
	}
	f.store.SpeedResults[oldA.ID] = SpeedResult{NodeID: oldA.ID, Success: true}
	prepareSubscriptionRuntimeFixture(t, f)
	before := cloneStore(f.store)
	oldUnit := f.file(f.unit)
	storedApp := NewApp(before.RuntimeConfig.applyTo(f.app.cfg))

	// A subscription refresh must not adopt unrelated environment overrides.
	f.app.cfg.GlobalHTTPPort += 1000
	f.app.cfg.DevHTTPPort += 1000
	f.app.cfg.ProxyHost = "127.0.0.2"
	f.app.cfg.XrayServiceUser = "unexpected-subscription-user"
	f.app.cfg.runtimeOverrides = runtimeConfigOverrideMask{GlobalHTTPPort: true, DevHTTPPort: true, ProxyHost: true, XrayServiceUser: true}
	newA := reconcileTestNode(t, "new-a", false)
	newB := reconcileTestNode(t, "new-b", false)
	if err := f.app.commitPreparedSubscriptions(f.store, []preparedSubscription{
		reconcileTestRefresh(t, reconcileSourceA, newA, reconcileTestNode(t, "second-a", false)),
		reconcileTestRefresh(t, reconcileSourceB, newB),
	}); err != nil {
		t.Fatal(err)
	}
	if f.store.findNode(oldA.ID) != nil || f.store.findNode(oldB.ID) != nil {
		t.Fatalf("replaced selected nodes remain: %+v", f.store.Nodes)
	}
	selectedA, selectedB := f.store.findNodeByURL(newA.RawURL), f.store.findNodeByURL(newB.RawURL)
	if selectedA == nil || selectedB == nil || len(f.store.Nodes) != 4 {
		t.Fatalf("unexpected replacement node set: %+v", f.store.Nodes)
	}
	if f.store.DefaultNodeID != selectedA.ID || f.store.selectedNodeID(SceneGlobal) != selectedA.ID || f.store.selectedNodeID(SceneDev) != selectedB.ID || f.store.selectedNodeID(SceneTelegram) != manual.ID {
		t.Fatalf("scene selections did not follow their subscription: default=%s scenes=%+v", f.store.DefaultNodeID, f.store.SceneNodes)
	}
	if _, found := f.store.SpeedResults[oldA.ID]; found {
		t.Fatal("obsolete speed result survived replacement")
	}
	if !reflect.DeepEqual(f.store.findNode(manual.ID), &manual) || !runtimeConfigsEqual(f.store.RuntimeConfig, before.RuntimeConfig) {
		t.Fatalf("refresh changed manual node or stored runtime: node=%+v runtime=%+v", f.store.findNode(manual.ID), f.store.RuntimeConfig)
	}
	wantConfig, err := storedApp.renderXrayConfig(f.store)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f.file(f.app.cfg.XrayConfig()).Content, wantConfig) || !coreFilesEqual(oldUnit, f.file(f.unit)) {
		t.Fatal("replacement did not render the new nodes with the stored ports and service user")
	}
	if f.restarts != 1 || len(f.checked) == 0 {
		t.Fatalf("replacement was not validated and restarted once: restarts=%d checks=%d", f.restarts, len(f.checked))
	}
	persisted, err := f.app.loadStore()
	if err != nil || persisted.Generation != before.Generation+1 {
		t.Fatalf("replacement not committed once: store=%+v err=%v", persisted, err)
	}
	persistedJSON, err := encodedRuntimeStore(persisted)
	if err != nil {
		t.Fatal(err)
	}
	memoryJSON, err := encodedRuntimeStore(f.store)
	if err != nil || !bytes.Equal(persistedJSON, memoryJSON) {
		t.Fatalf("persisted replacement differs from memory: %v", err)
	}
	if pending, err := f.app.hasRuntimeTransition(); err != nil || pending {
		t.Fatalf("successful replacement left a pending transaction: pending=%v err=%v", pending, err)
	}
}

func TestSubscriptionReplacementRestartFailureRestoresOldNodesAndConfig(t *testing.T) {
	f := newCoreRuntimeFixture(t)
	old := reconcileTestNode(t, "old", true, reconcileSourceA)
	f.store = reconcileTestStore(old)
	f.store.SceneEnabled[SceneGlobal] = true
	prepareSubscriptionRuntimeFixture(t, f)
	before := cloneStore(f.store)
	oldConfig, oldUnit := f.file(f.app.cfg.XrayConfig()), f.file(f.unit)
	failure := errors.New("injected subscription restart failure")
	run := systemctlRun
	attempts := 0
	systemctlRun = func(label string, args ...string) error {
		if len(args) > 0 && args[0] == "restart" {
			attempts++
			if attempts == 1 {
				if bytes.Equal(f.file(f.app.cfg.XrayConfig()).Content, oldConfig.Content) {
					t.Fatal("restart failure injected before replacement config was installed")
				}
				return failure
			}
		}
		return run(label, args...)
	}
	err := f.app.commitPreparedSubscriptions(f.store, []preparedSubscription{
		reconcileTestRefresh(t, reconcileSourceA, reconcileTestNode(t, "new", false)),
	})
	if !errors.Is(err, failure) || attempts != 2 {
		t.Fatalf("restart failure did not restore the old runtime: attempts=%d err=%v", attempts, err)
	}
	if !reflect.DeepEqual(f.store, before) {
		t.Fatalf("failed replacement changed in-memory nodes: got=%+v want=%+v", f.store, before)
	}
	assertSubscriptionStoreUnchanged(t, f.app, before)
	if !coreFilesEqual(oldConfig, f.file(f.app.cfg.XrayConfig())) || !coreFilesEqual(oldUnit, f.file(f.unit)) || f.service.Active != "active" {
		t.Fatal("failed replacement did not restore old config and running service")
	}
	if pending, err := f.app.hasRuntimeTransition(); err != nil || pending {
		t.Fatalf("successful rollback left a pending transaction: pending=%v err=%v", pending, err)
	}
}

func TestSubscriptionReplacementStoreFailureRestoresOldRuntime(t *testing.T) {
	f := newCoreRuntimeFixture(t)
	old := reconcileTestNode(t, "old", true, reconcileSourceA)
	f.store = reconcileTestStore(old)
	f.store.SceneEnabled[SceneGlobal] = true
	prepareSubscriptionRuntimeFixture(t, f)
	before := cloneStore(f.store)
	oldConfig := f.file(f.app.cfg.XrayConfig())
	failure := errors.New("injected subscription store failure")
	persist := runtimePersistStore
	t.Cleanup(func() { runtimePersistStore = persist })
	failed := false
	runtimePersistStore = func(*App, *Store) error {
		if f.restarts != 1 || bytes.Equal(f.file(f.app.cfg.XrayConfig()).Content, oldConfig.Content) {
			t.Fatal("store failure injected before replacement runtime was applied")
		}
		failed = true
		return failure
	}
	err := f.app.commitPreparedSubscriptions(f.store, []preparedSubscription{
		reconcileTestRefresh(t, reconcileSourceA, reconcileTestNode(t, "new", false)),
	})
	if !errors.Is(err, failure) || !failed || f.restarts != 2 {
		t.Fatalf("persistence failure did not restore old runtime: failed=%v restarts=%d err=%v", failed, f.restarts, err)
	}
	if !reflect.DeepEqual(f.store, before) || !coreFilesEqual(oldConfig, f.file(f.app.cfg.XrayConfig())) {
		t.Fatal("persistence failure left replacement nodes or config")
	}
	assertSubscriptionStoreUnchanged(t, f.app, before)
	if pending, err := f.app.hasRuntimeTransition(); err != nil || pending {
		t.Fatalf("successful store rollback left a transaction: pending=%v err=%v", pending, err)
	}
}

func TestSubscriptionReplacementWithoutEnabledConnectionChangeDoesNotRestart(t *testing.T) {
	for _, equivalentSelection := range []bool{false, true} {
		name := "disabled scene changes connection"
		if equivalentSelection {
			name = "enabled scene moves to equivalent node"
		}
		t.Run(name, func(t *testing.T) {
			f := newCoreRuntimeFixture(t)
			old := reconcileTestNode(t, "old", true, reconcileSourceA)
			keeper := reconcileTestNode(t, "keeper", false)
			incoming := reconcileTestNode(t, "new", false)
			if equivalentSelection {
				keeper.RawURL = old.RawURL + "#keeper"
				incoming.RawURL = old.RawURL + "#refreshed"
			}
			f.store = reconcileTestStore(keeper, old)
			f.store.SceneEnabled[SceneGlobal] = true
			if equivalentSelection {
				f.store.SceneNodes[SceneGlobal] = old.ID
			} else {
				f.store.SceneNodes[SceneDev] = old.ID
			}
			prepareSubscriptionRuntimeFixture(t, f)
			beforeConfig := f.file(f.app.cfg.XrayConfig())
			beforeGeneration := f.store.Generation
			systemctlRun = func(string, ...string) error {
				t.Fatal("unchanged enabled connections invoked systemctl")
				return nil
			}
			systemctlOutput = func(string, ...string) (string, error) {
				t.Fatal("unchanged enabled connections queried systemctl")
				return "", nil
			}
			if err := f.app.commitPreparedSubscriptions(f.store, []preparedSubscription{reconcileTestRefresh(t, reconcileSourceA, incoming)}); err != nil {
				t.Fatal(err)
			}
			if f.store.findNode(old.ID) != nil || f.store.selectedNodeID(SceneGlobal) != keeper.ID {
				t.Fatalf("obsolete selection was retained: %+v", f.store)
			}
			if !equivalentSelection {
				replacement := f.store.findNodeByURL(incoming.RawURL)
				if replacement == nil || f.store.selectedNodeID(SceneDev) != replacement.ID {
					t.Fatalf("disabled scene lost its replacement selection: %+v", f.store.SceneNodes)
				}
			}
			if !coreFilesEqual(beforeConfig, f.file(f.app.cfg.XrayConfig())) || len(f.checked) != 0 || f.store.Generation != beforeGeneration+1 {
				t.Fatalf("metadata replacement changed running config or did not commit once: checks=%d generation=%d", len(f.checked), f.store.Generation)
			}
		})
	}
}
