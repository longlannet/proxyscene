package manager

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

type telegramJournalTestHarness struct {
	app        *App
	dir        string
	systemPath string
	legacyPath string
	envPath    string
	identity   localUserIdentity
}

func newTelegramJournalTestHarness(t *testing.T) *telegramJournalTestHarness {
	t.Helper()
	stubTelegramPlanUnitsForLifecycle(t)
	h := &telegramJournalTestHarness{app: testApp(t), dir: t.TempDir()}
	stubTelegramServiceState(t, telegramServiceState{LoadState: "loaded", ActiveState: "active"})
	oldRestartPolicy := telegramValidateHermesRestartPolicy
	oldReleaseRestartPolicy := telegramValidateHermesReleaseRestartPolicy
	t.Cleanup(func() {
		telegramValidateHermesRestartPolicy = oldRestartPolicy
		telegramValidateHermesReleaseRestartPolicy = oldReleaseRestartPolicy
	})
	telegramValidateHermesRestartPolicy = func(systemdTargetName, *persistedUserIdentity) error { return nil }
	telegramValidateHermesReleaseRestartPolicy = func(target systemdTargetName, identity *persistedUserIdentity) error {
		return telegramValidateHermesRestartPolicy(target, identity)
	}
	oldReloadState := openClawReloadUnitState
	t.Cleanup(func() { openClawReloadUnitState = oldReloadState })
	openClawReloadUnitState = func(systemdTargetName, *persistedUserIdentity) (string, error) {
		return "", errors.New("fixture has no gateway RPC")
	}

	h.systemPath = filepath.Join(h.dir, "hermes-journal-test.service.d", telegramManagedDropInName)
	h.legacyPath = filepath.Join(h.dir, "legacy-system.conf")
	h.envPath = filepath.Join(h.dir, "legacy.env")
	h.identity = localUserIdentity{Name: "root", UID: os.Getuid(), GID: os.Getgid(), UIDText: strconv.Itoa(os.Getuid()), GIDText: strconv.Itoa(os.Getgid()), Home: h.dir}
	if err := os.MkdirAll(filepath.Dir(h.systemPath), 0o755); err != nil {
		t.Fatal(err)
	}

	oldManagedSystemPath := telegramManagedSystemPath
	oldManagedUserPath := telegramManagedUserPath
	oldLegacySystemPath := telegramLegacySystemPath
	oldLegacyEnvPath := telegramLegacyEnvPath
	oldReadSystem := telegramReadSystemArtifact
	oldReadUser := telegramReadUserArtifact
	oldCreateSystem := telegramCreateSystemArtifact
	oldReplaceSystem := telegramReplaceSystemArtifact
	oldCreateUser := telegramCreateUserArtifact
	oldReplaceUser := telegramReplaceUserArtifact
	oldRemoveSystem := telegramRemoveSystemArtifact
	oldRemoveUser := telegramRemoveUserArtifact
	oldConfirmUserAbsent := telegramConfirmUserAbsent
	oldSystemExists := telegramSystemUnitExists
	oldUserExists := telegramUserUnitExists
	oldSystemctl := systemctlRun
	oldUserSystemctl := userSystemctlRun
	oldLookup := telegramLookupUserIdentity
	oldSystemFsync := telegramSystemDirFsync
	oldSystemCASAfterQuarantine := telegramSystemCASAfterQuarantine
	oldRuntimeValidator := telegramValidateHermesTarget
	oldReleaseValidator := telegramValidateHermesReleaseTarget
	t.Cleanup(func() {
		telegramManagedSystemPath = oldManagedSystemPath
		telegramManagedUserPath = oldManagedUserPath
		telegramLegacySystemPath = oldLegacySystemPath
		telegramLegacyEnvPath = oldLegacyEnvPath
		telegramReadSystemArtifact = oldReadSystem
		telegramReadUserArtifact = oldReadUser
		telegramCreateSystemArtifact = oldCreateSystem
		telegramReplaceSystemArtifact = oldReplaceSystem
		telegramCreateUserArtifact = oldCreateUser
		telegramReplaceUserArtifact = oldReplaceUser
		telegramRemoveSystemArtifact = oldRemoveSystem
		telegramRemoveUserArtifact = oldRemoveUser
		telegramConfirmUserAbsent = oldConfirmUserAbsent
		telegramSystemUnitExists = oldSystemExists
		telegramUserUnitExists = oldUserExists
		systemctlRun = oldSystemctl
		userSystemctlRun = oldUserSystemctl
		telegramLookupUserIdentity = oldLookup
		telegramSystemDirFsync = oldSystemFsync
		telegramSystemCASAfterQuarantine = oldSystemCASAfterQuarantine
		telegramValidateHermesTarget = oldRuntimeValidator
		telegramValidateHermesReleaseTarget = oldReleaseValidator
	})

	telegramManagedSystemPath = func(Config, string) string { return h.systemPath }
	telegramManagedUserPath = func(_ Config, userName, _ string, service string) (string, error) {
		return filepath.Join(h.dir, "user-"+userName+"-"+service+".conf"), nil
	}
	telegramLegacySystemPath = func(Config, string) string { return h.legacyPath }
	telegramLegacyEnvPath = func() string { return h.envPath }
	telegramReadSystemArtifact = readRegularFileNoFollow
	telegramReadUserArtifact = func(_ string, path string, max int64) ([]byte, error) {
		return readRegularFileNoFollow(path, max)
	}
	telegramCreateSystemArtifact = writeTelegramSystemArtifactCreate
	telegramReplaceSystemArtifact = writeTelegramSystemArtifactCAS
	telegramCreateUserArtifact = func(_ string, _ *persistedUserIdentity, path string, data []byte, perm os.FileMode) error {
		return writeFileAtomic(path, data, perm)
	}
	telegramReplaceUserArtifact = func(_ string, _ *persistedUserIdentity, path string, _ []byte, data []byte, perm os.FileMode) error {
		return writeFileAtomic(path, data, perm)
	}
	telegramRemoveSystemArtifact = func(path string, _ [][]byte) (bool, error) {
		return removeFileForTest(path)
	}
	telegramRemoveUserArtifact = func(_ string, _ *persistedUserIdentity, path string, _ []byte) (bool, error) {
		return removeFileForTest(path)
	}
	telegramConfirmUserAbsent = func(string, *persistedUserIdentity, string) error { return nil }
	telegramSystemUnitExists = func(string) bool { return true }
	telegramUserUnitExists = func(string, string) bool { return true }
	systemctlRun = func(string, ...string) error { return nil }
	userSystemctlRun = func(string, *persistedUserIdentity, string, ...string) error { return nil }
	telegramLookupUserIdentity = func(user string) (localUserIdentity, error) {
		if user != h.identity.Name {
			return localUserIdentity{}, errors.New("unexpected test user")
		}
		return h.identity, nil
	}
	telegramSystemCASAfterQuarantine = func(string) {}
	telegramValidateHermesTarget = func(systemdTargetName, *persistedUserIdentity, string) error { return nil }
	telegramValidateHermesReleaseTarget = func(target systemdTargetName, identity *persistedUserIdentity, _ string) error {
		if err := telegramValidateHermesTarget(target, identity, ""); err != nil {
			return err
		}
		return validateHermesTelegramRestartSafety(target, identity)
	}
	return h
}

