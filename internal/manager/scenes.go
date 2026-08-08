package manager

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

var userSystemctlRun = func(userName string, identity *persistedUserIdentity, label string, args ...string) error {
	if err := runUserSystemctlQuietPersisted(userName, identity, lookupLocalUserIdentity, args...); err != nil {
		return fmt.Errorf("%s失败：%w", label, err)
	}
	return nil
}

var (
	telegramSystemUnitExists = systemUnitExists
	telegramUserUnitExists   = userUnitExists
)

func (a *App) toggleScene(scene Scene) error {
	if err := requireRoot(); err != nil {
		return err
	}
	return a.withStoreLock(func() error {
		st, err := a.loadStore()
		if err != nil {
			return err
		}
		return a.setSceneWithStore(st, scene, !st.SceneEnabled[scene])
	})
}

func hasEnabledScene(st *Store) bool {
	if st == nil {
		return false
	}
	return st.SceneEnabled[SceneGlobal] || st.SceneEnabled[SceneDev] || st.SceneEnabled[SceneTelegram]
}

// applySavedScenesWithCleanup applies every enabled scene. During an ordinary
// boot/reconcile it also removes disabled-scene residue. A runtime-config
// transition sets cleanupDisabled=false because the old configuration was
// already cleaned before the candidate is applied; cleaning disabled scenes
// again under the candidate configuration could touch a new user or service
// that proxyscene has never owned.
func (a *App) applySavedScenesWithCleanup(st *Store, cleanupDisabled bool) error {
	var errs []error
	for _, scene := range []Scene{SceneGlobal, SceneDev, SceneTelegram} {
		if st.SceneEnabled[scene] {
			if scene == SceneTelegram {
				// Discover once so apply and ownership reconciliation use the same set.
				targets, err := a.telegramTargets(st, false)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s目标解析失败：%w", sceneName(scene), err))
					continue
				}
				_, err = a.applyTelegram(st, targets)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s应用失败：%w", sceneName(scene), err))
				}
			} else if err := a.applyScene(st, scene); err != nil {
				errs = append(errs, fmt.Errorf("%s应用失败：%w", sceneName(scene), err))
				continue
			}
		} else if cleanupDisabled || (scene == SceneTelegram && len(st.TelegramTargets) > 0) {
			if err := a.restoreScene(st, scene); err != nil {
				errs = append(errs, fmt.Errorf("%s恢复失败：%w", sceneName(scene), err))
				continue
			}
		}
	}
	return errors.Join(errs...)
}

func (a *App) reloadIfEnabled(st *Store) error {
	if !hasEnabledScene(st) {
		return nil
	}
	return a.syncXrayServiceForStore(st)
}

func (a *App) setScene(scene Scene, enabled bool) error {
	if err := requireRoot(); err != nil {
		return err
	}
	return a.withStoreLock(func() error {
		st, err := a.loadStore()
		if err != nil {
			return err
		}
		return a.setSceneWithStore(st, scene, enabled)
	})
}

