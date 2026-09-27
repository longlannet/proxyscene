package manager

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf16"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const maxOpenClawRuntimeEnvBytes int64 = 1 << 20
const maxSystemdManagerEnvironmentBytes = 1 << 20
const maxHermesRuntimeFileBytes int64 = 1 << 20
const maxHermesRuntimeConfigBytes int64 = 8 << 20

var (
	telegramOutputUserManagerEnvironment = func(user string, identity *persistedUserIdentity, lookup localUserIdentityLookup) (string, error) {
		return outputUserSystemctlQuietPersisted(user, identity, lookup, "show-environment")
	}
	telegramOutputSystemManagerEnvironment = func() (string, error) {
		return outputQuietLabel("读取 systemd manager 环境", "systemctl", "show-environment")
	}
)

var openClawConfigSelectionEnvKeys = map[string]bool{
	"ANDROID_DATA":           true,
	"HOME":                   true,
	"HOMEDRIVE":              true,
	"HOMEPATH":               true,
	"OPENCLAW_AGENT_DIR":     true,
	"OPENCLAW_CONFIG_PATH":   true,
	"OPENCLAW_CONTAINER":     true,
	"OPENCLAW_HOME":          true,
	"OPENCLAW_INCLUDE_ROOTS": true,
	"OPENCLAW_NIX_MODE":      true,
	"OPENCLAW_OAUTH_DIR":     true,
	"OPENCLAW_PACKAGE_DIR":   true,
	"OPENCLAW_PROFILE":       true,
	"OPENCLAW_STATE_DIR":     true,
	"OPENCLAW_TEST_FAST":     true,
	"OPENCLAW_WORKSPACE_DIR": true,
	"LD_AUDIT":               true,
	"LD_LIBRARY_PATH":        true,
	"LD_PRELOAD":             true,
	"NODE_OPTIONS":           true,
	"NODE_PATH":              true,
	"PI_CODING_AGENT_DIR":    true,
	"PREFIX":                 true,
	"USERPROFILE":            true,
}

var hermesRuntimeDotEnvKeys = map[string]bool{
	"GATEWAY_MULTIPLEX_PROFILES":           true,
	"HOME":                                 true,
	"HERMES_HOME":                          true,
	"HERMES_MANAGED_DIR":                   true,
	"HERMES_S6_SUPERVISED_CHILD":           true,
	"HERMES_TELEGRAM_DISABLE_FALLBACK_IPS": true,
	"LD_AUDIT":                             true,
	"LD_LIBRARY_PATH":                      true,
	"LD_PRELOAD":                           true,
	"NO_PROXY":                             true,
	"PYTEST_CURRENT_TEST":                  true,
	"PYTHONHOME":                           true,
	"PYTHONPATH":                           true,
	"PYTHONSAFEPATH":                       true,
	"TELEGRAM_FALLBACK_IPS":                true,
	"TELEGRAM_PROXY":                       true,
	"no_proxy":                             true,
}

var hermesCodeInjectionEnvKeys = map[string]bool{
	"LD_AUDIT":        true,
	"LD_LIBRARY_PATH": true,
	"LD_PRELOAD":      true,
	"PYTHONHOME":      true,
	"PYTHONPATH":      true,
}

func effectiveTelegramTargetUnitContent(target systemdTargetName, identity *persistedUserIdentity) (string, error) {
	var roots []unitSearchRoot
	if target.UserMode {
		if err := validatePersistedUserIdentity(target.User, identity); err != nil {
			return "", err
		}
		roots = userUnitSearchRootSpecsFor(identity.Home, strconv.Itoa(identity.UID))
	} else {
		for _, root := range systemUnitSearchRoots() {
			roots = append(roots, unitSearchRoot{Path: root, Manage: true})
		}
	}
	paths := make([]string, 0, len(roots))
	for _, root := range roots {
		paths = append(paths, root.Path)
	}
	path, ok := effectiveUnitPathInRoots(target.Service, paths)
	if !ok {
		return "", fmt.Errorf("无法定位有效 systemd 单元：%s", canonicalTelegramTargetName(target))
	}
	content, err := readTelegramUnitContentWithDropIns(path, target.Service, roots)
	if err != nil {
		return "", fmt.Errorf("读取有效 systemd 单元失败（%s）：%w", canonicalTelegramTargetName(target), err)
	}
	return content, nil
}

func validateOpenClawTargetRuntime(target systemdTargetName, identity *persistedUserIdentity) error {
	if !target.UserMode {
		return fmt.Errorf("OpenClaw 配置接管只支持用户级服务")
	}
	if err := verifyOpenClawUserIdentity(target.User, identity); err != nil {
		return err
	}
	if err := validateOpenClawCanonicalConfigCandidate(target.User, identity); err != nil {
		return err
	}
	content, err := effectiveTelegramTargetUnitContent(target, identity)
	if err != nil {
		return err
	}
	if err := validateOpenClawEffectiveUnit(content, identity.Home); err != nil {
		return fmt.Errorf("OpenClaw 服务 %s 的运行配置无法绑定到默认配置：%w", canonicalTelegramTargetName(target), err)
	}
	managerEnvironment, err := telegramTargetManagerEnvironment(target, identity, openClawLookupUserIdentity)
	if err != nil {
		return fmt.Errorf("无法检查 OpenClaw 服务 %s 的 systemd manager 环境：%w", canonicalTelegramTargetName(target), err)
	}
	if err := validateOpenClawManagerEnvironment(content, managerEnvironment); err != nil {
		return fmt.Errorf("OpenClaw 服务 %s 可能从 systemd manager 继承其它配置路径：%w", canonicalTelegramTargetName(target), err)
	}
	if err := validateOpenClawRuntimeDotEnvFiles(target.User, identity); err != nil {
		return err
	}
	return nil
}

func validateOpenClawCanonicalConfigCandidate(user string, identity *persistedUserIdentity) error {
	candidates := []string{
		filepath.Join(identity.Home, ".openclaw", "openclaw.json"),
		filepath.Join(identity.Home, ".openclaw", "clawdbot.json"),
		filepath.Join(identity.Home, ".clawdbot", "openclaw.json"),
		filepath.Join(identity.Home, ".clawdbot", "clawdbot.json"),
	}
	for index, path := range candidates {
		_, err := readOpenClawRuntimeFile(user, identity, path, maxOpenClawConfigBytes)
		if err == nil {
			if index == 0 {
				return nil
			}
			return fmt.Errorf("OpenClaw 当前会使用 legacy 配置 %s；journal 只绑定 %s，拒绝部分接管", path, candidates[0])
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("无法安全判定 OpenClaw 默认配置候选 %s：%w", path, err)
		}
	}
	return nil
}

