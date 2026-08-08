package manager

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// OpenClaw 不读取 TELEGRAM_*PROXY 环境变量，其「仅代理 Telegram」的唯一开关是配置项
// channels.telegram.proxy（参见 openclaw 的 resolveTelegramDispatcherPolicy / "set
// channels.telegram.proxy in config" 报错）。因此对 openclaw 目标，proxyscene 不写无效的
// systemd env drop-in，而是直接读改其配置文件 <用户家目录>/.openclaw/openclaw.json。
//
// 接管使用 root-only journal 保存原始 proxy 的 absent/null/string 三态、托管 URL、目标身份和
// prepared/active/restoring 阶段。journal 在配置改动前落盘，服务重启成功后才提交阶段；关闭时
// 仅恢复仍由本程序持有的值，用户在托管期间改写配置则保留用户值并通过重启释放运行态旧值。

const maxOpenClawConfigBytes int64 = 8 << 20
const maxOpenClawJournalBytes int64 = 1 << 20

const (
	openClawJournalVersion = 1
	openClawPhasePrepared  = "prepared"
	openClawPhaseActive    = "active"
	openClawPhaseRestoring = "restoring"
)

var (
	openClawLookupUserIdentity = lookupLocalUserIdentity
	openClawReadUserConfig     = readUserFileNoFollow
	openClawWriteUserConfigCAS = func(user string, identity *persistedUserIdentity, path string, expected, data []byte, perm os.FileMode) error {
		return writeUserFileAtomicCASPersisted(user, identity, openClawLookupUserIdentity, path, expected, data, perm)
	}
	openClawWriteJournalFile      = writeFileAtomic
	openClawValidateTargetRuntime = validateOpenClawTargetRuntime
)

type openClawProxyJournal struct {
	Version    int                                   `json:"version"`
	Generation uint64                                `json:"generation"`
	Users      map[string]*openClawProxyJournalEntry `json:"users"`
}

type openClawProxyJournalEntry struct {
	User                      string                 `json:"user"`
	Identity                  *persistedUserIdentity `json:"identity,omitempty"`
	OriginalValue             json.RawMessage        `json:"original_value,omitempty"`
	OriginalLexeme            string                 `json:"original_lexeme,omitempty"`
	OriginalPresent           bool                   `json:"original_present"`
	OriginalStructureRecorded bool                   `json:"original_structure_recorded,omitempty"`
	OriginalChannelsPresent   bool                   `json:"original_channels_present,omitempty"`
	OriginalTelegramPresent   bool                   `json:"original_telegram_present,omitempty"`
	ManagedValue              string                 `json:"managed_value"`
	PendingManagedValue       string                 `json:"pending_managed_value,omitempty"`
	Targets                   []string               `json:"targets"`
	PendingTargets            []string               `json:"pending_targets,omitempty"`
	Phase                     string                 `json:"phase"`
}

// 与自动发现一致地优先用厂商标记判定：定位单元文件、确认带 OPENCLAW_SERVICE_MARKER=openclaw
// 且 KIND=gateway。只有在单元文件不可定位（例如来自状态文件、单元已被删除）时，才回退到按
// 单元名前缀 "openclaw" 判定。这样即使某个带标记的 openclaw 网关单元改了名，也不会被误路由到
// 对它无效的 TELEGRAM_* env 注入。
func (a *App) classifyOpenClawTarget(t systemdTargetName) (bool, error) {
	var roots []unitSearchRoot
	if t.UserMode {
		identity, err := lookupLocalUserIdentity(t.User)
		if err != nil {
			return false, err
		}
		roots = userUnitSearchRootSpecsFor(identity.Home, identity.UIDText)
	} else {
		for _, root := range systemUnitSearchRoots() {
			roots = append(roots, unitSearchRoot{Path: root, Manage: true})
		}
	}
	paths := make([]string, 0, len(roots))
	for _, root := range roots {
		paths = append(paths, root.Path)
	}
	path, ok := effectiveUnitPathInRoots(t.Service, paths)
	if !ok {
		return strings.HasPrefix(t.Service, "openclaw"), nil
	}
	content, err := readTelegramUnitContentWithDropIns(path, t.Service, roots)
	if err != nil {
		return false, err
	}
	return unitHasOpenClawGatewayMarker(content), nil
}

// systemUnitExists 报告某个系统级单元文件是否存在于 systemd 的系统单元搜索目录中。
func systemUnitExists(service string) bool {
	return unitFileExistsInRoots(service, systemUnitSearchRoots())
}