func (a *App) setSceneWithStore(st *Store, scene Scene, enabled bool) error {
	if enabled && scene == SceneDev && a.cfg.DevTargetUser == "" {
		user, err := a.devTargetUser()
		if err != nil {
			return fmt.Errorf("确定开发代理持久目标用户失败：%w", err)
		}
		a.cfg.DevTargetUser = user
	}
	before := cloneStore(st)
	// Telegram 的 apply/cleanup 具有跨 reload/restart 的 durable ownership 状态，
	// 始终走统一 Store 事务，确保候选应用和回滚都能保留待重试目标。
	if scene == SceneTelegram || a.runtimeConfigDiffers(st) {
		if err := a.commitStoreMutation(st, func(candidate *Store) error {
			candidate.SceneEnabled[scene] = enabled
			return nil
		}, storeRuntimeSyncAll); err != nil {
			return err
		}
		a.printSceneChange(scene, enabled)
		return nil
	}
	a.stageRuntimeConfig(st)
	var telegramTargets []systemdTargetName
	var err error
	if enabled && scene == SceneTelegram {
		telegramTargets, err = a.telegramTargets(st, false)
		if err != nil {
			return err
		}
	}
	if enabled {
		st.SceneEnabled[scene] = true
		// syncXrayServiceForStore 已写配置、校验并启动核心服务，无需再次 startXrayService。
		if err := a.syncXrayServiceForStore(st); err != nil {
			rollbackErr := a.rollbackSceneState(st, before, scene)
			journalErr := a.persistSceneRollbackIfNeeded(st, before)
			return errors.Join(err, rollbackErr, journalErr)
		}
		// 电报场景复用上面已发现的 telegramTargets，保证应用与持久化的目标一致。
		var applyErr error
		if scene == SceneTelegram {
			_, applyErr = a.applyTelegram(st, telegramTargets)
		} else {
			applyErr = a.applyScene(st, scene)
		}
		if applyErr != nil {
			rollbackErr := a.rollbackSceneState(st, before, scene)
			journalErr := a.persistSceneRollbackIfNeeded(st, before)
			return errors.Join(applyErr, rollbackErr, journalErr)
		}
	} else {
		if err := a.restoreScene(st, scene); err != nil {
			rollbackErr := a.rollbackSceneState(st, before, scene)
			journalErr := a.persistSceneRollbackIfNeeded(st, before)
			return errors.Join(err, rollbackErr, journalErr)
		}
		st.SceneEnabled[scene] = false
		if err := a.syncXrayServiceForStore(st); err != nil {
			rollbackErr := a.rollbackSceneState(st, before, scene)
			journalErr := a.persistSceneRollbackIfNeeded(st, before)
			return errors.Join(err, rollbackErr, journalErr)
		}
	}
	if err := a.saveStore(st); err != nil {
		// 系统侧改动已生效但状态未能持久化：回滚系统侧（恢复 SceneEnabled、重新
		// 应用/恢复场景并重新同步核心服务），使磁盘状态与实际系统保持一致，避免
		// status 误报、boot-restore 重复应用已被拆除的场景。
		rollbackErr := a.rollbackSceneState(st, before, scene)
		persistErr := a.saveStore(st)
		return errors.Join(err, rollbackErr, wrapRollbackError("恢复旧状态文件", persistErr))
	}
	a.printSceneChange(scene, enabled)
	return nil
}

func (a *App) printSceneChange(scene Scene, enabled bool) {
	fmt.Printf("%s：%s\n", sceneName(scene), onOff(enabled))
	if scene == SceneGlobal && enabled {
		fmt.Println("提示：当前已打开的 shell 不会自动继承新的代理环境变量。")
		fmt.Println("如需当前 shell 立即生效，请执行：source /etc/profile.d/proxyscene-global-proxy.sh")
	}
}

func (a *App) persistSceneRollbackIfNeeded(st, before *Store) error {
	if slices.Equal(st.TelegramTargets, before.TelegramTargets) {
		return nil
	}
	if err := a.saveStore(st); err != nil {
		return wrapRollbackError("保存待重试的 Telegram 清理状态", err)
	}
	return nil
}

func (a *App) syncXrayServiceForStore(st *Store) error {
	if !hasEnabledScene(st) {
		if err := a.stopXrayService(); err != nil {
			return err
		}
		return a.clearXrayConfig()
	}
	if err := a.writeCheckedXrayConfig(st); err != nil {
		return err
	}
	return a.startXrayService()
}

func (a *App) rollbackSceneState(st, before *Store, scene Scene) error {
	restoreStore(st, before)
	var errs []error
	if before.SceneEnabled[scene] {
		if scene == SceneTelegram {
			targets, discoverErr := a.telegramTargets(before, false)
			if discoverErr != nil {
				errs = append(errs, discoverErr)
			}
			_, err := a.applyTelegram(before, targets)
			errs = append(errs, err)
		} else {
			errs = append(errs, a.applyScene(before, scene))
		}
	} else {
		errs = append(errs, a.restoreScene(before, scene))
	}
	if err := a.syncXrayServiceForStore(before); err != nil {
		errs = append(errs, fmt.Errorf("场景回滚后同步核心服务失败：%w", err))
	}
	restoreStore(st, before)
	return errors.Join(errs...)
}

func sceneName(scene Scene) string {
	switch scene {
	case SceneGlobal:
		return "全局代理"
	case SceneDev:
		return "开发代理"
	case SceneTelegram:
		return "电报服务代理"
	default:
		return string(scene)
	}
}

func (a *App) applyScene(st *Store, scene Scene) error {
	switch scene {
	case SceneGlobal:
		return a.applyGlobalWithJournal()
	case SceneDev:
		return a.applyDev()
	case SceneTelegram:
		targets, err := a.telegramTargets(st, false)
		if err != nil {
			return err
		}
		_, err = a.applyTelegram(st, targets)
		return err
	default:
		return fmt.Errorf("未知场景：%s", scene)
	}
}

