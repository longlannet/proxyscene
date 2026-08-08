package manager

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxStoreBytes      int64 = 128 << 20
	maxTelegramTargets       = 512
)

func (a *App) withStoreLock(fn func() error) error {
	return a.withInstallLock(func() error {
		return a.withStoreLockAfterInstallLock(fn)
	})
}

func (a *App) withInstallLock(fn func() error) error {
	return withFileLockOrInherited(installLockPath, inheritedInstallLockFDEnv, fn)
}

// withStoreLockAfterInstallLock continues the global lock order when the
// caller already holds the install lock and has an established installation
// ownership record. It avoids trying to flock an inherited installer fd twice.
func (a *App) withStoreLockAfterInstallLock(fn func() error) error {
	if err := a.validateInstallationOwnership(); err != nil {
		return err
	}
	return a.withHostAndStoreLocksAfterInstallLock(fn)
}

// withHostAndStoreLocksAfterInstallLock acquires the remaining host and state
// locks in the global order. Install uses the innermost callback to bind or
// validate installation ownership only after all three lock boundaries hold.
func (a *App) withHostAndStoreLocksAfterInstallLock(fn func() error) error {
	return a.withHostOwnership(func() error {
		return withFileLockOrInherited(a.cfg.StoreLockPath(), inheritedStoreLockFDEnv, fn)
	})
}

func (a *App) loadStore() (*Store, error) {
	return a.loadStoreWithRuntime(true)
}

func (a *App) loadStoreForBoot() (*Store, error) {
	return a.loadStoreWithRuntime(false)
}

func (a *App) loadStoreWithRuntime(allowOverrides bool) (*Store, error) {
	st, err := a.loadStoreRaw()
	if err != nil {
		return nil, err
	}
	processConfig := a.cfg
	resolved := processConfig
	if st.RuntimeConfig != nil {
		resolved = st.RuntimeConfig.applyTo(resolved)
	}
	if allowOverrides {
		resolved = resolved.applyRuntimeOverrides(processConfig)
	}
	if err := resolved.ValidateRuntime(); err != nil {
		return nil, fmt.Errorf("运行配置无效：%w", err)
	}
	a.cfg = resolved
	return st, nil
}

func (a *App) runtimeConfigDiffers(st *Store) bool {
	if st == nil {
		return false
	}
	if st.RuntimeConfig == nil {
		// 旧 schema 没有可比较的 runtime baseline。没有外部运行态且本进程也没有
		// 显式 runtime 覆盖时只需原地迁移；活动场景、待清理 ownership 或显式覆盖
		// 则必须执行一次完整协调。
		return hasEnabledScene(st) || len(st.TelegramTargets) > 0 || a.cfg.runtimeOverrides.any()
	}
	return !runtimeConfigsEqual(st.RuntimeConfig, a.cfg.runtimeConfig())
}

func (a *App) stageRuntimeConfig(st *Store) bool {
	if st == nil {
		return false
	}
	changed := a.runtimeConfigDiffers(st)
	st.RuntimeConfig = a.cfg.runtimeConfig()
	return changed
}

func (a *App) appForStoreRuntime(st *Store) (*App, error) {
	cfg := a.cfg
	if st != nil && st.RuntimeConfig != nil {
		cfg = st.RuntimeConfig.applyTo(cfg)
	}
	cfg.runtimeOverrides = runtimeConfigOverrideMask{}
	if err := cfg.ValidateRuntime(); err != nil {
		return nil, err
	}
	return NewApp(cfg), nil
}

