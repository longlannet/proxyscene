package manager

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// Selection is an input to execution, never an inference from its success.
// Only the planner can authorize releasing an existing ownership record.
type telegramPlan struct {
	enabled        bool
	selected       []plannedTelegramTarget
	releases       []approvedTelegramRelease
	protected      []plannedTelegramTarget
	legacyHandoffs []systemdTargetName
	legacyReleases []systemdTargetName
	observed       telegramPlanObservation
}

type plannedTelegramTarget struct {
	target   systemdTargetName
	identity *persistedUserIdentity
	openClaw bool
	unit     telegramPlanUnit
	content  []byte
	missing  bool
}

type approvedTelegramRelease = plannedTelegramTarget

type telegramPlanUnit struct {
	resolution telegramUnitResolution
	roots      []unitSearchRoot
	content    string
	kind       telegramUnitKind
}

type telegramPlanObservation struct {
	hermes        *telegramProxyJournal
	openClaw      *openClawProxyJournal
	runtime       *RuntimeConfig
	legacy        []string
	legacyRuntime *RuntimeConfig
}

// This boundary is replaced only by lifecycle fixtures. Production always uses
// the typed resolver and retains its path evidence plus effective drop-in bytes.
var telegramInspectPlanUnit = inspectTelegramPlanUnit

func inspectTelegramPlanUnit(target systemdTargetName, identity *persistedUserIdentity) (telegramPlanUnit, error) {
	var unit telegramPlanUnit
	if target.UserMode {
		if err := validatePersistedUserIdentity(target.User, identity); err != nil {
			return unit, err
		}
		unit.roots = userUnitSearchRootSpecsFor(identity.Home, strconv.Itoa(identity.UID))
	} else {
		for _, path := range systemUnitSearchRoots() {
			unit.roots = append(unit.roots, unitSearchRoot{Path: path, Manage: true})
		}
	}
	paths := make([]string, 0, len(unit.roots))
	for _, root := range unit.roots {
		paths = append(paths, root.Path)
	}
	unit.resolution = resolveTelegramUnitInRoots(target.Service, paths)
	switch unit.resolution.State {
	case telegramUnitAbsent, telegramUnitMasked:
		return unit, nil
	case telegramUnitResolved:
		var err error
		unit.content, err = readResolvedTelegramUnitContent(unit.resolution, unit.roots)
		if err != nil {
			return unit, err
		}
		unit.kind, err = classifyTelegramUnitContent(unit.content)
		return unit, err
	default:
		return unit, fmt.Errorf("目标 %s 的有效 unit 不可确认：%v", canonicalTelegramTargetName(target), unit.resolution.Err)
	}
}

func (a *App) planTelegram(st *Store, enabled bool) (*telegramPlan, error) {
	var targets []systemdTargetName
	var err error
	if enabled {
		targets, err = a.telegramTargets(st, false)
		if err != nil {
			return nil, err
		}
	}
	return a.planTelegramSelection(st, targets, enabled, nil)
}

func (a *App) planTelegramTransition(before, candidate *Store) (*telegramPlan, error) {
	enabled := candidate.SceneEnabled[SceneTelegram]
	if !enabled {
		planningStore := cloneStore(candidate)
		if before != nil {
			planningStore.RuntimeConfig = cloneStore(before).RuntimeConfig
		}
		return a.planTelegram(planningStore, false)
	}
	targets, err := a.telegramTargets(candidate, false)
	if err != nil {
		return nil, err
	}
	authorized := map[string]bool{}
	if before != nil && before.RuntimeConfig != nil {
		selected := map[string]bool{}
		for _, target := range targets {
			selected[canonicalTelegramTargetName(target)] = true
		}
		for _, name := range before.RuntimeConfig.TGTargetServices {
			target, err := parseSystemdTargetName(name)
			if err != nil {
				return nil, err
			}
			key := canonicalTelegramTargetName(target)
			if !selected[key] && !containsString(a.cfg.TGTargetServices, name) {
				authorized[key] = true
			}
		}
		if before.RuntimeConfig.ManageOpenClawConfig && !a.cfg.ManageOpenClawConfig {
			owned, err := a.loadOpenClawProxyJournal()
			if err != nil {
				return nil, err
			}
			for _, entry := range owned.Users {
				for _, key := range entry.Targets {
					authorized[key] = true
				}
			}
		}
	}
	planningStore := cloneStore(candidate)
	if before != nil {
		planningStore.RuntimeConfig = cloneStore(before).RuntimeConfig
	}
	return a.planTelegramSelection(planningStore, targets, true, authorized)
}