func (a *App) restoreScene(st *Store, scene Scene) error {
	switch scene {
	case SceneGlobal:
		return a.restoreGlobalWithJournal()
	case SceneDev:
		return a.restoreDev()
	case SceneTelegram:
		return a.restoreTelegram(st)
	}
	return nil
}

func (a *App) applyDev() error {
	if err := a.resumePendingDevApplyRollback(); err != nil {
		return fmt.Errorf("续跑开发代理上一轮应用回滚失败：%w", err)
	}
	user, err := a.devTargetUser()
	if err != nil {
		return err
	}
	gitAvailable := devCommandExists("git")
	npmAvailable := devCommandExists("npm")
	if !gitAvailable && !npmAvailable {
		return fmt.Errorf("开发代理需要 git 或 npm，但当前都不可用")
	}
	if err := a.backupDevConfig(user); err != nil {
		return err
	}
	backup, err := a.loadDevBackup()
	if err != nil {
		return err
	}
	if backup.User != user {
		return fmt.Errorf("开发代理备份用户 %s 与目标用户 %s 不匹配", backup.User, user)
	}
	if err := verifyDevUserIdentity(user, backup.Identity); err != nil {
		return err
	}
	proxy := a.cfg.HTTPAddr(SceneDev)
	snapshot, err := snapshotDevProxyConfig(user, backup, gitAvailable, npmAvailable)
	if err != nil {
		return err
	}
	rollback := func() error {
		return a.restoreDevProxySnapshotDurable(snapshot, proxy)
	}
	appliedSteps := 0
	apply := func(err error) error {
		if err == nil {
			return nil
		}
		if appliedSteps == 0 {
			return err
		}
		return errors.Join(err, wrapRollbackError("恢复开发代理本轮修改", rollback()))
	}
	if gitAvailable {
		if !backup.GitManaged {
			return fmt.Errorf("开发代理 ownership 备份未绑定 Git global 配置")
		}
		if err := runGitConfigMutationForIdentity(user, backup.Identity, backup.GitConfigLocation, "--replace-all", "--", "http.proxy", proxy); err != nil {
			return apply(err)
		}
		appliedSteps++
		if err := devValidateGitTopology(user, backup.Identity, backup.GitConfigLocation); err != nil {
			return apply(err)
		}
		if err := verifyManagedGitProxy(user, backup.Identity, backup.GitConfigLocation, "http.proxy", proxy); err != nil {
			return apply(err)
		}
		if err := runGitConfigMutationForIdentity(user, backup.Identity, backup.GitConfigLocation, "--replace-all", "--", "https.proxy", proxy); err != nil {
			return apply(err)
		}
		appliedSteps++
		if err := devValidateGitTopology(user, backup.Identity, backup.GitConfigLocation); err != nil {
			return apply(err)
		}
		if err := verifyManagedGitProxy(user, backup.Identity, backup.GitConfigLocation, "https.proxy", proxy); err != nil {
			return apply(err)
		}
	} else {
		fmt.Println("提示：未找到 git，跳过 git 代理设置")
	}
	if npmAvailable {
		if err := runDevAsPersistedUser(user, backup.Identity, "npm", "config", "set", "proxy", proxy); err != nil {
			return apply(err)
		}
		appliedSteps++
		if err := runDevAsPersistedUser(user, backup.Identity, "npm", "config", "set", "https-proxy", proxy); err != nil {
			return apply(err)
		}
		appliedSteps++
	} else {
		fmt.Println("提示：未找到 npm，跳过 npm 代理设置")
	}
	return nil
}

func verifyManagedGitProxy(user string, identity *persistedUserIdentity, location devGitConfigLocation, key, proxy string) error {
	values, err := getGitConfigAllForIdentity(user, identity, location, key)
	if err != nil {
		return fmt.Errorf("验证 git %s 受管值失败：%w", key, err)
	}
	if !slices.Equal(values, []string{proxy}) {
		return fmt.Errorf("git %s 写入后不是唯一受管值，拒绝继续：%v", key, values)
	}
	return nil
}

