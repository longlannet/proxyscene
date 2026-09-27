package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func runtimeStoreCASFixture(t *testing.T) (*App, *Store) {
	t.Helper()
	a, st := storeTransitionFixture(t)
	oldCAS, oldHook := runtimeWriteStoreCAS, globalProxyCASAfterQuarantine
	t.Cleanup(func() {
		runtimeWriteStoreCAS, globalProxyCASAfterQuarantine = oldCAS, oldHook
	})
	runtimeWriteStoreCAS = writeRuntimeStoreCAS
	globalProxyCASAfterQuarantine = func(string) {}
	runtimePlanCore = func(*App, *Store) (*runtimeCorePlan, error) {
		t.Fatal("metadata transaction attempted core planning")
		return nil, nil
	}
	return a, st
}

func assertRuntimeStoreRecoveryFinished(t *testing.T, a *App, want *Store) {
	t.Helper()
	main, err := a.loadStoreRaw()
	if err != nil {
		t.Fatal(err)
	}
	backup, err := a.loadStoreBackup()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(main, backup) {
		t.Fatal("main and backup disagree after recovery")
	}
	semantic := cloneStore(main)
	semantic.Generation = want.Generation
	if !reflect.DeepEqual(semantic, want) {
		t.Fatalf("recovery changed previous Store semantics: got=%+v want=%+v", semantic, want)
	}
	if pending, err := a.hasRuntimeTransition(); err != nil || pending {
		t.Fatalf("completed recovery retained receipt: pending=%v err=%v", pending, err)
	}
	for _, path := range []string{a.cfg.StorePath(), a.cfg.StoreBackupPath()} {
		if _, err := os.Lstat(globalProxyTestQuarantinePath(path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("recovery left quarantine for %s: %v", path, err)
		}
	}
}

func TestRuntimeStoreCASPreservesConcurrentMainCreation(t *testing.T) {
	a, st := runtimeStoreCASFixture(t)
	administrator := cloneStore(st)
	administrator.Nodes[0].Name = "administrator won CAS race"
	administrator.Generation += 10
	external, err := encodedRuntimeStore(administrator)
	if err != nil {
		t.Fatal(err)
	}
	fired := false
	globalProxyCASAfterQuarantine = func(path string) {
		if path != a.cfg.StorePath() || fired {
			return
		}
		fired = true
		if err := os.WriteFile(path, external, 0600); err != nil {
			t.Fatal(err)
		}
	}
	err = a.commitStoreMutation(st, func(candidate *Store) error {
		candidate.Nodes[0].Name = "candidate"
		return nil
	}, storeRuntimeSyncNone)
	if err == nil || !fired {
		t.Fatalf("concurrent main creation accepted: fired=%v err=%v", fired, err)
	}
	main, err := os.ReadFile(a.cfg.StorePath())
	if err != nil || !bytes.Equal(main, external) {
		t.Fatalf("CAS overwrote administrator bytes: %v", err)
	}
	if pending, err := a.hasRuntimeTransition(); err != nil || !pending {
		t.Fatalf("CAS conflict lost receipt: pending=%v err=%v", pending, err)
	}
	if _, err := NewApp(a.cfg).recoverRuntimeTransition(); err == nil {
		t.Fatal("recovery accepted external main content")
	}
	main, err = os.ReadFile(a.cfg.StorePath())
	if err != nil || !bytes.Equal(main, external) {
		t.Fatalf("failed recovery overwrote administrator bytes: %v", err)
	}
}

func TestRuntimeStoreQuarantineRecoversAfterInterruption(t *testing.T) {
	for _, copyName := range []string{"main", "backup"} {
		t.Run(copyName, func(t *testing.T) {
			a, st := runtimeStoreCASFixture(t)
			before := cloneStore(st)
			target := a.cfg.StorePath()
			if copyName == "backup" {
				target = a.cfg.StoreBackupPath()
			}
			interruption := errors.New("simulated process interruption after durable quarantine")
			globalProxyCASAfterQuarantine = func(path string) {
				if path == target {
					panic(interruption)
				}
			}
			func() {
				defer func() {
					if got := recover(); got != interruption {
						t.Fatalf("quarantine interruption was not reached: %v", got)
					}
				}()
				if err := a.commitStoreMutation(st, func(candidate *Store) error {
					candidate.Nodes[0].Name = "candidate"
					return nil
				}, storeRuntimeSyncNone); err != nil {
					t.Fatalf("transaction failed before interruption: %v", err)
				}
			}()
			if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("interruption did not leave missing %s: %v", copyName, err)
			}
			if _, err := os.Lstat(globalProxyTestQuarantinePath(target)); err != nil {
				t.Fatalf("interruption lost quarantined %s: %v", copyName, err)
			}
			globalProxyCASAfterQuarantine = func(string) {}
			restarted := NewApp(a.cfg)
			if recovered, err := restarted.recoverRuntimeTransition(); err != nil || !recovered {
				t.Fatalf("new App could not recover %s quarantine: recovered=%v err=%v", copyName, recovered, err)
			}
			assertRuntimeStoreRecoveryFinished(t, restarted, before)
		})
	}
}

