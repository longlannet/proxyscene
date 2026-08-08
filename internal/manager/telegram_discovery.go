package manager

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxTelegramUnitReadBytes int64 = 64 << 10
const maxEffectiveTelegramUnitBytes = 1 << 20

type localUserAccount struct {
	Name string
	Home string
	UID  string
}

func (a *App) telegramTargets(st *Store, includeStored bool) ([]systemdTargetName, error) {
	names, err := a.telegramTargetNames(st, includeStored)
	if err != nil {
		return nil, err
	}
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

func (a *App) telegramTargetNames(st *Store, includeStored bool) ([]string, error) {
	names := []string{}
	names = append(names, a.cfg.TGTargetServices...)
	discovered, err := discoverTelegramTargetNames()
	names = append(names, discovered...)
	if includeStored && st != nil {
		names = append(names, st.TelegramTargets...)
	}
	return names, err
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

func discoverTelegramTargetNames() ([]string, error) {
	values := []string{}
	systemValues, systemErr := discoverSystemTelegramTargetNames()
	userValues, userErr := discoverUserTelegramTargetNames()
	values = append(values, systemValues...)
	values = append(values, userValues...)
	return values, errors.Join(systemErr, userErr)
}

func systemUnitSearchRoots() []string {
	// Matches systemd's precedence closely enough to honor administrator overrides,
	// runtime units, transient units, generators, masks, and vendor fallbacks.
	return []string{
		"/etc/systemd/system.control",
		"/run/systemd/system.control",
		"/run/systemd/transient",
		"/run/systemd/generator.early",
		"/etc/systemd/system",
		"/etc/systemd/system.attached",
		"/run/systemd/system",
		"/run/systemd/system.attached",
		"/run/systemd/generator",
		"/usr/local/lib/systemd/system",
		"/lib/systemd/system",
		"/usr/lib/systemd/system",
		"/run/systemd/generator.late",
	}
}

type unitSearchRoot struct {
	Path   string
	Manage bool
}

func discoverSystemTelegramTargetNames() ([]string, error) {
	roots := make([]unitSearchRoot, 0, len(systemUnitSearchRoots()))
	for _, root := range systemUnitSearchRoots() {
		roots = append(roots, unitSearchRoot{Path: root, Manage: true})
	}
	return discoverEffectiveTelegramUnits(roots, "")
}

func discoverUserTelegramTargetNames() ([]string, error) {
	accounts, accountErr := listLocalUserAccounts()
	if accountErr != nil {
		return nil, accountErr
	}
	return discoverUserTelegramTargetNamesFor(accounts)
}

func discoverUserTelegramTargetNamesFor(accounts []localUserAccount) ([]string, error) {
	values := []string{}
	seen := map[string]bool{}
	var errs []error
	for _, account := range accounts {
		if err := ensureUserHomeUsable(account.Home, account.Name); err != nil {
			// /etc/passwd normally contains system accounts whose home is /,
			// /nonexistent, or a symlink such as /bin. They are not candidates
			// for private user units and must not make unrelated scene cleanup
			// or uninstall fail. Explicit and stored user targets still go
			// through the strict home validation in their management paths.
			continue
		}
		services, err := discoverEffectiveTelegramUnits(userUnitSearchRootSpecsFor(account.Home, account.UID), "")
		if err != nil {
			errs = append(errs, fmt.Errorf("扫描用户 %s 的 systemd 单元失败：%w", account.Name, err))
			continue
		}
		for _, service := range services {
			name := "user:" + account.Name + ":" + service
			if !seen[name] {
				seen[name] = true
				values = append(values, name)
			}
		}
	}
	sort.Strings(values)
	return values, errors.Join(errs...)
}

func privateUserUnitRoots(home string) []string {
	return []string{
		filepath.Join(home, ".config/systemd/user"),
		filepath.Join(home, ".local/share/systemd/user"),
	}
}

func globalUserUnitRoots() []string {
	return []string{"/etc/xdg/systemd/user", "/etc/systemd/user", "/run/systemd/user", "/usr/local/lib/systemd/user", "/usr/local/share/systemd/user", "/lib/systemd/user", "/usr/lib/systemd/user", "/usr/share/systemd/user"}
}

func userUnitSearchRootSpecsFor(home, uid string) []unitSearchRoot {
	private := privateUserUnitRoots(home)
	roots := []unitSearchRoot{
		{Path: filepath.Join(home, ".config/systemd/user.control"), Manage: true},
	}
	if uid != "" {
		runtime := filepath.Join("/run/user", uid, "systemd")
		roots = append(roots,
			unitSearchRoot{Path: filepath.Join(runtime, "user.control"), Manage: true},
			unitSearchRoot{Path: filepath.Join(runtime, "transient"), Manage: true},
			unitSearchRoot{Path: filepath.Join(runtime, "generator.early"), Manage: true},
		)
	}
	roots = append(roots,
		unitSearchRoot{Path: private[0], Manage: true},
		unitSearchRoot{Path: "/etc/xdg/systemd/user"},
		unitSearchRoot{Path: "/etc/systemd/user"},
	)
	if uid != "" {
		runtime := filepath.Join("/run/user", uid, "systemd")
		roots = append(roots, unitSearchRoot{Path: filepath.Join(runtime, "user"), Manage: true})
	}
	roots = append(roots,
		unitSearchRoot{Path: "/run/systemd/user"},
		unitSearchRoot{Path: private[1], Manage: true},
		unitSearchRoot{Path: "/usr/local/lib/systemd/user"},
		unitSearchRoot{Path: "/usr/local/share/systemd/user"},
		unitSearchRoot{Path: "/lib/systemd/user"},
		unitSearchRoot{Path: "/usr/lib/systemd/user"},
		unitSearchRoot{Path: "/usr/share/systemd/user"},
	)
	if uid != "" {
		runtime := filepath.Join("/run/user", uid, "systemd")
		roots = append(roots,
			unitSearchRoot{Path: filepath.Join(runtime, "generator"), Manage: true},
			unitSearchRoot{Path: filepath.Join(runtime, "generator.late"), Manage: true},
		)
	}
	return roots
}

func unitFileExistsInRoots(service string, roots []string) bool {
	service = normalizeSystemdServiceName(service)
	_, exists := effectiveUnitPathInRoots(service, roots)
	return exists
}

func userUnitExists(userName, service string) bool {
	identity, err := lookupLocalUserIdentity(userName)
	if err != nil || ensureUserHomeUsable(identity.Home, userName) != nil {
		return false
	}
	roots := []string{}
	for _, root := range userUnitSearchRootSpecsFor(identity.Home, identity.UIDText) {
		roots = append(roots, root.Path)
	}
	return unitFileExistsInRoots(service, roots)
}

func discoverEffectiveTelegramUnits(roots []unitSearchRoot, prefix string) ([]string, error) {
	values := []string{}
	seen := map[string]bool{}
	seenFragments := map[string]bool{}
	var errs []error
	rootPaths := make([]string, 0, len(roots))
	for _, root := range roots {
		rootPaths = append(rootPaths, root.Path)
	}
	for _, root := range roots {
		entries, err := os.ReadDir(root.Path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("读取 systemd 单元目录 %s 失败：%w", root.Path, err))
			continue
		}
		for _, entry := range entries {
			service := entry.Name()
			if !strings.HasSuffix(service, ".service") || safeSystemdServiceName(service) != nil || seen[service] {
				continue
			}
			// The first occurrence wins even when it is a mask, alias, malformed file,
			// or unrelated service. Lower-priority vendor files must never punch through
			// an administrator/runtime override.
			seen[service] = true
			entryPath := filepath.Join(root.Path, service)
			info, err := os.Lstat(entryPath)
			if err != nil {
				errs = append(errs, fmt.Errorf("读取 systemd 单元信息 %s 失败：%w", entryPath, err))
				continue
			}
			if !root.Manage {
				continue
			}
			path, usable := effectiveUnitPathInRoots(service, rootPaths)
			if !usable {
				if info.Mode()&os.ModeSymlink != 0 {
					target, linkErr := filepath.EvalSymlinks(entryPath)
					if linkErr == nil && filepath.Clean(target) == "/dev/null" {
						continue
					}
					errs = append(errs, fmt.Errorf("systemd 单元别名无法解析为普通文件：%s", entryPath))
					continue
				}
				errs = append(errs, fmt.Errorf("systemd 单元不是普通文件：%s", entryPath))
				continue
			}
			fragmentIdentity := effectiveUnitFragmentIdentity(path, service)
			if seenFragments[fragmentIdentity] {
				continue
			}
			seenFragments[fragmentIdentity] = true
			content, err := readTelegramUnitContentWithDropIns(path, service, roots)
			if err != nil {
				errs = append(errs, fmt.Errorf("读取 systemd 单元 %s 失败：%w", path, err))
				continue
			}
			if unitLooksLikeTelegramClient(content) {
				discoveredService := canonicalUnitNameForFragment(path, service)
				if canonicalPath, exists := effectiveUnitPathInRoots(discoveredService, rootPaths); !exists || !sameUnitFragment(canonicalPath, path) {
					discoveredService = service
				}
				values = append(values, prefix+discoveredService)
			}
		}
	}
	sort.Strings(values)
	return values, errors.Join(errs...)
}

func effectiveUnitFragmentIdentity(path, requested string) string {
	_, instance, kind := splitServiceUnitName(normalizeSystemdServiceName(requested))
	if kind == serviceUnitInstance {
		return filepath.Clean(path) + "\x00" + instance
	}
	return filepath.Clean(path) + "\x00"
}

func effectiveUnitPathInRoots(service string, roots []string) (string, bool) {
	service = normalizeSystemdServiceName(service)
	path, _, usable := resolveEffectiveUnitPathInRoots(service, roots, map[string]bool{})
	return path, usable
}

func resolveEffectiveUnitPathInRoots(service string, roots []string, resolving map[string]bool) (string, bool, bool) {
	path, found, usable := effectiveUnitPathExactInRoots(service, roots, resolving)
	if found {
		return path, true, usable
	}
	template, _, kind := splitServiceUnitName(service)
	if kind != serviceUnitInstance {
		return "", false, false
	}
	return effectiveUnitPathExactInRoots(template, roots, resolving)
}

func effectiveUnitPathExactInRoots(service string, roots []string, resolving map[string]bool) (string, bool, bool) {
	if resolving[service] {
		return "", true, false
	}
	resolving[service] = true
	defer delete(resolving, service)

	for _, root := range roots {
		path := filepath.Join(root, service)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", true, false
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return "", true, false
			}
			target := link
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(path), target)
			}
			target = filepath.Clean(target)
			if target == "/dev/null" {
				return "", true, false
			}
			if targetName, ok := unitNameInSearchRoots(target, roots); ok {
				if targetName != service {
					resolved, _, usable := resolveEffectiveUnitPathInRoots(targetName, roots, resolving)
					return resolved, true, usable
				}
			}
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil || filepath.Clean(resolved) == "/dev/null" {
				return "", true, false
			}
			targetInfo, err := os.Stat(resolved)
			if err != nil || !targetInfo.Mode().IsRegular() {
				return "", true, false
			}
			return resolved, true, true
		}
		if !info.Mode().IsRegular() {
			return "", true, false
		}
		return path, true, true
	}
	return "", false, false
}