func snapshotDevProxyConfig(user string, ownership *devProxyBackup, gitAvailable, npmAvailable bool) (*devProxyBackup, error) {
	if ownership == nil || ownership.Identity == nil {
		return nil, fmt.Errorf("开发代理 ownership 备份缺失")
	}
	identity := ownership.Identity
	snapshot := &devProxyBackup{Version: devProxyBackupVersion, User: user, Identity: identity, ToolsRecorded: true, GitManaged: gitAvailable, NPMManaged: npmAvailable}
	var err error
	if gitAvailable {
		if !ownership.GitManaged {
			return nil, fmt.Errorf("开发代理 ownership 备份未绑定 Git global 配置")
		}
		snapshot.GitConfigLocation = ownership.GitConfigLocation
		if err := devValidateGitTopology(user, identity, snapshot.GitConfigLocation); err != nil {
			return nil, err
		}
		if snapshot.GitHTTPProxy, err = getGitConfigAllForIdentity(user, identity, snapshot.GitConfigLocation, "http.proxy"); err != nil {
			return nil, fmt.Errorf("读取 git http.proxy 当前值失败：%w", err)
		}
		if snapshot.GitHTTPSProxy, err = getGitConfigAllForIdentity(user, identity, snapshot.GitConfigLocation, "https.proxy"); err != nil {
			return nil, fmt.Errorf("读取 git https.proxy 当前值失败：%w", err)
		}
	}
	if npmAvailable {
		if snapshot.NPMProxy, err = getNPMConfigForIdentity(user, identity, "proxy"); err != nil {
			return nil, fmt.Errorf("读取 npm proxy 当前值失败：%w", err)
		}
		if snapshot.NPMHTTPSProxy, err = getNPMConfigForIdentity(user, identity, "https-proxy"); err != nil {
			return nil, fmt.Errorf("读取 npm https-proxy 当前值失败：%w", err)
		}
	}
	if err := validateDevProxyBackup(snapshot); err != nil {
		return nil, fmt.Errorf("开发代理应用快照无效，拒绝修改用户配置：%w", err)
	}
	return snapshot, nil
}

