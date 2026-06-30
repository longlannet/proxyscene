package manager

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxTelegramUnitReadBytes int64 = 64 << 10

type localUserAccount struct {
	Name string
	Home string
}

func (a *App) telegramTargets(st *Store, includeStored bool) ([]systemdTargetName, error) {
	names := a.telegramTargetNames(st, includeStored)
	seen := map[string]bool{}
	targets := make([]systemdTargetName, 0, len(names))
	for _, name := range names {
		var err error
		targets, err = appendTelegramTargetName(targets, seen, name)
		if err != nil {
			return nil, err
		}
	}
	return targets, nil
}

func (a *App) telegramTargetsBestEffort(st *Store, includeStored bool) ([]systemdTargetName, []error) {
	names := a.telegramTargetNames(st, includeStored)
	seen := map[string]bool{}
	targets := make([]systemdTargetName, 0, len(names))
	errs := []error{}
	for _, name := range names {
		var err error
		targets, err = appendTelegramTargetName(targets, seen, name)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return targets, errs
}

func (a *App) telegramTargetNames(st *Store, includeStored bool) []string {
	names := []string{}
	names = append(names, a.cfg.TGTargetServices...)
	names = append(names, discoverTelegramTargetNames()...)
	if includeStored && st != nil {
		names = append(names, st.TelegramTargets...)
	}
	return names
}

func appendTelegramTargetName(targets []systemdTargetName, seen map[string]bool, name string) ([]systemdTargetName, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return targets, nil
	}
	target, err := parseSystemdTargetName(name)
	if err != nil {
		return targets, err
	}
	key := canonicalTelegramTargetName(target)
	if seen[key] {
		return targets, nil
	}
	seen[key] = true
	return append(targets, target), nil
}

func canonicalTelegramTargetName(target systemdTargetName) string {
	if target.UserMode {
		return "user:" + target.User + ":" + target.Service
	}
	return target.Service
}

func canonicalTelegramTargetNames(targets []systemdTargetName) []string {
	values := make([]string, 0, len(targets))
	for _, target := range targets {
		values = appendUniqueString(values, canonicalTelegramTargetName(target))
	}
	return values
}

func discoverTelegramTargetNames() []string {
	values := []string{}
	values = append(values, discoverSystemTelegramTargetNames()...)
	values = append(values, discoverUserTelegramTargetNames()...)
	return values
}

func discoverSystemTelegramTargetNames() []string {
	// /usr/local/lib/systemd/system 是本地（非发行版）系统单元的标准安装位置，
	// 一并扫描，避免装在该处的网关被漏检。
	roots := []string{"/etc/systemd/system", "/usr/local/lib/systemd/system", "/lib/systemd/system", "/usr/lib/systemd/system"}
	values := []string{}
	seen := map[string]bool{}
	for _, root := range roots {
		walkTelegramUnitFiles(root, func(path, service string) {
			if !isTelegramRelatedUnit(path, service) || seen[service] {
				return
			}
			seen[service] = true
			values = append(values, service)
		})
	}
	sort.Strings(values)
	return values
}

func discoverUserTelegramTargetNames() []string {
	values := []string{}
	seen := map[string]bool{}
	for _, account := range listLocalUserAccounts() {
		if err := ensureUserHomeUsable(account.Home, account.Name); err != nil {
			continue
		}
		root := filepath.Join(account.Home, ".config/systemd/user")
		walkTelegramUnitFiles(root, func(path, service string) {
			if !isTelegramRelatedUnit(path, service) {
				return
			}
			name := "user:" + account.Name + ":" + service
			if seen[name] {
				return
			}
			seen[name] = true
			values = append(values, name)
		})
	}
	sort.Strings(values)
	return values
}

func walkTelegramUnitFiles(root string, visit func(path, service string)) {
	if root == "" {
		return
	}
	if _, err := os.Stat(root); err != nil {
		return
	}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Type()&os.ModeSymlink != 0 {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".service") {
			return nil
		}
		if err := safeSystemdServiceName(name); err != nil {
			return nil
		}
		visit(path, name)
		return nil
	})
}

