package manager

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestHermesStartsDuringPreflightStillRequiresMessagePolicy(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	if _, err := h.app.applyTelegram(newStore(), []systemdTargetName{h.target()}); err != nil {
		t.Fatal(err)
	}
	journal := h.journal(t)
	entry := journal.Targets[canonicalTelegramTargetName(h.target())]
	entry.Phase = telegramPhasePrepared
	entry.PendingManagedContent = entry.ManagedContent
	if err := h.app.saveTelegramProxyJournal(journal); err != nil {
		t.Fatal(err)
	}

	running, restarted, policyCalls := false, false, 0
	telegramReadServiceState = func(systemdTargetName, *persistedUserIdentity) (telegramServiceState, error) {
		state := "inactive"
		if running {
			state = "active"
		}
		return telegramServiceState{LoadState: "loaded", ActiveState: state}, nil
	}
	// An administrator or supervisor starts the gateway while final runtime
	// validation is taking place. An earlier inactive snapshot cannot exempt
	// the later restart from the message-preservation policy.
	telegramValidateHermesTarget = func(systemdTargetName, *persistedUserIdentity, string) error {
		running = true
		return nil
	}
	unsafePolicy := errors.New("unsafe pending-message policy")
	telegramValidateHermesRestartPolicy = func(systemdTargetName, *persistedUserIdentity) error {
		policyCalls++
		return unsafePolicy
	}
	systemctlRun = func(_ string, args ...string) error {
		if strings.Contains(strings.Join(args, " "), "try-restart") {
			restarted = true
		}
		return nil
	}
	if err := h.app.restartTelegramTarget(h.target()); !errors.Is(err, unsafePolicy) || restarted || policyCalls != 1 {
		t.Fatalf("err=%v restarted=%v policy checks=%d", err, restarted, policyCalls)
	}
	if h.journal(t).Targets[canonicalTelegramTargetName(h.target())].Phase != telegramPhasePrepared {
		t.Fatal("unsafe restart lost prepared retry evidence")
	}
}

func legacyLifecycleReviewFixture(t *testing.T) (*telegramJournalTestHarness, *Store, []byte) {
	t.Helper()
	h := newTelegramJournalTestHarness(t)
	st := newStore()
	st.RuntimeConfig = h.app.cfg.runtimeConfig()
	st.TelegramTargets = []string{canonicalTelegramTargetName(h.target())}
	original := []byte("[Service]\nEnvironmentFile=-/etc/openclaw-hermes-tg-proxy.env\n")
	if err := os.WriteFile(h.legacyPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.envPath, []byte(telegramProxyEnvContent(h.app.cfg)), 0o600); err != nil {
		t.Fatal(err)
	}
	return h, st, original
}

func TestLegacyHermesRejectedRestartRestoresDropInAndKeepsEvidence(t *testing.T) {
	h, st, original := legacyLifecycleReviewFixture(t)
	unsafePolicy := errors.New("unsafe pending-message policy")
	telegramValidateHermesRestartPolicy = func(systemdTargetName, *persistedUserIdentity) error {
		if _, err := os.Lstat(h.legacyPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("fixture did not reach post-removal policy guard: %v", err)
		}
		return unsafePolicy
	}
	reloads := 0
	systemctlRun = func(_ string, args ...string) error {
		if len(args) == 1 && args[0] == "daemon-reload" {
			reloads++
			return nil
		}
		t.Fatalf("unsafe legacy cleanup ran service command: %v", args)
		return nil
	}
	err := h.app.cleanupLegacyTelegramTargets(st, map[string]bool{}, map[string]bool{})
	if !errors.Is(err, unsafePolicy) {
		t.Fatalf("unsafe policy was not reported: %v", err)
	}
	current, err := os.ReadFile(h.legacyPath)
	if err != nil || !bytes.Equal(current, original) || reloads != 1 {
		t.Fatalf("legacy rollback failed: read=%v identical=%v reloads=%d", err, bytes.Equal(current, original), reloads)
	}
	if len(st.TelegramTargets) != 1 || st.TelegramTargets[0] != canonicalTelegramTargetName(h.target()) {
		t.Fatal("failed migration lost legacy ownership evidence")
	}
	if current, err := os.ReadFile(h.envPath); err != nil || string(current) != telegramProxyEnvContent(h.app.cfg) {
		t.Fatal("legacy shared environment was changed")
	}
}

func TestLegacyHermesRollbackPreservesConcurrentAdministratorReplacement(t *testing.T) {
	h, st, _ := legacyLifecycleReviewFixture(t)
	replacement := []byte("[Service]\nEnvironment=TELEGRAM_PROXY=http://operator.invalid:9876\n")
	unsafePolicy := errors.New("unsafe pending-message policy")
	telegramValidateHermesRestartPolicy = func(systemdTargetName, *persistedUserIdentity) error {
		if err := os.WriteFile(h.legacyPath, replacement, 0o644); err != nil {
			t.Fatal(err)
		}
		return unsafePolicy
	}
	systemctlRun = func(_ string, args ...string) error {
		t.Fatalf("failed create-only rollback must not operate service: %v", args)
		return nil
	}
	err := h.app.cleanupLegacyTelegramTargets(st, map[string]bool{}, map[string]bool{})
	if !errors.Is(err, unsafePolicy) {
		t.Fatalf("unsafe policy was not reported: %v", err)
	}
	current, err := os.ReadFile(h.legacyPath)
	if err != nil || !bytes.Equal(current, replacement) {
		t.Fatalf("administrator replacement overwritten: read=%v same=%v", err, bytes.Equal(current, replacement))
	}
	if len(st.TelegramTargets) != 1 {
		t.Fatal("incomplete migration lost retry evidence")
	}
}