func (a *App) loadStoreRaw() (*Store, error) {
	if err := a.validateInstallationOwnership(); err != nil {
		return nil, err
	}
	path := a.cfg.StorePath()
	b, err := readRegularFileNoFollow(path, maxStoreBytes)
	if errors.Is(err, os.ErrNotExist) {
		recovered, backupErr := a.loadStoreBackup()
		if backupErr == nil {
			fmt.Printf("警告：状态文件缺失，已从备份恢复：%s\n", path)
			return recovered, nil
		}
		if errors.Is(backupErr, os.ErrNotExist) {
			return newStore(), nil
		}
		return nil, fmt.Errorf("状态文件缺失且无法从备份恢复（%s）：%w", path, backupErr)
	}
	if err != nil {
		return nil, fmt.Errorf("读取状态文件失败（仅允许普通文件且拒绝符号链接）：%s：%w", path, err)
	}
	if len(b) == 0 {
		// 状态文件存在但为空：崩溃/掉电可能在 rename 落地后、数据块写回前把目标截断为
		// 0 字节。与 JSON 损坏一样优先尝试从备份恢复，避免静默清空全部节点与场景状态
		// （否则下一次 saveStore 会用空状态覆盖备份，造成不可逆丢失）。
		if recovered, backupErr := a.loadStoreBackup(); backupErr == nil {
			fmt.Printf("警告：状态文件为空，已从备份恢复：%s\n", path)
			return recovered, nil
		} else {
			return nil, fmt.Errorf("状态文件为空且无法从备份恢复（%s）：%w", path, backupErr)
		}
	}
	st, sanitized, decodeErr := decodeStore(b)
	if decodeErr != nil {
		if recovered, backupErr := a.loadStoreBackup(); backupErr == nil {
			fmt.Printf("警告：状态文件损坏，已从备份恢复：%s\n", path)
			return recovered, nil
		} else {
			return nil, errors.Join(
				fmt.Errorf("状态文件损坏（%s）：%w", path, decodeErr),
				fmt.Errorf("状态备份无法恢复：%w", backupErr),
			)
		}
	}
	if sanitized {
		fmt.Printf("警告：状态文件中的显示文本含控制字符，已在内存中清洗：%s\n", path)
	}
	return st, nil
}

func (a *App) loadStoreBackup() (*Store, error) {
	b, err := readRegularFileNoFollow(a.cfg.StoreBackupPath(), maxStoreBytes)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("状态备份为空")
	}
	st, sanitized, err := decodeStore(b)
	if err != nil {
		return nil, fmt.Errorf("状态备份损坏：%w", err)
	}
	if sanitized {
		fmt.Printf("警告：状态备份中的显示文本含控制字符，已在内存中清洗：%s\n", a.cfg.StoreBackupPath())
	}
	return st, nil
}

func decodeStore(data []byte) (*Store, bool, error) {
	st := newStore()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(st); err != nil {
		return nil, false, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("状态 JSON 含多余值")
		}
		return nil, false, err
	}
	normalizeStore(st)
	sanitized, err := validateStoreSemantics(st)
	if err != nil {
		return nil, false, err
	}
	return st, sanitized, nil
}

