package manager

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

var (
	devCommandExists       = commandExists
	devRemoveBackup        = removeDevBackupDurable
	devBackupDirFsync      = syscall.Fsync
	devLookupUserIdentity  = lookupLocalUserIdentity
	devResolveGitTopology  = resolveDevGitGlobalTopology
	devValidateGitTopology = validateDevGitGlobalTopology
	devGitConfigExists     = devGitConfigLocationExists
	devOutputAsUser        = func(user string, identity *persistedUserIdentity, name string, args ...string) (string, error) {
		return outputAsPersistedUser(user, identity, devLookupUserIdentity, name, args...)
	}
	devRunAsUser = func(user string, identity *persistedUserIdentity, name string, args ...string) error {
		return runAsPersistedUser(user, identity, devLookupUserIdentity, name, args...)
	}
)

const (
	devProxyBackupVersion   = 2
	devRestorePhasePrepared = "prepared"
	devRestorePhaseDone     = "done"
	maxDevProxyValues       = 4096
	maxDevProxyValueBytes   = 64 << 10
	maxDevProxyBackupBytes  = 1 << 20
)

type devGitConfigLocation string

const (
	devGitConfigHome devGitConfigLocation = "home"
	devGitConfigXDG  devGitConfigLocation = "xdg"
)

type devGitRestorePlan struct {
	Phase   string   `json:"phase"`
	Before  []string `json:"before"`
	Desired []string `json:"desired"`
	Next    int      `json:"next"`
}

type devNPMRestorePlan struct {
	Phase   string  `json:"phase"`
	Before  *string `json:"before"`
	Desired *string `json:"desired"`
}

type devApplyRollback struct {
	GitManaged      bool               `json:"git_managed,omitempty"`
	NPMManaged      bool               `json:"npm_managed,omitempty"`
	ManagedProxy    string             `json:"managed_proxy"`
	GitHTTPProxy    []string           `json:"git_http_proxy"`
	GitHTTPSProxy   []string           `json:"git_https_proxy"`
	NPMProxy        *string            `json:"npm_proxy,omitempty"`
	NPMHTTPSProxy   *string            `json:"npm_https_proxy,omitempty"`
	GitHTTPRestore  *devGitRestorePlan `json:"git_http_restore,omitempty"`
	GitHTTPSRestore *devGitRestorePlan `json:"git_https_restore,omitempty"`
	NPMProxyRestore *devNPMRestorePlan `json:"npm_proxy_restore,omitempty"`
	NPMHTTPSRestore *devNPMRestorePlan `json:"npm_https_restore,omitempty"`
}

type devProxyBackup struct {
	Version             int                    `json:"version"`
	User                string                 `json:"user"`
	Identity            *persistedUserIdentity `json:"identity,omitempty"`
	ToolsRecorded       bool                   `json:"tools_recorded,omitempty"`
	GitManaged          bool                   `json:"git_managed,omitempty"`
	GitConfigLocation   devGitConfigLocation   `json:"git_config_location,omitempty"`
	NPMManaged          bool                   `json:"npm_managed,omitempty"`
	GitHTTPProxy        []string               `json:"git_http_proxy"`
	GitHTTPSProxy       []string               `json:"git_https_proxy"`
	NPMProxy            *string                `json:"npm_proxy,omitempty"`
	NPMHTTPSProxy       *string                `json:"npm_https_proxy,omitempty"`
	ManagedHTTPProxy    string                 `json:"managed_http_proxy,omitempty"`
	ManagedHTTPSProxy   string                 `json:"managed_https_proxy,omitempty"`
	ManagedHTTPProxies  []string               `json:"managed_http_proxies,omitempty"`
	ManagedHTTPSProxies []string               `json:"managed_https_proxies,omitempty"`
	GitHTTPRestore      *devGitRestorePlan     `json:"git_http_restore,omitempty"`
	GitHTTPSRestore     *devGitRestorePlan     `json:"git_https_restore,omitempty"`
	NPMProxyRestore     *devNPMRestorePlan     `json:"npm_proxy_restore,omitempty"`
	NPMHTTPSRestore     *devNPMRestorePlan     `json:"npm_https_restore,omitempty"`
	ApplyRollback       *devApplyRollback      `json:"apply_rollback,omitempty"`
}

func (a *App) devTargetUser() (string, error) {
	user := a.cfg.DevTargetUser
	if user == "" {
		if sudoUser := strings.TrimSpace(os.Getenv("SUDO_USER")); sudoUser != "" && sudoUser != "root" {
			user = sudoUser
		}
	}
	if user == "" {
		user = strings.TrimSpace(os.Getenv("USER"))
	}
	if user == "" {
		user = "root"
	}
	if err := validateUserName(user); err != nil {
		return "", err
	}
	// 用 lookupLocalUserIdentity（getent 带 30s 超时 + /etc/passwd 回退）而非无超时的
	// `id -u`：NSS 后端卡死时不能在持有状态锁的情况下无限阻塞所有并发命令。
	if _, err := devLookupUserIdentity(user); err != nil {
		return "", fmt.Errorf("开发代理目标用户不存在：%s", user)
	}
	return user, nil
}

func verifyDevUserIdentity(user string, identity *persistedUserIdentity) error {
	_, err := verifyPersistedUserIdentity(user, identity, devLookupUserIdentity)
	return err
}