// openClawConfigPath 返回指定用户的 openclaw 配置文件默认路径。
func (a *App) openClawConfigPath(user string, identity *persistedUserIdentity) (string, error) {
	if err := validatePersistedUserIdentity(user, identity); err != nil {
		return "", err
	}
	return filepath.Join(identity.Home, ".openclaw", "openclaw.json"), nil
}

func verifyOpenClawUserIdentity(user string, identity *persistedUserIdentity) error {
	_, err := verifyPersistedUserIdentity(user, identity, openClawLookupUserIdentity)
	return err
}

func readOpenClawUserConfig(user string, identity *persistedUserIdentity, path string) ([]byte, error) {
	if err := verifyOpenClawUserIdentity(user, identity); err != nil {
		return nil, err
	}
	return openClawReadUserConfig(user, path, maxOpenClawConfigBytes)
}

func writeOpenClawUserConfigCAS(user string, identity *persistedUserIdentity, path string, expected, data []byte) error {
	if err := verifyOpenClawUserIdentity(user, identity); err != nil {
		return err
	}
	return openClawWriteUserConfigCAS(user, identity, path, expected, data, 0o600)
}

func (a *App) openClawJournalPath() string {
	return filepath.Join(a.cfg.CoreDir, "openclaw-proxy-journal.json")
}

func (a *App) openClawJournalBackupPath() string {
	return a.openClawJournalPath() + ".bak"
}

func (a *App) openClawJournalLockPath() string {
	return a.openClawJournalPath() + ".lock"
}

func newOpenClawProxyJournal() *openClawProxyJournal {
	return &openClawProxyJournal{Version: openClawJournalVersion, Users: map[string]*openClawProxyJournalEntry{}}
}

func decodeOpenClawProxyJournal(raw []byte) (*openClawProxyJournal, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	var journal *openClawProxyJournal
	if err := decoder.Decode(&journal); err != nil {
		return nil, err
	}
	if journal == nil {
		return nil, fmt.Errorf("OpenClaw 代理 journal 不能是 null")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("OpenClaw 代理 journal 含多个 JSON 值")
		}
		return nil, err
	}
	if err := validateOpenClawProxyJournal(journal); err != nil {
		return nil, err
	}
	return journal, nil
}

func validateOpenClawProxyJournal(journal *openClawProxyJournal) error {
	if journal == nil || journal.Version != openClawJournalVersion || journal.Users == nil {
		return fmt.Errorf("OpenClaw 代理 journal 版本或结构无效")
	}
	for user, entry := range journal.Users {
		if entry == nil || entry.User != user || validateUserName(user) != nil || entry.ManagedValue == "" || len(entry.Targets) == 0 {
			return fmt.Errorf("OpenClaw 代理 journal 用户记录无效：%s", user)
		}
		if err := validatePersistedUserIdentity(user, entry.Identity); err != nil {
			return fmt.Errorf("OpenClaw 代理 journal 用户身份无效（%s）：%w", user, err)
		}
		if entry.OriginalPresent {
			if err := validateOpenClawProxyRaw(entry.OriginalValue); err != nil {
				return fmt.Errorf("OpenClaw 代理 journal 原值无效（用户 %s）：%w", user, err)
			}
			if entry.OriginalLexeme != "" {
				canonical, err := canonicalOpenClawProxyValue([]byte(entry.OriginalLexeme))
				if err != nil || !proxyStatesEqual(canonical, true, entry.OriginalValue, true) {
					return fmt.Errorf("OpenClaw 代理 journal 原始 JSON5 词法值无效（用户 %s）", user)
				}
			}
		} else if len(entry.OriginalValue) != 0 {
			return fmt.Errorf("OpenClaw 代理 journal 缺失原值却携带 original_value：%s", user)
		} else if entry.OriginalLexeme != "" {
			return fmt.Errorf("OpenClaw 代理 journal 缺失原值却携带 original_lexeme：%s", user)
		}
		if entry.OriginalStructureRecorded {
			if entry.OriginalTelegramPresent && !entry.OriginalChannelsPresent {
				return fmt.Errorf("OpenClaw 代理 journal 原始容器结构无效：%s", user)
			}
			if entry.OriginalPresent && !entry.OriginalTelegramPresent {
				return fmt.Errorf("OpenClaw 代理 journal 原值存在但 telegram 容器缺失：%s", user)
			}
		}
		switch entry.Phase {
		case openClawPhasePrepared:
			if entry.PendingManagedValue == "" || len(entry.PendingTargets) == 0 {
				return fmt.Errorf("OpenClaw 代理 prepared journal 缺少 pending 值：%s", user)
			}
		case openClawPhaseActive, openClawPhaseRestoring:
			if entry.PendingManagedValue != "" || len(entry.PendingTargets) != 0 {
				return fmt.Errorf("OpenClaw 代理非 prepared journal 携带 pending 值：%s", user)
			}
		default:
			return fmt.Errorf("OpenClaw 代理 journal phase 无效：%s", entry.Phase)
		}
		for _, target := range entry.Targets {
			parsed, err := parseSystemdTargetName(target)
			if err != nil {
				return fmt.Errorf("OpenClaw 代理 journal 目标无效：%w", err)
			}
			if !parsed.UserMode || parsed.User != user {
				return fmt.Errorf("OpenClaw 代理 journal 目标与用户不匹配：%s", target)
			}
		}
		for _, target := range entry.PendingTargets {
			if !containsString(entry.Targets, target) {
				return fmt.Errorf("OpenClaw 代理 pending 目标不在 ownership 集合中：%s", target)
			}
		}
	}
	return nil
}

