package manager

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

type App struct {
	cfg Config
}

type installStoreCommit func(*Store, func(*Store) error, storeRuntimeSyncMode) error

func NewApp(cfg Config) *App { return &App{cfg: cfg} }

func (a *App) Run(args []string) error {
	if err := a.cfg.ValidateLocators(); err != nil {
		return err
	}
	if len(args) == 0 {
		return a.menu()
	}
	switch args[0] {
	case "install", "setup", "init":
		raw, promptNode, err := parseInstallArgs(args[1:])
		if err != nil {
			return err
		}
		return a.install(raw, promptNode)
	case "status":
		return a.status()
	case "global", "dev", "tg", "telegram":
		return a.sceneCommand(args)
	case "node", "nodes":
		return a.nodeCommand(args[1:])
	case "test":
		return a.testProxy()
	case "boot-restore":
		return a.bootRestore()
	case "uninstall":
		return a.uninstall()
	case "help", "-h", "--help":
		a.help()
		return nil
	case "version", "--version", "-v":
		fmt.Printf("proxyscene %s\n", VersionString())
		return nil
	default:
		return fmt.Errorf("未知命令：%s", args[0])
	}
}

func (a *App) help() {
	fmt.Println("用法：")
	fmt.Println("  proxyscene install                  初始化/更新管理服务，并交互录入节点")
	fmt.Println("  proxyscene install --skip-node      初始化/更新管理服务，不交互录入节点")
	fmt.Println("  proxyscene                         打开交互菜单")
	fmt.Println("  proxyscene global|dev|tg           切换场景开关")
	fmt.Println("  proxyscene global on|off           显式开启/关闭全局代理")
	fmt.Println("  proxyscene node                    打开节点管理菜单")
	fmt.Println("  proxyscene node list               查看节点")
	fmt.Println("  proxyscene node add --stdin [备注]  从标准输入读取节点链接")
	fmt.Println("  proxyscene node import --stdin      从标准输入读取订阅链接")
	fmt.Println("  proxyscene node rename '节点ID' '新备注'")
	fmt.Println("  proxyscene node remove '节点ID'      删除节点（别名：delete）")
	fmt.Println("  proxyscene node test               对所有节点做 TCP 连通性测速")
	fmt.Println("  proxyscene node auto [范围]         测速后自动选择最快节点；范围可为 默认(default)/全局(global)/开发(dev)/电报(telegram)/全部(all)")
	fmt.Println("  proxyscene node use '节点ID' [范围] 使用指定节点；范围可为 默认(default)/全局(global)/开发(dev)/电报(telegram)/全部(all)")
	fmt.Println("  proxyscene test                    通过全局代理测试连通性")
	fmt.Println("  proxyscene status                  查看状态")
	fmt.Println("  proxyscene version                 查看版本")
	fmt.Println("  proxyscene uninstall               卸载 systemd 服务（保留数据目录）")
}

func (a *App) menu() error {
	for {
		st, err := a.loadStore()
		fmt.Println()
		fmt.Println("==============================")
		fmt.Printf(" Xray 代理管理器 v%s\n", Version)
		fmt.Println("==============================")
		if err != nil {
			fmt.Println("状态读取失败：", err)
		} else {
			a.printSceneStatus(st)
		}
		fmt.Println()
		fmt.Println("1. 初始化/更新管理服务")
		fmt.Println("2. 切换全局代理")
		fmt.Println("3. 切换开发代理")
		fmt.Println("4. 切换电报服务代理")
		fmt.Println("5. 节点管理")
		fmt.Println("6. 测试代理")
		fmt.Println("7. 查看状态")
		fmt.Println("8. 卸载")
		fmt.Println("9. 退出")
		choice, ok := ask("请输入选项 [1-9]: ")
		if !ok {
			fmt.Println()
			return nil
		}
		switch choice {
		case "1":
			if err := a.install("", true); err != nil {
				fmt.Println(err)
			}
		case "2":
			if err := a.toggleScene(SceneGlobal); err != nil {
				fmt.Println(err)
			}
		case "3":
			if err := a.toggleScene(SceneDev); err != nil {
				fmt.Println(err)
			}
		case "4":
			if err := a.toggleScene(SceneTelegram); err != nil {
				fmt.Println(err)
			}
		case "5":
			if err := a.nodeMenu(); err != nil {
				fmt.Println(err)
			}
		case "6":
			if err := a.testProxy(); err != nil {
				fmt.Println(err)
			}
		case "7":
			if err := a.status(); err != nil {
				fmt.Println(err)
			}
		case "8":
			if err := a.uninstall(); err != nil {
				fmt.Println(err)
			}
		case "9":
			return nil
		}
	}
}

func parseInstallArgs(args []string) (raw string, promptNode bool, err error) {
	promptNode = true
	for _, arg := range args {
		switch arg {
		case "--skip-node", "--no-node":
			promptNode = false
		default:
			if strings.HasPrefix(arg, "-") {
				return "", false, fmt.Errorf("未知选项：%s（可用：--skip-node）", arg)
			}
			return "", false, fmt.Errorf("初始化命令不接受节点链接位置参数（避免凭据出现在进程 argv）；请交互录入，或使用 proxyscene node add --stdin")
		}
	}
	return raw, promptNode, nil
}