func validateOpenClawEffectiveUnit(content, expectedHome string) error {
	if err := validateTelegramServiceExecutionModel(content, false); err != nil {
		return err
	}
	environment, err := effectiveServiceEnvironment(content)
	if err != nil {
		return err
	}
	if environment["OPENCLAW_SERVICE_MARKER"] != "openclaw" || environment["OPENCLAW_SERVICE_KIND"] != "gateway" {
		return fmt.Errorf("缺少 OpenClaw gateway 身份标记")
	}
	home, ok := environment["HOME"]
	if !ok || strings.TrimSpace(home) == "" {
		return fmt.Errorf("有效环境没有显式 HOME，无法证明默认配置路径")
	}
	if !filepath.IsAbs(home) || filepath.Clean(home) != filepath.Clean(expectedHome) {
		return fmt.Errorf("有效 HOME=%q 与目标用户 home=%q 不一致", home, expectedHome)
	}
	serviceUser, err := effectiveServiceSingleWord(content, "User")
	if err != nil {
		return err
	}
	if serviceUser != "" {
		return fmt.Errorf("用户级 OpenClaw 服务声明了 User=%q，运行身份不再由目标 user manager 绑定", serviceUser)
	}
	workingDirectory, err := effectiveServiceSingleWord(content, "WorkingDirectory")
	if err != nil {
		return err
	}
	if workingDirectory != "" && workingDirectory != "~" {
		if !filepath.IsAbs(workingDirectory) || filepath.Clean(workingDirectory) != filepath.Clean(expectedHome) {
			return fmt.Errorf("有效 WorkingDirectory=%q 与 OpenClaw 会读取的目标用户 home=%q 不一致", workingDirectory, expectedHome)
		}
	}
	for key := range openClawConfigSelectionEnvKeys {
		if key == "HOME" {
			continue
		}
		if value, present := environment[key]; present && openClawUnsafeConfigSelector(key, value) {
			return fmt.Errorf("有效环境声明了配置选择器 %s", key)
		}
	}
	environmentFiles, err := effectiveServiceEnvironmentFiles(content)
	if err != nil {
		return err
	}
	if len(environmentFiles) != 0 {
		return fmt.Errorf("存在有效 EnvironmentFile，无法证明其中没有配置选择器")
	}
	passEnvironment, err := effectiveServicePassEnvironment(content)
	if err != nil {
		return err
	}
	for _, name := range passEnvironment {
		// Validate NODE_OPTIONS against the effective manager environment below.
		if name == "NODE_OPTIONS" {
			continue
		}
		if openClawConfigSelectionEnvKeys[strings.ToUpper(name)] {
			return fmt.Errorf("PassEnvironment 可能从 systemd manager 继承配置选择器 %s", name)
		}
	}
	execStarts, err := effectiveServiceExecStarts(content)
	if err != nil {
		return err
	}
	if len(execStarts) == 0 {
		return fmt.Errorf("没有可解析的 ExecStart")
	}
	for _, argv := range execStarts {
		if !openClawGatewayArgv(argv) {
			return fmt.Errorf("有效 ExecStart 不是可验证的 OpenClaw gateway 直接调用")
		}
		if selector := openClawExecSelector(argv); selector != "" {
			return fmt.Errorf("ExecStart 声明了配置选择器 %s", selector)
		}
	}
	return nil
}

func openClawGatewayArgv(argv []string) bool {
	if len(argv) < 3 {
		return false
	}
	command, ok := systemdDirectExecCommand(argv[0])
	if !ok || !filepath.IsAbs(command) || filepath.Clean(command) != command {
		return false
	}
	base := filepath.Base(command)
	if base != "node" && base != "nodejs" {
		return false
	}
	scriptIndex := 1
	for scriptIndex < len(argv) && strings.HasPrefix(argv[scriptIndex], "-") {
		if !openClawMemoryOption(argv[scriptIndex]) {
			return false
		}
		scriptIndex++
	}
	if scriptIndex+1 >= len(argv) || argv[scriptIndex+1] != "gateway" {
		return false
	}
	script := argv[scriptIndex]
	if !filepath.IsAbs(script) || filepath.Clean(script) != script {
		return false
	}
	switch filepath.Base(script) {
	case "index.js", "index.mjs", "entry.js", "entry.mjs":
		return strings.HasSuffix(filepath.Dir(script), string(os.PathSeparator)+filepath.Join("openclaw", "dist"))
	default:
		return false
	}
}

// Keep this whitelist limited to numeric memory controls. Flags that load code
// must never pass as harmless options even if the official entrypoint follows.

