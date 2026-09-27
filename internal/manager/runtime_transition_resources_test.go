package manager

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func TestRuntimeResourceGlobalRestoresTransactionBeforeNotTakeoverOriginal(t *testing.T) {
	for _, released := range []bool{false, true} {
		t.Run(map[bool]string{false: "updated", true: "released"}[released], func(t *testing.T) {
			app := testApp(t)
			profile, apt := withGlobalProxyTestPaths(t, app)
			if err := os.WriteFile(profile, []byte("operator baseline\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := app.applyGlobalWithJournal(); err != nil {
				t.Fatal(err)
			}
			cfg := app.cfg
			cfg.GlobalHTTPPort += 1000
			candidate := NewApp(cfg)
			snapshot, err := captureRuntimeGlobal(app, candidate, true)
			if err != nil {
				t.Fatal(err)
			}
			if released {
				err = app.restoreGlobalWithJournal()
			} else {
				err = candidate.applyGlobalWithJournal()
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := compensateRuntimeGlobal(app, snapshot); err != nil {
				t.Fatal(err)
			}
			for i, path := range []string{profile, apt} {
				if !globalProxyArtifactStatesEqual(mustReadGlobalProxyTestState(t, path), snapshot.Files[i].globalState()) {
					t.Fatalf("before-image not restored: %s", path)
				}
			}
			restored, err := app.loadGlobalProxyJournal()
			if err != nil {
				t.Fatal(err)
			}
			if restored.Generation <= snapshot.RecoveryGeneration {
				t.Fatalf("generation not advanced: %d", restored.Generation)
			}
			oldGeneration := restored.Generation
			restored.Generation = snapshot.Before.Generation
			if !reflect.DeepEqual(restored, snapshot.Before) {
				t.Fatal("initial ownership was not preserved")
			}
			if err := compensateRuntimeGlobal(app, snapshot); err != nil {
				t.Fatal(err)
			}
			replayed, _ := app.loadGlobalProxyJournal()
			if replayed.Generation != oldGeneration {
				t.Fatal("completed compensation rewrote journal")
			}
		})
	}
}

func TestRuntimeResourceGlobalNewOwnershipAndAdministratorConflict(t *testing.T) {
	app := testApp(t)
	profile, apt := withGlobalProxyTestPaths(t, app)
	snapshot, err := captureRuntimeGlobal(app, app, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.applyGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	if err := compensateRuntimeGlobal(app, snapshot); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{profile, apt} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("candidate artifact not removed")
		}
	}
	assertGlobalProxyJournalCopiesAbsent(t, app)
	if err := app.applyGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	candidateAPT, err := os.ReadFile(apt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profile, []byte("operator concurrent change"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := compensateRuntimeGlobal(app, snapshot); err == nil {
		t.Fatal("administrator divergence accepted")
	}
	actualAPT, _ := os.ReadFile(apt)
	if !bytes.Equal(candidateAPT, actualAPT) {
		t.Fatal("compensation modified peer before validating pair")
	}
	if _, err := app.loadGlobalProxyJournal(); err != nil {
		t.Fatal("failed compensation discarded ownership")
	}
}

func TestRuntimeResourceHermesRecoveryRetainsPendingAcrossServiceFailure(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	target := h.target()
	st := newStore()
	if _, err := h.app.applyTelegram(st, []systemdTargetName{target}); err != nil {
		t.Fatal(err)
	}
	cfg := h.app.cfg
	cfg.TGHTTPPort += 1000
	candidate := NewApp(cfg)
	plan, err := candidate.planTelegramSelection(st, []systemdTargetName{target}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := captureRuntimeTelegram(h.app, candidate, st, st, plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := candidate.applyTelegramPlan(st, plan); err != nil {
		t.Fatal(err)
	}
	calls := 0
	fail := true
	systemctlRun = func(_ string, args ...string) error {
		calls++
		if fail && len(args) > 0 && args[0] == "try-restart" {
			return errors.New("restart failed")
		}
		return nil
	}
	if err := compensateRuntimeTelegram(h.app, snapshot); err == nil {
		t.Fatal("service failure ignored")
	}
	entry := h.journal(t).Targets[canonicalTelegramTargetName(target)]
	if entry == nil || entry.Phase != telegramPhasePrepared {
		t.Fatal("service pending evidence lost")
	}
	raw, _ := os.ReadFile(h.systemPath)
	if !bytes.Equal(raw, snapshot.Hermes[0].Content) {
		t.Fatal("transaction before file not restored")
	}
	fail = false
	if err := compensateRuntimeTelegram(h.app, snapshot); err != nil {
		t.Fatal(err)
	}
	beforeCalls := calls
	if err := compensateRuntimeTelegram(h.app, snapshot); err != nil {
		t.Fatal(err)
	}
	if calls != beforeCalls {
		t.Fatal("completed recovery restarted unchanged service")
	}
	if !reflect.DeepEqual(h.journal(t).Targets[canonicalTelegramTargetName(target)], snapshot.Hermes[0].Before) {
		t.Fatal("semantic ownership not restored")
	}
}

func TestRuntimeResourceHermesReleasedOwnershipReconstructedWithNewGeneration(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	target := h.target()
	st := newStore()
	if _, err := h.app.applyTelegram(st, []systemdTargetName{target}); err != nil {
		t.Fatal(err)
	}
	plan, err := h.app.planTelegram(st, false)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := captureRuntimeTelegram(h.app, h.app, st, st, plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.app.restoreTelegramPlan(st, plan); err != nil {
		t.Fatal(err)
	}
	releasedGeneration := h.journal(t).Generation
	if err := compensateRuntimeTelegram(h.app, snapshot); err != nil {
		t.Fatal(err)
	}
	if h.journal(t).Generation <= releasedGeneration {
		t.Fatal("released ownership generation moved backwards")
	}
	if !reflect.DeepEqual(h.journal(t).Targets[canonicalTelegramTargetName(target)], snapshot.Hermes[0].Before) {
		t.Fatal("released ownership not reconstructed")
	}
}

func TestRuntimeResourceOpenClawPreservesOtherFieldsAndRetriesFixedTargets(t *testing.T) {
	h := newOpenClawTestHarness(t, `{"channels":{"telegram":{"botToken":"secret"}},"other":1}`)
	h.app.cfg.ManageOpenClawConfig = true
	first := h.target("openclaw-first.service")
	second := h.target("openclaw-second.service")
	h.applyAndCommit(t, first, h.app.cfg.HTTPAddr(SceneTelegram))
	h.applyAndCommit(t, second, h.app.cfg.HTTPAddr(SceneTelegram))
	oldExists := telegramUserUnitExists
	oldCtl := userSystemctlRun
	t.Cleanup(func() { telegramUserUnitExists = oldExists; userSystemctlRun = oldCtl })
	telegramUserUnitExists = func(string, string) bool { return true }
	userSystemctlRun = func(string, *persistedUserIdentity, string, ...string) error { return nil }
	cfg := h.app.cfg
	cfg.TGHTTPPort += 1000
	candidate := NewApp(cfg)
	st := newStore()
	plan, err := candidate.planTelegramSelection(st, []systemdTargetName{first, second}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := captureRuntimeTelegram(h.app, candidate, st, st, plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := candidate.applyOpenClawTelegramProxy(first, cfg.HTTPAddr(SceneTelegram)); err != nil {
		t.Fatal(err)
	}
	raw := bytes.Replace(h.config(t), []byte(`"other":1`), []byte(`"other":2`), 1)
	if err := os.WriteFile(h.configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var restarted []string
	fail := true
	userSystemctlRun = func(_ string, _ *persistedUserIdentity, _ string, args ...string) error {
		if len(args) > 0 && args[0] == "try-restart" {
			key := args[len(args)-1]
			restarted = append(restarted, key)
			if key == second.Service && fail {
				return errors.New("second restart failed")
			}
		}
		return nil
	}
	if err := compensateRuntimeTelegram(h.app, snapshot); err == nil {
		t.Fatal("restart failure ignored")
	}
	if !bytes.Contains(h.config(t), []byte(`"other":2`)) {
		t.Fatal("unrelated configuration overwritten")
	}
	entry := h.journal(t).Users[h.user]
	if entry.Phase != openClawPhasePrepared || !slices.Equal(entry.PendingTargets, []string{canonicalTelegramTargetName(second)}) {
		t.Fatalf("pending target progress lost: %+v", entry)
	}
	fail = false
	restarted = nil
	if err := compensateRuntimeTelegram(h.app, snapshot); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(restarted, []string{second.Service}) {
		t.Fatalf("recovery expanded or repeated fixed targets: %v", restarted)
	}
	if !reflect.DeepEqual(h.journal(t).Users[h.user], snapshot.OpenClaw[0].Before) {
		t.Fatal("OpenClaw before ownership not restored")
	}
}

func TestRuntimeResourceDevRestoresBeforeValuesAndRetainsInitialOwnership(t *testing.T) {
	requireDevGit(t)
	user, identity := realDevConfigFixture(t)
	app := testApp(t)
	app.cfg.DevTargetUser = user
	devCommandExists = func(name string) bool { return name == "git" || name == "npm" }
	devIOWrite(t, filepath.Join(identity.Home, ".gitconfig"), "[http]\nproxy = original-one\nproxy = original-two\n[https]\nproxy = original-https\n")
	devIOWrite(t, filepath.Join(identity.Home, ".npmrc"), "proxy=http://original.invalid:1234\nregistry=https://registry.npmjs.org/\n")
	if err := app.applyDev(); err != nil {
		t.Fatal(err)
	}
	cfg := app.cfg
	cfg.DevHTTPPort += 1000
	candidate := NewApp(cfg)
	snapshot, err := captureRuntimeDev(app, candidate, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := candidate.applyDev(); err != nil {
		t.Fatal(err)
	}
	if err := compensateRuntimeDev(app, snapshot); err != nil {
		t.Fatal(err)
	}
	values, err := snapshotDevProxyConfig(user, snapshot.Users[0].Values, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(values, snapshot.Users[0].Values) {
		t.Fatalf("actual before values were not restored: %+v", values)
	}
	backup, err := app.loadDevBackup()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(backup, snapshot.Before) {
		t.Fatal("initial takeover backup not preserved")
	}
	if err := app.restoreDev(); err != nil {
		t.Fatal(err)
	}
	if err := compensateRuntimeDev(app, snapshot); err != nil {
		t.Fatal(err)
	}
	values, err = snapshotDevProxyConfig(user, snapshot.Users[0].Values, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(values, snapshot.Users[0].Values) {
		t.Fatal("released original values were not compensated to transaction before")
	}
}

func TestRuntimeResourceLegacyServicePendingSurvivesCheckpoint(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	item := runtimeLegacyResource{Target: canonicalTelegramTargetName(h.target()), Present: true, Content: []byte("[Service]\nEnvironmentFile=-/etc/openclaw-hermes-tg-proxy.env\n")}
	var durable runtimeLegacyResource
	checkpoints := 0
	checkpoint := func() error { checkpoints++; durable = item; return nil }
	systemctlRun = func(string, ...string) error { return errors.New("reload unavailable") }
	if err := compensateRuntimeLegacy(h.app, &item, checkpoint); err == nil {
		t.Fatal("service error ignored")
	}
	if !durable.RecoveryPending || checkpoints != 1 {
		t.Fatal("service pending was not durable before file restore")
	}
	item = durable
	calls := 0
	systemctlRun = func(string, ...string) error { calls++; return nil }
	if err := compensateRuntimeLegacy(h.app, &item, checkpoint); err != nil {
		t.Fatal(err)
	}
	if calls == 0 || durable.RecoveryPending {
		t.Fatal("service pending did not finish")
	}
}

func TestRuntimeResourceDevCompensationReplaysUnrecordedGitMutation(t *testing.T) {
	requireDevGit(t)
	user, identity := realDevConfigFixture(t)
	app := testApp(t)
	app.cfg.DevTargetUser = user
	devCommandExists = func(name string) bool { return name == "git" }
	devIOWrite(t, filepath.Join(identity.Home, ".gitconfig"), "[http]\nproxy = original\n[https]\nproxy = original-https\n")
	if err := app.applyDev(); err != nil {
		t.Fatal(err)
	}
	cfg := app.cfg
	cfg.DevHTTPPort += 1000
	candidate := NewApp(cfg)
	snapshot, err := captureRuntimeDev(app, candidate, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := candidate.applyDev(); err != nil {
		t.Fatal(err)
	}
	originalMutate := devMutateGitConfig
	fail := true
	devMutateGitConfig = func(user string, id *persistedUserIdentity, location devGitConfigLocation, expected []string, args ...string) error {
		err := originalMutate(user, id, location, expected, args...)
		if err == nil && fail {
			fail = false
			return errors.New("process stopped after durable Git mutation")
		}
		return err
	}
	if err := compensateRuntimeDev(app, snapshot); err == nil {
		t.Fatal("simulated crash ignored")
	}
	pending, err := app.loadDevBackup()
	if err != nil {
		t.Fatal(err)
	}
	if pending.ApplyRollback == nil || pending.ApplyRollback.GitHTTPRestore == nil || pending.ApplyRollback.GitHTTPRestore.Next != 0 {
		t.Fatalf("CAS progress evidence lost: %+v", pending.ApplyRollback)
	}
	if err := compensateRuntimeDev(app, snapshot); err != nil {
		t.Fatal(err)
	}
	actual, err := snapshotDevProxyConfig(user, snapshot.Users[0].Values, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, snapshot.Users[0].Values) {
		t.Fatal("crash replay did not restore exact before-values")
	}
}

func TestRuntimeResourceDevUserTransitionRestoresBothIdentities(t *testing.T) {
	_, _ = realDevConfigFixture(t)
	app := testApp(t)
	app.cfg.DevTargetUser = "dev-old"
	homes := map[string]string{"dev-old": t.TempDir(), "dev-new": t.TempDir()}
	devLookupUserIdentity = func(name string) (localUserIdentity, error) {
		home, ok := homes[name]
		if !ok {
			return localUserIdentity{}, errors.New("unexpected user")
		}
		return localUserIdentity{Name: name, UID: os.Geteuid(), GID: os.Getegid(), Home: home}, nil
	}
	devCommandExists = func(name string) bool { return name == "npm" }
	devIOWrite(t, filepath.Join(homes["dev-old"], ".npmrc"), "proxy=http://old-original.invalid:1234\n")
	newOriginal := []byte("proxy=http://new-original.invalid:1234\nregistry=https://registry.npmjs.org/\n")
	devIOWrite(t, filepath.Join(homes["dev-new"], ".npmrc"), string(newOriginal))
	if err := app.applyDev(); err != nil {
		t.Fatal(err)
	}
	cfg := app.cfg
	cfg.DevTargetUser = "dev-new"
	cfg.DevHTTPPort += 1000
	candidate := NewApp(cfg)
	snapshot, err := captureRuntimeDev(app, candidate, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Users) != 2 {
		t.Fatal("both fixed identities not captured")
	}
	if err := candidate.applyDev(); err != nil {
		t.Fatal(err)
	}
	if err := compensateRuntimeDev(app, snapshot); err != nil {
		t.Fatal(err)
	}
	for _, item := range snapshot.Users {
		actual, err := snapshotDevProxyConfig(item.Values.User, item.Values, false, true)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(actual, item.Values) {
			t.Fatalf("identity %s not restored", item.Values.User)
		}
	}
	backup, err := app.loadDevBackup()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(backup, snapshot.Before) {
		t.Fatal("original identity ownership not restored")
	}
}

func TestRuntimeResourceDevPendingForwardReleaseCompletesBeforeInverse(t *testing.T) {
	requireDevGit(t)
	user, identity := realDevConfigFixture(t)
	app := testApp(t)
	app.cfg.DevTargetUser = user
	devCommandExists = func(name string) bool { return name == "git" }
	devIOWrite(t, filepath.Join(identity.Home, ".gitconfig"), "[http]\nproxy = original-one\nproxy = original-two\n[https]\nproxy = original-https\n")
	if err := app.applyDev(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := captureRuntimeDev(app, app, false)
	if err != nil {
		t.Fatal(err)
	}
	originalMutate := devMutateGitConfig
	fail := true
	devMutateGitConfig = func(user string, id *persistedUserIdentity, location devGitConfigLocation, expected []string, args ...string) error {
		err := originalMutate(user, id, location, expected, args...)
		if err == nil && fail {
			fail = false
			return errors.New("crash during forward release")
		}
		return err
	}
	if err := app.restoreDev(); err == nil {
		t.Fatal("forward crash ignored")
	}
	beforeReplay, err := app.loadDevBackup()
	if err != nil {
		t.Fatal(err)
	}
	if beforeReplay.GitHTTPRestore == nil || beforeReplay.GitHTTPRestore.Next != 0 {
		t.Fatal("forward progress missing")
	}
	if err := compensateRuntimeDev(app, snapshot); err != nil {
		t.Fatal(err)
	}
	after, err := app.loadDevBackup()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, snapshot.Before) {
		t.Fatal("forward release inverse changed baseline")
	}
	actual, err := snapshotDevProxyConfig(user, snapshot.Users[0].Values, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, snapshot.Users[0].Values) {
		t.Fatal("managed before-image was not restored")
	}
}

func TestRuntimeResourceHermesQuarantinedCASCrashRestoresBeforeFile(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	target := h.target()
	st := newStore()
	if _, err := h.app.applyTelegram(st, []systemdTargetName{target}); err != nil {
		t.Fatal(err)
	}
	cfg := h.app.cfg
	cfg.TGHTTPPort += 1000
	candidate := NewApp(cfg)
	plan, err := candidate.planTelegramSelection(st, []systemdTargetName{target}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := captureRuntimeTelegram(h.app, candidate, st, st, plan)
	if err != nil {
		t.Fatal(err)
	}
	retained := filepath.Join(filepath.Dir(h.systemPath), telegramSystemQuarantineName(filepath.Base(h.systemPath)))
	if err := os.Rename(h.systemPath, retained); err != nil {
		t.Fatal(err)
	}
	if err := compensateRuntimeTelegram(h.app, snapshot); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(h.systemPath)
	if err != nil || !bytes.Equal(raw, snapshot.Hermes[0].Content) {
		t.Fatalf("quarantined before not recovered: %v", err)
	}
	if _, err := os.Stat(retained); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("quarantine was not consumed")
	}
}

func TestRuntimeResourceSnapshotRejectsUnboundAfterImages(t *testing.T) {
	app := testApp(t)
	withGlobalProxyTestPaths(t, app)
	resources, err := captureRuntimeResources(app, app, newStore(), newStore(), []Scene{SceneGlobal}, nil)
	if err != nil {
		t.Fatal(err)
	}
	resources.Global.AllowedAfter[0] = append(resources.Global.AllowedAfter[0], runtimeArtifactState{Present: true, Content: []byte("arbitrary executable shell"), Mode: 0644})
	if err := validateRuntimeResources(resources); err == nil {
		t.Fatal("arbitrary after-image accepted")
	}
}

func TestRuntimeResourceHermesAbsentReleaseDoesNotRestartReplacement(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	target := h.target()
	st := newStore()
	if _, err := h.app.applyTelegram(st, []systemdTargetName{target}); err != nil {
		t.Fatal(err)
	}
	telegramInspectPlanUnit = func(systemdTargetName, *persistedUserIdentity) (telegramPlanUnit, error) {
		return telegramPlanUnit{resolution: telegramUnitResolution{State: telegramUnitAbsent}}, nil
	}
	telegramSystemUnitExists = func(string) bool { return false }
	stubTelegramServiceState(t, telegramServiceState{LoadState: "not-found", ActiveState: "inactive"})
	plan, err := h.app.planTelegram(st, false)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := captureRuntimeTelegram(h.app, h.app, st, st, plan)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Hermes[0].SkipRestart {
		t.Fatal("absence not retained in fixed recovery plan")
	}
	if err := h.app.restoreTelegramPlan(st, plan); err != nil {
		t.Fatal(err)
	}
	telegramSystemUnitExists = func(string) bool { return true }
	telegramValidateHermesTarget = func(systemdTargetName, *persistedUserIdentity, string) error {
		return errors.New("replacement service must not be inspected or restarted")
	}
	systemctlRun = func(_ string, args ...string) error {
		if len(args) > 0 && args[0] == "try-restart" {
			t.Fatal("replacement restarted")
		}
		return nil
	}
	if err := compensateRuntimeTelegram(h.app, snapshot); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(h.journal(t).Targets[canonicalTelegramTargetName(target)], snapshot.Hermes[0].Before) {
		t.Fatal("absent target ownership not restored")
	}
}

func TestRuntimeResourceOpenClawAbsentReleaseRestoresWithoutReplacementRestart(t *testing.T) {
	h := newOpenClawTestHarness(t, `{"channels":{"telegram":{"proxy":null}},"keep":true}`)
	h.app.cfg.ManageOpenClawConfig = true
	target := h.target("openclaw-absent.service")
	h.applyAndCommit(t, target, h.app.cfg.HTTPAddr(SceneTelegram))
	oldExists := telegramUserUnitExists
	oldCtl := userSystemctlRun
	t.Cleanup(func() { telegramUserUnitExists = oldExists; userSystemctlRun = oldCtl })
	telegramUserUnitExists = func(string, string) bool { return false }
	userSystemctlRun = func(string, *persistedUserIdentity, string, ...string) error { return nil }
	telegramInspectPlanUnit = func(systemdTargetName, *persistedUserIdentity) (telegramPlanUnit, error) {
		return telegramPlanUnit{resolution: telegramUnitResolution{State: telegramUnitAbsent}}, nil
	}
	st := newStore()
	stubTelegramServiceState(t, telegramServiceState{LoadState: "not-found", ActiveState: "inactive"})
	plan, err := h.app.planTelegram(st, false)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := captureRuntimeTelegram(h.app, h.app, st, st, plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.app.restoreTelegramPlan(st, plan); err != nil {
		t.Fatal(err)
	}
	telegramUserUnitExists = func(string, string) bool { return true }
	openClawValidateTargetRuntime = func(systemdTargetName, *persistedUserIdentity) error {
		return errors.New("replacement should not be validated")
	}
	userSystemctlRun = func(string, *persistedUserIdentity, string, ...string) error {
		t.Fatal("replacement user manager was operated")
		return nil
	}
	if err := compensateRuntimeTelegram(h.app, snapshot); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(h.journal(t).Users[h.user], snapshot.OpenClaw[0].Before) {
		t.Fatal("absent OpenClaw before ownership not restored")
	}
}
