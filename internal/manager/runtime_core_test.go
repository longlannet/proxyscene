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
	"syscall"
	"testing"
	"time"
)

type coreRuntimeFixture struct {
	t        *testing.T
	app      *App
	store    *Store
	unit     string
	identity localUserIdentity
	service  coreServiceState
	commands [][]string
	checked  []string
	restarts int
}

func newCoreRuntimeFixture(t *testing.T) *coreRuntimeFixture {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("trusted core ownership requires root; every mutation is in temporary fixtures")
	}
	a := testApp(t)
	a.cfg.XrayServiceUser = "root"
	f := &coreRuntimeFixture{t: t, app: a, unit: filepath.Join(t.TempDir(), "proxyscene.service")}
	f.identity = localUserIdentity{Name: "root", UID: 0, GID: 0, UIDText: "0", GIDText: "0", Home: t.TempDir()}
	f.service = coreServiceState{Load: "loaded", Active: "active", Sub: "running", Enabled: "enabled", Invocation: strings.Repeat("a", 32), NeedReload: "no", PID: 123}
	oldUnit, oldCheck, oldIdentity, oldRead := coreUnitPath, coreCheckConfig, coreLookupIdentity, coreReadService
	oldVerify := coreVerifyExecution
	oldCapture := coreCaptureBeforeProcess
	oldRun, oldOutput, oldCAS := systemctlRun, systemctlOutput, globalProxyCASAfterQuarantine
	t.Cleanup(func() {
		coreUnitPath, coreCheckConfig, coreLookupIdentity, coreReadService = oldUnit, oldCheck, oldIdentity, oldRead
		coreVerifyExecution = oldVerify
		coreCaptureBeforeProcess = oldCapture
		systemctlRun, systemctlOutput, globalProxyCASAfterQuarantine = oldRun, oldOutput, oldCAS
	})
	coreUnitPath = func(Config) string { return f.unit }
	coreVerifyExecution = func(app *App, _ coreServiceState, _ []byte, _ localUserIdentity, _ string, _ bool) error {
		if app.cfg.CoreDir != a.cfg.CoreDir {
			t.Fatal("core execution verification escaped fixture")
		}
		return nil
	}
	coreCaptureBeforeProcess = func(app *App, state coreServiceState, _ []byte, _ localUserIdentity) (*coreProcessIdentity, error) {
		if app.cfg.CoreDir != a.cfg.CoreDir {
			t.Fatal("core process capture escaped fixture")
		}
		if state.Active != "active" {
			return nil, nil
		}
		metadata, err := readCoreMetadata(app.cfg.XrayBin())
		if err != nil {
			return nil, err
		}
		digest, err := coreBinaryDigest(app.cfg.XrayBin())
		if err != nil {
			return nil, err
		}
		return &coreProcessIdentity{Device: metadata.Device, Inode: metadata.Inode, Digest: digest, Start: "12345"}, nil
	}
	initialRootIdentity := f.identity
	coreLookupIdentity = func(name string) (localUserIdentity, error) {
		if name == f.identity.Name {
			return f.identity, nil
		}
		if name == initialRootIdentity.Name {
			return initialRootIdentity, nil
		}
		return localUserIdentity{}, fmt.Errorf("unexpected fixture user %s", name)
	}
	coreReadService = func(app *App) (coreServiceState, error) {
		if app.cfg.CoreDir != a.cfg.CoreDir {
			t.Fatal("service read escaped fixture")
		}
		return f.service, nil
	}
	coreCheckConfig = func(app *App, path string) error {
		if app.cfg.CoreDir != a.cfg.CoreDir || path == a.cfg.XrayConfig() || filepath.Dir(path) == a.cfg.CoreDir {
			t.Fatalf("preflight used live configuration path: %s", path)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		dirInfo, err := os.Lstat(filepath.Dir(path))
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || dirInfo.Mode().Perm() != 0700 {
			t.Fatalf("preflight staging permissions are not private: file=%v dir=%v", info.Mode(), dirInfo.Mode())
		}
		f.checked = append(f.checked, path)
		return nil
	}
	systemctlRun = func(_ string, args ...string) error {
		f.commands = append(f.commands, append([]string(nil), args...))
		switch args[0] {
		case "daemon-reload":
			f.service.NeedReload = "no"
		case "reset-failed":
		case "restart":
			f.restarts++
			f.service.Load = "loaded"
			f.service.Active = "active"
			f.service.Sub = "running"
			f.service.PID = 123
			f.service.Invocation = fmt.Sprintf("%032x", f.restarts)
		case "stop":
			f.service.Active = "inactive"
			f.service.Sub = "dead"
			f.service.PID = 0
			f.service.Invocation = ""
		case "enable":
			f.service.Enabled = "enabled"
		case "disable":
			f.service.Enabled = "disabled"
		default:
			t.Fatalf("unexpected core service operation %v", args)
		}
		return nil
	}
	systemctlOutput = func(_ string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "show" {
			return f.service.Active + "\n", nil
		}
		t.Fatalf("unexpected systemctl output request %v", args)
		return "", nil
	}
	globalProxyCASAfterQuarantine = func(string) {}
	binary, err := os.ReadFile("/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.cfg.XrayBin(), binary, 0755); err != nil {
		t.Fatal(err)
	}
	f.store = newStore()
	f.store.RuntimeConfig = a.cfg.runtimeConfig()
	f.store.Nodes = []Node{{ID: "fixture", Name: "fixture", Protocol: "trojan", RawURL: "trojan://secret@fixture.example:443"}}
	f.store.DefaultNodeID = "fixture"
	f.store.SceneEnabled[SceneGlobal] = true
	config, err := a.renderXrayConfig(f.store)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.cfg.XrayConfig(), config, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.unit, a.xrayUnitContent(), 0644); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *coreRuntimeFixture) plan() *runtimeCorePlan {
	f.t.Helper()
	p, err := f.app.planCoreRuntime(f.store)
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}
func (f *coreRuntimeFixture) writeReceipt(p *runtimeCorePlan, edit func(*coreLoadedReceipt)) {
	f.t.Helper()
	r := coreLoadedReceipt{Version: 1, Config: contentDigest(p.AfterConfig.Content), Unit: contentDigest(p.AfterUnit.Content), Binary: p.BinaryDigest, Invocation: f.service.Invocation}
	if edit != nil {
		edit(&r)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.app.coreReceiptPath(), raw, 0600); err != nil {
		f.t.Fatal(err)
	}
}
func (f *coreRuntimeFixture) file(path string) coreFileState {
	f.t.Helper()
	s, err := readCoreFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}