func unitNameInSearchRoots(path string, roots []string) (string, bool) {
	clean := filepath.Clean(path)
	for _, root := range roots {
		if filepath.Dir(clean) != filepath.Clean(root) {
			continue
		}
		name := filepath.Base(clean)
		if safeSystemdServiceName(name) == nil {
			return name, true
		}
	}
	return "", false
}

func walkTelegramUnitFiles(root string, visit func(path, service string) error) error {
	if root == "" {
		return nil
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".service") {
			continue
		}
		if err := safeSystemdServiceName(name); err != nil {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if err := visit(filepath.Join(root, name), name); err != nil {
			return err
		}
	}
	return nil
}

// 采用精确识别而非泛关键字子串，避免把名字里碰巧含 openclaw/hermes 的无关单元误判进来
// （例如 openclaw-xhigh-guard.service 这种仅文件名带 openclaw 的守护单元）：
//   - OpenClaw：要求带厂商标记 Environment=OPENCLAW_SERVICE_MARKER=openclaw 且
//     OPENCLAW_SERVICE_KIND=gateway——只命中网关，排除 guard 与 node 等非 Telegram 角色。
//   - Hermes：无专用 env 标记，按单元名 hermes-gateway[-<profile>] 或 ExecStart 调用
//     hermes_cli / hermes-agent 的 gateway 子命令来识别（涵盖 profile 实例单元）。
func telegramRelatedUnit(path, service string) (bool, error) {
	content, err := readTelegramUnitContent(path)
	if err != nil {
		return false, err
	}
	return unitLooksLikeTelegramClient(content), nil
}