func getGitConfigAllForIdentity(user string, identity *persistedUserIdentity, location devGitConfigLocation, key string) ([]string, error) {
	if err := verifyDevUserIdentity(user, identity); err != nil {
		return nil, err
	}
	if err := devValidateGitTopology(user, identity, location); err != nil {
		return nil, err
	}
	exists, err := devGitConfigExists(identity, location)
	if err != nil {
		return nil, err
	}
	if !exists {
		if err := devValidateGitTopology(user, identity, location); err != nil {
			return nil, err
		}
		return nil, nil
	}
	values, err := getGitConfigAll(user, identity, location, key)
	if err != nil {
		return nil, err
	}
	if err := devValidateGitTopology(user, identity, location); err != nil {
		return nil, err
	}
	return values, nil
}

func getNPMConfigForIdentity(user string, identity *persistedUserIdentity, key string) (*string, error) {
	if err := verifyDevUserIdentity(user, identity); err != nil {
		return nil, err
	}
	return getNPMConfig(user, identity, key)
}

func runDevAsPersistedUser(user string, identity *persistedUserIdentity, name string, args ...string) error {
	if err := verifyDevUserIdentity(user, identity); err != nil {
		return err
	}
	return devRunAsUser(user, identity, name, args...)
}

func runGitConfigMutationForIdentity(user string, identity *persistedUserIdentity, location devGitConfigLocation, args ...string) error {
	if err := verifyDevUserIdentity(user, identity); err != nil {
		return err
	}
	if err := devValidateGitTopology(user, identity, location); err != nil {
		return err
	}
	path, err := devGitConfigPath(identity, location)
	if err != nil {
		return err
	}
	gitArgs := append([]string{"config", "--file", path, "--no-includes"}, args...)
	return devRunAsUser(user, identity, "git", gitArgs...)
}

func runGitConfigForIdentity(user string, identity *persistedUserIdentity, location devGitConfigLocation, args ...string) error {
	if err := runGitConfigMutationForIdentity(user, identity, location, args...); err != nil {
		return err
	}
	return devValidateGitTopology(user, identity, location)
}

