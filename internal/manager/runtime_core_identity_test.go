package manager

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type coreExecutionFixture struct {
	core       *coreRuntimeFixture
	plan       *runtimeCorePlan
	properties map[string]string
	processDir string
}

func newCoreExecutionFixture(t *testing.T) *coreExecutionFixture {
	t.Helper()
	f := newCoreRuntimeFixture(t)
	p := f.plan()
	oldRoots, oldProc := coreUnitSearchRoots, coreProcRoot
	t.Cleanup(func() { coreUnitSearchRoots = oldRoots; coreProcRoot = oldProc })
	coreUnitSearchRoots = func() []string { return []string{filepath.Dir(f.unit)} }
	coreProcRoot = t.TempDir()
	processDir := filepath.Join(coreProcRoot, strconv.Itoa(f.service.PID))
	if err := os.Mkdir(processDir, 0700); err != nil {
		t.Fatal(err)
	}
	properties := map[string]string{"Id": f.app.cfg.SystemdService, "Names": f.app.cfg.SystemdService, "FragmentPath": f.unit, "DropInPaths": "", "User": "root", "Group": "", "Type": "simple", "DynamicUser": "no", "RootDirectory": "", "RootImage": "", "MainPID": strconv.Itoa(f.service.PID), "InvocationID": f.service.Invocation, "ControlGroup": "/system.slice/" + f.app.cfg.SystemdService, "NeedDaemonReload": "no"}
	properties["ExecStart"] = "{ path=" + f.app.cfg.XrayBin() + " ; argv[]=" + f.app.cfg.XrayBin() + " run -config " + f.app.cfg.XrayConfig() + " ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=123 ; code=(null) ; status=0/0 }"
	systemctlOutput = func(_ string, args ...string) (string, error) {
		if len(args) == 0 || args[0] != "show" {
			t.Fatal("non-read operation")
		}
		if containsString(args, "--value") {
			return f.service.Active + "\n", nil
		}
		keys := make([]string, 0, len(properties))
		for key := range properties {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var lines []string
		for _, key := range keys {
			lines = append(lines, key+"="+properties[key])
		}
		return strings.Join(lines, "\n") + "\n", nil
	}
	f.commands = nil
	fixture := &coreExecutionFixture{core: f, plan: p, properties: properties, processDir: processDir}
	fixture.write(t, "cmdline", strings.Join([]string{f.app.cfg.XrayBin(), "run", "-config", f.app.cfg.XrayConfig()}, "\x00")+"\x00")
	fixture.write(t, "status", "Name:\txray\nUid:\t0\t0\t0\t0\nGid:\t0\t0\t0\t0\n")
	fixture.write(t, "stat", fmt.Sprintf("%d (xray) S%s 12345", f.service.PID, strings.Repeat(" 0", 18)))
	fixture.write(t, "cgroup", "0::"+properties["ControlGroup"]+"\n")
	if err := os.Symlink(f.app.cfg.XrayBin(), filepath.Join(processDir, "exe")); err != nil {
		t.Fatal(err)
	}
	return fixture
}
func (f *coreExecutionFixture) write(t *testing.T, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.processDir, name), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}
func (f *coreExecutionFixture) verify() error {
	return verifyCoreExecution(f.core.app, f.core.service, f.plan.BeforeUnit.Content, f.core.identity, f.plan.BinaryDigest, true)
}

