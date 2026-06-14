package manager

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (a *App) installXrayService() error {
	if err := a.prepareXrayServiceRuntime(); err != nil {
		return err
	}
	unit := fmt.Sprintf(`[Unit]
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
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
`, a.cfg.XrayServiceUser, systemdPath(a.cfg.CoreDir), systemdQuote(a.cfg.XrayBin()), systemdQuote(a.cfg.XrayConfig()), systemdPath(a.cfg.CoreDir))
	if err := writeFileAtomic("/etc/systemd/system/"+a.cfg.SystemdService, []byte(unit), 0o644); err != nil {
		return err
	}
	return runQuietLabel("重新加载 systemd 配置", "systemctl", "daemon-reload")
}

func (a *App) prepareXrayServiceRuntime() error {
	identity, err := a.ensureXrayServiceUser()
	if err != nil {
		return err
	}
	if a.cfg.XrayServiceUser == "root" || identity.GID == 0 {
		return nil
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
		{path: filepath.Join(a.cfg.CoreDir, "geoip.dat"), mode: 0o640},
		{path: filepath.Join(a.cfg.CoreDir, "geosite.dat"), mode: 0o640},
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
		return fmt.Errorf("Xray 服务文件不能是符号链接：%s", path)
	}
	if err := os.Chown(path, 0, gid); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func (a *App) installRestoreService() error {
	unit := fmt.Sprintf(`[Unit]
Description=恢复已启用的 Xray 代理场景
After=network-online.target %s
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=%s boot-restore
RemainAfterExit=no

[Install]
WantedBy=multi-user.target
`, a.cfg.SystemdService, systemdQuote(a.cfg.InstallBin))
	if err := writeFileAtomic("/etc/systemd/system/"+a.cfg.RestoreService, []byte(unit), 0o644); err != nil {
		return err
	}
	if err := runQuietLabel("重新加载 systemd 配置", "systemctl", "daemon-reload"); err != nil {
		return err
	}
	return runQuietLabel("启用开机恢复服务", "systemctl", "enable", a.cfg.RestoreService)
}

func (a *App) startXrayService() error {
	if err := a.installXrayService(); err != nil {
		return err
	}
	if err := runQuietLabel("启用 Xray 主服务", "systemctl", "enable", a.cfg.SystemdService); err != nil {
		return err
	}
	return runQuietLabel("重启 Xray 主服务", "systemctl", "restart", a.cfg.SystemdService)
}
func (a *App) stopXrayService() error {
	_ = runQuiet("systemctl", "stop", a.cfg.SystemdService)
	return runQuietLabel("禁用 Xray 主服务", "systemctl", "disable", a.cfg.SystemdService)
}
