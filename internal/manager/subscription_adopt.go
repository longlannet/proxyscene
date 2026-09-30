package manager

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"unicode"
)

// Old stores did not record subscription ownership. Adopt only explicitly
// selected nodes: a saved subscription alone is not evidence of their origin.
func adoptSubscriptionNodesInStore(st *Store, selector string, nodeIDs []string) error {
	sourceID, err := subscriptionMenuID(st, selector)
	if err != nil {
		return err
	}
	if len(nodeIDs) == 0 || len(nodeIDs) > maxTotalNodes {
		return fmt.Errorf("请选择 1 至 %d 个未由订阅管理的节点", maxTotalNodes)
	}
	seen := make(map[string]bool, len(nodeIDs))
	for _, id := range nodeIDs {
		if id == "" || seen[id] {
			return fmt.Errorf("节点选择为空或重复，请重新选择")
		}
		seen[id] = true
		node := st.findNode(id)
		if node == nil {
			return fmt.Errorf("所选节点不存在，请查看节点列表后重试")
		}
		if node.SubscriptionManaged {
			return fmt.Errorf("所选节点已由订阅管理，本次操作未提交")
		}
	}
	for _, id := range nodeIDs {
		node := st.findNode(id)
		if !slices.Contains(node.SubscriptionIDs, sourceID) {
			node.SubscriptionIDs = append(node.SubscriptionIDs, sourceID)
			slices.Sort(node.SubscriptionIDs)
		}
		node.SubscriptionManaged = true
	}
	return nil
}

func (a *App) commitSubscriptionNodeAdoption(st *Store, selector string, nodeIDs []string) error {
	if err := a.commitStoreMutation(st, func(candidate *Store) error {
		return adoptSubscriptionNodesInStore(candidate, selector, nodeIDs)
	}, storeRuntimeSyncNone); err != nil {
		return err
	}
	fmt.Printf("已将 %d 个历史或手动节点交由订阅管理；下次更新将完整替换该订阅的节点；已撤回的选中节点也会删除，并切换到当前列表中的节点。\n", len(nodeIDs))
	return nil
}

// The command itself is an explicit ownership change. Node arguments must be
// full IDs; menu indices are instead resolved against the displayed snapshot.
func (a *App) adoptSubscriptionNodes(selector string, nodeIDs []string) error {
	return a.withLockedStoreRoot(func(st *Store) error {
		return a.commitSubscriptionNodeAdoption(st, selector, nodeIDs)
	})
}

func (a *App) adoptSubscriptionNodesMenu() error {
	st, err := a.loadStore()
	if err != nil {
		return err
	}
	if len(st.Subscriptions) == 0 {
		return fmt.Errorf("没有已保存的订阅，请先导入")
	}
	candidates := &Store{}
	for _, node := range st.Nodes {
		if !node.SubscriptionManaged {
			candidates.Nodes = append(candidates.Nodes, node)
		}
	}
	if len(candidates.Nodes) == 0 {
		fmt.Println("没有尚未由订阅管理的历史或手动节点。")
		return nil
	}
	fmt.Println("请选择这些历史或手动节点原本所属的订阅：")
	writeSubscriptionList(os.Stdout, st)
	selector, err := menuInput("订阅序号或 ID（q 取消）: ")
	if err != nil {
		return err
	}
	if selector == "" {
		return errMenuCancelled
	}
	sourceID, err := subscriptionMenuID(st, selector)
	if err != nil {
		return err
	}
	fmt.Println("以下历史或手动节点尚未由订阅管理；请只选择确认来自该订阅的节点，勿选择其他独立添加的节点：")
	for i, node := range candidates.Nodes {
		usage := "备用"
		if st.DefaultNodeID == node.ID || st.selectedNodeID(SceneGlobal) == node.ID || st.selectedNodeID(SceneDev) == node.ID || st.selectedNodeID(SceneTelegram) == node.ID {
			usage = "当前选用，若订阅撤回则在更新时替换"
		}
		fmt.Printf("%d. %s [%s] ID:%s；%s\n", i+1, sanitizeDisplayText(node.Name, 100), node.Protocol, menuShortNodeID(candidates, node.ID), usage)
	}
	input, err := menuInput("节点序号或 ID，可用逗号或空格分隔（留空或 q 取消）: ")
	if err != nil {
		return err
	}
	if input == "" {
		return errMenuCancelled
	}
	selectors := strings.FieldsFunc(input, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	if len(selectors) == 0 || len(selectors) > maxTotalNodes {
		return fmt.Errorf("请选择 1 至 %d 个节点", maxTotalNodes)
	}
	nodeIDs := make([]string, 0, len(selectors))
	for _, nodeSelector := range selectors {
		id, err := menuResolveNode(candidates, nodeSelector)
		if err != nil {
			return err
		}
		nodeIDs = append(nodeIDs, id)
	}
	if err := adoptSubscriptionNodesInStore(cloneStore(st), sourceID, nodeIDs); err != nil {
		return err
	}
	fmt.Printf("以下 %d 个节点将交由订阅 %s 管理（保留已有来源）：\n", len(nodeIDs), sourceID[:16])
	for _, id := range nodeIDs {
		fmt.Printf("- %s [ID:%s]\n", sanitizeDisplayText(st.findNode(id).Name, 100), menuShortNodeID(candidates, id))
	}
	fmt.Println("下次更新时，所有来源均已移除的节点将被删除；当前选用的节点若被撤回，将自动选择替代节点并同步代理配置。")
	confirmed, err := menuConfirm("确认这些节点来自该订阅并交由订阅管理？")
	if err != nil {
		return err
	}
	if !confirmed {
		return errMenuCancelled
	}
	return a.withMenuNodeSnapshot(st, func(current *Store) error {
		return a.commitSubscriptionNodeAdoption(current, sourceID, nodeIDs)
	})
}
