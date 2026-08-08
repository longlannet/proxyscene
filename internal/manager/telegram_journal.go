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
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	telegramProxyJournalVersion          = 1
	maxTelegramProxyJournalBytes   int64 = 4 << 20
	maxTelegramManagedContentBytes       = 4 << 10

	telegramPhasePrepared  = "prepared"
	telegramPhaseActive    = "active"
	telegramPhaseRestoring = "restoring"

	telegramArtifactSystemDropIn = "system-drop-in"
	telegramArtifactUserDropIn   = "user-drop-in"

	telegramManagedDropInName          = "90-proxyscene-telegram-proxy.conf"
	telegramSystemQuarantineSuffix     = ".proxyscene-quarantine"
	maxRemoveTelegramSystemCASAttempts = 3
)

var errTelegramSystemArtifactChanged = errors.New("系统级 Telegram drop-in 在读取后已被修改")

var (
	telegramJournalWriteFile = writeFileAtomic

	telegramManagedSystemPath = func(cfg Config, service string) string {
		return filepath.Join(cfg.TelegramDropInDir(service), telegramManagedDropInName)
	}
	telegramManagedUserPath = func(_ Config, _ string, home, service string) (string, error) {
		return filepath.Join(home, ".config/systemd/user", normalizeSystemdServiceName(service)+".d", telegramManagedDropInName), nil
	}
	telegramLegacySystemPath = func(cfg Config, service string) string {
		return cfg.TelegramDropInPath(service)
	}
	telegramLegacyEnvPath = func() string { return "/etc/openclaw-hermes-tg-proxy.env" }

	telegramReadSystemArtifact    = readRegularFileNoFollow
	telegramReadUserArtifact      = readUserFileNoFollow
	telegramCreateSystemArtifact  = writeTelegramSystemArtifactCreate
	telegramReplaceSystemArtifact = writeTelegramSystemArtifactCAS
	telegramCreateUserArtifact    = func(userName string, identity *persistedUserIdentity, path string, data []byte, perm os.FileMode) error {
		return writeUserFileAtomicCreatePersisted(userName, identity, telegramLookupUserIdentity, path, data, perm)
	}
	telegramReplaceUserArtifact = func(userName string, identity *persistedUserIdentity, path string, expected, data []byte, perm os.FileMode) error {
		return writeUserFileAtomicCASPersisted(userName, identity, telegramLookupUserIdentity, path, expected, data, perm)
	}
	telegramRemoveSystemArtifact = removeSystemFileAndEmptyParentCAS
	telegramRemoveUserArtifact   = func(userName string, identity *persistedUserIdentity, path string, expected []byte) (bool, error) {
		return removeUserFileCASPersisted(userName, identity, telegramLookupUserIdentity, path, expected)
	}
	telegramConfirmUserAbsent = func(userName string, identity *persistedUserIdentity, path string) error {
		return confirmUserFileAbsentPersisted(userName, identity, telegramLookupUserIdentity, path)
	}
	telegramLookupUserIdentity       = lookupLocalUserIdentity
	telegramSystemDirFsync           = syscall.Fsync
	telegramSystemCASAfterQuarantine = func(string) {}
	telegramValidateHermesTarget     = validateHermesTargetRuntime
)

type telegramProxyJournal struct {
	Version    int                                   `json:"version"`
	Generation uint64                                `json:"generation"`
	Targets    map[string]*telegramProxyJournalEntry `json:"targets"`
}

type telegramProxyJournalEntry struct {
	Target                string                 `json:"target"`
	Artifact              string                 `json:"artifact"`
	Identity              *persistedUserIdentity `json:"identity,omitempty"`
	Phase                 string                 `json:"phase"`
	ManagedContent        string                 `json:"managed_content"`
	PendingManagedContent string                 `json:"pending_managed_content,omitempty"`
}

type telegramHermesApplyPreparation struct {
	managed   bool
	reconcile bool
	release   bool
}

type telegramHermesRestorePreparation struct {
	owned          bool
	removedContent []byte
}

func newTelegramProxyJournal() *telegramProxyJournal {
	return &telegramProxyJournal{
		Version: telegramProxyJournalVersion,
		Targets: map[string]*telegramProxyJournalEntry{},
	}
}

func (a *App) telegramProxyJournalPath() string {
	return filepath.Join(a.cfg.CoreDir, "telegram-proxy-journal.json")
}

func (a *App) telegramProxyJournalBackupPath() string {
	return a.telegramProxyJournalPath() + ".bak"
}

func (a *App) telegramProxyJournalLockPath() string {
	return a.telegramProxyJournalPath() + ".lock"
}

func telegramArtifactForTarget(target systemdTargetName) string {
	if target.UserMode {
		return telegramArtifactUserDropIn
	}
	return telegramArtifactSystemDropIn
}

func (a *App) telegramManagedArtifactPath(target systemdTargetName, identity *persistedUserIdentity) (string, error) {
	if target.UserMode {
		if err := validatePersistedUserIdentity(target.User, identity); err != nil {
			return "", err
		}
		return telegramManagedUserPath(a.cfg, target.User, identity.Home, target.Service)
	}
	return telegramManagedSystemPath(a.cfg, target.Service), nil
}

func decodeTelegramProxyJournal(raw []byte) (*telegramProxyJournal, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var journal *telegramProxyJournal
	if err := decoder.Decode(&journal); err != nil {
		return nil, err
	}
	if journal == nil {
		return nil, fmt.Errorf("电报代理 journal 不能是 null")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("电报代理 journal 含多个 JSON 值")
		}
		return nil, err
	}
	if err := validateTelegramProxyJournal(journal); err != nil {
		return nil, err
	}
	return journal, nil
}