// unitLooksLikeTelegramClient 解析单元内容，按 OpenClaw 厂商标记或 Hermes 程序身份判定。
func unitLooksLikeTelegramClient(content string) bool {
	return unitHasOpenClawGatewayMarker(content) || unitIsHermesGatewayExec(content)
}

// unitHasOpenClawGatewayMarker 报告单元是否带 OpenClaw 网关的厂商标记
// （Environment=OPENCLAW_SERVICE_MARKER=openclaw 且 OPENCLAW_SERVICE_KIND=gateway）。
// 该标记是 OpenClaw 网关稳定、机器可读的身份信号，检测与 openclaw 路由都据此判定。
func unitHasOpenClawGatewayMarker(content string) bool {
	environment, err := effectiveServiceEnvironment(content)
	if err != nil {
		return false
	}
	return environment["OPENCLAW_SERVICE_MARKER"] == "openclaw" && environment["OPENCLAW_SERVICE_KIND"] == "gateway"
}

func effectiveServiceEnvironment(content string) (map[string]string, error) {
	return effectiveServiceEnvironmentWithBase(content, nil)
}

func effectiveServiceEnvironmentWithBase(content string, inherited map[string]string) (map[string]string, error) {
	declared := map[string]string{}
	unsetNames := map[string]bool{}
	unsetAssignments := map[string]map[string]bool{}
	section := ""
	for _, line := range systemdLogicalLines(content) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			continue
		}
		if section != "Service" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Environment":
			words, err := splitSystemdWords(value)
			if err != nil {
				return nil, err
			}
			if len(words) == 0 {
				declared = map[string]string{}
				continue
			}
			for _, assign := range words {
				name, v, ok := strings.Cut(assign, "=")
				if ok && name != "" {
					declared[name] = v
				}
			}
		case "UnsetEnvironment":
			words, err := splitSystemdWords(value)
			if err != nil {
				return nil, err
			}
			if len(words) == 0 {
				unsetNames = map[string]bool{}
				unsetAssignments = map[string]map[string]bool{}
				continue
			}
			for _, word := range words {
				name, assignment, exact := strings.Cut(word, "=")
				if name == "" {
					continue
				}
				if exact {
					if unsetAssignments[name] == nil {
						unsetAssignments[name] = map[string]bool{}
					}
					unsetAssignments[name][assignment] = true
				} else {
					unsetNames[name] = true
				}
			}
		}
	}
	environment := make(map[string]string, len(inherited)+len(declared))
	for name, value := range inherited {
		environment[name] = value
	}
	for name, value := range declared {
		environment[name] = value
	}
	for name := range unsetNames {
		delete(environment, name)
	}
	for name, values := range unsetAssignments {
		if values[environment[name]] {
			delete(environment, name)
		}
	}
	return environment, nil
}