func (a *App) install(raw string, promptNode bool) error {
	if err := requireRoot(); err != nil {
		return err
	}
	if raw == "" && promptNode {
		raw, _ = ask("请输入节点链接（VLESS / VMess / Trojan / Shadowsocks，可留空跳过）: ")
	}
	return a.installPrepared(raw)
}

func (a *App) installPrepared(raw string) error {
	installerHandoff, err := installerLockHandoffRequested()
	if err != nil {
		return err
	}
	return a.withInstallLock(func() error {
		if err := a.ensureCoreDirs(); err != nil {
			return err
		}
		return a.withHostAndStoreLocksAfterInstallLock(func() error {
			if installerHandoff {
				if err := a.validateInstallationOwnership(); err != nil {
					return err
				}
			} else if err := a.ensureInstallationOwnership(); err != nil {
				return err
			}
			st, err := a.loadStore()
			if err != nil {
				return err
			}
			if err := a.installWithStore(st, raw, a.ensureXrayInstalled, a.installXrayService, a.installRestoreService, a.commitStoreMutation); err != nil {
				return err
			}
			fmt.Println("管理服务初始化/更新完成")
			return nil
		})
	})
}

func (a *App) installWithStore(st *Store, raw string, ensureXray, installMainUnit, installRestoreUnit func() error, commit installStoreCommit) error {
	if err := ensureXray(); err != nil {
		return err
	}
	// 先安装 unit，再执行可能停止/重启 Xray 的状态事务。干净系统首次带节点
	// 初始化时，候选同步因此不会对尚不存在的 unit 执行 stop/show。
	if err := installMainUnit(); err != nil {
		return err
	}
	if err := installRestoreUnit(); err != nil {
		return err
	}
	mode := storeRuntimeSyncXray
	if hasEnabledScene(st) || a.runtimeConfigDiffers(st) {
		mode = storeRuntimeSyncAll
	}
	return commit(st, func(candidate *Store) error {
		if strings.TrimSpace(raw) == "" {
			return nil
		}
		_, err := a.addNode(candidate, raw, "", "default")
		return err
	}, mode)
}

func (a *App) sceneCommand(args []string) error {
	scene := Scene(args[0])
	if scene == "tg" {
		scene = SceneTelegram
	}
	if len(args) == 1 || args[1] == "toggle" {
		return a.toggleScene(scene)
	}
	switch args[1] {
	case "on", "enable", "start":
		return a.setScene(scene, true)
	case "off", "disable", "stop":
		return a.setScene(scene, false)
	default:
		return fmt.Errorf("未知场景参数：%s", args[1])
	}
}

func (a *App) printSceneStatus(st *Store) {
	if st == nil {
		st = newStore()
	}
	fmt.Printf("全局代理：%s\n", onOff(st.SceneEnabled[SceneGlobal]))
	fmt.Printf("开发代理：%s\n", onOff(st.SceneEnabled[SceneDev]))
	fmt.Printf("电报代理配置开关：%s（连接状态见 status）\n", onOff(st.SceneEnabled[SceneTelegram]))
}

func onOff(v bool) string {
	if v {
		return "已开启"
	}
	return "已关闭"
}

func (a *App) status() error {
	st, err := a.loadStore()
	if err != nil {
		return err
	}
	a.printSceneStatus(st)
	a.printTelegramTargetStatus()
	fmt.Printf("核心目录：%s\n", a.cfg.CoreDir)
	fmt.Printf("Xray：%s\n", a.cfg.XrayBin())
	fmt.Printf("配置：%s\n", a.cfg.XrayConfig())
	fmt.Println("节点：")
	for _, n := range st.Nodes {
		fmt.Printf("  %s [%s] %s\n", n.ID, n.Protocol, n.Name)
	}
	return nil
}

func (a *App) bootRestore() error {
	if err := requireRoot(); err != nil {
		return err
	}
	return a.withStoreLock(func() error {
		st, err := a.loadStoreForBoot()
		if err != nil {
			return err
		}
		return a.bootRestoreWithStore(st, a.commitStoreMutation)
	})
}

func (a *App) bootRestoreWithStore(st *Store, commit installStoreCommit) error {
	if err := commit(st, func(*Store) error { return nil }, storeRuntimeSyncAll); err != nil {
		return fmt.Errorf("恢复场景未完全成功：%w", err)
	}
	return nil
}

func (a *App) uninstall() error {
	if err := requireRoot(); err != nil {
		return err
	}
	return a.withStoreLock(func() error {
		// 卸载必须按最后一次已提交的运行配置清理所有权；忽略本次进程中可能污染
		// 端口、用户或 Telegram 目标的 runtime 环境变量，仅保留 locator 环境。
		st, err := a.loadStoreForBoot()
		if err != nil {
			return err
		}
		return a.uninstallWithStore(st)
	})
}