func openClawMemoryOption(arg string) bool {
	flag, value, assigned := strings.Cut(arg, "=")
	if !assigned || value == "" {
		return false
	}
	flag = strings.ReplaceAll(flag, "_", "-")
	switch flag {
	case "--max-old-space-size", "--max-semi-space-size", "--max-heap-size", "--max-old-space-size-percentage":
	default:
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	limit, err := strconv.ParseUint(value, 10, 53)
	return err == nil && (flag != "--max-old-space-size-percentage" || limit > 0 && limit <= 100)
}

func openClawMemoryNodeOptions(value string) bool {
	// NODE_OPTIONS uses literal spaces and double quotes, not shell syntax.
	// Refuse escapes and control characters instead of guessing how Node
	// would consume them; numeric heap options do not need either feature.
	args := []string{}
	var token strings.Builder
	quoted := false
	for _, char := range value {
		switch {
		case char == '"':
			quoted = !quoted
		case char == ' ' && !quoted:
			if token.Len() != 0 {
				args = append(args, token.String())
				token.Reset()
			}
		case char == '\\' || char < ' ' || char > '~':
			return false
		default:
			token.WriteRune(char)
		}
	}
	if quoted {
		return false
	}
	if token.Len() != 0 {
		args = append(args, token.String())
	}
	for _, arg := range args {
		if !openClawMemoryOption(arg) {
			return false
		}
	}
	return true
}

func openClawUnsafeConfigSelector(key, value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	return key != "NODE_OPTIONS" || !openClawMemoryNodeOptions(value)
}

func openClawExecSelector(argv []string) string {
	for _, arg := range argv {
		if containsCommandOption(arg, "--profile") {
			return "--profile"
		}
		if containsCommandOption(arg, "--dev") {
			return "--dev"
		}
		if containsCommandOption(arg, "--container") {
			return "--container"
		}
		upperArg := strings.ToUpper(arg)
		for key := range openClawConfigSelectionEnvKeys {
			if containsShellAssignment(upperArg, key) {
				return key
			}
		}
	}
	return ""
}

func containsCommandOption(value, option string) bool {
	for offset := 0; ; {
		index := strings.Index(value[offset:], option)
		if index < 0 {
			return false
		}
		index += offset
		beforeOK := index == 0 || isShellWordBoundary(value[index-1])
		after := index + len(option)
		afterOK := after == len(value) || value[after] == '=' || isShellWordBoundary(value[after])
		if beforeOK && afterOK {
			return true
		}
		offset = index + len(option)
		if offset >= len(value) {
			return false
		}
	}
}

func containsShellAssignment(value, key string) bool {
	needle := key + "="
	for offset := 0; ; {
		index := strings.Index(value[offset:], needle)
		if index < 0 {
			return false
		}
		index += offset
		if index == 0 || isShellWordBoundary(value[index-1]) {
			return true
		}
		offset = index + len(needle)
		if offset >= len(value) {
			return false
		}
	}
}

func isShellWordBoundary(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n' ||
		value == ';' || value == '&' || value == '|' || value == '(' || value == ')' ||
		value == '{' || value == '}'
}

func effectiveServiceEnvironmentFiles(content string) ([]string, error) {
	return effectiveServiceWordList(content, "EnvironmentFile")
}

func effectiveServicePassEnvironment(content string) ([]string, error) {
	return effectiveServiceWordList(content, "PassEnvironment")
}

func effectiveServiceWordList(content, directive string) ([]string, error) {
	return effectiveSectionWordList(content, "Service", directive)
}

func effectiveSectionWordList(content, wantedSection, directive string) ([]string, error) {
	files := []string{}
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
		if section != wantedSection {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != directive {
			continue
		}
		words, err := splitSystemdWords(value)
		if err != nil {
			return nil, err
		}
		if len(words) == 0 {
			files = nil
			continue
		}
		files = append(files, words...)
	}
	return files, nil
}

func validateTelegramServiceExecutionModel(content string, allowHermesStopHooks bool) error {
	for _, directive := range []string{"Environment", "UnsetEnvironment", "PassEnvironment", "User", "WorkingDirectory", "ExecStart"} {
		words, err := effectiveServiceWordList(content, directive)
		if err != nil {
			return err
		}
		for _, word := range words {
			if hasActiveSystemdSpecifier(word) {
				return fmt.Errorf("%s 使用 systemd specifier，无法静态绑定实际运行配置", directive)
			}
			if directive == "ExecStart" && strings.Contains(word, "$") {
				return fmt.Errorf("ExecStart 使用 systemd 环境变量展开，无法静态绑定实际 argv")
			}
		}
	}
	protectHome, err := effectiveServiceSingleWord(content, "ProtectHome")
	if err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(protectHome)) {
	case "", "no", "false", "off", "0":
	default:
		return fmt.Errorf("ProtectHome=%s 会改变服务看到的 home 文件视图，无法绑定宿主机运行配置", protectHome)
	}
	dynamicUser, err := effectiveServiceSingleWord(content, "DynamicUser")
	if err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(dynamicUser)) {
	case "", "no", "false", "off", "0":
	default:
		return fmt.Errorf("DynamicUser=%s 使服务身份无法绑定到本地账号", dynamicUser)
	}
	serviceType, err := effectiveServiceSingleWord(content, "Type")
	if err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(serviceType)) {
	case "", "simple", "exec":
	default:
		return fmt.Errorf("服务 Type=%s 的启动完成语义无法绑定到单一前台 gateway 进程", serviceType)
	}
	privateNetwork, err := effectiveServiceSingleWord(content, "PrivateNetwork")
	if err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(privateNetwork)) {
	case "", "no", "false", "off", "0":
	default:
		return fmt.Errorf("PrivateNetwork=%s 使服务中的 127.0.0.1 不再指向宿主机代理", privateNetwork)
	}
	forbiddenDirectives := []string{
		"ExecCondition",
		"ExecStartPre",
		"ExecStartPost",
		"PAMName",
		"RootDirectory",
		"RootImage",
		"BindPaths",
		"BindReadOnlyPaths",
		"TemporaryFileSystem",
		"InaccessiblePaths",
		"MountImages",
		"ExtensionImages",
		"ExtensionDirectories",
		"NetworkNamespacePath",
	}
	if !allowHermesStopHooks {
		forbiddenDirectives = append(forbiddenDirectives, "ExecStop", "ExecStopPost")
	}
	for _, directive := range forbiddenDirectives {
		words, err := effectiveServiceWordList(content, directive)
		if err != nil {
			return err
		}
		if len(words) != 0 {
			return fmt.Errorf("%s 会改变服务启动时的环境、配置或文件视图，无法绑定宿主机运行配置", directive)
		}
	}
	joinsNamespaceOf, err := effectiveSectionWordList(content, "Unit", "JoinsNamespaceOf")
	if err != nil {
		return err
	}
	if len(joinsNamespaceOf) != 0 {
		return fmt.Errorf("JoinsNamespaceOf 会使服务共享其它单元的网络命名空间，无法保证 127.0.0.1 指向宿主机代理")
	}
	return nil
}

func hasActiveSystemdSpecifier(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] != '%' {
			continue
		}
		if i+1 < len(value) && value[i+1] == '%' {
			i++
			continue
		}
		return true
	}
	return false
}

func telegramTargetManagerEnvironment(target systemdTargetName, identity *persistedUserIdentity, lookup localUserIdentityLookup) (map[string]string, error) {
	var (
		output string
		err    error
	)
	if target.UserMode {
		if err := validatePersistedUserIdentity(target.User, identity); err != nil {
			return nil, err
		}
		output, err = telegramOutputUserManagerEnvironment(target.User, identity, lookup)
	} else {
		output, err = telegramOutputSystemManagerEnvironment()
	}
	if err != nil {
		return nil, err
	}
	if len(output) > maxSystemdManagerEnvironmentBytes {
		return nil, fmt.Errorf("systemd manager 环境输出超过 %d 字节", maxSystemdManagerEnvironmentBytes)
	}
	return parseSystemdManagerEnvironment(output)
}

func parseSystemdManagerEnvironment(output string) (map[string]string, error) {
	environment := map[string]string{}
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		if line == "" {
			continue
		}
		name, encoded, ok := strings.Cut(line, "=")
		if !ok || name == "" || strings.TrimSpace(name) != name || strings.ContainsAny(name, " \t\r\n") {
			return nil, fmt.Errorf("systemd manager 环境含无法解析的变量名")
		}
		words, err := splitSystemdWords(encoded)
		if err != nil {
			return nil, fmt.Errorf("systemd manager 环境变量 %s 无法解析：%w", name, err)
		}
		switch len(words) {
		case 0:
			environment[name] = ""
		case 1:
			environment[name] = words[0]
		default:
			return nil, fmt.Errorf("systemd manager 环境变量 %s 的编码不明确", name)
		}
	}
	return environment, nil
}

