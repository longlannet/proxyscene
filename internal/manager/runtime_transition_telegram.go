package manager

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
)

func captureRuntimeTelegram(beforeApp, candidateApp *App, before, candidate *Store, plan *telegramPlan) (*runtimeTelegramResources, error) {
	if plan == nil {
		return nil, errors.New("Telegram资源缺少固定计划")
	}
	hermes, err := beforeApp.loadTelegramProxyJournal()
	if err != nil {
		return nil, err
	}
	claw, err := beforeApp.loadOpenClawProxyJournal()
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(hermes, plan.observed.hermes) || !reflect.DeepEqual(claw, plan.observed.openClaw) {
		return nil, errors.New("网关 ownership 在计划后已变化")
	}
	result := &runtimeTelegramResources{HermesGeneration: hermes.Generation, OpenClawGeneration: claw.Generation}
	clawUsers := map[string]int{}
	for groupIndex, group := range [][]plannedTelegramTarget{plan.selected, plan.releases} {
		for _, item := range group {
			key := canonicalTelegramTargetName(item.target)
			if item.openClaw {
				path, err := beforeApp.openClawConfigPath(item.target.User, item.identity)
				if err != nil {
					return nil, err
				}
				raw, err := readOpenClawUserConfig(item.target.User, item.identity, path)
				if err != nil {
					return nil, err
				}
				if !bytes.Equal(raw, item.content) {
					return nil, errors.New("用户OpenClaw配置在计划后已变化")
				}
				if index, ok := clawUsers[item.target.User]; ok {
					result.OpenClaw[index].Targets = appendUniqueString(result.OpenClaw[index].Targets, key)
					if item.unit.resolution.State != telegramUnitResolved {
						result.OpenClaw[index].SkipRestartTargets = appendUniqueString(result.OpenClaw[index].SkipRestartTargets, key)
					}
					continue
				}
				entry, err := cloneRuntimeValue(claw.Users[item.target.User])
				if err != nil {
					return nil, err
				}
				value, present, channels, telegram, err := openClawTelegramProxyState(raw)
				if err != nil {
					return nil, err
				}
				lexeme, _, err := openClawTelegramProxyLexeme(raw)
				if err != nil {
					return nil, err
				}
				if entry != nil && (entry.Phase != openClawPhaseActive || !proxyStateMatchesString(value, present, entry.ManagedValue)) {
					return nil, errors.New("用户OpenClaw初始ownership待恢复或实际值不符")
				}
				resource := runtimeOpenClawResource{User: item.target.User, Identity: item.identity, Targets: []string{key}, Before: entry, Value: value, Lexeme: lexeme, Present: present, ChannelsPresent: channels, TelegramPresent: telegram}
				if groupIndex == 0 {
					resource.AllowedProxy = candidateApp.cfg.HTTPAddr(SceneTelegram)
				}
				if item.unit.resolution.State != telegramUnitResolved {
					resource.SkipRestartTargets = []string{key}
				}
				clawUsers[resource.User] = len(result.OpenClaw)
				result.OpenClaw = append(result.OpenClaw, resource)
			} else {
				path, err := beforeApp.telegramManagedArtifactPath(item.target, item.identity)
				if err != nil {
					return nil, err
				}
				raw, err := readTelegramManagedArtifact(item.target, item.identity, path, maxTelegramManagedContentBytes)
				missing := errors.Is(err, os.ErrNotExist)
				if err != nil && !missing {
					return nil, err
				}
				if missing != item.missing || !bytes.Equal(raw, item.content) {
					return nil, errors.New("网关Hermes drop-in在计划后已变化")
				}
				entry, err := cloneRuntimeValue(hermes.Targets[key])
				if err != nil {
					return nil, err
				}
				if entry != nil && (entry.Phase != telegramPhaseActive || missing || entry.ManagedContent != string(raw)) {
					return nil, errors.New("网关Hermes初始ownership待恢复或实际文件不符")
				}
				resource := runtimeHermesResource{SkipRestart: item.unit.resolution.State != telegramUnitResolved, Target: key, Identity: item.identity, Before: entry, Present: !missing, Content: raw, AllowAbsent: groupIndex == 1 || entry == nil}
				if groupIndex == 0 {
					resource.AllowedContent = []byte("[Service]\n" + telegramProxySystemdEnvironmentLines(candidateApp.cfg))
				}
				result.Hermes = append(result.Hermes, resource)
			}
		}
	}
	for _, target := range append(slices.Clone(plan.legacyHandoffs), plan.legacyReleases...) {
		key := canonicalTelegramTargetName(target)
		if target.UserMode || before == nil || before.RuntimeConfig == nil || !containsString(before.TelegramTargets, key) {
			return nil, errors.New("旧Telegram资源缺少历史系统目标ownership")
		}
		raw, err := telegramReadSystemArtifact(telegramLegacySystemPath(beforeApp.cfg, target.Service), maxTelegramManagedContentBytes)
		missing := errors.Is(err, os.ErrNotExist)
		if err != nil && !missing {
			return nil, err
		}
		if !missing && !bytes.Equal(raw, []byte("[Service]\nEnvironmentFile=-/etc/openclaw-hermes-tg-proxy.env\n")) {
			return nil, errors.New("旧Telegram drop-in与历史模板不符")
		}
		result.Legacy = append(result.Legacy, runtimeLegacyResource{Target: key, Present: !missing, Content: raw})
	}
	return result, validateRuntimeTelegram(result)
}

