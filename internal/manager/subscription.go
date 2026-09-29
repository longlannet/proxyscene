package manager

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

const maxSubscriptionUpdateTime = 2 * time.Minute

// IDs identify saved URLs without putting their credentials in argv or output.
func subscriptionID(rawURL string) string {
	return fmt.Sprintf("sub-%x", sha256.Sum256([]byte(rawURL)))
}

func subscriptionLabel(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "未知主机"
	}
	return safeSubscriptionHost(u)
}

func writeSubscriptionList(w io.Writer, st *Store) {
	if len(st.Subscriptions) == 0 {
		fmt.Fprintln(w, "没有已保存的订阅，请先通过 node import --stdin 导入")
		return
	}
	counts := make(map[string]int)
	for _, node := range st.Nodes {
		for _, id := range node.SubscriptionIDs {
			counts[id]++
		}
	}
	for i, raw := range st.Subscriptions {
		id := subscriptionID(raw)
		fmt.Fprintf(w, "%d. %s  %s（关联节点：%d）\n", i+1, id[:16], subscriptionLabel(raw), counts[id])
	}
}

func selectSubscriptionURLs(st *Store, selector string) ([]string, error) {
	if len(st.Subscriptions) == 0 {
		return nil, fmt.Errorf("没有已保存的订阅，请先导入")
	}
	if selector == "--all" {
		return slices.Clone(st.Subscriptions), nil
	}
	if index, err := strconv.Atoi(selector); err == nil && index > 0 && index <= len(st.Subscriptions) {
		return []string{st.Subscriptions[index-1]}, nil
	}
	// Only opaque IDs or numeric indices are accepted, never secret URLs.
	if len(selector) < 16 || len(selector) > 68 || !strings.HasPrefix(selector, "sub-") {
		return nil, fmt.Errorf("请提供订阅序号、列表中的订阅 ID 或 --all")
	}
	for _, c := range selector[4:] {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return nil, fmt.Errorf("订阅 ID 格式无效")
		}
	}
	var matches []string
	for _, raw := range st.Subscriptions {
		if strings.HasPrefix(subscriptionID(raw), selector) {
			matches = append(matches, raw)
		}
	}
	if len(matches) != 1 {
		return nil, fmt.Errorf("订阅 ID 不存在或前缀不唯一，请查看 subscription list")
	}
	return matches, nil
}

func (a *App) subscriptionCommand(args []string) error {
	if len(args) == 0 {
		return a.subscriptionMenu()
	}
	if len(args) == 1 && args[0] == "list" {
		st, err := a.loadStore()
		if err != nil {
			return err
		}
		writeSubscriptionList(os.Stdout, st)
		return nil
	}
	if len(args) == 2 && args[0] == "update" {
		return a.updateSubscriptions(args[1])
	}
	return fmt.Errorf("用法：subscription list | subscription update <序号或ID> | subscription update --all")
}

func (a *App) subscriptionMenu() error {
	for {
		st, err := a.loadStore()
		if err != nil {
			return err
		}
		fmt.Println("\n========== 订阅管理 ==========")
		if len(st.Subscriptions) == 0 {
			fmt.Println("暂无订阅，选择 1 添加/导入订阅")
		} else {
			writeSubscriptionList(os.Stdout, st)
		}
		fmt.Println("1. 添加/导入订阅\n2. 更新单个订阅\n3. 更新全部订阅\n0. 返回")
		choice, err := menuInput("请输入选项 [0-3，q 返回]: ")
		if errors.Is(err, errMenuCancelled) || choice == "0" {
			return nil
		}
		if err != nil {
			return err
		}
		switch choice {
		case "1":
			raw, inputErr := menuInput("订阅链接（q 取消）: ")
			err = inputErr
			if err == nil {
				err = a.importSubscriptionWithLock(raw)
			}
		case "2":
			selector, inputErr := menuInput("订阅序号或 ID（q 取消）: ")
			err = inputErr
			if err == nil {
				var id string
				id, err = subscriptionMenuID(st, selector)
				if err == nil {
					err = a.updateSubscriptions(id)
				}
			}
		case "3":
			if len(st.Subscriptions) == 0 {
				err = fmt.Errorf("暂无订阅，请选择 1 添加/导入")
				break
			}
			var confirmed bool
			confirmed, err = menuConfirm("确认更新当前全部已保存的订阅？")
			if err == nil && confirmed {
				err = a.updateSubscriptions("--all")
			}
		default:
			fmt.Println("无效选项，请输入 0-3")
			continue
		}
		if err := reportMenuAction(err); err != nil {
			return err
		}
	}
}