func (f *coreRuntimeFixture) changedPlan() *runtimeCorePlan {
	f.t.Helper()
	if err := os.WriteFile(f.app.cfg.XrayConfig(), []byte("previous config\n"), 0600); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.unit, []byte(managedSystemdUnitHeader+"[Service]\nExecStart=/previous\n"), 0644); err != nil {
		f.t.Fatal(err)
	}
	return f.plan()
}
func writeCoreTestState(t *testing.T, path string, state coreFileState) {
	t.Helper()
	if !state.Present {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return
	}
	if err := os.WriteFile(path, state.Content, os.FileMode(state.Mode)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, os.FileMode(state.Mode)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, int(state.UID), int(state.GID)); err != nil {
		t.Fatal(err)
	}
}

func TestCorePreflightChecksPrivateTempWithoutChangingLiveState(t *testing.T) {
	f := newCoreRuntimeFixture(t)
	beforeConfig, beforeUnit, beforeBinary := f.file(f.app.cfg.XrayConfig()), f.file(f.unit), f.file(f.app.cfg.XrayBin())
	beforeDir, err := readCoreMetadata(f.app.cfg.CoreDir)
	if err != nil {
		t.Fatal(err)
	}
	check := coreCheckConfig
	failure := errors.New("candidate configuration rejected")
	coreCheckConfig = func(a *App, path string) error {
		if err := check(a, path); err != nil {
			return err
		}
		return failure
	}
	if _, err := f.app.planCoreRuntime(f.store); !errors.Is(err, failure) {
		t.Fatalf("preflight accepted rejected config: %v", err)
	}
	if !coreFilesEqual(f.file(f.app.cfg.XrayConfig()), beforeConfig) || !coreFilesEqual(f.file(f.unit), beforeUnit) || !coreFilesEqual(f.file(f.app.cfg.XrayBin()), beforeBinary) {
		t.Fatal("failed preflight changed live file")
	}
	afterDir, err := readCoreMetadata(f.app.cfg.CoreDir)
	if err != nil || afterDir != beforeDir {
		t.Fatalf("failed preflight changed core metadata: %+v %v", afterDir, err)
	}
	if len(f.commands) != 0 || len(f.checked) != 1 {
		t.Fatalf("unexpected preflight effects: commands=%v paths=%v", f.commands, f.checked)
	}
	if _, err := os.Stat(filepath.Dir(f.checked[0])); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private config staging survived failure: %v", err)
	}
	for _, path := range []string{f.app.coreReceiptPath(), f.app.runtimeTransitionPath()} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed preflight created %s: %v", path, err)
		}
	}
}

