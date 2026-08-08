package manager

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestTelegramProxyEnvPairsOnlyTelegramScoped(t *testing.T) {
	cfg := DefaultConfig()
	pairs := telegramProxyEnvPairs(cfg)

	// 只注入 TELEGRAM_PROXY（Hermes 实际消费的唯一 telegram 专用变量）。
	want := map[string]string{
		"TELEGRAM_PROXY": "http://127.0.0.1:7892",
	}
	if len(pairs) != len(want) {
		t.Fatalf("telegramProxyEnvPairs() returned %d pairs, want %d: %v", len(pairs), len(want), pairs)
	}
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			t.Fatalf("env pair missing '=': %q", pair)
		}
		if !strings.HasPrefix(key, "TELEGRAM_") {
			t.Fatalf("telegram proxy env must not inject broad proxy variable %q", key)
		}
		if wantValue, ok := want[key]; !ok {
			t.Fatalf("unexpected telegram proxy env key %q", key)
		} else if value != wantValue {
			t.Fatalf("%s=%q, want %q", key, value, wantValue)
		}
		delete(want, key)
	}
	for key := range want {
		t.Fatalf("missing telegram proxy env key %q", key)
	}
}

func TestApplyTelegramRejectsZeroActualCandidates(t *testing.T) {
	a := testApp(t)
	applied, err := a.applyTelegram(newStore(), []systemdTargetName{})
	if err == nil || len(applied) != 0 {
		t.Fatalf("zero candidates must fail: applied=%v err=%v", applied, err)
	}
}

func TestSyncXrayWithoutScenesPreservesConfigWhenStopFails(t *testing.T) {
	a := testApp(t)
	credentialConfig := []byte(`{"outbounds":[{"password":"credential-must-survive"}]}`)
	if err := os.WriteFile(a.cfg.XrayConfig(), credentialConfig, 0o600); err != nil {
		t.Fatal(err)
	}

	oldRun := systemctlRun
	oldOutput := systemctlOutput
	t.Cleanup(func() {
		systemctlRun = oldRun
		systemctlOutput = oldOutput
	})
	systemctlRun = func(string, ...string) error { return errors.New("stop or disable failed") }
	systemctlOutput = func(string, ...string) (string, error) { return "active\n", nil }

	if err := a.syncXrayServiceForStore(newStore()); err == nil {
		t.Fatal("stop failure must abort no-scene synchronization")
	}
	got, err := os.ReadFile(a.cfg.XrayConfig())
	if err != nil {
		t.Fatalf("credential config must remain while Xray may still be active: %v", err)
	}
	if !bytes.Equal(got, credentialConfig) {
		t.Fatalf("credential config changed after failed stop: %q", got)
	}
}

func TestLegacyTelegramMigrationRetainsInvalidRecord(t *testing.T) {
	a := testApp(t)
	st := newStore()
	st.TelegramTargets = []string{"bad//name"}
	err := a.cleanupLegacyTelegramTargets(st, map[string]bool{}, map[string]bool{})
	if err == nil {
		t.Fatal("invalid stored record must be reported")
	}
	if !containsString(st.TelegramTargets, "bad//name") {
		t.Fatalf("invalid migration record must be retained: %v", st.TelegramTargets)
	}
}

func TestApplyTelegramReplaysUnchangedSystemTargetAfterReloadFailure(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	a := h.app
	target := systemdTargetName{Service: "hermes-reload-replay-test.service"}
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

	applied, err := a.applyTelegram(newStore(), []systemdTargetName{target})
	if err == nil || len(applied) != 1 || restartAttempts != 0 {
		t.Fatalf("reload failure must retain target and skip restart: applied=%v restarts=%d err=%v", applied, restartAttempts, err)
	}
	applied, err = a.applyTelegram(newStore(), []systemdTargetName{target})
	if err != nil || len(applied) != 1 {
		t.Fatalf("unchanged retry failed: applied=%v err=%v", applied, err)
	}
	if reloadAttempts != 2 || restartAttempts != 1 {
		t.Fatalf("unchanged target was not reconciled: reload=%d restart=%d", reloadAttempts, restartAttempts)
	}
}