func validateTelegramProxyJournal(journal *telegramProxyJournal) error {
	if journal.Version != telegramProxyJournalVersion || journal.Targets == nil {
		return fmt.Errorf("电报代理 journal 版本或结构无效")
	}
	if len(journal.Targets) > maxTelegramTargets {
		return fmt.Errorf("电报代理 journal 目标数超过上限 %d", maxTelegramTargets)
	}
	for key, entry := range journal.Targets {
		if entry == nil || entry.Target != key {
			return fmt.Errorf("电报代理 journal 目标记录无效：%s", key)
		}
		target, err := parseSystemdTargetName(key)
		if err != nil || canonicalTelegramTargetName(target) != key {
			return fmt.Errorf("电报代理 journal 目标无效：%s", key)
		}
		if entry.Artifact != telegramArtifactForTarget(target) {
			return fmt.Errorf("电报代理 journal artifact 与目标不匹配：%s", key)
		}
		if target.UserMode {
			if err := validatePersistedUserIdentity(target.User, entry.Identity); err != nil {
				return fmt.Errorf("电报代理 journal 用户身份无效（%s）：%w", key, err)
			}
		} else if entry.Identity != nil {
			return fmt.Errorf("系统级电报代理 journal 不应携带用户身份：%s", key)
		}
		if err := validateTelegramManagedContent(entry.ManagedContent); err != nil {
			return fmt.Errorf("电报代理 journal managed_content 无效（%s）：%w", key, err)
		}
		if entry.PendingManagedContent != "" {
			if err := validateTelegramManagedContent(entry.PendingManagedContent); err != nil {
				return fmt.Errorf("电报代理 journal pending_managed_content 无效（%s）：%w", key, err)
			}
		}
		switch entry.Phase {
		case telegramPhasePrepared:
			if entry.PendingManagedContent == "" {
				return fmt.Errorf("电报代理 prepared journal 缺少 pending 内容：%s", key)
			}
		case telegramPhaseActive:
			if entry.PendingManagedContent != "" {
				return fmt.Errorf("电报代理 active journal 携带 pending 内容：%s", key)
			}
		case telegramPhaseRestoring:
			// A crash may leave a restoring entry from either an active or a
			// prepared apply, so pending content remains valid in this phase.
		default:
			return fmt.Errorf("电报代理 journal phase 无效：%s", entry.Phase)
		}
	}
	return nil
}

func validateTelegramManagedContent(content string) error {
	if content == "" || len(content) > maxTelegramManagedContentBytes || strings.IndexByte(content, 0) >= 0 {
		return fmt.Errorf("内容为空、过长或含 NUL")
	}
	if !strings.HasPrefix(content, "[Service]\nEnvironment=\"TELEGRAM_PROXY=") || !strings.HasSuffix(content, "\"\n") {
		return fmt.Errorf("内容不是 proxyscene Telegram direct Environment drop-in")
	}
	return nil
}

func telegramProxyFromManagedContent(content string) (string, error) {
	if err := validateTelegramManagedContent(content); err != nil {
		return "", err
	}
	environment, err := effectiveServiceEnvironment(content)
	if err != nil {
		return "", err
	}
	proxy := strings.TrimSpace(environment["TELEGRAM_PROXY"])
	if proxy == "" {
		return "", fmt.Errorf("受管 Telegram drop-in 缺少 TELEGRAM_PROXY")
	}
	return proxy, nil
}

func readTelegramJournalFile(path string) (*telegramProxyJournal, error) {
	raw, err := readRegularFileNoFollow(path, maxTelegramProxyJournalBytes)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("电报代理 journal 必须是 root-only 普通文件：%s", path)
	}
	if os.Geteuid() == 0 {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 {
			return nil, fmt.Errorf("电报代理 journal 必须属于 root：%s", path)
		}
	}
	return decodeTelegramProxyJournal(raw)
}

func (a *App) loadTelegramProxyJournal() (*telegramProxyJournal, error) {
	mainJournal, mainErr := readTelegramJournalFile(a.telegramProxyJournalPath())
	backupJournal, backupErr := readTelegramJournalFile(a.telegramProxyJournalBackupPath())
	if mainErr == nil && backupErr == nil {
		switch {
		case mainJournal.Generation > backupJournal.Generation:
			fmt.Println("警告：Telegram 代理 journal 主文件比备份新，使用主文件恢复")
			return mainJournal, nil
		case backupJournal.Generation > mainJournal.Generation:
			fmt.Println("警告：Telegram 代理 journal 备份比主文件新，使用备份恢复")
			return backupJournal, nil
		case !reflect.DeepEqual(mainJournal, backupJournal):
			return nil, fmt.Errorf("电报代理 journal 主备在同一 generation 内容不一致")
		default:
			return mainJournal, nil
		}
	}
	if mainErr == nil {
		if backupErr != nil && !errors.Is(backupErr, os.ErrNotExist) {
			fmt.Printf("警告：Telegram 代理 journal 备份不可用，使用主文件：%v\n", backupErr)
		}
		return mainJournal, nil
	}
	if backupErr == nil {
		if !errors.Is(mainErr, os.ErrNotExist) {
			fmt.Printf("警告：Telegram 代理主 journal 不可用，使用备份：%v\n", mainErr)
		}
		return backupJournal, nil
	}
	if errors.Is(mainErr, os.ErrNotExist) && errors.Is(backupErr, os.ErrNotExist) {
		return newTelegramProxyJournal(), nil
	}
	return nil, fmt.Errorf("电报代理 journal 与备份均不可用：主=%v，备份=%v", mainErr, backupErr)
}

func (a *App) saveTelegramProxyJournal(journal *telegramProxyJournal) error {
	if journal.Generation == ^uint64(0) {
		return fmt.Errorf("电报代理 journal generation 已耗尽")
	}
	journal.Version = telegramProxyJournalVersion
	if journal.Targets == nil {
		journal.Targets = map[string]*telegramProxyJournalEntry{}
	}
	journal.Generation++
	if err := validateTelegramProxyJournal(journal); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	// The backup is allowed to be one generation ahead. load chooses the newer
	// valid generation, so a crash between these writes preserves prepared state.
	if err := telegramJournalWriteFile(a.telegramProxyJournalBackupPath(), raw, 0o600); err != nil {
		return fmt.Errorf("保存 Telegram 代理 journal 备份失败：%w", err)
	}
	if err := telegramJournalWriteFile(a.telegramProxyJournalPath(), raw, 0o600); err != nil {
		return fmt.Errorf("保存 Telegram 代理 journal 失败：%w", err)
	}
	return nil
}

