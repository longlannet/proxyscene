package manager

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
)

func TestRuntimeRemoveAllNodesUnsettledRestartRetainsRecovery(t *testing.T) {
	f := newRemoveAllNodesRuntimeFixture(t)
	claw := newOpenClawTestHarness(t, `{"channels":{"telegram":{"proxy":"http://original.invalid:8089","botToken":"fixture"}},"keep":true}`)
	claw.app = f.core.app
	telegramLookupUserIdentity = openClawLookupUserIdentity
	claw.app.cfg.ManageOpenClawConfig = true
	target := claw.target("openclaw-unsettled.service")
	claw.applyAndCommit(t, target, claw.app.cfg.HTTPAddr(SceneTelegram))
	f.core.store.RuntimeConfig = claw.app.cfg.runtimeConfig()
	if err := claw.app.saveStore(f.core.store); err != nil {
		t.Fatal(err)
	}
	var err error
	f.core.store, err = claw.app.loadStore()
	if err != nil {
		t.Fatal(err)
	}
	before := cloneStore(f.core.store)
	configBefore := claw.config(t)
	ownershipBefore := claw.journal(t).Users[claw.user]
	paths := []string{f.profile, f.apt, f.telegram.systemPath, claw.configPath, claw.app.cfg.XrayConfig(), f.core.unit}
	filesBefore := make(map[string]coreFileState, len(paths))
	for _, path := range paths {
		filesBefore[path] = f.core.file(path)
	}

	pending := false
	activeState := "active"
	telegramReadServiceState = func(current systemdTargetName, identity *persistedUserIdentity) (telegramServiceState, error) {
		state := telegramServiceState{LoadState: "loaded", ActiveState: "active"}
		if current == target {
			if identity == nil || identity.Home != claw.home {
				t.Fatal("restart status query lost the recorded OpenClaw identity")
			}
			state.ActiveState = activeState
			if pending {
				state.Job = "91 /org/freedesktop/systemd1/job/91"
			}
		}
		return state, nil
	}
	restarts := 0
	userSystemctlRun = func(user string, identity *persistedUserIdentity, _ string, args ...string) error {
		if user != claw.user || identity == nil || identity.Home != claw.home {
			t.Fatal("restart lost the recorded OpenClaw identity")
		}
		if len(args) > 0 && args[0] == "try-restart" {
			if args[len(args)-1] != target.Service {
				t.Fatalf("restart escaped the fixed target: %v", args)
			}
			restarts++
			if restarts == 1 {
				pending = true
				return fmt.Errorf("fixture client deadline: %w", errSystemdRestartUnsettled)
			}
		}
		return nil
	}
	if err := claw.app.removeAllNodes(f.core.store); !errors.Is(err, errSystemdRestartUnsettled) {
		t.Fatalf("unsettled restart outcome was lost: %v", err)
	}
	if restarts != 1 || !reflect.DeepEqual(f.core.store, before) {
		t.Fatalf("failed deletion retried restart or changed nodes: restarts=%d", restarts)
	}
	assertSubscriptionStoreUnchanged(t, claw.app, before)
	if exists, err := claw.app.hasRuntimeTransition(); err != nil || !exists {
		t.Fatalf("unsettled deletion lost the recovery transaction: exists=%v err=%v", exists, err)
	}
	releasedConfig := claw.config(t)
	if bytes.Equal(configBefore, releasedConfig) || !bytes.Contains(releasedConfig, []byte(`"proxy":"http://original.invalid:8089"`)) {
		t.Fatal("deletion did not reach proxy restoration, or immediately compensated its in-flight restart")
	}
	if entry := claw.journal(t).Users[claw.user]; entry == nil || entry.Phase != openClawPhaseRestoring {
		t.Fatalf("unsettled restart lost its OpenClaw restore evidence: %+v", entry)
	}

	filesPending := make(map[string]coreFileState, len(paths))
	for _, path := range paths {
		filesPending[path] = f.core.file(path)
	}
	pendingOwnership := claw.journal(t)
	commands, hermesRestarts := len(f.core.commands), f.restarts
	oldDiscover := telegramDiscoverTargetNames
	t.Cleanup(func() { telegramDiscoverTargetNames = oldDiscover })
	telegramDiscoverTargetNames = func() ([]string, error) {
		t.Fatal("recovery rediscovered services instead of using its fixed targets")
		return nil, nil
	}
	for _, state := range []string{"active", "inactive"} {
		activeState = state
		recovered, err := NewApp(claw.app.cfg).recoverRuntimeTransition()
		if !recovered || !errors.Is(err, errSystemdRestartUnsettled) {
			t.Fatalf("recovery accepted %s service with a pending job: recovered=%v err=%v", state, recovered, err)
		}
		for _, path := range paths {
			if !coreFilesEqual(filesPending[path], f.core.file(path)) {
				t.Fatalf("pending %s service allowed recovery to modify %s", state, path)
			}
		}
		if restarts != 1 || len(f.core.commands) != commands || f.restarts != hermesRestarts {
			t.Fatalf("pending service caused another service operation: user=%d core=%d Hermes=%d", restarts, len(f.core.commands), f.restarts)
		}
		if !reflect.DeepEqual(claw.journal(t), pendingOwnership) {
			t.Fatal("pending restart allowed recovery to rewrite OpenClaw ownership")
		}
		assertSubscriptionStoreUnchanged(t, claw.app, before)
		if exists, err := claw.app.hasRuntimeTransition(); err != nil || !exists {
			t.Fatalf("blocked recovery lost the transaction: exists=%v err=%v", exists, err)
		}
	}

	pending = false
	activeState = "active"
	if recovered, err := NewApp(claw.app.cfg).recoverRuntimeTransition(); err != nil || !recovered {
		t.Fatalf("settled restart could not recover: recovered=%v err=%v", recovered, err)
	}
	if restarts != 2 {
		t.Fatalf("recovery did not restart once to apply the restored proxy: restarts=%d", restarts)
	}
	for _, path := range paths {
		if !coreFilesEqual(filesBefore[path], f.core.file(path)) {
			t.Fatalf("settled recovery did not restore %s", path)
		}
	}
	if !reflect.DeepEqual(claw.journal(t).Users[claw.user], ownershipBefore) {
		t.Fatal("recovery did not restore the previous OpenClaw ownership")
	}
	assertSubscriptionStoreUnchanged(t, claw.app, before)
	if exists, err := claw.app.hasRuntimeTransition(); err != nil || exists {
		t.Fatalf("successful recovery retained the transaction: exists=%v err=%v", exists, err)
	}
	if f.core.service.Active != "active" || f.core.service.Enabled != "enabled" {
		t.Fatalf("recovery changed the core service state: %+v", f.core.service)
	}
	devLive, err := snapshotDevProxyConfig(f.devUser, f.devOwned, true, true)
	if err != nil || !reflect.DeepEqual(devLive, f.devLive) {
		t.Fatalf("recovery did not restore Dev proxy settings: %+v %v", devLive, err)
	}
}