func (a *App) loadOpenClawProxyJournal() (*openClawProxyJournal, error) {
	load := func(path string) (*openClawProxyJournal, error) {
		raw, err := readRegularFileNoFollow(path, maxOpenClawJournalBytes)
		if err != nil {
			return nil, err
		}
		return decodeOpenClawProxyJournal(raw)
	}
	mainJournal, mainErr := load(a.openClawJournalPath())
	backupJournal, backupErr := load(a.openClawJournalBackupPath())
	if mainErr == nil && backupErr == nil {
		switch {
		case mainJournal.Generation > backupJournal.Generation:
			fmt.Println("警告：OpenClaw 代理主 journal 比备份新，使用主文件恢复")
			return mainJournal, nil
		case backupJournal.Generation > mainJournal.Generation:
			fmt.Println("警告：OpenClaw 代理备份 journal 比主文件新，使用备份恢复")
			return backupJournal, nil
		case !reflect.DeepEqual(mainJournal, backupJournal):
			return nil, fmt.Errorf("OpenClaw 代理 journal 主备在同一 generation 内容不一致")
		default:
			return mainJournal, nil
		}
	}
	if mainErr == nil {
		if !errors.Is(backupErr, os.ErrNotExist) {
			fmt.Printf("警告：OpenClaw 代理备份 journal 不可用，使用主文件：%v\n", backupErr)
		}
		return mainJournal, nil
	}
	if backupErr == nil {
		if !errors.Is(mainErr, os.ErrNotExist) {
			fmt.Printf("警告：OpenClaw 代理主 journal 不可用，使用备份：%v\n", mainErr)
		}
		return backupJournal, nil
	}
	if errors.Is(mainErr, os.ErrNotExist) && errors.Is(backupErr, os.ErrNotExist) {
		return newOpenClawProxyJournal(), nil
	}
	return nil, fmt.Errorf("OpenClaw 代理 journal 与备份均不可用：主=%v，备份=%v", mainErr, backupErr)
}

func (a *App) saveOpenClawProxyJournal(journal *openClawProxyJournal) error {
	if journal == nil {
		return fmt.Errorf("OpenClaw 代理 journal 不能是 null")
	}
	if journal.Generation == ^uint64(0) {
		return fmt.Errorf("OpenClaw 代理 journal generation 已耗尽")
	}
	journal.Version = openClawJournalVersion
	if journal.Users == nil {
		journal.Users = map[string]*openClawProxyJournalEntry{}
	}
	if err := validateOpenClawProxyJournal(journal); err != nil {
		return err
	}
	journal.Generation++
	raw, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if int64(len(raw)) > maxOpenClawJournalBytes {
		journal.Generation--
		return fmt.Errorf("OpenClaw 代理 journal 序列化后超过 %d 字节上限", maxOpenClawJournalBytes)
	}
	// The backup is the recoverable next generation. Commit the main file last so
	// a crash can leave the backup ahead of main, never silently one transaction
	// behind it. Replaying an ahead prepared/restoring record is idempotent.
	if err := openClawWriteJournalFile(a.openClawJournalBackupPath(), raw, 0o600); err != nil {
		return fmt.Errorf("保存 OpenClaw 代理 journal 备份失败：%w", err)
	}
	if err := openClawWriteJournalFile(a.openClawJournalPath(), raw, 0o600); err != nil {
		return err
	}
	return nil
}