// applyTelegram applies every usable target and returns the targets for which a
// durable injection/config ownership record now exists. Errors are aggregated so
// callers can roll back every partial success instead of losing its identity.
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
	dropIn := []byte("[Service]\n" + telegramProxySystemdEnvironmentLines(a.cfg))
	proxyURL := a.cfg.HTTPAddr(SceneTelegram)
	manageOpenClaw := a.cfg.ManageOpenClawConfig
	userManagers := map[string]systemdTargetName{}
	warnedBus := map[string]bool{}
	restart := []systemdTargetName{}
	openClawPendingCommit := map[string]bool{}
	hermesPendingCommit := map[string]telegramHermesApplyPreparation{}
	desired := map[string]bool{}
	ready := map[string]bool{}
	applied := []systemdTargetName{}
	systemReload := false
	for _, target := range targets {
		key := canonicalTelegramTargetName(target)
		if target.UserMode {
			if _, err := a.verifyKnownTelegramTargetIdentity(target); err != nil {
				errs = append(errs, fmt.Errorf("校验 Telegram 目标 %s 的持久用户身份失败：%w", key, err))
				continue
			}
			if !telegramUserUnitExists(target.User, target.Service) {
				fmt.Printf("提示：用户级目标 %s 未安装，已跳过注入；安装后请重新执行 proxyscene tg on\n", key)
				continue
			}
			if !warnedBus[target.User] {
				warnIfUserBusMissing(target.User)
				warnedBus[target.User] = true
			}
		} else if !telegramSystemUnitExists(target.Service) {
			fmt.Printf("提示：系统级目标 %s 未安装，已跳过注入；安装后请重新执行 proxyscene tg on\n", target.Service)
			continue
		}
		isOpenClaw, classifyErr := a.classifyOpenClawTarget(target)
		if classifyErr != nil {
			errs = append(errs, fmt.Errorf("识别 Telegram 目标 %s 失败：%w", key, classifyErr))
			continue
		}
		if ownershipErr := a.validateTelegramTargetOwnershipClass(target, isOpenClaw); ownershipErr != nil {
			// The target is still explicitly desired. Retain the old owner unchanged;
			// stale cleanup must not turn a type-conflict rejection into a mutation.
			desired[key] = true
			errs = append(errs, fmt.Errorf("校验 Telegram 目标 %s 的 ownership 类型失败：%w", key, ownershipErr))
			continue
		}
		if isOpenClaw {
			if target.UserMode {
				if !manageOpenClaw {
					continue
				}
				desired[key] = true
				managed, changed, err := a.applyOpenClawTelegramProxy(target, proxyURL)
				if managed {
					applied = appendUniqueTelegramTarget(applied, target)
					if err == nil && !changed {
						ready[key] = true
					}
				}
				if changed {
					// Runtime validation reads the effective unit from disk. Reload the
					// user manager before restart so systemd cannot use an older cached
					// unit than the one we validated. The map also deduplicates multiple
					// OpenClaw gateways owned by the same user manager.
					userManagers[target.User] = target
					restart = appendUniqueTelegramTarget(restart, target)
					if managed {
						openClawPendingCommit[key] = true
					}
				}
				if err != nil {
					errs = append(errs, fmt.Errorf("设置 %s 的 OpenClaw Telegram 代理失败：%w", key, err))
				}
			} else {
				fmt.Printf("警告：系统级 openclaw 单元 %s 无法确定配置归属用户，已跳过，请手动设置 channels.telegram.proxy=%s\n", target.Service, proxyURL)
			}
			continue
		}

		desired[key] = true
		prepared, err := a.prepareHermesTelegramApply(target, dropIn)
		if prepared.managed {
			applied = appendUniqueTelegramTarget(applied, target)
			if err == nil && !prepared.reconcile {
				ready[key] = true
			}
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !prepared.reconcile {
			continue
		}
		hermesPendingCommit[key] = prepared
		if target.UserMode {
			userManagers[target.User] = target
			restart = appendUniqueTelegramTarget(restart, target)
		} else {
			systemReload = true
			restart = appendUniqueTelegramTarget(restart, target)
		}
	}
	systemReloadFailed := false
	if systemReload {
		if err := systemctlRun("重新加载 systemd 配置", "daemon-reload"); err != nil {
			errs = append(errs, err)
			systemReloadFailed = true
		}
	}
	userReloadFailed := map[string]bool{}
	for userName, target := range userManagers {
		identity, managed, identityErr := a.knownTelegramTargetIdentity(target)
		if identityErr != nil || !managed {
			if identityErr == nil {
				identityErr = fmt.Errorf("目标缺少持久 ownership 身份")
			}
			errs = append(errs, fmt.Errorf("重新加载用户 %s 的 systemd 配置前身份校验失败：%w", userName, identityErr))
			userReloadFailed[userName] = true
			continue
		}
		if err := userSystemctlRun(userName, identity, "重新加载用户级 systemd 配置", "daemon-reload"); err != nil {
			errs = append(errs, err)
			userReloadFailed[userName] = true
		}
	}
	for _, target := range restart {
		key := canonicalTelegramTargetName(target)
		if (!target.UserMode && systemReloadFailed) || (target.UserMode && userReloadFailed[target.User]) {
			continue
		}
		if target.UserMode {
			managed, identityErr := a.verifyKnownTelegramTargetIdentity(target)
			if identityErr != nil || !managed {
				if identityErr == nil {
					identityErr = fmt.Errorf("目标缺少持久 ownership 身份")
				}
				errs = append(errs, fmt.Errorf("重启 Telegram 目标 %s 前身份校验失败：%w", key, identityErr))
				continue
			}
		}
		if err := a.restartTelegramTarget(target); err != nil {
			errs = append(errs, err)
			continue
		}
		if openClawPendingCommit[key] {
			if err := a.commitOpenClawTelegramProxyApply(target); err != nil {
				errs = append(errs, fmt.Errorf("提交 %s 的 OpenClaw apply journal 失败：%w", key, err))
				continue
			}
			ready[key] = true
		}
		if prepared, ok := hermesPendingCommit[key]; ok {
			if prepared.release {
				if err := a.commitHermesTelegramRestore(target); err != nil {
					errs = append(errs, fmt.Errorf("释放 %s 的 Hermes Telegram ownership 失败：%w", key, err))
				}
				continue
			}
			if err := a.commitHermesTelegramApply(target); err != nil {
				errs = append(errs, fmt.Errorf("提交 %s 的 Hermes Telegram apply journal 失败：%w", key, err))
				continue
			}
			ready[key] = true
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

func appendUniqueTelegramTarget(targets []systemdTargetName, target systemdTargetName) []systemdTargetName {
	key := canonicalTelegramTargetName(target)
	for _, existing := range targets {
		if canonicalTelegramTargetName(existing) == key {
			return targets
		}
	}
	return append(targets, target)
}

func (a *App) verifyKnownTelegramTargetIdentity(target systemdTargetName) (bool, error) {
	_, managed, err := a.knownTelegramTargetIdentity(target)
	return managed, err
}

func (a *App) knownTelegramTargetIdentity(target systemdTargetName) (*persistedUserIdentity, bool, error) {
	identity, hermesOwned, openClawOwned, err := a.telegramTargetOwnership(target)
	if err != nil {
		return nil, false, err
	}
	if hermesOwned && openClawOwned {
		return nil, false, fmt.Errorf("目标 %s 同时出现在 Hermes 与 OpenClaw ownership journal 中", canonicalTelegramTargetName(target))
	}
	return identity, hermesOwned || openClawOwned, nil
}

func (a *App) validateTelegramTargetOwnershipClass(target systemdTargetName, isOpenClaw bool) error {
	_, hermesOwned, openClawOwned, err := a.telegramTargetOwnership(target)
	if err != nil {
		return err
	}
	key := canonicalTelegramTargetName(target)
	if hermesOwned && openClawOwned {
		return fmt.Errorf("目标 %s 同时出现在 Hermes 与 OpenClaw ownership journal 中", key)
	}
	if isOpenClaw && hermesOwned {
		return fmt.Errorf("目标 %s 当前识别为 OpenClaw，但仍由 Hermes ownership journal 持有；拒绝在清理旧 ownership 前修改配置", key)
	}
	if !isOpenClaw && openClawOwned {
		return fmt.Errorf("目标 %s 当前识别为 Hermes，但仍由 OpenClaw ownership journal 持有；拒绝在清理旧 ownership 前写入 drop-in", key)
	}
	return nil
}

func (a *App) telegramTargetOwnership(target systemdTargetName) (*persistedUserIdentity, bool, bool, error) {
	key := canonicalTelegramTargetName(target)
	var hermesIdentity *persistedUserIdentity
	hermesOwned := false
	if err := withFileLock(a.telegramProxyJournalLockPath(), func() error {
		journal, err := a.loadTelegramProxyJournal()
		if err != nil {
			return err
		}
		entry := journal.Targets[key]
		if entry == nil {
			return nil
		}
		hermesOwned = true
		if target.UserMode {
			if err := verifyTelegramUserIdentity(target, entry.Identity); err != nil {
				return err
			}
			copy := *entry.Identity
			hermesIdentity = &copy
		}
		return nil
	}); err != nil {
		return nil, false, false, err
	}
	var openClawIdentity *persistedUserIdentity
	openClawOwned := false
	if target.UserMode {
		if err := withFileLock(a.openClawJournalLockPath(), func() error {
			journal, err := a.loadOpenClawProxyJournal()
			if err != nil {
				return err
			}
			entry := journal.Users[target.User]
			if entry == nil || !containsString(entry.Targets, key) {
				return nil
			}
			openClawOwned = true
			if err := verifyOpenClawUserIdentity(target.User, entry.Identity); err != nil {
				return err
			}
			copy := *entry.Identity
			openClawIdentity = &copy
			return nil
		}); err != nil {
			return nil, false, false, err
		}
	}
	if hermesOwned {
		return hermesIdentity, true, openClawOwned, nil
	}
	if openClawOwned {
		return openClawIdentity, false, true, nil
	}
	return nil, false, false, nil
}

func (a *App) restartTelegramTarget(target systemdTargetName) error {
	if err := a.validatePreparedHermesTelegramRestart(target); err != nil {
		return err
	}
	if target.UserMode {
		identity, managed, err := a.knownTelegramTargetIdentity(target)
		if err != nil {
			return err
		}
		if !managed {
			return fmt.Errorf("目标 %s 缺少持久 ownership 身份，拒绝操作用户服务", canonicalTelegramTargetName(target))
		}
		return userSystemctlRun(target.User, identity, "重启用户级服务 "+target.Service, "try-restart", "--", target.Service)
	}
	return systemctlRun("重启系统级服务 "+target.Service, "try-restart", "--", target.Service)
}

// warnSystemWideUserTelegramUnits 扫描系统级 user-unit 目录（这些单元对所有用户的
// systemctl --user 生效），若发现疑似 Telegram 客户端单元，提示无法自动判定作用用户、
// 请用 PROXYSCENE_TG_SERVICES 显式指定 user:用户名:服务名。
func warnSystemWideUserTelegramUnits() error {
	var errs []error
	for _, root := range globalUserUnitRoots() {
		if err := walkTelegramUnitFiles(root, func(path, service string) error {
			related, err := telegramRelatedUnit(path, service)
			if err != nil {
				return err
			}
			if !related {
				return nil
			}
			fmt.Printf("提示：发现系统级用户单元 %s（%s），它对所有用户的 systemctl --user 生效，无法自动判定作用用户，已跳过；如需接管请显式设置 PROXYSCENE_TG_SERVICES='user:用户名:%s'\n", service, root, service)
			return nil
		}); err != nil {
			errs = append(errs, fmt.Errorf("扫描系统级用户单元目录 %s 失败：%w", root, err))
		}
	}
	return errors.Join(errs...)
}

func telegramProxyEnvContent(cfg Config) string {
	return fmt.Sprintf("# 由 proxyscene 管理\n%s", telegramProxyEnvironmentLines(cfg))
}

// telegramProxyEnvPairs 返回旧版共享环境文件使用的 Telegram 专用代理变量。
// TELEGRAM_PROXY 是 Hermes 实际消费的变量（其 config 描述为
// "Proxy URL for Telegram connections (overrides HTTPS_PROXY)，支持 http/https/socks5"）。
// 此前一并注入的 TELEGRAM_HTTP_PROXY / TELEGRAM_HTTPS_PROXY / TELEGRAM_SOCKS_PROXY 没有任何
// 已知消费方，属冗余，已移除。绝不注入 HTTP_PROXY/ALL_PROXY 等宽口径变量——本场景只代理
// Telegram，注入通用代理会把目标服务的全部出网都导流。OpenClaw 的 TG-only 精确入口是
// channels.telegram.proxy，因此由独立配置 journal 接管，不走本环境变量列表。
func telegramProxyEnvPairs(cfg Config) []string {
	return []string{
		"TELEGRAM_PROXY=" + cfg.HTTPAddr(SceneTelegram),
	}
}

// Hermes 通过 `python -m` 启动时，Python 默认会把 WorkingDirectory 放到
// sys.path 首位。固定 PYTHONSAFEPATH 可防止 HERMES_HOME 中的同名模块遮蔽
// 已从受绑定 venv 加载的 hermes_cli；Hermes 当前要求 Python 3.11+。
func hermesManagedEnvironmentPairs(cfg Config) []string {
	return append(telegramProxyEnvPairs(cfg), "PYTHONSAFEPATH=1")
}

func telegramProxyEnvironmentLines(cfg Config) string {
	return strings.Join(telegramProxyEnvPairs(cfg), "\n") + "\n"
}

func telegramProxySystemdEnvironmentLines(cfg Config) string {
	pairs := hermesManagedEnvironmentPairs(cfg)
	lines := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		lines = append(lines, "Environment="+systemdQuote(pair))
	}
	return strings.Join(lines, "\n") + "\n"
}

func (a *App) restoreTelegram(st *Store) error {
	var errs []error
	hermesTargets, hermesErr := a.allManagedHermesTelegramTargets()
	if hermesErr != nil {
		errs = append(errs, hermesErr)
	} else {
		for _, target := range hermesTargets {
			if err := a.cleanupHermesTelegramTarget(target); err != nil {
				errs = append(errs, fmt.Errorf("清理 Hermes Telegram 目标 %s 失败：%w", canonicalTelegramTargetName(target), err))
			}
		}
	}
	openClawTargets, openClawErr := a.allManagedOpenClawTargets()
	if openClawErr != nil {
		errs = append(errs, openClawErr)
	}
	processedOpenClawUsers := map[string]bool{}
	for _, target := range openClawTargets {
		if processedOpenClawUsers[target.User] {
			continue
		}
		processedOpenClawUsers[target.User] = true
		if _, err := a.cleanupManagedOpenClawTargets(target, true); err != nil {
			errs = append(errs, fmt.Errorf("清理用户 %s 的 OpenClaw Telegram 代理失败：%w", target.User, err))
		}
	}
	if err := a.cleanupLegacyTelegramTargets(st, map[string]bool{}, map[string]bool{}); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (a *App) cleanupManagedOpenClawTargets(trigger systemdTargetName, allowShared bool) ([]systemdTargetName, error) {
	targets, err := a.managedOpenClawTargets(trigger)
	if err != nil || len(targets) == 0 {
		return targets, err
	}
	if !allowShared && len(targets) > 1 {
		return targets, fmt.Errorf("用户 %s 有多个 OpenClaw 服务共享同一配置，差集清理必须保留到完整关闭场景", trigger.User)
	}
	restartAll, err := a.prepareRestoreOpenClawTelegramProxy(trigger)
	if err != nil {
		return targets, err
	}
	var errs []error
	if restartAll {
		identity, managed, identityErr := a.knownTelegramTargetIdentity(trigger)
		if identityErr != nil || !managed {
			if identityErr == nil {
				identityErr = fmt.Errorf("目标缺少 OpenClaw ownership 身份")
			}
			return targets, fmt.Errorf("重新加载用户 %s 的 systemd 配置前身份校验失败：%w", trigger.User, identityErr)
		}
		// Match the apply path: the runtime validator reads unit files from disk,
		// so reload the user manager before restarting and committing restoration.
		// Otherwise systemd could execute an older cached unit than the one checked
		// below. A reload failure leaves the restoring journal for the next replay.
		if err := userSystemctlRun(trigger.User, identity, "重新加载用户级 systemd 配置", "daemon-reload"); err != nil {
			return targets, err
		}
		for _, target := range targets {
			managed, identityErr := a.checkOpenClawTargetIdentity(target)
			if identityErr != nil || !managed {
				if identityErr == nil {
					identityErr = fmt.Errorf("目标缺少 OpenClaw ownership 身份")
				}
				errs = append(errs, fmt.Errorf("检查用户 %s 身份失败：%w", target.User, identityErr))
				continue
			}
			if !telegramUserUnitExists(target.User, target.Service) {
				continue
			}
			if err := openClawValidateTargetRuntime(target, identity); err != nil {
				errs = append(errs, fmt.Errorf("OpenClaw 服务 %s 的恢复运行配置验证失败：%w", canonicalTelegramTargetName(target), err))
				continue
			}
			managed, identityErr = a.checkOpenClawTargetIdentity(target)
			if identityErr != nil || !managed {
				if identityErr == nil {
					identityErr = fmt.Errorf("目标缺少 OpenClaw ownership 身份")
				}
				errs = append(errs, fmt.Errorf("重启用户 %s 的 OpenClaw 服务前身份再次校验失败：%w", target.User, identityErr))
				continue
			}
			if err := a.restartTelegramTarget(target); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return targets, err
	}
	if err := a.commitOpenClawTelegramProxyRestore(trigger); err != nil {
		return targets, err
	}
	return targets, nil
}

func (a *App) cleanupStaleTelegramOwnership(st *Store, desired, ready map[string]bool) error {
	var errs []error
	hermesTargets, err := a.allManagedHermesTelegramTargets()
	if err != nil {
		errs = append(errs, err)
	} else {
		for _, target := range hermesTargets {
			key := canonicalTelegramTargetName(target)
			if desired[key] {
				continue
			}
			fmt.Printf("清理不再管理的 Hermes Telegram 目标：%s\n", key)
			if err := a.cleanupHermesTelegramTarget(target); err != nil {
				errs = append(errs, fmt.Errorf("清理 Hermes Telegram 目标 %s 失败：%w", key, err))
			}
		}
	}
	openClawTargets, err := a.allManagedOpenClawTargets()
	if err != nil {
		errs = append(errs, err)
	} else {
		processedUsers := map[string]bool{}
		for _, target := range openClawTargets {
			key := canonicalTelegramTargetName(target)
			if desired[key] || processedUsers[target.User] {
				continue
			}
			processedUsers[target.User] = true
			fmt.Printf("清理不再管理的 OpenClaw Telegram 目标：%s\n", key)
			if _, err := a.cleanupManagedOpenClawTargets(target, false); err != nil {
				errs = append(errs, fmt.Errorf("清理 OpenClaw Telegram 目标 %s 失败：%w", key, err))
			}
		}
	}
	if err := a.cleanupLegacyTelegramTargets(st, desired, ready); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func mergeStringSlices(values ...[]string) []string {
	merged := []string{}
	for _, group := range values {
		for _, value := range group {
			merged = appendUniqueString(merged, value)
		}
	}
	return merged
}