func validateOpenClawManagerEnvironment(content string, inherited map[string]string) error {
	environment, err := effectiveServiceEnvironmentWithBase(content, inherited)
	if err != nil {
		return err
	}
	for key := range openClawConfigSelectionEnvKeys {
		if key == "HOME" {
			// The managed unit must declare an explicit, matching HOME, which has
			// higher precedence than the user manager's inherited HOME.
			continue
		}
		if value, present := environment[key]; present && openClawUnsafeConfigSelector(key, value) {
			return fmt.Errorf("systemd manager 环境声明了配置选择器 %s", key)
		}
	}
	return nil
}

func rejectOpenClawSelectorDotEnv(user string, identity *persistedUserIdentity, path string) error {
	raw, err := readOpenClawRuntimeFile(user, identity, path, maxOpenClawRuntimeEnvBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("无法安全检查 OpenClaw 环境文件 %s：%w", path, err)
	}
	if err := validateDotEnvText(raw); err != nil {
		return fmt.Errorf("无法安全检查 OpenClaw 环境文件 %s：%w", path, err)
	}
	nodeOptionsChecked, nodeOptionsSafe := false, false
	for _, key := range openClawDotEnvDeclaredKeys(raw) {
		if key == "NODE_OPTIONS" {
			if !nodeOptionsChecked {
				nodeOptionsSafe = openClawDotEnvHasOnlyMemoryOptions(raw)
				nodeOptionsChecked = true
			}
			if nodeOptionsSafe {
				continue
			}
		}
		if openClawConfigSelectionEnvKeys[strings.ToUpper(key)] {
			return fmt.Errorf("OpenClaw 环境文件 %s 声明了配置选择器 %s，拒绝接管默认配置", path, key)
		}
	}
	return nil
}

func openClawDotEnvHasOnlyMemoryOptions(raw []byte) bool {
	// The declaration scanner deliberately over-approximates dotenv syntax.
	// Only relax NODE_OPTIONS for unambiguous single-line assignments; bare,
	// multiline or malformed declarations remain unsafe.
	for _, line := range strings.FieldsFunc(normalizeDotEnvLineEndings(string(raw)), func(r rune) bool {
		return r == '\n' || r == '\u2028' || r == '\u2029'
	}) {
		declared := false
		for _, key := range openClawDotEnvDeclaredKeys([]byte(line)) {
			if key == "NODE_OPTIONS" {
				declared = true
			}
		}
		if !declared {
			continue
		}
		line = strings.TrimFunc(line, dotEnvSpace)
		if strings.HasPrefix(line, "export") {
			line = strings.TrimLeftFunc(line[len("export"):], dotEnvSpace)
		}
		if !strings.HasPrefix(line, "NODE_OPTIONS") {
			return false
		}
		tail := line[len("NODE_OPTIONS"):]
		value := strings.TrimLeftFunc(tail, dotEnvSpace)
		if strings.HasPrefix(value, "=") {
			value = value[1:]
		} else if strings.HasPrefix(tail, ":") && len(tail) > 1 {
			space, _ := utf8.DecodeRuneInString(tail[1:])
			if !dotEnvSpace(space) {
				return false
			}
			value = tail[1:]
		} else {
			return false
		}
		value = strings.TrimFunc(value, dotEnvSpace)
		if len(value) > 0 && (value[0] == '\'' || value[0] == '"' || value[0] == '`') {
			quote := value[0]
			end := strings.IndexByte(value[1:], quote)
			if end < 0 {
				return false
			}
			end++
			rest := strings.TrimFunc(value[end+1:], dotEnvSpace)
			if rest != "" && !strings.HasPrefix(rest, "#") {
				return false
			}
			value = value[1:end]
		} else {
			var comment bool
			value, _, comment = strings.Cut(value, "#")
			value = strings.TrimFunc(value, dotEnvSpace)
			// dotenv can attach a quoted value on a later line to an empty
			// assignment. Require explicit quotes or a comment terminator for
			// empty values so the following line cannot hide a Node preload.
			if value == "" && !comment {
				return false
			}
		}
		if !openClawMemoryNodeOptions(value) {
			return false
		}
	}
	return true
}

func validateOpenClawRuntimeDotEnvFiles(user string, identity *persistedUserIdentity) error {
	// OpenClaw gateway startup loads process.cwd()/.env before selecting its
	// runtime configuration. validateOpenClawEffectiveUnit binds cwd to Home,
	// so all three paths below are deterministic and can be inspected safely.
	for _, path := range []string{
		filepath.Join(identity.Home, ".env"),
		filepath.Join(identity.Home, ".openclaw", ".env"),
		filepath.Join(identity.Home, ".config", "openclaw", "gateway.env"),
	} {
		if err := rejectOpenClawSelectorDotEnv(user, identity, path); err != nil {
			return err
		}
	}
	return nil
}

func readOpenClawRuntimeFile(user string, expected *persistedUserIdentity, path string, max int64) ([]byte, error) {
	identity, err := verifyPersistedUserIdentity(user, expected, openClawLookupUserIdentity)
	if err != nil {
		return nil, err
	}
	cleanPath, dirFD, err := openUserFileDirForIdentity(user, identity, path, false)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(dirFD)
	return readRegularFileAtNoFollow(dirFD, filepath.Base(cleanPath), max)
}

func decodeHermesDotEnv(raw []byte) (string, error) {
	if bytes.HasPrefix(raw, []byte{0xff, 0xfe, 0x00, 0x00}) ||
		bytes.HasPrefix(raw, []byte{0x00, 0x00, 0xfe, 0xff}) {
		return "", fmt.Errorf("不支持 UTF-32 编码")
	}
	if bytes.HasPrefix(raw, []byte{0xff, 0xfe}) {
		return decodeHermesUTF16(raw[2:], binary.LittleEndian)
	}
	if bytes.HasPrefix(raw, []byte{0xfe, 0xff}) {
		return decodeHermesUTF16(raw[2:], binary.BigEndian)
	}
	if bytes.HasPrefix(raw, []byte{0xef, 0xbb, 0xbf}) {
		raw = raw[3:]
	}
	if bytes.IndexByte(raw, 0) >= 0 {
		return "", fmt.Errorf("无 BOM 的 NUL 字节编码无法安全判定")
	}
	if !utf8.Valid(raw) {
		return "", fmt.Errorf("环境文件不是有效 UTF-8")
	}
	return string(raw), nil
}