func addOpenClawJournalTarget(entry *openClawProxyJournalEntry, target string) {
	entry.Targets = appendUniqueString(entry.Targets, target)
}

func removeString(values []string, target string) []string {
	kept := values[:0]
	for _, value := range values {
		if value != target {
			kept = append(kept, value)
		}
	}
	return kept
}

// applyOpenClawTelegramProxy durably records ownership before changing the user
// config. A prepared journal is replayable after a crash; the expected-bytes
// guard rejects a user/OpenClaw update observed immediately before replacement.
func (a *App) applyOpenClawTelegramProxy(target systemdTargetName, proxyURL string) (managed, changed bool, err error) {
	if !target.UserMode {
		return false, false, fmt.Errorf("OpenClaw 配置接管仅支持用户级目标：%s", target.Service)
	}
	user := target.User
	targetKey := canonicalTelegramTargetName(target)
	err = withFileLock(a.openClawJournalLockPath(), func() error {
		journal, err := a.loadOpenClawProxyJournal()
		if err != nil {
			return err
		}
		entry := journal.Users[user]
		var identity *persistedUserIdentity
		if entry == nil {
			identity, err = capturePersistedUserIdentity(user, openClawLookupUserIdentity)
		} else {
			identity = entry.Identity
			err = verifyOpenClawUserIdentity(user, identity)
		}
		if err != nil {
			return err
		}
		if err := openClawValidateTargetRuntime(target, identity); err != nil {
			return err
		}
		path, err := a.openClawConfigPath(user, identity)
		if err != nil {
			return err
		}
		raw, err := readOpenClawUserConfig(user, identity, path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fmt.Printf("警告：未找到用户 %s 的 openclaw 配置 %s，跳过 Telegram 代理设置（请确认 openclaw 已安装）\n", user, path)
				return nil
			}
			return err
		}
		if err := rejectOpenClawRuntimeConfigSelectors(raw); err != nil {
			return err
		}
		if err := rejectOpenClawAccountProxyOverrides(raw); err != nil {
			return err
		}
		current, present, channelsPresent, telegramPresent, err := openClawTelegramProxyState(raw)
		if err != nil {
			return fmt.Errorf("解析 openclaw 配置失败（%s）：%w", path, err)
		}
		if entry == nil {
			originalLexeme, sourcePresent, err := openClawTelegramProxyLexeme(raw)
			if err != nil {
				return fmt.Errorf("读取 openclaw proxy 原始词法值失败（%s）：%w", path, err)
			}
			if sourcePresent != present {
				return fmt.Errorf("读取 openclaw proxy 原始词法值失败（%s）：字段存在状态不一致", path)
			}
			entry = &openClawProxyJournalEntry{
				User:                      user,
				Identity:                  identity,
				OriginalValue:             cloneRawMessage(current),
				OriginalLexeme:            originalLexeme,
				OriginalPresent:           present,
				OriginalStructureRecorded: true,
				OriginalChannelsPresent:   channelsPresent,
				OriginalTelegramPresent:   telegramPresent,
				ManagedValue:              proxyURL,
				PendingManagedValue:       proxyURL,
				Targets:                   []string{targetKey},
				PendingTargets:            []string{targetKey},
				Phase:                     openClawPhasePrepared,
			}
			journal.Users[user] = entry
			if err := a.saveOpenClawProxyJournal(journal); err != nil {
				return err
			}
			managed = true
		} else {
			managed = containsString(entry.Targets, targetKey)
			if entry.Phase == openClawPhaseActive && entry.ManagedValue == proxyURL {
				if !proxyStateMatchesString(current, present, entry.ManagedValue) {
					return fmt.Errorf("用户 %s 的 OpenClaw proxy 已在托管期间被修改，拒绝覆盖", user)
				}
				if managed {
					return nil
				}
				addOpenClawJournalTarget(entry, targetKey)
				entry.PendingTargets = appendUniqueString(entry.PendingTargets, targetKey)
				entry.PendingManagedValue = proxyURL
				entry.Phase = openClawPhasePrepared
				if err := a.saveOpenClawProxyJournal(journal); err != nil {
					return err
				}
				managed = true
				changed = true
				return nil
			}

			acceptable := false
			switch entry.Phase {
			case openClawPhaseActive:
				acceptable = proxyStateMatchesString(current, present, entry.ManagedValue)
			case openClawPhasePrepared:
				acceptable = proxyStateMatchesString(current, present, entry.PendingManagedValue) ||
					proxyStateMatchesString(current, present, entry.ManagedValue)
				if entry.PendingManagedValue == entry.ManagedValue {
					acceptable = acceptable || proxyStatesEqual(current, present, entry.OriginalValue, entry.OriginalPresent)
				}
			case openClawPhaseRestoring:
				acceptable = proxyStateMatchesString(current, present, entry.ManagedValue) ||
					proxyStatesEqual(current, present, entry.OriginalValue, entry.OriginalPresent)
			}
			if !acceptable {
				return fmt.Errorf("用户 %s 的 OpenClaw proxy 在事务期间发生并发变化，拒绝覆盖", user)
			}
			addOpenClawJournalTarget(entry, targetKey)
			for _, ownedTarget := range entry.Targets {
				entry.PendingTargets = appendUniqueString(entry.PendingTargets, ownedTarget)
			}
			entry.PendingManagedValue = proxyURL
			entry.Phase = openClawPhasePrepared
			if err := a.saveOpenClawProxyJournal(journal); err != nil {
				return err
			}
			managed = true
		}

		desired := entry.PendingManagedValue
		if !proxyStateMatchesString(current, present, desired) {
			out, didChange, err := setOpenClawTelegramProxy(raw, desired)
			if err != nil {
				return err
			}
			if didChange {
				if err := writeOpenClawUserConfigCAS(user, identity, path, raw, out); err != nil {
					return fmt.Errorf("按预期原值写入 openclaw 配置失败（%s）：%w", path, err)
				}
				changed = true
			}
		} else {
			// prepared/restoring can mean the previous process changed the file but
			// crashed before restarting the service. Force the caller to replay it.
			changed = true
		}
		changed = true
		managed = true
		return nil
	})
	if err != nil {
		return managed, changed, err
	}
	if changed {
		fmt.Printf("已设置用户 %s 的 openclaw channels.telegram.proxy=%s\n", target.User, proxyURL)
	}
	return managed, changed, nil
}

