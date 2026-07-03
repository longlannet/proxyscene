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
			if scene == SceneTelegram {
				// 只发现一次目标：应用与持久化使用同一份列表，避免两次发现结果不一致。
				targets, err := a.telegramTargets(st, false)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s目标解析失败：%w", sceneName(scene), err))
					continue
				}
				if err := a.applyTelegram(targets); err != nil {
					errs = append(errs, fmt.Errorf("%s应用失败：%w", sceneName(scene), err))
					continue
				}
				st.TelegramTargets = canonicalTelegramTargetNames(targets)
			} else if err := a.applyScene(scene); err != nil {
				errs = append(errs, fmt.Errorf("%s应用失败：%w", sceneName(scene), err))
				continue
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
		// 电报场景复用上面已发现的 telegramTargets，保证应用与持久化的目标一致。
		var applyErr error
		if scene == SceneTelegram {
			applyErr = a.applyTelegram(telegramTargets)
		} else {
			applyErr = a.applyScene(scene)
		}
		if applyErr != nil {
			a.rollbackSceneState(st, scene, old)
			return applyErr
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
		// 系统侧改动已生效但状态未能持久化：回滚系统侧（恢复 SceneEnabled、重新
		// 应用/恢复场景并重新同步核心服务），使磁盘状态与实际系统保持一致，避免
		// status 误报、boot-restore 重复应用已被拆除的场景。
		a.rollbackSceneState(st, scene, old)
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
		return a.applyTelegram(nil)
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

// applyTelegram 应用电报场景。targets 为 nil 时自行发现（applyScene/回滚路径）；
// setSceneWithStore/applySavedScenes 会把已发现的目标传入，避免同一次操作里发现两次
// （两次自动发现结果可能不一致，持久化的应与实际应用的一致）。
func (a *App) applyTelegram(targets []systemdTargetName) error {
	if targets == nil {
		var err error
		targets, err = a.telegramTargets(nil, false)
		if err != nil {
			return err
		}
	}
	if len(targets) == 0 {
		return fmt.Errorf("没有可注入的 OpenClaw/Hermes systemd 目标服务")
	}
	// 系统级 user-unit 目录里的 Telegram 客户端无法确定作用用户，提示操作员显式指定。
	warnSystemWideUserTelegramUnits()
	content := telegramProxyEnvContent(a.cfg)
	envChanged, err := writeFileAtomicIfChanged("/etc/openclaw-hermes-tg-proxy.env", []byte(content), 0o600)
	if err != nil {
		return err
	}
	// EnvironmentFile 带 `-` 前缀：env 文件缺失时单元仍能启动。restoreTelegram 先删 env
	// 文件再删各 drop-in，中间窗口（或清理中途失败残留 drop-in）不能把目标服务打到起不来。
	dropIn := "[Service]\nEnvironmentFile=-/etc/openclaw-hermes-tg-proxy.env\n"
	proxyURL := a.cfg.HTTPAddr(SceneTelegram)
	manageOpenClaw := envBool("PROXYSCENE_MANAGE_OPENCLAW_CONFIG", true)
	userManagers := map[string]bool{}
	warnedBus := map[string]bool{}
	restart := []systemdTargetName{} // 只重启确有改动的目标
	for _, target := range targets {
		// 用户级目标需要其 systemd 用户总线在运行（linger/已登录）才能注入/重启生效，先做可见提示。
		if target.UserMode && !warnedBus[target.User] {
			warnIfUserBusMissing(target.User)
			warnedBus[target.User] = true
		}
		// OpenClaw 不读 TELEGRAM_*，env 注入对它无效；改其 channels.telegram.proxy 配置实现仅代理 Telegram。
		if a.isOpenClawTarget(target) {
			if !manageOpenClaw {
				continue
			}
			if target.UserMode {
				changed, err := a.applyOpenClawTelegramProxy(target.User, proxyURL)
				if err != nil {
					return err
				}
				if changed {
					restart = append(restart, target)
				}
			} else if systemUnitExists(target.Service) {
				fmt.Printf("警告：系统级 openclaw 单元 %s 无法确定配置归属用户，已跳过，请手动设置 channels.telegram.proxy=%s\n", target.Service, proxyURL)
			}
			continue
		}
		if target.UserMode {
			path, err := a.cfg.UserTelegramDropInPath(target.User, target.Service)
			if err != nil {
				return err
			}
			userDropIn := "[Service]\n" + telegramProxySystemdEnvironmentLines(a.cfg)
			changed, err := writeUserFileAtomicIfChanged(target.User, path, []byte(userDropIn), 0o644)
			if err != nil {
				return err
			}
			userManagers[target.User] = true
			if changed {
				restart = append(restart, target)
			}
			continue
		}
		// 系统级 hermes：单元不存在时跳过，避免为配置里锚定但未安装的服务（默认含
		// hermes-gateway）留下 phantom drop-in；安装后重开场景即可注入。
		if !systemUnitExists(target.Service) {
			fmt.Printf("提示：系统级目标 %s 未安装，已跳过注入；安装后请重新执行 proxyscene tg on\n", target.Service)
			continue
		}
		// drop-in 内容固定（EnvironmentFile=），仅在 drop-in 新建或共享 env 文件
		// 内容变化时才需重启。
		changed, err := writeFileAtomicIfChanged(a.cfg.TelegramDropInPath(target.Service), []byte(dropIn), 0o644)
		if err != nil {
			return err
		}
		if changed || envChanged {
			restart = append(restart, target)
		}
	}
	if err := runQuietLabel("重新加载 systemd 配置", "systemctl", "daemon-reload"); err != nil {
		return err
	}
	for userName := range userManagers {
		runUserSystemctlWarn(userName, "重新加载用户级 systemd 配置", "daemon-reload")
	}
	a.restartTelegramTargets(restart)
	return nil
}

// restartTelegramTargets 只重启确有改动（drop-in 内容变化或 openclaw 配置变化）的目标，
// 避免在内容未变时无谓地把网关弹一次。
func (a *App) restartTelegramTargets(restart []systemdTargetName) {
	for _, target := range restart {
		if target.UserMode {
			runUserSystemctlWarn(target.User, "重启用户级服务 "+target.Service, "try-restart", "--", target.Service)
			continue
		}
		_ = runQuiet("systemctl", "try-restart", "--", target.Service)
	}
}

// warnSystemWideUserTelegramUnits 扫描系统级 user-unit 目录（这些单元对所有用户的
// systemctl --user 生效），若发现疑似 Telegram 客户端单元，提示无法自动判定作用用户、
// 请用 PROXYSCENE_TG_SERVICES 显式指定 user:用户名:服务名。
func warnSystemWideUserTelegramUnits() {
	for _, root := range []string{"/etc/systemd/user", "/usr/local/lib/systemd/user", "/lib/systemd/user", "/usr/lib/systemd/user"} {
		walkTelegramUnitFiles(root, func(path, service string) {
			if !isTelegramRelatedUnit(path, service) {
				return
			}
			fmt.Printf("提示：发现系统级用户单元 %s（%s），它对所有用户的 systemctl --user 生效，无法自动判定作用用户，已跳过；如需接管请显式设置 PROXYSCENE_TG_SERVICES='user:用户名:%s'\n", service, root, service)
		})
	}
}

func telegramProxyEnvContent(cfg Config) string {
	return fmt.Sprintf("# 由 proxyscene 管理\n%s", telegramProxyEnvironmentLines(cfg))
}

// telegramProxyEnvPairs 返回注入目标服务的 Telegram 专用代理环境变量。
// 只注入 TELEGRAM_PROXY 这一个：它是 Hermes 实际消费的变量（其 config 描述为
// "Proxy URL for Telegram connections (overrides HTTPS_PROXY)，支持 http/https/socks5"）。
// 此前一并注入的 TELEGRAM_HTTP_PROXY / TELEGRAM_HTTPS_PROXY / TELEGRAM_SOCKS_PROXY 没有任何
// 已知消费方，属冗余，已移除。绝不注入 HTTP_PROXY/ALL_PROXY 等宽口径变量——本场景只代理
// Telegram，注入通用代理会把目标服务的全部出网都导流（OpenClaw 即只认通用变量，因此
// 无法用环境变量做到「仅代理 Telegram」，需改用其 channels.telegram.proxy 配置项）。
func telegramProxyEnvPairs(cfg Config) []string {
	return []string{
		"TELEGRAM_PROXY=" + cfg.HTTPAddr(SceneTelegram),
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
	proxyURL := a.cfg.HTTPAddr(SceneTelegram)
	userManagers := map[string]bool{}
	restart := []systemdTargetName{} // 只重启确有清理（drop-in 删除 / openclaw 配置还原）的目标
	for _, target := range targets {
		if a.isOpenClawTarget(target) {
			// 还原 openclaw 配置（仅当当前值仍是我们设置的托管值），并顺带清理旧版本可能写入的无效 drop-in。
			if target.UserMode {
				changed, err := a.restoreOpenClawTelegramProxy(target.User, proxyURL)
				if err != nil {
					fmt.Printf("警告：还原用户 %s 的 openclaw Telegram 代理配置失败：%v\n", target.User, err)
				}
				if path, err := a.cfg.UserTelegramDropInPath(target.User, target.Service); err == nil {
					if removeFileReport(path) {
						// 真的清掉了旧版遗留 drop-in 时，同样需要 reload 该用户的 user systemd 才生效。
						userManagers[target.User] = true
					}
					_ = os.Remove(filepath.Dir(path))
				}
				if changed {
					restart = append(restart, target)
				}
			}
			continue
		}
		if target.UserMode {
			path, err := a.cfg.UserTelegramDropInPath(target.User, target.Service)
			if err == nil {
				removed := removeFileReport(path)
				// 删除清空后的 .service.d 目录；目录非空（有其他 drop-in）时 Remove 失败，忽略。
				_ = os.Remove(filepath.Dir(path))
				userManagers[target.User] = true
				if removed {
					restart = append(restart, target)
				}
			}
			continue
		}
		removed := removeFileReport(a.cfg.TelegramDropInPath(target.Service))
		_ = os.Remove(a.cfg.TelegramDropInDir(target.Service))
		if removed {
			restart = append(restart, target)
		}
	}
	_ = runQuietLabel("重新加载 systemd 配置", "systemctl", "daemon-reload")
	for userName := range userManagers {
		runUserSystemctlWarn(userName, "重新加载用户级 systemd 配置", "daemon-reload")
	}
	a.restartTelegramTargets(restart)
	return nil
}
