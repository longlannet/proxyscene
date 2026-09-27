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
)

// One operation holds the transient state of one target. Durable phases and
// crash recovery remain in the application-specific ownership journals.
type telegramTargetApply struct {
	target     systemdTargetName
	openClaw   bool
	owned      bool
	ready      bool
	failed     bool
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
	var plan *telegramPlan
	var err error
	if targets == nil {
		plan, err = a.planTelegram(st, true)
	} else {
		plan, err = a.planTelegramSelection(st, targets, true, nil)
	}
	if err != nil {
		return nil, err
	}
	return a.applyTelegramPlan(st, plan)
}

func (a *App) applyTelegramPlan(st *Store, plan *telegramPlan) ([]systemdTargetName, error) {
	if plan == nil || !plan.enabled || len(plan.selected) == 0 {
		return nil, fmt.Errorf("电报代理 应用缺少非空的已验证选择计划")
	}
	if err := a.validateTelegramPlan(st, plan); err != nil {
		return nil, err
	}
	var errs []error
	operations := make([]telegramTargetApply, 0, len(plan.selected))
	for _, selected := range plan.selected {
		operations = append(operations, telegramTargetApply{target: selected.target, openClaw: selected.openClaw})
	}
	// All gateways sharing a user config need their baseline before any write.
	a.prepareTelegramApplyReloads(operations)
	dropIn := []byte("[Service]\n" + telegramProxySystemdEnvironmentLines(a.cfg))
	warnedBus := map[string]bool{}
	for i := range operations {
		if err := a.prepareTelegramTargetApply(&operations[i], dropIn, warnedBus); err != nil {
			operations[i].failed = true
			errs = append(errs, err)
		}
	}
	if err := a.reconcileTelegramPreparedOpenClaw(operations); err != nil {
		var acquired []systemdTargetName
		for _, op := range operations {
			if op.owned {
				acquired = append(acquired, op.target)
			}
		}
		return acquired, errors.Join(append(errs, err)...)
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
	ready := map[string]bool{}
	applied := []systemdTargetName{}
	for _, op := range operations {
		ready[canonicalTelegramTargetName(op.target)] = op.ready
		if op.owned {
			applied = append(applied, op.target)
		}
	}
	if len(applied) == 0 {
		errs = append(errs, fmt.Errorf("没有实际接管任何 OpenClaw/Hermes systemd 目标服务"))
	}
	// Failed selected targets remain selected. No release or legacy handoff is
	// attempted after a failed apply; the outer transaction compensates effects.
	if err := errors.Join(errs...); err != nil {
		return applied, err
	}
	return applied, a.cleanupStaleTelegramOwnership(st, plan, ready)
}

func (a *App) prepareTelegramApplyReloads(operations []telegramTargetApply) {
	if !a.cfg.ManageOpenClawConfig {
		return
	}
	owned, ownedErr := a.loadOpenClawProxyJournal()
	changingGroups := map[string]bool{}
	if ownedErr == nil {
		for _, op := range operations {
			if op.openClaw {
				entry := owned.Users[op.target.User]
				if entry == nil || entry.Phase != openClawPhaseActive || entry.ManagedValue != a.cfg.HTTPAddr(SceneTelegram) || !containsString(entry.Targets, canonicalTelegramTargetName(op.target)) {
					changingGroups[op.target.User] = true
				}
			}
		}
	}
	for i := range operations {
		op := &operations[i]
		target := op.target
		if !target.UserMode || !telegramUserUnitExists(target.User, target.Service) {
			continue
		}
		if ownedErr == nil {
			entry := owned.Users[target.User]
			if !changingGroups[target.User] && entry != nil && entry.Phase == openClawPhaseActive && entry.ManagedValue == a.cfg.HTTPAddr(SceneTelegram) && containsString(entry.Targets, canonicalTelegramTargetName(target)) {
				// Unchanged ownership still gets validated during apply, without
				// requiring a new channel generation or making RPC calls.
				continue
			}
		}
		if op.openClaw {
			op.reloadPlan = a.prepareOpenClawTelegramReload(target)
		}
	}
}

// Later targets can prepare an entire shared group after an earlier target was
// observed unchanged. Every pending owner in the fixed selection must finish.
func (a *App) reconcileTelegramPreparedOpenClaw(operations []telegramTargetApply) error {
	selected := map[string]bool{}
	users := map[string]bool{}
	for _, op := range operations {
		if op.openClaw {
			selected[canonicalTelegramTargetName(op.target)] = true
			users[op.target.User] = true
		}
	}
	if len(users) == 0 {
		return nil
	}
	journal, err := a.loadOpenClawProxyJournal()
	if err != nil {
		return err
	}
	for user := range users {
		if entry := journal.Users[user]; entry != nil {
			for _, key := range entry.PendingTargets {
				if !selected[key] {
					return fmt.Errorf("共享 OpenClaw 配置出现计划外待协调目标 %s，保留 ownership", key)
				}
			}
		}
	}
	for i := range operations {
		op := &operations[i]
		if op.openClaw && op.owned && !op.failed {
			entry := journal.Users[op.target.User]
			if entry != nil && containsString(entry.PendingTargets, canonicalTelegramTargetName(op.target)) {
				op.action = telegramApplyOpenClaw
				op.ready = false
			}
		}
	}
	return nil
}

func (a *App) prepareTelegramTargetApply(op *telegramTargetApply, dropIn []byte, warnedBus map[string]bool) error {
	target := op.target
	key := canonicalTelegramTargetName(target)
	if target.UserMode {
		if _, err := a.verifyKnownTelegramTargetIdentity(target); err != nil {
			return fmt.Errorf("校验 Telegram 目标 %s 的持久用户身份失败：%w", key, err)
		}
		if !telegramUserUnitExists(target.User, target.Service) {
			return fmt.Errorf("已选 Telegram 目标 %s 在执行前不再可用，保留 ownership", key)
		}
		if !warnedBus[target.User] {
			warnIfUserBusMissing(target.User)
			warnedBus[target.User] = true
		}
	} else if !telegramSystemUnitExists(target.Service) {
		return fmt.Errorf("已选 Telegram 目标 %s 在执行前不再可用，保留 ownership", key)
	}
	isOpenClaw := op.openClaw
	if err := a.validateTelegramTargetOwnershipClass(target, isOpenClaw); err != nil {
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
		if !managed {
			return fmt.Errorf("已选 Telegram 目标 %s 未能完成 OpenClaw 接管，保留原 ownership", key)
		}
		return nil
	}
	prepared, err := a.prepareHermesTelegramApply(target, dropIn)
	op.owned, op.ready = prepared.managed, prepared.managed && err == nil && !prepared.reconcile
	if err == nil && prepared.reconcile {
		op.action = telegramApplyHermes
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
	default:
		return nil
	}
	op.ready = true
	return nil
}