func (h *telegramJournalTestHarness) target() systemdTargetName {
	return systemdTargetName{Service: "hermes-journal-test.service"}
}

func (h *telegramJournalTestHarness) journal(t *testing.T) *telegramProxyJournal {
	t.Helper()
	journal, err := h.app.loadTelegramProxyJournal()
	if err != nil {
		t.Fatal(err)
	}
	return journal
}

func TestHermesJournalPreparedBeforeArtifactWriteAndReplaysCrash(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	target := h.target()
	oldWrite := telegramCreateSystemArtifact
	writes := 0
	telegramCreateSystemArtifact = func(path string, data []byte, perm os.FileMode) error {
		writes++
		if writes == 1 {
			return errors.New("simulated crash before artifact write")
		}
		return oldWrite(path, data, perm)
	}

	applied, err := h.app.applyTelegram(newStore(), []systemdTargetName{target})
	if err == nil || len(applied) != 1 {
		t.Fatalf("failed write must retain durable prepared ownership: applied=%v err=%v", applied, err)
	}
	entry := h.journal(t).Targets[canonicalTelegramTargetName(target)]
	if entry == nil || entry.Phase != telegramPhasePrepared {
		t.Fatalf("journal was not prepared before failed write: %+v", entry)
	}
	if _, err := os.Lstat(h.systemPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed artifact write unexpectedly created file: %v", err)
	}

	applied, err = h.app.applyTelegram(newStore(), []systemdTargetName{target})
	if err != nil || len(applied) != 1 {
		t.Fatalf("prepared crash replay failed: applied=%v err=%v", applied, err)
	}
	entry = h.journal(t).Targets[canonicalTelegramTargetName(target)]
	if entry == nil || entry.Phase != telegramPhaseActive || entry.PendingManagedContent != "" {
		t.Fatalf("replayed journal did not commit active: %+v", entry)
	}
}