func (a *App) planTelegramSelection(st *Store, targets []systemdTargetName, enabled bool, authorized map[string]bool) (*telegramPlan, error) {
	hermes, err := a.loadTelegramProxyJournal()
	if err != nil {
		return nil, err
	}
	claw, err := a.loadOpenClawProxyJournal()
	if err != nil {
		return nil, err
	}
	plan := &telegramPlan{enabled: enabled, observed: telegramPlanObservation{hermes: hermes, openClaw: claw, runtime: a.cfg.runtimeConfig()}}
	if st != nil {
		plan.observed.legacy = slices.Clone(st.TelegramTargets)
		plan.observed.legacyRuntime = cloneStore(st).RuntimeConfig
	}
	owned := map[string]systemdTargetName{}
	for key := range hermes.Targets {
		target, err := parseSystemdTargetName(key)
		if err != nil {
			return nil, err
		}
		owned[key] = target
	}
	for _, entry := range claw.Users {
		for _, key := range entry.Targets {
			target, err := parseSystemdTargetName(key)
			if err != nil {
				return nil, err
			}
			if _, conflict := owned[key]; conflict {
				return nil, fmt.Errorf("目标 %s 同时存在 Hermes 与 OpenClaw ownership", key)
			}
			owned[key] = target
		}
	}
	selected := map[string]bool{}
	for _, target := range targets {
		key := canonicalTelegramTargetName(target)
		if selected[key] {
			continue
		}
		selected[key] = true
		if authorized[key] {
			continue
		}
		item, skip, err := a.inspectTelegramPlanTarget(target, plan.observed, false)
		if err != nil {
			return nil, err
		}
		if !skip {
			plan.selected = append(plan.selected, item)
		}
	}
	ownedKeys := make([]string, 0, len(owned))
	for key := range owned {
		ownedKeys = append(ownedKeys, key)
	}
	slices.Sort(ownedKeys)
	for _, key := range ownedKeys {
		target := owned[key]
		if !enabled || authorized[key] {
			item, _, err := a.inspectTelegramPlanTarget(target, plan.observed, true)
			if err != nil {
				return nil, err
			}
			plan.releases = append(plan.releases, item)
		} else if !selected[key] {
			item, _, err := a.inspectTelegramPlanTarget(target, plan.observed, false)
			if err != nil {
				return nil, err
			}
			plan.protected = append(plan.protected, item)
		}
	}
	if enabled {
		if err := warnSystemWideUserTelegramUnits(); err != nil {
			return nil, err
		}
	}
	if enabled && len(plan.selected) == 0 {
		return nil, fmt.Errorf("没有可安全接管的 OpenClaw/Hermes systemd 目标服务")
	}
	// Shared OpenClaw config can be restored only when every recorded target is
	// explicitly released; a missing or failed selection never grants that right.
	releasing := map[string]bool{}
	for _, item := range plan.releases {
		releasing[canonicalTelegramTargetName(item.target)] = true
	}
	for _, entry := range claw.Users {
		count := 0
		for _, key := range entry.Targets {
			if releasing[key] {
				count++
			}
		}
		if count != 0 && count != len(entry.Targets) {
			return nil, fmt.Errorf("用户 %s 的共享 OpenClaw 配置未获得完整组退管授权", entry.User)
		}
		applying := map[string]bool{}
		needsWholeGroup := entry.Phase != openClawPhaseActive || entry.ManagedValue != a.cfg.HTTPAddr(SceneTelegram)
		for _, item := range plan.selected {
			if item.openClaw && item.target.User == entry.User {
				key := canonicalTelegramTargetName(item.target)
				applying[key] = true
				// Adding a target prepares the shared journal. A subsequent apply
				// can replay every owner even if the proxy URL has not changed.
				needsWholeGroup = needsWholeGroup || !containsString(entry.Targets, key)
			}
		}
		if len(applying) != 0 && needsWholeGroup {
			for _, key := range entry.Targets {
				if !applying[key] {
					return nil, fmt.Errorf("用户 %s 的共享 OpenClaw 配置变更必须同时选择全部已接管目标，缺少 %s", entry.User, key)
				}
			}
		}
	}
	for _, key := range plan.observed.legacy {
		target, err := parseSystemdTargetName(key)
		if err != nil {
			return nil, err
		}
		if !enabled || authorized[canonicalTelegramTargetName(target)] {
			if err := a.preflightLegacyTelegramTarget(st, target); err != nil {
				return nil, err
			}
			plan.legacyReleases = append(plan.legacyReleases, target)
		} else if selected[canonicalTelegramTargetName(target)] {
			if err := a.preflightLegacyTelegramTarget(st, target); err != nil {
				return nil, err
			}
			plan.legacyHandoffs = append(plan.legacyHandoffs, target)
		}
	}
	return plan, nil
}