// uninstallWithStore 在调用方持有 Store 锁期间完成整个卸载事务，避免各场景分别
// 加锁留下可被并发开启命令插入的窗口。
func (a *App) uninstallWithStore(st *Store) error {
	if err := disableScenesForUninstall(st, a.setSceneWithStore); err != nil {
		fmt.Println("警告：", err)
		return fmt.Errorf("场景清理失败，已保留 systemd unit 和核心服务：%w", err)
	}
	if err := removeSystemdUnitsForUninstall(a.cfg, disableAndRemoveSystemdUnit, func() error {
		return runQuietLabel("重新加载 systemd 配置", "systemctl", "daemon-reload")
	}); err != nil {
		fmt.Println("警告：", err)
		return fmt.Errorf("卸载 systemd unit 未完全成功：%w", err)
	}
	if err := a.releaseHostOwnership(); err != nil {
		return fmt.Errorf("卸载已清理共享资源，但释放主机 ownership 失败：%w", err)
	}
	fmt.Println("已卸载 systemd 服务，数据目录保留：", a.cfg.CoreDir)
	return nil
}

type uninstallSceneSetter func(*Store, Scene, bool) error

func disableScenesForUninstall(st *Store, setScene uninstallSceneSetter) error {
	for _, scene := range []Scene{SceneTelegram, SceneDev, SceneGlobal} {
		if err := setScene(st, scene, false); err != nil {
			return fmt.Errorf("关闭%s失败：%w", sceneName(scene), err)
		}
	}
	return nil
}

type systemdUnitRemover func(service, unitPath, label string) error

func removeSystemdUnitsForUninstall(cfg Config, remove systemdUnitRemover, reload func() error) error {
	// 先拆开机恢复 unit：只要它仍启用，下一次启动就可能重新创建并启动 Xray 主 unit。
	if err := remove(
		cfg.RestoreService,
		"/etc/systemd/system/"+cfg.RestoreService,
		"开机恢复服务",
	); err != nil {
		return err
	}
	mainErr := remove(
		cfg.SystemdService,
		"/etc/systemd/system/"+cfg.SystemdService,
		"Xray 主服务",
	)
	// restore unit 已经从磁盘删除，即使主 unit 拆除失败也要刷新 systemd 的缓存。
	return errors.Join(mainErr, reload())
}

func disableAndRemoveSystemdUnit(service, unitPath, label string) error {
	if err := safeProxysceneServiceName(service); err != nil {
		return err
	}
	_, statErr := os.Lstat(unitPath)
	if errors.Is(statErr, os.ErrNotExist) {
		loadState, activeState, err := querySystemdUnitState(service, label)
		if err != nil {
			return err
		}
		if loadState != "not-found" || (activeState != "inactive" && activeState != "failed") {
			// 上一次卸载可能已 unlink unit、却在 outer daemon-reload 前崩溃。此时
			// systemd 仍持有 loaded 缓存；主动停止/禁用并 reload 后再验证，保证重试可收敛。
			if err := systemctlRun("停止并禁用残留"+label, "disable", "--now", "--", service); err != nil {
				return fmt.Errorf("%s unit 文件已缺失且仍被 systemd 跟踪，清理失败：%w", label, err)
			}
			if err := systemctlRun("重新加载 systemd 配置", "daemon-reload"); err != nil {
				return err
			}
			loadState, activeState, err = querySystemdUnitState(service, label)
			if err != nil {
				return err
			}
			if loadState != "not-found" || (activeState != "inactive" && activeState != "failed") {
				return fmt.Errorf("清理%s残留后 systemd LoadState=%q ActiveState=%q", label, loadState, activeState)
			}
		}
		return nil
	}
	if statErr != nil {
		return fmt.Errorf("检查%s unit 失败：%w", label, statErr)
	}
	if err := validateManagedUnitForRemoval(unitPath); err != nil {
		return err
	}
	if err := systemctlRun("停止并禁用"+label, "disable", "--now", "--", service); err != nil {
		return fmt.Errorf("%w；为避免遗留已加载但磁盘不可追踪的配置，已保留 unit 文件 %s", err, unitPath)
	}
	if err := removeIfExists(unitPath); err != nil {
		return fmt.Errorf("删除%s unit 失败：%w", label, err)
	}
	return nil
}

func querySystemdUnitState(service, label string) (string, string, error) {
	state, err := systemctlOutput("确认"+label+"未加载", "show", "--property=LoadState", "--property=ActiveState", "--", service)
	if err != nil {
		return "", "", fmt.Errorf("%s unit 文件已缺失，但无法确认 systemd 加载状态：%w", label, err)
	}
	loadState, activeState, err := parseSystemdUnitState(state)
	if err != nil {
		return "", "", fmt.Errorf("%s unit 文件已缺失，但 systemd 状态无法确认：%w", label, err)
	}
	return loadState, activeState, nil
}

func parseSystemdUnitState(output string) (loadState, activeState string, err error) {
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "LoadState":
			loadState = strings.TrimSpace(value)
		case "ActiveState":
			activeState = strings.TrimSpace(value)
		}
	}
	if loadState == "" || activeState == "" {
		return "", "", fmt.Errorf("缺少 LoadState 或 ActiveState")
	}
	return loadState, activeState, nil
}