func TestHermesCommitRuntimeValidatorReceivesExpectedProxy(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	var events []string
	telegramValidateHermesTarget = func(_ systemdTargetName, _ *persistedUserIdentity, expected string) error {
		events = append(events, "validate:"+expected)
		return nil
	}
	systemctlRun = func(_ string, args ...string) error {
		events = append(events, strings.Join(args, " "))
		return nil
	}
	if _, err := h.app.applyTelegram(newStore(), []systemdTargetName{h.target()}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"validate:", // read-only plan
		"validate:", // plan revalidation before execution
		"validate:", // artifact preparation
		"daemon-reload",
		"validate:http://127.0.0.1:7892",
		"try-restart -- hermes-journal-test.service",
		"validate:http://127.0.0.1:7892",
	}
	if !slices.Equal(events, want) {
		t.Fatalf("runtime validation order=%q, want %q", events, want)
	}
}

func TestHermesPreRestartValidationFailureSkipsRestart(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	validations := 0
	telegramValidateHermesTarget = func(_ systemdTargetName, _ *persistedUserIdentity, expected string) error {
		validations++
		if expected != "" {
			return errors.New("late drop-in overrides managed proxy")
		}
		return nil
	}
	reloads := 0
	restarts := 0
	systemctlRun = func(_ string, args ...string) error {
		if slices.Equal(args, []string{"daemon-reload"}) {
			reloads++
		}
		if len(args) > 0 && args[0] == "try-restart" {
			restarts++
		}
		return nil
	}

	applied, err := h.app.applyTelegram(newStore(), []systemdTargetName{h.target()})
	if err == nil || len(applied) != 1 {
		t.Fatalf("pre-restart validation failure must be reported: applied=%v err=%v", applied, err)
	}
	if validations != 4 || reloads != 1 || restarts != 0 {
		t.Fatalf("validation/reload/restart counts=%d/%d/%d", validations, reloads, restarts)
	}
	entry := h.journal(t).Targets[canonicalTelegramTargetName(h.target())]
	if entry == nil || entry.Phase != telegramPhasePrepared {
		t.Fatalf("failed pre-restart validation lost prepared ownership: %+v", entry)
	}
}

func TestTelegramSystemArtifactCreateDoesNotReplaceConcurrentFile(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	operator := []byte("operator owns the final name\n")
	realCreate := telegramCreateSystemArtifact
	telegramCreateSystemArtifact = func(path string, data []byte, perm os.FileMode) error {
		if err := os.WriteFile(path, operator, perm); err != nil {
			return err
		}
		return realCreate(path, data, perm)
	}

	applied, err := h.app.applyTelegram(newStore(), []systemdTargetName{h.target()})
	if !errors.Is(err, errTelegramSystemArtifactChanged) || len(applied) != 1 {
		t.Fatalf("concurrent create error=%v applied=%v", err, applied)
	}
	got, readErr := os.ReadFile(h.systemPath)
	if readErr != nil || !slices.Equal(got, operator) {
		t.Fatalf("concurrent operator file was replaced: got=%q err=%v", got, readErr)
	}
}

func TestTelegramSystemArtifactCASConcurrentWriterWinsFinalName(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	expected := []byte("managed old\n")
	desired := []byte("managed new\n")
	operator := []byte("operator concurrent value\n")
	if err := os.WriteFile(h.systemPath, expected, 0o644); err != nil {
		t.Fatal(err)
	}
	telegramSystemCASAfterQuarantine = func(path string) {
		if err := os.WriteFile(path, operator, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	err := writeTelegramSystemArtifactCAS(h.systemPath, expected, desired, 0o644)
	if !errors.Is(err, errTelegramSystemArtifactChanged) {
		t.Fatalf("CAS race error=%v", err)
	}
	got, readErr := os.ReadFile(h.systemPath)
	if readErr != nil || !slices.Equal(got, operator) {
		t.Fatalf("CAS overwrote concurrent operator file: got=%q err=%v", got, readErr)
	}
	quarantine := filepath.Join(filepath.Dir(h.systemPath), telegramSystemQuarantineName(filepath.Base(h.systemPath)))
	if _, statErr := os.Lstat(quarantine); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("owned quarantine remained after concurrent writer won: %v", statErr)
	}
}

func TestTelegramSystemArtifactCASReplaysClaimedFileAfterCrash(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	expected := []byte("managed old\n")
	desired := []byte("managed new\n")
	if err := os.WriteFile(h.systemPath, expected, 0o644); err != nil {
		t.Fatal(err)
	}
	quarantine := filepath.Join(filepath.Dir(h.systemPath), telegramSystemQuarantineName(filepath.Base(h.systemPath)))
	if err := os.Rename(h.systemPath, quarantine); err != nil {
		t.Fatal(err)
	}

	if err := writeTelegramSystemArtifactCAS(h.systemPath, expected, desired, 0o644); err != nil {
		t.Fatalf("crash replay failed: %v", err)
	}
	got, readErr := os.ReadFile(h.systemPath)
	if readErr != nil || !slices.Equal(got, desired) {
		t.Fatalf("crash replay content=%q err=%v", got, readErr)
	}
	if _, statErr := os.Lstat(quarantine); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("crash replay quarantine remained: %v", statErr)
	}
}

