package manager

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// applySavedScenes 按状态文件逐场景应用或恢复代理设置。这里采用尽力而为策略：
// 单个场景失败不会中断其余场景，所有错误聚合后一并返回。这样可避免例如开发代理
// 目标用户配置错误时，连带阻塞全局/电报代理在开机恢复时的应用。
func (a *App) applySavedScenes(st *Store) error {
	var errs []error
	for _, scene := range []Scene{SceneGlobal, SceneDev, SceneTelegram} {
		if st.SceneEnabled[scene] {
			if err := a.applyScene(scene); err != nil {
				errs = append(errs, fmt.Errorf("%s应用失败：%w", sceneName(scene), err))
				continue
			}
			if scene == SceneTelegram {
				targets, err := a.telegramTargets(st, false)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s目标解析失败：%w", sceneName(scene), err))
					continue
				}
				st.TelegramTargets = canonicalTelegramTargetNames(targets)
			}
		} else {
			if err := a.restoreScene(scene); err != nil {
				errs = append(errs, fmt.Errorf("%s恢复失败：%w", sceneName(scene), err))
				continue
			}
			if scene == SceneTelegram {
				st.TelegramTargets = nil
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
	old := st.SceneEnabled[scene]
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
			a.rollbackSceneState(st, scene, old)
			return err
		}
		if err := a.applyScene(scene); err != nil {
			a.rollbackSceneState(st, scene, old)
			return err
		}
		if scene == SceneTelegram {
			st.TelegramTargets = canonicalTelegramTargetNames(telegramTargets)
		}
	} else {
		if err := a.restoreScene(scene); err != nil {
			if old {
				_ = a.applyScene(scene)
			}
			return err
		}
		st.SceneEnabled[scene] = false
		if scene == SceneTelegram {
			st.TelegramTargets = nil
		}
		if err := a.syncXrayServiceForStore(st); err != nil {
			a.rollbackSceneState(st, scene, old)
			return err
		}
	}
	if err := a.saveStore(st); err != nil {
		return err
	}
	fmt.Printf("%s：%s\n", sceneName(scene), onOff(enabled))
	if scene == SceneGlobal && enabled {
		fmt.Println("提示：当前已打开的 shell 不会自动继承新的代理环境变量。")
		fmt.Println("如需当前 shell 立即生效，请执行：source /etc/profile.d/proxyscene-global-proxy.sh")
	}
	return nil
}

func (a *App) syncXrayServiceForStore(st *Store) error {
	if !hasEnabledScene(st) {
		return a.stopXrayService()
	}
	if err := a.writeCheckedXrayConfig(st); err != nil {
		return err
	}
	return a.startXrayService()
}

func (a *App) rollbackSceneState(st *Store, scene Scene, old bool) {
	st.SceneEnabled[scene] = old
	if old {
		_ = a.applyScene(scene)
	} else {
		_ = a.restoreScene(scene)
	}
	if err := a.syncXrayServiceForStore(st); err != nil {
		fmt.Printf("警告：场景回滚后同步核心服务失败，服务可能处于降级状态：%v\n", err)
	}
}

