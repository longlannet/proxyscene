package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const managedSystemdUnitHeader = "# Managed by proxyscene\n"

var systemctlRun = func(label string, args ...string) error {
	if len(args) == 3 && args[0] == "try-restart" && args[1] == "--" {
		return runSystemdRestart(label, args[2], func(ctx context.Context, commandArgs ...string) (*exec.Cmd, error) {
			return exec.CommandContext(ctx, "systemctl", commandArgs...), nil
		})
	}
	return runQuietLabel(label, "systemctl", args...)
}

var systemctlOutput = func(label string, args ...string) (string, error) {
	return outputQuietLabel(label, "systemctl", args...)
}

func (a *App) installXrayService() error {
	if err := a.prepareXrayServiceRuntime(); err != nil {
		return err
	}
	unit := a.xrayUnitContent()
	unitPath := "/etc/systemd/system/" + a.cfg.SystemdService
	legacyExec := "ExecStart=" + systemdQuote(a.cfg.XrayBin()) + " run -config " + systemdQuote(a.cfg.XrayConfig())
	if err := validateUnitReplacementOwnership(unitPath, legacyExec); err != nil {
		return err
	}
	if err := writeFileAtomic(unitPath, unit, 0o644); err != nil {
		return err
	}
	return systemctlRun("重新加载 systemd 配置", "daemon-reload")
}

func (a *App) xrayUnitContent() []byte {
	// 默认端口(789x)不需要任何 capability，清空 bounding set；仅在操作员显式配置
	// <1024 的监听端口时才授予绑定特权端口所需的 CAP_NET_BIND_SERVICE。
	capLines := "CapabilityBoundingSet="
	if a.cfg.needsPrivilegedPortCap() {
		capLines = "CapabilityBoundingSet=CAP_NET_BIND_SERVICE\nAmbientCapabilities=CAP_NET_BIND_SERVICE"
	}
	return []byte(fmt.Sprintf(managedSystemdUnitHeader+`[Unit]
Description=Xray 代理主服务
After=network-online.target nss-lookup.target
Wants=network-online.target

[Service]
Type=simple
User=%s
WorkingDirectory=%s
ExecStart=%s run -config %s
Restart=on-failure
RestartSec=5s
LimitNOFILE=1048576
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=%s
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
RestrictRealtime=true
LockPersonality=true
SystemCallArchitectures=native
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
%s

[Install]
WantedBy=multi-user.target
`, a.cfg.XrayServiceUser, systemdPath(a.cfg.CoreDir), systemdQuote(a.cfg.XrayBin()), systemdQuote(a.cfg.XrayConfig()), systemdPath(a.cfg.CoreDir), capLines))
}

