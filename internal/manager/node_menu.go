package manager

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

func (a *App) nodeMenu() error {
	for {
		st, err := a.loadStore()
		if err != nil {
			return err
		}
		fmt.Println("\n========== 节点管理 ==========")
		fmt.Printf("共 %d 个节点；默认：%s\n", len(st.Nodes), menuNodeLabel(st, st.DefaultNodeID))
		if len(st.Nodes) == 0 {
			fmt.Println("请选择 3 添加节点，或返回主菜单进入订阅管理。")
		}
		fmt.Println("1. 查看节点列表")
		fmt.Println("2. 选择使用节点")
		fmt.Println("3. 添加节点")
		fmt.Println("4. 代理连通性/延迟测试")
		fmt.Println("5. 按代理请求延迟选用节点")
		fmt.Println("6. 修改节点备注")
		fmt.Println("7. 删除节点")
		fmt.Println("0. 返回")
		choice, err := menuInput("请输入选项 [0-7]，q 返回: ")
		if errors.Is(err, errMenuCancelled) || choice == "0" {
			return nil
		}
		if err != nil {
			return err
		}
		switch choice {
		case "1":
			printMenuNodes(st)
		case "2":
			err = a.menuUseNode(st)
		case "3":
			err = a.menuAddNode(st)
		case "4":
			err = a.speedTestWithLock()
		case "5":
			err = a.menuAutoNode(st)
		case "6":
			err = a.menuRenameNode(st)
		case "7":
			err = a.menuRemoveNode(st)
		default:
			fmt.Println("无效选项，请输入 0-7。")
		}
		if err := reportMenuAction(err); err != nil {
			return err
		}
	}
}

// Menu selections are resolved against the list the operator actually saw.
// The locked write checks the same snapshot instead of reinterpreting an index.
func menuResolveNode(st *Store, selector string) (string, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return "", fmt.Errorf("请输入节点序号或 ID")
	}
	if n := st.findNode(selector); n != nil {
		return n.ID, nil
	}
	if index, err := strconv.Atoi(selector); err == nil && index > 0 && index <= len(st.Nodes) {
		return st.Nodes[index-1].ID, nil
	}
	id := ""
	for _, n := range st.Nodes {
		if strings.HasPrefix(n.ID, selector) {
			if id != "" {
				return "", fmt.Errorf("短 ID 匹配多个节点，请输入更长的 ID 或列表序号")
			}
			id = n.ID
		}
	}
	if id == "" {
		return "", fmt.Errorf("未找到该节点，请使用列表序号、完整 ID 或唯一短 ID")
	}
	return id, nil
}

func menuShortNodeID(st *Store, id string) string {
	for size := min(8, len(id)); size < len(id); size++ {
		resolved, err := menuResolveNode(st, id[:size])
		if err == nil && resolved == id {
			return id[:size]
		}
	}
	return id
}

func printMenuNodes(st *Store) {
	if len(st.Nodes) == 0 {
		fmt.Println("节点列表为空；请选择“添加节点”，或在订阅管理中导入订阅。")
		return
	}
	for i, n := range st.Nodes {
		usage := []string{}
		if st.DefaultNodeID == n.ID {
			usage = append(usage, "默认")
		}
		for _, scene := range []Scene{SceneGlobal, SceneDev, SceneTelegram} {
			if st.selectedNodeID(scene) != n.ID {
				continue
			}
			kind := "单独指定"
			if st.SceneNodes[scene] == "" {
				kind = "跟随默认"
			}
			usage = append(usage, menuSceneName(scene)+"（"+kind+"）")
		}
		if n.SubscriptionManaged && len(n.SubscriptionIDs) == 0 {
			usage = append(usage, "订阅已移除，保留待切换")
		}
		if len(usage) == 0 {
			usage = append(usage, "备用")
		}
		fmt.Printf("%d. %s [%s] ID:%s；%s\n", i+1, sanitizeDisplayText(n.Name, 100), n.Protocol, menuShortNodeID(st, n.ID), strings.Join(usage, "、"))
	}
}