func openTelegramSystemRemovalDir(path string) (cleanPath string, parentFD, dirFD int, exists bool, err error) {
	cleanPath = filepath.Clean(path)
	dirPath := filepath.Dir(cleanPath)
	parentPath := filepath.Dir(dirPath)
	dirName := filepath.Base(dirPath)
	base := filepath.Base(cleanPath)
	if !filepath.IsAbs(cleanPath) || dirName == "." || dirName == string(os.PathSeparator) || base == "." || base == string(os.PathSeparator) {
		return "", -1, -1, false, fmt.Errorf("systemd drop-in 路径无效：%s", path)
	}
	if err := validateNoSymlinkComponents(parentPath, false); err != nil {
		return "", -1, -1, false, err
	}

	parentFD, err = syscall.Open(parentPath, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return "", -1, -1, false, fmt.Errorf("打开 systemd drop-in 父目录失败：%s：%w", parentPath, err)
	}
	if err := validatePrivilegedDirFD(parentPath, parentFD); err != nil {
		_ = syscall.Close(parentFD)
		return "", -1, -1, false, err
	}

	dirFD, err = syscall.Openat(parentFD, dirName, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err == syscall.ENOENT {
		if err := telegramSystemDirFsync(parentFD); err != nil {
			_ = syscall.Close(parentFD)
			return "", -1, -1, false, fmt.Errorf("同步已删除的 systemd drop-in 目录父目录失败：%s：%w", parentPath, err)
		}
		return cleanPath, parentFD, -1, false, nil
	}
	if err != nil {
		_ = syscall.Close(parentFD)
		return "", -1, -1, false, fmt.Errorf("打开 systemd drop-in 目录失败（拒绝符号链接）：%s：%w", dirPath, err)
	}
	if err := validatePrivilegedDirFD(dirPath, dirFD); err != nil {
		_ = syscall.Close(dirFD)
		_ = syscall.Close(parentFD)
		return "", -1, -1, false, err
	}
	return cleanPath, parentFD, dirFD, true, nil
}

func telegramSystemContentMatchesAny(current []byte, expected [][]byte) bool {
	for _, candidate := range expected {
		if bytes.Equal(current, candidate) {
			return true
		}
	}
	return false
}

func readExpectedTelegramSystemQuarantine(dirFD int, quarantine, quarantinePath string, expected [][]byte) (bool, error) {
	maxBytes := 0
	for _, candidate := range expected {
		if len(candidate) > maxBytes {
			maxBytes = len(candidate)
		}
	}
	if maxBytes == 0 {
		return false, fmt.Errorf("系统级 Telegram drop-in CAS 缺少预期内容")
	}
	current, err := readRegularFileAtNoFollow(dirFD, quarantine, int64(maxBytes)+1)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, errors.Join(errTelegramSystemArtifactChanged, fmt.Errorf("无法安全读取 systemd drop-in 隔离文件，已保留：%s：%w", quarantinePath, err))
	}
	if !telegramSystemContentMatchesAny(current, expected) {
		return true, errors.Join(errTelegramSystemArtifactChanged, fmt.Errorf("systemd drop-in 隔离文件内容与预期不匹配，已保留：%s", quarantinePath))
	}
	return true, nil
}

func finishTelegramSystemArtifactRemoval(cleanPath string, parentFD, dirFD int) error {
	dirPath := filepath.Dir(cleanPath)
	dirName := filepath.Base(dirPath)
	var pinned, current unix.Stat_t
	if err := unix.Fstat(dirFD, &pinned); err != nil {
		return fmt.Errorf("校验 systemd drop-in 目录失败：%s：%w", dirPath, err)
	}
	if err := unix.Fstatat(parentFD, dirName, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			if syncErr := telegramSystemDirFsync(parentFD); syncErr != nil {
				return fmt.Errorf("同步已删除的 systemd drop-in 目录父目录失败：%s：%w", filepath.Dir(dirPath), syncErr)
			}
			return nil
		}
		return fmt.Errorf("校验 systemd drop-in 目录最终名称失败：%s：%w", dirPath, err)
	}
	if pinned.Dev != current.Dev || pinned.Ino != current.Ino {
		// A concurrent administrator replaced the directory name. It is not ours to remove.
		return nil
	}

	if err := unlinkatWithFlags(parentFD, dirName, unix.AT_REMOVEDIR); err != nil {
		switch err {
		case syscall.ENOTEMPTY, syscall.EEXIST:
			return nil
		case syscall.ENOENT:
			// A concurrent/replayed removal still needs a parent-directory barrier.
		default:
			return fmt.Errorf("删除空 systemd drop-in 目录失败：%s：%w", dirPath, err)
		}
	}
	if err := telegramSystemDirFsync(parentFD); err != nil {
		return fmt.Errorf("同步 systemd drop-in 目录删除失败：%s：%w", filepath.Dir(dirPath), err)
	}
	return nil
}

// removeSystemFileAndEmptyParentCAS removes only a claimed regular file whose
// bytes match one of expected. A concurrent writer owns the final name and is
// never unlinked.
func removeSystemFileAndEmptyParentCAS(path string, expected [][]byte) (bool, error) {
	cleanPath, parentFD, dirFD, exists, err := openTelegramSystemRemovalDir(path)
	if err != nil || !exists {
		if parentFD >= 0 {
			_ = syscall.Close(parentFD)
		}
		return false, err
	}
	defer syscall.Close(parentFD)
	defer syscall.Close(dirFD)
	base := filepath.Base(cleanPath)
	quarantine := telegramSystemQuarantineName(base)
	quarantinePath := filepath.Join(filepath.Dir(cleanPath), quarantine)

	for attempt := 0; attempt < maxRemoveTelegramSystemCASAttempts; attempt++ {
		claimErr := unix.Renameat2(dirFD, base, dirFD, quarantine, unix.RENAME_NOREPLACE)
		if claimErr == nil {
			telegramSystemCASAfterQuarantine(cleanPath)
			matched, err := readExpectedTelegramSystemQuarantine(dirFD, quarantine, quarantinePath, expected)
			if err != nil {
				return false, errors.Join(err, restoreTelegramSystemQuarantine(dirFD, base, quarantine, quarantinePath))
			}
			if !matched {
				return false, fmt.Errorf("systemd drop-in 隔离文件在删除前消失，拒绝自动提交：%s", quarantinePath)
			}
			if err := removeTelegramSystemQuarantine(dirFD, quarantine, quarantinePath); err != nil {
				return false, err
			}
			return true, finishTelegramSystemArtifactRemoval(cleanPath, parentFD, dirFD)
		}

		switch claimErr {
		case unix.ENOENT:
			found, replayErr := readExpectedTelegramSystemQuarantine(dirFD, quarantine, quarantinePath, expected)
			if replayErr != nil {
				return false, errors.Join(replayErr, restoreTelegramSystemQuarantine(dirFD, base, quarantine, quarantinePath))
			}
			if found {
				if err := removeTelegramSystemQuarantine(dirFD, quarantine, quarantinePath); err != nil {
					return false, err
				}
				return true, finishTelegramSystemArtifactRemoval(cleanPath, parentFD, dirFD)
			}
			if err := telegramSystemDirFsync(dirFD); err != nil {
				return false, fmt.Errorf("同步已删除的 systemd drop-in 失败：%s：%w", filepath.Dir(cleanPath), err)
			}
			return false, finishTelegramSystemArtifactRemoval(cleanPath, parentFD, dirFD)
		case unix.EEXIST:
			found, replayErr := readExpectedTelegramSystemQuarantine(dirFD, quarantine, quarantinePath, expected)
			if replayErr != nil {
				return false, errors.Join(replayErr, restoreTelegramSystemQuarantine(dirFD, base, quarantine, quarantinePath))
			}
			if !found {
				return false, fmt.Errorf("systemd drop-in 隔离文件在重放前消失，拒绝自动删除：%s", quarantinePath)
			}
			maxBytes := 0
			for _, candidate := range expected {
				if len(candidate) > maxBytes {
					maxBytes = len(candidate)
				}
			}
			current, readErr := readRegularFileAtNoFollow(dirFD, base, int64(maxBytes)+1)
			if errors.Is(readErr, os.ErrNotExist) {
				if err := removeTelegramSystemQuarantine(dirFD, quarantine, quarantinePath); err != nil {
					return false, err
				}
				return true, finishTelegramSystemArtifactRemoval(cleanPath, parentFD, dirFD)
			}
			if readErr != nil {
				return false, errors.Join(errTelegramSystemArtifactChanged, fmt.Errorf("并发 systemd drop-in 无法按普通文件安全读取，隔离文件已保留：%s：%w", quarantinePath, readErr))
			}
			if !telegramSystemContentMatchesAny(current, expected) {
				return false, errors.Join(errTelegramSystemArtifactChanged, fmt.Errorf("并发 systemd drop-in 内容与预期不匹配，隔离文件已保留：%s", quarantinePath))
			}
			if attempt+1 >= maxRemoveTelegramSystemCASAttempts {
				return false, errors.Join(errTelegramSystemArtifactChanged, fmt.Errorf("systemd drop-in 删除重放超过重试上限，隔离文件已保留：%s", quarantinePath))
			}
			if err := removeTelegramSystemQuarantine(dirFD, quarantine, quarantinePath); err != nil {
				return false, err
			}
			continue
		default:
			return false, fmt.Errorf("隔离待删除 systemd drop-in 失败：%s：%w", cleanPath, claimErr)
		}
	}
	return false, errors.Join(errTelegramSystemArtifactChanged, fmt.Errorf("systemd drop-in 删除重放超过重试上限，隔离文件已保留：%s", quarantinePath))
}