// isTelegramRelatedUnit 判定一个 systemd 单元是否为「需要注入 Telegram 代理」的客户端网关。
// 采用精确识别而非泛关键字子串，避免把名字里碰巧含 openclaw/hermes 的无关单元误判进来
// （例如 openclaw-xhigh-guard.service 这种仅文件名带 openclaw 的守护单元）：
//   - OpenClaw：要求带厂商标记 Environment=OPENCLAW_SERVICE_MARKER=openclaw 且
//     OPENCLAW_SERVICE_KIND=gateway——只命中网关，排除 guard 与 node 等非 Telegram 角色。
//   - Hermes：无专用 env 标记，按单元名 hermes-gateway[-<profile>] 或 ExecStart 调用
//     hermes_cli / hermes-agent 的 gateway 子命令来识别（涵盖 profile 实例单元）。
func isTelegramRelatedUnit(path, service string) bool {
	if isHermesGatewayUnitName(service) {
		return true
	}
	content, err := readTelegramUnitContent(path)
	if err != nil {
		return false
	}
	return unitLooksLikeTelegramClient(content)
}

// isHermesGatewayUnitName 匹配 hermes 网关单元名：默认 profile 的 hermes-gateway.service，
// 以及命名 profile 实例 hermes-gateway-<profile>.service（见 hermes 的 _SERVICE_BASE 规律）。
func isHermesGatewayUnitName(service string) bool {
	name := strings.TrimSuffix(service, ".service")
	return name == "hermes-gateway" || strings.HasPrefix(name, "hermes-gateway-")
}

// unitLooksLikeTelegramClient 解析单元内容，按 OpenClaw 厂商标记或 Hermes 程序身份判定。
func unitLooksLikeTelegramClient(content string) bool {
	return unitHasOpenClawGatewayMarker(content) || unitIsHermesGatewayExec(content)
}

// unitHasOpenClawGatewayMarker 报告单元是否带 OpenClaw 网关的厂商标记
// （Environment=OPENCLAW_SERVICE_MARKER=openclaw 且 OPENCLAW_SERVICE_KIND=gateway）。
// 该标记是 OpenClaw 网关稳定、机器可读的身份信号，检测与 openclaw 路由都据此判定。
func unitHasOpenClawGatewayMarker(content string) bool {
	marker, gateway := false, false
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		// Environment 行可能形如 Environment=OPENCLAW_SERVICE_MARKER=openclaw 或带引号、
		// 多个赋值同行，子串判定对这些写法都成立。
		if strings.EqualFold(strings.TrimSpace(key), "environment") {
			if strings.Contains(value, "OPENCLAW_SERVICE_MARKER=openclaw") {
				marker = true
			}
			if strings.Contains(value, "OPENCLAW_SERVICE_KIND=gateway") {
				gateway = true
			}
		}
	}
	return marker && gateway
}

// unitIsHermesGatewayExec 报告单元的 ExecStart 是否调用 hermes_cli/hermes-agent 的 gateway 子命令。
func unitIsHermesGatewayExec(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(key), "execstart") {
			es := strings.ToLower(value)
			if (strings.Contains(es, "hermes_cli") || strings.Contains(es, "hermes-agent")) && strings.Contains(es, "gateway") {
				return true
			}
		}
	}
	return false
}

func readTelegramUnitContent(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxTelegramUnitReadBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(b)) > maxTelegramUnitReadBytes {
		b = b[:maxTelegramUnitReadBytes]
	}
	return string(b), nil
}

func listLocalUserAccounts() []localUserAccount {
	b, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return nil
	}
	accounts := []localUserAccount{}
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 6 {
			continue
		}
		name := fields[0]
		home := fields[5]
		if validateUserName(name) != nil || home == "" || !filepath.IsAbs(home) {
			continue
		}
		accounts = append(accounts, localUserAccount{Name: name, Home: home})
	}
	return accounts
}