func TestCoreExecutionProofBindsEffectiveUnitAndProcess(t *testing.T) {
	f := newCoreExecutionFixture(t)
	if err := f.verify(); err != nil {
		t.Fatal(err)
	}
	if len(f.core.commands) != 0 {
		t.Fatal("identity proof performed service mutations")
	}
}
func TestCoreExecutionProofRejectsEffectiveOverrides(t *testing.T) {
	cases := map[string]string{"ExecStart": "{ path=/usr/bin/true ; argv[]=/usr/bin/true ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=123 ; code=(null) ; status=0/0 }", "User": "nobody", "Group": "root", "FragmentPath": "/run/systemd/system/proxyscene.service", "DropInPaths": "/etc/systemd/system/proxyscene.service.d/override.conf", "DynamicUser": "yes", "RootDirectory": "/alternate-root", "RootImage": "/alternate.img", "Type": "forking", "Id": "other.service", "Names": "proxyscene.service alias.service", "MainPID": "456", "InvocationID": strings.Repeat("b", 32)}
	for key, value := range cases {
		t.Run(key, func(t *testing.T) {
			f := newCoreExecutionFixture(t)
			f.properties[key] = value
			if err := f.verify(); err == nil {
				t.Fatalf("effective %s override accepted", key)
			}
			if len(f.core.commands) != 0 {
				t.Fatal("rejected identity operated service")
			}
		})
	}
}
func TestCoreExecutionProofRejectsUnloadedDiskDropIns(t *testing.T) {
	for _, directory := range []string{"proxyscene.service.d", "service.d"} {
		t.Run(directory, func(t *testing.T) {
			f := newCoreExecutionFixture(t)
			dir := filepath.Join(filepath.Dir(f.core.unit), directory)
			if err := os.Mkdir(dir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "override.conf"), []byte("[Service]\nUser=nobody\n"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := f.verify(); err == nil {
				t.Fatal("unloaded disk overlay accepted")
			}
		})
	}
}
func TestCoreExecutionProofRejectsProcessReplacement(t *testing.T) {
	cases := map[string]func(*testing.T, *coreExecutionFixture){
		"argv": func(t *testing.T, f *coreExecutionFixture) { f.write(t, "cmdline", "/usr/bin/true\x00") },
		"config": func(t *testing.T, f *coreExecutionFixture) {
			f.write(t, "cmdline", strings.Join([]string{f.core.app.cfg.XrayBin(), "run", "-config", "/other/config.json"}, "\x00")+"\x00")
		},
		"uid": func(t *testing.T, f *coreExecutionFixture) {
			f.write(t, "status", "Uid:\t0\t1000\t0\t0\nGid:\t0\t0\t0\t0\n")
		},
		"gid": func(t *testing.T, f *coreExecutionFixture) {
			f.write(t, "status", "Uid:\t0\t0\t0\t0\nGid:\t1000\t1000\t1000\t1000\n")
		},
		"cgroup": func(t *testing.T, f *coreExecutionFixture) { f.write(t, "cgroup", "0::/system.slice/other.service\n") },
		"stat": func(t *testing.T, f *coreExecutionFixture) {
			f.write(t, "stat", "999 (other) S"+strings.Repeat(" 0", 18)+" 12345")
		},
		"exe_inode": func(t *testing.T, f *coreExecutionFixture) {
			raw, err := os.ReadFile(f.core.app.cfg.XrayBin())
			if err != nil {
				t.Fatal(err)
			}
			replacement := filepath.Join(t.TempDir(), "replacement")
			if err := os.WriteFile(replacement, raw, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(f.processDir, "exe")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(replacement, filepath.Join(f.processDir, "exe")); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			f := newCoreExecutionFixture(t)
			edit(t, f)
			if err := f.verify(); err == nil {
				t.Fatal("process replacement accepted")
			}
		})
	}
}
func TestCoreLoadedReceiptRequiresLiveExecutionProof(t *testing.T) {
	f := newCoreExecutionFixture(t)
	coreVerifyExecution = verifyCoreExecution
	f.core.writeReceipt(f.plan, nil)
	if !f.core.app.coreLoadedMatches(f.plan, f.core.service) {
		t.Fatal("valid fixture proof not accepted")
	}
	f.properties["DropInPaths"] = "/run/systemd/system/proxyscene.service.d/changed.conf"
	if f.core.app.coreLoadedMatches(f.plan, f.core.service) {
		t.Fatal("matching hashes hid effective override")
	}
	if err := f.core.app.saveCoreLoaded(f.plan); err == nil {
		t.Fatal("saved loaded receipt for overridden effective service")
	}
}
func TestCoreExecutionProofRechecksManagerAfterProcessRead(t *testing.T) {
	f := newCoreExecutionFixture(t)
	output := systemctlOutput
	calls := 0
	systemctlOutput = func(label string, args ...string) (string, error) {
		calls++
		if calls == 2 {
			f.properties["InvocationID"] = strings.Repeat("b", 32)
		}
		return output(label, args...)
	}
	if err := f.verify(); err == nil {
		t.Fatal("manager invocation changed during proof")
	}
}
func TestCorePlanRecordValidatesOldServiceIdentity(t *testing.T) {
	f := newCoreExecutionFixture(t)
	if err := validateCorePlanRecord(f.plan); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*runtimeCorePlan){"missing": func(p *runtimeCorePlan) { p.BeforeServiceIdentity = nil }, "name": func(p *runtimeCorePlan) { p.BeforeServiceUser = "nobody" }, "uid_text": func(p *runtimeCorePlan) { p.BeforeServiceIdentity.UIDText = "999" }, "home": func(p *runtimeCorePlan) { p.BeforeServiceIdentity.Home = "../user" }, "inactive": func(p *runtimeCorePlan) {
		p.BeforeService.Active = "inactive"
		p.BeforeService.Sub = "dead"
		p.BeforeService.PID = 0
	}, "invocation": func(p *runtimeCorePlan) { p.BeforeService.Invocation = "forged" }, "candidate_user": func(p *runtimeCorePlan) { p.Identity.Name = "nobody" }}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			p, err := cloneRuntimeValue(f.plan)
			if err != nil {
				t.Fatal(err)
			}
			edit(p)
			if err := validateCorePlanRecord(p); err == nil {
				t.Fatal("forged service identity accepted")
			}
		})
	}
}
func TestCoreExecStartDisplayRejectsAdditionalCommands(t *testing.T) {
	f := newCoreExecutionFixture(t)
	value := f.properties["ExecStart"]
	for _, bad := range []string{value + " " + value, strings.Replace(value, "ignore_errors=no", "ignore_errors=yes", 1), strings.Replace(value, " run -config ", " run -c ", 1), strings.Replace(value, " ; status=0/0", " ; argv[]=/evil ; status=0/0", 1)} {
		if coreExecStartMatches(bad, f.core.app.cfg.XrayBin(), f.core.app.cfg.XrayConfig()) {
			t.Fatal("unsupported manager command display accepted")
		}
	}
}