func verifyTelegramUserIdentity(target systemdTargetName, identity *persistedUserIdentity) error {
	if !target.UserMode {
		return nil
	}
	_, err := verifyPersistedUserIdentity(target.User, identity, telegramLookupUserIdentity)
	return err
}

func readTelegramManagedArtifact(target systemdTargetName, identity *persistedUserIdentity, path string, max int64) ([]byte, error) {
	if target.UserMode {
		if err := verifyTelegramUserIdentity(target, identity); err != nil {
			return nil, err
		}
		return telegramReadUserArtifact(target.User, path, max)
	}
	return telegramReadSystemArtifact(path, max)
}

func openTelegramSystemArtifactDir(path string, create bool) (string, int, error) {
	cleanPath := filepath.Clean(path)
	if !filepath.IsAbs(cleanPath) || filepath.Base(cleanPath) == "." || filepath.Base(cleanPath) == string(os.PathSeparator) {
		return "", -1, fmt.Errorf("systemd drop-in 路径无效：%s", path)
	}
	dir := filepath.Dir(cleanPath)
	if create {
		if err := ensurePublicDir(dir); err != nil {
			return "", -1, err
		}
	} else if err := validateNoSymlinkComponents(dir, false); err != nil {
		return "", -1, err
	}
	dirFD, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return "", -1, err
	}
	if err := validatePrivilegedDirFD(dir, dirFD); err != nil {
		_ = syscall.Close(dirFD)
		return "", -1, err
	}
	return cleanPath, dirFD, nil
}