func (a *App) inspectTelegramPlanTarget(target systemdTargetName, observed telegramPlanObservation, release bool) (plannedTelegramTarget, bool, error) {
	item := plannedTelegramTarget{target: target}
	key := canonicalTelegramTargetName(target)
	hermesEntry := observed.hermes.Targets[key]
	clawEntry := observed.openClaw.Users[target.User]
	clawOwned := target.UserMode && clawEntry != nil && containsString(clawEntry.Targets, key)
	owned := hermesEntry != nil || clawOwned
	if hermesEntry != nil && clawOwned {
		return item, false, fmt.Errorf("目标 %s 的 ownership 类型冲突", key)
	}
	if target.UserMode {
		var err error
		switch {
		case hermesEntry != nil:
			item.identity = hermesEntry.Identity
			err = verifyTelegramUserIdentity(target, item.identity)
		case clawOwned:
			item.identity = clawEntry.Identity
			err = verifyOpenClawUserIdentity(target.User, item.identity)
		default:
			lookup := telegramLookupUserIdentity
			if strings.HasPrefix(target.Service, "openclaw") {
				lookup = openClawLookupUserIdentity
			}
			item.identity, err = capturePersistedUserIdentity(target.User, lookup)
		}
		if err != nil {
			return item, false, fmt.Errorf("目标 %s 身份预检失败：%w", key, err)
		}
	}
	unit, err := telegramInspectPlanUnit(target, item.identity)
	if err != nil {
		return item, false, err
	}
	item.unit = unit
	if unit.resolution.State != telegramUnitResolved {
		if !owned && !release {
			return item, true, nil
		}
		if !release {
			return item, false, fmt.Errorf("已接管目标 %s 的有效 unit 不可用，保留 ownership", key)
		}
		if err := validateTelegramAbsentRelease(target, item.identity); err != nil {
			return item, false, err
		}
		item.openClaw = clawOwned
	} else {
		item.openClaw = unit.kind == telegramUnitOpenClaw
		if hermesEntry != nil && item.openClaw {
			return item, false, fmt.Errorf("目标 %s 当前识别为 OpenClaw，但仍由 Hermes ownership journal 持有", key)
		}
		if clawOwned && unit.kind == telegramUnitHermes {
			return item, false, fmt.Errorf("目标 %s 当前识别为 Hermes，但仍由 OpenClaw ownership journal 持有", key)
		}
		if unit.kind == telegramUnitOther {
			if owned {
				return item, false, fmt.Errorf("已接管目标 %s 不再是可验证的网关", key)
			}
			return item, true, nil
		}
	}
	if item.openClaw {
		if !target.UserMode || (!release && !a.cfg.ManageOpenClawConfig && !owned) {
			return item, true, nil
		}
		if !release && !a.cfg.ManageOpenClawConfig {
			return item, false, fmt.Errorf("目标 %s 仍有 OpenClaw ownership，但没有明确退管授权", key)
		}
		if unit.resolution.State == telegramUnitResolved {
			if err := openClawValidateTargetRuntime(target, item.identity); err != nil {
				return item, false, err
			}
		}
		if !release && clawEntry != nil && clawEntry.Phase == openClawPhaseRestoring {
			return item, false, fmt.Errorf("用户 %s 的 OpenClaw ownership 正在退管，须先完成恢复", target.User)
		}
		path, err := a.openClawConfigPath(target.User, item.identity)
		if err != nil {
			return item, false, err
		}
		item.content, err = readOpenClawUserConfig(target.User, item.identity, path)
		if errors.Is(err, os.ErrNotExist) && !owned {
			return item, true, nil
		}
		if err != nil {
			return item, false, err
		}
		if err := rejectOpenClawRuntimeConfigSelectors(item.content); err != nil {
			return item, false, err
		}
		if err := rejectOpenClawAccountProxyOverrides(item.content); err != nil {
			return item, false, err
		}
		current, present, _, _, err := openClawTelegramProxyState(item.content)
		if err != nil {
			return item, false, err
		}
		if owned && !release {
			acceptable := proxyStateMatchesString(current, present, clawEntry.ManagedValue)
			if clawEntry.Phase == openClawPhasePrepared {
				acceptable = acceptable || proxyStateMatchesString(current, present, clawEntry.PendingManagedValue)
			}
			if clawEntry.Phase != openClawPhaseActive {
				acceptable = acceptable || proxyStatesEqual(current, present, clawEntry.OriginalValue, clawEntry.OriginalPresent)
			}
			if !acceptable {
				return item, false, fmt.Errorf("目标 %s 的 OpenClaw 配置已发生并发修改", key)
			}
		}
	} else {
		if unit.resolution.State == telegramUnitResolved {
			validate := telegramValidateHermesTarget
			if release {
				validate = telegramValidateHermesReleaseTarget
			}
			if err := validate(target, item.identity, ""); err != nil {
				return item, false, err
			}
		}
		if !release && hermesEntry != nil && hermesEntry.Phase == telegramPhaseRestoring {
			return item, false, fmt.Errorf("目标 %s 的 Telegram ownership 正在退管，须先完成恢复", key)
		}
		path, err := a.telegramManagedArtifactPath(target, item.identity)
		if err != nil {
			return item, false, err
		}
		item.content, err = readTelegramManagedArtifact(target, item.identity, path, maxTelegramManagedContentBytes)
		item.missing = errors.Is(err, os.ErrNotExist)
		if err != nil && !item.missing {
			return item, false, err
		}
		if !owned && !item.missing {
			return item, false, fmt.Errorf("目标 %s 的 drop-in 已存在但没有 ownership", key)
		}
		if owned && !item.missing && !telegramContentOwnedByEntry(item.content, hermesEntry) && !release {
			return item, false, fmt.Errorf("目标 %s 的受管 drop-in 已被修改", key)
		}
		desired := []byte("[Service]\n" + telegramProxySystemdEnvironmentLines(a.cfg))
		unchanged := !release && hermesEntry != nil && hermesEntry.Phase == telegramPhaseActive && bytes.Equal(item.content, desired) && hermesEntry.ManagedContent == string(desired)
		if !release && !unchanged && unit.resolution.State == telegramUnitResolved {
			if err := validateHermesTelegramRestartSafety(target, item.identity); err != nil {
				return item, false, err
			}
		}
	}
	return item, false, nil
}

