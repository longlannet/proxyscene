package manager

import (
	"bytes"
	"os"
	"testing"
)

func TestRuntimeCommitRejectsStoreEditedDuringExecution(t *testing.T) {
	f, _, _ := runtimeTransactionFixture(t)
	oldApply := runtimeApplyCore
	t.Cleanup(func() { runtimeApplyCore = oldApply })
	var external []byte
	applied := false
	runtimeApplyCore = func(app *App, p *runtimeCorePlan) error {
		if err := oldApply(app, p); err != nil {
			return err
		}
		applied = true
		administrator := cloneStore(f.store)
		administrator.Nodes[0].Name = "administrator edit during service execution"
		administrator.Generation++
		var err error
		external, err = encodedRuntimeStore(administrator)
		if err != nil {
			return err
		}
		return os.WriteFile(f.app.cfg.StorePath(), external, 0600)
	}
	err := f.app.commitStoreMutation(f.store, func(candidate *Store) error {
		candidate.Nodes[0].Name = "candidate"
		return nil
	}, storeRuntimeSyncXray)
	if err == nil || !applied {
		t.Fatalf("execution-time Store edit was accepted: applied=%v err=%v", applied, err)
	}
	actual, readErr := os.ReadFile(f.app.cfg.StorePath())
	if readErr != nil || !bytes.Equal(actual, external) {
		t.Fatalf("administrator Store bytes were overwritten: %v", readErr)
	}
	if pending, err := f.app.hasRuntimeTransition(); err != nil || !pending {
		t.Fatalf("Store conflict discarded runtime recovery evidence: pending=%v err=%v", pending, err)
	}
}

func TestRuntimePlanningRejectsStoreEditedDuringPreflight(t *testing.T) {
	f, _, _ := runtimeTransactionFixture(t)
	check := coreCheckConfig
	var external []byte
	checked := false
	coreCheckConfig = func(app *App, path string) error {
		if err := check(app, path); err != nil {
			return err
		}
		checked = true
		administrator := cloneStore(f.store)
		administrator.Nodes[0].Name = "administrator edit during preflight"
		administrator.Generation++
		var err error
		external, err = encodedRuntimeStore(administrator)
		if err != nil {
			return err
		}
		return os.WriteFile(f.app.cfg.StorePath(), external, 0600)
	}
	err := f.app.commitStoreMutation(f.store, func(candidate *Store) error {
		candidate.Nodes[0].Name = "candidate"
		return nil
	}, storeRuntimeSyncXray)
	if err == nil || !checked {
		t.Fatalf("preflight-time Store edit was accepted: checked=%v err=%v", checked, err)
	}
	actual, readErr := os.ReadFile(f.app.cfg.StorePath())
	if readErr != nil || !bytes.Equal(actual, external) {
		t.Fatalf("administrator Store bytes were overwritten: %v", readErr)
	}
	if len(f.commands) > 0 {
		t.Fatalf("changed preflight Store allowed runtime effects: %v", f.commands)
	}
	if pending, err := f.app.hasRuntimeTransition(); err != nil || pending {
		t.Fatalf("failed preflight created recovery record: pending=%v err=%v", pending, err)
	}
}