func TestRuntimeMetadataBackupOnlyFailureRecoversAcrossProcess(t *testing.T) {
	const childConfig = "PROXYSCENE_TEST_METADATA_RECOVERY_CONFIG"
	if path := os.Getenv(childConfig); path != "" {
		stubStoreTransitionCore(t)
		runtimePlanCore = func(*App, *Store) (*runtimeCorePlan, error) {
			t.Fatal("metadata recovery attempted core planning")
			return nil, nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var cfg Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			t.Fatal(err)
		}
		if recovered, err := NewApp(cfg).recoverRuntimeTransition(); err != nil || !recovered {
			t.Fatalf("separate process recovery failed: recovered=%v err=%v", recovered, err)
		}
		return
	}
	a, st := runtimeStoreCASFixture(t)
	st.RuntimeConfig = nil
	st.SceneEnabled[SceneGlobal] = true
	if err := a.saveStore(st); err != nil {
		t.Fatal(err)
	}
	before := cloneStore(st)
	a.cfg.GlobalHTTPPort = 28091
	a.cfg.runtimeOverrides.GlobalHTTPPort = true
	failure := errors.New("main commit remains unavailable")
	runtimeWriteStoreCAS = func(path string, evidence runtimeStoreEvidence, data []byte) error {
		if path == a.cfg.StorePath() {
			return failure
		}
		return writeRuntimeStoreCAS(path, evidence, data)
	}
	if err := a.commitStoreMutation(st, func(candidate *Store) error {
		candidate.Nodes[0].Name = "candidate"
		return nil
	}, storeRuntimeSyncNone); !errors.Is(err, failure) {
		t.Fatalf("main failure lost: %v", err)
	}
	record, err := a.readRuntimeTransition()
	if err != nil {
		t.Fatal(err)
	}
	if record.Resources != nil || record.Core != nil || len(record.Steps) != 0 || record.Before.RuntimeConfig != nil || record.Candidate.RuntimeConfig != nil || record.Compensation == nil || record.Compensation.RuntimeConfig != nil {
		t.Fatalf("metadata recovery invented runtime configuration or lost compensation: %+v", record)
	}
	main, err := a.loadStoreRaw()
	if err != nil || !reflect.DeepEqual(main, before) {
		t.Fatalf("backup-only failure changed main: err=%v", err)
	}
	backup, err := a.loadStoreBackup()
	if err != nil || backup.Generation <= before.Generation+1 || backup.Nodes[0].Name != before.Nodes[0].Name || backup.RuntimeConfig != nil {
		t.Fatalf("failed compensation did not persist a distinct backup generation: err=%v", err)
	}
	raw, err := json.Marshal(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "recovery-config.json")
	if err := os.WriteFile(cfgPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestRuntimeMetadataBackupOnlyFailureRecoversAcrossProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), childConfig+"="+cfgPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("recovery subprocess failed: %v\n%s", err, output)
	}
	assertRuntimeStoreRecoveryFinished(t, NewApp(a.cfg), before)
}