func decodeHermesUTF16(raw []byte, order binary.ByteOrder) (string, error) {
	if len(raw)%2 != 0 {
		return "", fmt.Errorf("UTF-16 字节数不是偶数")
	}
	var decoded strings.Builder
	for i := 0; i < len(raw); i += 2 {
		unit := order.Uint16(raw[i : i+2])
		if unit == 0 {
			return "", fmt.Errorf("UTF-16 环境文件含 NUL 字符")
		}
		if unit >= 0xd800 && unit <= 0xdbff {
			if i+4 > len(raw) {
				return "", fmt.Errorf("UTF-16 高代理项不完整")
			}
			low := order.Uint16(raw[i+2 : i+4])
			if low < 0xdc00 || low > 0xdfff {
				return "", fmt.Errorf("UTF-16 代理项配对无效")
			}
			decoded.WriteRune(utf16.DecodeRune(rune(unit), rune(low)))
			i += 2
			continue
		}
		if unit >= 0xdc00 && unit <= 0xdfff {
			return "", fmt.Errorf("UTF-16 低代理项缺少高代理项")
		}
		decoded.WriteRune(rune(unit))
	}
	return decoded.String(), nil
}

func rejectOpenClawRuntimeConfigSelectors(raw []byte) error {
	var resolvedShape any
	if err := decodeOpenClawJSON5Value(raw, &resolvedShape); err != nil {
		return err
	}
	if openClawConfigContainsInclude(resolvedShape) {
		return fmt.Errorf("OpenClaw 配置使用 $include，无法证明被包含配置没有路径选择器或账号级 proxy")
	}
	cfg, err := decodeRawJSONObject(raw, "OpenClaw 顶层配置")
	if err != nil {
		return err
	}
	envRaw, present := cfg["env"]
	if !present {
		return nil
	}
	env, err := decodeRawJSONObject(envRaw, "OpenClaw env 配置")
	if err != nil {
		return err
	}
	if shellRaw, ok := env["shellEnv"]; ok {
		shellEnv, err := decodeRawJSONObject(shellRaw, "OpenClaw env.shellEnv")
		if err != nil {
			return err
		}
		if enabledRaw, ok := shellEnv["enabled"]; ok {
			var enabled bool
			if err := decodeOpenClawJSON5Value(enabledRaw, &enabled); err != nil {
				return fmt.Errorf("OpenClaw env.shellEnv.enabled 无效：%w", err)
			}
			if enabled {
				return fmt.Errorf("OpenClaw env.shellEnv.enabled=true 可能导入配置选择器，拒绝接管默认配置")
			}
		}
	}
	if varsRaw, ok := env["vars"]; ok {
		vars, err := decodeRawJSONObject(varsRaw, "OpenClaw env.vars")
		if err != nil {
			return err
		}
		if key, found, err := firstOpenClawConfigSelector(vars); err != nil || found {
			if err != nil {
				return err
			}
			return fmt.Errorf("OpenClaw env.vars 声明了配置选择器 %s", key)
		}
	}
	direct := make(map[string]json.RawMessage, len(env))
	for key, value := range env {
		if key != "vars" && key != "shellEnv" {
			direct[key] = value
		}
	}
	if key, found, err := firstOpenClawConfigSelector(direct); err != nil || found {
		if err != nil {
			return err
		}
		return fmt.Errorf("OpenClaw env 声明了配置选择器 %s", key)
	}
	return nil
}

func openClawConfigContainsInclude(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "$include" || openClawConfigContainsInclude(child) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if openClawConfigContainsInclude(child) {
				return true
			}
		}
	}
	return false
}

func firstOpenClawConfigSelector(values map[string]json.RawMessage) (string, bool, error) {
	for key, raw := range values {
		if !openClawConfigSelectionEnvKeys[strings.ToUpper(strings.TrimSpace(key))] {
			continue
		}
		var value string
		if err := decodeOpenClawJSON5Value(raw, &value); err != nil {
			return "", false, fmt.Errorf("OpenClaw 配置选择器 %s 的值不是字符串：%w", key, err)
		}
		if openClawUnsafeConfigSelector(key, value) {
			return key, true, nil
		}
	}
	return "", false, nil
}

func validateHermesTargetRuntime(target systemdTargetName, identity *persistedUserIdentity, expectedProxy string) error {
	return validateHermesTargetRuntimeWithRestartPolicy(target, identity, expectedProxy, false)
}

func validateHermesTargetRuntimeWithRestartPolicy(target systemdTargetName, identity *persistedUserIdentity, expectedProxy string, checkRestartPolicy bool) error {
	if target.UserMode {
		if err := verifyTelegramUserIdentity(target, identity); err != nil {
			return err
		}
	}
	content, err := effectiveTelegramTargetUnitContent(target, identity)
	if err != nil {
		return err
	}
	projectRoot, err := validateHermesEffectiveUnit(content)
	if err != nil {
		return fmt.Errorf("目标 Hermes 服务 %s 无法保证 Telegram 代理生效：%w", canonicalTelegramTargetName(target), err)
	}
	managerEnvironment, err := telegramTargetManagerEnvironment(target, identity, telegramLookupUserIdentity)
	if err != nil {
		return fmt.Errorf("无法检查 Hermes 服务 %s 的 systemd manager 环境：%w", canonicalTelegramTargetName(target), err)
	}
	effectiveEnvironment, err := effectiveServiceEnvironmentWithBase(content, managerEnvironment)
	if err != nil {
		return fmt.Errorf("无法计算 Hermes 服务 %s 的最终环境：%w", canonicalTelegramTargetName(target), err)
	}
	if err := validateHermesManagerEnvironment(effectiveEnvironment); err != nil {
		return fmt.Errorf("检测到 Hermes 服务 %s 会从 systemd manager 继承代理绕过：%w", canonicalTelegramTargetName(target), err)
	}
	if expectedProxy != "" && effectiveEnvironment["TELEGRAM_PROXY"] != expectedProxy {
		return fmt.Errorf(
			"检测到 Hermes 服务 %s 的最终 TELEGRAM_PROXY=%q，与受管值 %q 不一致",
			canonicalTelegramTargetName(target), effectiveEnvironment["TELEGRAM_PROXY"], expectedProxy,
		)
	}
	if err := validateHermesPythonSafePath(effectiveEnvironment, expectedProxy != ""); err != nil {
		return fmt.Errorf("目标 Hermes 服务 %s 的 Python 模块搜索路径不安全：%w", canonicalTelegramTargetName(target), err)
	}
	if err := validateHermesFallbackDiscoveryEnvironment(effectiveEnvironment, expectedProxy != ""); err != nil {
		return fmt.Errorf("目标 Hermes 服务 %s 的 Telegram 辅助网络发现不符合受管策略：%w", canonicalTelegramTargetName(target), err)
	}
	if err := validateHermesRuntimeFilesWithRestartPolicy(target, identity, content, projectRoot, effectiveEnvironment, checkRestartPolicy); err != nil {
		return fmt.Errorf("目标 Hermes 服务 %s 的应用级环境无法安全验证：%w", canonicalTelegramTargetName(target), err)
	}
	return nil
}

