package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// OpenClaw 不读取 TELEGRAM_*PROXY 环境变量，其「仅代理 Telegram」的唯一开关是配置项
// channels.telegram.proxy（参见 openclaw 的 resolveTelegramDispatcherPolicy / "set
// channels.telegram.proxy in config" 报错）。因此对 openclaw 目标，proxyscene 不写无效的
// systemd env drop-in，而是直接读改其配置文件 <用户家目录>/.openclaw/openclaw.json。
//
// 设计上采用无状态、对称的「找到项即改」：开启场景时把 channels.telegram.proxy 设为本机
// Telegram 入站地址；关闭场景时——只有当该值「仍等于我们设的地址」时——才删除它。用值比对
// 判断归属，既不需要备份文件，也不会覆盖用户在开启期间手动改成别的值的情况。每次只改这一个
// 键、保留其余全部字段（含 botToken 等机密），原子写入；配置不存在则跳过（不凭空创建）。

const maxOpenClawConfigBytes int64 = 8 << 20

// isOpenClawTarget 判定一个 telegram 目标是否应走 openclaw 配置接管（而非 env 注入）。
// 与自动发现一致地优先用厂商标记判定：定位单元文件、确认带 OPENCLAW_SERVICE_MARKER=openclaw
// 且 KIND=gateway。只有在单元文件不可定位（例如来自状态文件、单元已被删除）时，才回退到按
// 单元名前缀 "openclaw" 判定。这样即使某个带标记的 openclaw 网关单元改了名，也不会被误路由到
// 对它无效的 TELEGRAM_* env 注入。
func (a *App) isOpenClawTarget(t systemdTargetName) bool {
	if path, ok := a.locateTelegramUnitFile(t); ok {
		if content, err := readTelegramUnitContent(path); err == nil {
			return unitHasOpenClawGatewayMarker(content)
		}
	}
	return strings.HasPrefix(t.Service, "openclaw")
}

// locateTelegramUnitFile 定位一个 telegram 目标对应的 systemd 单元文件。
func (a *App) locateTelegramUnitFile(t systemdTargetName) (string, bool) {
	if t.UserMode {
		home, err := userHomeDir(t.User)
		if err != nil || strings.TrimSpace(home) == "" {
			return "", false
		}
		p := filepath.Join(home, ".config/systemd/user", t.Service)
		if fileExists(p) {
			return p, true
		}
		return "", false
	}
	for _, root := range []string{"/etc/systemd/system", "/usr/local/lib/systemd/system", "/lib/systemd/system", "/usr/lib/systemd/system"} {
		p := filepath.Join(root, t.Service)
		if fileExists(p) {
			return p, true
		}
	}
	return "", false
}

// systemUnitExists 报告某个系统级单元文件是否存在于 systemd 的系统单元搜索目录中。
func systemUnitExists(service string) bool {
	for _, root := range []string{"/etc/systemd/system", "/usr/local/lib/systemd/system", "/lib/systemd/system", "/usr/lib/systemd/system"} {
		if fileExists(filepath.Join(root, service)) {
			return true
		}
	}
	return false
}

// openClawConfigPath 返回指定用户的 openclaw 配置文件默认路径。
func (a *App) openClawConfigPath(user string) (string, error) {
	home, err := userHomeDir(user)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(home) == "" {
		return "", fmt.Errorf("用户 %s 没有可用家目录，无法定位 openclaw 配置", user)
	}
	return filepath.Join(home, ".openclaw", "openclaw.json"), nil
}

// applyOpenClawTelegramProxy 把目标用户的 openclaw 配置 channels.telegram.proxy 设为 proxyURL。
// 返回是否发生了改动（用于决定是否需要重启 openclaw 以重载配置）。配置不存在则跳过。
func (a *App) applyOpenClawTelegramProxy(user, proxyURL string) (bool, error) {
	path, err := a.openClawConfigPath(user)
	if err != nil {
		return false, err
	}
	raw, err := readUserFileNoFollow(user, path, maxOpenClawConfigBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Printf("警告：未找到用户 %s 的 openclaw 配置 %s，跳过 Telegram 代理设置（请确认 openclaw 已安装）\n", user, path)
			return false, nil
		}
		return false, err
	}
	out, changed, err := setOpenClawTelegramProxy(raw, proxyURL)
	if err != nil {
		return false, fmt.Errorf("解析 openclaw 配置失败（%s）：%w", path, err)
	}
	if !changed {
		return false, nil
	}
	if err := writeUserFileAtomic(user, path, out, 0o600); err != nil {
		return false, fmt.Errorf("写入 openclaw 配置失败（%s）：%w", path, err)
	}
	fmt.Printf("已设置用户 %s 的 openclaw channels.telegram.proxy=%s\n", user, proxyURL)
	return true, nil
}