func menuPickNode(st *Store) (string, error) {
	printMenuNodes(st)
	if len(st.Nodes) == 0 {
		return "", errMenuCancelled
	}
	selector, err := menuInput("节点序号或 ID（q 取消）: ")
	if err != nil {
		return "", err
	}
	return menuResolveNode(st, selector)
}

func menuPickNodeScope() (string, error) {
	fmt.Println("使用范围：1. 默认  2. 全局  3. 开发  4. Telegram  5. 全部  0. 取消")
	choice, err := menuInput("请选择范围 [1-5]（q 取消）: ")
	if err != nil {
		return "", err
	}
	switch choice {
	case "1":
		return "default", nil
	case "2":
		return string(SceneGlobal), nil
	case "3":
		return string(SceneDev), nil
	case "4":
		return string(SceneTelegram), nil
	case "5":
		return "all", nil
	case "0":
		return "", errMenuCancelled
	default:
		return "", fmt.Errorf("无效范围，请重新选择节点及范围")
	}
}

func printMenuNodeChanges(before, after *Store) {
	if before.DefaultNodeID != after.DefaultNodeID {
		fmt.Printf("默认节点：%s → %s\n", menuNodeLabel(before, before.DefaultNodeID), menuNodeLabel(after, after.DefaultNodeID))
	}
	changed := false
	for _, scene := range []Scene{SceneGlobal, SceneDev, SceneTelegram} {
		oldID, nextID := before.selectedNodeID(scene), after.selectedNodeID(scene)
		if oldID == nextID && before.SceneNodes[scene] == after.SceneNodes[scene] && before.SceneEnabled[scene] == after.SceneEnabled[scene] {
			continue
		}
		changed = true
		kind := "跟随默认"
		if after.SceneNodes[scene] != "" {
			kind = "单独指定"
		}
		state := "关闭"
		if after.SceneEnabled[scene] {
			state = "开启"
		}
		fmt.Printf("%s：%s → %s（%s，%s）\n", menuSceneName(scene), menuNodeLabel(before, oldID), menuNodeLabel(after, nextID), kind, state)
	}
	if !changed {
		fmt.Println("各场景选用节点与开关不变。")
	}
}

func (a *App) withMenuNodeSnapshot(snapshot *Store, action func(*Store) error) error {
	return a.withLockedStoreRoot(func(current *Store) error {
		if snapshot == nil || !reflect.DeepEqual(snapshot, current) {
			return fmt.Errorf("交互期间节点或配置发生变化，本次操作未提交，请重新选择并确认")
		}
		return action(current)
	})
}

func (a *App) menuConfirmUseNode(st *Store, id, scope string, results []SpeedResult) error {
	preview := cloneStore(st)
	if err := a.useNodeInStore(preview, id, scope); err != nil {
		return err
	}
	fmt.Printf("选用节点：%s\n", menuNodeLabel(st, id))
	printMenuNodeChanges(st, preview)
	ok, err := menuConfirm("确认选用此节点？")
	if err != nil {
		return err
	}
	if !ok {
		return errMenuCancelled
	}
	return a.withMenuNodeSnapshot(st, func(current *Store) error {
		return a.commitNodeStoreMutation(current, func(candidate *Store) error {
			if results != nil {
				mergeSpeedResults(candidate, st.Nodes, results)
			}
			return a.useNodeInStore(candidate, id, scope)
		})
	})
}

func (a *App) menuUseNode(st *Store) error {
	id, err := menuPickNode(st)
	if err != nil {
		return err
	}
	scope, err := menuPickNodeScope()
	if err != nil {
		return err
	}
	return a.menuConfirmUseNode(st, id, scope, nil)
}

