package manager

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const installationOwnershipVersion = 1
const maxInstallationOwnershipBytes int64 = 64 << 10

const (
	defaultInstallLockPath   = "/run/proxyscene-install.lock"
	defaultHostOwnershipPath = "/etc/proxyscene-host-ownership.json"
	defaultHostLockPath      = "/run/proxyscene-host-ownership.lock"
)

var (
	installLockPath   = defaultInstallLockPath
	hostOwnershipPath = defaultHostOwnershipPath
	hostLockPath      = defaultHostLockPath
	hostOwnershipSync = fsyncDir
)

const (
	inheritedInstallLockFDEnv = "PROXYSCENE_INHERITED_INSTALL_LOCK_FD"
	inheritedHostLockFDEnv    = "PROXYSCENE_INHERITED_HOST_LOCK_FD"
	inheritedStoreLockFDEnv   = "PROXYSCENE_INHERITED_STORE_LOCK_FD"
)

func installerLockHandoffRequested() (bool, error) {
	installPresent := strings.TrimSpace(os.Getenv(inheritedInstallLockFDEnv)) != ""
	hostPresent := strings.TrimSpace(os.Getenv(inheritedHostLockFDEnv)) != ""
	storePresent := strings.TrimSpace(os.Getenv(inheritedStoreLockFDEnv)) != ""
	if !installPresent && !hostPresent && !storePresent {
		return false, nil
	}
	// A fresh CoreDir has no state-lock inode when install.sh acquires its two
	// outer locks. In that case the manager validates both inherited fds, then
	// creates and acquires the state lock itself after the CoreDir is committed.
	if installPresent && hostPresent {
		return true, nil
	}
	return false, fmt.Errorf("安装器锁交接组合无效：必须不提供继承锁，或同时提供 install、host 并可选提供 state 锁")
}

const managerMarkerText = "由 proxyscene 管理\n"
const installerMarkerText = "由 proxyscene 安装器管理\n"

type installationOwnership struct {
	Version        int    `json:"version"`
	CoreDir        string `json:"core_dir"`
	InstallBin     string `json:"install_bin"`
	SystemdService string `json:"systemd_service"`
	RestoreService string `json:"restore_service"`
}

func (a *App) expectedInstallationOwnership() installationOwnership {
	return installationOwnership{
		Version:        installationOwnershipVersion,
		CoreDir:        a.cfg.CoreDir,
		InstallBin:     a.cfg.InstallBin,
		SystemdService: a.cfg.SystemdService,
		RestoreService: a.cfg.RestoreService,
	}
}

func (a *App) validateManagedCoreDir() error {
	info, err := os.Lstat(a.cfg.CoreDir)
	if err != nil {
		return fmt.Errorf("proxyscene 管理目录不可用：%w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("proxyscene 管理目录必须是非符号链接目录：%s", a.cfg.CoreDir)
	}
	if err := validatePrivilegedDirInfo(a.cfg.CoreDir, info); err != nil {
		return err
	}
	marker, err := readRegularFileNoFollow(a.cfg.MarkerPath(), 256)
	if err != nil {
		return fmt.Errorf("管理目录缺少可信 ownership marker：%s：%w", a.cfg.MarkerPath(), err)
	}
	if !bytes.Equal(marker, []byte(managerMarkerText)) && !bytes.Equal(marker, []byte(installerMarkerText)) {
		return fmt.Errorf("管理目录 ownership marker 内容无效：%s", a.cfg.MarkerPath())
	}
	return nil
}

func (a *App) ensureInstallationOwnership() error {
	if err := a.validateManagedCoreDir(); err != nil {
		return err
	}
	expected := a.expectedInstallationOwnership()
	actual, err := a.loadInstallationOwnership()
	if err == nil {
		return compareInstallationOwnership(actual, expected)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := runningExecutableMatches(a.cfg.InstallBin); err != nil {
		return err
	}
	return a.writeInstallationOwnership(expected)
}

func (a *App) validateInstallationOwnership() error {
	if err := a.validateManagedCoreDir(); err != nil {
		return err
	}
	actual, err := a.loadInstallationOwnership()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("缺少安装 ownership 记录：%s；请从受信 bundle 重新运行 proxyscene install --skip-node", a.cfg.InstallationOwnershipPath())
		}
		return err
	}
	return compareInstallationOwnership(actual, a.expectedInstallationOwnership())
}

// withHostOwnership serializes all state-changing instances and ensures that
// only one locator set can own the host-wide /etc and user configuration.
func (a *App) withHostOwnership(fn func() error) error {
	return withFileLockOrInherited(hostLockPath, inheritedHostLockFDEnv, func() error {
		if err := a.ensureHostOwnership(); err != nil {
			return err
		}
		return fn()
	})
}