func TestTelegramRestartRejectsPendingJobBeforeAndAfterCommand(t *testing.T) {
	for _, stage := range []string{"before", "after"} {
		for _, active := range []string{"active", "inactive"} {
			t.Run(stage+"/"+active, func(t *testing.T) {
				h := newTelegramJournalTestHarness(t)
				commands := 0
				telegramReadServiceState = func(systemdTargetName, *persistedUserIdentity) (telegramServiceState, error) {
					state := telegramServiceState{LoadState: "loaded", ActiveState: "active"}
					if stage == "before" || commands > 0 {
						state.ActiveState = active
						state.Job = "92 /org/freedesktop/systemd1/job/92"
					}
					return state, nil
				}
				systemctlRun = func(_ string, args ...string) error {
					if len(args) == 0 || args[0] != "try-restart" {
						t.Fatalf("unexpected service operation: %v", args)
					}
					commands++
					return nil
				}
				if err := h.app.restartTelegramTarget(h.target()); !errors.Is(err, errSystemdRestartUnsettled) {
					t.Fatalf("pending job was treated as a settled restart: %v", err)
				}
				want := 0
				if stage == "after" {
					want = 1
				}
				if commands != want {
					t.Fatalf("restart count=%d, want %d", commands, want)
				}
				if _, err := os.Stat(h.systemPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("restart check wrote configuration: %v", err)
				}
			})
		}
	}
}

