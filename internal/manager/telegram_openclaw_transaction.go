package manager

import (
	"bytes"
	"encoding/json"
	"errors"
)

var errOpenClawReloadUnsupported = errors.New("配置不支持可信频道重载确认")

// This layer owns persisted identity and journal phases. The gateway observer
// receives only the recorded identity, config path and expected config bytes;
// it cannot accept or commit a pending transaction by itself.
func (a *App) prepareOpenClawTelegramReload(target systemdTargetName) *openClawTelegramReloadPlan {
	plan := &openClawTelegramReloadPlan{target: canonicalTelegramTargetName(target), reason: "无法取得可信的频道重载确认，使用服务重启"}
	if !target.UserMode {
		return plan
	}
	identity, managed, err := a.knownTelegramTargetIdentity(target)
	if err != nil {
		return plan
	}
	if !managed {
		identity, err = capturePersistedUserIdentity(target.User, openClawLookupUserIdentity)
	}
	if err != nil || identity == nil {
		return plan
	}
	plan.identity = *identity
	if err := openClawValidateTargetRuntime(target, identity); err != nil {
		return plan
	}
	stateRaw, err := openClawReloadUnitState(target, identity)
	if err != nil {
		return plan
	}
	state, invocation, err := parseOpenClawReloadUnitState(stateRaw)
	if err != nil {
		return plan
	}
	if state != "active" {
		plan.inactive = true
		plan.reason = "服务未运行，仅保存配置；没有验证 Telegram 连接"
		return plan
	}
	content, err := effectiveTelegramTargetUnitContent(target, identity)
	if err != nil {
		return plan
	}
	starts, err := effectiveServiceExecStarts(content)
	if err != nil || len(starts) != 1 || !openClawGatewayArgv(starts[0]) {
		return plan
	}
	path, err := a.openClawConfigPath(target.User, identity)
	if err != nil {
		return plan
	}
	raw, err := readOpenClawUserConfig(target.User, identity, path)
	if err != nil {
		return plan
	}
	if rejectOpenClawRuntimeConfigSelectors(raw) != nil || rejectOpenClawAccountProxyOverrides(raw) != nil {
		return plan
	}
	command, port, err := openClawReloadCommand(starts[0], raw)
	if err != nil {
		return plan
	}
	plan.command, plan.port, plan.invocation = command, port, invocation
	return captureOpenClawReloadBaseline(target, plan, path, raw)
}

// finish coordinates the pending transaction around the read-only observer.
// Ownership, runtime binding and the exact disk bytes must still agree even
// when observation is unavailable, before the caller can fall back to restart.
func (a *App) finishOpenClawTelegramReload(target systemdTargetName, plan *openClawTelegramReloadPlan) (bool, error) {
	if plan == nil || plan.target != canonicalTelegramTargetName(target) {
		return false, nil
	}
	if !plan.inactive && (plan.reason != "" || plan.beforeHash == "") {
		return false, nil
	}
	path, expected, err := a.openClawReloadTransactionConfig(target, &plan.identity)
	if errors.Is(err, errOpenClawReloadUnsupported) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	handled := observeOpenClawTelegramReload(target, plan, path, expected)
	currentPath, current, err := a.openClawReloadTransactionConfig(target, &plan.identity)
	if errors.Is(err, errOpenClawReloadUnsupported) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if path != currentPath || !bytes.Equal(expected, current) {
		return false, errors.New("等待 OpenClaw 重载期间配置被修改，保留 ownership 以待重试")
	}
	return handled, nil
}

func (a *App) openClawReloadTransactionConfig(target systemdTargetName, identity *persistedUserIdentity) (string, []byte, error) {
	if err := verifyOpenClawUserIdentity(target.User, identity); err != nil {
		return "", nil, err
	}
	if err := openClawValidateTargetRuntime(target, identity); err != nil {
		return "", nil, err
	}
	path, err := a.openClawConfigPath(target.User, identity)
	if err != nil {
		return "", nil, err
	}
	raw, err := a.pendingOpenClawTelegramConfig(target, identity, path)
	return path, raw, err
}

func (a *App) pendingOpenClawTelegramConfig(target systemdTargetName, identity *persistedUserIdentity, path string) ([]byte, error) {
	raw, err := readOpenClawUserConfig(target.User, identity, path)
	if err != nil {
		return nil, err
	}
	if err := rejectOpenClawRuntimeConfigSelectors(raw); err != nil {
		return nil, errOpenClawReloadUnsupported
	}
	if err := rejectOpenClawAccountProxyOverrides(raw); err != nil {
		return nil, errOpenClawReloadUnsupported
	}
	err = withFileLock(a.openClawJournalLockPath(), func() error {
		journal, err := a.loadOpenClawProxyJournal()
		if err != nil {
			return err
		}
		entry := journal.Users[target.User]
		if entry == nil || entry.Identity == nil || *entry.Identity != *identity || !containsString(entry.Targets, canonicalTelegramTargetName(target)) {
			return errors.New("OpenClaw 重载目标缺少匹配的 ownership")
		}
		current, present, err := openClawTelegramProxyRawValue(raw)
		if err != nil {
			return err
		}
		var expected json.RawMessage
		var expectedPresent bool
		switch entry.Phase {
		case openClawPhasePrepared:
			expected, err = json.Marshal(entry.PendingManagedValue)
			expectedPresent = entry.PendingManagedValue != ""
		case openClawPhaseRestoring:
			expected, expectedPresent = entry.OriginalValue, entry.OriginalPresent
		default:
			return errors.New("OpenClaw 重载 ownership 未处于待应用或待恢复状态")
		}
		if err != nil || !proxyStatesEqual(current, present, expected, expectedPresent) {
			return errors.New("OpenClaw 重载配置与待提交的代理不一致")
		}
		return nil
	})
	return raw, err
}