// validateHermesEffectiveUnit returns the project root bound to the validated
// ExecStart. Reuse it only within this runtime check; later operations must read
// and validate the effective unit and user identity again.
func validateHermesEffectiveUnit(content string) (string, error) {
	if err := validateTelegramServiceExecutionModel(content, true); err != nil {
		return "", err
	}
	execStarts, err := effectiveServiceExecStarts(content)
	if err != nil {
		return "", err
	}
	if len(execStarts) != 1 || !hermesGatewayArgv(execStarts[0]) {
		return "", fmt.Errorf("有效 ExecStart 必须且只能有一个 Hermes gateway 直接调用")
	}
	projectRoot, err := hermesProjectRootFromArgv(execStarts[0])
	if err != nil {
		return "", err
	}
	if err := validateHermesCleanupHook(content, projectRoot); err != nil {
		return "", err
	}
	if err := validateHermesPlannedStopHook(content, projectRoot); err != nil {
		return "", err
	}
	environmentFiles, err := effectiveServiceEnvironmentFiles(content)
	if err != nil {
		return "", err
	}
	if len(environmentFiles) != 0 {
		return "", fmt.Errorf("存在有效 EnvironmentFile，无法证明其中的 NO_PROXY 不会绕过 Telegram")
	}
	passEnvironment, err := effectiveServicePassEnvironment(content)
	if err != nil {
		return "", err
	}
	for _, name := range passEnvironment {
		if hermesRuntimeDotEnvKeys[name] {
			return "", fmt.Errorf("PassEnvironment 可能从 systemd manager 继承 Hermes 路由变量 %s", name)
		}
	}
	for _, argv := range execStarts {
		for _, arg := range argv {
			upperArg := strings.ToUpper(arg)
			for name := range hermesRuntimeDotEnvKeys {
				if containsShellAssignment(upperArg, strings.ToUpper(name)) {
					return "", fmt.Errorf("ExecStart 内声明了 Hermes 路由变量 %s，无法证明 Telegram 代理生效", name)
				}
			}
			if containsCommandOption(arg, "--profile") || containsCommandOption(arg, "-p") {
				return "", fmt.Errorf("ExecStart 使用 Hermes profile 选择器，无法绑定实际 HERMES_HOME")
			}
		}
	}
	environment, err := effectiveServiceEnvironment(content)
	if err != nil {
		return "", err
	}
	if err := validateHermesMultiplexEnvironment(environment); err != nil {
		return "", err
	}
	if environment["HERMES_S6_SUPERVISED_CHILD"] != "" {
		return "", fmt.Errorf("HERMES_S6_SUPERVISED_CHILD 会改变 Hermes active_profile 选择，无法绑定实际 HERMES_HOME")
	}
	for key := range hermesCodeInjectionEnvKeys {
		if environment[key] != "" {
			return "", fmt.Errorf("%s 会改变 Hermes 实际加载的代码", key)
		}
	}
	if err := validateHermesPythonSafePath(environment, false); err != nil {
		return "", err
	}
	for _, key := range []string{"NO_PROXY", "no_proxy"} {
		if entry, ok := firstHermesTelegramNoProxyMatch(environment[key]); ok {
			return "", fmt.Errorf("%s 中的 %q 会绕过 api.telegram.org 或 Hermes Telegram 回退网段", key, entry)
		}
	}
	return projectRoot, nil
}

func validateHermesCleanupHook(content, projectRoot string) error {
	return validateHermesStopHook(content, projectRoot, "ExecStopPost", "gateway.cgroup_cleanup")
}

func validateHermesPlannedStopHook(content, projectRoot string) error {
	return validateHermesStopHook(content, projectRoot, "ExecStop", "gateway.systemd_stop_mark")
}

func validateHermesStopHook(content, projectRoot, directive, module string) error {
	commands, err := effectiveServiceCommands(content, directive)
	if err != nil {
		return err
	}
	if len(commands) == 0 {
		return nil
	}
	expectedCommand := "-" + filepath.Join(projectRoot, "venv", "bin", "python")
	if len(commands) != 1 || len(commands[0]) != 3 ||
		commands[0][0] != expectedCommand || commands[0][1] != "-m" || commands[0][2] != module {
		return fmt.Errorf("%s 必须且只能是与 Hermes PROJECT_ROOT 绑定的 %s -m %s", directive, expectedCommand, module)
	}
	return nil
}

func effectiveServiceCommands(content, directive string) ([][]string, error) {
	section := ""
	commands := [][]string{}
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
		if !ok || strings.TrimSpace(key) != directive {
			continue
		}
		words, err := splitSystemdWords(value)
		if err != nil {
			return nil, err
		}
		if len(words) == 0 {
			commands = nil
			continue
		}
		commands = append(commands, words)
	}
	return commands, nil
}

func validateHermesManagerEnvironment(environment map[string]string) error {
	if err := validateHermesMultiplexEnvironment(environment); err != nil {
		return err
	}
	if environment["HERMES_S6_SUPERVISED_CHILD"] != "" {
		return fmt.Errorf("HERMES_S6_SUPERVISED_CHILD 会改变 Hermes active_profile 选择")
	}
	for key := range hermesCodeInjectionEnvKeys {
		if environment[key] != "" {
			return fmt.Errorf("%s 会改变 Hermes 实际加载的代码", key)
		}
	}
	if err := validateHermesPythonSafePath(environment, false); err != nil {
		return err
	}
	for _, key := range []string{"NO_PROXY", "no_proxy"} {
		if entry, ok := firstHermesTelegramNoProxyMatch(environment[key]); ok {
			return fmt.Errorf("%s 中的 %q 会绕过 api.telegram.org 或 Hermes Telegram 回退网段", key, entry)
		}
	}
	return nil
}

func validateHermesPythonSafePath(environment map[string]string, required bool) error {
	value := environment["PYTHONSAFEPATH"]
	if required && value != "1" {
		return fmt.Errorf("最终 PYTHONSAFEPATH=%q，与受管值 %q 不一致", value, "1")
	}
	if value != "" && value != "1" {
		return fmt.Errorf("PYTHONSAFEPATH=%q 无法绑定到 proxyscene 管理的安全模块搜索路径", value)
	}
	return nil
}

func validateHermesRuntimeFiles(target systemdTargetName, identity *persistedUserIdentity, content, projectRoot string, environment map[string]string) error {
	return validateHermesRuntimeFilesWithRestartPolicy(target, identity, content, projectRoot, environment, false)
}