func (a *App) stopXrayIfIdle(st *Store) error {
	if hasEnabledScene(st) {
		return nil
	}
	return a.stopXrayService()
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

func (a *App) applyScene(scene Scene) error {
	switch scene {
	case SceneGlobal:
		return a.applyGlobal()
	case SceneDev:
		return a.applyDev()
	case SceneTelegram:
		return a.applyTelegram()
	default:
		return fmt.Errorf("未知场景：%s", scene)
	}
}

func (a *App) restoreScene(scene Scene) error {
	switch scene {
	case SceneGlobal:
		_ = os.Remove("/etc/profile.d/proxyscene-global-proxy.sh")
		_ = os.Remove("/etc/apt/apt.conf.d/99proxyscene-global-proxy")
	case SceneDev:
		return a.restoreDev()
	case SceneTelegram:
		return a.restoreTelegram()
	}
	return nil
}

func (a *App) applyGlobal() error {
	// 这里用 %q 给 shell / apt 配置值加引号。代理地址由 ProxyHost（validateProxyHost
	// 限定为 IP 或 [A-Za-z0-9.-]）和已校验端口拼成，不含 shell 元字符，因此 %q 的
	// Go 双引号语义在此等价于安全引用；如果未来放宽 ProxyHost 字符集，需改用严格的
	// shell 引用。
	content := fmt.Sprintf("# 由 proxyscene 管理\nexport http_proxy=%q\nexport https_proxy=%q\nexport all_proxy=%q\nexport HTTP_PROXY=%q\nexport HTTPS_PROXY=%q\nexport ALL_PROXY=%q\n", a.cfg.HTTPAddr(SceneGlobal), a.cfg.HTTPAddr(SceneGlobal), a.cfg.GlobalSocksAddr(), a.cfg.HTTPAddr(SceneGlobal), a.cfg.HTTPAddr(SceneGlobal), a.cfg.GlobalSocksAddr())
	if err := writeFileAtomic("/etc/profile.d/proxyscene-global-proxy.sh", []byte(content), 0o644); err != nil {
		return err
	}
	apt := fmt.Sprintf("// 由 proxyscene 管理\nAcquire::http::Proxy %q;\nAcquire::https::Proxy %q;\n", a.cfg.HTTPAddr(SceneGlobal), a.cfg.HTTPAddr(SceneGlobal))
	if err := writeFileAtomic("/etc/apt/apt.conf.d/99proxyscene-global-proxy", []byte(apt), 0o644); err != nil {
		return err
	}
	return nil
}

func (a *App) applyDev() error {
	user, err := a.devTargetUser()
	if err != nil {
		return err
	}
	if err := a.backupDevConfig(user); err != nil {
		return err
	}
	proxy := a.cfg.HTTPAddr(SceneDev)
	applied := false
	if commandExists("git") {
		if err := runAsUser(user, "git", "config", "--global", "http.proxy", proxy); err != nil {
			return err
		}
		if err := runAsUser(user, "git", "config", "--global", "https.proxy", proxy); err != nil {
			return err
		}
		applied = true
	} else {
		fmt.Println("提示：未找到 git，跳过 git 代理设置")
	}
	if commandExists("npm") {
		if err := runAsUser(user, "npm", "config", "set", "proxy", proxy); err != nil {
			return err
		}
		if err := runAsUser(user, "npm", "config", "set", "https-proxy", proxy); err != nil {
			return err
		}
		applied = true
	} else {
		fmt.Println("提示：未找到 npm，跳过 npm 代理设置")
	}
	if !applied {
		return fmt.Errorf("开发代理需要 git 或 npm，但当前都不可用")
	}
	return nil
}

func (a *App) applyTelegram() error {
	targets, err := a.telegramTargets(nil, false)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return fmt.Errorf("没有可注入的 OpenClaw/Hermes systemd 目标服务")
	}
	content := telegramProxyEnvContent(a.cfg)
	if err := writeFileAtomic("/etc/openclaw-hermes-tg-proxy.env", []byte(content), 0o600); err != nil {
		return err
	}
	dropIn := "[Service]\nEnvironmentFile=/etc/openclaw-hermes-tg-proxy.env\n"
	userManagers := map[string]bool{}
	for _, target := range targets {
		if target.UserMode {
			path, err := a.cfg.UserTelegramDropInPath(target.User, target.Service)
			if err != nil {
				return err
			}
			userDropIn := "[Service]\n" + telegramProxySystemdEnvironmentLines(a.cfg)
			if err := writeUserFileAtomic(target.User, path, []byte(userDropIn), 0o644); err != nil {
				return err
			}
			userManagers[target.User] = true
			continue
		}
		if err := writeFileAtomic(a.cfg.TelegramDropInPath(target.Service), []byte(dropIn), 0o644); err != nil {
			return err
		}
	}
	if err := runQuietLabel("重新加载 systemd 配置", "systemctl", "daemon-reload"); err != nil {
		return err
	}
	for userName := range userManagers {
		runUserSystemctlWarn(userName, "重新加载用户级 systemd 配置", "daemon-reload")
	}
	for _, target := range targets {
		if target.UserMode {
			runUserSystemctlWarn(target.User, "重启用户级服务 "+target.Service, "try-restart", "--", target.Service)
			continue
		}
		_ = runQuiet("systemctl", "try-restart", "--", target.Service)
	}
	return nil
}

func telegramProxyEnvContent(cfg Config) string {
	return fmt.Sprintf("# 由 proxyscene 管理\n%s", telegramProxyEnvironmentLines(cfg))
}

func telegramProxyEnvPairs(cfg Config) []string {
	httpProxy := cfg.HTTPAddr(SceneTelegram)
	socksProxy := cfg.TGSocksAddr()
	return []string{
		"TELEGRAM_PROXY=" + httpProxy,
		"TELEGRAM_HTTP_PROXY=" + httpProxy,
		"TELEGRAM_HTTPS_PROXY=" + httpProxy,
		"TELEGRAM_SOCKS_PROXY=" + socksProxy,
	}
}

func telegramProxyEnvironmentLines(cfg Config) string {
	return strings.Join(telegramProxyEnvPairs(cfg), "\n") + "\n"
}

func telegramProxySystemdEnvironmentLines(cfg Config) string {
	lines := make([]string, 0, len(telegramProxyEnvPairs(cfg)))
	for _, pair := range telegramProxyEnvPairs(cfg) {
		lines = append(lines, "Environment="+systemdQuote(pair))
	}
	return strings.Join(lines, "\n") + "\n"
}

func (a *App) restoreTelegram() error {
	_ = os.Remove("/etc/openclaw-hermes-tg-proxy.env")
	st, err := a.loadStore()
	if err != nil {
		fmt.Printf("警告：读取电报服务代理状态失败，将按默认和自动发现目标清理：%v\n", err)
		st = nil
	}
	targets, targetErrs := a.telegramTargetsBestEffort(st, true)
	for _, err := range targetErrs {
		fmt.Printf("警告：跳过无效的电报服务代理清理目标：%v\n", err)
	}
	userManagers := map[string]bool{}
	for _, target := range targets {
		if target.UserMode {
			path, err := a.cfg.UserTelegramDropInPath(target.User, target.Service)
			if err == nil {
				_ = os.Remove(path)
				// 删除清空后的 .service.d 目录；目录非空（有其他 drop-in）时 Remove 失败，忽略。
				_ = os.Remove(filepath.Dir(path))
			}
			userManagers[target.User] = true
			continue
		}
		_ = os.Remove(a.cfg.TelegramDropInPath(target.Service))
		_ = os.Remove(a.cfg.TelegramDropInDir(target.Service))
	}
	_ = runQuietLabel("重新加载 systemd 配置", "systemctl", "daemon-reload")
	for userName := range userManagers {
		runUserSystemctlWarn(userName, "重新加载用户级 systemd 配置", "daemon-reload")
	}
	for _, target := range targets {
		if target.UserMode {
			runUserSystemctlWarn(target.User, "重启用户级服务 "+target.Service, "try-restart", "--", target.Service)
			continue
		}
		_ = runQuiet("systemctl", "try-restart", "--", target.Service)
	}
	return nil
}