// unitIsHermesGatewayExec 报告单元的 ExecStart 是否调用 hermes_cli/hermes-agent 的 gateway 子命令。
func unitIsHermesGatewayExec(content string) bool {
	execStarts, err := effectiveServiceExecStarts(content)
	if err != nil {
		return false
	}
	for _, words := range execStarts {
		if hermesGatewayArgv(words) {
			return true
		}
	}
	return false
}

func effectiveServiceExecStarts(content string) ([][]string, error) {
	section := ""
	execStarts := [][]string{}
	for _, line := range systemdLogicalLines(content) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			continue
		}
		if section != "Service" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "ExecStart" {
			continue
		}
		words, err := splitSystemdWords(value)
		if err != nil {
			return nil, err
		}
		if len(words) == 0 {
			execStarts = nil
			continue
		}
		execStarts = append(execStarts, words)
	}
	return execStarts, nil
}

func systemdLogicalLines(content string) []string {
	physical := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	logical := []string{}
	current := ""
	for _, line := range physical {
		trimmedLeft := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmedLeft, "#") || strings.HasPrefix(trimmedLeft, ";") {
			// systemd ignores complete comment lines, including comment lines
			// between the two physical parts of a continued logical line. A
			// trailing backslash in a comment never continues that comment.
			continue
		}
		trimmed := line
		continued := strings.HasSuffix(line, "\\")
		if continued {
			trimmed = strings.TrimSuffix(trimmed, "\\")
		}
		current += trimmed
		if continued {
			current += " "
			continue
		}
		logical = append(logical, current)
		current = ""
	}
	if current != "" {
		logical = append(logical, current)
	}
	return logical
}