func (a *App) prepareXrayServiceRuntime() error {
	identity, err := a.ensureXrayServiceUser()
	if err != nil {
		return err
	}
	if a.cfg.XrayServiceUser == "root" {
		return nil
	}
	// 非 root 服务用户却以 root(GID 0) 为主组时，无法通过"组可读"安全地授予配置访问；
	// 直接报错而不是静默跳过锁定（那会让服务读不到自己的配置）。
	if identity.GID == 0 {
		return fmt.Errorf("服务用户 %s 的主组为 root(GID 0)，无法安全授予配置读取权限，请为其分配独立用户组", a.cfg.XrayServiceUser)
	}
	if err := os.Chown(a.cfg.CoreDir, 0, identity.GID); err != nil {
		return err
	}
	if err := os.Chmod(a.cfg.CoreDir, 0o750); err != nil {
		return err
	}
	files := []struct {
		path     string
		mode     os.FileMode
		required bool
	}{
		{path: a.cfg.XrayBin(), mode: 0o750, required: true},
		{path: a.cfg.XrayConfig(), mode: 0o640},
	}
	for _, file := range files {
		if err := chownRootGroupMode(file.path, identity.GID, file.mode, file.required); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) ensureXrayServiceUser() (localUserIdentity, error) {
	if err := validateUserName(a.cfg.XrayServiceUser); err != nil {
		return localUserIdentity{}, err
	}
	if a.cfg.XrayServiceUser == "root" {
		return lookupLocalUserIdentity("root")
	}
	if runQuiet("id", "-u", a.cfg.XrayServiceUser) != nil {
		shell := "/usr/sbin/nologin"
		if !fileExists(shell) {
			shell = "/sbin/nologin"
		}
		if !fileExists(shell) {
			shell = "/bin/false"
		}
		if err := runQuietLabel("创建 Xray 服务用户", "useradd", "--system", "--user-group", "--no-create-home", "--home-dir", "/nonexistent", "--shell", shell, a.cfg.XrayServiceUser); err != nil {
			return localUserIdentity{}, err
		}
	}
	return lookupLocalUserIdentity(a.cfg.XrayServiceUser)
}

func systemdPath(path string) string {
	return strings.ReplaceAll(path, "%", "%%")
}

func chownRootGroupMode(path string, gid int, mode os.FileMode, required bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) && !required {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("该 Xray 服务文件不能是符号链接：%s", path)
	}
	if err := os.Chown(path, 0, gid); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func (a *App) preflightRestoreService() error {
	if err := validatePrivilegedExecutable(a.cfg.InstallBin, "PROXYSCENE_SWITCH_BIN"); err != nil {
		return err
	}
	unitPath := "/etc/systemd/system/" + a.cfg.RestoreService
	legacyExec := "ExecStart=" + systemdQuote(a.cfg.InstallBin) + " boot-restore"
	return validateUnitReplacementOwnership(unitPath, legacyExec)
}

func (a *App) installRestoreService() error {
	if err := a.preflightRestoreService(); err != nil {
		return err
	}
	envLines := restoreServiceEnvironmentLines(a.cfg)
	unit := fmt.Sprintf(managedSystemdUnitHeader+`[Unit]
Description=恢复已启用的 Xray 代理场景
After=network-online.target %s
Wants=network-online.target

[Service]
Type=oneshot
%s
ExecStart=%s boot-restore
RemainAfterExit=no

[Install]
WantedBy=multi-user.target
`, a.cfg.SystemdService, envLines, systemdQuote(a.cfg.InstallBin))
	unitPath := "/etc/systemd/system/" + a.cfg.RestoreService
	if err := writeFileAtomic(unitPath, []byte(unit), 0o644); err != nil {
		return err
	}
	if err := systemctlRun("重新加载 systemd 配置", "daemon-reload"); err != nil {
		return err
	}
	return systemctlRun("启用开机恢复服务", "enable", "--", a.cfg.RestoreService)
}

func validateUnitReplacementOwnership(path, legacyExec string) error {
	data, err := readRegularFileNoFollow(path, 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取现有 systemd unit 失败：%s：%w", path, err)
	}
	if bytes.HasPrefix(data, []byte(managedSystemdUnitHeader)) {
		return nil
	}
	if bytes.Contains(data, []byte("\n"+legacyExec+"\n")) {
		return nil
	}
	return fmt.Errorf("拒绝覆盖没有 proxyscene ownership marker 的 systemd unit：%s", path)
}

func validateManagedUnitForRemoval(path string) error {
	data, err := readRegularFileNoFollow(path, 1<<20)
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(data, []byte(managedSystemdUnitHeader)) {
		return fmt.Errorf("拒绝删除没有 proxyscene ownership marker 的 systemd unit：%s", path)
	}
	return nil
}

func restoreServiceEnvironmentLines(cfg Config) string {
	values := []struct {
		key   string
		value string
	}{
		{"PROXYSCENE_MANAGER_DIR", cfg.CoreDir},
		{"PROXYSCENE_SWITCH_BIN", cfg.InstallBin},
		{"PROXYSCENE_SYSTEMD_SERVICE_NAME", cfg.SystemdService},
		{"PROXYSCENE_BOOT_RESTORE_SERVICE_NAME", cfg.RestoreService},
	}
	lines := make([]string, 0, len(values))
	for _, value := range values {
		lines = append(lines, "Environment="+systemdQuote(value.key+"="+value.value))
	}
	return strings.Join(lines, "\n")
}

func (a *App) restartXrayService() error {
	// systemd 的启动频率计数也包含成功的显式 restart。升级、场景切换和
	// boot-restore 可能在短窗口内连续协调同一 unit；在应用已通过 Xray
	// 校验的配置前重置该 unit 的失败/启动计数，避免合法的管理操作被限流。
	if err := systemctlRun("重置 Xray 主服务失败状态", "reset-failed", "--", a.cfg.SystemdService); err != nil {
		return err
	}
	if err := systemctlRun("重启 Xray 主服务", "restart", "--", a.cfg.SystemdService); err != nil {
		return err
	}
	state, err := systemctlOutput("确认 Xray 主服务已启动", "show", "--property=ActiveState", "--value", "--", a.cfg.SystemdService)
	if err != nil {
		return err
	}
	if state = strings.TrimSpace(state); state != "active" {
		return fmt.Errorf("检测到 Xray 主服务重启后处于 %q 状态，而不是 active", state)
	}
	return nil
}

func (a *App) stopXrayService() error {
	stopErr := systemctlRun("停止 Xray 主服务", "stop", "--", a.cfg.SystemdService)
	disableErr := systemctlRun("禁用 Xray 主服务", "disable", "--", a.cfg.SystemdService)
	state, stateErr := systemctlOutput("确认 Xray 主服务已停止", "show", "--property=ActiveState", "--value", "--", a.cfg.SystemdService)
	if stateErr == nil {
		switch strings.TrimSpace(state) {
		case "inactive", "failed":
		default:
			stateErr = fmt.Errorf("检测到 Xray 主服务停止后仍处于 %q 状态", strings.TrimSpace(state))
		}
	}
	return errors.Join(stopErr, disableErr, stateErr)
}
