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

// Existing lifecycle fixtures provide synthetic unit existence and runtime
// validators. Give their read-only plans matching unit evidence as well.
func stubTelegramPlanUnitsForLifecycle(t *testing.T) {
	t.Helper()
	previous := telegramInspectPlanUnit
	t.Cleanup(func() { telegramInspectPlanUnit = previous })
	telegramInspectPlanUnit = func(target systemdTargetName, identity *persistedUserIdentity) (telegramPlanUnit, error) {
		unit, err := inspectTelegramPlanUnit(target, identity)
		if err != nil || unit.resolution.State != telegramUnitAbsent || !telegramTargetUnitInstalled(target) {
			return unit, err
		}
		unit.resolution.State = telegramUnitResolved
		unit.kind = telegramUnitHermes
		if strings.HasPrefix(target.Service, "openclaw") {
			unit.kind = telegramUnitOpenClaw
		}
		unit.content = "fixture:" + canonicalTelegramTargetName(target)
		return unit, nil
	}
}

func telegramPlanPairFixture(t *testing.T) (*telegramJournalTestHarness, *openClawTestHarness, []systemdTargetName) {
	t.Helper()
	hermes := newTelegramJournalTestHarness(t)
	claw := newOpenClawTestHarness(t, `{}`)
	targets := []systemdTargetName{hermes.target(), claw.target("openclaw-plan-fixture.service")}
	if _, err := claw.app.applyTelegram(newStore(), targets); err != nil {
		t.Fatal(err)
	}
	return hermes, claw, targets
}

func TestTelegramPlanEmptyAndPartialSelectionPreserveOtherOwnership(t *testing.T) {
	hermes, claw, targets := telegramPlanPairFixture(t)
	clawBefore := claw.config(t)
	hermesBefore, err := os.ReadFile(hermes.systemPath)
	if err != nil {
		t.Fatal(err)
	}
	hermesJournal, _ := claw.app.loadTelegramProxyJournal()
	clawJournal := claw.journal(t)
	calls := 0
	systemctlRun = func(string, ...string) error { calls++; return nil }
	userSystemctlRun = func(string, *persistedUserIdentity, string, ...string) error { calls++; return nil }
	if _, err := claw.app.applyTelegram(newStore(), []systemdTargetName{}); err == nil {
		t.Fatal("empty enabled selection accepted")
	}
	if _, err := claw.app.applyTelegram(newStore(), targets[:1]); err != nil {
		t.Fatal(err)
	}
	gotHermes, err := os.ReadFile(hermes.systemPath)
	gotJournal, _ := claw.app.loadTelegramProxyJournal()
	if err != nil || !bytes.Equal(gotHermes, hermesBefore) || !bytes.Equal(claw.config(t), clawBefore) || !reflect.DeepEqual(claw.journal(t), clawJournal) || !reflect.DeepEqual(gotJournal, hermesJournal) || calls != 0 {
		t.Fatalf("omitted owner changed, service calls=%d err=%v", calls, err)
	}
}

func TestTelegramPlanSelectedMissingUnitKeepsArtifactsAndJournals(t *testing.T) {
	hermes, claw, targets := telegramPlanPairFixture(t)
	before, _ := os.ReadFile(hermes.systemPath)
	clawBefore := claw.config(t)
	hj, _ := claw.app.loadTelegramProxyJournal()
	cj := claw.journal(t)
	telegramSystemUnitExists = func(string) bool { return false }
	systemctlRun = func(string, ...string) error { t.Fatal("preflight failure ran systemctl"); return nil }
	userSystemctlRun = func(string, *persistedUserIdentity, string, ...string) error {
		t.Fatal("preflight failure ran user systemctl")
		return nil
	}
	if _, err := claw.app.applyTelegram(newStore(), targets); err == nil {
		t.Fatal("missing owned selected unit accepted")
	}
	after, err := os.ReadFile(hermes.systemPath)
	current, _ := claw.app.loadTelegramProxyJournal()
	if err != nil || !bytes.Equal(before, after) || !bytes.Equal(clawBefore, claw.config(t)) || !reflect.DeepEqual(hj, current) || !reflect.DeepEqual(cj, claw.journal(t)) {
		t.Fatal("failed selected target was released")
	}
}