func validateHermesRuntimeFilesWithRestartPolicy(target systemdTargetName, identity *persistedUserIdentity, content, projectRoot string, environment map[string]string, checkRestartPolicy bool) error {
	if _, present := environment["PYTEST_CURRENT_TEST"]; present {
		return fmt.Errorf("PYTEST_CURRENT_TEST 会改变 Hermes managed 配置加载，无法绑定实际生效配置")
	}
	user, runtimeIdentity, err := hermesRuntimeIdentity(target, identity, content)
	if err != nil {
		return err
	}
	effectiveHome := strings.TrimSpace(environment["HOME"])
	if effectiveHome == "" || !filepath.IsAbs(effectiveHome) || filepath.Clean(effectiveHome) != runtimeIdentity.Home {
		return fmt.Errorf("最终 HOME=%q 与服务用户 %s 的已绑定 home=%q 不一致", effectiveHome, user, runtimeIdentity.Home)
	}
	hermesHome := strings.TrimSpace(environment["HERMES_HOME"])
	if hermesHome == "" {
		hermesHome = filepath.Join(runtimeIdentity.Home, ".hermes")
	}
	hermesHome = filepath.Clean(hermesHome)
	if !filepath.IsAbs(hermesHome) || hermesHome == string(os.PathSeparator) ||
		(hermesHome != runtimeIdentity.Home && !strings.HasPrefix(hermesHome, runtimeIdentity.Home+string(os.PathSeparator))) {
		return fmt.Errorf("HERMES_HOME 必须位于服务用户 %s 的已绑定 home 内：%s", user, hermesHome)
	}

	defaultRoot := filepath.Join(runtimeIdentity.Home, ".hermes")
	if strings.HasPrefix(hermesHome, defaultRoot+string(os.PathSeparator)) &&
		filepath.Dir(hermesHome) != filepath.Join(defaultRoot, "profiles") {
		return fmt.Errorf("HERMES_HOME 位于默认根内但不是直接 profile，无法绑定新版 Hermes profile 枚举根")
	}
	hermesRoot := hermesHome
	activeHome := hermesHome
	if filepath.Base(filepath.Dir(hermesHome)) == "profiles" {
		hermesRoot = filepath.Dir(filepath.Dir(hermesHome))
	} else if environment["HERMES_S6_SUPERVISED_CHILD"] == "" {
		activeRaw, err := readHermesRuntimeFile(user, runtimeIdentity, filepath.Join(hermesRoot, "active_profile"), 256)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("读取 Hermes active_profile 失败：%w", err)
		}
		if err == nil {
			profile := strings.TrimSpace(string(activeRaw))
			if profile != "" && profile != "default" {
				if !validHermesProfileName(profile) {
					return fmt.Errorf("无法安全解析 Hermes active_profile 名称")
				}
				activeHome = filepath.Join(hermesRoot, "profiles", profile)
			}
		}
	}

	if err := validateHermesSingleProfile(user, runtimeIdentity, hermesRoot, activeHome); err != nil {
		return err
	}

	systemLayout, err := hermesProjectUsesSystemLayout(hermesRoot, projectRoot)
	if err != nil {
		return err
	}
	if systemLayout {
		if err := validateHermesSystemProject(content, projectRoot); err != nil {
			return err
		}
		if err := rejectHermesSystemDotEnv(filepath.Join(projectRoot, ".env")); err != nil {
			return err
		}
	} else {
		if err := validateHermesUserProjectDirectory(user, runtimeIdentity, projectRoot); err != nil {
			return err
		}
		if err := rejectHermesRuntimeDotEnv(user, runtimeIdentity, filepath.Join(projectRoot, ".env")); err != nil {
			return err
		}
	}

	for _, path := range []string{
		filepath.Join(activeHome, ".env"),
		filepath.Join(activeHome, ".op.env"),
	} {
		if err := rejectHermesRuntimeDotEnv(user, runtimeIdentity, path); err != nil {
			return err
		}
	}
	configPath := filepath.Join(activeHome, "config.yaml")
	configRaw, err := readHermesRuntimeFile(user, runtimeIdentity, configPath, maxHermesRuntimeConfigBytes)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("无法安全检查 Hermes config.yaml：%w", err)
	}
	if err == nil {
		if err := rejectHermesConfigOverrides(configPath, configRaw); err != nil {
			return err
		}
	}
	if managedDir := strings.TrimSpace(environment["HERMES_MANAGED_DIR"]); managedDir != "" && filepath.Clean(managedDir) != "/etc/hermes" {
		return fmt.Errorf("自定义 HERMES_MANAGED_DIR=%q 无法绑定到受信任的应用级环境", managedDir)
	}
	if err := rejectHermesSystemDotEnv("/etc/hermes/.env"); err != nil {
		return err
	}
	managedConfigPath := "/etc/hermes/config.yaml"
	managedRaw, err := readHermesManagedRuntimeFile(managedConfigPath, maxHermesRuntimeConfigBytes)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("无法安全检查 Hermes managed 配置文件 %s：%w", managedConfigPath, err)
	}
	if err == nil {
		if err := rejectHermesConfigOverrides(managedConfigPath, managedRaw); err != nil {
			return err
		}
	}
	if checkRestartPolicy {
		legacyRaw, err := readHermesRuntimeFile(user, runtimeIdentity, filepath.Join(activeHome, "gateway.json"), maxHermesRuntimeConfigBytes)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("无法安全检查 Hermes gateway.json 的重启消息策略：%w", err)
		}
		return validateHermesTelegramRestartConfig(configPath, configRaw, managedRaw, legacyRaw)
	}
	return nil
}

func hermesRuntimeIdentity(target systemdTargetName, identity *persistedUserIdentity, content string) (string, *persistedUserIdentity, error) {
	if target.UserMode {
		if err := verifyTelegramUserIdentity(target, identity); err != nil {
			return "", nil, err
		}
		return target.User, identity, nil
	}
	user, err := effectiveServiceSingleWord(content, "User")
	if err != nil {
		return "", nil, err
	}
	if user == "" {
		user = "root"
	}
	persisted, err := capturePersistedUserIdentity(user, telegramLookupUserIdentity)
	if err != nil {
		return "", nil, fmt.Errorf("无法绑定 Hermes system service 用户 %s：%w", user, err)
	}
	return user, persisted, nil
}

func effectiveServiceSingleWord(content, directive string) (string, error) {
	value := ""
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
		key, raw, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != directive {
			continue
		}
		words, err := splitSystemdWords(raw)
		if err != nil {
			return "", err
		}
		switch len(words) {
		case 0:
			value = ""
		case 1:
			value = words[0]
		default:
			return "", fmt.Errorf("%s 含多个值", directive)
		}
	}
	return value, nil
}

