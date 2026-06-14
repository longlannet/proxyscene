package manager

import (
	"fmt"
	"strings"
)

type App struct {
	cfg Config
}

func NewApp(cfg Config) *App { return &App{cfg: cfg} }

func (a *App) Run(args []string) error {
	if err := a.cfg.Validate(); err != nil {
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
	default:
		return fmt.Errorf("未知命令：%s", args[0])
	}
}

func (a *App) help() {
	fmt.Println("用法：")
	fmt.Println("  xray-proxy install '节点链接'       初始化/更新管理服务，可同时导入节点")
	fmt.Println("  xray-proxy install --skip-node      初始化/更新管理服务，不交互录入节点")
	fmt.Println("  xray-proxy                         打开交互菜单")
	fmt.Println("  xray-proxy global|dev|tg           切换场景开关")
	fmt.Println("  xray-proxy global on|off           显式开启/关闭全局代理")
	fmt.Println("  xray-proxy node                    打开节点管理菜单")
	fmt.Println("  xray-proxy node list               查看节点")
	fmt.Println("  xray-proxy node add '节点链接' '备注'")
	fmt.Println("  xray-proxy node import '订阅链接'")
	fmt.Println("  xray-proxy node auto [范围]         自动选择节点；范围可为 默认(default)/全局(global)/开发(dev)/电报(telegram)")
	fmt.Println("  xray-proxy node use '节点ID' [范围] 使用指定节点；范围可为 默认(default)/全局(global)/开发(dev)/电报(telegram)")
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
		choice := ask("请输入选项 [1-9]: ")
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
			if raw != "" {
				return "", false, fmt.Errorf("初始化命令只接受一个节点链接参数")
			}
			raw = arg
		}
	}
	return raw, promptNode, nil
}

func (a *App) install(raw string, promptNode bool) error {
	if err := requireRoot(); err != nil {
		return err
	}
	if raw == "" && promptNode {
		raw = ask("请输入节点链接（VLESS / VMess / Trojan / Shadowsocks，可留空跳过）: ")
	}
	if err := a.ensureCoreDirs(); err != nil {
		return err
	}
	if err := a.ensureXrayInstalled(); err != nil {
		return err
	}
	return a.withStoreLock(func() error {
		st, err := a.loadStore()
		if err != nil {
			return err
		}
		if strings.TrimSpace(raw) != "" {
			if _, err := a.addNode(st, raw, "", "default"); err != nil {
				return err
			}
			if err := a.saveStore(st); err != nil {
				return err
			}
		}
		if hasEnabledScene(st) {
			if len(st.Nodes) == 0 {
				return fmt.Errorf("已有场景处于开启状态，但没有可用节点")
			}
			if err := a.writeXrayConfig(st); err != nil {
				return err
			}
			if err := a.checkXrayConfig(); err != nil {
				return err
			}
		}
		if err := a.installXrayService(); err != nil {
			return err
		}
		if err := a.installRestoreService(); err != nil {
			return err
		}
		if hasEnabledScene(st) {
			if err := a.applySavedScenes(st); err != nil {
				return err
			}
			if err := a.saveStore(st); err != nil {
				return err
			}
			if err := a.startXrayService(); err != nil {
				return err
			}
		} else {
			_ = a.stopXrayService()
		}
		fmt.Println("管理服务初始化/更新完成")
		return nil
	})
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
	fmt.Printf("电报服务代理：%s\n", onOff(st.SceneEnabled[SceneTelegram]))
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
		st, err := a.loadStore()
		if err != nil {
			return err
		}
		if !hasEnabledScene(st) {
			if err := a.applySavedScenes(st); err != nil {
				return err
			}
			if err := a.saveStore(st); err != nil {
				return err
			}
			return a.stopXrayIfIdle(st)
		}
		if err := a.writeXrayConfig(st); err != nil {
			return err
		}
		if err := a.checkXrayConfig(); err != nil {
			return err
		}
		if err := a.applySavedScenes(st); err != nil {
			return err
		}
		if err := a.saveStore(st); err != nil {
			return err
		}
		return a.startXrayService()
	})
}

func (a *App) uninstall() error {
	if err := requireRoot(); err != nil {
		return err
	}
	var errs []error
	if err := a.setScene(SceneTelegram, false); err != nil {
		errs = append(errs, fmt.Errorf("关闭电报服务代理失败：%w", err))
	}
	if err := a.setScene(SceneDev, false); err != nil {
		errs = append(errs, fmt.Errorf("关闭开发代理失败：%w", err))
	}
	if err := a.setScene(SceneGlobal, false); err != nil {
		errs = append(errs, fmt.Errorf("关闭全局代理失败：%w", err))
	}
	if err := runQuietLabel("停止并禁用 Xray 主服务", "systemctl", "disable", "--now", a.cfg.SystemdService); err != nil {
		errs = append(errs, err)
	}
	if err := runQuietLabel("停止并禁用开机恢复服务", "systemctl", "disable", "--now", a.cfg.RestoreService); err != nil {
		errs = append(errs, err)
	}
	if err := removeIfExists("/etc/systemd/system/" + a.cfg.SystemdService); err != nil {
		errs = append(errs, fmt.Errorf("删除 Xray 主服务 unit 失败：%w", err))
	}
	if err := removeIfExists("/etc/systemd/system/" + a.cfg.RestoreService); err != nil {
		errs = append(errs, fmt.Errorf("删除开机恢复服务 unit 失败：%w", err))
	}
	if err := runQuietLabel("重新加载 systemd 配置", "systemctl", "daemon-reload"); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		for _, err := range errs {
			fmt.Println("警告：", err)
		}
		return fmt.Errorf("卸载过程中有 %d 个步骤失败，请查看上方警告", len(errs))
	}
	fmt.Println("已卸载 systemd 服务，数据目录保留：", a.cfg.CoreDir)
	return nil
}