func (a *App) menuAddNode(st *Store) error {
	raw, err := menuInput("节点链接（q 取消）: ")
	if err != nil {
		return err
	}
	prepared, err := prepareNode(raw)
	if err != nil {
		return err
	}
	name, err := menuInput("备注名（可留空，q 取消）: ")
	if err != nil {
		return err
	}
	preview := cloneStore(st)
	id, err := a.addPreparedNodeIndexed(preview, prepared, name, "", nil)
	if err != nil {
		return err
	}
	if len(st.Nodes) != 0 {
		// Existing stores may use the first-node fallback without an explicit
		// default. Saving a backup must preserve that selection as well.
		preview.DefaultNodeID = st.DefaultNodeID
	}
	fmt.Printf("保存节点：%s\n", menuNodeLabel(preview, id))
	if len(st.Nodes) == 0 {
		fmt.Println("首个节点会自动设为默认节点。")
		ok, err := menuConfirm("确认添加节点？")
		if err != nil {
			return err
		}
		if !ok {
			return errMenuCancelled
		}
	} else {
		withDefault := cloneStore(preview)
		if err := a.useNodeInStore(withDefault, id, "default"); err != nil {
			return err
		}
		fmt.Println("如设为默认节点，影响如下；回答否仅保存节点：")
		printMenuNodeChanges(st, withDefault)
		ok, err := menuConfirm("同时设为默认节点？")
		if err != nil {
			return err
		}
		if ok {
			preview = withDefault
		}
	}
	mode := storeRuntimeSyncNone
	if preview.DefaultNodeID != st.DefaultNodeID {
		mode = storeRuntimeSyncXray
	}
	return a.withMenuNodeSnapshot(st, func(current *Store) error {
		return a.commitStoreMutation(current, func(candidate *Store) error {
			// Persist the exact staged node, including the ID shown in the preview.
			staged := cloneStore(preview)
			candidate.Nodes = staged.Nodes
			candidate.DefaultNodeID = staged.DefaultNodeID
			return nil
		}, mode)
	})
}

func (a *App) menuRenameNode(st *Store) error {
	id, err := menuPickNode(st)
	if err != nil {
		return err
	}
	name, err := menuInput("新备注（q 取消）: ")
	if err != nil {
		return err
	}
	return a.withMenuNodeSnapshot(st, func(current *Store) error { return a.renameNode(current, id, name) })
}

func (a *App) menuRemoveNode(st *Store) error {
	id, err := menuPickNode(st)
	if err != nil {
		return err
	}
	preview := cloneStore(st)
	if err := removeNodeFromStore(preview, id); err != nil {
		return err
	}
	fmt.Printf("即将删除：%s\n", menuNodeLabel(st, id))
	if len(preview.Nodes) == 0 {
		fmt.Println("这是最后一个节点，删除后所有代理场景均关闭。")
	}
	printMenuNodeChanges(st, preview)
	ok, err := menuConfirm("确认删除节点？")
	if err != nil {
		return err
	}
	if !ok {
		return errMenuCancelled
	}
	return a.withMenuNodeSnapshot(st, func(current *Store) error { return a.removeNode(current, id) })
}

func (a *App) menuAutoNode(st *Store) error {
	if len(st.Nodes) == 0 {
		return fmt.Errorf("没有可测试节点，请先添加节点或导入订阅")
	}
	fmt.Println("正在通过各节点请求 HTTPS，检查代理连通性并测量请求延迟；这不是带宽测试。")
	results, err := a.runSpeedTests(st.Nodes)
	printSpeedResults(st.Nodes, results)
	if err != nil {
		return err
	}
	preview := cloneStore(st)
	id := fastestNodeID(mergeSpeedResults(preview, st.Nodes, results))
	if id == "" {
		return fmt.Errorf("没有通过代理请求测试的节点")
	}
	fmt.Printf("本次代理请求延迟最低的节点：%s\n", menuNodeLabel(st, id))
	scope, err := menuPickNodeScope()
	if err != nil {
		return err
	}
	return a.menuConfirmUseNode(st, id, scope, results)
}