func TestTelegramSystemArtifactRemoveCASPreservesConcurrentReplacement(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	expected := []byte("managed old\n")
	operator := []byte("operator concurrent value\n")
	if err := os.WriteFile(h.systemPath, expected, 0o644); err != nil {
		t.Fatal(err)
	}
	telegramSystemCASAfterQuarantine = func(path string) {
		if err := os.WriteFile(path, operator, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := removeSystemFileAndEmptyParentCAS(h.systemPath, [][]byte{expected})
	if err != nil || !removed {
		t.Fatalf("remove CAS=(%v,%v), want (true,nil)", removed, err)
	}
	got, readErr := os.ReadFile(h.systemPath)
	if readErr != nil || !slices.Equal(got, operator) {
		t.Fatalf("remove CAS changed concurrent final name: got=%q err=%v", got, readErr)
	}
	quarantine := filepath.Join(filepath.Dir(h.systemPath), telegramSystemQuarantineName(filepath.Base(h.systemPath)))
	if _, statErr := os.Lstat(quarantine); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("confirmed managed quarantine remained: %v", statErr)
	}
}

func TestTelegramSystemArtifactRemoveCASReplaysClaimedFileAfterCrash(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	expected := []byte("managed old\n")
	if err := os.WriteFile(h.systemPath, expected, 0o644); err != nil {
		t.Fatal(err)
	}
	quarantine := filepath.Join(filepath.Dir(h.systemPath), telegramSystemQuarantineName(filepath.Base(h.systemPath)))
	if err := os.Rename(h.systemPath, quarantine); err != nil {
		t.Fatal(err)
	}

	removed, err := removeSystemFileAndEmptyParentCAS(h.systemPath, [][]byte{expected})
	if err != nil || !removed {
		t.Fatalf("remove replay=(%v,%v), want (true,nil)", removed, err)
	}
	if _, statErr := os.Lstat(h.systemPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("replayed final name exists: %v", statErr)
	}
	if _, statErr := os.Lstat(quarantine); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("replayed quarantine exists: %v", statErr)
	}
}

func TestHermesCleanupSystemArtifactCASRejectsPostReadReplacement(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	target := h.target()
	if _, err := h.app.applyTelegram(newStore(), []systemdTargetName{target}); err != nil {
		t.Fatal(err)
	}
	operator := []byte("[Service]\nEnvironment=\"TELEGRAM_PROXY=http://operator.invalid:9999\"\n")
	telegramRemoveSystemArtifact = func(path string, expected [][]byte) (bool, error) {
		if err := os.WriteFile(path, operator, 0o644); err != nil {
			return false, err
		}
		return removeSystemFileAndEmptyParentCAS(path, expected)
	}

	err := h.app.restoreTelegram(newStore())
	if !errors.Is(err, errTelegramSystemArtifactChanged) {
		t.Fatalf("post-read replacement error=%v, want changed", err)
	}
	got, readErr := os.ReadFile(h.systemPath)
	if readErr != nil || !slices.Equal(got, operator) {
		t.Fatalf("post-read operator file changed: got=%q err=%v", got, readErr)
	}
	entry := h.journal(t).Targets[canonicalTelegramTargetName(target)]
	if entry == nil || entry.Phase != telegramPhaseRestoring {
		t.Fatalf("failed CAS lost restoring ownership: %+v", entry)
	}
}