func splitSystemdWords(value string) ([]string, error) {
	words := []string{}
	var current strings.Builder
	quote := rune(0)
	started := false
	flush := func() {
		if started {
			words = append(words, current.String())
			current.Reset()
			started = false
		}
	}
	for i := 0; i < len(value); {
		r, size := utf8.DecodeRuneInString(value[i:])
		if r == utf8.RuneError && size == 1 {
			return nil, fmt.Errorf("systemd 指令含无效 UTF-8")
		}
		if r == '\\' {
			decoded, consumed, err := decodeSystemdEscape(value[i:])
			if err != nil {
				return nil, err
			}
			current.WriteRune(decoded)
			started = true
			i += consumed
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
			started = true
			i += size
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
			started = true
		case ' ', '\t':
			flush()
		default:
			current.WriteRune(r)
			started = true
		}
		i += size
	}
	if quote != 0 {
		return nil, fmt.Errorf("systemd 指令引号或转义不完整")
	}
	flush()
	return words, nil
}

func decodeSystemdEscape(value string) (rune, int, error) {
	if len(value) < 2 || value[0] != '\\' {
		return 0, 0, fmt.Errorf("systemd 指令转义不完整")
	}
	switch value[1] {
	case 'a':
		return '\a', 2, nil
	case 'b':
		return '\b', 2, nil
	case 'f':
		return '\f', 2, nil
	case 'n':
		return '\n', 2, nil
	case 'r':
		return '\r', 2, nil
	case 's':
		return ' ', 2, nil
	case 't':
		return '\t', 2, nil
	case 'v':
		return '\v', 2, nil
	case '\\', '\'', '"':
		return rune(value[1]), 2, nil
	case 'x':
		return decodeSystemdHexEscape(value, 2)
	case 'u':
		return decodeSystemdHexEscape(value, 4)
	case 'U':
		return decodeSystemdHexEscape(value, 8)
	default:
		if value[1] >= '0' && value[1] <= '7' {
			return decodeSystemdOctalEscape(value)
		}
		return 0, 0, fmt.Errorf("systemd 指令含不支持的转义 \\%c", value[1])
	}
}