func validateStoreSemantics(st *Store) (bool, error) {
	if st.RuntimeConfig != nil {
		if err := validateRuntimeConfig(st.RuntimeConfig); err != nil {
			return false, fmt.Errorf("状态运行配置无效：%w", err)
		}
	}
	if len(st.Nodes) > maxTotalNodes {
		return false, fmt.Errorf("节点数超过上限 %d", maxTotalNodes)
	}
	if len(st.Subscriptions) > maxSubscriptions {
		return false, fmt.Errorf("订阅记录数超过上限 %d", maxSubscriptions)
	}
	if len(st.TelegramTargets) > maxTelegramTargets {
		return false, fmt.Errorf("状态中的 Telegram 目标数超过上限 %d", maxTelegramTargets)
	}
	if len(st.Nodes) == 0 && hasEnabledScene(st) {
		return false, fmt.Errorf("没有节点时不能保留已开启场景")
	}

	sanitized := false
	ids := make(map[string]bool, len(st.Nodes))
	urls := make(map[string]bool, len(st.Nodes))
	deadline := time.Now().Add(maxSubscriptionProcessingTime)
	for i := range st.Nodes {
		node := &st.Nodes[i]
		if time.Now().After(deadline) {
			return false, fmt.Errorf("状态节点校验超时")
		}
		if !safeStoreIdentifier(node.ID) || ids[node.ID] {
			return false, fmt.Errorf("状态含非法或重复节点 ID")
		}
		ids[node.ID] = true
		if len(node.RawURL) > maxNodeURLBytes || strings.TrimSpace(node.RawURL) != node.RawURL || urls[node.RawURL] {
			return false, fmt.Errorf("状态含过长、带外部空白或重复的节点链接")
		}
		prepared, err := prepareNode(node.RawURL)
		if err != nil {
			return false, fmt.Errorf("状态节点 %s 无效：%w", node.ID, err)
		}
		if node.Protocol != prepared.Parsed.Protocol {
			return false, fmt.Errorf("状态节点 %s 的协议与链接不一致", node.ID)
		}
		urls[node.RawURL] = true
		cleanName := sanitizeNodeName(node.Name)
		if len(cleanName) > maxNodeNameBytes {
			return false, fmt.Errorf("状态节点 %s 的备注过长", node.ID)
		}
		if cleanName != node.Name {
			node.Name = cleanName
			sanitized = true
		}
	}
	if st.DefaultNodeID != "" && !ids[st.DefaultNodeID] {
		return false, fmt.Errorf("默认节点 ID 不存在")
	}
	for scene, id := range st.SceneNodes {
		if !knownScene(scene) || id == "" || !ids[id] {
			return false, fmt.Errorf("场景节点映射无效")
		}
	}
	for scene := range st.SceneEnabled {
		if !knownScene(scene) {
			return false, fmt.Errorf("状态含未知场景")
		}
	}
	for id, result := range st.SpeedResults {
		if !ids[id] || result.NodeID != id {
			return false, fmt.Errorf("测速结果引用无效节点")
		}
		cleanTarget := sanitizeDisplayText(result.Target, 512)
		cleanError := sanitizeDisplayText(result.Error, 4096)
		if cleanTarget != result.Target || cleanError != result.Error {
			result.Target = cleanTarget
			result.Error = cleanError
			st.SpeedResults[id] = result
			sanitized = true
		}
	}

	seenSubscriptions := make(map[string]bool, len(st.Subscriptions))
	for _, subscription := range st.Subscriptions {
		if len(subscription) == 0 || len(subscription) > maxSubscriptionURLBytes || strings.TrimSpace(subscription) != subscription || seenSubscriptions[subscription] {
			return false, fmt.Errorf("状态含无效或重复订阅链接")
		}
		parsed, err := url.Parse(subscription)
		if err != nil || parsed.Host == "" || (strings.ToLower(parsed.Scheme) != "https" && strings.ToLower(parsed.Scheme) != "http") {
			return false, fmt.Errorf("状态含无效订阅链接")
		}
		seenSubscriptions[subscription] = true
	}
	seenTargets := make(map[string]bool, len(st.TelegramTargets))
	for _, target := range st.TelegramTargets {
		if seenTargets[target] || safeSystemdTargetName(target) != nil {
			return false, fmt.Errorf("状态含无效或重复 Telegram 目标")
		}
		seenTargets[target] = true
	}
	return sanitized, nil
}

func validateRuntimeConfig(runtime *RuntimeConfig) error {
	if runtime == nil {
		return nil
	}
	if runtime.Version != runtimeConfigVersion {
		return fmt.Errorf("不支持的版本 %d（当前支持 %d）", runtime.Version, runtimeConfigVersion)
	}
	if len(runtime.TGTargetServices) > maxTelegramTargets {
		return fmt.Errorf("运行配置中的 Telegram 目标数超过上限 %d", maxTelegramTargets)
	}
	cfg := runtime.applyTo(Config{TestURL: "https://www.google.com/generate_204"})
	return cfg.ValidateRuntime()
}

