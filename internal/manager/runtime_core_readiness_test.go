package manager

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func coreReadinessClock(t *testing.T, advance func()) *int {
	t.Helper()
	oldNow, oldSleep := coreRestartNow, coreRestartSleep
	t.Cleanup(func() { coreRestartNow, coreRestartSleep = oldNow, oldSleep })
	now := time.Unix(1, 0)
	calls := 0
	coreRestartNow = func() time.Time { return now }
	coreRestartSleep = func(d time.Duration) {
		calls++
		now = now.Add(d)
		if advance != nil {
			advance()
		}
	}
	return &calls
}

func coreReadinessFixture(t *testing.T) *coreExecutionFixture {
	t.Helper()
	f := newCoreExecutionFixture(t)
	coreVerifyExecution = verifyCoreExecution
	coreCaptureBeforeProcess = captureCoreBeforeProcess
	f.simulateCoreRestarts(t)
	return f
}

func coreReadyArgv(f *coreExecutionFixture) string {
	return strings.Join([]string{f.core.app.cfg.XrayBin(), "run", "-config", f.core.app.cfg.XrayConfig()}, "\x00") + "\x00"
}

func TestCoreRestartWaitsForExecBeforeRecordingLoaded(t *testing.T) {
	f := coreReadinessFixture(t)
	run := systemctlRun
	systemctlRun = func(label string, args ...string) error {
		if err := run(label, args...); err != nil {
			return err
		}
		if args[0] == "restart" {
			f.write(t, "cmdline", "/usr/lib/systemd/systemd-executor\x00")
		}
		return nil
	}
	waits := coreReadinessClock(t, func() { f.write(t, "cmdline", coreReadyArgv(f)) })
	if err := f.core.app.applyCorePlan(f.plan); err != nil {
		t.Fatal(err)
	}
	if *waits != 1 || f.core.restarts != 1 {
		t.Fatalf("readiness did not wait for the same start: waits=%d restarts=%d", *waits, f.core.restarts)
	}
	if !f.core.app.coreLoadedMatches(f.plan, f.core.service) {
		t.Fatal("fully verified process was not recorded as loaded")
	}
}

func TestCoreCompensationWaitsForRestoredProcessExec(t *testing.T) {
	f := coreReadinessFixture(t)
	before := f.core.file(f.core.app.cfg.XrayConfig())
	f.core.app.cfg.GlobalHTTPPort++
	p := f.core.plan()
	if err := f.core.app.applyCorePlan(p); err != nil {
		t.Fatal(err)
	}
	run := systemctlRun
	systemctlRun = func(label string, args ...string) error {
		if err := run(label, args...); err != nil {
			return err
		}
		if args[0] == "restart" {
			f.write(t, "cmdline", "")
		}
		return nil
	}
	waits := coreReadinessClock(t, func() { f.write(t, "cmdline", coreReadyArgv(f)) })
	if err := f.core.app.compensateCorePlan(p, nil); err != nil {
		t.Fatal(err)
	}
	if *waits != 1 || f.core.restarts != 2 || p.RestoreServicePending {
		t.Fatalf("compensation did not confirm its own start: waits=%d restarts=%d pending=%v", *waits, f.core.restarts, p.RestoreServicePending)
	}
	if !coreFilesEqual(before, f.core.file(f.core.app.cfg.XrayConfig())) {
		t.Fatal("compensation did not restore the old configuration")
	}
}

func TestCoreRestartReadinessRejectsUnitChangeWithoutWaiting(t *testing.T) {
	f := coreReadinessFixture(t)
	run := systemctlRun
	systemctlRun = func(label string, args ...string) error {
		if err := run(label, args...); err != nil {
			return err
		}
		if args[0] == "restart" {
			f.properties["User"] = "nobody"
		}
		return nil
	}
	waits := coreReadinessClock(t, nil)
	if err := f.core.app.applyCorePlan(f.plan); err == nil || !strings.Contains(err.Error(), "有效ExecStart") {
		t.Fatalf("unit override was not rejected: %v", err)
	}
	if *waits != 0 {
		t.Fatal("unit override used the exec readiness grace period")
	}
	if _, err := os.Stat(f.core.app.coreReceiptPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed start was recorded as loaded: %v", err)
	}
}