func (a *App) validateTelegramPlan(st *Store, plan *telegramPlan) error {
	if plan == nil {
		return fmt.Errorf("电报代理 操作缺少已验证计划")
	}
	if !runtimeConfigsEqual(plan.observed.runtime, a.cfg.runtimeConfig()) || (st != nil && !slices.Equal(st.TelegramTargets, plan.observed.legacy)) {
		return fmt.Errorf("电报代理 规划后的运行配置或 legacy 集合已变化")
	}
	hermes, err := a.loadTelegramProxyJournal()
	if err != nil {
		return err
	}
	claw, err := a.loadOpenClawProxyJournal()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(hermes, plan.observed.hermes) || !reflect.DeepEqual(claw, plan.observed.openClaw) {
		return fmt.Errorf("电报代理 规划后的 ownership journal 已变化")
	}
	for index, group := range [][]plannedTelegramTarget{plan.selected, plan.releases, plan.protected} {
		for _, previous := range group {
			current, skip, err := a.inspectTelegramPlanTarget(previous.target, plan.observed, index == 1)
			if err != nil {
				return err
			}
			if previous.unit.resolution.Path != "" {
				if err := validateTelegramUnitResolution(previous.unit.resolution); err != nil {
					return err
				}
			}
			if skip || !sameTelegramPlannedTarget(current, previous) {
				return fmt.Errorf("目标 %s 在 Telegram 规划后发生变化", canonicalTelegramTargetName(previous.target))
			}
		}
	}
	legacyStore := cloneStore(st)
	legacyStore.RuntimeConfig = plan.observed.legacyRuntime
	for _, target := range append(slices.Clone(plan.legacyHandoffs), plan.legacyReleases...) {
		if err := a.preflightLegacyTelegramTarget(legacyStore, target); err != nil {
			return err
		}
	}
	return nil
}