func decodeSystemdHexEscape(value string, digits int) (rune, int, error) {
	end := 2 + digits
	if len(value) < end {
		return 0, 0, fmt.Errorf("systemd 十六进制转义不完整")
	}
	decoded, err := strconv.ParseUint(value[2:end], 16, 32)
	if err != nil || decoded == 0 || !utf8.ValidRune(rune(decoded)) {
		return 0, 0, fmt.Errorf("systemd 十六进制转义无效")
	}
	return rune(decoded), end, nil
}

func decodeSystemdOctalEscape(value string) (rune, int, error) {
	const end = 4
	if len(value) < end {
		return 0, 0, fmt.Errorf("systemd 八进制转义不完整")
	}
	for _, digit := range value[1:end] {
		if digit < '0' || digit > '7' {
			return 0, 0, fmt.Errorf("systemd 八进制转义无效")
		}
	}
	decoded, err := strconv.ParseUint(value[1:end], 8, 8)
	if err != nil || decoded == 0 {
		return 0, 0, fmt.Errorf("systemd 八进制转义无效")
	}
	return rune(decoded), end, nil
}

func hermesGatewayArgv(words []string) bool {
	if len(words) < 3 {
		return false
	}
	command, ok := systemdDirectExecCommand(words[0])
	if !ok {
		return false
	}
	base := filepath.Base(command)
	if base == "hermes_cli" || base == "hermes-agent" {
		return words[1] == "gateway" && words[2] == "run"
	}
	if !isPythonInterpreter(base) || len(words) < 5 || words[1] != "-m" {
		return false
	}
	module := words[2]
	return (module == "hermes_cli" || module == "hermes_cli.main") &&
		words[3] == "gateway" && words[4] == "run"
}

func systemdDirectExecCommand(word string) (string, bool) {
	seenDash := false
	seenColon := false
	for len(word) > 0 {
		switch word[0] {
		case '-':
			if seenDash {
				return "", false
			}
			seenDash = true
			word = word[1:]
		case ':':
			if seenColon {
				return "", false
			}
			seenColon = true
			word = word[1:]
		case '@', '+', '!':
			// These prefixes alter argv[0] or the execution credentials. The
			// runtime-path proof deliberately does not model those variants.
			return "", false
		default:
			return word, true
		}
	}
	return "", false
}

func isPythonInterpreter(base string) bool {
	if base == "python" {
		return true
	}
	version := strings.TrimPrefix(base, "python")
	if version == base || version == "" || version[0] == '.' || version[len(version)-1] == '.' {
		return false
	}
	seenDigit := false
	previousDot := false
	for _, c := range version {
		if c >= '0' && c <= '9' {
			seenDigit = true
			previousDot = false
			continue
		}
		if c != '.' || previousDot {
			return false
		}
		previousDot = true
	}
	return seenDigit
}