func (a *App) backupDevConfig(user string) error {
	proxy := a.cfg.HTTPAddr(SceneDev)
	gitAvailable := devCommandExists("git")
	npmAvailable := devCommandExists("npm")
	backup, err := a.loadDevBackup()
	if err == nil && backup.User != user {
		previousUser := backup.User
		// A runtime-user transition may have applied the candidate user and then
		// crashed before state.json committed. The backup is the durable ownership
		// authority for that partial apply, so release it before taking ownership of
		// the user selected by the still-committed Store.
		if err := a.restoreDev(); err != nil {
			return fmt.Errorf("恢复上一开发代理目标用户 %s 的 ownership 失败：%w", previousUser, err)
		}
		if _, err := a.loadDevBackup(); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				err = fmt.Errorf("ownership 备份仍存在")
			}
			return fmt.Errorf("确认上一开发代理目标用户 %s 的 ownership 已释放失败：%w", previousUser, err)
		}
		backup = nil
		err = os.ErrNotExist
	}
	if err == nil {
		if err := verifyDevUserIdentity(user, backup.Identity); err != nil {
			return err
		}
		if gitAvailable && backup.GitManaged {
			if err := devValidateGitTopology(user, backup.Identity, backup.GitConfigLocation); err != nil {
				return err
			}
		}
		if backup.ApplyRollback != nil {
			return fmt.Errorf("开发代理上一轮应用回滚尚未完成，请先重试应用或关闭开发代理")
		}
		if devRestoreStarted(backup) {
			return fmt.Errorf("开发代理配置正在恢复，拒绝重新接管；请先重试关闭开发代理")
		}
		changed := mergeManagedDevProxyValues(backup, proxy)
		if backup.ToolsRecorded {
			if !backup.GitManaged && gitAvailable {
				backup.GitConfigLocation, err = devResolveGitTopology(user, backup.Identity)
				if err != nil {
					return fmt.Errorf("选择新增 Git global 配置文件失败：%w", err)
				}
				backup.GitHTTPProxy, err = getGitConfigAllForIdentity(user, backup.Identity, backup.GitConfigLocation, "http.proxy")
				if err != nil {
					return fmt.Errorf("读取新增 git http.proxy 原值失败：%w", err)
				}
				backup.GitHTTPSProxy, err = getGitConfigAllForIdentity(user, backup.Identity, backup.GitConfigLocation, "https.proxy")
				if err != nil {
					return fmt.Errorf("读取新增 git https.proxy 原值失败：%w", err)
				}
				backup.GitManaged = true
				changed = true
			}
			if !backup.NPMManaged && npmAvailable {
				backup.NPMProxy, err = getNPMConfigForIdentity(user, backup.Identity, "proxy")
				if err != nil {
					return fmt.Errorf("读取新增 npm proxy 原值失败：%w", err)
				}
				backup.NPMHTTPSProxy, err = getNPMConfigForIdentity(user, backup.Identity, "https-proxy")
				if err != nil {
					return fmt.Errorf("读取新增 npm https-proxy 原值失败：%w", err)
				}
				backup.NPMManaged = true
				changed = true
			}
		}
		if changed {
			return a.writeDevBackup(backup)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	identity, err := capturePersistedUserIdentity(user, devLookupUserIdentity)
	if err != nil {
		return err
	}
	newBackup := devProxyBackup{Version: devProxyBackupVersion, User: user, Identity: identity, ToolsRecorded: true, ManagedHTTPProxy: proxy, ManagedHTTPSProxy: proxy, ManagedHTTPProxies: []string{proxy}, ManagedHTTPSProxies: []string{proxy}}
	if gitAvailable {
		newBackup.GitConfigLocation, err = devResolveGitTopology(user, identity)
		if err != nil {
			return fmt.Errorf("选择 Git global 配置文件失败：%w", err)
		}
		newBackup.GitManaged = true
		newBackup.GitHTTPProxy, err = getGitConfigAllForIdentity(user, newBackup.Identity, newBackup.GitConfigLocation, "http.proxy")
		if err != nil {
			return fmt.Errorf("读取 git http.proxy 原值失败：%w", err)
		}
		newBackup.GitHTTPSProxy, err = getGitConfigAllForIdentity(user, newBackup.Identity, newBackup.GitConfigLocation, "https.proxy")
		if err != nil {
			return fmt.Errorf("读取 git https.proxy 原值失败：%w", err)
		}
	}
	if npmAvailable {
		newBackup.NPMManaged = true
		newBackup.NPMProxy, err = getNPMConfigForIdentity(user, newBackup.Identity, "proxy")
		if err != nil {
			return fmt.Errorf("读取 npm proxy 原值失败：%w", err)
		}
		newBackup.NPMHTTPSProxy, err = getNPMConfigForIdentity(user, newBackup.Identity, "https-proxy")
		if err != nil {
			return fmt.Errorf("读取 npm https-proxy 原值失败：%w", err)
		}
	}
	return a.writeDevBackup(&newBackup)
}

func devGitConfigPath(identity *persistedUserIdentity, location devGitConfigLocation) (string, error) {
	if identity == nil {
		return "", fmt.Errorf("开发代理 Git global 配置缺少用户身份绑定")
	}
	switch location {
	case devGitConfigHome:
		return filepath.Join(identity.Home, ".gitconfig"), nil
	case devGitConfigXDG:
		return filepath.Join(identity.Home, ".config", "git", "config"), nil
	default:
		return "", fmt.Errorf("开发代理 Git global 配置位置无效：%q", location)
	}
}

func devGitConfigLocationExists(identity *persistedUserIdentity, location devGitConfigLocation) (bool, error) {
	path, err := devGitConfigPath(identity, location)
	if err != nil {
		return false, err
	}
	relDir := "."
	base := ".gitconfig"
	if location == devGitConfigXDG {
		relDir = filepath.Join(".config", "git")
		base = "config"
	}
	dirFD, err := openExistingUserDirChain(identity.Home, relDir, identity.UID, identity.GID)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("检查 Git global 配置目录失败（拒绝符号链接）：%s：%w", filepath.Dir(path), err)
	}
	defer syscall.Close(dirFD)
	fd, err := syscall.Openat(dirFD, base, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err == syscall.ENOENT {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("打开 Git global 配置失败（拒绝符号链接）：%s：%w", path, err)
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return false, fmt.Errorf("校验 Git global 配置失败：%s：%w", path, err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return false, fmt.Errorf("开发代理 Git global 配置不是普通文件，拒绝接管：%s", path)
	}
	return true, nil
}

func resolveDevGitGlobalTopology(user string, identity *persistedUserIdentity) (devGitConfigLocation, error) {
	if err := verifyDevUserIdentity(user, identity); err != nil {
		return "", err
	}
	homeExists, err := devGitConfigLocationExists(identity, devGitConfigHome)
	if err != nil {
		return "", err
	}
	xdgExists, err := devGitConfigLocationExists(identity, devGitConfigXDG)
	if err != nil {
		return "", err
	}
	if homeExists && xdgExists {
		return "", fmt.Errorf("用户 %s 同时存在多个 Git global 配置文件，无法证明跨文件精确恢复，拒绝接管", user)
	}
	location := devGitConfigHome
	if xdgExists {
		location = devGitConfigXDG
	}
	if err := validateDevGitGlobalTopology(user, identity, location); err != nil {
		return "", err
	}
	return location, nil
}

func validateDevGitGlobalTopology(user string, identity *persistedUserIdentity, location devGitConfigLocation) error {
	if err := verifyDevUserIdentity(user, identity); err != nil {
		return err
	}
	path, err := devGitConfigPath(identity, location)
	if err != nil {
		return err
	}
	homeExists, err := devGitConfigLocationExists(identity, devGitConfigHome)
	if err != nil {
		return err
	}
	xdgExists, err := devGitConfigLocationExists(identity, devGitConfigXDG)
	if err != nil {
		return err
	}
	if homeExists && xdgExists || location == devGitConfigHome && xdgExists || location == devGitConfigXDG && homeExists {
		return fmt.Errorf("用户 %s 的 Git global 配置拓扑已偏离记录位置 %s，拒绝接管或恢复", user, location)
	}
	selectedExists := homeExists
	if location == devGitConfigXDG {
		selectedExists = xdgExists
	}
	if !selectedExists {
		return nil
	}
	out, err := devOutputAsUser(user, identity, "git", "config", "--file", path, "--no-includes", "--null", "--name-only", "--list")
	if err != nil {
		return fmt.Errorf("检查用户 %s 的 Git global include 配置失败：%w", user, err)
	}
	keys, err := parseGitNullOutput(out)
	if err != nil {
		return fmt.Errorf("解析用户 %s 的 Git global 配置键失败：%w", user, err)
	}
	for _, rawKey := range keys {
		key := strings.ToLower(rawKey)
		if key == "" {
			return fmt.Errorf("用户 %s 的 Git global 配置包含空键，拒绝接管", user)
		}
		if key == "include.path" || (strings.HasPrefix(key, "includeif.") && strings.HasSuffix(key, ".path")) {
			return fmt.Errorf("用户 %s 的 Git global 配置包含 %s；当前 ownership 记录无法跨 include 精确恢复，拒绝接管", user, rawKey)
		}
	}
	return nil
}

func getGitConfigAll(user string, identity *persistedUserIdentity, location devGitConfigLocation, key string) ([]string, error) {
	path, err := devGitConfigPath(identity, location)
	if err != nil {
		return nil, err
	}
	out, err := devOutputAsUser(user, identity, "git", "config", "--file", path, "--no-includes", "--null", "--get-all", "--", key)
	if err != nil {
		if commandExitCode(err) == 1 {
			return nil, nil
		}
		return nil, err
	}
	return parseGitNullOutput(out)
}

func parseGitNullOutput(out string) ([]string, error) {
	if out == "" {
		return nil, nil
	}
	if out[len(out)-1] != 0 {
		return nil, fmt.Errorf("git NUL 输出缺少终止分隔符")
	}
	parts := strings.Split(out, "\x00")
	return parts[:len(parts)-1], nil
}

func getNPMConfig(user string, identity *persistedUserIdentity, key string) (*string, error) {
	return devReadNPMConfig(user, identity, key)
}

func commandExitCode(err error) int {
	type exitCoder interface{ ExitCode() int }
	var coded exitCoder
	if errors.As(err, &coded) {
		return coded.ExitCode()
	}
	return -1
}

func (a *App) restoreDev() error {
	backup, err := a.loadDevBackup()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// An earlier restore may have unlinked the backup and then failed its
			// directory barrier. Replaying the durable remover on ENOENT completes
			// that barrier before treating the cleanup as finished.
			if err := devRemoveBackup(a.cfg.DevBackupPath()); err != nil {
				return fmt.Errorf("确认开发代理备份删除持久化失败：%w", err)
			}
			fmt.Println("未找到开发代理备份，无法证明配置所有权；已跳过清理")
			return nil
		}
		return err
	}
	user := backup.User
	if err := validateUserName(user); err != nil {
		return err
	}
	if err := verifyDevUserIdentity(user, backup.Identity); err != nil {
		return err
	}
	if err := a.resumeDevApplyRollback(backup); err != nil {
		return fmt.Errorf("完成开发代理上一轮应用回滚失败：%w", err)
	}
	managedHTTP := managedDevProxyValues(backup, true, a.cfg.HTTPAddr(SceneDev))
	managedHTTPS := managedDevProxyValues(backup, false, a.cfg.HTTPAddr(SceneDev))
	var errs []error
	if backup.GitManaged {
		if err := a.restoreDevGitProxy(backup, "http.proxy", backup.GitHTTPProxy, managedHTTP, &backup.GitHTTPRestore); err != nil {
			errs = append(errs, err)
		}
		if err := a.restoreDevGitProxy(backup, "https.proxy", backup.GitHTTPSProxy, managedHTTPS, &backup.GitHTTPSRestore); err != nil {
			errs = append(errs, err)
		}
	}
	if backup.NPMManaged {
		if err := a.restoreDevNPMProxy(backup, "proxy", backup.NPMProxy, managedHTTP, &backup.NPMProxyRestore); err != nil {
			errs = append(errs, err)
		}
		if err := a.restoreDevNPMProxy(backup, "https-proxy", backup.NPMHTTPSProxy, managedHTTPS, &backup.NPMHTTPSRestore); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if backup.GitManaged {
		if err := devValidateGitTopology(user, backup.Identity, backup.GitConfigLocation); err != nil {
			return fmt.Errorf("删除开发代理备份前复核 Git global 配置失败：%w", err)
		}
	}
	if err := verifyDevUserIdentity(user, backup.Identity); err != nil {
		return err
	}
	if err := devRemoveBackup(a.cfg.DevBackupPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("删除开发代理备份失败：%w", err)
	}
	return nil
}

func (a *App) resumePendingDevApplyRollback() error {
	backup, err := a.loadDevBackup()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if backup.ApplyRollback == nil {
		return nil
	}
	if err := verifyDevUserIdentity(backup.User, backup.Identity); err != nil {
		return err
	}
	return a.resumeDevApplyRollback(backup)
}

func (a *App) restoreDevProxySnapshotDurable(snapshot *devProxyBackup, managedProxy string) error {
	if snapshot == nil {
		return nil
	}
	backup, err := a.loadDevBackup()
	if err != nil {
		return fmt.Errorf("加载开发代理 ownership 备份失败：%w", err)
	}
	if err := verifyDevUserIdentity(backup.User, backup.Identity); err != nil {
		return err
	}
	if backup.ApplyRollback != nil {
		return a.resumeDevApplyRollback(backup)
	}
	if devRestoreStarted(backup) {
		return fmt.Errorf("开发代理最终恢复已开始，拒绝创建应用回滚计划")
	}
	if snapshot.Version != devProxyBackupVersion || !snapshot.ToolsRecorded || snapshot.User != backup.User ||
		!samePersistedUserIdentity(snapshot.Identity, backup.Identity) ||
		snapshot.GitManaged && (!backup.GitManaged || snapshot.GitConfigLocation != backup.GitConfigLocation) ||
		snapshot.NPMManaged && !backup.NPMManaged {
		return fmt.Errorf("开发代理应用快照与 ownership 备份的版本、工具范围或身份不一致")
	}
	if !containsString(managedDevProxyValues(backup, true, a.cfg.HTTPAddr(SceneDev)), managedProxy) ||
		!containsString(managedDevProxyValues(backup, false, a.cfg.HTTPAddr(SceneDev)), managedProxy) {
		return fmt.Errorf("开发代理应用回滚值不属于当前 ownership 备份：%s", managedProxy)
	}
	backup.ApplyRollback = &devApplyRollback{
		GitManaged:    snapshot.GitManaged,
		NPMManaged:    snapshot.NPMManaged,
		ManagedProxy:  managedProxy,
		GitHTTPProxy:  slices.Clone(snapshot.GitHTTPProxy),
		GitHTTPSProxy: slices.Clone(snapshot.GitHTTPSProxy),
		NPMProxy:      cloneStringPointer(snapshot.NPMProxy),
		NPMHTTPSProxy: cloneStringPointer(snapshot.NPMHTTPSProxy),
	}
	if err := a.writeDevBackup(backup); err != nil {
		return fmt.Errorf("持久化开发代理应用回滚快照失败：%w", err)
	}
	return a.resumeDevApplyRollback(backup)
}

func (a *App) resumeDevApplyRollback(backup *devProxyBackup) error {
	if backup == nil || backup.ApplyRollback == nil {
		return nil
	}
	if err := verifyDevUserIdentity(backup.User, backup.Identity); err != nil {
		return err
	}
	rollback := backup.ApplyRollback
	managed := []string{rollback.ManagedProxy}
	var errs []error
	if rollback.GitManaged {
		if err := a.restoreDevGitProxy(backup, "http.proxy", rollback.GitHTTPProxy, managed, &rollback.GitHTTPRestore); err != nil {
			errs = append(errs, err)
		}
		if err := a.restoreDevGitProxy(backup, "https.proxy", rollback.GitHTTPSProxy, managed, &rollback.GitHTTPSRestore); err != nil {
			errs = append(errs, err)
		}
	}
	if rollback.NPMManaged {
		if err := a.restoreDevNPMProxy(backup, "proxy", rollback.NPMProxy, managed, &rollback.NPMProxyRestore); err != nil {
			errs = append(errs, err)
		}
		if err := a.restoreDevNPMProxy(backup, "https-proxy", rollback.NPMHTTPSProxy, managed, &rollback.NPMHTTPSRestore); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if err := verifyDevUserIdentity(backup.User, backup.Identity); err != nil {
		return err
	}
	backup.ApplyRollback = nil
	if err := a.writeDevBackup(backup); err != nil {
		return fmt.Errorf("提交开发代理应用回滚结果失败：%w", err)
	}
	return nil
}

func samePersistedUserIdentity(left, right *persistedUserIdentity) bool {
	return left != nil && right != nil && left.UID == right.UID && left.GID == right.GID && left.Home == right.Home
}

func removeDevBackupDurable(path string) error {
	dir := filepath.Dir(path)
	if err := validateNoSymlinkComponents(dir, false); err != nil {
		return err
	}
	dirFD, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(dirFD)
	if err := validatePrivilegedDirFD(dir, dirFD); err != nil {
		return err
	}
	if err := syscall.Unlinkat(dirFD, filepath.Base(path)); err != nil && err != syscall.ENOENT {
		return err
	}
	if err := devBackupDirFsync(dirFD); err != nil {
		return fmt.Errorf("持久化开发代理备份删除失败：%w", err)
	}
	return nil
}

func (a *App) restoreDevGitProxy(backup *devProxyBackup, key string, original, managed []string, plan **devGitRestorePlan) error {
	if *plan != nil && (*plan).Phase == devRestorePhaseDone {
		return devValidateGitTopology(backup.User, backup.Identity, backup.GitConfigLocation)
	}
	if !devCommandExists("git") {
		return fmt.Errorf("恢复 git %s 失败：找不到 git", key)
	}
	if err := devValidateGitTopology(backup.User, backup.Identity, backup.GitConfigLocation); err != nil {
		return fmt.Errorf("恢复 git %s 前检查 global 配置失败：%w", key, err)
	}
	current, err := getGitConfigAllForIdentity(backup.User, backup.Identity, backup.GitConfigLocation, key)
	if err != nil {
		return fmt.Errorf("读取 git %s 当前值失败：%w", key, err)
	}
	if *plan == nil {
		if !containsManagedDevValue(current, managed) {
			*plan = &devGitRestorePlan{
				Phase:   devRestorePhaseDone,
				Before:  slices.Clone(current),
				Desired: slices.Clone(current),
				Next:    len(current) + 1,
			}
			return a.writeDevBackup(backup)
		}
		*plan = &devGitRestorePlan{
			Phase:   devRestorePhasePrepared,
			Before:  slices.Clone(current),
			Desired: desiredGitProxyValues(original, current, managed),
		}
		if err := a.writeDevBackup(backup); err != nil {
			return fmt.Errorf("持久化 git %s 恢复计划失败：%w", key, err)
		}
	}

	restorePlan := *plan
	for restorePlan.Phase == devRestorePhasePrepared {
		current, err = getGitConfigAllForIdentity(backup.User, backup.Identity, backup.GitConfigLocation, key)
		if err != nil {
			return fmt.Errorf("读取 git %s 当前值失败：%w", key, err)
		}
		before, after, err := devGitRestoreTransition(restorePlan)
		if err != nil {
			return fmt.Errorf("git %s 恢复计划无效：%w", key, err)
		}
		switch {
		case slices.Equal(current, after):
		// The command completed before a crash or an ambiguous journal write.
		case slices.Equal(current, before):
			if restorePlan.Next == 0 {
				err = devMutateGitConfig(backup.User, backup.Identity, backup.GitConfigLocation, before, "--unset-all", "--", key)
			} else {
				err = devMutateGitConfig(backup.User, backup.Identity, backup.GitConfigLocation, before, "--add", "--", key, restorePlan.Desired[restorePlan.Next-1])
			}
			if err != nil {
				return fmt.Errorf("执行 git %s 恢复步骤 %d 失败：%w", key, restorePlan.Next, err)
			}
		default:
			return fmt.Errorf("git %s 在恢复步骤 %d 期间被并发修改，拒绝覆盖并保留开发代理备份", key, restorePlan.Next)
		}

		actual, err := getGitConfigAllForIdentity(backup.User, backup.Identity, backup.GitConfigLocation, key)
		if err != nil {
			return fmt.Errorf("验证 git %s 恢复步骤 %d 失败：%w", key, restorePlan.Next, err)
		}
		if !slices.Equal(actual, after) {
			return fmt.Errorf("git %s 恢复步骤 %d 结果与持久化计划不一致，保留开发代理备份", key, restorePlan.Next)
		}

		restorePlan.Next++
		if restorePlan.Next == len(restorePlan.Desired)+1 {
			restorePlan.Phase = devRestorePhaseDone
		}
		if err := a.writeDevBackup(backup); err != nil {
			return fmt.Errorf("提交 git %s 恢复步骤 %d 失败：%w", key, restorePlan.Next-1, err)
		}
	}
	return nil
}

func devGitRestoreTransition(plan *devGitRestorePlan) (before, after []string, err error) {
	if plan == nil || plan.Phase != devRestorePhasePrepared || plan.Next < 0 || plan.Next > len(plan.Desired) {
		return nil, nil, fmt.Errorf("next 超出 prepared 计划范围")
	}
	if plan.Next == 0 {
		return plan.Before, []string{}, nil
	}
	return plan.Desired[:plan.Next-1], plan.Desired[:plan.Next], nil
}

func (a *App) restoreDevNPMProxy(backup *devProxyBackup, key string, original *string, managed []string, plan **devNPMRestorePlan) error {
	if *plan != nil && (*plan).Phase == devRestorePhaseDone {
		return nil
	}
	if !devCommandExists("npm") {
		return fmt.Errorf("恢复 npm %s 失败：找不到 npm", key)
	}
	current, err := getNPMConfigForIdentity(backup.User, backup.Identity, key)
	if err != nil {
		return fmt.Errorf("读取 npm %s 当前值失败：%w", key, err)
	}
	if *plan == nil {
		if current == nil || !containsManagedNPMProxy(managed, *current) {
			*plan = &devNPMRestorePlan{
				Phase:   devRestorePhaseDone,
				Before:  cloneStringPointer(current),
				Desired: cloneStringPointer(current),
			}
			return a.writeDevBackup(backup)
		}
		*plan = &devNPMRestorePlan{
			Phase:   devRestorePhasePrepared,
			Before:  cloneStringPointer(current),
			Desired: cloneStringPointer(original),
		}
		if err := a.writeDevBackup(backup); err != nil {
			return fmt.Errorf("持久化 npm %s 恢复计划失败：%w", key, err)
		}
	}

	restorePlan := *plan
	current, err = getNPMConfigForIdentity(backup.User, backup.Identity, key)
	if err != nil {
		return fmt.Errorf("重新读取 npm %s 当前值失败：%w", key, err)
	}
	if optionalStringsEqual(current, restorePlan.Desired) {
		restorePlan.Phase = devRestorePhaseDone
		return a.writeDevBackup(backup)
	}
	if !optionalStringsEqual(current, restorePlan.Before) {
		return fmt.Errorf("npm %s 在恢复期间被并发修改，拒绝覆盖并保留开发代理备份", key)
	}
	err = devMutateNPMConfig(backup.User, backup.Identity, key, restorePlan.Before, restorePlan.Desired)
	if err != nil {
		return err
	}
	actual, err := getNPMConfigForIdentity(backup.User, backup.Identity, key)
	if err != nil {
		return fmt.Errorf("验证 npm %s 恢复结果失败：%w", key, err)
	}
	if !optionalStringsEqual(actual, restorePlan.Desired) {
		return fmt.Errorf("npm %s 恢复结果与持久化计划不一致，保留开发代理备份", key)
	}
	restorePlan.Phase = devRestorePhaseDone
	if err := a.writeDevBackup(backup); err != nil {
		return fmt.Errorf("提交 npm %s 恢复结果失败：%w", key, err)
	}
	return nil
}

func desiredGitProxyValues(original, current, managed []string) []string {
	values := append(make([]string, 0, len(original)+len(current)), original...)
	for _, value := range current {
		if !containsString(managed, value) {
			values = append(values, value)
		}
	}
	return values
}

func containsManagedDevValue(current, managed []string) bool {
	for _, value := range current {
		if containsString(managed, value) {
			return true
		}
	}
	return false
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func optionalStringsEqual(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

// A repeat apply may replace only the recorded baseline or a single value we
// previously managed. This also covers a crash after ownership was written but
// before the first mutation: an administrator's later edit must not be erased.
func devGitApplyValueKnown(current, original, managed []string) bool {
	return slices.Equal(current, original) || len(current) == 1 && containsString(managed, current[0])
}

func devNPMApplyValueKnown(current, original *string, managed []string) bool {
	return optionalStringsEqual(current, original) || current != nil && containsManagedNPMProxy(managed, *current)
}

func containsManagedNPMProxy(managed []string, current string) bool {
	current = strings.TrimSpace(current)
	for _, candidate := range managed {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if current == candidate || current == candidate+"/" || current+"/" == candidate {
			return true
		}
	}
	return false
}

func mergeManagedDevProxyValues(backup *devProxyBackup, proxy string) bool {
	changed := false
	beforeHTTP := len(backup.ManagedHTTPProxies)
	beforeHTTPS := len(backup.ManagedHTTPSProxies)
	backup.ManagedHTTPProxies = appendUniqueString(backup.ManagedHTTPProxies, backup.ManagedHTTPProxy)
	backup.ManagedHTTPProxies = appendUniqueString(backup.ManagedHTTPProxies, proxy)
	backup.ManagedHTTPSProxies = appendUniqueString(backup.ManagedHTTPSProxies, backup.ManagedHTTPSProxy)
	backup.ManagedHTTPSProxies = appendUniqueString(backup.ManagedHTTPSProxies, proxy)
	if backup.ManagedHTTPProxy == "" {
		backup.ManagedHTTPProxy = proxy
		changed = true
	}
	if backup.ManagedHTTPSProxy == "" {
		backup.ManagedHTTPSProxy = proxy
		changed = true
	}
	return changed || len(backup.ManagedHTTPProxies) != beforeHTTP || len(backup.ManagedHTTPSProxies) != beforeHTTPS
}

func managedDevProxyValues(backup *devProxyBackup, httpProxy bool, fallback string) []string {
	values := []string{}
	if httpProxy {
		values = appendUniqueString(values, backup.ManagedHTTPProxy)
		for _, v := range backup.ManagedHTTPProxies {
			values = appendUniqueString(values, v)
		}
	} else {
		values = appendUniqueString(values, backup.ManagedHTTPSProxy)
		for _, v := range backup.ManagedHTTPSProxies {
			values = appendUniqueString(values, v)
		}
	}
	values = appendUniqueString(values, fallback)
	return values
}

func appendUniqueString(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, v := range values {
		if v == value {
			return values
		}
	}
	return append(values, value)
}

func devRestoreStarted(backup *devProxyBackup) bool {
	return backup != nil && (backup.GitHTTPRestore != nil || backup.GitHTTPSRestore != nil ||
		backup.NPMProxyRestore != nil || backup.NPMHTTPSRestore != nil)
}

func (a *App) writeDevBackup(backup *devProxyBackup) error {
	if err := validateDevProxyBackup(backup); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(backup, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if len(raw) > maxDevProxyBackupBytes {
		return fmt.Errorf("开发代理备份超过大小限制 %d 字节", maxDevProxyBackupBytes)
	}
	return writeFileAtomic(a.cfg.DevBackupPath(), raw, 0o600)
}

func validateDevProxyBackup(backup *devProxyBackup) error {
	if backup == nil || backup.Version != devProxyBackupVersion {
		return fmt.Errorf("开发代理备份版本无效或缺少安全身份绑定")
	}
	if !backup.ToolsRecorded {
		return fmt.Errorf("开发代理备份缺少受管工具范围")
	}
	if err := validateUserName(backup.User); err != nil {
		return fmt.Errorf("开发代理备份用户无效：%w", err)
	}
	if err := validatePersistedUserIdentity(backup.User, backup.Identity); err != nil {
		return fmt.Errorf("开发代理备份身份无效：%w", err)
	}
	if backup.GitManaged {
		if _, err := devGitConfigPath(backup.Identity, backup.GitConfigLocation); err != nil {
			return fmt.Errorf("开发代理备份 Git global 位置无效：%w", err)
		}
	} else if backup.GitConfigLocation != "" {
		return fmt.Errorf("开发代理备份含未受管 Git global 位置")
	}
	for name, values := range map[string][]string{
		"git_http_proxy":        backup.GitHTTPProxy,
		"git_https_proxy":       backup.GitHTTPSProxy,
		"managed_http_proxies":  backup.ManagedHTTPProxies,
		"managed_https_proxies": backup.ManagedHTTPSProxies,
	} {
		if err := validateDevProxyValueList(name, values); err != nil {
			return err
		}
	}
	for name, value := range map[string]*string{
		"npm_proxy":           backup.NPMProxy,
		"npm_https_proxy":     backup.NPMHTTPSProxy,
		"managed_http_proxy":  stringPointerIfPresent(backup.ManagedHTTPProxy),
		"managed_https_proxy": stringPointerIfPresent(backup.ManagedHTTPSProxy),
	} {
		if err := validateDevProxyValue(name, value); err != nil {
			return err
		}
	}
	for name, plan := range map[string]*devGitRestorePlan{
		"git_http_restore":  backup.GitHTTPRestore,
		"git_https_restore": backup.GitHTTPSRestore,
	} {
		if err := validateDevGitRestorePlan(name, plan); err != nil {
			return err
		}
	}
	for name, plan := range map[string]*devNPMRestorePlan{
		"npm_proxy_restore":       backup.NPMProxyRestore,
		"npm_https_proxy_restore": backup.NPMHTTPSRestore,
	} {
		if err := validateDevNPMRestorePlan(name, plan); err != nil {
			return err
		}
	}
	if err := validateDevApplyRollback(backup); err != nil {
		return err
	}
	if !backup.GitManaged && (len(backup.GitHTTPProxy) != 0 || len(backup.GitHTTPSProxy) != 0 ||
		backup.GitHTTPRestore != nil || backup.GitHTTPSRestore != nil) {
		return fmt.Errorf("开发代理备份含未受管 git 状态")
	}
	if !backup.NPMManaged && (backup.NPMProxy != nil || backup.NPMHTTPSProxy != nil ||
		backup.NPMProxyRestore != nil || backup.NPMHTTPSRestore != nil) {
		return fmt.Errorf("开发代理备份含未受管 npm 状态")
	}
	return nil
}

func validateDevGitRestorePlan(name string, plan *devGitRestorePlan) error {
	if plan == nil {
		return nil
	}
	if plan.Phase != devRestorePhasePrepared && plan.Phase != devRestorePhaseDone {
		return fmt.Errorf("开发代理 %s phase 无效", name)
	}
	if err := validateDevProxyValueList(name+".before", plan.Before); err != nil {
		return err
	}
	if err := validateDevProxyValueList(name+".desired", plan.Desired); err != nil {
		return err
	}
	if plan.Next < 0 || (plan.Phase == devRestorePhasePrepared && plan.Next > len(plan.Desired)) ||
		(plan.Phase == devRestorePhaseDone && plan.Next != len(plan.Desired)+1) {
		return fmt.Errorf("开发代理 %s next 无效", name)
	}
	return nil
}

func validateDevNPMRestorePlan(name string, plan *devNPMRestorePlan) error {
	if plan == nil {
		return nil
	}
	if plan.Phase != devRestorePhasePrepared && plan.Phase != devRestorePhaseDone {
		return fmt.Errorf("开发代理 %s phase 无效", name)
	}
	if err := validateDevProxyValue(name+".before", plan.Before); err != nil {
		return err
	}
	return validateDevProxyValue(name+".desired", plan.Desired)
}

func validateDevApplyRollback(backup *devProxyBackup) error {
	rollback := backup.ApplyRollback
	if rollback == nil {
		return nil
	}
	if !backup.ToolsRecorded || devRestoreStarted(backup) {
		return fmt.Errorf("开发代理 apply_rollback 与 ownership 状态冲突")
	}
	if !rollback.GitManaged && !rollback.NPMManaged {
		return fmt.Errorf("开发代理 apply_rollback 未记录任何受管工具")
	}
	if rollback.GitManaged && !backup.GitManaged || rollback.NPMManaged && !backup.NPMManaged {
		return fmt.Errorf("开发代理 apply_rollback 工具范围超出 ownership 备份")
	}
	if err := validateDevProxyValue("apply_rollback.managed_proxy", &rollback.ManagedProxy); err != nil {
		return err
	}
	if !containsString(managedDevProxyValues(backup, true, ""), rollback.ManagedProxy) ||
		!containsString(managedDevProxyValues(backup, false, ""), rollback.ManagedProxy) {
		return fmt.Errorf("开发代理 apply_rollback managed_proxy 不属于 ownership 备份")
	}
	for name, values := range map[string][]string{
		"apply_rollback.git_http_proxy":  rollback.GitHTTPProxy,
		"apply_rollback.git_https_proxy": rollback.GitHTTPSProxy,
	} {
		if err := validateDevProxyValueList(name, values); err != nil {
			return err
		}
	}
	for name, value := range map[string]*string{
		"apply_rollback.npm_proxy":       rollback.NPMProxy,
		"apply_rollback.npm_https_proxy": rollback.NPMHTTPSProxy,
	} {
		if err := validateDevProxyValue(name, value); err != nil {
			return err
		}
	}
	for name, plan := range map[string]*devGitRestorePlan{
		"apply_rollback.git_http_restore":  rollback.GitHTTPRestore,
		"apply_rollback.git_https_restore": rollback.GitHTTPSRestore,
	} {
		if err := validateDevGitRestorePlan(name, plan); err != nil {
			return err
		}
	}
	for name, plan := range map[string]*devNPMRestorePlan{
		"apply_rollback.npm_proxy_restore":       rollback.NPMProxyRestore,
		"apply_rollback.npm_https_proxy_restore": rollback.NPMHTTPSRestore,
	} {
		if err := validateDevNPMRestorePlan(name, plan); err != nil {
			return err
		}
	}
	if !rollback.GitManaged && (len(rollback.GitHTTPProxy) != 0 || len(rollback.GitHTTPSProxy) != 0 ||
		rollback.GitHTTPRestore != nil || rollback.GitHTTPSRestore != nil) {
		return fmt.Errorf("开发代理 apply_rollback 含未受管 git 状态")
	}
	if !rollback.NPMManaged && (rollback.NPMProxy != nil || rollback.NPMHTTPSProxy != nil ||
		rollback.NPMProxyRestore != nil || rollback.NPMHTTPSRestore != nil) {
		return fmt.Errorf("开发代理 apply_rollback 含未受管 npm 状态")
	}
	return nil
}

func validateDevProxyValueList(name string, values []string) error {
	if len(values) > maxDevProxyValues {
		return fmt.Errorf("开发代理 %s 值数量超过上限 %d", name, maxDevProxyValues)
	}
	for i := range values {
		value := &values[i]
		if err := validateDevProxyValue(name, value); err != nil {
			return err
		}
	}
	return nil
}

func validateDevProxyValue(name string, value *string) error {
	if value == nil {
		return nil
	}
	if len(*value) == 0 || len(*value) > maxDevProxyValueBytes || strings.TrimSpace(*value) != *value || strings.IndexByte(*value, 0) >= 0 {
		return fmt.Errorf("开发代理 %s 含空值、过长值、首尾空白或 NUL", name)
	}
	return nil
}

func stringPointerIfPresent(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func (a *App) loadDevBackup() (*devProxyBackup, error) {
	b, err := readRegularFileNoFollow(a.cfg.DevBackupPath(), maxDevProxyBackupBytes)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return nil, fmt.Errorf("开发代理备份不能是 null")
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	var backup devProxyBackup
	if err := decoder.Decode(&backup); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("开发代理备份含多个 JSON 值")
		}
		return nil, err
	}
	if err := validateDevProxyBackup(&backup); err != nil {
		return nil, err
	}
	return &backup, nil
}