func TestCoreExecutionProofIgnoresUnrelatedDanglingAlias(t *testing.T) {
	f := newCoreExecutionFixture(t)
	if err := os.Symlink("missing-timesync.service", filepath.Join(filepath.Dir(f.core.unit), "dbus-org.freedesktop.timesync1.service")); err != nil {
		t.Fatal(err)
	}
	if err := f.verify(); err != nil {
		t.Fatalf("unrelated alias blocked core-only proof: %v", err)
	}
}

func (f *coreExecutionFixture) installReplacement(t *testing.T) string {
	t.Helper()
	oldPath := f.core.app.cfg.XrayBin() + " (deleted)"
	if err := os.Rename(f.core.app.cfg.XrayBin(), oldPath); err != nil {
		t.Fatal(err)
	}
	replacement, err := os.ReadFile("/bin/echo")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.core.app.cfg.XrayBin(), replacement, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.processDir, "exe")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(oldPath, filepath.Join(f.processDir, "exe")); err != nil {
		t.Fatal(err)
	}
	coreCaptureBeforeProcess = captureCoreBeforeProcess
	coreVerifyExecution = verifyCoreExecution
	return oldPath
}
func (f *coreExecutionFixture) simulateCoreRestarts(t *testing.T) {
	t.Helper()
	run := systemctlRun
	systemctlRun = func(label string, args ...string) error {
		if err := run(label, args...); err != nil {
			return err
		}
		f.properties["MainPID"] = strconv.Itoa(f.core.service.PID)
		f.properties["InvocationID"] = f.core.service.Invocation
		f.properties["NeedDaemonReload"] = f.core.service.NeedReload
		if len(args) > 0 && args[0] == "restart" {
			if err := os.Remove(filepath.Join(f.processDir, "exe")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(f.core.app.cfg.XrayBin(), filepath.Join(f.processDir, "exe")); err != nil {
				t.Fatal(err)
			}
			f.write(t, "stat", fmt.Sprintf("%d (xray) S%s %d", f.core.service.PID, strings.Repeat(" 0", 18), 12345+f.core.restarts))
		}
		return nil
	}
}
func TestCoreInstalledReplacementBindsOldProcessWithoutNoop(t *testing.T) {
	f := newCoreExecutionFixture(t)
	f.core.writeReceipt(f.plan, nil)
	oldDigest := f.plan.BinaryDigest
	f.installReplacement(t)
	p, err := f.core.app.planCoreRuntime(f.core.store)
	if err != nil {
		t.Fatal(err)
	}
	if p.BeforeProcess == nil || p.BeforeProcess.Digest != oldDigest || p.BeforeProcess.Inode == p.BeforeBinary.Inode || p.Noop {
		t.Fatal("old process confused with installed candidate")
	}
	if err := f.core.app.validateCorePlan(p); err != nil {
		t.Fatal(err)
	}
	if err := f.core.app.compensateCorePlan(p, nil); err != nil {
		t.Fatalf("untouched trusted old process could not remain running: %v", err)
	}
	if len(f.core.commands) != 0 {
		t.Fatalf("untouched old process restarted during compensation: %v", f.core.commands)
	}
	if f.core.app.coreLoadedMatches(p, f.core.service) {
		t.Fatal("old running inode counted as loaded candidate")
	}
	if err := f.core.app.saveCoreLoaded(p); err == nil {
		t.Fatal("saved candidate-loaded proof for old inode")
	}
}
func TestCoreInstalledReplacementRejectsOldProcessDrift(t *testing.T) {
	f := newCoreExecutionFixture(t)
	f.installReplacement(t)
	p, err := f.core.app.planCoreRuntime(f.core.store)
	if err != nil {
		t.Fatal(err)
	}
	f.write(t, "stat", fmt.Sprintf("%d (xray) S%s 54321", f.core.service.PID, strings.Repeat(" 0", 18)))
	if err := f.core.app.validateCorePlan(p); err == nil {
		t.Fatal("old process changed identity between preflight and apply")
	}
	if len(f.core.commands) != 0 {
		t.Fatal("old process mismatch performed service action")
	}
}
func TestCoreInstalledReplacementRejectsUntrustedOldExecutable(t *testing.T) {
	for _, invalid := range []string{"writable", "not-elf"} {
		t.Run(invalid, func(t *testing.T) {
			f := newCoreExecutionFixture(t)
			path := f.installReplacement(t)
			if invalid == "writable" {
				if err := os.Chmod(path, 0777); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.core.app.planCoreRuntime(f.core.store); err == nil {
				t.Fatal("untrusted old executable accepted")
			}
		})
	}
}
func TestCoreInstalledReplacementChecksRollbackConfigBeforeSwitch(t *testing.T) {
	f := newCoreExecutionFixture(t)
	f.installReplacement(t)
	before := f.core.file(f.core.app.cfg.XrayConfig())
	f.core.store.Nodes[0].RawURL = "trojan://changed@new-fixture.example:443"
	check := coreCheckConfig
	failure := errors.New("new binary rejects old config")
	coreCheckConfig = func(app *App, path string) error {
		if err := check(app, path); err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Equal(raw, before.Content) {
			return failure
		}
		return nil
	}
	if _, err := f.core.app.planCoreRuntime(f.core.store); !errors.Is(err, failure) {
		t.Fatalf("old config compatibility not checked: %v", err)
	}
	if len(f.core.commands) != 0 || !coreFilesEqual(f.core.file(f.core.app.cfg.XrayConfig()), before) {
		t.Fatal("failed rollback compatibility check changed live runtime")
	}
}
func TestCoreInstalledReplacementStoreFailureUsesInstalledBinaryForOldConfig(t *testing.T) {
	f := newCoreExecutionFixture(t)
	f.installReplacement(t)
	f.simulateCoreRestarts(t)
	profile, apt := withGlobalProxyTestPaths(t, f.core.app)
	if err := f.core.app.saveStore(f.core.store); err != nil {
		t.Fatal(err)
	}
	beforeStore, err := os.ReadFile(f.core.app.cfg.StorePath())
	if err != nil {
		t.Fatal(err)
	}
	beforeConfig := f.core.file(f.core.app.cfg.XrayConfig())
	installedDigest, err := coreBinaryDigest(f.core.app.cfg.XrayBin())
	if err != nil {
		t.Fatal(err)
	}
	persist := runtimePersistStore
	t.Cleanup(func() { runtimePersistStore = persist })
	failure := errors.New("store failed after upgraded core start")
	runtimePersistStore = func(*App, *Store) error { return failure }
	err = f.core.app.commitStoreMutation(f.core.store, func(st *Store) error { st.Nodes[0].RawURL = "trojan://changed@new-fixture.example:443"; return nil }, storeRuntimeSyncGlobal)
	if !errors.Is(err, failure) {
		t.Fatalf("expected Store failure, got %v", err)
	}
	if f.core.restarts != 2 {
		t.Fatalf("expected upgrade and compensation starts, got %d: %v", f.core.restarts, err)
	}
	if !coreFilesEqual(f.core.file(f.core.app.cfg.XrayConfig()), beforeConfig) {
		t.Fatal("old configuration not restored")
	}
	digest, err := coreBinaryDigest(f.core.app.cfg.XrayBin())
	if err != nil || digest != installedDigest {
		t.Fatalf("compensation changed installer's committed binary: %v", err)
	}
	afterStore, _ := os.ReadFile(f.core.app.cfg.StorePath())
	if !bytes.Equal(afterStore, beforeStore) {
		t.Fatal("Store changed despite failed commit")
	}
	assertRuntimeNoGlobalOwnership(t, f.core.app, profile, apt)
	if pending, _ := f.core.app.hasRuntimeTransition(); pending {
		t.Fatalf("compensation retained completed receipt: %v", err)
	}
}
func TestCoreNeedDaemonReloadCanReconcileVerifiedProcess(t *testing.T) {
	f := newCoreExecutionFixture(t)
	coreCaptureBeforeProcess = captureCoreBeforeProcess
	coreVerifyExecution = verifyCoreExecution
	f.core.service.NeedReload = "yes"
	f.properties["NeedDaemonReload"] = "yes"
	f.simulateCoreRestarts(t)
	p, err := f.core.app.planCoreRuntime(f.core.store)
	if err != nil {
		t.Fatal(err)
	}
	if p.Noop {
		t.Fatal("NeedDaemonReload=yes counted as no-op")
	}
	if err := f.core.app.applyCorePlan(p); err != nil {
		t.Fatal(err)
	}
	if f.core.restarts != 1 || f.core.service.NeedReload != "no" {
		t.Fatal("verified pending unit was not reconciled")
	}
}