func readTelegramUnitContent(path string) (string, error) {
	b, err := readRegularFileNoFollow(path, maxTelegramUnitReadBytes)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func readTelegramUnitContentWithDropIns(path, service string, roots []unitSearchRoot) (string, error) {
	content, err := readTelegramUnitContent(path)
	if err != nil {
		return "", err
	}
	names, err := effectiveDropInUnitNames(path, service, roots)
	if err != nil {
		return "", err
	}
	selected := map[string]string{}
	seenFiles := map[string]bool{}
	seenDirs := map[string]bool{}
	for _, dir := range effectiveDropInSearchDirs(names, roots) {
		if seenDirs[dir] {
			continue
		}
		seenDirs[dir] = true
		info, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("systemd drop-in 路径不是普通目录：%s", dir)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return "", err
		}
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasSuffix(name, ".conf") || seenFiles[name] {
				continue
			}
			// systemd chooses one file per basename before parsing the selected files
			// lexicographically. A /dev/null symlink therefore masks lower-priority files.
			seenFiles[name] = true
			dropInPath := filepath.Join(dir, name)
			entryInfo, err := os.Lstat(dropInPath)
			if err != nil {
				return "", err
			}
			if entryInfo.Mode()&os.ModeSymlink != 0 {
				target, err := filepath.EvalSymlinks(dropInPath)
				if err != nil {
					return "", err
				}
				if filepath.Clean(target) == "/dev/null" {
					continue
				}
				return "", fmt.Errorf("systemd drop-in 不能是符号链接：%s", dropInPath)
			}
			if !entryInfo.Mode().IsRegular() {
				return "", fmt.Errorf("systemd drop-in 不是普通文件：%s", dropInPath)
			}
			selected[name] = dropInPath
		}
	}
	dropInNames := make([]string, 0, len(selected))
	for name := range selected {
		dropInNames = append(dropInNames, name)
	}
	sort.Strings(dropInNames)
	var merged strings.Builder
	merged.WriteString(content)
	for _, name := range dropInNames {
		dropIn, err := readTelegramUnitContent(selected[name])
		if err != nil {
			return "", err
		}
		merged.WriteString("\n[Unit]\n")
		merged.WriteString(dropIn)
		if merged.Len() > maxEffectiveTelegramUnitBytes {
			return "", fmt.Errorf("systemd 单元及 drop-in 总大小超过 %d 字节", maxEffectiveTelegramUnitBytes)
		}
	}
	return merged.String(), nil
}

type serviceUnitNameKind uint8

const (
	serviceUnitInvalid serviceUnitNameKind = iota
	serviceUnitPlain
	serviceUnitTemplate
	serviceUnitInstance
)

func splitServiceUnitName(name string) (template, instance string, kind serviceUnitNameKind) {
	if !strings.HasSuffix(name, ".service") {
		return "", "", serviceUnitInvalid
	}
	stem := strings.TrimSuffix(name, ".service")
	at := strings.IndexByte(stem, '@')
	if at < 0 {
		return "", "", serviceUnitPlain
	}
	if at == 0 || strings.Contains(stem[at+1:], "@") {
		return "", "", serviceUnitInvalid
	}
	template = stem[:at] + "@.service"
	instance = stem[at+1:]
	if instance == "" {
		return template, "", serviceUnitTemplate
	}
	return template, instance, serviceUnitInstance
}

func instantiateServiceTemplate(template, instance string) (string, bool) {
	if _, _, kind := splitServiceUnitName(template); kind != serviceUnitTemplate || instance == "" {
		return "", false
	}
	return strings.TrimSuffix(template, "@.service") + "@" + instance + ".service", true
}

func unitNameForRequestedUnit(candidate, requested string) (string, bool) {
	_, requestedInstance, requestedKind := splitServiceUnitName(requested)
	candidateTemplate, candidateInstance, candidateKind := splitServiceUnitName(candidate)
	switch requestedKind {
	case serviceUnitPlain:
		return candidate, candidateKind == serviceUnitPlain
	case serviceUnitTemplate:
		return candidate, candidateKind == serviceUnitTemplate
	case serviceUnitInstance:
		switch candidateKind {
		case serviceUnitTemplate:
			return instantiateServiceTemplate(candidateTemplate, requestedInstance)
		case serviceUnitInstance:
			return candidate, candidateInstance == requestedInstance
		}
	}
	return "", false
}