func writeTelegramSystemArtifactAtNoReplace(dirFD int, base string, data []byte, perm os.FileMode) error {
	tmpName, tmpFD, err := createTempFileAt(dirFD, base, perm)
	if err != nil {
		return err
	}
	committed := false
	f := os.NewFile(uintptr(tmpFD), tmpName)
	defer func() {
		if !committed {
			_ = syscall.Unlinkat(dirFD, tmpName)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := syscall.Fchmod(int(f.Fd()), uint32(perm.Perm())); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := unix.Renameat2(dirFD, tmpName, dirFD, base, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	if err := telegramSystemDirFsync(dirFD); err != nil {
		return fmt.Errorf("同步 systemd drop-in 目录失败：%w", err)
	}
	committed = true
	return nil
}

func writeTelegramSystemArtifactCreate(path string, data []byte, perm os.FileMode) error {
	cleanPath, dirFD, err := openTelegramSystemArtifactDir(path, true)
	if err != nil {
		return err
	}
	defer syscall.Close(dirFD)
	if err := writeTelegramSystemArtifactAtNoReplace(dirFD, filepath.Base(cleanPath), data, perm); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return errTelegramSystemArtifactChanged
		}
		return err
	}
	return nil
}

func telegramSystemQuarantineName(base string) string {
	return "." + base + telegramSystemQuarantineSuffix
}

func removeTelegramSystemQuarantine(dirFD int, quarantine, path string) error {
	if err := syscall.Unlinkat(dirFD, quarantine); err != nil {
		return fmt.Errorf("删除已确认的 systemd drop-in 隔离文件失败，文件已保留：%s：%w", path, err)
	}
	if err := telegramSystemDirFsync(dirFD); err != nil {
		return fmt.Errorf("同步 systemd drop-in 隔离文件删除失败：%s：%w", path, err)
	}
	return nil
}

func restoreTelegramSystemQuarantine(dirFD int, base, quarantine, path string) error {
	err := unix.Renameat2(dirFD, quarantine, dirFD, base, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("系统级 Telegram drop-in 已被并发创建，隔离文件已保留：%s", path)
	}
	if err != nil {
		return fmt.Errorf("恢复系统级 Telegram drop-in 隔离文件失败，文件已保留：%s：%w", path, err)
	}
	if err := telegramSystemDirFsync(dirFD); err != nil {
		return fmt.Errorf("同步系统级 Telegram drop-in 隔离文件恢复失败：%s：%w", path, err)
	}
	return nil
}

func telegramSystemContentMismatchError(message, path string, cause error) error {
	if cause != nil {
		return fmt.Errorf("%s：%s：%w", message, path, cause)
	}
	return fmt.Errorf("%s：%s", message, path)
}

func replayTelegramSystemWriteQuarantine(dirFD int, base, quarantine, path string, expected, data []byte) (found, resume bool, err error) {
	quarantined, quarantineErr := readRegularFileAtNoFollow(dirFD, quarantine, int64(len(expected))+1)
	if errors.Is(quarantineErr, os.ErrNotExist) {
		return false, false, nil
	}
	if quarantineErr != nil || !bytes.Equal(quarantined, expected) {
		_, finalErr := readRegularFileAtNoFollow(dirFD, base, int64(len(data))+1)
		if errors.Is(finalErr, os.ErrNotExist) {
			restoreErr := restoreTelegramSystemQuarantine(dirFD, base, quarantine, path)
			return true, false, errors.Join(
				errTelegramSystemArtifactChanged,
				telegramSystemContentMismatchError("systemd drop-in 隔离文件与预期内容不一致，已尝试恢复最终名称", path, quarantineErr),
				restoreErr,
			)
		}
		return true, false, errors.Join(
			errTelegramSystemArtifactChanged,
			telegramSystemContentMismatchError("systemd drop-in 隔离文件与预期内容不一致，最终名称也已存在；两个文件均已保留", path, quarantineErr),
			finalErr,
		)
	}

	current, readErr := readRegularFileAtNoFollow(dirFD, base, int64(len(data))+1)
	if errors.Is(readErr, os.ErrNotExist) {
		return true, true, nil
	}
	if readErr == nil && bytes.Equal(current, data) {
		return true, false, removeTelegramSystemQuarantine(dirFD, quarantine, path)
	}
	cleanupErr := removeTelegramSystemQuarantine(dirFD, quarantine, path)
	if readErr != nil {
		return true, false, errors.Join(errTelegramSystemArtifactChanged, fmt.Errorf("无法安全读取并发 systemd drop-in，已保留最终名称：%s：%w", path, readErr), cleanupErr)
	}
	return true, false, errors.Join(errTelegramSystemArtifactChanged, cleanupErr)
}

func writeTelegramSystemArtifactCAS(path string, expected, data []byte, perm os.FileMode) error {
	cleanPath, dirFD, err := openTelegramSystemArtifactDir(path, false)
	if err != nil {
		return err
	}
	defer syscall.Close(dirFD)
	base := filepath.Base(cleanPath)
	quarantine := telegramSystemQuarantineName(base)
	quarantinePath := filepath.Join(filepath.Dir(cleanPath), quarantine)

	claimErr := unix.Renameat2(dirFD, base, dirFD, quarantine, unix.RENAME_NOREPLACE)
	if claimErr != nil {
		switch claimErr {
		case unix.ENOENT, unix.EEXIST:
			found, resume, replayErr := replayTelegramSystemWriteQuarantine(dirFD, base, quarantine, quarantinePath, expected, data)
			if replayErr != nil {
				return replayErr
			}
			if found {
				if resume {
					break
				}
				return nil
			}
			if errors.Is(claimErr, unix.ENOENT) {
				return errTelegramSystemArtifactChanged
			}
			return fmt.Errorf("systemd drop-in 隔离文件在重放前消失，拒绝自动覆盖：%s", quarantinePath)
		default:
			return fmt.Errorf("隔离系统级 Telegram drop-in 失败：%s：%w", cleanPath, claimErr)
		}
	} else {
		telegramSystemCASAfterQuarantine(cleanPath)
	}

	current, readErr := readRegularFileAtNoFollow(dirFD, quarantine, int64(len(expected))+1)
	if readErr != nil || !bytes.Equal(current, expected) {
		return errors.Join(
			errTelegramSystemArtifactChanged,
			telegramSystemContentMismatchError("系统级 Telegram drop-in 隔离内容与预期不一致", cleanPath, readErr),
			restoreTelegramSystemQuarantine(dirFD, base, quarantine, quarantinePath),
		)
	}
	if err := writeTelegramSystemArtifactAtNoReplace(dirFD, base, data, perm); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return errors.Join(errTelegramSystemArtifactChanged, removeTelegramSystemQuarantine(dirFD, quarantine, quarantinePath))
		}
		return errors.Join(
			fmt.Errorf("提交系统级 Telegram drop-in 替换失败：%w", err),
			restoreTelegramSystemQuarantine(dirFD, base, quarantine, quarantinePath),
		)
	}
	return removeTelegramSystemQuarantine(dirFD, quarantine, quarantinePath)
}

func writeTelegramManagedArtifact(target systemdTargetName, identity *persistedUserIdentity, path string, current []byte, missing bool, data []byte) error {
	if target.UserMode {
		if err := verifyTelegramUserIdentity(target, identity); err != nil {
			return err
		}
		if missing {
			return telegramCreateUserArtifact(target.User, identity, path, data, 0o644)
		}
		return telegramReplaceUserArtifact(target.User, identity, path, current, data, 0o644)
	}
	if missing {
		return telegramCreateSystemArtifact(path, data, 0o644)
	}
	return telegramReplaceSystemArtifact(path, current, data, 0o644)
}

func removeTelegramManagedArtifact(target systemdTargetName, identity *persistedUserIdentity, path string, current []byte, missing bool, replayExpected ...[]byte) (bool, error) {
	if target.UserMode {
		if err := verifyTelegramUserIdentity(target, identity); err != nil {
			return false, err
		}
		if missing {
			return false, telegramConfirmUserAbsent(target.User, identity, path)
		}
		return telegramRemoveUserArtifact(target.User, identity, path, current)
	}
	expected := make([][]byte, 0, 1+len(replayExpected))
	if len(current) != 0 {
		expected = append(expected, current)
	}
	for _, candidate := range replayExpected {
		if len(candidate) != 0 {
			expected = append(expected, candidate)
		}
	}
	return telegramRemoveSystemArtifact(path, expected)
}

func telegramContentOwnedByEntry(raw []byte, entry *telegramProxyJournalEntry) bool {
	return bytes.Equal(raw, []byte(entry.ManagedContent)) ||
		(entry.PendingManagedContent != "" && bytes.Equal(raw, []byte(entry.PendingManagedContent)))
}

func telegramOwnedContentCandidates(entry *telegramProxyJournalEntry) [][]byte {
	candidates := [][]byte{[]byte(entry.ManagedContent)}
	if entry.PendingManagedContent != "" && entry.PendingManagedContent != entry.ManagedContent {
		candidates = append(candidates, []byte(entry.PendingManagedContent))
	}
	return candidates
}

