package manager

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func runtimeTransactionFixture(t *testing.T) (*coreRuntimeFixture, string, string) {
	t.Helper()
	f := newCoreRuntimeFixture(t)
	profile, apt := withGlobalProxyTestPaths(t, f.app)
	if err := f.app.saveStore(f.store); err != nil {
		t.Fatal(err)
	}
	return f, profile, apt
}
func assertRuntimeNoGlobalOwnership(t *testing.T, a *App, paths ...string) {
	t.Helper()
	assertGlobalProxyJournalCopiesAbsent(t, a)
	for _, path := range paths {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unexpected Global file %s: %v", path, err)
		}
	}
}
func TestRuntimeDiscoveryFailurePrecedesAllProductionEffects(t *testing.T) {
	f, profile, apt := runtimeTransactionFixture(t)
	before, _ := os.ReadFile(f.app.cfg.StorePath())
	config := f.file(f.app.cfg.XrayConfig())
	unit := f.file(f.unit)
	root := t.TempDir()
	writeTestUnitFile(t, filepath.Join(root, "hermes-gateway.service"), "[Service]\nExecStart=/usr/bin/python -m hermes_cli.main gateway run\n")
	if err := os.Symlink("missing.service", filepath.Join(root, "dbus-org.freedesktop.timesync1.service")); err != nil {
		t.Fatal(err)
	}
	old := telegramDiscoverTargetNames
	t.Cleanup(func() { telegramDiscoverTargetNames = old })
	telegramDiscoverTargetNames = func() ([]string, error) {
		return discoverEffectiveTelegramUnits([]unitSearchRoot{{Path: root, Manage: true}}, "")
	}
	err := f.app.commitStoreMutation(f.store, func(s *Store) error { s.SceneEnabled[SceneTelegram] = true; return nil }, storeRuntimeSyncTelegram)
	if err == nil || !strings.Contains(err.Error(), "timesync1") {
		t.Fatalf("expected precise discovery refusal: %v", err)
	}
	assertRuntimeNoGlobalOwnership(t, f.app, profile, apt)
	after, _ := os.ReadFile(f.app.cfg.StorePath())
	if !bytes.Equal(before, after) || !coreFilesEqual(config, f.file(f.app.cfg.XrayConfig())) || !coreFilesEqual(unit, f.file(f.unit)) || len(f.commands) != 0 || len(f.checked) != 0 {
		t.Fatal("discovery failure changed runtime or reached core preparation")
	}
	if pending, _ := f.app.hasRuntimeTransition(); pending {
		t.Fatal("failed preflight created a production transition")
	}
}
func TestRuntimeFirstExecutionFailureCannotAcquireUntouchedGlobal(t *testing.T) {
	f, profile, apt := runtimeTransactionFixture(t)
	before, _ := os.ReadFile(f.app.cfg.StorePath())
	old := runtimeApplyCore
	t.Cleanup(func() { runtimeApplyCore = old })
	failure := errors.New("injected core failure before any write")
	runtimeApplyCore = func(*App, *runtimeCorePlan) error { return failure }
	err := f.app.commitStoreMutation(f.store, func(*Store) error { return nil }, storeRuntimeSyncAll)
	if !errors.Is(err, failure) {
		t.Fatalf("injected failure lost: %v", err)
	}
	assertRuntimeNoGlobalOwnership(t, f.app, profile, apt)
	after, _ := os.ReadFile(f.app.cfg.StorePath())
	if !bytes.Equal(before, after) || len(f.commands) != 0 {
		t.Fatalf("failed core acquired/restarted unrelated state: %v %v", err, f.commands)
	}
	if pending, _ := f.app.hasRuntimeTransition(); pending {
		t.Fatalf("untouched compensation stranded record: %v", err)
	}
}
func TestRuntimeStoreFailureRestoresNewGlobalWithoutReplayingOtherScenes(t *testing.T) {
	f, profile, apt := runtimeTransactionFixture(t)
	f.writeReceipt(f.plan(), nil)
	before, _ := os.ReadFile(f.app.cfg.StorePath())
	old := runtimePersistStore
	t.Cleanup(func() { runtimePersistStore = old })
	failure := errors.New("injected store failure")
	runtimePersistStore = func(*App, *Store) error { return failure }
	err := f.app.commitStoreMutation(f.store, func(*Store) error { return nil }, storeRuntimeSyncGlobal)
	if !errors.Is(err, failure) {
		t.Fatalf("injected failure lost: %v", err)
	}
	assertRuntimeNoGlobalOwnership(t, f.app, profile, apt)
	after, _ := os.ReadFile(f.app.cfg.StorePath())
	if !bytes.Equal(before, after) || len(f.commands) != 0 {
		t.Fatalf("Store compensation changed unrelated state: %v", err)
	}
	if pending, _ := f.app.hasRuntimeTransition(); pending {
		t.Fatalf("compensation incomplete: %v", err)
	}
}
func TestRuntimeRecoverAfterStartedGlobalUsesFixedSnapshot(t *testing.T) {
	f, profile, apt := runtimeTransactionFixture(t)
	f.writeReceipt(f.plan(), nil)
	before := cloneStore(f.store)
	candidate := cloneStore(f.store)
	plan, err := f.app.planRuntimeMutation(before, candidate, storeRuntimeSyncGlobal)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.record.Steps) != 1 || plan.record.Steps[0].Name != string(SceneGlobal) {
		t.Fatalf("unexpected plan: %+v", plan.record.Steps)
	}
	plan.record.Steps[0].Started = true
	if err := f.app.writeRuntimeTransition(plan.record); err != nil {
		t.Fatal(err)
	}
	if err := f.app.executeRuntimeStep(plan, string(SceneGlobal)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(profile); err != nil {
		t.Fatal("fixture did not apply Global")
	}
	old := telegramDiscoverTargetNames
	t.Cleanup(func() { telegramDiscoverTargetNames = old })
	telegramDiscoverTargetNames = func() ([]string, error) { t.Fatal("recovery rediscovered targets"); return nil, nil }
	restarted := NewApp(f.app.cfg)
	recovered, err := restarted.recoverRuntimeTransition()
	if err != nil || !recovered {
		t.Fatalf("fixed recovery failed: %t %v", recovered, err)
	}
	assertRuntimeNoGlobalOwnership(t, restarted, profile, apt)
	if len(f.commands) != 0 {
		t.Fatal("unstarted core was coordinated")
	}
}
func TestRuntimeRecoveryRejectsChangedStoreBeforeRestoringResources(t *testing.T) {
	f, profile, _ := runtimeTransactionFixture(t)
	f.writeReceipt(f.plan(), nil)
	plan, err := f.app.planRuntimeMutation(cloneStore(f.store), cloneStore(f.store), storeRuntimeSyncGlobal)
	if err != nil {
		t.Fatal(err)
	}
	plan.record.Steps[0].Started = true
	if err := f.app.writeRuntimeTransition(plan.record); err != nil {
		t.Fatal(err)
	}
	if err := f.app.executeRuntimeStep(plan, string(SceneGlobal)); err != nil {
		t.Fatal(err)
	}
	applied, _ := os.ReadFile(profile)
	external := cloneStore(f.store)
	external.Nodes[0].Name = "administrator"
	if err := f.app.saveStore(external); err != nil {
		t.Fatal(err)
	}
	if _, err := NewApp(f.app.cfg).recoverRuntimeTransition(); err == nil {
		t.Fatal("external Store accepted for compensation")
	}
	after, _ := os.ReadFile(profile)
	if !bytes.Equal(applied, after) {
		t.Fatal("resources changed before rejecting Store drift")
	}
	if pending, _ := f.app.hasRuntimeTransition(); !pending {
		t.Fatal("recovery evidence lost")
	}
}
func TestRuntimeCommitConfirmedAfterLostTransitionFinalization(t *testing.T) {
	f, profile, _ := runtimeTransactionFixture(t)
	f.writeReceipt(f.plan(), nil)
	old := runtimeWriteTransition
	t.Cleanup(func() { runtimeWriteTransition = old })
	runtimeWriteTransition = func(path string, data []byte, mode os.FileMode) error {
		if bytes.Contains(data, []byte(`"phase":"committed"`)) {
			return errors.New("injected final receipt failure")
		}
		return old(path, data, mode)
	}
	if err := f.app.commitStoreMutation(f.store, func(*Store) error { return nil }, storeRuntimeSyncGlobal); err == nil {
		t.Fatal("injected finalization failure lost")
	}
	live, _ := os.ReadFile(profile)
	runtimeWriteTransition = old
	recovered, err := NewApp(f.app.cfg).recoverRuntimeTransition()
	if err != nil || !recovered {
		t.Fatalf("could not confirm committed main: %v", err)
	}
	after, _ := os.ReadFile(profile)
	if !bytes.Equal(live, after) {
		t.Fatal("committed runtime was rolled back")
	}
	if _, err := f.app.loadGlobalProxyJournal(); err != nil {
		t.Fatal(err)
	}
}
func TestRuntimeMetadataEditDoesNotInventHistoricalRuntime(t *testing.T) {
	a := testApp(t)
	st := newStore()
	st.Nodes = []Node{{ID: "fixture", Name: "old", Protocol: "trojan", RawURL: "trojan://s@fixture.example:443"}}
	st.DefaultNodeID = "fixture"
	st.SceneEnabled[SceneGlobal] = true
	if err := a.saveStore(st); err != nil {
		t.Fatal(err)
	}
	before := cloneStore(st)
	if err := a.commitStoreMutation(st, func(s *Store) error { s.Nodes[0].Name = "new"; return nil }, storeRuntimeSyncNone); err != nil {
		t.Fatal(err)
	}
	if st.RuntimeConfig != nil || !reflect.DeepEqual(st.SceneEnabled, before.SceneEnabled) {
		t.Fatal("metadata edit guessed historical runtime")
	}
}
func TestRuntimeLegacyMissingConfigurationFailsBeforeCoreOrCleanup(t *testing.T) {
	f, profile, apt := runtimeTransactionFixture(t)
	f.store.RuntimeConfig = nil
	if err := f.app.saveStore(f.store); err != nil {
		t.Fatal(err)
	}
	err := f.app.commitStoreMutation(f.store, func(s *Store) error { s.SceneEnabled[SceneGlobal] = false; return nil }, storeRuntimeSyncGlobal)
	if err == nil || !strings.Contains(err.Error(), "RuntimeConfig") {
		t.Fatalf("legacy accepted without history: %v", err)
	}
	assertRuntimeNoGlobalOwnership(t, f.app, profile, apt)
	if len(f.commands) != 0 || len(f.checked) != 0 {
		t.Fatal("legacy preflight modified core")
	}
}