func TestRuntimeStoreCASAcceptsStoreLargerThanGlobalArtifactLimit(t *testing.T) {
	a, st := runtimeStoreCASFixture(t)
	st.Nodes = nil
	for i := 0; i < 2048; i++ {
		st.Nodes = append(st.Nodes, Node{ID: fmt.Sprintf("node-%d", i), Name: strings.Repeat("x", maxNodeNameBytes), Protocol: "trojan", RawURL: fmt.Sprintf("trojan://%s@node-%d.example:443", strings.Repeat("s", 256), i)})
	}
	st.DefaultNodeID = st.Nodes[0].ID
	if err := a.saveStore(st); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(a.cfg.StorePath())
	if err != nil || int64(len(before)) <= maxGlobalProxyArtifactBytes {
		t.Fatalf("fixture is not larger than ordinary Global artifacts: size=%d err=%v", len(before), err)
	}
	if err := a.commitStoreMutation(st, func(candidate *Store) error {
		candidate.Nodes[0].Name = "renamed"
		return nil
	}, storeRuntimeSyncNone); err != nil {
		t.Fatalf("valid Store exceeded wrong artifact limit: %v", err)
	}
	loaded, err := a.loadStoreRaw()
	if err != nil || len(loaded.Nodes) != 2048 || loaded.Nodes[0].Name != "renamed" {
		t.Fatalf("large Store commit was incomplete: %v", err)
	}
	assertRuntimeStoreRecoveryFinished(t, a, st)
}

func TestRuntimeStoreCommittedBackupConflictPreservesReceipt(t *testing.T) {
	for _, conflictAt := range []string{"persist-success", "new-process-recovery"} {
		t.Run(conflictAt, func(t *testing.T) {
			a, st := runtimeStoreCASFixture(t)
			oldPersist := runtimePersistStore
			t.Cleanup(func() { runtimePersistStore = oldPersist })
			commitError := errors.New("error after main commit")
			var external []byte
			writeExternalBackup := func(committed *Store) {
				t.Helper()
				administrator := cloneStore(committed)
				administrator.Generation += 10
				administrator.Nodes[0].Name = "administrator backup"
				var err error
				external, err = encodedRuntimeStore(administrator)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(a.cfg.StoreBackupPath(), external, 0600); err != nil {
					t.Fatal(err)
				}
			}
			runtimePersistStore = func(app *App, candidate *Store) error {
				if err := oldPersist(app, candidate); err != nil {
					return err
				}
				if conflictAt == "persist-success" {
					writeExternalBackup(candidate)
					return nil
				}
				return commitError
			}
			err := a.commitStoreMutation(st, func(candidate *Store) error {
				candidate.Nodes[0].Name = "committed"
				return nil
			}, storeRuntimeSyncNone)
			if err == nil {
				t.Fatal("forward commit silently ignored concurrent backup replacement")
			}
			main, err := a.loadStoreRaw()
			if err != nil || main.Nodes[0].Name != "committed" {
				t.Fatalf("main commit was not retained: %+v %v", main, err)
			}
			mainBytes, err := os.ReadFile(a.cfg.StorePath())
			if err != nil {
				t.Fatal(err)
			}
			if conflictAt == "new-process-recovery" {
				writeExternalBackup(main)
			}
			if pending, err := a.hasRuntimeTransition(); err != nil || !pending {
				t.Fatalf("committed backup conflict lost receipt: pending=%v err=%v", pending, err)
			}
			restarted := NewApp(a.cfg)
			if recovered, err := restarted.recoverRuntimeTransition(); err == nil || !recovered {
				t.Fatalf("recovery silently finalized divergent backup: recovered=%v err=%v", recovered, err)
			}
			currentMain, mainErr := os.ReadFile(a.cfg.StorePath())
			currentBackup, backupErr := os.ReadFile(a.cfg.StoreBackupPath())
			if mainErr != nil || backupErr != nil || !bytes.Equal(currentMain, mainBytes) || !bytes.Equal(currentBackup, external) {
				t.Fatal("conflict recovery overwrote committed main or administrator backup")
			}
			if pending, err := restarted.hasRuntimeTransition(); err != nil || !pending {
				t.Fatalf("conflict recovery removed receipt: pending=%v err=%v", pending, err)
			}
			// An explicit repair of the backup resolves the conflict without
			// changing the committed main or reapplying any runtime resources.
			if err := os.WriteFile(a.cfg.StoreBackupPath(), mainBytes, 0600); err != nil {
				t.Fatal(err)
			}
			if recovered, err := restarted.recoverRuntimeTransition(); err != nil || !recovered {
				t.Fatalf("repaired committed transaction did not finish: recovered=%v err=%v", recovered, err)
			}
			assertRuntimeStoreRecoveryFinished(t, restarted, main)
		})
	}
}