func (a *App) commitOpenClawTelegramProxyApply(target systemdTargetName) error {
	if !target.UserMode {
		return nil
	}
	return withFileLock(a.openClawJournalLockPath(), func() error {
		journal, err := a.loadOpenClawProxyJournal()
		if err != nil {
			return err
		}
		entry := journal.Users[target.User]
		targetKey := canonicalTelegramTargetName(target)
		if entry == nil || !containsString(entry.PendingTargets, targetKey) {
			return nil
		}
		if entry.Phase != openClawPhasePrepared || entry.PendingManagedValue == "" {
			return fmt.Errorf("OpenClaw 代理 apply journal 状态无效：%s", target.User)
		}
		if err := verifyOpenClawUserIdentity(target.User, entry.Identity); err != nil {
			return err
		}
		if err := openClawValidateTargetRuntime(target, entry.Identity); err != nil {
			return err
		}
		path, err := a.openClawConfigPath(target.User, entry.Identity)
		if err != nil {
			return err
		}
		raw, err := readOpenClawUserConfig(target.User, entry.Identity, path)
		if err != nil {
			return err
		}
		if err := rejectOpenClawRuntimeConfigSelectors(raw); err != nil {
			return err
		}
		if err := rejectOpenClawAccountProxyOverrides(raw); err != nil {
			return err
		}
		current, present, err := openClawTelegramProxyRawValue(raw)
		if err != nil {
			return err
		}
		if !proxyStateMatchesString(current, present, entry.PendingManagedValue) {
			return fmt.Errorf("用户 %s 的 OpenClaw proxy 在重启后发生变化，拒绝提交 ownership", target.User)
		}
		entry.PendingTargets = removeString(entry.PendingTargets, targetKey)
		if len(entry.PendingTargets) == 0 {
			entry.ManagedValue = entry.PendingManagedValue
			entry.PendingManagedValue = ""
			entry.Phase = openClawPhaseActive
		}
		return a.saveOpenClawProxyJournal(journal)
	})
}

func (a *App) checkOpenClawTargetIdentity(target systemdTargetName) (bool, error) {
	if !target.UserMode {
		return false, nil
	}
	managed := false
	err := withFileLock(a.openClawJournalLockPath(), func() error {
		journal, err := a.loadOpenClawProxyJournal()
		if err != nil {
			return err
		}
		entry := journal.Users[target.User]
		if entry == nil || !containsString(entry.Targets, canonicalTelegramTargetName(target)) {
			return nil
		}
		managed = true
		return verifyOpenClawUserIdentity(target.User, entry.Identity)
	})
	return managed, err
}