func TestApplyTelegramReplaysUnchangedSystemTargetAfterRestartFailure(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	a := h.app
	target := systemdTargetName{Service: "hermes-restart-replay-test.service"}
	reloadAttempts := 0
	restartAttempts := 0
	systemctlRun = func(_ string, args ...string) error {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "daemon-reload") {
			reloadAttempts++
		}
		if strings.Contains(joined, "try-restart") {
			restartAttempts++
			if restartAttempts == 1 {
				return errors.New("restart failed")
			}
		}
		return nil
	}

	applied, err := a.applyTelegram(newStore(), []systemdTargetName{target})
	if err == nil || len(applied) != 1 {
		t.Fatalf("restart failure must retain target: applied=%v err=%v", applied, err)
	}
	applied, err = a.applyTelegram(newStore(), []systemdTargetName{target})
	if err != nil || len(applied) != 1 {
		t.Fatalf("unchanged retry failed: applied=%v err=%v", applied, err)
	}
	if reloadAttempts != 2 || restartAttempts != 2 {
		t.Fatalf("unchanged target was not replayed: reload=%d restart=%d", reloadAttempts, restartAttempts)
	}
}

func TestApplyTelegramReloadsOpenClawUserManagerOnceBeforeMultipleRestarts(t *testing.T) {
	h := newOpenClawTestHarness(t, `{}`)
	a := h.app
	targets := []systemdTargetName{
		h.target("openclaw-first-reload-test.service"),
		h.target("openclaw-second-reload-test.service"),
	}

	oldUserRun := userSystemctlRun
	oldUserExists := telegramUserUnitExists
	t.Cleanup(func() {
		userSystemctlRun = oldUserRun
		telegramUserUnitExists = oldUserExists
	})
	telegramUserUnitExists = func(string, string) bool { return true }
	events := []string{}
	userSystemctlRun = func(user string, identity *persistedUserIdentity, _ string, args ...string) error {
		if user != h.user || identity == nil || identity.Home != h.home {
			return errors.New("unexpected OpenClaw user identity")
		}
		if len(args) == 1 && args[0] == "daemon-reload" {
			events = append(events, "reload")
			return nil
		}
		if len(args) == 3 && args[0] == "try-restart" && args[1] == "--" {
			events = append(events, "restart:"+args[2])
			return nil
		}
		return errors.New("unexpected systemctl invocation")
	}

	applied, err := a.applyTelegram(newStore(), targets)
	if err != nil || len(applied) != len(targets) {
		t.Fatalf("multi-target OpenClaw apply failed: applied=%v err=%v", applied, err)
	}
	want := []string{
		"reload",
		"restart:" + targets[0].Service,
		"restart:" + targets[1].Service,
	}
	if !slices.Equal(events, want) {
		t.Fatalf("OpenClaw reload/restart order=%v, want %v", events, want)
	}
	entry := h.journal(t).Users[h.user]
	if entry == nil || entry.Phase != openClawPhaseActive || len(entry.PendingTargets) != 0 {
		t.Fatalf("multi-target OpenClaw ownership not committed: %+v", entry)
	}
}