func TestTelegramPlanValidatesAllTargetsBeforeAnyWrite(t *testing.T) {
	hermes := newTelegramJournalTestHarness(t)
	claw := newOpenClawTestHarness(t, `{}`)
	failure := errors.New("invalid second gateway runtime")
	openClawValidateTargetRuntime = func(systemdTargetName, *persistedUserIdentity) error { return failure }
	systemctlRun = func(string, ...string) error { t.Fatal("preflight called systemctl"); return nil }
	userSystemctlRun = func(string, *persistedUserIdentity, string, ...string) error {
		t.Fatal("preflight called user systemctl")
		return nil
	}
	_, err := claw.app.applyTelegram(newStore(), []systemdTargetName{hermes.target(), claw.target("openclaw-invalid-plan.service")})
	if !errors.Is(err, failure) {
		t.Fatalf("runtime error lost: %v", err)
	}
	if _, err := os.Lstat(hermes.systemPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("first target was written before second target validation")
	}
	for _, path := range []string{claw.app.telegramProxyJournalPath(), claw.app.telegramProxyJournalLockPath(), claw.app.openClawJournalPath(), claw.app.openClawJournalLockPath()} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("preflight created journal/control file: %s", path)
		}
	}
	if string(claw.config(t)) != "{}" {
		t.Fatal("preflight changed OpenClaw config")
	}
}

