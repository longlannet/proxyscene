package manager

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the real effective-unit parser and runtime/file/message validators.
// Only unit search roots, account lookup and systemctl are redirected to fixtures.
type telegramReleaseFixture struct {
	*telegramJournalTestHarness
	unitPath string
	base     string
	roots    []unitSearchRoot
	calls    int
}

func newTelegramReleaseFixture(t *testing.T) *telegramReleaseFixture {
	t.Helper()
	h := newTelegramJournalTestHarness(t)
	f := &telegramReleaseFixture{telegramJournalTestHarness: h}
	root := filepath.Join(h.dir, "units")
	f.roots = []unitSearchRoot{{Path: root, Manage: true}}
	f.unitPath = filepath.Join(root, h.target().Service)
	h.systemPath = filepath.Join(f.unitPath+".d", telegramManagedDropInName)
	h.legacyPath = filepath.Join(f.unitPath+".d", "10-openclaw-hermes-telegram-proxy.conf")
	project := filepath.Join(h.dir, ".hermes", "hermes-agent")
	for _, dir := range []string{filepath.Dir(h.systemPath), project} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.base = "[Service]\nExecStart=" + project + "/venv/bin/python -m hermes_cli.main gateway run\nEnvironment=HOME=" + h.dir + "\n"
	f.writeUnit(t, "")
	if err := os.WriteFile(filepath.Join(h.dir, ".hermes", "config.yaml"), []byte("platforms:\n  telegram:\n    extra:\n      drop_pending_on_cold_boot: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	previousEnvironment := telegramOutputSystemManagerEnvironment
	t.Cleanup(func() { telegramOutputSystemManagerEnvironment = previousEnvironment })
	telegramOutputSystemManagerEnvironment = func() (string, error) { return "", nil }
	telegramInspectPlanUnit = func(target systemdTargetName, _ *persistedUserIdentity) (telegramPlanUnit, error) {
		unit := telegramPlanUnit{roots: f.roots, resolution: resolveTelegramUnitInRoots(target.Service, []string{root})}
		var err error
		unit.content, err = readResolvedTelegramUnitContent(unit.resolution, f.roots)
		if err == nil {
			unit.kind, err = classifyTelegramUnitContent(unit.content)
		}
		return unit, err
	}
	telegramValidateHermesTarget = func(target systemdTargetName, identity *persistedUserIdentity, proxy string) error {
		return f.validate(target, identity, "", proxy, false, true)
	}
	telegramValidateHermesReleaseTarget = func(target systemdTargetName, identity *persistedUserIdentity, omitted string) error {
		running, err := telegramTargetRunning(target, identity)
		if err != nil {
			return err
		}
		return f.validate(target, identity, omitted, "", running, false)
	}
	telegramValidateHermesRestartPolicy = func(target systemdTargetName, identity *persistedUserIdentity) error {
		return f.validate(target, identity, "", "", true, true)
	}
	telegramValidateHermesReleaseRestartPolicy = func(target systemdTargetName, identity *persistedUserIdentity) error {
		return f.validate(target, identity, "", "", true, false)
	}
	systemctlRun = func(string, ...string) error { f.calls++; return nil }
	return f
}

func (f *telegramReleaseFixture) writeUnit(t *testing.T, extra string) {
	t.Helper()
	if err := os.WriteFile(f.unitPath, []byte(f.base+extra), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *telegramReleaseFixture) validate(target systemdTargetName, identity *persistedUserIdentity, omitted, proxy string, restart, enforceProxy bool) error {
	content, err := readTelegramUnitContentWithoutDropIn(f.unitPath, target.Service, f.roots, omitted)
	if err != nil {
		return err
	}
	return validateHermesRuntimeContent(target, identity, content, proxy, restart, enforceProxy)
}

func (f *telegramReleaseFixture) legacy(t *testing.T) *Store {
	t.Helper()
	st := newStore()
	st.RuntimeConfig = f.app.cfg.runtimeConfig()
	st.TelegramTargets = []string{canonicalTelegramTargetName(f.target())}
	for path, content := range map[string]string{
		f.legacyPath: "[Service]\nEnvironmentFile=-/etc/openclaw-hermes-tg-proxy.env\n",
		f.envPath:    telegramProxyEnvContent(f.app.cfg),
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func TestLegacyTelegramReleaseProjectsExactOwnedDropInWithoutWriting(t *testing.T) {
	f := newTelegramReleaseFixture(t)
	st := f.legacy(t)
	before, _ := os.ReadFile(f.legacyPath)
	if err := telegramValidateHermesTarget(f.target(), nil, ""); err == nil || !strings.Contains(err.Error(), "EnvironmentFile") {
		t.Fatalf("enable unexpectedly accepted the legacy EnvironmentFile: %v", err)
	}
	plan, err := f.app.planTelegramSelection(st, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(f.legacyPath)
	if err != nil || !bytes.Equal(before, after) || f.calls != 0 || len(st.TelegramTargets) != 1 {
		t.Fatalf("release planning mutated legacy evidence or service: err=%v calls=%d", err, f.calls)
	}
	if err := f.app.restoreTelegramPlan(st, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(f.legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy drop-in was not removed: %v", err)
	}
	if raw, err := os.ReadFile(f.envPath); err != nil || string(raw) != telegramProxyEnvContent(f.app.cfg) {
		t.Fatalf("shared legacy env was not retained: %v", err)
	}
	if len(st.TelegramTargets) != 0 || f.calls == 0 {
		t.Fatalf("explicit release did not finish: targets=%v calls=%d", st.TelegramTargets, f.calls)
	}
}

func TestLegacyTelegramReleaseProjectionRetainsAllOtherSafetyChecks(t *testing.T) {
	for _, scenario := range []string{"other env file", "lower-priority same basename", "unsafe message policy", "project symlink", "changed account identity", "dotenv injection", "mismatched historical env"} {
		t.Run(scenario, func(t *testing.T) {
			f := newTelegramReleaseFixture(t)
			st := f.legacy(t)
			switch scenario {
			case "other env file":
				f.writeUnit(t, "EnvironmentFile=-/etc/operator.env\n")
			case "lower-priority same basename":
				vendor := filepath.Join(f.dir, "vendor")
				f.roots = append(f.roots, unitSearchRoot{Path: vendor, Manage: true})
				path := filepath.Join(vendor, f.target().Service+".d", filepath.Base(f.legacyPath))
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("[Service]\nEnvironmentFile=-/etc/operator.env\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unsafe message policy":
				if err := os.WriteFile(filepath.Join(f.dir, ".hermes", "config.yaml"), []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "project symlink":
				project := filepath.Join(f.dir, ".hermes", "hermes-agent")
				if err := os.Rename(project, project+".real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(project+".real", project); err != nil {
					t.Fatal(err)
				}
			case "changed account identity":
				f.identity.UID++
			case "dotenv injection":
				if err := os.WriteFile(filepath.Join(f.dir, ".hermes", ".env"), []byte("PYTHONPATH=/tmp/injection\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "mismatched historical env":
				if err := os.WriteFile(f.envPath, []byte("TELEGRAM_PROXY=http://changed.invalid\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(f.legacyPath)
			if _, err := f.app.planTelegramSelection(st, nil, false, nil); err == nil {
				t.Fatal("unsafe legacy release accepted")
			}
			after, err := os.ReadFile(f.legacyPath)
			if err != nil || !bytes.Equal(before, after) || f.calls != 0 || len(st.TelegramTargets) != 1 {
				t.Fatal("unsafe release changed files, tracking or service")
			}
		})
	}
}

func TestModernTelegramExplicitReleaseDoesNotRequireProxyRouting(t *testing.T) {
	f := newTelegramReleaseFixture(t)
	st := newStore()
	if _, err := f.app.applyTelegram(st, []systemdTargetName{f.target()}); err != nil {
		t.Fatal(err)
	}
	f.calls = 0
	f.writeUnit(t, "Environment=NO_PROXY=*\n")
	if _, err := f.app.planTelegramSelection(st, []systemdTargetName{f.target()}, true, nil); err == nil {
		t.Fatal("enable accepted a Telegram proxy bypass")
	}
	plan, err := f.app.planTelegramSelection(st, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.calls != 0 {
		t.Fatal("plan operated service")
	}
	if err := f.app.restoreTelegramPlan(st, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(f.systemPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed drop-in retained: %v", err)
	}
	if len(f.journal(t).Targets) != 0 {
		t.Fatal("completed release retained ownership")
	}
}

func TestTelegramApplyFinalRestartGuardStillRejectsNewProxyBypass(t *testing.T) {
	f := newTelegramReleaseFixture(t)
	if _, err := f.app.applyTelegram(newStore(), []systemdTargetName{f.target()}); err != nil {
		t.Fatal(err)
	}
	journal := f.journal(t)
	entry := journal.Targets[canonicalTelegramTargetName(f.target())]
	entry.Phase = telegramPhasePrepared
	entry.PendingManagedContent = entry.ManagedContent
	if err := f.app.saveTelegramProxyJournal(journal); err != nil {
		t.Fatal(err)
	}
	f.calls = 0
	telegramReadServiceState = func(systemdTargetName, *persistedUserIdentity) (telegramServiceState, error) {
		// This is after the prepared apply's final expected-proxy validation.
		f.writeUnit(t, "Environment=NO_PROXY=*\n")
		return telegramServiceState{LoadState: "loaded", ActiveState: "active"}, nil
	}
	if err := f.app.restartTelegramTarget(f.target()); err == nil || !strings.Contains(err.Error(), "NO_PROXY") {
		t.Fatalf("final apply restart guard accepted new proxy bypass: %v", err)
	}
	if f.calls != 0 {
		t.Fatal("unsafe apply restarted gateway")
	}
	if f.journal(t).Targets[canonicalTelegramTargetName(f.target())].Phase != telegramPhasePrepared {
		t.Fatal("rejected apply lost prepared ownership")
	}
}

func TestTelegramCompensationRestoringManagedProxyKeepsStrictRestartGuard(t *testing.T) {
	f := newTelegramReleaseFixture(t)
	if _, err := f.app.applyTelegram(newStore(), []systemdTargetName{f.target()}); err != nil {
		t.Fatal(err)
	}
	journal := f.journal(t)
	entry := journal.Targets[canonicalTelegramTargetName(f.target())]
	before := *entry
	entry.Phase = telegramPhasePrepared
	entry.PendingManagedContent = entry.ManagedContent
	if err := f.app.saveTelegramProxyJournal(journal); err != nil {
		t.Fatal(err)
	}
	restarted := false
	systemctlRun = func(_ string, args ...string) error {
		if strings.Contains(strings.Join(args, " "), "try-restart") {
			restarted = true
		}
		return nil
	}
	telegramReadServiceState = func(systemdTargetName, *persistedUserIdentity) (telegramServiceState, error) {
		f.writeUnit(t, "Environment=NO_PROXY=*\n")
		return telegramServiceState{LoadState: "loaded", ActiveState: "active"}, nil
	}
	err := runtimeReconcileHermes(f.app, f.target(), &runtimeHermesResource{Before: &before})
	if err == nil || !strings.Contains(err.Error(), "NO_PROXY") || restarted {
		t.Fatalf("compensation weakened final apply policy: err=%v restarted=%v", err, restarted)
	}
	if f.journal(t).Targets[canonicalTelegramTargetName(f.target())].Phase != telegramPhasePrepared {
		t.Fatal("rejected compensation lost prepared ownership")
	}
}