func TestApplyTelegramRejectsHermesToOpenClawOwnershipDriftBeforeMutation(t *testing.T) {
	h := newOpenClawTestHarness(t, `{}`)
	target := h.target("openclaw-ownership-drift-test.service")
	identity := &persistedUserIdentity{UID: h.identity.UID, GID: h.identity.GID, Home: h.identity.Home}
	journal := newTelegramProxyJournal()
	key := canonicalTelegramTargetName(target)
	journal.Targets[key] = &telegramProxyJournalEntry{
		Target:         key,
		Artifact:       telegramArtifactUserDropIn,
		Identity:       identity,
		Phase:          telegramPhaseActive,
		ManagedContent: "[Service]\nEnvironment=\"TELEGRAM_PROXY=http://127.0.0.1:7892\"\n",
	}
	if err := h.app.saveTelegramProxyJournal(journal); err != nil {
		t.Fatal(err)
	}

	oldLookup := telegramLookupUserIdentity
	oldUserExists := telegramUserUnitExists
	t.Cleanup(func() {
		telegramLookupUserIdentity = oldLookup
		telegramUserUnitExists = oldUserExists
	})
	telegramLookupUserIdentity = func(user string) (localUserIdentity, error) {
		if user != h.user {
			return localUserIdentity{}, errors.New("unexpected test user")
		}
		return h.identity, nil
	}
	telegramUserUnitExists = func(string, string) bool { return true }
	beforeConfig := append([]byte(nil), h.config(t)...)

	applied, err := h.app.applyTelegram(newStore(), []systemdTargetName{target})
	if err == nil || !strings.Contains(err.Error(), "当前识别为 OpenClaw") || len(applied) != 0 {
		t.Fatalf("Hermes->OpenClaw drift was not rejected before apply: applied=%v err=%v", applied, err)
	}
	if got := h.config(t); !bytes.Equal(got, beforeConfig) {
		t.Fatalf("rejected drift changed OpenClaw config: %s", got)
	}
	if _, statErr := os.Lstat(h.app.openClawJournalPath()); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rejected drift created OpenClaw journal: %v", statErr)
	}
	persisted, loadErr := h.app.loadTelegramProxyJournal()
	if loadErr != nil || len(persisted.Targets) != 1 || persisted.Targets[key] == nil {
		t.Fatalf("rejected drift changed Hermes ownership: journal=%+v err=%v", persisted, loadErr)
	}
}

func TestApplyTelegramRejectsOpenClawToHermesOwnershipDriftBeforeMutation(t *testing.T) {
	h := newOpenClawTestHarness(t, `{}`)
	target := h.target("hermes-ownership-drift-test.service")
	h.applyAndCommit(t, target, "http://127.0.0.1:7892")
	beforeConfig := append([]byte(nil), h.config(t)...)
	beforeJournal, err := json.Marshal(h.journal(t))
	if err != nil {
		t.Fatal(err)
	}

	oldUserExists := telegramUserUnitExists
	t.Cleanup(func() { telegramUserUnitExists = oldUserExists })
	telegramUserUnitExists = func(string, string) bool { return true }

	applied, err := h.app.applyTelegram(newStore(), []systemdTargetName{target})
	if err == nil || !strings.Contains(err.Error(), "当前识别为 Hermes") || len(applied) != 0 {
		t.Fatalf("OpenClaw->Hermes drift was not rejected before apply: applied=%v err=%v", applied, err)
	}
	if got := h.config(t); !bytes.Equal(got, beforeConfig) {
		t.Fatalf("rejected drift changed OpenClaw config: %s", got)
	}
	afterJournal, marshalErr := json.Marshal(h.journal(t))
	if marshalErr != nil || !bytes.Equal(afterJournal, beforeJournal) {
		t.Fatalf("rejected drift changed OpenClaw ownership: before=%s after=%s err=%v", beforeJournal, afterJournal, marshalErr)
	}
	if _, statErr := os.Lstat(h.app.telegramProxyJournalPath()); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rejected drift created Hermes journal: %v", statErr)
	}
}