func validateRuntimeTelegram(snapshot *runtimeTelegramResources) error {
	if snapshot == nil {
		return nil
	}
	if len(snapshot.Hermes)+len(snapshot.OpenClaw)+len(snapshot.Legacy) > maxTelegramTargets || snapshot.HermesGeneration == ^uint64(0) || snapshot.OpenClawGeneration == ^uint64(0) {
		return errors.New("Telegram资源范围或generation无效")
	}
	seen := map[string]bool{}
	for _, item := range snapshot.Hermes {
		target, err := parseSystemdTargetName(item.Target)
		if err != nil || canonicalTelegramTargetName(target) != item.Target || seen[item.Target] {
			return errors.New("网关Hermes固定目标无效或重复")
		}
		seen[item.Target] = true
		if target.UserMode {
			if err := validatePersistedUserIdentity(target.User, item.Identity); err != nil {
				return err
			}
		} else if item.Identity != nil {
			return errors.New("系统Hermes目标含用户身份")
		}
		if !item.Present && len(item.Content) != 0 {
			return errors.New("缺失Hermes文件携带内容")
		}
		if item.Present {
			if err := validateTelegramManagedContent(string(item.Content)); err != nil {
				return err
			}
		}
		if len(item.AllowedContent) > 0 {
			if err := validateTelegramManagedContent(string(item.AllowedContent)); err != nil {
				return err
			}
		}
		if item.Before != nil {
			journal := newTelegramProxyJournal()
			journal.Targets[item.Target] = item.Before
			if err := validateTelegramProxyJournal(journal); err != nil {
				return err
			}
			if item.Before.Phase != telegramPhaseActive || !item.Present || item.Before.ManagedContent != string(item.Content) || !reflect.DeepEqual(item.Before.Identity, item.Identity) {
				return errors.New("网关Hermes before-image与ownership不符")
			}
		} else if item.Present {
			return errors.New("网关Hermes before-image缺少ownership")
		}
	}
	users := map[string]bool{}
	for _, item := range snapshot.OpenClaw {
		if users[item.User] || len(item.Targets) == 0 || len(item.Targets) > maxTelegramTargets || int64(len(item.Value)) > maxOpenClawConfigBytes || int64(len(item.Lexeme)) > maxOpenClawConfigBytes {
			return errors.New("用户OpenClaw资源重复或范围无效")
		}
		users[item.User] = true
		if err := validatePersistedUserIdentity(item.User, item.Identity); err != nil {
			return err
		}
		for _, key := range item.Targets {
			target, err := parseSystemdTargetName(key)
			if err != nil || !target.UserMode || target.User != item.User || canonicalTelegramTargetName(target) != key || seen[key] {
				return errors.New("用户OpenClaw固定目标无效或重复")
			}
			seen[key] = true
		}
		for _, key := range item.SkipRestartTargets {
			if !containsString(item.Targets, key) {
				return errors.New("用户OpenClaw跳过服务目标不在固定范围")
			}
		}
		probe := &openClawProxyJournalEntry{User: item.User, Identity: item.Identity, OriginalValue: item.Value, OriginalLexeme: item.Lexeme, OriginalPresent: item.Present, OriginalStructureRecorded: true, OriginalChannelsPresent: item.ChannelsPresent, OriginalTelegramPresent: item.TelegramPresent, ManagedValue: "http://127.0.0.1:1", Targets: item.Targets, Phase: openClawPhaseActive}
		journal := newOpenClawProxyJournal()
		journal.Users[item.User] = probe
		if err := validateOpenClawProxyJournal(journal); err != nil {
			return err
		}
		if item.Before != nil {
			journal.Users[item.User] = item.Before
			if err := validateOpenClawProxyJournal(journal); err != nil {
				return err
			}
			if item.Before.Phase != openClawPhaseActive || !reflect.DeepEqual(item.Before.Identity, item.Identity) || !proxyStateMatchesString(item.Value, item.Present, item.Before.ManagedValue) {
				return errors.New("用户OpenClaw before-image与ownership不符")
			}
		}
	}
	if len(seen) > maxTelegramTargets {
		return errors.New("Telegram固定目标总数超限")
	}
	legacySeen := map[string]bool{}
	for _, item := range snapshot.Legacy {
		target, err := parseSystemdTargetName(item.Target)
		if err != nil || target.UserMode || canonicalTelegramTargetName(target) != item.Target || legacySeen[item.Target] {
			return errors.New("旧Telegram固定目标无效或重复")
		}
		legacySeen[item.Target] = true
		if item.Present {
			if string(item.Content) != "[Service]\nEnvironmentFile=-/etc/openclaw-hermes-tg-proxy.env\n" {
				return errors.New("旧Telegram before-image不符")
			}
		} else if len(item.Content) != 0 {
			return errors.New("缺失旧Telegram文件携带内容")
		}
	}
	return nil
}

