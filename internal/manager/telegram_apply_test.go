package manager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestApplyTelegramManagerFailureRetainsOnlyPendingWork(t *testing.T) {
	hermes := newTelegramJournalTestHarness(t)
	claw := newOpenClawTestHarness(t, `{}`)
	targets := []systemdTargetName{hermes.target(), claw.target("openclaw-batch-first.service"), claw.target("openclaw-batch-second.service")}
	reloadErr := errors.New("system manager unavailable")
	failSystem := true
	systemRestarts, userReloads, userRestarts := 0, 0, 0
	systemctlRun = func(_ string, args ...string) error {
		if args[0] == "daemon-reload" && failSystem {
			return reloadErr
		}
		if args[0] == "try-restart" {
			systemRestarts++
		}
		return nil
	}
	userSystemctlRun = func(user string, identity *persistedUserIdentity, _ string, args ...string) error {
		if user != claw.user || identity == nil || identity.Home != claw.home {
			t.Fatal("user manager identity was not bound to its journal")
		}
		if args[0] == "daemon-reload" {
			userReloads++
		}
		if args[0] == "try-restart" {
			userRestarts++
		}
		return nil
	}
	// Include a duplicate selection: it must still describe one owned target.
	applied, err := claw.app.applyTelegram(newStore(), append(targets, targets[1]))
	if !errors.Is(err, reloadErr) || len(applied) != 3 {
		t.Fatalf("partial ownership lost: applied=%v, err=%v", applied, err)
	}
	if systemRestarts != 0 || userReloads != 1 || userRestarts != 2 {
		t.Fatalf("manager failure crossed groups: system=%d user reload=%d restart=%d", systemRestarts, userReloads, userRestarts)
	}
	journal, err := claw.app.loadTelegramProxyJournal()
	if err != nil {
		t.Fatal(err)
	}
	if entry := journal.Targets[canonicalTelegramTargetName(targets[0])]; entry == nil || entry.Phase != telegramPhasePrepared {
		t.Fatal("failed manager lost pending ownership")
	}
	if entry := claw.journal(t).Users[claw.user]; entry == nil || entry.Phase != openClawPhaseActive || len(entry.PendingTargets) != 0 {
		t.Fatal("independent user manager did not commit its shared configuration")
	}
	failSystem = false
	if applied, err = claw.app.applyTelegram(newStore(), targets); err != nil || len(applied) != 3 {
		t.Fatalf("pending manager replay failed: applied=%v err=%v", applied, err)
	}
	if systemRestarts != 1 || userReloads != 1 || userRestarts != 2 {
		t.Fatalf("replay repeated completed work: system=%d user reload=%d restart=%d", systemRestarts, userReloads, userRestarts)
	}
}

func TestApplyTelegramSharedConfigCapturesEveryReloadBeforeWrite(t *testing.T) {
	h, first, _, changed := setupOpenClawReloadFixture(t, `{}`)
	second := h.target("openclaw-batch-other.service")
	unitPath := filepath.Join(h.home, ".config", "systemd", "user", second.Service)
	if err := os.WriteFile(unitPath, []byte("[Service]\nExecStart=/usr/bin/node /opt/openclaw/dist/index.js gateway --port 18789\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldExists, oldUserRun := telegramUserUnitExists, userSystemctlRun
	t.Cleanup(func() { telegramUserUnitExists, userSystemctlRun = oldExists, oldUserRun })
	telegramUserUnitExists = func(string, string) bool { return true }
	reloads := 0
	userSystemctlRun = func(_ string, _ *persistedUserIdentity, _ string, args ...string) error {
		if len(args) != 1 || args[0] != "daemon-reload" {
			t.Fatalf("acknowledged reload fell back to restart: %v", args)
		}
		reloads++
		return nil
	}
	rpc := openClawReloadRPC
	baselines, acknowledgements := map[string]bool{}, map[string]bool{}
	openClawReloadRPC = func(ctx context.Context, target systemdTargetName, plan *openClawTelegramReloadPlan, method string, result any) error {
		if method == "channels.status" {
			if !*changed {
				baselines[target.Service] = true
			} else {
				acknowledgements[target.Service] = true
			}
		}
		return rpc(ctx, target, plan, method, result)
	}
	h.beforeNextCAS = func() {
		if !baselines[first.Service] || !baselines[second.Service] {
			t.Fatal("shared config changed before every gateway baseline was captured")
		}
		*changed = true
	}
	applied, err := h.app.applyTelegram(newStore(), []systemdTargetName{first, second})
	if err != nil || len(applied) != 2 || reloads != 1 {
		t.Fatalf("shared reload failed: applied=%v reloads=%d err=%v", applied, reloads, err)
	}
	if !acknowledgements[first.Service] || !acknowledgements[second.Service] {
		t.Fatal("not every shared gateway acknowledged the new configuration")
	}
	if entry := h.journal(t).Users[h.user]; entry == nil || entry.Phase != openClawPhaseActive || len(entry.PendingTargets) != 0 {
		t.Fatal("shared ownership not committed after all acknowledgements")
	}
}