func (a *App) prepareHermesTelegramApply(target systemdTargetName, desired []byte) (telegramHermesApplyPreparation, error) {
	result := telegramHermesApplyPreparation{}
	key := canonicalTelegramTargetName(target)
	err := withFileLock(a.telegramProxyJournalLockPath(), func() error {
		journal, err := a.loadTelegramProxyJournal()
		if err != nil {
			return err
		}
		entry := journal.Targets[key]
		var identity *persistedUserIdentity
		if target.UserMode {
			if entry == nil {
				identity, err = capturePersistedUserIdentity(target.User, telegramLookupUserIdentity)
			} else {
				identity = entry.Identity
				err = verifyTelegramUserIdentity(target, identity)
			}
			if err != nil {
				return err
			}
		}
		if err := telegramValidateHermesTarget(target, identity, ""); err != nil {
			return err
		}
		path, err := a.telegramManagedArtifactPath(target, identity)
		if err != nil {
			return err
		}
		current, readErr := readTelegramManagedArtifact(target, identity, path, maxTelegramManagedContentBytes)
		missing := errors.Is(readErr, os.ErrNotExist)
		if entry == nil {
			if readErr == nil {
				return fmt.Errorf("目标 %s 的 proxyscene drop-in 已存在但没有 ownership journal，拒绝覆盖：%s", key, path)
			}
			if !missing {
				return fmt.Errorf("检查目标 %s 的未托管 drop-in 失败，拒绝覆盖：%w", key, readErr)
			}
			entry = &telegramProxyJournalEntry{
				Target:                key,
				Artifact:              telegramArtifactForTarget(target),
				Identity:              identity,
				Phase:                 telegramPhasePrepared,
				ManagedContent:        string(desired),
				PendingManagedContent: string(desired),
			}
			journal.Targets[key] = entry
			if err := a.saveTelegramProxyJournal(journal); err != nil {
				return err
			}
			result.managed = true
			if err := writeTelegramManagedArtifact(target, identity, path, nil, true, desired); err != nil {
				return fmt.Errorf("写入目标 %s 的 Telegram drop-in 失败：%w", key, err)
			}
			result.reconcile = true
			return nil
		}

		result.managed = true
		if entry.Phase == telegramPhaseRestoring {
			if readErr == nil && telegramContentOwnedByEntry(current, entry) {
				if _, err := removeTelegramManagedArtifact(target, identity, path, current, false); err != nil {
					return fmt.Errorf("继续清理目标 %s 的 Telegram drop-in 失败：%w", key, err)
				}
			} else if readErr != nil && !missing {
				fmt.Printf("警告：目标 %s 的 restoring drop-in 无法安全读取，已保留文件：%v\n", key, readErr)
			} else if readErr == nil {
				fmt.Printf("警告：目标 %s 的 restoring drop-in 已被修改，已保留文件并释放 ownership\n", key)
			}
			result.managed = false
			result.reconcile = true
			result.release = true
			return nil
		}

		if readErr != nil && !missing {
			entry.Phase = telegramPhaseRestoring
			if err := a.saveTelegramProxyJournal(journal); err != nil {
				return err
			}
			fmt.Printf("警告：目标 %s 的受管 drop-in 无法安全读取，已保留文件并准备释放 ownership：%v\n", key, readErr)
			result.managed = false
			result.reconcile = true
			result.release = true
			return nil
		}
		if readErr == nil && !telegramContentOwnedByEntry(current, entry) {
			entry.Phase = telegramPhaseRestoring
			if err := a.saveTelegramProxyJournal(journal); err != nil {
				return err
			}
			fmt.Printf("警告：目标 %s 的受管 drop-in 已被操作员修改，已保留文件并准备释放 ownership\n", key)
			result.managed = false
			result.reconcile = true
			result.release = true
			return nil
		}
		if entry.Phase == telegramPhaseActive && readErr == nil &&
			bytes.Equal(current, []byte(entry.ManagedContent)) && entry.ManagedContent == string(desired) {
			return nil
		}

		entry.Phase = telegramPhasePrepared
		entry.PendingManagedContent = string(desired)
		if err := a.saveTelegramProxyJournal(journal); err != nil {
			return err
		}
		if readErr != nil || !bytes.Equal(current, desired) {
			if err := writeTelegramManagedArtifact(target, identity, path, current, missing, desired); err != nil {
				return fmt.Errorf("写入目标 %s 的 Telegram drop-in 失败：%w", key, err)
			}
		}
		result.reconcile = true
		return nil
	})
	return result, err
}

func (a *App) commitHermesTelegramApply(target systemdTargetName) error {
	key := canonicalTelegramTargetName(target)
	return withFileLock(a.telegramProxyJournalLockPath(), func() error {
		journal, err := a.loadTelegramProxyJournal()
		if err != nil {
			return err
		}
		entry := journal.Targets[key]
		if entry == nil {
			return fmt.Errorf("目标 %s 缺少 Telegram ownership journal", key)
		}
		if entry.Phase != telegramPhasePrepared || entry.PendingManagedContent == "" {
			return fmt.Errorf("目标 %s 的 Telegram apply journal 状态无效", key)
		}
		if err := verifyTelegramUserIdentity(target, entry.Identity); err != nil {
			return err
		}
		expectedProxy, err := telegramProxyFromManagedContent(entry.PendingManagedContent)
		if err != nil {
			return fmt.Errorf("目标 %s 的 pending Telegram drop-in 无效：%w", key, err)
		}
		if err := telegramValidateHermesTarget(target, entry.Identity, expectedProxy); err != nil {
			return err
		}
		path, err := a.telegramManagedArtifactPath(target, entry.Identity)
		if err != nil {
			return err
		}
		current, err := readTelegramManagedArtifact(target, entry.Identity, path, maxTelegramManagedContentBytes)
		if err != nil {
			return err
		}
		if !bytes.Equal(current, []byte(entry.PendingManagedContent)) {
			return fmt.Errorf("目标 %s 的 Telegram drop-in 在重启后发生变化，拒绝提交 ownership", key)
		}
		entry.ManagedContent = entry.PendingManagedContent
		entry.PendingManagedContent = ""
		entry.Phase = telegramPhaseActive
		return a.saveTelegramProxyJournal(journal)
	})
}

func (a *App) validatePreparedHermesTelegramRestart(target systemdTargetName) error {
	key := canonicalTelegramTargetName(target)
	return withFileLock(a.telegramProxyJournalLockPath(), func() error {
		journal, err := a.loadTelegramProxyJournal()
		if err != nil {
			return err
		}
		entry := journal.Targets[key]
		if entry == nil || entry.Phase != telegramPhasePrepared {
			return nil
		}
		if err := verifyTelegramUserIdentity(target, entry.Identity); err != nil {
			return err
		}
		expectedProxy, err := telegramProxyFromManagedContent(entry.PendingManagedContent)
		if err != nil {
			return fmt.Errorf("目标 %s 的 pending Telegram drop-in 无效：%w", key, err)
		}
		if err := telegramValidateHermesTarget(target, entry.Identity, expectedProxy); err != nil {
			return fmt.Errorf("重启目标 %s 前最终 Telegram 代理校验失败：%w", key, err)
		}
		return nil
	})
}

