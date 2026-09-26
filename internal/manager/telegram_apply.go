package manager

import (
	"errors"
	"fmt"
)

type telegramApplyAction uint8

const (
	telegramApplyNoAction telegramApplyAction = iota
	telegramApplyRestart
	telegramApplyOpenClaw
	telegramApplyHermes
	telegramApplyHermesRelease
)

// One operation holds the transient state of one target. Durable phases and
// crash recovery remain in the application-specific ownership journals.
type telegramTargetApply struct {
	target     systemdTargetName
	desired    bool
	owned      bool
	ready      bool
	action     telegramApplyAction
	reloadPlan *openClawTelegramReloadPlan
}

func (op *telegramTargetApply) managerKey() string {
	if op.target.UserMode {
		return "user:" + op.target.User
	}
	return "system"
}

// applyTelegram returns every target for which ownership was acquired, including
// partial successes that the caller must retain for rollback or retry.
func (a *App) applyTelegram(st *Store, targets []systemdTargetName) ([]systemdTargetName, error) {
	if targets == nil {
		var err error
		targets, err = a.telegramTargets(st, false)
		if err != nil {
			return nil, err
		}
	}
	var errs []error
	if err := warnSystemWideUserTelegramUnits(); err != nil {
		errs = append(errs, err)
	}
	operations := make([]telegramTargetApply, 0, len(targets))
	seen := map[string]bool{}
	for _, target := range targets {
		key := canonicalTelegramTargetName(target)
		if !seen[key] {
			operations = append(operations, telegramTargetApply{target: target})
			seen[key] = true
		}
	}
	// All gateways sharing a user config need their baseline before any write.
	a.prepareTelegramApplyReloads(operations)
	dropIn := []byte("[Service]\n" + telegramProxySystemdEnvironmentLines(a.cfg))
	warnedBus := map[string]bool{}
	for i := range operations {
		if err := a.prepareTelegramTargetApply(&operations[i], dropIn, warnedBus); err != nil {
			errs = append(errs, err)
		}
	}
	reloadFailures, err := a.reloadTelegramApplyManagers(operations)
	errs = append(errs, err)
	for i := range operations {
		op := &operations[i]
		if op.action == telegramApplyNoAction || reloadFailures[op.managerKey()] {
			continue
		}
		if err := a.finishTelegramTargetApply(op); err != nil {
			errs = append(errs, err)
		}
	}
	// Legacy cleanup consumes a projection of the completed operations; it does
	// not maintain another independent copy of the in-flight target state.
	desired, ready := map[string]bool{}, map[string]bool{}
	applied := []systemdTargetName{}
	for _, op := range operations {
		key := canonicalTelegramTargetName(op.target)
		desired[key], ready[key] = op.desired, op.ready
		if op.owned {
			applied = append(applied, op.target)
		}
	}
	if err := a.cleanupStaleTelegramOwnership(st, desired, ready); err != nil {
		errs = append(errs, err)
	}
	if len(applied) == 0 {
		errs = append(errs, fmt.Errorf("没有实际接管任何 OpenClaw/Hermes systemd 目标服务"))
	}
	return applied, errors.Join(errs...)
}

func (a *App) prepareTelegramApplyReloads(operations []telegramTargetApply) {
	if !a.cfg.ManageOpenClawConfig {
		return
	}
	owned, ownedErr := a.loadOpenClawProxyJournal()
	for i := range operations {
		op := &operations[i]
		target := op.target
		if !target.UserMode || !telegramUserUnitExists(target.User, target.Service) {
			continue
		}
		if ownedErr == nil {
			entry := owned.Users[target.User]
			if entry != nil && entry.Phase == openClawPhaseActive && entry.ManagedValue == a.cfg.HTTPAddr(SceneTelegram) && containsString(entry.Targets, canonicalTelegramTargetName(target)) {
				// Unchanged ownership still gets validated during apply, without
				// requiring a new channel generation or making RPC calls.
				continue
			}
		}
		if isOpenClaw, err := a.classifyOpenClawTarget(target); err == nil && isOpenClaw {
			op.reloadPlan = a.prepareOpenClawTelegramReload(target)
		}
	}
}