func TestHermesCleanupRejectsOperatorReplacedUnitBeforeRemovingDropIn(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	target := h.target()
	if _, err := h.app.applyTelegram(newStore(), []systemdTargetName{target}); err != nil {
		t.Fatal(err)
	}
	wantContent, err := os.ReadFile(h.systemPath)
	if err != nil {
		t.Fatal(err)
	}
	telegramValidateHermesTarget = func(systemdTargetName, *persistedUserIdentity, string) error {
		return errors.New("operator replaced Hermes unit")
	}
	systemctlCalls := 0
	systemctlRun = func(string, ...string) error {
		systemctlCalls++
		return nil
	}

	if err := h.app.cleanupHermesTelegramTarget(target); err == nil {
		t.Fatal("operator-replaced Hermes unit was accepted during cleanup")
	}
	gotContent, err := os.ReadFile(h.systemPath)
	if err != nil || !slices.Equal(gotContent, wantContent) {
		t.Fatalf("failed validation changed managed drop-in: got=%q err=%v", gotContent, err)
	}
	entry := h.journal(t).Targets[canonicalTelegramTargetName(target)]
	if entry == nil || entry.Phase != telegramPhaseActive {
		t.Fatalf("failed validation changed Hermes ownership: %+v", entry)
	}
	if systemctlCalls != 0 {
		t.Fatalf("failed pre-validation touched systemd: calls=%d", systemctlCalls)
	}
}

func TestHermesCleanupPostReloadValidationFailureRestoresDropIn(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	target := h.target()
	if _, err := h.app.applyTelegram(newStore(), []systemdTargetName{target}); err != nil {
		t.Fatal(err)
	}
	wantContent, err := os.ReadFile(h.systemPath)
	if err != nil {
		t.Fatal(err)
	}
	validations := 0
	telegramValidateHermesTarget = func(systemdTargetName, *persistedUserIdentity, string) error {
		validations++
		if validations == 2 {
			return errors.New("unit changed after daemon-reload")
		}
		return nil
	}
	reloads := 0
	restarts := 0
	systemctlRun = func(_ string, args ...string) error {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "daemon-reload") {
			reloads++
		}
		if strings.Contains(joined, "try-restart") {
			restarts++
		}
		return nil
	}

	if err := h.app.cleanupHermesTelegramTarget(target); err == nil {
		t.Fatal("post-reload Hermes validation failure was ignored")
	}
	gotContent, err := os.ReadFile(h.systemPath)
	if err != nil || !slices.Equal(gotContent, wantContent) {
		t.Fatalf("failed post-validation did not restore drop-in: got=%q err=%v", gotContent, err)
	}
	entry := h.journal(t).Targets[canonicalTelegramTargetName(target)]
	if entry == nil || entry.Phase != telegramPhaseRestoring {
		t.Fatalf("failed post-validation lost retry journal: %+v", entry)
	}
	if reloads != 2 || restarts != 0 {
		t.Fatalf("post-validation rollback calls: reload=%d restart=%d", reloads, restarts)
	}
}

func TestTelegramProxyFromManagedContent(t *testing.T) {
	proxy, err := telegramProxyFromManagedContent("[Service]\nEnvironment=\"TELEGRAM_PROXY=http://127.0.0.1:7892\"\n")
	if err != nil || proxy != "http://127.0.0.1:7892" {
		t.Fatalf("proxy=%q err=%v", proxy, err)
	}
	if _, err := telegramProxyFromManagedContent("[Service]\nEnvironment=\"OTHER=value\"\n"); err == nil {
		t.Fatal("invalid managed content was accepted")
	}
}