func sameTelegramPlannedTarget(current, previous plannedTelegramTarget) bool {
	return current.target == previous.target && current.openClaw == previous.openClaw &&
		reflect.DeepEqual(current.identity, previous.identity) && current.missing == previous.missing &&
		bytes.Equal(current.content, previous.content) && current.unit.kind == previous.unit.kind &&
		current.unit.content == previous.unit.content && reflect.DeepEqual(current.unit.roots, previous.unit.roots) &&
		current.unit.resolution.State == previous.unit.resolution.State &&
		current.unit.resolution.Path == previous.unit.resolution.Path &&
		current.unit.resolution.Requested == previous.unit.resolution.Requested
}

func (a *App) preflightLegacyTelegramTarget(st *Store, target systemdTargetName) error {
	key := canonicalTelegramTargetName(target)
	if st == nil || st.RuntimeConfig == nil {
		return fmt.Errorf("旧 Telegram 目标 %s 缺少历史 RuntimeConfig，拒绝协调服务或丢弃记录", key)
	}
	if target.UserMode {
		return fmt.Errorf("旧 Telegram 用户级 ownership 记录 %s 没有 uid/gid/home 身份绑定，拒绝自动迁移", key)
	}
	path := telegramLegacySystemPath(a.cfg, target.Service)
	expected := []byte("[Service]\nEnvironmentFile=-/etc/openclaw-hermes-tg-proxy.env\n")
	current, err := telegramReadSystemArtifact(path, int64(len(expected))+1)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if !bytes.Equal(current, expected) {
			return fmt.Errorf("旧 Telegram 目标 %s 的 drop-in 不符合已记录模板，保留迁移证据", key)
		}
		legacyCfg := st.RuntimeConfig.applyTo(a.cfg)
		expectedEnv := []byte(telegramProxyEnvContent(legacyCfg))
		currentEnv, err := telegramReadSystemArtifact(telegramLegacyEnvPath(), int64(len(expectedEnv))+1)
		if err != nil || !bytes.Equal(currentEnv, expectedEnv) {
			return fmt.Errorf("旧 Telegram 目标 %s 的环境文件无法与历史 RuntimeConfig 绑定，保留迁移证据", key)
		}
	}
	unit, err := telegramInspectPlanUnit(target, nil)
	if err != nil {
		return err
	}
	if unit.resolution.State != telegramUnitResolved {
		return validateTelegramAbsentRelease(target, nil)
	}
	if unit.resolution.State == telegramUnitResolved {
		if unit.kind != telegramUnitHermes {
			return fmt.Errorf("旧 Telegram 目标 %s 不是可安全协调的 Hermes 网关", key)
		}
		if err := telegramValidateHermesReleaseTarget(target, nil, path); err != nil {
			return err
		}
	}
	return nil
}

// A missing disk unit does not prove its previously loaded process stopped.
// Explicit shutdown may release its artifact only after that process is inactive.
func validateTelegramAbsentRelease(target systemdTargetName, identity *persistedUserIdentity) error {
	state, err := telegramReadServiceState(target, identity)
	if err != nil {
		return fmt.Errorf("无法确认缺失或屏蔽的 Telegram 目标 %s 已停止，保留 ownership", canonicalTelegramTargetName(target))
	}
	if state.ActiveState != "inactive" && state.ActiveState != "failed" {
		return fmt.Errorf("电报代理 目标 %s 的磁盘 unit 不可用但服务尚未确认停止，保留 ownership", canonicalTelegramTargetName(target))
	}
	switch state.LoadState {
	case "loaded", "not-found", "masked":
		return nil
	default:
		return fmt.Errorf("电报代理 目标 %s 的加载状态不明确，保留 ownership", canonicalTelegramTargetName(target))
	}
}

func (a *App) restoreTelegramPlan(st *Store, plan *telegramPlan) error {
	if plan == nil || plan.enabled {
		return fmt.Errorf("电报代理 退管缺少明确关闭计划")
	}
	if err := a.validateTelegramPlan(st, plan); err != nil {
		return err
	}
	return a.cleanupStaleTelegramOwnership(st, plan, nil)
}
