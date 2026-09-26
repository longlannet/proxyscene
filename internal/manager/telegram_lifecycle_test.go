package manager

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestHermesUnsafeColdBootRejectedBeforeFirstWrite(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	telegramValidateHermesRestartPolicy = func(systemdTargetName, *persistedUserIdentity) error {
		return errors.New("drop_pending_on_cold_boot must be false")
	}
	desired := []byte("[Service]\n" + telegramProxySystemdEnvironmentLines(h.app.cfg))
	prepared, err := h.app.prepareHermesTelegramApply(h.target(), desired)
	if err == nil || prepared.managed || prepared.reconcile {
		t.Fatalf("unsafe first apply accepted: %+v, %v", prepared, err)
	}
	if _, err := os.Lstat(h.systemPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe apply changed drop-in: %v", err)
	}
	if len(h.journal(t).Targets) != 0 {
		t.Fatal("unsafe apply claimed ownership")
	}
}

func TestHermesUnchangedConfigDoesNotRequireRestartPolicy(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	if _, err := h.app.applyTelegram(newStore(), []systemdTargetName{h.target()}); err != nil {
		t.Fatal(err)
	}
	policyCalls := 0
	telegramValidateHermesRestartPolicy = func(systemdTargetName, *persistedUserIdentity) error {
		policyCalls++
		return errors.New("unsafe cold boot")
	}
	systemctlRun = func(string, ...string) error { t.Fatal("unchanged apply restarted service"); return nil }
	if _, err := h.app.applyTelegram(newStore(), []systemdTargetName{h.target()}); err != nil || policyCalls != 0 {
		t.Fatalf("unchanged configuration should require no disruptive work: %v, calls=%d", err, policyCalls)
	}
	desired := []byte("[Service]\nEnvironment=TELEGRAM_PROXY=http://127.0.0.1:8892\nEnvironment=PYTHONSAFEPATH=1\nEnvironment=HERMES_TELEGRAM_DISABLE_FALLBACK_IPS=1\n")
	before, err := os.ReadFile(h.systemPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.prepareHermesTelegramApply(h.target(), desired); err == nil || policyCalls != 1 {
		t.Fatalf("changed proxy did not invoke policy: %v, calls=%d", err, policyCalls)
	}
	after, err := os.ReadFile(h.systemPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("unsafe update changed existing proxy")
	}
}

func TestHermesRestoreRetainsRetryEvidenceWhenMessagePolicyChanges(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	if _, err := h.app.applyTelegram(newStore(), []systemdTargetName{h.target()}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(h.systemPath)
	if err != nil {
		t.Fatal(err)
	}
	telegramValidateHermesRestartPolicy = func(systemdTargetName, *persistedUserIdentity) error { return errors.New("unsafe cold boot") }
	systemctlRun = func(_ string, args ...string) error {
		if strings.Contains(strings.Join(args, " "), "try-restart") {
			t.Fatal("restoration restarted with an unsafe message policy")
		}
		return nil
	}
	if err := h.app.cleanupHermesTelegramTarget(h.target()); err == nil {
		t.Fatal("unsafe restoration restart accepted")
	}
	after, err := os.ReadFile(h.systemPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed restoration did not put owned drop-in back")
	}
	entry := h.journal(t).Targets[canonicalTelegramTargetName(h.target())]
	if entry == nil || entry.Phase != telegramPhaseRestoring {
		t.Fatal("restoration retry evidence lost")
	}
	// A stopped gateway can be safely detached without changing its message
	// policy: the operation must not start it or discard any queued updates.
	stubTelegramServiceState(t, telegramServiceState{LoadState: "loaded", ActiveState: "inactive"})
	if err := h.app.cleanupHermesTelegramTarget(h.target()); err != nil {
		t.Fatal(err)
	}
	if len(h.journal(t).Targets) != 0 {
		t.Fatal("stopped gateway restoration was not completed")
	}
}

func TestHermesInactiveServiceOnlySavesConfiguration(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	stubTelegramServiceState(t, telegramServiceState{LoadState: "loaded", ActiveState: "inactive"})
	telegramValidateHermesRestartPolicy = func(systemdTargetName, *persistedUserIdentity) error {
		t.Fatal("inactive service cannot lose messages through a restart")
		return nil
	}
	systemctlRun = func(_ string, args ...string) error {
		if strings.Contains(strings.Join(args, " "), "try-restart") {
			t.Fatal("inactive service was restarted")
		}
		return nil
	}
	if _, err := h.app.applyTelegram(newStore(), []systemdTargetName{h.target()}); err != nil {
		t.Fatal(err)
	}
	if entry := h.journal(t).Targets[canonicalTelegramTargetName(h.target())]; entry == nil || entry.Phase != telegramPhaseActive {
		t.Fatal("configuration ownership not committed")
	}
}

func TestTelegramRestartExitSuccessDoesNotProveServiceRunning(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	restarted := false
	telegramReadServiceState = func(systemdTargetName, *persistedUserIdentity) (telegramServiceState, error) {
		state := "active"
		if restarted {
			state = "failed"
		}
		return telegramServiceState{LoadState: "loaded", ActiveState: state}, nil
	}
	systemctlRun = func(_ string, args ...string) error {
		if strings.Contains(strings.Join(args, " "), "try-restart") {
			restarted = true
		}
		return nil
	}
	if _, err := h.app.applyTelegram(newStore(), []systemdTargetName{h.target()}); err == nil {
		t.Fatal("failed gateway was reported successfully reconciled")
	}
	if entry := h.journal(t).Targets[canonicalTelegramTargetName(h.target())]; entry == nil || entry.Phase != telegramPhasePrepared {
		t.Fatal("failed gateway lost prepared retry evidence")
	}
}

func TestHermesUnknownServiceStateBlocksWrite(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	telegramReadServiceState = func(systemdTargetName, *persistedUserIdentity) (telegramServiceState, error) {
		return telegramServiceState{}, errors.New("manager unavailable")
	}
	if _, err := h.app.prepareHermesTelegramApply(h.target(), []byte("[Service]\n"+telegramProxySystemdEnvironmentLines(h.app.cfg))); err == nil {
		t.Fatal("unknown state accepted")
	}
	if len(h.journal(t).Targets) != 0 {
		t.Fatal("unknown service state claimed ownership")
	}
}