func TestCoreNoopRequiresCurrentLoadedReceipt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(*coreRuntimeFixture, *runtimeCorePlan)
	}{
		{"missing receipt", func(f *coreRuntimeFixture, _ *runtimeCorePlan) {
			if err := os.Remove(f.app.coreReceiptPath()); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"stale config receipt", func(f *coreRuntimeFixture, p *runtimeCorePlan) {
			f.writeReceipt(p, func(r *coreLoadedReceipt) { r.Config = strings.Repeat("0", 64) })
		}},
		{"stale unit receipt", func(f *coreRuntimeFixture, p *runtimeCorePlan) {
			f.writeReceipt(p, func(r *coreLoadedReceipt) { r.Unit = strings.Repeat("0", 64) })
		}},
		{"stale binary receipt", func(f *coreRuntimeFixture, p *runtimeCorePlan) {
			f.writeReceipt(p, func(r *coreLoadedReceipt) { r.Binary = strings.Repeat("0", 64) })
		}},
		{"stale invocation", func(f *coreRuntimeFixture, _ *runtimeCorePlan) { f.service.Invocation = strings.Repeat("b", 32) }},
		{"daemon reload required", func(f *coreRuntimeFixture, _ *runtimeCorePlan) { f.service.NeedReload = "yes" }},
		{"service stopped", func(f *coreRuntimeFixture, _ *runtimeCorePlan) {
			f.service.Active = "inactive"
			f.service.Sub = "dead"
			f.service.PID = 0
			f.service.Invocation = ""
		}},
		{"service disabled", func(f *coreRuntimeFixture, _ *runtimeCorePlan) { f.service.Enabled = "disabled" }},
		{"unit not loaded", func(f *coreRuntimeFixture, _ *runtimeCorePlan) { f.service.Load = "not-found" }},
		{"live configuration drift", func(f *coreRuntimeFixture, _ *runtimeCorePlan) {
			if err := os.WriteFile(f.app.cfg.XrayConfig(), []byte("drift\n"), 0600); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"live unit drift", func(f *coreRuntimeFixture, _ *runtimeCorePlan) {
			if err := os.WriteFile(f.unit, []byte(managedSystemdUnitHeader+"[Service]\nExecStart=/different\n"), 0644); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"live binary drift", func(f *coreRuntimeFixture, _ *runtimeCorePlan) {
			data, err := os.ReadFile(f.app.cfg.XrayBin())
			if err != nil {
				f.t.Fatal(err)
			}
			if err := os.WriteFile(f.app.cfg.XrayBin(), append(data, 0), 0755); err != nil {
				f.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCoreRuntimeFixture(t)
			p := f.plan()
			if p.Noop {
				t.Fatal("no receipt accepted as loaded")
			}
			f.writeReceipt(p, nil)
			if !f.plan().Noop {
				t.Fatal("matching loaded receipt did not allow no-op")
			}
			tc.alter(f, p)
			if f.plan().Noop {
				t.Fatal("stale running/configuration evidence allowed no-op")
			}
			if len(f.commands) != 0 {
				t.Fatalf("planning invoked runtime: %v", f.commands)
			}
		})
	}
}

func TestCoreApplyNoopDoesNotRestartVerifiedInvocation(t *testing.T) {
	f := newCoreRuntimeFixture(t)
	p := f.plan()
	f.writeReceipt(p, nil)
	p = f.plan()
	before := f.service
	if !p.Noop {
		t.Fatal("matching fixture not a no-op")
	}
	if err := f.app.applyCorePlan(p); err != nil {
		t.Fatal(err)
	}
	if f.service != before || len(f.commands) != 0 {
		t.Fatalf("no-op touched service: before=%+v after=%+v commands=%v", before, f.service, f.commands)
	}
}

func TestCorePlanRevalidationRejectsDriftBeforeMutation(t *testing.T) {
	for _, kind := range []string{"config", "binary", "identity", "invocation"} {
		t.Run(kind, func(t *testing.T) {
			f := newCoreRuntimeFixture(t)
			p := f.changedPlan()
			switch kind {
			case "config":
				if err := os.WriteFile(f.app.cfg.XrayConfig(), []byte("operator edit\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "binary":
				data, err := os.ReadFile(f.app.cfg.XrayBin())
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f.app.cfg.XrayBin(), append(data, 0), 0755); err != nil {
					t.Fatal(err)
				}
			case "identity":
				f.identity.GID = 123
				f.identity.GIDText = "123"
			case "invocation":
				f.service.Invocation = strings.Repeat("b", 32)
			}
			beforeConfig, beforeUnit := f.file(f.app.cfg.XrayConfig()), f.file(f.unit)
			if err := f.app.applyCorePlan(p); err == nil {
				t.Fatal("stale plan applied")
			}
			if !coreFilesEqual(beforeConfig, f.file(f.app.cfg.XrayConfig())) || !coreFilesEqual(beforeUnit, f.file(f.unit)) || len(f.commands) != 0 {
				t.Fatal("stale plan modified runtime before rejection")
			}
		})
	}
}

func TestCoreCandidateCASFailurePreservesConcurrentEdit(t *testing.T) {
	f := newCoreRuntimeFixture(t)
	p := f.changedPlan()
	operator := []byte("operator configuration\n")
	fired := false
	globalProxyCASAfterQuarantine = func(path string) {
		if path != f.app.cfg.XrayConfig() || fired {
			return
		}
		fired = true
		if err := os.WriteFile(path, operator, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.app.applyCorePlan(p); err == nil || !fired {
		t.Fatalf("candidate CAS race accepted: err=%v fired=%v", err, fired)
	}
	if !bytes.Equal(f.file(f.app.cfg.XrayConfig()).Content, operator) || len(f.commands) != 0 {
		t.Fatal("failed candidate overwrote external config or restarted")
	}
	checkpoints := []bool{}
	checkpoint := func() error { checkpoints = append(checkpoints, p.RestoreServicePending); return nil }
	if err := f.app.compensateCorePlan(p, checkpoint); err == nil {
		t.Fatal("compensation overwrote external config")
	}
	if !bytes.Equal(f.file(f.app.cfg.XrayConfig()).Content, operator) {
		t.Fatal("external edit was lost")
	}
	writeCoreTestState(t, f.app.cfg.XrayConfig(), p.BeforeConfig)
	if err := f.app.compensateCorePlan(p, checkpoint); err != nil {
		t.Fatal(err)
	}
	if !coreFilesEqual(f.file(f.unit), p.BeforeUnit) || !coreFilesEqual(f.file(f.app.cfg.XrayConfig()), p.BeforeConfig) || p.RestoreServicePending || f.restarts != 1 {
		t.Fatalf("retry did not restore exact core state: pending=%v restarts=%d", p.RestoreServicePending, f.restarts)
	}
	if !reflect.DeepEqual(checkpoints, []bool{true, false}) {
		t.Fatalf("restore checkpoints=%v", checkpoints)
	}
}

func TestCoreCompensationRetryRestartsWhenFilesAlreadyRestored(t *testing.T) {
	f := newCoreRuntimeFixture(t)
	p := f.changedPlan()
	if err := f.app.applyCorePlan(p); err != nil {
		t.Fatal(err)
	}
	fired := false
	globalProxyCASAfterQuarantine = func(path string) {
		if path != f.unit || fired {
			return
		}
		fired = true
		if err := os.WriteFile(path, []byte("operator unit\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	checkpoints := []bool{}
	checkpoint := func() error { checkpoints = append(checkpoints, p.RestoreServicePending); return nil }
	if err := f.app.compensateCorePlan(p, checkpoint); err == nil || !fired {
		t.Fatalf("fixture did not interrupt unit restoration: %v", err)
	}
	if !coreFilesEqual(f.file(f.app.cfg.XrayConfig()), p.BeforeConfig) || !p.RestoreServicePending {
		t.Fatal("partial restoration did not retain service recovery checkpoint")
	}
	writeCoreTestState(t, f.unit, p.BeforeUnit)
	restarts := f.restarts
	if err := f.app.compensateCorePlan(p, checkpoint); err != nil {
		t.Fatal(err)
	}
	if f.restarts != restarts+1 || p.RestoreServicePending {
		t.Fatalf("files already-before suppressed pending restart: before=%d after=%d pending=%v", restarts, f.restarts, p.RestoreServicePending)
	}
	if !reflect.DeepEqual(checkpoints, []bool{true, false}) {
		t.Fatalf("service checkpoint progression=%v", checkpoints)
	}
}

func TestCoreCompensationCheckpointFailurePrecedesFileRestoration(t *testing.T) {
	f := newCoreRuntimeFixture(t)
	p := f.changedPlan()
	if err := f.app.applyCorePlan(p); err != nil {
		t.Fatal(err)
	}
	beforeCommands := len(f.commands)
	failure := errors.New("checkpoint persistence failed")
	if err := f.app.compensateCorePlan(p, func() error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("checkpoint failure lost: %v", err)
	}
	if !coreFilesEqual(f.file(f.unit), p.AfterUnit) || !coreFilesEqual(f.file(f.app.cfg.XrayConfig()), p.AfterConfig) || len(f.commands) != beforeCommands {
		t.Fatal("checkpoint failure modified core files or services")
	}
	called := false
	if err := f.app.compensateCorePlan(p, func() error { called = true; return failure }); !errors.Is(err, failure) || !called {
		t.Fatalf("same-process retry skipped durable checkpoint: called=%v err=%v", called, err)
	}
	if !coreFilesEqual(f.file(f.unit), p.AfterUnit) || !coreFilesEqual(f.file(f.app.cfg.XrayConfig()), p.AfterConfig) {
		t.Fatal("retry without checkpoint modified core files")
	}
}

func TestCoreCompensationRequiresVerifiedServiceResult(t *testing.T) {
	for _, operation := range []string{"restart", "stop", "enable", "disable", "daemon-reload"} {
		t.Run(operation, func(t *testing.T) {
			f := newCoreRuntimeFixture(t)
			if operation == "stop" {
				f.service.Active, f.service.Sub = "inactive", "dead"
				f.service.PID, f.service.Invocation = 0, ""
			}
			if operation == "disable" {
				f.service.Enabled = "disabled"
			}
			p := f.changedPlan()
			if err := f.app.applyCorePlan(p); err != nil {
				t.Fatal(err)
			}
			if operation == "enable" {
				f.service.Enabled = "disabled"
			}
			if operation == "daemon-reload" {
				f.service.NeedReload = "yes"
			}
			run := systemctlRun
			called := false
			systemctlRun = func(label string, args ...string) error {
				if args[0] != operation {
					return run(label, args...)
				}
				called = true
				if operation == "restart" {
					if err := run(label, args...); err != nil {
						return err
					}
					// ActiveState alone still looks successful, while full service
					// evidence shows that the restored process is not running.
					f.service.Sub, f.service.PID = "exited", 0
				}
				return nil
			}
			checkpoints := []bool{}
			checkpoint := func() error {
				checkpoints = append(checkpoints, p.RestoreServicePending)
				return nil
			}
			if err := f.app.compensateCorePlan(p, checkpoint); err == nil || !called {
				t.Fatalf("unverified %s accepted: called=%v err=%v", operation, called, err)
			}
			if !p.RestoreServicePending || !reflect.DeepEqual(checkpoints, []bool{true}) {
				t.Fatalf("failed service verification discarded recovery state: pending=%v checkpoints=%v", p.RestoreServicePending, checkpoints)
			}
			systemctlRun = run
			if err := f.app.compensateCorePlan(p, checkpoint); err != nil {
				t.Fatalf("service restoration could not resume after %s: %v", operation, err)
			}
			if p.RestoreServicePending || !reflect.DeepEqual(checkpoints, []bool{true, false}) {
				t.Fatalf("successful retry did not finish recovery state: pending=%v checkpoints=%v", p.RestoreServicePending, checkpoints)
			}
		})
	}
}

func TestCoreCompensationCheckpointsReloadWithoutFileChanges(t *testing.T) {
	f := newCoreRuntimeFixture(t)
	p := f.plan()
	f.service.NeedReload = "yes"
	run := systemctlRun
	systemctlRun = func(label string, args ...string) error {
		if args[0] == "daemon-reload" {
			return nil // The command reports success but leaves reload pending.
		}
		return run(label, args...)
	}
	checkpoints := []bool{}
	checkpoint := func() error {
		checkpoints = append(checkpoints, p.RestoreServicePending)
		return nil
	}
	if err := f.app.compensateCorePlan(p, checkpoint); err == nil {
		t.Fatal("reload without file restoration skipped final service verification")
	}
	if !p.RestoreServicePending || !reflect.DeepEqual(checkpoints, []bool{true}) {
		t.Fatalf("reload did not preserve durable service recovery: pending=%v checkpoints=%v", p.RestoreServicePending, checkpoints)
	}
	systemctlRun = run
	if err := f.app.compensateCorePlan(p, checkpoint); err != nil {
		t.Fatal(err)
	}
	if p.RestoreServicePending || !reflect.DeepEqual(checkpoints, []bool{true, false}) {
		t.Fatalf("reload retry did not complete recovery: pending=%v checkpoints=%v", p.RestoreServicePending, checkpoints)
	}
}

func TestCoreMetadataCompensationHandlesInterruptedChown(t *testing.T) {
	f := newCoreRuntimeFixture(t)
	f.app.cfg.XrayServiceUser = "fixture-xray"
	f.identity.Name = "fixture-xray"
	f.identity.UID = 54320
	f.identity.GID = 54321
	f.identity.UIDText = "54320"
	f.identity.GIDText = "54321"
	p := f.plan()
	for _, path := range []string{f.app.cfg.CoreDir, f.app.cfg.XrayBin()} {
		if err := os.Chown(path, 0, 54321); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.app.compensateCorePlan(p, func() error { return nil }); err != nil {
		t.Fatalf("known partial chown could not be restored: %v", err)
	}
	for path, want := range map[string]coreMetadata{f.app.cfg.CoreDir: p.BeforeDir, f.app.cfg.XrayBin(): p.BeforeBinary} {
		got, err := readCoreMetadata(path)
		if err != nil || got != want {
			t.Fatalf("metadata restore %s: got=%+v want=%+v err=%v", path, got, want, err)
		}
	}
	if len(f.commands) != 0 {
		t.Fatalf("metadata-only partial application touched untouched service: %v", f.commands)
	}
}

func TestCoreMetadataCompensationRejectsUnrelatedPermissions(t *testing.T) {
	f := newCoreRuntimeFixture(t)
	f.app.cfg.XrayServiceUser = "fixture-xray"
	f.identity.Name = "fixture-xray"
	f.identity.UID = 54320
	f.identity.GID = 54321
	f.identity.UIDText = "54320"
	f.identity.GIDText = "54321"
	p := f.plan()
	if err := os.Chmod(f.app.cfg.XrayBin(), 0711); err != nil {
		t.Fatal(err)
	}
	before, err := readCoreMetadata(f.app.cfg.XrayBin())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.app.compensateCorePlan(p, func() error { return nil }); err == nil {
		t.Fatal("unrelated administrator permissions were overwritten")
	}
	after, err := readCoreMetadata(f.app.cfg.XrayBin())
	if err != nil || after != before {
		t.Fatalf("external permissions changed: %+v %v", after, err)
	}
	if len(f.commands) != 0 {
		t.Fatal("conflicting metadata triggered service operations")
	}
}

func TestCoreBinaryDigestRejectsFIFOWithoutBlocking(t *testing.T) {
	const env = "PROXYSCENE_TEST_CORE_DIGEST_FIFO"
	if path := os.Getenv(env); path != "" {
		if _, err := coreBinaryDigest(path); err == nil {
			t.Fatal("FIFO accepted as core binary")
		}
		return
	}
	path := filepath.Join(t.TempDir(), "xray")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestCoreBinaryDigestRejectsFIFOWithoutBlocking$", "-test.count=1")
	cmd.Env = append(os.Environ(), env+"="+path)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("core binary hashing blocked on FIFO: %v", ctx.Err())
	}
	if err != nil {
		t.Fatalf("FIFO safety subprocess failed: %v\n%s", err, output)
	}
}