func (a *App) prepareTelegramTargetApply(op *telegramTargetApply, dropIn []byte, warnedBus map[string]bool) error {
	target := op.target
	key := canonicalTelegramTargetName(target)
	if target.UserMode {
		if _, err := a.verifyKnownTelegramTargetIdentity(target); err != nil {
			return fmt.Errorf("校验 Telegram 目标 %s 的持久用户身份失败：%w", key, err)
		}
		if !telegramUserUnitExists(target.User, target.Service) {
			fmt.Printf("提示：用户级目标 %s 未安装，已跳过注入；安装后请重新执行 proxyscene tg on\n", key)
			return nil
		}
		if !warnedBus[target.User] {
			warnIfUserBusMissing(target.User)
			warnedBus[target.User] = true
		}
	} else if !telegramSystemUnitExists(target.Service) {
		fmt.Printf("提示：系统级目标 %s 未安装，已跳过注入；安装后请重新执行 proxyscene tg on\n", key)
		return nil
	}
	isOpenClaw, err := a.classifyOpenClawTarget(target)
	if err != nil {
		return fmt.Errorf("识别 Telegram 目标 %s 失败：%w", key, err)
	}
	if err := a.validateTelegramTargetOwnershipClass(target, isOpenClaw); err != nil {
		// A type conflict must not turn rejection into stale-owner cleanup.
		op.desired = true
		return fmt.Errorf("校验 Telegram 目标 %s 的 ownership 类型失败：%w", key, err)
	}
	if isOpenClaw {
		if !target.UserMode {
			fmt.Printf("警告：系统级 openclaw 单元 %s 无法确定配置归属用户，已跳过，请手动设置 channels.telegram.proxy=%s\n", target.Service, a.cfg.HTTPAddr(SceneTelegram))
			return nil
		}
		if !a.cfg.ManageOpenClawConfig {
			return nil
		}
		op.desired = true
		managed, changed, err := a.applyOpenClawTelegramProxy(target, a.cfg.HTTPAddr(SceneTelegram))
		op.owned, op.ready = managed, managed && err == nil && !changed
		if changed {
			op.action = telegramApplyRestart
			if managed {
				op.action = telegramApplyOpenClaw
			}
		}
		if err != nil {
			return fmt.Errorf("设置 %s 的 OpenClaw Telegram 代理失败：%w", key, err)
		}
		return nil
	}
	op.desired = true
	prepared, err := a.prepareHermesTelegramApply(target, dropIn)
	op.owned, op.ready = prepared.managed, prepared.managed && err == nil && !prepared.reconcile
	if err == nil && prepared.reconcile {
		op.action = telegramApplyHermes
		if prepared.release {
			op.action = telegramApplyHermesRelease
		}
	}
	return err
}

// Reload each affected manager once. Runtime validation reads disk unit files;
// daemon-reload must succeed before executing any target in that manager.
func (a *App) reloadTelegramApplyManagers(operations []telegramTargetApply) (map[string]bool, error) {
	failed := map[string]bool{}
	var errs []error
	for _, op := range operations {
		if op.action == telegramApplyNoAction {
			continue
		}
		key := op.managerKey()
		if _, seen := failed[key]; seen {
			continue
		}
		target := op.target
		var err error
		if target.UserMode {
			identity, managed, identityErr := a.knownTelegramTargetIdentity(target)
			if identityErr != nil || !managed {
				if identityErr == nil {
					identityErr = fmt.Errorf("目标缺少持久 ownership 身份")
				}
				err = fmt.Errorf("重新加载用户 %s 的 systemd 配置前身份校验失败：%w", target.User, identityErr)
			} else {
				err = userSystemctlRun(target.User, identity, "重新加载用户级 systemd 配置", "daemon-reload")
			}
		} else {
			err = systemctlRun("重新加载 systemd 配置", "daemon-reload")
		}
		failed[key] = err != nil
		errs = append(errs, err)
	}
	return failed, errors.Join(errs...)
}

func (a *App) finishTelegramTargetApply(op *telegramTargetApply) error {
	target := op.target
	key := canonicalTelegramTargetName(target)
	if target.UserMode {
		managed, err := a.verifyKnownTelegramTargetIdentity(target)
		if err != nil || !managed {
			if err == nil {
				err = fmt.Errorf("目标缺少持久 ownership 身份")
			}
			return fmt.Errorf("重启 Telegram 目标 %s 前身份校验失败：%w", key, err)
		}
	}
	if op.action == telegramApplyOpenClaw {
		if err := a.reconcileOpenClawTelegramTarget(target, op.reloadPlan); err != nil {
			return err
		}
	} else if err := a.restartTelegramTarget(target); err != nil {
		return err
	}
	switch op.action {
	case telegramApplyOpenClaw:
		if err := a.commitOpenClawTelegramProxyApply(target); err != nil {
			return fmt.Errorf("提交 %s 的 OpenClaw apply journal 失败：%w", key, err)
		}
	case telegramApplyHermes:
		if err := a.commitHermesTelegramApply(target); err != nil {
			return fmt.Errorf("提交 %s 的 Hermes Telegram apply journal 失败：%w", key, err)
		}
	case telegramApplyHermesRelease:
		if err := a.commitHermesTelegramRestore(target); err != nil {
			return fmt.Errorf("释放 %s 的 Hermes Telegram ownership 失败：%w", key, err)
		}
		return nil
	default:
		return nil
	}
	op.ready = true
	return nil
}