func safeStoreIdentifier(value string) bool {
	if value == "" || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	for _, c := range value {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func knownScene(scene Scene) bool {
	return scene == SceneGlobal || scene == SceneDev || scene == SceneTelegram
}

func sanitizeDisplayText(value string, maxBytes int) string {
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, value)
	if len(clean) > maxBytes {
		clean = clean[:maxBytes]
		for !utf8.ValidString(clean) {
			clean = clean[:len(clean)-1]
		}
	}
	return clean
}

func normalizeStore(st *Store) {
	if st.SceneNodes == nil {
		st.SceneNodes = map[Scene]string{}
	}
	if st.SceneEnabled == nil {
		st.SceneEnabled = map[Scene]bool{}
	}
	if st.SpeedResults == nil {
		st.SpeedResults = map[string]SpeedResult{}
	}
}

func cloneStore(st *Store) *Store {
	if st == nil {
		return newStore()
	}
	clone := *st
	clone.Nodes = append([]Node(nil), st.Nodes...)
	if st.RuntimeConfig != nil {
		runtimeClone := *st.RuntimeConfig
		runtimeClone.TGTargetServices = append([]string(nil), st.RuntimeConfig.TGTargetServices...)
		clone.RuntimeConfig = &runtimeClone
	}
	clone.Subscriptions = append([]string(nil), st.Subscriptions...)
	clone.TelegramTargets = append([]string(nil), st.TelegramTargets...)
	clone.SceneNodes = make(map[Scene]string, len(st.SceneNodes))
	for scene, id := range st.SceneNodes {
		clone.SceneNodes[scene] = id
	}
	clone.SceneEnabled = make(map[Scene]bool, len(st.SceneEnabled))
	for scene, enabled := range st.SceneEnabled {
		clone.SceneEnabled[scene] = enabled
	}
	clone.SpeedResults = make(map[string]SpeedResult, len(st.SpeedResults))
	for id, result := range st.SpeedResults {
		clone.SpeedResults[id] = result
	}
	normalizeStore(&clone)
	return &clone
}

func restoreStore(dst, snapshot *Store) {
	*dst = *cloneStore(snapshot)
}

var storeWriteFile = writeFileAtomic

func (a *App) saveStore(st *Store) error {
	candidate := cloneStore(st)
	if candidate.Generation == ^uint64(0) {
		return fmt.Errorf("状态 generation 已耗尽，拒绝回绕")
	}
	candidate.Generation++
	normalizeStore(candidate)
	if _, err := validateStoreSemantics(candidate); err != nil {
		return fmt.Errorf("拒绝保存无效状态：%w", err)
	}
	b, err := json.MarshalIndent(candidate, "", "  ")
	if err != nil {
		return err
	}
	data := append(b, '\n')
	if err := validateStoreDataSize(data, maxStoreBytes); err != nil {
		return err
	}
	// 先完整落地备份，再以主文件作为提交点。这样 main 仍有效时始终代表最后一次
	// 成功事务；若 main 缺失或损坏，backup 则保留最新的可恢复候选代。
	if err := storeWriteFile(a.cfg.StoreBackupPath(), data, 0o600); err != nil {
		return fmt.Errorf("写入状态备份失败：%w", err)
	}
	if err := storeWriteFile(a.cfg.StorePath(), data, 0o600); err != nil {
		return fmt.Errorf("提交主状态文件失败：%w", err)
	}
	restoreStore(st, candidate)
	return nil
}

func validateStoreDataSize(data []byte, limit int64) error {
	if int64(len(data)) > limit {
		return fmt.Errorf("状态数据过大：%d 字节，最大允许 %d 字节", len(data), limit)
	}
	return nil
}

func (st *Store) findNode(id string) *Node {
	for i := range st.Nodes {
		if st.Nodes[i].ID == id {
			return &st.Nodes[i]
		}
	}
	return nil
}

func (st *Store) findNodeByURL(raw string) *Node {
	for i := range st.Nodes {
		if st.Nodes[i].RawURL == raw {
			return &st.Nodes[i]
		}
	}
	return nil
}

func (st *Store) firstNodeID() string {
	if len(st.Nodes) == 0 {
		return ""
	}
	return st.Nodes[0].ID
}

func (st *Store) selectedNodeID(scene Scene) string {
	if id := st.SceneNodes[scene]; id != "" && st.findNode(id) != nil {
		return id
	}
	if st.DefaultNodeID != "" && st.findNode(st.DefaultNodeID) != nil {
		return st.DefaultNodeID
	}
	return st.firstNodeID()
}

// nodeIDCounter 保证同一进程内连续生成的节点 ID 唯一，避免在订阅导入等紧循环里
// 因 time.Now() 分辨率不足或时钟回拨而产生重复 ID。
var nodeIDCounter uint64

func newNodeID() string {
	seq := atomic.AddUint64(&nodeIDCounter, 1)
	return fmt.Sprintf("node-%s-%s", strconv.FormatInt(time.Now().UnixNano(), 36), strconv.FormatUint(seq, 36))
}
