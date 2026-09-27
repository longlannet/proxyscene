package manager

import (
	"errors"
	"fmt"
	"os"
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
	mode := storeRuntimeSyncGlobal
	switch scene {
	case SceneGlobal:
	case SceneDev:
		mode = storeRuntimeSyncDev
	case SceneTelegram:
		mode = storeRuntimeSyncTelegram
	default:
		return fmt.Errorf("未知场景：%s", scene)
	}
	if err := a.commitStoreMutation(st, func(candidate *Store) error { candidate.SceneEnabled[scene] = enabled; return nil }, mode); err != nil {
		return err
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
	priorOwnership, err := a.loadDevBackup()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	rejectNewOwnership := func(reason error) error {
		var cleanupErr error
		if priorOwnership == nil {
			cleanupErr = devRemoveBackup(a.cfg.DevBackupPath())
		} else {
			cleanupErr = a.writeDevBackup(priorOwnership)
		}
		return errors.Join(reason, wrapRollbackError("撤销尚未应用的开发代理 ownership", cleanupErr))
	}
	if err := a.backupDevConfig(user); err != nil {
		return rejectNewOwnership(err)
	}
	backup, err := a.loadDevBackup()
	if err != nil {
		return rejectNewOwnership(err)
	}
	if backup.User != user {
		return rejectNewOwnership(fmt.Errorf("开发代理备份用户 %s 与目标用户 %s 不匹配", backup.User, user))
	}
	if err := verifyDevUserIdentity(user, backup.Identity); err != nil {
		return rejectNewOwnership(err)
	}
	proxy := a.cfg.HTTPAddr(SceneDev)
	snapshot, err := snapshotDevProxyConfig(user, backup, gitAvailable, npmAvailable)
	if err != nil {
		return rejectNewOwnership(err)
	}

	// Newly acquired ownership and the apply snapshot must describe the same
	// proxy values. Otherwise an edit between the two reads would be overwritten
	// while the eventual restore still used the earlier ownership values.
	if gitAvailable && (priorOwnership == nil || !priorOwnership.GitManaged) &&
		(!slices.Equal(snapshot.GitHTTPProxy, backup.GitHTTPProxy) || !slices.Equal(snapshot.GitHTTPSProxy, backup.GitHTTPSProxy)) {
		return rejectNewOwnership(fmt.Errorf("开发代理：Git 原值在首次备份后变化，拒绝覆盖：%w", errUserFileChanged))
	}
	if npmAvailable && (priorOwnership == nil || !priorOwnership.NPMManaged) &&
		(!optionalStringsEqual(snapshot.NPMProxy, backup.NPMProxy) || !optionalStringsEqual(snapshot.NPMHTTPSProxy, backup.NPMHTTPSProxy)) {
		return rejectNewOwnership(fmt.Errorf("npm 原值在首次备份后变化，拒绝覆盖：%w", errUserFileChanged))
	}
	if priorOwnership != nil {
		managedHTTP := managedDevProxyValues(backup, true, proxy)
		managedHTTPS := managedDevProxyValues(backup, false, proxy)
		if gitAvailable && priorOwnership.GitManaged &&
			(!devGitApplyValueKnown(snapshot.GitHTTPProxy, backup.GitHTTPProxy, managedHTTP) ||
				!devGitApplyValueKnown(snapshot.GitHTTPSProxy, backup.GitHTTPSProxy, managedHTTPS)) {
			return rejectNewOwnership(fmt.Errorf("开发代理：Git 代理已被用户修改，请先停用再重新启用开发代理：%w", errUserFileChanged))
		}
		if npmAvailable && priorOwnership.NPMManaged &&
			(!devNPMApplyValueKnown(snapshot.NPMProxy, backup.NPMProxy, managedHTTP) ||
				!devNPMApplyValueKnown(snapshot.NPMHTTPSProxy, backup.NPMHTTPSProxy, managedHTTPS)) {
			return rejectNewOwnership(fmt.Errorf("npm 代理已被用户修改，请先停用再重新启用开发代理：%w", errUserFileChanged))
		}
	}
	rollback := func() error {
		return a.restoreDevProxySnapshotDurable(snapshot, proxy)
	}
	appliedSteps := 0
	apply := func(err error) error {
		if err == nil {
			return nil
		}
		if appliedSteps == 0 && !errors.Is(err, errDevConfigMutationUncertain) {
			return rejectNewOwnership(err)
		}
		return errors.Join(err, wrapRollbackError("恢复开发代理本轮修改", rollback()))
	}
	if gitAvailable {
		if !backup.GitManaged {
			return rejectNewOwnership(fmt.Errorf("开发代理 ownership 备份未绑定 Git global 配置"))
		}
		if err := devMutateGitConfig(user, backup.Identity, backup.GitConfigLocation, snapshot.GitHTTPProxy, "--replace-all", "--", "http.proxy", proxy); err != nil {
			return apply(err)
		}
		appliedSteps++
		if err := devValidateGitTopology(user, backup.Identity, backup.GitConfigLocation); err != nil {
			return apply(err)
		}
		if err := verifyManagedGitProxy(user, backup.Identity, backup.GitConfigLocation, "http.proxy", proxy); err != nil {
			return apply(err)
		}
		if err := devMutateGitConfig(user, backup.Identity, backup.GitConfigLocation, snapshot.GitHTTPSProxy, "--replace-all", "--", "https.proxy", proxy); err != nil {
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
		if err := devMutateNPMConfig(user, backup.Identity, "proxy", snapshot.NPMProxy, &proxy); err != nil {
			return apply(err)
		}
		appliedSteps++
		if err := devMutateNPMConfig(user, backup.Identity, "https-proxy", snapshot.NPMHTTPSProxy, &proxy); err != nil {
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
	identity, hermesOwned, openClawOwned, err := a.telegramTargetOwnership(target)
	if err != nil {
		return err
	}
	if hermesOwned && openClawOwned {
		return fmt.Errorf("目标 %s 的代理 ownership 类型冲突", canonicalTelegramTargetName(target))
	}
	if target.UserMode && !hermesOwned && !openClawOwned {
		return fmt.Errorf("目标 %s 缺少持久 ownership 身份，拒绝操作用户服务", canonicalTelegramTargetName(target))
	}
	running, err := telegramTargetRunning(target, identity)
	if err != nil {
		return err
	}
	if !running {
		fmt.Printf("目标 %s 未运行，仅保存配置；尚未验证 Telegram 连接\n", canonicalTelegramTargetName(target))
		return nil
	}
	// Check the message policy after the final running-state decision. A gateway
	// that was stopped during preflight may have started in the meantime.
	if !openClawOwned {
		if err := telegramValidateHermesRestartPolicy(target, identity); err != nil {
			return err
		}
	}
	if target.UserMode {
		err = userSystemctlRun(target.User, identity, "重启用户级服务 "+target.Service, "try-restart", "--", target.Service)
	} else {
		err = systemctlRun("重启系统级服务 "+target.Service, "try-restart", "--", target.Service)
	}
	if err != nil {
		return err
	}
	return confirmTelegramTargetRunning(target, identity)
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
	return append(telegramProxyEnvPairs(cfg), "PYTHONSAFEPATH=1", hermesDisableFallbackEnv+"=1")
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
	reloadPlans := map[string]*openClawTelegramReloadPlan{}
	for _, target := range targets {
		if telegramUserUnitExists(target.User, target.Service) {
			reloadPlans[canonicalTelegramTargetName(target)] = a.prepareOpenClawTelegramReload(target)
		}
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
			if err := a.reconcileOpenClawTelegramTarget(target, reloadPlans[canonicalTelegramTargetName(target)]); err != nil {
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

func (a *App) cleanupStaleTelegramOwnership(st *Store, plan *telegramPlan, ready map[string]bool) error {
	if plan == nil {
		return fmt.Errorf("电报代理清理缺少明确授权计划")
	}
	var errs []error
	processedUsers := map[string]bool{}
	for _, release := range plan.releases {
		target := release.target
		key := canonicalTelegramTargetName(target)
		if release.openClaw {
			if processedUsers[target.User] {
				continue
			}
			processedUsers[target.User] = true
			if _, err := a.cleanupManagedOpenClawTargets(target, true); err != nil {
				errs = append(errs, fmt.Errorf("退管 OpenClaw Telegram 目标 %s 失败：%w", key, err))
			}
		} else if err := a.cleanupHermesTelegramTarget(target); err != nil {
			errs = append(errs, fmt.Errorf("退管 Hermes Telegram 目标 %s 失败：%w", key, err))
		}
	}
	// Preserve every legacy record unless this plan explicitly authorizes its
	// release, or its selected replacement has completed the required apply.
	preserve, handoffReady := map[string]bool{}, map[string]bool{}
	if st != nil {
		for _, key := range st.TelegramTargets {
			if target, err := parseSystemdTargetName(key); err == nil {
				preserve[canonicalTelegramTargetName(target)] = true
			}
		}
	}
	for _, target := range plan.legacyReleases {
		preserve[canonicalTelegramTargetName(target)] = false
	}
	for _, target := range plan.legacyHandoffs {
		key := canonicalTelegramTargetName(target)
		handoffReady[key] = ready[key]
	}
	legacyStore := cloneStore(st)
	legacyStore.RuntimeConfig = plan.observed.legacyRuntime
	if err := a.cleanupLegacyTelegramTargets(legacyStore, preserve, handoffReady); err != nil {
		errs = append(errs, err)
	}
	if st != nil {
		st.TelegramTargets = legacyStore.TelegramTargets
	}
	return errors.Join(errs...)
}