// Resolve numeric choices against the list that was displayed. A later reload
// may reorder subscriptions, but must never change which source was selected.
func subscriptionMenuID(displayed *Store, selector string) (string, error) {
	if selector == "--all" {
		return "", fmt.Errorf("请选择单个订阅的序号或 ID")
	}
	urls, err := selectSubscriptionURLs(displayed, selector)
	if err != nil {
		return "", err
	}
	return subscriptionID(urls[0]), nil
}

func (a *App) updateSubscriptions(selector string) error {
	if err := requireRoot(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), maxSubscriptionUpdateTime)
	defer cancel()
	snapshot, err := a.loadStore()
	if err != nil {
		return err
	}
	downloader, err := a.subscriptionDownloaderForStore(snapshot)
	if err != nil {
		return err
	}
	defer downloader.Close()
	return a.updateSubscriptionsFromSnapshot(snapshot, selector, func(raw string) (preparedSubscription, error) {
		return downloader.Prepare(ctx, raw)
	})
}

func (a *App) updateSubscriptionsWithDownloader(selector string, download func(string) (preparedSubscription, error)) error {
	snapshot, err := a.loadStore()
	if err != nil {
		return err
	}
	return a.updateSubscriptionsFromSnapshot(snapshot, selector, download)
}

func (a *App) updateSubscriptionsFromSnapshot(snapshot *Store, selector string, download func(string) (preparedSubscription, error)) error {
	urls, err := selectSubscriptionURLs(snapshot, selector)
	if err != nil {
		return err
	}
	prepared := make([]preparedSubscription, 0, len(urls))
	total := 0
	for _, raw := range urls {
		next, err := download(raw)
		if err != nil {
			return fmt.Errorf("订阅 %s 更新失败：%w", subscriptionID(raw)[:16], err)
		}
		// Keep the exact saved identity even for legacy URLs whose scheme was
		// normalized by the downloader. Redirect targets never change ownership.
		next.URL = raw
		if err := validateSubscriptionRefresh(next); err != nil {
			return err
		}
		total += len(next.Nodes)
		if total > maxTotalNodes {
			return fmt.Errorf("本次更新节点条目总数超过上限 %d，请分别更新订阅", maxTotalNodes)
		}
		prepared = append(prepared, next)
	}
	return a.withStoreLock(func() error {
		st, err := a.loadStore()
		if err != nil {
			return err
		}
		if st.Generation != snapshot.Generation || !slices.Equal(st.Subscriptions, snapshot.Subscriptions) {
			return fmt.Errorf("下载期间状态发生变化，本次更新未提交，请重试")
		}
		return a.commitPreparedSubscriptions(st, prepared)
	})
}

func validateSubscriptionRefresh(prepared preparedSubscription) error {
	if len(prepared.Nodes) == 0 || prepared.Invalid != 0 || prepared.Incomplete {
		return fmt.Errorf("订阅 %s 内容为空或含无效/无法完整识别的条目，本次更新未提交（%s）", subscriptionID(prepared.URL)[:16], prepared.diagnosticSummary())
	}
	return nil
}

type subscriptionChanges struct {
	Added, Existing, Removed, Retained, Invalid int
}