func TestTelegramJournalRejectsEqualGenerationSplitBrain(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	target := h.target()
	if _, err := h.app.applyTelegram(newStore(), []systemdTargetName{target}); err != nil {
		t.Fatal(err)
	}
	backup, err := readTelegramJournalFile(h.app.telegramProxyJournalBackupPath())
	if err != nil {
		t.Fatal(err)
	}
	entry := backup.Targets[canonicalTelegramTargetName(target)]
	entry.ManagedContent = "[Service]\nEnvironment=\"TELEGRAM_PROXY=http://127.0.0.1:17992\"\n"
	raw, err := json.MarshalIndent(backup, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(h.app.telegramProxyJournalBackupPath(), append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.loadTelegramProxyJournal(); err == nil || !strings.Contains(err.Error(), "同一 generation 内容不一致") {
		t.Fatalf("equal-generation split brain was accepted: %v", err)
	}
}

func TestTelegramJournalRejectsLegacyUserEntryWithoutIdentity(t *testing.T) {
	target := systemdTargetName{UserMode: true, User: "root", Service: "hermes-legacy.service"}
	key := canonicalTelegramTargetName(target)
	journal := newTelegramProxyJournal()
	journal.Targets[key] = &telegramProxyJournalEntry{
		Target:         key,
		Artifact:       telegramArtifactUserDropIn,
		Phase:          telegramPhaseActive,
		ManagedContent: "[Service]\nEnvironment=\"TELEGRAM_PROXY=http://127.0.0.1:7892\"\n",
	}
	raw, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeTelegramProxyJournal(raw); err == nil || !strings.Contains(err.Error(), "缺少 uid/gid/home") {
		t.Fatalf("legacy user journal did not fail closed: %v", err)
	}
}

func TestHermesCleanupPreservesModifiedArtifactThenReleasesOwnership(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	target := h.target()
	if _, err := h.app.applyTelegram(newStore(), []systemdTargetName{target}); err != nil {
		t.Fatal(err)
	}
	operatorContent := []byte("[Service]\nEnvironment=\"TELEGRAM_PROXY=http://operator.invalid:9999\"\n")
	if err := os.WriteFile(h.systemPath, operatorContent, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.app.restoreTelegram(newStore()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(h.systemPath)
	if err != nil || string(got) != string(operatorContent) {
		t.Fatalf("operator-modified artifact was not preserved: got=%q err=%v", got, err)
	}
	if len(h.journal(t).Targets) != 0 {
		t.Fatalf("ownership remained after reload/restart: %+v", h.journal(t).Targets)
	}
}

func TestRestoreTelegramWithoutOwnershipDoesNotDeleteFiles(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	for path, data := range map[string]string{
		h.systemPath: "operator managed path",
		h.legacyPath: "operator legacy path",
		h.envPath:    "operator shared env",
	} {
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.app.restoreTelegram(newStore()); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		h.systemPath: "operator managed path",
		h.legacyPath: "operator legacy path",
		h.envPath:    "operator shared env",
	} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("unowned file changed: path=%s got=%q err=%v", path, got, err)
		}
	}
}

func TestHermesApplyWithoutOwnershipRefusesExistingManagedPath(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	operatorContent := []byte("operator owns this path")
	if err := os.WriteFile(h.systemPath, operatorContent, 0o644); err != nil {
		t.Fatal(err)
	}
	applied, err := h.app.applyTelegram(newStore(), []systemdTargetName{h.target()})
	if err == nil || len(applied) != 0 {
		t.Fatalf("unowned existing path must be rejected: applied=%v err=%v", applied, err)
	}
	got, readErr := os.ReadFile(h.systemPath)
	if readErr != nil || string(got) != string(operatorContent) {
		t.Fatalf("unowned path changed: got=%q err=%v", got, readErr)
	}
	if len(h.journal(t).Targets) != 0 {
		t.Fatalf("refused path unexpectedly gained ownership: %+v", h.journal(t).Targets)
	}
}

func TestHermesCleanupRetainsRestoringJournalAcrossReloadFailure(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	target := h.target()
	if _, err := h.app.applyTelegram(newStore(), []systemdTargetName{target}); err != nil {
		t.Fatal(err)
	}
	reloadAttempts := 0
	restartAttempts := 0
	systemctlRun = func(_ string, args ...string) error {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "daemon-reload") {
			reloadAttempts++
			if reloadAttempts == 1 {
				return errors.New("reload failed")
			}
		}
		if strings.Contains(joined, "try-restart") {
			restartAttempts++
		}
		return nil
	}
	if err := h.app.cleanupHermesTelegramTarget(target); err == nil {
		t.Fatal("reload failure must be returned")
	}
	entry := h.journal(t).Targets[canonicalTelegramTargetName(target)]
	if entry == nil || entry.Phase != telegramPhaseRestoring {
		t.Fatalf("reload failure lost restoring ownership: %+v", entry)
	}
	if raw, err := os.ReadFile(h.systemPath); err != nil || !telegramContentOwnedByEntry(raw, entry) {
		t.Fatalf("reload failure did not restore managed drop-in: content=%q err=%v", raw, err)
	}
	if err := h.app.cleanupHermesTelegramTarget(target); err != nil {
		t.Fatal(err)
	}
	if len(h.journal(t).Targets) != 0 || reloadAttempts != 3 || restartAttempts != 1 {
		t.Fatalf("cleanup replay mismatch: journal=%+v reload=%d restart=%d", h.journal(t), reloadAttempts, restartAttempts)
	}
}

func TestHermesCleanupRetriesMissingArtifactAfterDirectoryFsyncFailure(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	target := h.target()
	if _, err := h.app.applyTelegram(newStore(), []systemdTargetName{target}); err != nil {
		t.Fatal(err)
	}
	telegramRemoveSystemArtifact = removeSystemFileAndEmptyParentCAS
	fsyncCalls := 0
	telegramSystemDirFsync = func(fd int) error {
		fsyncCalls++
		if fsyncCalls == 1 {
			return errors.New("simulated directory fsync failure")
		}
		return syscall.Fsync(fd)
	}
	systemctlCalls := 0
	systemctlRun = func(string, ...string) error {
		systemctlCalls++
		return nil
	}

	err := h.app.cleanupHermesTelegramTarget(target)
	if err == nil || !strings.Contains(err.Error(), "fsync failure") {
		t.Fatalf("directory fsync failure was not returned: %v", err)
	}
	if _, statErr := os.Lstat(h.systemPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unlink should have completed before injected fsync failure: %v", statErr)
	}
	entry := h.journal(t).Targets[canonicalTelegramTargetName(target)]
	if entry == nil || entry.Phase != telegramPhaseRestoring {
		t.Fatalf("fsync failure lost retry ownership: %+v", entry)
	}
	if systemctlCalls != 0 {
		t.Fatalf("reload/restart ran before deletion was durable: calls=%d", systemctlCalls)
	}

	if err := h.app.cleanupHermesTelegramTarget(target); err != nil {
		t.Fatalf("missing-artifact durability replay failed: %v", err)
	}
	if len(h.journal(t).Targets) != 0 {
		t.Fatalf("successful replay did not release journal: %+v", h.journal(t).Targets)
	}
	if systemctlCalls != 2 {
		t.Fatalf("successful replay should daemon-reload and restart once: calls=%d", systemctlCalls)
	}
	if _, statErr := os.Lstat(filepath.Dir(h.systemPath)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("empty drop-in directory was not removed durably: %v", statErr)
	}
}

func TestHermesUserIdentityDriftPreservesArtifactJournalAndService(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	target := systemdTargetName{UserMode: true, User: "root", Service: "hermes-user-drift.service"}
	if _, err := h.app.applyTelegram(newStore(), []systemdTargetName{target}); err != nil {
		t.Fatal(err)
	}
	path, err := telegramManagedUserPath(h.app.cfg, target.User, h.identity.Home, target.Service)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	serviceCalls := 0
	userSystemctlRun = func(string, *persistedUserIdentity, string, ...string) error {
		serviceCalls++
		return nil
	}
	newHome := t.TempDir()
	h.identity.UID = 4242
	h.identity.UIDText = "4242"
	h.identity.Home = newHome
	telegramRemoveUserArtifact = func(string, *persistedUserIdentity, string, []byte) (bool, error) {
		t.Fatal("identity drift must be rejected before unlink")
		return false, nil
	}

	err = h.app.restoreTelegram(newStore())
	if err == nil || !strings.Contains(err.Error(), "身份已变化") {
		t.Fatalf("identity drift was not rejected: %v", err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || string(after) != string(before) {
		t.Fatalf("old identity artifact changed: got=%q err=%v", after, readErr)
	}
	entry := h.journal(t).Targets[canonicalTelegramTargetName(target)]
	if entry == nil || entry.Phase != telegramPhaseActive {
		t.Fatalf("identity drift should retain active ownership journal: %+v", entry)
	}
	if serviceCalls != 0 {
		t.Fatalf("identity drift restarted the replacement account: calls=%d", serviceCalls)
	}
}

func TestRestartTelegramTargetPassesCopiedJournalIdentity(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	target := systemdTargetName{UserMode: true, User: "root", Service: "hermes-user-identity.service"}
	if _, err := h.app.applyTelegram(newStore(), []systemdTargetName{target}); err != nil {
		t.Fatal(err)
	}
	key := canonicalTelegramTargetName(target)
	entry := h.journal(t).Targets[key]
	if entry == nil || entry.Identity == nil {
		t.Fatalf("missing persisted target identity: %+v", entry)
	}
	recorded := *entry.Identity
	calls := 0
	userSystemctlRun = func(user string, identity *persistedUserIdentity, label string, args ...string) error {
		calls++
		if user != target.User || identity == nil || *identity != recorded {
			t.Fatalf("systemctl identity = user:%q identity:%+v, want user:%q identity:%+v", user, identity, target.User, recorded)
		}
		if label != "重启用户级服务 "+target.Service || !slices.Equal(args, []string{"try-restart", "--", target.Service}) {
			t.Fatalf("systemctl call = label:%q args:%v", label, args)
		}
		identity.UID++
		identity.Home = t.TempDir()
		return nil
	}
	if err := h.app.restartTelegramTarget(target); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("systemctl calls = %d, want 1", calls)
	}
	persisted := h.journal(t).Targets[key]
	if persisted == nil || persisted.Identity == nil || *persisted.Identity != recorded {
		t.Fatalf("executor mutated persisted journal identity: %+v", persisted)
	}
}

func TestLegacyTelegramMigrationRequiresExactRuntimeBytes(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	target := h.target()
	st := newStore()
	st.RuntimeConfig = h.app.cfg.runtimeConfig()
	st.TelegramTargets = []string{canonicalTelegramTargetName(target)}
	expectedDropIn := "[Service]\nEnvironmentFile=-/etc/openclaw-hermes-tg-proxy.env\n"
	if err := os.WriteFile(h.legacyPath, []byte(expectedDropIn), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.envPath, []byte("operator changed env"), 0o600); err != nil {
		t.Fatal(err)
	}
	systemctlRun = func(string, ...string) error { t.Fatal("mismatched legacy evidence operated the service"); return nil }
	if err := h.app.restoreTelegram(st); err == nil {
		t.Fatal("mismatched legacy evidence accepted")
	}
	if len(st.TelegramTargets) != 1 {
		t.Fatalf("mismatched legacy evidence lost tracking: %v", st.TelegramTargets)
	}
	got, err := os.ReadFile(h.legacyPath)
	if err != nil || string(got) != expectedDropIn {
		t.Fatalf("legacy drop-in was removed despite env mismatch: got=%q err=%v", got, err)
	}
}

func TestLegacyUserTelegramRecordFailsClosedWithoutIdentityBinding(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	target := systemdTargetName{UserMode: true, User: "root", Service: "legacy-user.service"}
	st := newStore()
	st.RuntimeConfig = h.app.cfg.runtimeConfig()
	st.TelegramTargets = []string{canonicalTelegramTargetName(target)}
	telegramReadUserArtifact = func(string, string, int64) ([]byte, error) {
		t.Fatal("legacy user record must not read a current same-name account")
		return nil, nil
	}
	telegramRemoveUserArtifact = func(string, *persistedUserIdentity, string, []byte) (bool, error) {
		t.Fatal("legacy user record must not unlink a current same-name account")
		return false, nil
	}
	userSystemctlRun = func(string, *persistedUserIdentity, string, ...string) error {
		t.Fatal("legacy user record must not restart a current same-name account")
		return nil
	}

	err := h.app.cleanupLegacyTelegramTargets(st, map[string]bool{}, map[string]bool{})
	if err == nil || !strings.Contains(err.Error(), "没有 uid/gid/home 身份绑定") {
		t.Fatalf("legacy user record did not fail closed: %v", err)
	}
	if len(st.TelegramTargets) != 1 || st.TelegramTargets[0] != canonicalTelegramTargetName(target) {
		t.Fatalf("legacy evidence was not retained: %v", st.TelegramTargets)
	}
}

func TestLegacyTelegramMigrationRemovesOnlyExactDropInAndRetainsSharedEnv(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	target := h.target()
	st := newStore()
	st.RuntimeConfig = h.app.cfg.runtimeConfig()
	st.TelegramTargets = []string{canonicalTelegramTargetName(target)}
	if err := os.WriteFile(h.legacyPath, []byte("[Service]\nEnvironmentFile=-/etc/openclaw-hermes-tg-proxy.env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectedEnv := []byte(telegramProxyEnvContent(h.app.cfg))
	if err := os.WriteFile(h.envPath, expectedEnv, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.app.restoreTelegram(st); err != nil {
		t.Fatal(err)
	}
	if len(st.TelegramTargets) != 0 {
		t.Fatalf("exact legacy migration evidence was not cleared: %v", st.TelegramTargets)
	}
	if _, err := os.Lstat(h.legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exact legacy drop-in was not removed: %v", err)
	}
	gotEnv, err := os.ReadFile(h.envPath)
	if err != nil || string(gotEnv) != string(expectedEnv) {
		t.Fatalf("shared legacy env should be retained conservatively: got=%q err=%v", gotEnv, err)
	}
}