func (a *App) managedOpenClawTargets(target systemdTargetName) ([]systemdTargetName, error) {
	if !target.UserMode {
		return nil, nil
	}
	var targets []systemdTargetName
	err := withFileLock(a.openClawJournalLockPath(), func() error {
		journal, err := a.loadOpenClawProxyJournal()
		if err != nil {
			return err
		}
		entry := journal.Users[target.User]
		if entry == nil || !containsString(entry.Targets, canonicalTelegramTargetName(target)) {
			return nil
		}
		for _, name := range entry.Targets {
			parsed, err := parseSystemdTargetName(name)
			if err != nil {
				return err
			}
			targets = append(targets, parsed)
		}
		return nil
	})
	return targets, err
}

func (a *App) allManagedOpenClawTargets() ([]systemdTargetName, error) {
	var targets []systemdTargetName
	err := withFileLock(a.openClawJournalLockPath(), func() error {
		journal, err := a.loadOpenClawProxyJournal()
		if err != nil {
			return err
		}
		for _, entry := range journal.Users {
			for _, name := range entry.Targets {
				parsed, err := parseSystemdTargetName(name)
				if err != nil {
					return err
				}
				targets = appendUniqueTelegramTarget(targets, parsed)
			}
		}
		return nil
	})
	return targets, err
}

// prepareRestoreOpenClawTelegramProxy restores the original value but retains a
// phase=restoring journal until the caller successfully restarts the service.
func (a *App) prepareRestoreOpenClawTelegramProxy(target systemdTargetName) (restartNeeded bool, err error) {
	if !target.UserMode {
		return false, nil
	}
	user := target.User
	targetKey := canonicalTelegramTargetName(target)
	err = withFileLock(a.openClawJournalLockPath(), func() error {
		journal, err := a.loadOpenClawProxyJournal()
		if err != nil {
			return err
		}
		entry := journal.Users[user]
		if entry == nil || !containsString(entry.Targets, targetKey) {
			return nil
		}
		if err := verifyOpenClawUserIdentity(user, entry.Identity); err != nil {
			return err
		}
		path, err := a.openClawConfigPath(user, entry.Identity)
		if err != nil {
			return err
		}
		raw, err := readOpenClawUserConfig(user, entry.Identity, path)
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("用户 %s 的 OpenClaw 配置缺失，保留 journal 以待恢复", user)
		}
		if err != nil {
			return err
		}
		current, present, channelsPresent, telegramPresent, err := openClawTelegramProxyState(raw)
		if err != nil {
			return err
		}
		if entry.Phase == openClawPhaseRestoring {
			if proxyStatesEqual(current, present, entry.OriginalValue, entry.OriginalPresent) {
				restartNeeded = !proxyStateMatchesString(entry.OriginalValue, entry.OriginalPresent, entry.ManagedValue)
				if !restartNeeded {
					delete(journal.Users, user)
					return a.saveOpenClawProxyJournal(journal)
				}
				return nil
			}
			if !proxyStateMatchesString(current, present, entry.ManagedValue) {
				// The user changed the desired final value after restoration began.
				// Preserve it and retain a restoring record until every service reloads it.
				entry.OriginalValue = cloneRawMessage(current)
				entry.OriginalLexeme, _, err = openClawTelegramProxyLexeme(raw)
				if err != nil {
					return err
				}
				entry.OriginalPresent = present
				entry.OriginalStructureRecorded = true
				entry.OriginalChannelsPresent = channelsPresent
				entry.OriginalTelegramPresent = telegramPresent
				if err := a.saveOpenClawProxyJournal(journal); err != nil {
					return err
				}
				restartNeeded = true
				return nil
			}
		}
		if entry.Phase == openClawPhasePrepared && proxyStatesEqual(current, present, entry.OriginalValue, entry.OriginalPresent) {
			restartNeeded = !proxyStateMatchesString(entry.OriginalValue, entry.OriginalPresent, entry.PendingManagedValue)
			if !restartNeeded {
				delete(journal.Users, user)
				return a.saveOpenClawProxyJournal(journal)
			}
			entry.PendingManagedValue = ""
			entry.PendingTargets = nil
			entry.Phase = openClawPhaseRestoring
			return a.saveOpenClawProxyJournal(journal)
		}
		managedCurrent := proxyStateMatchesString(current, present, entry.ManagedValue)
		if entry.Phase == openClawPhasePrepared {
			managedCurrent = managedCurrent || proxyStateMatchesString(current, present, entry.PendingManagedValue)
		}
		if !managedCurrent {
			// The user took ownership while the scene was active. Preserve the file,
			// but keep ownership until all managed services restart with the new value.
			entry.OriginalValue = cloneRawMessage(current)
			entry.OriginalLexeme, _, err = openClawTelegramProxyLexeme(raw)
			if err != nil {
				return err
			}
			entry.OriginalPresent = present
			entry.OriginalStructureRecorded = true
			entry.OriginalChannelsPresent = channelsPresent
			entry.OriginalTelegramPresent = telegramPresent
			entry.PendingManagedValue = ""
			entry.PendingTargets = nil
			entry.Phase = openClawPhaseRestoring
			if err := a.saveOpenClawProxyJournal(journal); err != nil {
				return err
			}
			restartNeeded = true
			return nil
		}
		entry.PendingManagedValue = ""
		entry.PendingTargets = nil
		entry.Phase = openClawPhaseRestoring
		if err := a.saveOpenClawProxyJournal(journal); err != nil {
			return err
		}
		restoreSource := cloneRawMessage(entry.OriginalValue)
		if entry.OriginalLexeme != "" {
			restoreSource = []byte(entry.OriginalLexeme)
		}
		out, changed, err := replaceOpenClawTelegramProxySourceWithStructure(
			raw,
			entry.OriginalValue,
			restoreSource,
			entry.OriginalPresent,
			entry.OriginalStructureRecorded,
			entry.OriginalChannelsPresent,
			entry.OriginalTelegramPresent,
		)
		if err != nil {
			return err
		}
		if changed {
			if err := writeOpenClawUserConfigCAS(user, entry.Identity, path, raw, out); err != nil {
				return err
			}
			fmt.Printf("已恢复用户 %s 的 openclaw channels.telegram.proxy 原值\n", user)
			restartNeeded = true
			return nil
		}
		delete(journal.Users, user)
		return a.saveOpenClawProxyJournal(journal)
	})
	return restartNeeded, err
}