func effectiveDropInUnitNames(path, requested string, roots []unitSearchRoot) ([]string, error) {
	requested = normalizeSystemdServiceName(requested)
	canonical := canonicalUnitNameForFragment(path, requested)
	names := []string{canonical}
	if requested != canonical {
		names = append(names, requested)
	}

	rootPaths := make([]string, 0, len(roots))
	for _, root := range roots {
		rootPaths = append(rootPaths, root.Path)
	}
	seenEntries := map[string]bool{}
	aliases := []string{}
	for _, root := range roots {
		entries, err := os.ReadDir(root.Path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			candidate := entry.Name()
			if seenEntries[candidate] || !strings.HasSuffix(candidate, ".service") || safeSystemdServiceName(candidate) != nil {
				continue
			}
			seenEntries[candidate] = true
			if entry.Type()&os.ModeSymlink == 0 {
				continue
			}
			adapted, ok := unitNameForRequestedUnit(candidate, requested)
			if !ok || adapted == canonical || adapted == requested {
				continue
			}
			candidatePath, exists := effectiveUnitPathInRoots(adapted, rootPaths)
			if !exists || !sameUnitFragment(candidatePath, path) {
				continue
			}
			aliases = appendUniqueString(aliases, adapted)
		}
	}
	sort.Strings(aliases)
	return append(names, aliases...), nil
}

func canonicalUnitNameForFragment(path, requested string) string {
	requested = normalizeSystemdServiceName(requested)
	canonical, ok := unitNameForRequestedUnit(filepath.Base(path), requested)
	if !ok || safeSystemdServiceName(canonical) != nil {
		return requested
	}
	return canonical
}

func sameUnitFragment(left, right string) bool {
	if filepath.Clean(left) == filepath.Clean(right) {
		return true
	}
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}

func effectiveDropInSearchDirs(names []string, roots []unitSearchRoot) []string {
	dirs := []string{}
	for _, name := range names {
		hierarchy := serviceDropInNameHierarchy(name)
		for _, root := range roots {
			for _, item := range hierarchy {
				dirs = append(dirs, filepath.Join(root.Path, item+".d"))
			}
		}
	}
	// Type-wide drop-ins are less specific than every canonical/alias hierarchy.
	for _, root := range roots {
		dirs = append(dirs, filepath.Join(root.Path, "service.d"))
	}
	return dirs
}

func serviceDropInNameHierarchy(name string) []string {
	names := []string{name}
	template, _, kind := splitServiceUnitName(name)
	if kind == serviceUnitInstance {
		names = appendUniqueString(names, template)
		for _, prefix := range serviceDashPrefixNames(template) {
			names = appendUniqueString(names, prefix)
		}
	}
	for _, prefix := range serviceDashPrefixNames(name) {
		names = appendUniqueString(names, prefix)
	}
	return names
}

func serviceDashPrefixNames(name string) []string {
	_, instance, kind := splitServiceUnitName(name)
	if kind == serviceUnitInvalid {
		return nil
	}
	stem := strings.TrimSuffix(name, ".service")
	prefix := stem
	if at := strings.IndexByte(stem, '@'); at >= 0 {
		prefix = stem[:at]
	}
	search := strings.TrimSuffix(prefix, "-")
	names := []string{}
	for {
		dash := strings.LastIndexByte(search, '-')
		if dash <= 0 {
			break
		}
		truncated := search[:dash+1]
		built := truncated + ".service"
		if kind == serviceUnitInstance {
			built = truncated + "@" + instance + ".service"
		}
		names = append(names, built)
		search = strings.TrimSuffix(truncated, "-")
	}
	return names
}

func listLocalUserAccounts() ([]localUserAccount, error) {
	b, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return nil, err
	}
	accounts := []localUserAccount{}
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		// 与 parsePasswdLine 一致要求完整的 7 字段记录。
		if len(fields) < 7 {
			continue
		}
		name := fields[0]
		uid := fields[2]
		home := fields[5]
		if _, err := strconv.ParseUint(uid, 10, 32); err != nil || validateUserName(name) != nil || home == "" || !filepath.IsAbs(home) {
			continue
		}
		accounts = append(accounts, localUserAccount{Name: name, Home: home, UID: uid})
	}
	return accounts, nil
}