func (a *App) prepareHermesTelegramRestore(target systemdTargetName) (telegramHermesRestorePreparation, error) {
	key := canonicalTelegramTargetName(target)
	result := telegramHermesRestorePreparation{}
	err := withFileLock(a.telegramProxyJournalLockPath(), func() error {
		journal, err := a.loadTelegramProxyJournal()
		if err != nil {
			return err
		}
		entry := journal.Targets[key]
		if entry == nil {
			return nil
		}
		result.owned = true
		if err := verifyTelegramUserIdentity(target, entry.Identity); err != nil {
			return err
		}
		path, err := a.telegramManagedArtifactPath(target, entry.Identity)
		if err != nil {
			return err
		}
		if entry.Phase != telegramPhaseRestoring {
			entry.Phase = telegramPhaseRestoring
			if err := a.saveTelegramProxyJournal(journal); err != nil {
				return err
			}
		}
		current, readErr := readTelegramManagedArtifact(target, entry.Identity, path, maxTelegramManagedContentBytes)
		if errors.Is(readErr, os.ErrNotExist) {
			_, err := removeTelegramManagedArtifact(target, entry.Identity, path, nil, true, telegramOwnedContentCandidates(entry)...)
			return err
		}
		if readErr != nil {
			fmt.Printf("警告：目标 %s 的受管 drop-in 无法安全读取，已保留文件：%v\n", key, readErr)
			return nil
		}
		if !telegramContentOwnedByEntry(current, entry) {
			fmt.Printf("警告：目标 %s 的受管 drop-in 已被操作员修改，已保留文件\n", key)
			return nil
		}
		if _, err := removeTelegramManagedArtifact(target, entry.Identity, path, current, false); err != nil {
			return err
		}
		result.removedContent = append([]byte(nil), current...)
		return nil
	})
	return result, err
}

func (a *App) commitHermesTelegramRestore(target systemdTargetName) error {
	key := canonicalTelegramTargetName(target)
	return withFileLock(a.telegramProxyJournalLockPath(), func() error {
		journal, err := a.loadTelegramProxyJournal()
		if err != nil {
			return err
		}
		entry := journal.Targets[key]
		if entry == nil {
			return nil
		}
		if entry.Phase != telegramPhaseRestoring {
			return fmt.Errorf("目标 %s 的 Telegram journal 尚未进入 restoring", key)
		}
		if err := verifyTelegramUserIdentity(target, entry.Identity); err != nil {
			return err
		}
		delete(journal.Targets, key)
		return a.saveTelegramProxyJournal(journal)
	})
}

func (a *App) checkHermesTelegramTargetIdentity(target systemdTargetName) (bool, error) {
	if !target.UserMode {
		return false, nil
	}
	key := canonicalTelegramTargetName(target)
	managed := false
	err := withFileLock(a.telegramProxyJournalLockPath(), func() error {
		journal, err := a.loadTelegramProxyJournal()
		if err != nil {
			return err
		}
		entry := journal.Targets[key]
		if entry == nil {
			return nil
		}
		managed = true
		return verifyTelegramUserIdentity(target, entry.Identity)
	})
	return managed, err
}

func (a *App) verifyHermesTelegramTargetIdentity(target systemdTargetName) error {
	managed, err := a.checkHermesTelegramTargetIdentity(target)
	if err != nil {
		return err
	}
	if target.UserMode && !managed {
		return fmt.Errorf("目标 %s 缺少 Telegram ownership journal，拒绝操作用户服务", canonicalTelegramTargetName(target))
	}
	return nil
}

func (a *App) allManagedHermesTelegramTargets() ([]systemdTargetName, error) {
	var targets []systemdTargetName
	err := withFileLock(a.telegramProxyJournalLockPath(), func() error {
		journal, err := a.loadTelegramProxyJournal()
		if err != nil {
			return err
		}
		for key := range journal.Targets {
			target, err := parseSystemdTargetName(key)
			if err != nil {
				return err
			}
			targets = appendUniqueTelegramTarget(targets, target)
		}
		return nil
	})
	return targets, err
}

func (a *App) reloadAndRestartTelegramArtifactTarget(target systemdTargetName) error {
	if target.UserMode {
		identity, managed, err := a.knownTelegramTargetIdentity(target)
		if err != nil {
			return err
		}
		if !managed {
			return fmt.Errorf("目标 %s 缺少 Telegram ownership journal，拒绝操作用户服务", canonicalTelegramTargetName(target))
		}
		if err := userSystemctlRun(target.User, identity, "重新加载用户级 systemd 配置", "daemon-reload"); err != nil {
			return err
		}
		if err := a.verifyHermesTelegramTargetIdentity(target); err != nil {
			return err
		}
		if !telegramUserUnitExists(target.User, target.Service) {
			return nil
		}
		if err := a.verifyHermesTelegramTargetIdentity(target); err != nil {
			return err
		}
	} else {
		if err := systemctlRun("重新加载 systemd 配置", "daemon-reload"); err != nil {
			return err
		}
		if !telegramSystemUnitExists(target.Service) {
			return nil
		}
	}
	return a.restartTelegramTarget(target)
}

func telegramTargetUnitInstalled(target systemdTargetName) bool {
	if target.UserMode {
		return telegramUserUnitExists(target.User, target.Service)
	}
	return telegramSystemUnitExists(target.Service)
}

func (a *App) reloadHermesTelegramTargetManager(target systemdTargetName, identity *persistedUserIdentity) error {
	if target.UserMode {
		if err := a.verifyHermesTelegramTargetIdentity(target); err != nil {
			return err
		}
		return userSystemctlRun(target.User, identity, "重新加载用户级 systemd 配置", "daemon-reload")
	}
	return systemctlRun("重新加载 systemd 配置", "daemon-reload")
}

func (a *App) reloadValidateAndRestartManagedHermesTarget(target systemdTargetName, identity *persistedUserIdentity) error {
	if err := a.reloadHermesTelegramTargetManager(target, identity); err != nil {
		return err
	}
	if !telegramTargetUnitInstalled(target) {
		return nil
	}
	if target.UserMode {
		if err := a.verifyHermesTelegramTargetIdentity(target); err != nil {
			return err
		}
	}
	if err := telegramValidateHermesTarget(target, identity, ""); err != nil {
		return fmt.Errorf("恢复时 Hermes 服务 %s 的运行配置验证失败：%w", canonicalTelegramTargetName(target), err)
	}
	if target.UserMode {
		if err := a.verifyHermesTelegramTargetIdentity(target); err != nil {
			return err
		}
	}
	return a.restartTelegramTarget(target)
}