// Reconcile all sources together so a node moving between subscriptions keeps
// its identity. Build the final set before applying the global capacity limit.
func reconcileSubscriptions(st *Store, prepared []preparedSubscription) (subscriptionChanges, error) {
	var changes subscriptionChanges
	subscriptions := slices.Clone(st.Subscriptions)
	refreshing := make(map[string]bool, len(prepared))
	desired := make(map[string]preparedNode)
	sources := make(map[string][]string)
	var order []string
	for _, sub := range prepared {
		if len(sub.Nodes) == 0 {
			return changes, fmt.Errorf("订阅中没有可导入节点")
		}
		if slices.Contains(st.Subscriptions, sub.URL) {
			if err := validateSubscriptionRefresh(sub); err != nil {
				return changes, err
			}
		} else {
			if len(subscriptions) >= maxSubscriptions {
				return changes, fmt.Errorf("订阅记录数已达到上限 %d", maxSubscriptions)
			}
			subscriptions = append(subscriptions, sub.URL)
		}
		id := subscriptionID(sub.URL)
		if refreshing[id] {
			return changes, fmt.Errorf("本次更新含重复订阅")
		}
		refreshing[id] = true
		changes.Invalid += sub.Invalid
		for _, node := range sub.Nodes {
			if node.Parsed == nil || node.RawURL == "" {
				return changes, fmt.Errorf("订阅节点解析结果无效")
			}
			if _, found := desired[node.RawURL]; !found {
				desired[node.RawURL] = node
				order = append(order, node.RawURL)
			}
			if !slices.Contains(sources[node.RawURL], id) {
				sources[node.RawURL] = append(sources[node.RawURL], id)
			}
		}
	}
	protected := map[string]bool{st.DefaultNodeID: true}
	for _, id := range st.SceneNodes {
		protected[id] = true
	}
	for _, scene := range []Scene{SceneGlobal, SceneDev, SceneTelegram} {
		protected[st.selectedNodeID(scene)] = true
	}
	nodes := make([]Node, 0, len(st.Nodes))
	seen := make(map[string]bool, len(st.Nodes))
	ids := make(map[string]bool, len(st.Nodes))
	var removed []string
	for _, node := range st.Nodes {
		seen[node.RawURL] = true
		ids[node.ID] = true
		memberships := make([]string, 0, len(node.SubscriptionIDs))
		for _, id := range node.SubscriptionIDs {
			if !refreshing[id] {
				memberships = append(memberships, id)
			}
		}
		memberships = append(memberships, sources[node.RawURL]...)
		slices.Sort(memberships)
		node.SubscriptionIDs = memberships
		if node.SubscriptionManaged && len(memberships) == 0 {
			if !protected[node.ID] {
				removed = append(removed, node.ID)
				changes.Removed++
				continue
			}
			changes.Retained++
		}
		if _, found := desired[node.RawURL]; found {
			changes.Existing++
		}
		nodes = append(nodes, node)
	}
	now := time.Now()
	for _, raw := range order {
		if seen[raw] {
			continue
		}
		prepared := desired[raw]
		id := newNodeID()
		for ids[id] {
			id = newNodeID()
		}
		ids[id] = true
		name := prepared.Parsed.Name
		if name == "" {
			name = prepared.Parsed.Protocol + "-node"
		}
		memberships := sources[raw]
		slices.Sort(memberships)
		nodes = append(nodes, Node{ID: id, Name: name, Protocol: prepared.Parsed.Protocol, RawURL: raw,
			CreatedAt: now, UpdatedAt: now, SubscriptionIDs: memberships, SubscriptionManaged: true})
		changes.Added++
	}
	if len(nodes) > maxTotalNodes {
		return subscriptionChanges{}, fmt.Errorf("更新后节点总数超过上限 %d，本次更新未提交", maxTotalNodes)
	}
	// An old store may use firstNodeID as an implicit selection. Never replace
	// that choice merely because DefaultNodeID was empty before the update.
	if len(st.Nodes) == 0 && len(nodes) > 0 {
		st.DefaultNodeID = nodes[0].ID
	}
	st.Nodes = nodes
	st.Subscriptions = subscriptions
	for _, id := range removed {
		delete(st.SpeedResults, id)
	}
	return changes, nil
}

func (a *App) commitPreparedSubscriptions(st *Store, prepared []preparedSubscription) error {
	var changes subscriptionChanges
	err := a.commitStoreMutation(st, func(candidate *Store) error {
		var err error
		changes, err = reconcileSubscriptions(candidate, prepared)
		return err
	}, storeRuntimeSyncNone)
	if err != nil {
		return err
	}
	fmt.Printf("订阅同步完成：新增 %d 个，已有 %d 个，移除 %d 个，跳过无效 %d 个\n", changes.Added, changes.Existing, changes.Removed, changes.Invalid)
	for _, sub := range prepared {
		if sub.Invalid > 0 || sub.Incomplete || sub.Format != "" {
			fmt.Printf("订阅 %s：%s\n", subscriptionID(sub.URL)[:16], sub.diagnosticSummary())
		}
	}
	if changes.Retained > 0 {
		fmt.Printf("保留 %d 个已不在订阅中但仍被选中的节点；请手动选择替代节点，下次更新再清理\n", changes.Retained)
	}
	return nil
}