// restoreOpenClawTelegramProxy 在关闭场景时移除我们设置的 channels.telegram.proxy。
// 仅当当前值仍等于 proxyURL（我们设的值）时才删除，避免覆盖用户在开启期间手动改成的其它值。
// 返回是否发生了改动。配置不存在则视为无需处理。
func (a *App) restoreOpenClawTelegramProxy(user, proxyURL string) (bool, error) {
	path, err := a.openClawConfigPath(user)
	if err != nil {
		return false, err
	}
	raw, err := readUserFileNoFollow(user, path, maxOpenClawConfigBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	out, changed, err := clearOpenClawTelegramProxy(raw, proxyURL)
	if err != nil {
		return false, fmt.Errorf("解析 openclaw 配置失败（%s）：%w", path, err)
	}
	if !changed {
		return false, nil
	}
	if err := writeUserFileAtomic(user, path, out, 0o600); err != nil {
		return false, fmt.Errorf("写入 openclaw 配置失败（%s）：%w", path, err)
	}
	fmt.Printf("已移除用户 %s 的 openclaw channels.telegram.proxy（场景已关闭）\n", user)
	return true, nil
}

// --- 纯函数：openclaw 配置 JSON 的读改（与文件/用户无关，便于测试） ---

// setOpenClawTelegramProxy 设置 channels.telegram.proxy=proxyURL，保留其余字段；
// 若已是该值则不改（changed=false，返回原始 raw）。
func setOpenClawTelegramProxy(raw []byte, proxyURL string) (out []byte, changed bool, err error) {
	var cfg map[string]any
	if err = json.Unmarshal(raw, &cfg); err != nil {
		return nil, false, err
	}
	if cur, _ := getJSONString(cfg, "channels", "telegram", "proxy"); cur == proxyURL {
		return raw, false, nil
	}
	setJSONString(cfg, proxyURL, "channels", "telegram", "proxy")
	out, err = marshalOpenClawConfig(cfg)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// clearOpenClawTelegramProxy 仅当 channels.telegram.proxy 仍等于 proxyURL（我们设的值）时删除它，
// 避免覆盖用户后来手动改成的其它值。否则不改（changed=false，返回原始 raw）。
func clearOpenClawTelegramProxy(raw []byte, proxyURL string) (out []byte, changed bool, err error) {
	var cfg map[string]any
	if err = json.Unmarshal(raw, &cfg); err != nil {
		return nil, false, err
	}
	cur, ok := getJSONString(cfg, "channels", "telegram", "proxy")
	if !ok || cur != proxyURL {
		return raw, false, nil
	}
	deleteJSONKey(cfg, "channels", "telegram", "proxy")
	out, err = marshalOpenClawConfig(cfg)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func marshalOpenClawConfig(cfg map[string]any) ([]byte, error) {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func getJSONString(m map[string]any, keys ...string) (string, bool) {
	cur := m
	for i, k := range keys {
		v, ok := cur[k]
		if !ok {
			return "", false
		}
		if i == len(keys)-1 {
			s, ok := v.(string)
			return s, ok
		}
		next, ok := v.(map[string]any)
		if !ok {
			return "", false
		}
		cur = next
	}
	return "", false
}

func setJSONString(m map[string]any, value string, keys ...string) {
	cur := m
	for i, k := range keys {
		if i == len(keys)-1 {
			cur[k] = value
			return
		}
		next, ok := cur[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[k] = next
		}
		cur = next
	}
}

func deleteJSONKey(m map[string]any, keys ...string) {
	cur := m
	for i, k := range keys {
		if i == len(keys)-1 {
			delete(cur, k)
			return
		}
		next, ok := cur[k].(map[string]any)
		if !ok {
			return
		}
		cur = next
	}
}