func TestRuntimeCompensationFinishesExistingMainWithoutNewGeneration(t *testing.T) {
	for _, backupState := range []string{"candidate", "unknown"} {
		t.Run(backupState, func(t *testing.T) {
			a, st := runtimeStoreCASFixture(t)
			before := cloneStore(st)
			var candidateBackup []byte
			mainWrites := 0
			runtimeWriteStoreCAS = func(path string, evidence runtimeStoreEvidence, data []byte) error {
				if path != a.cfg.StorePath() {
					return writeRuntimeStoreCAS(path, evidence, data)
				}
				mainWrites++
				if mainWrites == 1 {
					var err error
					candidateBackup, err = os.ReadFile(a.cfg.StoreBackupPath())
					if err != nil {
						return err
					}
					return errors.New("forward main write rejected")
				}
				if err := writeRuntimeStoreCAS(path, evidence, data); err != nil {
					return err
				}
				return errors.New("interrupted after compensation main commit")
			}
			if err := a.commitStoreMutation(st, func(candidate *Store) error {
				candidate.Nodes[0].Name = "failed candidate"
				return nil
			}, storeRuntimeSyncNone); err == nil || mainWrites != 2 {
				t.Fatalf("fixture did not reach committed compensation: calls=%d err=%v", mainWrites, err)
			}
			record, err := a.readRuntimeTransition()
			if err != nil || record.Compensation == nil {
				t.Fatalf("compensation receipt missing: %v", err)
			}
			main, err := a.loadStoreRaw()
			if err != nil || main.Generation != record.Compensation.Generation || main.Generation <= before.Generation+1 || main.Nodes[0].Name != before.Nodes[0].Name {
				t.Fatalf("compensation main not committed: %+v %v", main, err)
			}
			backupBytes := candidateBackup
			if backupState == "unknown" {
				external := cloneStore(main)
				external.Generation += 10
				external.Nodes[0].Name = "external compensation backup"
				backupBytes, err = encodedRuntimeStore(external)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(a.cfg.StoreBackupPath(), backupBytes, 0600); err != nil {
				t.Fatal(err)
			}
			backupWrites := 0
			runtimeWriteStoreCAS = func(path string, evidence runtimeStoreEvidence, data []byte) error {
				if path == a.cfg.StorePath() {
					t.Fatal("recovery rewrote already committed compensation main")
				}
				backupWrites++
				return writeRuntimeStoreCAS(path, evidence, data)
			}
			restarted := NewApp(a.cfg)
			recovered, err := restarted.recoverRuntimeTransition()
			if backupState == "unknown" {
				if err == nil || !recovered || backupWrites != 0 {
					t.Fatalf("unknown backup was accepted or changed: recovered=%v writes=%d err=%v", recovered, backupWrites, err)
				}
				current, err := os.ReadFile(a.cfg.StoreBackupPath())
				if err != nil || !bytes.Equal(current, backupBytes) {
					t.Fatal("unknown backup overwritten")
				}
				if pending, err := restarted.hasRuntimeTransition(); err != nil || !pending {
					t.Fatalf("unknown backup conflict lost receipt: pending=%v err=%v", pending, err)
				}
				return
			}
			if err != nil || !recovered || backupWrites != 1 {
				t.Fatalf("known candidate backup was not repaired: recovered=%v writes=%d err=%v", recovered, backupWrites, err)
			}
			assertRuntimeStoreRecoveryFinished(t, restarted, main)
			current, err := restarted.loadStoreRaw()
			if err != nil || current.Generation != main.Generation {
				t.Fatalf("replayed compensation allocated another generation: %+v %v", current, err)
			}
		})
	}
}