func compensateRuntimeTelegram(app *App, snapshot *runtimeTelegramResources, checkpoint ...func() error) error {
	if snapshot == nil {
		return errors.New("缺少Telegram恢复计划")
	}
	var errs []error
	// Legacy handoff is the last forward operation, so restore it first.
	for i := len(snapshot.Legacy) - 1; i >= 0; i-- {
		if err := compensateRuntimeLegacy(app, &snapshot.Legacy[i], checkpoint...); err != nil {
			errs = append(errs, err)
		}
	}
	for i := len(snapshot.OpenClaw) - 1; i >= 0; i-- {
		if err := compensateRuntimeOpenClaw(app, snapshot, &snapshot.OpenClaw[i]); err != nil {
			errs = append(errs, err)
		}
	}
	for i := len(snapshot.Hermes) - 1; i >= 0; i-- {
		if err := compensateRuntimeHermes(app, snapshot, &snapshot.Hermes[i]); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func runtimeHermesContentAllowed(item *runtimeHermesResource, raw []byte, missing bool) bool {
	if missing {
		return !item.Present || item.AllowAbsent
	}
	return item.Present && bytes.Equal(raw, item.Content) || len(item.AllowedContent) > 0 && bytes.Equal(raw, item.AllowedContent)
}
func runtimeHermesEntryAllowed(item *runtimeHermesResource, entry *telegramProxyJournalEntry) bool {
	if entry == nil {
		return true
	}
	if entry.Target != item.Target || !reflect.DeepEqual(entry.Identity, item.Identity) {
		return false
	}
	if !runtimeHermesContentAllowed(item, []byte(entry.ManagedContent), false) {
		return false
	}
	return entry.PendingManagedContent == "" || runtimeHermesContentAllowed(item, []byte(entry.PendingManagedContent), false)
}

func compensateRuntimeHermes(app *App, snapshot *runtimeTelegramResources, item *runtimeHermesResource) error {
	target, err := parseSystemdTargetName(item.Target)
	if err != nil {
		return err
	}
	if err := verifyTelegramUserIdentity(target, item.Identity); err != nil {
		return err
	}
	journal, err := app.loadTelegramProxyJournal()
	if err != nil {
		return err
	}
	if journal.Generation < snapshot.HermesGeneration {
		return errors.New("网关Hermes journal generation回退")
	}
	entry := journal.Targets[item.Target]
	if !runtimeHermesEntryAllowed(item, entry) {
		return errors.New("网关Hermes当前ownership超出事务范围")
	}
	path, err := app.telegramManagedArtifactPath(target, item.Identity)
	if err != nil {
		return err
	}
	raw, err := readTelegramManagedArtifact(target, item.Identity, path, maxTelegramManagedContentBytes)
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		return err
	}
	// A system CAS may have durably quarantined the old file before crashing.
	// Resume only the fixed artifact and only receipt-owned quarantine bytes.
	quarantined := false
	if missing && !item.AllowAbsent && item.Present && !target.UserMode && entry != nil {
		quarantine := filepath.Join(filepath.Dir(path), telegramSystemQuarantineName(filepath.Base(path)))
		retained, readErr := telegramReadSystemArtifact(quarantine, maxTelegramManagedContentBytes)
		if readErr == nil && runtimeHermesContentAllowed(item, retained, false) {
			raw = retained
			missing = false
			quarantined = true
		} else if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
	}
	if !runtimeHermesContentAllowed(item, raw, missing) {
		return errors.New("网关Hermes drop-in已在事务外修改")
	}
	sameFile := !quarantined && missing == !item.Present && bytes.Equal(raw, item.Content)
	if sameFile && reflect.DeepEqual(entry, item.Before) {
		return nil
	}
	// Persist service reconciliation before restoring a file. A retry sees this
	// phase even after the before-image has already reached the filesystem.
	if item.Before != nil {
		desired, err := cloneRuntimeValue(item.Before)
		if err != nil {
			return err
		}
		desired.Phase = telegramPhasePrepared
		desired.PendingManagedContent = string(item.Content)
		if !reflect.DeepEqual(entry, desired) {
			journal.Targets[item.Target] = desired
			if err := app.saveTelegramProxyJournal(journal); err != nil {
				return err
			}
		}
		if !sameFile {
			if err := writeTelegramManagedArtifact(target, item.Identity, path, raw, missing, item.Content); err != nil {
				return err
			}
		}
		if err := runtimeReconcileHermes(app, target, item); err != nil {
			return err
		}
		if !item.SkipRestart && telegramTargetUnitInstalled(target) {
			return app.commitHermesTelegramApply(target)
		}
		journal, err = app.loadTelegramProxyJournal()
		if err != nil {
			return err
		}
		if !runtimeHermesEntryAllowed(item, journal.Targets[item.Target]) {
			return errors.New("网关Hermes恢复期间ownership变化")
		}
		actual, err := readTelegramManagedArtifact(target, item.Identity, path, maxTelegramManagedContentBytes)
		if err != nil {
			return err
		}
		if !bytes.Equal(actual, item.Content) {
			return errors.New("网关Hermes恢复期间文件变化")
		}
		journal.Targets[item.Target], err = cloneRuntimeValue(item.Before)
		if err != nil {
			return err
		}
		return app.saveTelegramProxyJournal(journal)
	}
	if entry == nil && missing {
		return nil
	}
	if entry == nil {
		return errors.New("网关Hermes候选文件没有持久ownership")
	}
	if entry.Phase != telegramPhaseRestoring {
		entry.Phase = telegramPhaseRestoring
		if err := app.saveTelegramProxyJournal(journal); err != nil {
			return err
		}
	}
	if _, err := removeTelegramManagedArtifact(target, item.Identity, path, raw, missing, telegramOwnedContentCandidates(entry)...); err != nil {
		return err
	}
	if err := runtimeReconcileHermes(app, target, item); err != nil {
		return err
	}
	return app.commitHermesTelegramRestore(target)
}

func runtimeOpenClawValueAllowed(item *runtimeOpenClawResource, value json.RawMessage, present bool) bool {
	return proxyStatesEqual(value, present, item.Value, item.Present) || item.AllowedProxy != "" && proxyStateMatchesString(value, present, item.AllowedProxy) || item.Before != nil && proxyStatesEqual(value, present, item.Before.OriginalValue, item.Before.OriginalPresent)
}
func runtimeOpenClawEntryAllowed(item *runtimeOpenClawResource, entry *openClawProxyJournalEntry) bool {
	if entry == nil {
		return true
	}
	if entry.User != item.User || !samePersistedUserIdentity(entry.Identity, item.Identity) {
		return false
	}
	original, present, lexeme, channels, telegram := item.Value, item.Present, item.Lexeme, item.ChannelsPresent, item.TelegramPresent
	if item.Before != nil {
		original, present, lexeme, channels, telegram = item.Before.OriginalValue, item.Before.OriginalPresent, item.Before.OriginalLexeme, item.Before.OriginalChannelsPresent, item.Before.OriginalTelegramPresent
	}
	if !proxyStatesEqual(entry.OriginalValue, entry.OriginalPresent, original, present) || entry.OriginalLexeme != lexeme || entry.OriginalChannelsPresent != channels || entry.OriginalTelegramPresent != telegram {
		return false
	}
	for _, key := range entry.Targets {
		if !containsString(item.Targets, key) && (item.Before == nil || !containsString(item.Before.Targets, key)) {
			return false
		}
	}
	for _, proxy := range []string{entry.ManagedValue, entry.PendingManagedValue} {
		if proxy == "" {
			continue
		}
		raw, _ := json.Marshal(proxy)
		if !runtimeOpenClawValueAllowed(item, raw, true) {
			return false
		}
	}
	return true
}

func compensateRuntimeOpenClaw(app *App, snapshot *runtimeTelegramResources, item *runtimeOpenClawResource) error {
	if err := verifyOpenClawUserIdentity(item.User, item.Identity); err != nil {
		return err
	}
	journal, err := app.loadOpenClawProxyJournal()
	if err != nil {
		return err
	}
	if journal.Generation < snapshot.OpenClawGeneration {
		return errors.New("用户OpenClaw journal generation回退")
	}
	entry := journal.Users[item.User]
	if !runtimeOpenClawEntryAllowed(item, entry) {
		return errors.New("用户OpenClaw当前ownership超出事务范围")
	}
	path, err := app.openClawConfigPath(item.User, item.Identity)
	if err != nil {
		return err
	}
	raw, err := readOpenClawUserConfig(item.User, item.Identity, path)
	if err != nil {
		return err
	}
	value, present, _, _, err := openClawTelegramProxyState(raw)
	if err != nil {
		return err
	}
	if !runtimeOpenClawValueAllowed(item, value, present) {
		return errors.New("用户OpenClaw代理值已在事务外修改")
	}
	sameValue := proxyStatesEqual(value, present, item.Value, item.Present)
	if sameValue && reflect.DeepEqual(entry, item.Before) {
		return nil
	}
	desired, changed, err := replaceOpenClawTelegramProxySourceWithStructure(raw, item.Value, []byte(item.Lexeme), item.Present, true, item.ChannelsPresent, item.TelegramPresent)
	if err != nil {
		return err
	}
	var recovery *openClawProxyJournalEntry
	if item.Before != nil {
		recovery, err = cloneRuntimeValue(item.Before)
		if err != nil {
			return err
		}
		for _, key := range item.Targets {
			recovery.Targets = appendUniqueString(recovery.Targets, key)
		}
		recovery.Phase = openClawPhasePrepared
		recovery.PendingManagedValue = item.Before.ManagedValue
		recovery.PendingTargets = slices.Clone(item.Targets)
		// Preserve progress from this exact compensation, not from a candidate apply.
		if entry != nil && entry.Phase == openClawPhasePrepared && entry.PendingManagedValue == recovery.PendingManagedValue && sameValue {
			recovery.PendingTargets = slices.Clone(entry.PendingTargets)
		}
	} else {
		if entry == nil {
			return errors.New("用户OpenClaw候选变更没有持久ownership")
		}
		recovery, err = cloneRuntimeValue(entry)
		if err != nil {
			return err
		}
		recovery.Phase = openClawPhaseRestoring
		recovery.PendingManagedValue = ""
		recovery.PendingTargets = nil
	}
	if !reflect.DeepEqual(entry, recovery) {
		journal.Users[item.User] = recovery
		if err := app.saveOpenClawProxyJournal(journal); err != nil {
			return err
		}
	}
	if changed {
		if err := writeOpenClawUserConfigCAS(item.User, item.Identity, path, raw, desired); err != nil {
			return err
		}
	}
	targets := item.Targets
	if item.Before != nil {
		targets = slices.Clone(recovery.PendingTargets)
	}
	for _, key := range targets {
		target, err := parseSystemdTargetName(key)
		if err != nil {
			return err
		}
		if containsString(item.SkipRestartTargets, key) || !telegramTargetUnitInstalled(target) {
			if item.Before != nil {
				if err := runtimeCommitAbsentOpenClawTarget(app, target, item); err != nil {
					return err
				}
			}
			continue
		}
		if err := openClawValidateTargetRuntime(target, item.Identity); err != nil {
			return err
		}
		if err := app.restartTelegramTarget(target); err != nil {
			return err
		}
		if item.Before != nil {
			if err := app.commitOpenClawTelegramProxyApply(target); err != nil {
				return err
			}
		}
	}
	// Read the latest generation after per-target commits. Only this user changes;
	// unrelated users and the newest journal generation remain intact.
	journal, err = app.loadOpenClawProxyJournal()
	if err != nil {
		return err
	}
	actual, err := readOpenClawUserConfig(item.User, item.Identity, path)
	if err != nil {
		return err
	}
	finalValue, finalPresent, err := openClawTelegramProxyRawValue(actual)
	if err != nil {
		return err
	}
	if !proxyStatesEqual(finalValue, finalPresent, item.Value, item.Present) {
		return errors.New("用户OpenClaw代理在恢复服务后已变化，保留ownership")
	}
	if !runtimeOpenClawEntryAllowed(item, journal.Users[item.User]) {
		return errors.New("用户OpenClaw恢复期间ownership已变化")
	}
	if item.Before == nil {
		delete(journal.Users, item.User)
	} else {
		journal.Users[item.User], err = cloneRuntimeValue(item.Before)
		if err != nil {
			return err
		}
	}
	return app.saveOpenClawProxyJournal(journal)
}

func runtimeCheckpoint(callbacks []func() error) error {
	for _, fn := range callbacks {
		if fn != nil {
			if err := fn(); err != nil {
				return err
			}
		}
	}
	return nil
}
func compensateRuntimeLegacy(app *App, item *runtimeLegacyResource, checkpoint ...func() error) error {
	target, err := parseSystemdTargetName(item.Target)
	if err != nil {
		return err
	}
	path := telegramLegacySystemPath(app.cfg, target.Service)
	raw, err := telegramReadSystemArtifact(path, maxTelegramManagedContentBytes)
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		return err
	}
	if !missing && (!item.Present || !bytes.Equal(raw, item.Content)) {
		return errors.New("旧Telegram文件已在事务外修改")
	}
	changed := missing && item.Present
	if !changed && !item.RecoveryPending {
		return nil
	}
	if !item.RecoveryPending {
		item.RecoveryPending = true
		if err := runtimeCheckpoint(checkpoint); err != nil {
			return err
		}
	}
	if changed {
		if err := telegramCreateSystemArtifact(path, item.Content, 0o644); err != nil {
			return err
		}
	}
	if err := app.reloadAndRestartTelegramArtifactTarget(target); err != nil {
		return err
	}
	item.RecoveryPending = false
	return runtimeCheckpoint(checkpoint)
}