func TestTelegramPlanRejectsChangedArtifactBeforeExecute(t *testing.T) {
	hermes := newTelegramJournalTestHarness(t)
	st := newStore()
	plan, err := hermes.app.planTelegramSelection(st, []systemdTargetName{hermes.target()}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	replacement := []byte("operator replacement")
	if err := os.WriteFile(hermes.systemPath, replacement, 0600); err != nil {
		t.Fatal(err)
	}
	systemctlRun = func(string, ...string) error { t.Fatal("changed plan executed systemctl"); return nil }
	if _, err := hermes.app.applyTelegramPlan(st, plan); err == nil {
		t.Fatal("changed artifact accepted")
	}
	raw, _ := os.ReadFile(hermes.systemPath)
	if !bytes.Equal(raw, replacement) {
		t.Fatal("operator replacement overwritten")
	}
}

func TestTelegramPlanExplicitShutdownReleasesSharedConfig(t *testing.T) {
	hermes := newTelegramJournalTestHarness(t)
	claw := newOpenClawTestHarness(t, `{}`)
	targets := []systemdTargetName{hermes.target(), claw.target("openclaw-plan-one.service"), claw.target("openclaw-plan-two.service")}
	if _, err := claw.app.applyTelegram(newStore(), targets); err != nil {
		t.Fatal(err)
	}
	st := newStore()
	plan, err := claw.app.planTelegram(st, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.releases) != 3 || len(plan.selected) != 0 {
		t.Fatalf("unexpected shutdown plan: %+v", plan)
	}
	if err := claw.app.restoreTelegramPlan(st, plan); err != nil {
		t.Fatal(err)
	}
	hj, _ := claw.app.loadTelegramProxyJournal()
	if len(hj.Targets) != 0 || len(claw.journal(t).Users) != 0 {
		t.Fatal("explicit full shutdown retained ownership")
	}
}

func TestTelegramPlanLegacyMissingRuntimeDoesNotCoordinateOrDropRecord(t *testing.T) {
	h, st, before := legacyLifecycleReviewFixture(t)
	st.RuntimeConfig = nil
	systemctlRun = func(string, ...string) error { t.Fatal("missing runtime called service"); return nil }
	if _, err := h.app.planTelegram(st, false); err == nil {
		t.Fatal("missing history accepted by planning")
	}
	if err := h.app.cleanupLegacyTelegramTarget(st, h.target()); err == nil {
		t.Fatal("legacy executor accepted missing runtime")
	}
	if len(st.TelegramTargets) != 1 {
		t.Fatal("missing history lost ownership")
	}
	after, err := os.ReadFile(h.legacyPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("missing history modified artifact")
	}
}

func TestTelegramPlanDefaultAbsentAnchorDoesNotCreateArtifact(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	old := telegramInspectPlanUnit
	telegramInspectPlanUnit = func(target systemdTargetName, identity *persistedUserIdentity) (telegramPlanUnit, error) {
		if target.Service == "hermes-absent.service" {
			return telegramPlanUnit{resolution: telegramUnitResolution{State: telegramUnitAbsent, Requested: target.Service}}, nil
		}
		return old(target, identity)
	}
	plan, err := h.app.planTelegramSelection(newStore(), []systemdTargetName{h.target(), {Service: "hermes-absent.service"}}, true, nil)
	if err != nil || len(plan.selected) != 1 {
		t.Fatalf("unused absent anchor blocked valid target: %+v %v", plan, err)
	}
	if _, err := os.Lstat(filepath.Join(h.app.cfg.CoreDir, "telegram-proxy-journal.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("planning wrote journal")
	}
}

func TestTelegramPlanSharedReleaseRequiresWholeGroupAuthorization(t *testing.T) {
	hermes := newTelegramJournalTestHarness(t)
	claw := newOpenClawTestHarness(t, `{}`)
	targets := []systemdTargetName{hermes.target(), claw.target("openclaw-plan-one.service"), claw.target("openclaw-plan-two.service")}
	if _, err := claw.app.applyTelegram(newStore(), targets); err != nil {
		t.Fatal(err)
	}
	before := claw.config(t)
	authorized := map[string]bool{canonicalTelegramTargetName(targets[1]): true}
	if _, err := claw.app.planTelegramSelection(newStore(), targets, true, authorized); err == nil {
		t.Fatal("partial shared release accepted")
	}
	if !bytes.Equal(before, claw.config(t)) {
		t.Fatal("rejected shared release changed config")
	}
	authorized[canonicalTelegramTargetName(targets[2])] = true
	plan, err := claw.app.planTelegramSelection(newStore(), targets, true, authorized)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := claw.app.applyTelegramPlan(newStore(), plan); err != nil {
		t.Fatal(err)
	}
	if len(claw.journal(t).Users) != 0 {
		t.Fatal("explicit complete-group release did not retire OpenClaw")
	}
}

func TestTelegramPlanMissingUnitShutdownRequiresStoppedProcess(t *testing.T) {
	hermes := newTelegramJournalTestHarness(t)
	if _, err := hermes.app.applyTelegram(newStore(), []systemdTargetName{hermes.target()}); err != nil {
		t.Fatal(err)
	}
	telegramSystemUnitExists = func(string) bool { return false }
	if _, err := hermes.app.planTelegram(newStore(), false); err == nil {
		t.Fatal("missing disk unit authorized release of an active process")
	}
	stubTelegramServiceState(t, telegramServiceState{LoadState: "not-found", ActiveState: "inactive"})
	plan, err := hermes.app.planTelegram(newStore(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := hermes.app.restoreTelegramPlan(newStore(), plan); err != nil {
		t.Fatal(err)
	}
	if len(hermes.journal(t).Targets) != 0 {
		t.Fatal("explicit inactive missing-unit release retained ownership")
	}
}

func TestTelegramPlanSharedApplyRequiresWholeAffectedGroup(t *testing.T) {
	for _, change := range []string{"new-proxy", "pending", "new-target"} {
		t.Run(change, func(t *testing.T) {
			claw := newOpenClawTestHarness(t, `{}`)
			oldExists := telegramUserUnitExists
			oldRun := userSystemctlRun
			t.Cleanup(func() {
				telegramUserUnitExists = oldExists
				userSystemctlRun = oldRun
			})
			telegramUserUnitExists = func(string, string) bool { return true }
			userSystemctlRun = func(string, *persistedUserIdentity, string, ...string) error { return nil }
			targets := []systemdTargetName{claw.target("openclaw-group-one.service"), claw.target("openclaw-group-two.service")}
			if _, err := claw.app.applyTelegram(newStore(), targets); err != nil {
				t.Fatal(err)
			}
			selected := targets[:1]
			switch change {
			case "new-proxy":
				claw.app.cfg.TGHTTPPort++
			case "pending":
				if _, _, err := claw.app.applyOpenClawTelegramProxy(targets[0], "http://127.0.0.1:8892"); err != nil {
					t.Fatal(err)
				}
			case "new-target":
				selected = append([]systemdTargetName{targets[0]}, claw.target("openclaw-group-three.service"))
			}
			beforeConfig := claw.config(t)
			beforeJournal := claw.journal(t)
			userSystemctlRun = func(string, *persistedUserIdentity, string, ...string) error {
				t.Fatal("incomplete shared plan reached service operations")
				return nil
			}
			if _, err := claw.app.applyTelegram(newStore(), selected); err == nil || !strings.Contains(err.Error(), "全部已接管目标") {
				t.Fatalf("incomplete affected group accepted: %v", err)
			}
			if !bytes.Equal(beforeConfig, claw.config(t)) || !reflect.DeepEqual(beforeJournal, claw.journal(t)) {
				t.Fatal("rejected group changed shared config or pending ownership")
			}
		})
	}
}

func TestTelegramPlanUnchangedSharedOwnerCanBeSelectedAlone(t *testing.T) {
	claw := newOpenClawTestHarness(t, `{}`)
	oldExists := telegramUserUnitExists
	oldRun := userSystemctlRun
	t.Cleanup(func() {
		telegramUserUnitExists = oldExists
		userSystemctlRun = oldRun
	})
	telegramUserUnitExists = func(string, string) bool { return true }
	userSystemctlRun = func(string, *persistedUserIdentity, string, ...string) error { return nil }
	targets := []systemdTargetName{claw.target("openclaw-group-one.service"), claw.target("openclaw-group-two.service")}
	if _, err := claw.app.applyTelegram(newStore(), targets); err != nil {
		t.Fatal(err)
	}
	beforeConfig := claw.config(t)
	beforeJournal := claw.journal(t)
	userSystemctlRun = func(string, *persistedUserIdentity, string, ...string) error {
		t.Fatal("unchanged selected owner restarted")
		return nil
	}
	if _, err := claw.app.applyTelegram(newStore(), targets[:1]); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeConfig, claw.config(t)) || !reflect.DeepEqual(beforeJournal, claw.journal(t)) {
		t.Fatal("unchanged partial selection changed shared config or journal")
	}
}

func TestTelegramApplyExecutionDriftCannotReleaseSelectedOwnership(t *testing.T) {
	for _, changed := range []string{"hermes-artifact", "hermes-unreadable", "openclaw-config"} {
		t.Run(changed, func(t *testing.T) {
			hermes, claw, targets := telegramPlanPairFixture(t)
			beforeHermes, err := claw.app.loadTelegramProxyJournal()
			if err != nil {
				t.Fatal(err)
			}
			beforeClaw := claw.journal(t)
			validations := 0
			operatorContent := []byte("[Service]\nEnvironment=\"TELEGRAM_PROXY=http://operator.invalid:9999\"\n")
			if changed != "openclaw-config" {
				originalRead := telegramReadSystemArtifact
				telegramValidateHermesTarget = func(systemdTargetName, *persistedUserIdentity, string) error {
					validations++
					if validations == 3 {
						if changed == "hermes-unreadable" {
							telegramReadSystemArtifact = func(path string, max int64) ([]byte, error) {
								if path == hermes.systemPath {
									return nil, os.ErrPermission
								}
								return originalRead(path, max)
							}
							return nil
						}
						return os.WriteFile(hermes.systemPath, operatorContent, 0o600)
					}
					return nil
				}
			} else {
				openClawValidateTargetRuntime = func(systemdTargetName, *persistedUserIdentity) error {
					validations++
					if validations == 3 {
						return os.Remove(claw.configPath)
					}
					return nil
				}
			}
			systemctlRun = func(string, ...string) error { t.Fatal("execution drift restarted service"); return nil }
			userSystemctlRun = func(string, *persistedUserIdentity, string, ...string) error {
				t.Fatal("execution drift restarted user service")
				return nil
			}
			if _, err := claw.app.applyTelegram(newStore(), targets); err == nil {
				t.Fatal("selected target drift was silently skipped or released")
			}
			afterHermes, err := claw.app.loadTelegramProxyJournal()
			if err != nil || !reflect.DeepEqual(beforeHermes, afterHermes) || !reflect.DeepEqual(beforeClaw, claw.journal(t)) {
				t.Fatalf("execution drift changed ownership: %v", err)
			}
			if validations < 3 {
				t.Fatalf("fixture did not reach execution after planning: %d", validations)
			}
			if changed == "hermes-artifact" {
				after, err := os.ReadFile(hermes.systemPath)
				if err != nil || !bytes.Equal(after, operatorContent) {
					t.Fatal("operator artifact changed during failed apply")
				}
			}
		})
	}
}

func TestHermesApplyCannotResumePendingRelease(t *testing.T) {
	hermes := newTelegramJournalTestHarness(t)
	target := hermes.target()
	if _, err := hermes.app.applyTelegram(newStore(), []systemdTargetName{target}); err != nil {
		t.Fatal(err)
	}
	journal := hermes.journal(t)
	journal.Targets[canonicalTelegramTargetName(target)].Phase = telegramPhaseRestoring
	if err := hermes.app.saveTelegramProxyJournal(journal); err != nil {
		t.Fatal(err)
	}
	beforeJournal := hermes.journal(t)
	beforeArtifact, err := os.ReadFile(hermes.systemPath)
	if err != nil {
		t.Fatal(err)
	}
	systemctlRun = func(string, ...string) error { t.Fatal("pending release ran from apply"); return nil }
	if _, err := hermes.app.planTelegramSelection(newStore(), []systemdTargetName{target}, true, nil); err == nil {
		t.Fatal("pending release accepted by apply planner")
	}
	if _, err := hermes.app.prepareHermesTelegramApply(target, beforeArtifact); err == nil {
		t.Fatal("pending release accepted by apply executor")
	}
	after, err := os.ReadFile(hermes.systemPath)
	if err != nil || !bytes.Equal(after, beforeArtifact) || !reflect.DeepEqual(beforeJournal, hermes.journal(t)) {
		t.Fatal("apply consumed pending release evidence")
	}
}

func TestTelegramApplySharedGroupCompletesEarlierUnchangedOwner(t *testing.T) {
	claw := newOpenClawTestHarness(t, `{}`)
	oldExists := telegramUserUnitExists
	oldRun := userSystemctlRun
	t.Cleanup(func() {
		telegramUserUnitExists = oldExists
		userSystemctlRun = oldRun
	})
	telegramUserUnitExists = func(string, string) bool { return true }
	userSystemctlRun = func(string, *persistedUserIdentity, string, ...string) error { return nil }
	first := claw.target("openclaw-shared-first.service")
	second := claw.target("openclaw-shared-second.service")
	added := claw.target("openclaw-shared-new.service")
	if _, err := claw.app.applyTelegram(newStore(), []systemdTargetName{first, second}); err != nil {
		t.Fatal(err)
	}
	restarted := map[string]bool{}
	userSystemctlRun = func(_ string, _ *persistedUserIdentity, _ string, args ...string) error {
		if len(args) == 3 && args[0] == "try-restart" && args[1] == "--" {
			restarted[args[2]] = true
		}
		return nil
	}
	selected := []systemdTargetName{first, added, second}
	if _, err := claw.app.applyTelegram(newStore(), selected); err != nil {
		t.Fatal(err)
	}
	entry := claw.journal(t).Users[claw.user]
	if entry == nil || entry.Phase != openClawPhaseActive || len(entry.PendingTargets) != 0 || len(entry.Targets) != len(selected) {
		t.Fatalf("shared group did not fully commit: %+v", entry)
	}
	for _, target := range selected {
		if !restarted[target.Service] {
			t.Fatalf("pending owner never coordinated: %s, restarts=%v", target.Service, restarted)
		}
	}
}

func TestOpenClawApplyCannotResumePendingRelease(t *testing.T) {
	claw := newOpenClawTestHarness(t, `{}`)
	target := claw.target("openclaw-pending-release.service")
	proxy := claw.app.cfg.HTTPAddr(SceneTelegram)
	if _, _, err := claw.app.applyOpenClawTelegramProxy(target, proxy); err != nil {
		t.Fatal(err)
	}
	if err := claw.app.commitOpenClawTelegramProxyApply(target); err != nil {
		t.Fatal(err)
	}
	journal := claw.journal(t)
	journal.Users[claw.user].Phase = openClawPhaseRestoring
	if err := claw.app.saveOpenClawProxyJournal(journal); err != nil {
		t.Fatal(err)
	}
	oldExists := telegramUserUnitExists
	t.Cleanup(func() { telegramUserUnitExists = oldExists })
	telegramUserUnitExists = func(string, string) bool { return true }
	beforeJournal := claw.journal(t)
	beforeConfig := claw.config(t)
	if _, err := claw.app.planTelegramSelection(newStore(), []systemdTargetName{target}, true, nil); err == nil {
		t.Fatal("pending release accepted by apply planner")
	}
	if _, _, err := claw.app.applyOpenClawTelegramProxy(target, proxy); err == nil {
		t.Fatal("pending release accepted by apply executor")
	}
	if !bytes.Equal(beforeConfig, claw.config(t)) || !reflect.DeepEqual(beforeJournal, claw.journal(t)) {
		t.Fatal("apply changed pending OpenClaw release evidence")
	}
}