func TestRuntimeTelegramRecoveryChecksJobsWhenRestartIsSkipped(t *testing.T) {
	for _, kind := range []string{"Hermes", "OpenClaw", "legacy"} {
		for _, missing := range []bool{true, false} {
			if kind == "legacy" && !missing {
				continue // Legacy resources have no SkipRestart marker.
			}
			mode := "skip restart"
			if missing {
				mode = "missing unit"
			}
			t.Run(kind+"/"+mode, func(t *testing.T) {
				h := newTelegramJournalTestHarness(t)
				oldOpenClawLookup := openClawLookupUserIdentity
				t.Cleanup(func() { openClawLookupUserIdentity = oldOpenClawLookup })
				openClawLookupUserIdentity = telegramLookupUserIdentity
				identity, err := capturePersistedUserIdentity(h.identity.Name, telegramLookupUserIdentity)
				if err != nil {
					t.Fatal(err)
				}
				target := h.target()
				snapshot := &runtimeTelegramResources{}
				switch kind {
				case "Hermes":
					snapshot.Hermes = []runtimeHermesResource{{Target: canonicalTelegramTargetName(target), SkipRestart: !missing}}
				case "OpenClaw":
					target = systemdTargetName{UserMode: true, User: h.identity.Name, Service: "openclaw-skipped.service"}
					resource := runtimeOpenClawResource{User: target.User, Identity: identity, Targets: []string{canonicalTelegramTargetName(target)}}
					if !missing {
						resource.SkipRestartTargets = append([]string(nil), resource.Targets...)
					}
					snapshot.OpenClaw = []runtimeOpenClawResource{resource}
				case "legacy":
					snapshot.Legacy = []runtimeLegacyResource{{Target: canonicalTelegramTargetName(target)}}
				}
				telegramSystemUnitExists = func(string) bool { return !missing }
				telegramUserUnitExists = func(string, string) bool { return !missing }
				systemctlRun = func(string, ...string) error { t.Fatal("stability check mutated a system service"); return nil }
				userSystemctlRun = func(string, *persistedUserIdentity, string, ...string) error {
					t.Fatal("stability check mutated a user service")
					return nil
				}
				state := telegramServiceState{LoadState: "masked", ActiveState: "active", Job: "93 /org/freedesktop/systemd1/job/93"}
				if missing {
					state.LoadState = "not-found"
				}
				queries := 0
				telegramReadServiceState = func(current systemdTargetName, recorded *persistedUserIdentity) (telegramServiceState, error) {
					if current != target || (target.UserMode && !reflect.DeepEqual(recorded, identity)) {
						t.Fatal("stability check escaped the fixed target or recorded identity")
					}
					queries++
					return state, nil
				}
				for _, active := range []string{"active", "inactive", "failed"} {
					state.ActiveState = active
					if err := validateRuntimeTelegramSettled(snapshot); !errors.Is(err, errSystemdRestartUnsettled) {
						t.Fatalf("%s %s with pending job accepted: %v", state.LoadState, active, err)
					}
				}
				if queries != 3 {
					t.Fatalf("missing unit or SkipRestart bypassed service queries: %d", queries)
				}
				state.Job = ""
				for _, active := range []string{"active", "inactive", "failed"} {
					state.ActiveState = active
					if err := validateRuntimeTelegramSettled(snapshot); err != nil {
						t.Fatalf("stable %s %s blocked configuration recovery: %v", state.LoadState, active, err)
					}
				}
			})
		}
	}
}