func TestCoreRestartReadinessCannotFollowAnotherInvocation(t *testing.T) {
	f := coreReadinessFixture(t)
	run := systemctlRun
	systemctlRun = func(label string, args ...string) error {
		if err := run(label, args...); err != nil {
			return err
		}
		if args[0] == "restart" {
			f.write(t, "cmdline", "/usr/lib/systemd/systemd-executor\x00")
		}
		return nil
	}
	waits := coreReadinessClock(t, func() {
		f.core.service.Invocation = strings.Repeat("b", 32)
		f.properties["InvocationID"] = f.core.service.Invocation
		f.write(t, "cmdline", coreReadyArgv(f))
	})
	if err := f.core.app.applyCorePlan(f.plan); err == nil || !strings.Contains(err.Error(), "运行代际变化") {
		t.Fatalf("replacement invocation was accepted: %v", err)
	}
	if *waits != 1 {
		t.Fatalf("unexpected waits: %d", *waits)
	}
	if _, err := os.Stat(f.core.app.coreReceiptPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement start was recorded as loaded: %v", err)
	}
}

func TestCoreRestartTimeoutRetainsRecoveryUntilExactProcessReturns(t *testing.T) {
	f := coreReadinessFixture(t)
	profile, apt := withGlobalProxyTestPaths(t, f.core.app)
	if err := f.core.app.saveStore(f.core.store); err != nil {
		t.Fatal(err)
	}
	beforeStore, err := os.ReadFile(f.core.app.cfg.StorePath())
	if err != nil {
		t.Fatal(err)
	}
	run := systemctlRun
	systemctlRun = func(label string, args ...string) error {
		if err := run(label, args...); err != nil {
			return err
		}
		if args[0] == "restart" {
			f.write(t, "cmdline", "/permanently-wrong-program\x00")
		}
		return nil
	}
	waits := coreReadinessClock(t, nil)
	err = f.core.app.commitStoreMutation(f.core.store, func(st *Store) error {
		st.Nodes[0].RawURL = "trojan://changed@candidate.example:443"
		return nil
	}, storeRuntimeSyncGlobal)
	if err == nil || !strings.Contains(err.Error(), "2s 内未通过精确进程验证") {
		t.Fatalf("persistent wrong argv was accepted: %v", err)
	}
	if *waits != 2*int(coreRestartReadyTimeout/coreRestartPollInterval) {
		t.Fatalf("apply and compensation did not stop at the bound: waits=%d", *waits)
	}
	record, err := f.core.app.readRuntimeTransition()
	if err != nil || record.Phase != "compensating" || record.Core == nil || !record.Core.RestoreServicePending {
		t.Fatalf("failed compensation lost its durable progress: %+v %v", record, err)
	}
	if _, err := os.Stat(f.core.app.coreReceiptPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unverified process got a loaded receipt: %v", err)
	}
	afterStore, _ := os.ReadFile(f.core.app.cfg.StorePath())
	if !bytes.Equal(beforeStore, afterStore) {
		t.Fatal("readiness failure committed Store")
	}
	assertRuntimeNoGlobalOwnership(t, f.core.app, profile, apt)
	systemctlRun = run
	f.write(t, "cmdline", coreReadyArgv(f))
	if recovered, err := f.core.app.recoverRuntimeTransition(); !recovered || err != nil {
		t.Fatalf("exact process could not complete persisted recovery: %v %v", recovered, err)
	}
	if _, err := os.Stat(filepath.Join(f.core.app.cfg.CoreDir, "runtime-transition.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed recovery retained its record: %v", err)
	}
}

func TestCoreExistingProcessProofDoesNotUseRestartGracePeriod(t *testing.T) {
	f := coreReadinessFixture(t)
	f.core.writeReceipt(f.plan, nil)
	waits := coreReadinessClock(t, func() { f.write(t, "cmdline", coreReadyArgv(f)) })
	f.write(t, "cmdline", "/wrong-existing-process\x00")
	if f.core.app.coreLoadedMatches(f.plan, f.core.service) {
		t.Fatal("invalid old process counted as no-op")
	}
	if err := f.core.app.saveCoreLoaded(f.plan); err == nil {
		t.Fatal("direct receipt update accepted an invalid process")
	}
	if _, err := f.core.app.planCoreRuntime(f.core.store); err == nil {
		t.Fatal("preflight accepted an invalid old process")
	}
	if *waits != 0 || len(f.core.commands) != 0 {
		t.Fatal("existing-process rejection waited or performed service mutations")
	}
}
