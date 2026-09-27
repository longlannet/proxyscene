package manager

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Keep interactive cancellation distinct from I/O errors in business operations.
var (
	errMenuClosed    = errors.New("交互输入已结束")
	errMenuCancelled = errors.New("已取消")
)

func menuInput(prompt string) (string, error) {
	fmt.Print(prompt)
	// An unterminated answer followed by EOF is not a submitted choice. In
	// particular, a pipe ending in "y" must never authorize a mutation.
	value, err := stdinReader.ReadString('\n')
	if err != nil {
		return "", errMenuClosed
	}
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "q") {
		return "", errMenuCancelled
	}
	return value, nil
}

func menuConfirm(prompt string) (bool, error) {
	value, err := menuInput(prompt + " [y/N，q 取消]: ")
	if err != nil {
		return false, err
	}
	return strings.EqualFold(value, "y") || strings.EqualFold(value, "yes"), nil
}

// EOF propagates through nested menus to Run. Cancelling a single action leaves
// its menu open; ordinary failures remain visible before the next prompt.
func reportMenuAction(err error) error {
	if errors.Is(err, errMenuClosed) {
		return err
	}
	if err != nil {
		fmt.Println(err)
	}
	return nil
}

func menuSceneName(scene Scene) string {
	if scene == SceneTelegram {
		return "Telegram 服务代理"
	}
	return sceneName(scene)
}

func menuNodeLabel(st *Store, id string) string {
	if st != nil {
		if node := st.findNode(id); node != nil {
			return fmt.Sprintf("%s [%s]", sanitizeDisplayText(node.Name, 100), node.ID)
		}
	}
	return "未选择节点"
}

func printMenuSceneSummary(st *Store) {
	writeMenuSceneSummary(os.Stdout, st)
}

func writeMenuSceneSummary(w io.Writer, st *Store) {
	if st == nil {
		fmt.Fprintln(w, "配置状态未知")
		return
	}
	fmt.Fprintln(w, "配置开关与选用节点：")
	for _, scene := range []Scene{SceneGlobal, SceneDev, SceneTelegram} {
		id := st.selectedNodeID(scene)
		selection := "跟随默认"
		if st.SceneNodes[scene] != "" && st.SceneNodes[scene] == id {
			selection = "单独指定"
		}
		fmt.Fprintf(w, "  %s：%s；%s（%s）\n", menuSceneName(scene), onOff(st.SceneEnabled[scene]), menuNodeLabel(st, id), selection)
	}
	fmt.Fprintln(w, "连接是否可用，请进入「状态与连接检测」查看或测试。")
}

func (a *App) statusMenu() error {
	for {
		fmt.Println("\n========== 状态与连接检测 ==========")
		fmt.Println("1. 查看详细状态\n2. 测试全局代理出网\n3. 查看 Telegram 接管状态\n0. 返回")
		choice, err := menuInput("请输入选项 [0-3，q 返回]: ")
		if errors.Is(err, errMenuCancelled) || (err == nil && choice == "0") {
			return nil
		}
		if err != nil {
			return err
		}
		switch choice {
		case "1":
			err = a.status()
		case "2":
			err = a.testProxy()
		case "3":
			// Refresh the persisted runtime before inspecting its targets.
			if _, err = a.loadStore(); err == nil {
				a.printTelegramTargetStatus()
			}
		default:
			fmt.Println("无效选项，请输入 0-3")
		}
		if err := reportMenuAction(err); err != nil {
			return err
		}
	}
}

func (a *App) maintenanceMenu() error {
	for {
		fmt.Println("\n========== 安装与维护 ==========")
		fmt.Println("1. 初始化/修复管理服务\n2. 卸载服务（保留数据）\n0. 返回")
		choice, err := menuInput("请输入选项 [0-2，q 返回]: ")
		if errors.Is(err, errMenuCancelled) || (err == nil && choice == "0") {
			return nil
		}
		if err != nil {
			return err
		}
		switch choice {
		case "1":
			fmt.Println("将初始化或修复管理服务与运行配置，可能重启相关服务。")
			err = a.install("", true)
		case "2":
			err = a.confirmUninstall()
		default:
			fmt.Println("无效选项，请输入 0-2")
		}
		if err := reportMenuAction(err); err != nil {
			return err
		}
	}
}

func (a *App) confirmUninstall() error {
	if err := requireRoot(); err != nil {
		return err
	}
	fmt.Println("将关闭全部代理场景、恢复本程序接管的配置，并停止和移除管理服务。")
	fmt.Println("程序文件及数据目录保留：", a.cfg.CoreDir)
	confirmed, err := menuConfirm("确认卸载服务？")
	if err != nil {
		return err
	}
	if !confirmed {
		return errMenuCancelled
	}
	// Uninstall reads only persisted runtime settings. Preserve this menu's
	// explicit overrides if cleanup fails and the operator continues using it.
	return NewApp(a.cfg).uninstall()
}