// The frozen release plan may refer to an absent or masked unit. Restoring its
// owned file never grants permission to restart a service that appears later.
func runtimeReconcileHermes(app *App, target systemdTargetName, item *runtimeHermesResource) error {
	if item.SkipRestart {
		return app.reloadHermesTelegramTargetManager(target, item.Identity)
	}
	return app.reloadValidateAndRestartManagedHermesTarget(target, item.Identity, item.Before == nil)
}
func runtimeCommitAbsentOpenClawTarget(app *App, target systemdTargetName, item *runtimeOpenClawResource) error {
	return withFileLock(app.openClawJournalLockPath(), func() error {
		journal, err := app.loadOpenClawProxyJournal()
		if err != nil {
			return err
		}
		entry := journal.Users[item.User]
		key := canonicalTelegramTargetName(target)
		if entry == nil || entry.Phase != openClawPhasePrepared || entry.PendingManagedValue != item.Before.ManagedValue || !containsString(entry.PendingTargets, key) || !runtimeOpenClawEntryAllowed(item, entry) {
			return errors.New("用户OpenClaw缺失服务的恢复ownership变化")
		}
		path, err := app.openClawConfigPath(item.User, item.Identity)
		if err != nil {
			return err
		}
		raw, err := readOpenClawUserConfig(item.User, item.Identity, path)
		if err != nil {
			return err
		}
		value, present, err := openClawTelegramProxyRawValue(raw)
		if err != nil {
			return err
		}
		if !proxyStatesEqual(value, present, item.Value, item.Present) {
			return errors.New("用户OpenClaw缺失服务的恢复代理值变化")
		}
		entry.PendingTargets = removeString(entry.PendingTargets, key)
		if len(entry.PendingTargets) == 0 {
			entry.Phase = openClawPhaseActive
			entry.ManagedValue = entry.PendingManagedValue
			entry.PendingManagedValue = ""
		}
		return app.saveOpenClawProxyJournal(journal)
	})
}