// Derive the installation only from the executable, never from an option value.
func hermesProjectRootFromArgv(argv []string) (string, error) {
	if hermesGatewayArgv(argv) {
		command, ok := systemdDirectExecCommand(argv[0])
		if ok && filepath.IsAbs(command) && filepath.Clean(command) == command {
			bin := filepath.Dir(command)
			venv := filepath.Dir(bin)
			projectRoot := filepath.Dir(venv)
			if filepath.Base(bin) == "bin" && filepath.Base(venv) == "venv" && projectRoot != string(os.PathSeparator) {
				return projectRoot, nil
			}
		}
	}
	return "", fmt.Errorf("无法从 Hermes gateway ExecStart argv[0] 绑定 PROJECT_ROOT")
}

func validHermesProfileName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	first := name[0]
	if !((first >= 'a' && first <= 'z') || (first >= '0' && first <= '9')) {
		return false
	}
	for _, c := range name[1:] {
		if c < 'a' || c > 'z' {
			if c < '0' || c > '9' {
				if c != '_' && c != '-' {
					return false
				}
			}
		}
	}
	return true
}

func readHermesRuntimeFile(user string, expected *persistedUserIdentity, path string, max int64) ([]byte, error) {
	identity, err := verifyPersistedUserIdentity(user, expected, telegramLookupUserIdentity)
	if err != nil {
		return nil, err
	}
	cleanPath, dirFD, err := openUserFileDirForIdentity(user, identity, path, false)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(dirFD)
	return readRegularFileAtNoFollow(dirFD, filepath.Base(cleanPath), max)
}

func rejectHermesRuntimeDotEnv(user string, identity *persistedUserIdentity, path string) error {
	raw, err := readHermesRuntimeFile(user, identity, path, maxHermesRuntimeFileBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("无法安全检查 Hermes 环境文件 %s：%w", path, err)
	}
	return rejectHermesDotEnvKeys(path, raw)
}

func rejectHermesSystemDotEnv(path string) error {
	raw, err := readHermesManagedRuntimeFile(path, maxHermesRuntimeFileBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("无法安全检查 Hermes managed 环境文件 %s：%w", path, err)
	}
	return rejectHermesDotEnvKeys(path, raw)
}

func rejectHermesManagedConfig(path string) error {
	raw, err := readHermesManagedRuntimeFile(path, maxHermesRuntimeConfigBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("无法安全检查 Hermes managed 配置文件 %s：%w", path, err)
	}
	return rejectHermesConfigOverrides(path, raw)
}

// readHermesManagedRuntimeFile validates and reads the same opened inode. The
// directory chain is root-owned and non-writable, so it cannot be swapped after
// validation by an unprivileged Hermes service user.
func readHermesManagedRuntimeFile(path string, max int64) ([]byte, error) {
	dir := filepath.Dir(path)
	if err := validateNoSymlinkComponents(dir, true); err != nil {
		return nil, fmt.Errorf("managed 目录不受信任：%w", err)
	}
	dirFD, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		if err == syscall.ENOENT {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	defer syscall.Close(dirFD)
	if err := validatePrivilegedDirFD(dir, dirFD); err != nil {
		return nil, err
	}
	fd, err := syscall.Openat(dirFD, filepath.Base(path), syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		if err == syscall.ENOENT {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	mode := os.FileMode(stat.Mode)
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Uid != 0 || mode.Perm()&0o022 != 0 {
		return nil, fmt.Errorf("不是 root-owned 且不可由组/其他用户写入的普通文件")
	}
	if max >= 0 && stat.Size > max {
		return nil, fmt.Errorf("文件超过大小限制 %d 字节", max)
	}
	raw, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > max {
		return nil, fmt.Errorf("文件超过大小限制 %d 字节", max)
	}
	return raw, nil
}

func rejectHermesDotEnvKeys(path string, raw []byte) error {
	content, err := decodeHermesDotEnv(raw)
	if err != nil {
		return fmt.Errorf("无法安全解析 Hermes 环境文件 %s 的编码：%w", path, err)
	}
	for _, key := range dotenvDeclaredKeys([]byte(content)) {
		if hermesRuntimeDotEnvKeys[key] {
			return fmt.Errorf("检测到 Hermes 环境文件 %s 声明了会覆盖或绕过代理的 %s", path, key)
		}
	}
	for _, line := range strings.Split(normalizeDotEnvLineEndings(content), "\n") {
		for key := range hermesRuntimeDotEnvKeys {
			if index := strings.Index(line, key+"="); index > 0 {
				return fmt.Errorf("检测到 Hermes 环境文件 %s 含可被修复器拆出的路由变量 %s", path, key)
			}
		}
	}
	return nil
}

func rejectHermesConfigOverrides(path string, raw []byte) error {
	var document map[string]any
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return fmt.Errorf("无法安全解析 Hermes config.yaml %s：%w", path, err)
	}
	if err := rejectHermesMultiplexConfig(document); err != nil {
		return fmt.Errorf("电报代理：Hermes config.yaml %s：%w", path, err)
	}
	for name, value := range document {
		if hermesRuntimeDotEnvKeys[name] && hermesConfigEnvironmentScalar(value) {
			return fmt.Errorf("检测到 Hermes config.yaml %s 的顶层标量 %s 会在启动后覆盖或绕过代理", path, name)
		}
	}
	secretsValue, present := document["secrets"]
	if !present || secretsValue == nil {
		return nil
	}
	secrets, ok := secretsValue.(map[string]any)
	if !ok {
		return fmt.Errorf("检测到 Hermes config.yaml 的 secrets 不是映射")
	}
	for source, rawConfig := range secrets {
		if source == "sources" || rawConfig == nil {
			continue
		}
		config, ok := rawConfig.(map[string]any)
		if !ok {
			return fmt.Errorf("检测到 Hermes secret source %s 配置不是映射", source)
		}
		enabledValue, present := config["enabled"]
		if !present {
			continue
		}
		enabled, ok := enabledValue.(bool)
		if !ok {
			return fmt.Errorf("检测到 Hermes secret source %s 的 enabled 不是布尔值", source)
		}
		if !enabled {
			continue
		}
		if source != "onepassword" {
			return fmt.Errorf("已启用的 Hermes secret source %s 可在启动后注入未知环境变量，拒绝接管", source)
		}
		envValue, present := config["env"]
		if !present || envValue == nil {
			continue
		}
		env, ok := envValue.(map[string]any)
		if !ok {
			return fmt.Errorf("检测到 Hermes onepassword.env 不是映射")
		}
		for name := range env {
			if hermesRuntimeDotEnvKeys[name] {
				return fmt.Errorf("检测到 Hermes onepassword.env 可在启动后覆盖路由变量 %s，拒绝接管", name)
			}
		}
	}
	return nil
}

func hermesConfigEnvironmentScalar(value any) bool {
	switch value.(type) {
	case string, bool, int, int64, uint64, float64:
		return true
	default:
		return false
	}
}