func TestApplyTelegramReplaysOpenClawAfterUserManagerReloadFailure(t *testing.T) {
	h := newOpenClawTestHarness(t, `{}`)
	a := h.app
	target := h.target("openclaw-reload-replay-test.service")

	oldUserRun := userSystemctlRun
	oldUserExists := telegramUserUnitExists
	t.Cleanup(func() {
		userSystemctlRun = oldUserRun
		telegramUserUnitExists = oldUserExists
	})
	telegramUserUnitExists = func(string, string) bool { return true }
	reloadAttempts := 0
	restartAttempts := 0
	userSystemctlRun = func(_ string, _ *persistedUserIdentity, _ string, args ...string) error {
		if len(args) == 1 && args[0] == "daemon-reload" {
			reloadAttempts++
			if reloadAttempts == 1 {
				return errors.New("reload failed")
			}
			return nil
		}
		if len(args) == 3 && args[0] == "try-restart" {
			restartAttempts++
			return nil
		}
		return errors.New("unexpected systemctl invocation")
	}

	applied, err := a.applyTelegram(newStore(), []systemdTargetName{target})
	if err == nil || len(applied) != 1 || restartAttempts != 0 {
		t.Fatalf("reload failure must retain ownership and skip restart: applied=%v restarts=%d err=%v", applied, restartAttempts, err)
	}
	entry := h.journal(t).Users[h.user]
	if entry == nil || entry.Phase != openClawPhasePrepared || !containsString(entry.PendingTargets, canonicalTelegramTargetName(target)) {
		t.Fatalf("reload failure did not retain prepared OpenClaw journal: %+v", entry)
	}

	applied, err = a.applyTelegram(newStore(), []systemdTargetName{target})
	if err != nil || len(applied) != 1 {
		t.Fatalf("OpenClaw reload replay failed: applied=%v err=%v", applied, err)
	}
	if reloadAttempts != 2 || restartAttempts != 1 {
		t.Fatalf("OpenClaw reload replay counts: reload=%d restart=%d", reloadAttempts, restartAttempts)
	}
	entry = h.journal(t).Users[h.user]
	if entry == nil || entry.Phase != openClawPhaseActive || len(entry.PendingTargets) != 0 {
		t.Fatalf("OpenClaw replay did not commit ownership: %+v", entry)
	}
}

func TestCleanupOpenClawReloadsUserManagerOnceBeforeMultipleRestarts(t *testing.T) {
	const original = "socks5://original.example:1080"
	const managedProxy = "http://127.0.0.1:7892"
	h := newOpenClawTestHarness(t, `{"channels":{"telegram":{"proxy":"`+original+`"}}}`)
	a := h.app
	targets := []systemdTargetName{
		h.target("openclaw-first-restore-test.service"),
		h.target("openclaw-second-restore-test.service"),
	}
	h.applyAndCommit(t, targets[0], managedProxy)
	managed, changed, err := a.applyOpenClawTelegramProxy(targets[1], managedProxy)
	if err != nil || !managed || !changed {
		t.Fatalf("prepare second OpenClaw target: managed=%v changed=%v err=%v", managed, changed, err)
	}
	if err := a.commitOpenClawTelegramProxyApply(targets[1]); err != nil {
		t.Fatal(err)
	}

	oldUserRun := userSystemctlRun
	oldUserExists := telegramUserUnitExists
	t.Cleanup(func() {
		userSystemctlRun = oldUserRun
		telegramUserUnitExists = oldUserExists
	})
	telegramUserUnitExists = func(string, string) bool { return true }
	events := []string{}
	userSystemctlRun = func(_ string, _ *persistedUserIdentity, _ string, args ...string) error {
		if len(args) == 1 && args[0] == "daemon-reload" {
			events = append(events, "reload")
			return nil
		}
		if len(args) == 3 && args[0] == "try-restart" && args[1] == "--" {
			events = append(events, "restart:"+args[2])
			return nil
		}
		return errors.New("unexpected systemctl invocation")
	}

	gotTargets, err := a.cleanupManagedOpenClawTargets(targets[0], true)
	if err != nil || len(gotTargets) != len(targets) {
		t.Fatalf("multi-target OpenClaw restore failed: targets=%v err=%v", gotTargets, err)
	}
	want := []string{
		"reload",
		"restart:" + targets[0].Service,
		"restart:" + targets[1].Service,
	}
	if !slices.Equal(events, want) {
		t.Fatalf("OpenClaw restore reload/restart order=%v, want %v", events, want)
	}
	if h.journal(t).Users[h.user] != nil {
		t.Fatal("OpenClaw restore ownership remained after all restarts")
	}
	value, present, err := openClawTelegramProxyRawValue(h.config(t))
	if err != nil || !proxyStateMatchesString(value, present, original) {
		t.Fatalf("OpenClaw original proxy not restored: value=%s present=%v err=%v", value, present, err)
	}
}