func (a *App) ensureHostOwnership() error {
	expected := a.expectedInstallationOwnership()
	actual, err := loadOwnershipRecord(hostOwnershipPath, "主机 ownership 记录")
	if err == nil {
		if compareErr := compareInstallationOwnership(actual, expected); compareErr != nil {
			return fmt.Errorf("主机已由另一套 proxyscene 安装接管：%w", compareErr)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := marshalInstallationOwnership(expected)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(hostOwnershipPath, data, 0o600); err != nil {
		return fmt.Errorf("写入主机 ownership 记录失败：%w", err)
	}
	return nil
}

func (a *App) releaseHostOwnership() error {
	actual, err := loadOwnershipRecord(hostOwnershipPath, "主机 ownership 记录")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if syncErr := hostOwnershipSync(filepath.Dir(hostOwnershipPath)); syncErr != nil {
				return fmt.Errorf("主机 ownership 记录已缺失，但无法持久化释放状态：%w", syncErr)
			}
			return nil
		}
		return err
	}
	if err := compareInstallationOwnership(actual, a.expectedInstallationOwnership()); err != nil {
		return fmt.Errorf("拒绝释放其他安装的主机 ownership：%w", err)
	}
	if err := os.Remove(hostOwnershipPath); err != nil {
		return fmt.Errorf("删除主机 ownership 记录失败：%w", err)
	}
	if err := hostOwnershipSync(filepath.Dir(hostOwnershipPath)); err != nil {
		return fmt.Errorf("主机 ownership 记录已删除，但持久化释放失败；重试卸载可重新建立目录屏障：%w", err)
	}
	return nil
}

func compareInstallationOwnership(actual, expected installationOwnership) error {
	if actual != expected {
		return fmt.Errorf("当前 locator 与已接管安装不匹配：记录=%+v 当前=%+v", actual, expected)
	}
	return nil
}

func (a *App) loadInstallationOwnership() (installationOwnership, error) {
	return loadOwnershipRecord(a.cfg.InstallationOwnershipPath(), "安装 ownership 记录")
}

func loadOwnershipRecord(path, label string) (installationOwnership, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return installationOwnership{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return installationOwnership{}, fmt.Errorf("%s必须是非符号链接普通文件：%s", label, path)
	}
	if os.Geteuid() == 0 {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 {
			return installationOwnership{}, fmt.Errorf("%s必须属于 root：%s", label, path)
		}
		if info.Mode().Perm() != 0o600 {
			return installationOwnership{}, fmt.Errorf("%s权限必须为 0600：%s（当前 %#o）", label, path, info.Mode().Perm())
		}
	}
	data, err := readRegularFileNoFollow(path, maxInstallationOwnershipBytes)
	if err != nil {
		return installationOwnership{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var ownership installationOwnership
	if err := decoder.Decode(&ownership); err != nil {
		return installationOwnership{}, fmt.Errorf("%s损坏：%w", label, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("含多余 JSON 值")
		}
		return installationOwnership{}, fmt.Errorf("%s损坏：%w", label, err)
	}
	if ownership.Version != installationOwnershipVersion {
		return installationOwnership{}, fmt.Errorf("不支持的安装 ownership 版本：%d", ownership.Version)
	}
	return ownership, nil
}

func (a *App) writeInstallationOwnership(ownership installationOwnership) error {
	data, err := marshalInstallationOwnership(ownership)
	if err != nil {
		return err
	}
	return writeFileAtomic(a.cfg.InstallationOwnershipPath(), data, 0o600)
}

func marshalInstallationOwnership(ownership installationOwnership) ([]byte, error) {
	data, err := json.MarshalIndent(ownership, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func runningExecutableMatches(configured string) error {
	running, err := os.Executable()
	if err != nil {
		return fmt.Errorf("无法定位当前 proxyscene 可执行文件：%w", err)
	}
	runningInfo, err := os.Stat(running)
	if err != nil {
		return fmt.Errorf("无法检查当前 proxyscene 可执行文件：%w", err)
	}
	configuredInfo, err := os.Stat(configured)
	if err != nil {
		return fmt.Errorf("配置的 PROXYSCENE_SWITCH_BIN 不可用：%w", err)
	}
	if !os.SameFile(runningInfo, configuredInfo) {
		return fmt.Errorf("当前运行文件 %s 与 PROXYSCENE_SWITCH_BIN %s 不是同一文件，拒绝绑定安装 ownership", running, configured)
	}
	return nil
}
