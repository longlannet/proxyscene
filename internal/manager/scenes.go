package manager

import (
	"fmt"
	"os"
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

func (a *App) applySavedScenes(st *Store) error {
	for _, scene := range []Scene{SceneGlobal, SceneDev, SceneTelegram} {
		if st.SceneEnabled[scene] {
			if err := a.applyScene(scene); err != nil {
				return err
			}
			if scene == SceneTelegram {
				targets, err := a.telegramTargets(st, false)
				if err != nil {
					return err
				}
				st.TelegramTargets = canonicalTelegramTargetNames(targets)
			}
		} else {
			if err := a.restoreScene(scene); err != nil {
				return err
			}
			if scene == SceneTelegram {
				st.TelegramTargets = nil
			}
		}
	}
	return nil
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
		if err := a.startXrayService(); err != nil {
			a.rollbackSceneState(st, scene, old)
			return err
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
		fmt.Println("如需当前 shell 立即生效，请执行：source /etc/profile.d/xray-global-proxy.sh")
	}
	return nil
}

func (a *App) syncXrayServiceForStore(st *Store) error {
	if !hasEnabledScene(st) {
		return a.stopXrayService()
	}
	if err := a.writeXrayConfig(st); err != nil {
		return err
	}
	if err := a.checkXrayConfig(); err != nil {
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
	_ = a.syncXrayServiceForStore(st)
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
		_ = os.Remove("/etc/profile.d/xray-global-proxy.sh")
		_ = os.Remove("/etc/apt/apt.conf.d/99xray-global-proxy")
	case SceneDev:
		return a.restoreDev()
	case SceneTelegram:
		return a.restoreTelegram()
	}
	return nil
}

func (a *App) applyGlobal() error {
	content := fmt.Sprintf("# 由 xray-proxy-go 管理\nexport http_proxy=%q\nexport https_proxy=%q\nexport all_proxy=%q\nexport HTTP_PROXY=%q\nexport HTTPS_PROXY=%q\nexport ALL_PROXY=%q\n", a.cfg.HTTPAddr(SceneGlobal), a.cfg.HTTPAddr(SceneGlobal), a.cfg.GlobalSocksAddr(), a.cfg.HTTPAddr(SceneGlobal), a.cfg.HTTPAddr(SceneGlobal), a.cfg.GlobalSocksAddr())
	if err := writeFileAtomic("/etc/profile.d/xray-global-proxy.sh", []byte(content), 0o644); err != nil {
		return err
	}
	apt := fmt.Sprintf("// 由 xray-proxy-go 管理\nAcquire::http::Proxy %q;\nAcquire::https::Proxy %q;\n", a.cfg.HTTPAddr(SceneGlobal), a.cfg.HTTPAddr(SceneGlobal))
	if err := writeFileAtomic("/etc/apt/apt.conf.d/99xray-global-proxy", []byte(apt), 0o644); err != nil {
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
	if err := runAsUser(user, "git", "config", "--global", "http.proxy", proxy); err != nil {
		return err
	}
	if err := runAsUser(user, "git", "config", "--global", "https.proxy", proxy); err != nil {
		return err
	}
	if err := runAsUser(user, "npm", "config", "set", "proxy", proxy); err != nil {
		return err
	}
	return runAsUser(user, "npm", "config", "set", "https-proxy", proxy)
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
			runUserSystemctlWarn(target.User, "重启用户级服务 "+target.Service, "try-restart", target.Service)
			continue
		}
		_ = runQuiet("systemctl", "try-restart", target.Service)
	}
	return nil
}

func telegramProxyEnvContent(cfg Config) string {
	return fmt.Sprintf("# 由 xray-proxy-go 管理\n%s", telegramProxyEnvironmentLines(cfg))
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
			}
			userManagers[target.User] = true
			continue
		}
		_ = os.Remove(a.cfg.TelegramDropInPath(target.Service))
	}
	_ = runQuietLabel("重新加载 systemd 配置", "systemctl", "daemon-reload")
	for userName := range userManagers {
		runUserSystemctlWarn(userName, "重新加载用户级 systemd 配置", "daemon-reload")
	}
	for _, target := range targets {
		if target.UserMode {
			runUserSystemctlWarn(target.User, "重启用户级服务 "+target.Service, "try-restart", target.Service)
			continue
		}
		_ = runQuiet("systemctl", "try-restart", target.Service)
	}
	return nil
}