func TestCleanupOpenClawReplaysAfterUserManagerReloadFailure(t *testing.T) {
	h := newOpenClawTestHarness(t, `{}`)
	a := h.app
	target := h.target("openclaw-restore-reload-replay-test.service")
	h.applyAndCommit(t, target, "http://127.0.0.1:7892")

	oldUserRun := userSystemctlRun
	oldUserExists := telegramUserUnitExists
	t.Cleanup(func() {
		userSystemctlRun = oldUserRun
		telegramUserUnitExists = oldUserExists
	})
	telegramUserUnitExists = func(string, string) bool { return true }
	reloadAttempts := 0
	restartAttempts := 0
	userSystemctlRun = func(_ string, _ *persistedUserIdentity, _ string, args ...string) error {
		if len(args) == 1 && args[0] == "daemon-reload" {
			reloadAttempts++
			if reloadAttempts == 1 {
				return errors.New("reload failed")
			}
			return nil
		}
		if len(args) == 3 && args[0] == "try-restart" {
			restartAttempts++
			return nil
		}
		return errors.New("unexpected systemctl invocation")
	}

	if _, err := a.cleanupManagedOpenClawTargets(target, true); err == nil || restartAttempts != 0 {
		t.Fatalf("restore reload failure must skip restart: restarts=%d err=%v", restartAttempts, err)
	}
	entry := h.journal(t).Users[h.user]
	if entry == nil || entry.Phase != openClawPhaseRestoring {
		t.Fatalf("restore reload failure did not retain restoring journal: %+v", entry)
	}
	value, present, err := openClawTelegramProxyRawValue(h.config(t))
	if err != nil || present {
		t.Fatalf("restore reload failure did not preserve restored config: value=%s present=%v err=%v", value, present, err)
	}

	if _, err := a.cleanupManagedOpenClawTargets(target, true); err != nil {
		t.Fatalf("restore reload replay failed: %v", err)
	}
	if reloadAttempts != 2 || restartAttempts != 1 {
		t.Fatalf("restore reload replay counts: reload=%d restart=%d", reloadAttempts, restartAttempts)
	}
	if h.journal(t).Users[h.user] != nil {
		t.Fatal("restore reload replay did not release ownership")
	}
}

func TestTelegramProxySystemdEnvironmentLinesDoNotInjectBroadProxy(t *testing.T) {
	lines := telegramProxySystemdEnvironmentLines(DefaultConfig())
	for _, line := range strings.Split(strings.TrimSpace(lines), "\n") {
		line = strings.TrimPrefix(line, "Environment=")
		line = strings.Trim(line, "\"")
		key, _, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("systemd environment line missing env assignment: %q", line)
		}
		switch key {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy":
			t.Fatalf("systemd env lines include forbidden broad proxy %q in:\n%s", key, lines)
		}
	}
	for _, required := range []string{
		`Environment="TELEGRAM_PROXY=http://127.0.0.1:7892"`,
		`Environment="PYTHONSAFEPATH=1"`,
	} {
		if !strings.Contains(lines, required) {
			t.Fatalf("systemd env lines missing %s in:\n%s", required, lines)
		}
	}
}

func TestRuntimeTransitionDoesNotCleanDisabledUnownedScenes(t *testing.T) {
	a := testApp(t)
	st := newStore()
	if err := a.applySavedScenesWithCleanup(st, false); err != nil {
		t.Fatalf("disabled unowned scenes should be skipped during a runtime transition: %v", err)
	}
}