func (a *App) restoreHermesTelegramArtifactForRetry(target systemdTargetName, content []byte) error {
	if len(content) == 0 {
		return nil
	}
	key := canonicalTelegramTargetName(target)
	return withFileLock(a.telegramProxyJournalLockPath(), func() error {
		journal, err := a.loadTelegramProxyJournal()
		if err != nil {
			return err
		}
		entry := journal.Targets[key]
		if entry == nil || entry.Phase != telegramPhaseRestoring {
			return fmt.Errorf("目标 %s 缺少 restoring Telegram ownership journal", key)
		}
		if err := verifyTelegramUserIdentity(target, entry.Identity); err != nil {
			return err
		}
		path, err := a.telegramManagedArtifactPath(target, entry.Identity)
		if err != nil {
			return err
		}
		current, readErr := readTelegramManagedArtifact(target, entry.Identity, path, maxTelegramManagedContentBytes)
		if readErr == nil {
			if bytes.Equal(current, content) {
				return nil
			}
			return fmt.Errorf("目标 %s 的 Telegram drop-in 在恢复回滚前出现并发变化，拒绝覆盖", key)
		}
		if !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
		return writeTelegramManagedArtifact(target, entry.Identity, path, nil, true, content)
	})
}

func (a *App) cleanupHermesTelegramTarget(target systemdTargetName) error {
	var identity *persistedUserIdentity
	if target.UserMode {
		var managed bool
		var err error
		identity, managed, err = a.knownTelegramTargetIdentity(target)
		if err != nil {
			return err
		}
		if !managed {
			return nil
		}
	}
	// Validate before removing the drop-in so an operator-replaced unit leaves
	// both the artifact and ownership untouched.
	if telegramTargetUnitInstalled(target) {
		if err := telegramValidateHermesTarget(target, identity, ""); err != nil {
			return fmt.Errorf("当前 Hermes 服务 %s 已不再是可安全管理的 gateway：%w", canonicalTelegramTargetName(target), err)
		}
	}
	prepared, err := a.prepareHermesTelegramRestore(target)
	if err != nil || !prepared.owned {
		return err
	}
	if err := a.reloadValidateAndRestartManagedHermesTarget(target, identity); err != nil {
		rollbackArtifactErr := a.restoreHermesTelegramArtifactForRetry(target, prepared.removedContent)
		var rollbackReloadErr error
		if rollbackArtifactErr == nil && len(prepared.removedContent) != 0 {
			rollbackReloadErr = a.reloadHermesTelegramTargetManager(target, identity)
		}
		return errors.Join(
			err,
			wrapRollbackError("恢复 Hermes Telegram drop-in", rollbackArtifactErr),
			wrapRollbackError("重新加载恢复后的 Hermes systemd 配置", rollbackReloadErr),
		)
	}
	return a.commitHermesTelegramRestore(target)
}

func (a *App) cleanupLegacyTelegramTargets(st *Store, desired, ready map[string]bool) error {
	if st == nil || len(st.TelegramTargets) == 0 {
		return nil
	}
	next := make([]string, 0, len(st.TelegramTargets))
	var errs []error
	for _, name := range st.TelegramTargets {
		target, err := parseSystemdTargetName(name)
		if err != nil {
			next = appendUniqueString(next, name)
			errs = append(errs, fmt.Errorf("旧 Telegram migration 目标无效，已保留记录：%s：%w", name, err))
			continue
		}
		key := canonicalTelegramTargetName(target)
		if desired[key] && !ready[key] {
			next = appendUniqueString(next, key)
			continue
		}
		if err := a.cleanupLegacyTelegramTarget(st, target); err != nil {
			next = appendUniqueString(next, key)
			errs = append(errs, fmt.Errorf("协调旧 Telegram migration 目标 %s 失败：%w", key, err))
		}
	}
	st.TelegramTargets = next
	return errors.Join(errs...)
}

func (a *App) cleanupLegacyTelegramTarget(st *Store, target systemdTargetName) error {
	key := canonicalTelegramTargetName(target)
	if st.RuntimeConfig == nil {
		fmt.Printf("警告：旧 Telegram 目标 %s 缺少 RuntimeConfig，无法证明 drop-in 字节归属；已保留文件\n", key)
		return a.reloadAndRestartTelegramArtifactTarget(target)
	}
	legacyCfg := st.RuntimeConfig.applyTo(a.cfg)
	if target.UserMode {
		return fmt.Errorf("旧 Telegram 用户级 ownership 记录 %s 没有 uid/gid/home 身份绑定，拒绝读取、删除或重启同名当前用户；请人工核对后清理", key)
	}

	path := telegramLegacySystemPath(a.cfg, target.Service)
	expectedDropIn := []byte("[Service]\nEnvironmentFile=-/etc/openclaw-hermes-tg-proxy.env\n")
	currentDropIn, dropInErr := telegramReadSystemArtifact(path, int64(len(expectedDropIn))+1)
	switch {
	case errors.Is(dropInErr, os.ErrNotExist):
		// The old drop-in may already have been removed before a crash. The shared
		// env file is deliberately retained because Store cannot prove that no
		// other legacy drop-in references it.
	case dropInErr != nil:
		fmt.Printf("警告：旧 Telegram drop-in %s 无法安全读取，已保留文件：%v\n", path, dropInErr)
	case !bytes.Equal(currentDropIn, expectedDropIn):
		fmt.Printf("警告：旧 Telegram drop-in %s 与历史模板不一致，已保留文件\n", path)
	default:
		expectedEnv := []byte(telegramProxyEnvContent(legacyCfg))
		envPath := telegramLegacyEnvPath()
		currentEnv, envErr := telegramReadSystemArtifact(envPath, int64(len(expectedEnv))+1)
		if envErr != nil || !bytes.Equal(currentEnv, expectedEnv) {
			if envErr != nil && !errors.Is(envErr, os.ErrNotExist) {
				fmt.Printf("警告：旧 Telegram 环境文件 %s 无法安全读取，已保留 drop-in：%v\n", envPath, envErr)
			} else {
				fmt.Printf("警告：旧 Telegram 环境文件 %s 与 RuntimeConfig 生成内容不一致，已保留 drop-in\n", envPath)
			}
		} else if _, err := telegramRemoveSystemArtifact(path, [][]byte{expectedDropIn}); err != nil {
			return err
		} else {
			fmt.Printf("提示：旧共享 Telegram 环境文件 %s 已保留；Store 无法证明不存在其它引用\n", envPath)
		}
	}
	return a.reloadAndRestartTelegramArtifactTarget(target)
}