func (a *App) commitOpenClawTelegramProxyRestore(target systemdTargetName) error {
	if !target.UserMode {
		return nil
	}
	return withFileLock(a.openClawJournalLockPath(), func() error {
		journal, err := a.loadOpenClawProxyJournal()
		if err != nil {
			return err
		}
		entry := journal.Users[target.User]
		if entry == nil {
			return nil
		}
		if entry.Phase != openClawPhaseRestoring {
			return fmt.Errorf("OpenClaw 代理 journal 尚未进入 restoring，拒绝提交：%s", target.User)
		}
		if err := verifyOpenClawUserIdentity(target.User, entry.Identity); err != nil {
			return err
		}
		path, err := a.openClawConfigPath(target.User, entry.Identity)
		if err != nil {
			return err
		}
		raw, err := readOpenClawUserConfig(target.User, entry.Identity, path)
		if err != nil {
			return err
		}
		current, present, err := openClawTelegramProxyRawValue(raw)
		if err != nil {
			return err
		}
		if !proxyStatesEqual(current, present, entry.OriginalValue, entry.OriginalPresent) {
			return fmt.Errorf("用户 %s 的 OpenClaw proxy 在重启后发生变化，保留 journal 等待再次重启", target.User)
		}
		delete(journal.Users, target.User)
		return a.saveOpenClawProxyJournal(journal)
	})
}

// --- 纯函数：openclaw 配置 JSON 的读改（与文件/用户无关，便于测试） ---

// setOpenClawTelegramProxy 设置 channels.telegram.proxy=proxyURL，保留其余字段；
// 若已是该值则不改（changed=false，返回原始 raw）。
func setOpenClawTelegramProxy(raw []byte, proxyURL string) (out []byte, changed bool, err error) {
	if err := rejectOpenClawAccountProxyOverrides(raw); err != nil {
		return nil, false, err
	}
	cur, present, err := openClawTelegramProxyRawValue(raw)
	if err != nil {
		return nil, false, err
	}
	if proxyStateMatchesString(cur, present, proxyURL) {
		return raw, false, nil
	}
	return replaceOpenClawTelegramProxy(raw, proxyURL, true)
}

// OpenClaw merges Telegram account config as {...base, ...account}. An
// account-level proxy therefore overrides channels.telegram.proxy. Until the
// ownership journal records every account value independently, claiming only
// the top-level field would silently leave a partially unmanaged transport.
func rejectOpenClawAccountProxyOverrides(raw []byte) error {
	doc, err := parseOpenClawJSON5(raw)
	if err != nil {
		return err
	}
	if doc.root.kind != openClawJSON5Object {
		return fmt.Errorf("OpenClaw 顶层配置必须是对象")
	}
	channelsField, present, err := openClawJSON5ObjectField(doc.root, "channels", "channels")
	if err != nil || !present {
		return err
	}
	if channelsField.value.kind != openClawJSON5Object {
		return fmt.Errorf("channels 必须是对象")
	}
	telegramField, present, err := openClawJSON5ObjectField(channelsField.value, "telegram", "channels.telegram")
	if err != nil || !present {
		return err
	}
	if telegramField.value.kind != openClawJSON5Object {
		return fmt.Errorf("channels.telegram 必须是对象")
	}
	accountsField, present, err := openClawJSON5ObjectField(telegramField.value, "accounts", "channels.telegram.accounts")
	if err != nil || !present {
		return err
	}
	if accountsField.value.kind != openClawJSON5Object {
		return fmt.Errorf("channels.telegram.accounts 必须是对象")
	}
	overrides := 0
	for _, account := range openClawJSON5EffectiveFields(accountsField.value) {
		if account.value.kind != openClawJSON5Object {
			return fmt.Errorf("channels.telegram.accounts.%s 必须是对象", account.key)
		}
		_, present, err := openClawJSON5ObjectField(account.value, "proxy", "channels.telegram.accounts."+account.key+".proxy")
		if err != nil {
			return err
		}
		if present {
			overrides++
		}
	}
	if overrides > 0 {
		return fmt.Errorf("channels.telegram.accounts 中有 %d 个账号定义了 proxy；账号级 proxy 会覆盖顶层 channels.telegram.proxy，拒绝声称完整接管", overrides)
	}
	return nil
}

func decodeRawJSONObject(raw []byte, field string) (map[string]json.RawMessage, error) {
	object, err := decodeOpenClawJSON5RawObject(raw)
	if err != nil {
		return nil, err
	}
	if object == nil {
		return nil, fmt.Errorf("%s 不能是 null", field)
	}
	return object, nil
}

func openClawTelegramProxyRawValue(raw []byte) (json.RawMessage, bool, error) {
	value, present, _, _, err := openClawTelegramProxyState(raw)
	return value, present, err
}

func openClawTelegramProxyState(raw []byte) (value json.RawMessage, present, channelsPresent, telegramPresent bool, err error) {
	doc, err := parseOpenClawJSON5(raw)
	if err != nil {
		return nil, false, false, false, err
	}
	return openClawJSON5ProxyState(doc)
}

func replaceOpenClawTelegramProxy(raw []byte, value string, present bool) ([]byte, bool, error) {
	var encoded json.RawMessage
	if present {
		encoded, _ = json.Marshal(value)
	}
	return replaceOpenClawTelegramProxyRaw(raw, encoded, present)
}

func replaceOpenClawTelegramProxyRaw(raw []byte, value json.RawMessage, present bool) ([]byte, bool, error) {
	return replaceOpenClawTelegramProxyRawWithStructure(raw, value, present, false, false, false)
}

func replaceOpenClawTelegramProxyRawWithStructure(raw []byte, value json.RawMessage, present, structureRecorded, channelsPresent, telegramPresent bool) ([]byte, bool, error) {
	return setOpenClawJSON5Proxy(raw, value, present, structureRecorded, channelsPresent, telegramPresent)
}

func replaceOpenClawTelegramProxySourceWithStructure(raw []byte, value json.RawMessage, source []byte, present, structureRecorded, channelsPresent, telegramPresent bool) ([]byte, bool, error) {
	return setOpenClawJSON5ProxySource(raw, value, source, present, structureRecorded, channelsPresent, telegramPresent)
}

func validateOpenClawProxyRaw(raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	var value string
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return fmt.Errorf("必须是字符串或 null：%w", err)
	}
	return nil
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func proxyStatesEqual(a json.RawMessage, aPresent bool, b json.RawMessage, bPresent bool) bool {
	if aPresent != bPresent {
		return false
	}
	if !aPresent {
		return true
	}
	return bytes.Equal(bytes.TrimSpace(a), bytes.TrimSpace(b))
}

func proxyStateMatchesString(raw json.RawMessage, present bool, expected string) bool {
	if !present {
		return false
	}
	encoded, _ := json.Marshal(expected)
	return bytes.Equal(bytes.TrimSpace(raw), encoded)
}
