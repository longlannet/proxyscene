package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// OpenClaw 2026.9.6 exposes a resolved config revision and the applied revision
// through the read-only config.get RPC. A changed hash alone is insufficient:
// the desired proxy, a new channel start and its ready state must also agree.
// Older releases/auth policies may not provide that evidence; they fall back
// to the caller's normal service reconciliation, never to a claimed probe.
const openClawReloadOutputLimit = 8 << 20

var (
	openClawReloadUnitState = func(target systemdTargetName, identity *persistedUserIdentity) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd, _, err := commandUserSystemctlPersisted(ctx, target.User, identity, openClawLookupUserIdentity,
			"show", "--property=ActiveState", "--property=InvocationID", "--", target.Service)
		if err != nil {
			return "", err
		}
		var output openClawReloadBoundedOutput
		cmd.Stdout, cmd.Stderr = &output, io.Discard
		cmd.WaitDelay = 250 * time.Millisecond
		if err := cmd.Run(); err != nil || output.exceeded {
			return "", errors.New("无法取得 OpenClaw 服务状态")
		}
		return string(output.Bytes()), nil
	}
	openClawReloadRPC          = runOpenClawReloadRPC
	openClawReloadTimeout      = 35 * time.Second
	openClawReloadPollInterval = 500 * time.Millisecond
)

type openClawTelegramReloadPlan struct {
	target         string
	identity       persistedUserIdentity
	command        []string
	port           int
	invocation     string
	beforeHash     string
	beforeAccounts map[string]openClawReloadAccount
	inactive       bool
	reason         string
}

type openClawReloadConfig struct {
	Path               string          `json:"path"`
	Valid              bool            `json:"valid"`
	ConfigRevisionHash string          `json:"configRevisionHash"`
	AppliedConfigHash  string          `json:"appliedConfigHash"`
	Config             json.RawMessage `json:"config"`
}

type openClawReloadAccount struct {
	AccountID      string  `json:"accountId"`
	Enabled        *bool   `json:"enabled"`
	Configured     *bool   `json:"configured"`
	Running        *bool   `json:"running"`
	Connected      *bool   `json:"connected"`
	Lifecycle      string  `json:"lifecycle"`
	LastStartAt    int64   `json:"lastStartAt"`
	LastError      *string `json:"lastError"`
	RestartPending bool    `json:"restartPending"`
}

type openClawReloadChannels struct {
	Partial  bool                               `json:"partial"`
	Warnings []json.RawMessage                  `json:"warnings"`
	Accounts map[string][]openClawReloadAccount `json:"channelAccounts"`
}

func parseOpenClawReloadUnitState(raw string) (state, invocation string, err error) {
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || (key != "ActiveState" && key != "InvocationID") {
			continue
		}
		if _, duplicate := values[key]; duplicate {
			return "", "", errors.New("重复的服务状态字段")
		}
		values[key] = value
	}
	state = values["ActiveState"]
	invocation = values["InvocationID"]
	if state != "active" && state != "inactive" && state != "failed" {
		return "", "", errors.New("服务未处于稳定运行或停止状态")
	}
	if state == "active" {
		if len(invocation) != 32 || strings.Trim(invocation, "0123456789abcdef") != "" {
			return "", "", errors.New("缺少有效的服务运行代际")
		}
	}
	return state, invocation, nil
}

// captureOpenClawReloadBaseline records gateway evidence before the config edit.
// Transaction ownership and expected config selection belong to the caller.
func captureOpenClawReloadBaseline(target systemdTargetName, plan *openClawTelegramReloadPlan, path string, raw []byte) *openClawTelegramReloadPlan {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	var config openClawReloadConfig
	if err := openClawReloadRPC(ctx, target, plan, "config.get", &config); err != nil {
		return plan
	}
	if !openClawReloadConfigApplied(config, path) || !openClawReloadProxyEqual(raw, config.Config) {
		return plan
	}
	var channels openClawReloadChannels
	if err := openClawReloadRPC(ctx, target, plan, "channels.status", &channels); err != nil {
		return plan
	}
	accounts, ok := openClawReloadAccountMap(channels)
	if !ok {
		return plan
	}
	plan.beforeHash, plan.beforeAccounts = config.AppliedConfigHash, accounts
	plan.reason = ""
	return plan
}

// observeOpenClawTelegramReload reports only gateway evidence. It never reads
// transaction journals or interprets prepared/restoring phases. Missing or old
// evidence returns false so the transaction layer can choose a safe fallback.
func observeOpenClawTelegramReload(target systemdTargetName, plan *openClawTelegramReloadPlan, path string, expected []byte) bool {
	stateRaw, err := openClawReloadUnitState(target, &plan.identity)
	if err != nil {
		return false
	}
	state, invocation, err := parseOpenClawReloadUnitState(stateRaw)
	if err != nil {
		return false
	}
	if plan.inactive {
		return state != "active"
	}
	if state != "active" || invocation != plan.invocation {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), openClawReloadTimeout)
	defer cancel()
	for {
		var config openClawReloadConfig
		if err := openClawReloadRPC(ctx, target, plan, "config.get", &config); err != nil {
			return false
		}
		if config.AppliedConfigHash != plan.beforeHash && openClawReloadConfigApplied(config, path) && openClawReloadProxyEqual(expected, config.Config) {
			var channels openClawReloadChannels
			if err := openClawReloadRPC(ctx, target, plan, "channels.status", &channels); err != nil {
				return false
			}
			if openClawReloadAccountsReady(plan.beforeAccounts, channels) {
				// Bind the channel status to the same applied generation. The
				// transaction layer checks disk bytes and ownership after return.
				var confirmed openClawReloadConfig
				if err := openClawReloadRPC(ctx, target, plan, "config.get", &confirmed); err != nil {
					return false
				}
				if openClawReloadConfigApplied(confirmed, path) && confirmed.AppliedConfigHash == config.AppliedConfigHash && openClawReloadProxyEqual(expected, confirmed.Config) {
					stateRaw, err := openClawReloadUnitState(target, &plan.identity)
					if err != nil {
						return false
					}
					state, invocation, err := parseOpenClawReloadUnitState(stateRaw)
					return err == nil && state == "active" && invocation == plan.invocation
				}
			}
		}
		timer := time.NewTimer(openClawReloadPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

func openClawReloadConfigApplied(config openClawReloadConfig, path string) bool {
	return config.Valid && config.Path == path && config.ConfigRevisionHash != "" && len(config.ConfigRevisionHash) <= 256 &&
		config.ConfigRevisionHash == config.AppliedConfigHash && len(config.Config) > 0
}

func openClawReloadProxyEqual(expected, observed []byte) bool {
	left, leftPresent, err := openClawTelegramProxyRawValue(expected)
	if err != nil {
		return false
	}
	right, rightPresent, err := openClawTelegramProxyRawValue(observed)
	if err != nil {
		return false
	}
	if rejectOpenClawAccountProxyOverrides(observed) != nil {
		return false
	}
	return proxyStatesEqual(left, leftPresent, right, rightPresent)
}

func openClawReloadAccountMap(channels openClawReloadChannels) (map[string]openClawReloadAccount, bool) {
	accounts, present := channels.Accounts["telegram"]
	if !present || len(accounts) == 0 || channels.Partial || len(channels.Warnings) != 0 {
		return nil, false
	}
	result := make(map[string]openClawReloadAccount, len(accounts))
	for _, account := range accounts {
		if account.AccountID == "" || account.Enabled == nil || account.Configured == nil || account.Running == nil {
			return nil, false
		}
		if _, duplicate := result[account.AccountID]; duplicate {
			return nil, false
		}
		result[account.AccountID] = account
	}
	return result, true
}

func openClawReloadAccountsReady(before map[string]openClawReloadAccount, channels openClawReloadChannels) bool {
	after, ok := openClawReloadAccountMap(channels)
	if !ok || len(after) != len(before) {
		return false
	}
	for id, account := range after {
		old, present := before[id]
		if !present || *old.Enabled != *account.Enabled || *old.Configured != *account.Configured {
			return false
		}
		if !*account.Enabled || !*account.Configured {
			if *account.Running {
				return false
			}
			continue
		}
		if !*account.Running || account.Connected == nil || !*account.Connected || account.Lifecycle != "ready" || account.RestartPending ||
			account.LastStartAt <= old.LastStartAt || account.LastStartAt <= 0 || (account.LastError != nil && *account.LastError != "") {
			return false
		}
	}
	return true
}

// Use the service's absolute interpreter and entrypoint; never resolve a CLI on
// root's PATH, copy runtime injection environment or pass authentication args.
// --expect-url refuses a configured remote endpoint without changing auth.
func openClawReloadCommand(argv []string, raw []byte) ([]string, int, error) {
	if !openClawGatewayArgv(argv) {
		return nil, 0, errors.New("不支持的 OpenClaw 启动命令")
	}
	gateway := -1
	for i, arg := range argv {
		if arg == "gateway" {
			gateway = i
			break
		}
	}
	if gateway < 2 {
		return nil, 0, errors.New("缺少 OpenClaw 入口")
	}
	var cfg struct {
		Gateway struct {
			Port   int    `json:"port"`
			Mode   string `json:"mode"`
			Reload struct {
				Mode string `json:"mode"`
			} `json:"reload"`
			TLS struct {
				Enabled bool `json:"enabled"`
			} `json:"tls"`
		} `json:"gateway"`
	}
	if err := decodeOpenClawJSON5Value(raw, &cfg); err != nil {
		return nil, 0, errors.New("无法解析重载设置")
	}
	if cfg.Gateway.Mode == "remote" || cfg.Gateway.TLS.Enabled || (cfg.Gateway.Reload.Mode != "" && cfg.Gateway.Reload.Mode != "hybrid") {
		return nil, 0, errors.New("网关没有受支持的本地频道重载")
	}
	port := cfg.Gateway.Port
	if port == 0 {
		port = 18789
	}
	for i := gateway + 1; i < len(argv); i++ {
		arg := argv[i]
		if arg == "run" && i == gateway+1 {
			continue
		}
		if arg == "--port" && i+1 < len(argv) {
			i++
			var err error
			port, err = strconv.Atoi(argv[i])
			if err != nil {
				return nil, 0, err
			}
			continue
		}
		if strings.HasPrefix(arg, "--port=") {
			var err error
			port, err = strconv.Atoi(strings.TrimPrefix(arg, "--port="))
			if err != nil {
				return nil, 0, err
			}
			continue
		}
		// Other gateway flags may override authentication, bind mode or config.
		return nil, 0, errors.New("网关命令含未建模的运行参数")
	}
	if port < 1 || port > 65535 {
		return nil, 0, errors.New("无效的网关端口")
	}
	command := append([]string(nil), argv[:gateway]...)
	resolved, ok := systemdDirectExecCommand(command[0])
	if !ok {
		return nil, 0, errors.New("无效的解释器")
	}
	command[0] = resolved
	return command, port, nil
}

type openClawReloadBoundedOutput struct {
	buffer   bytes.Buffer
	exceeded bool
}

func (b *openClawReloadBoundedOutput) Bytes() []byte { return b.buffer.Bytes() }
func (b *openClawReloadBoundedOutput) Len() int      { return b.buffer.Len() }

func (b *openClawReloadBoundedOutput) Write(p []byte) (int, error) {
	if len(p) > openClawReloadOutputLimit-b.Len() {
		b.exceeded = true
		return 0, errors.New("OpenClaw 状态输出过大")
	}
	return b.buffer.Write(p)
}

func runOpenClawReloadRPC(ctx context.Context, target systemdTargetName, plan *openClawTelegramReloadPlan, method string, result any) error {
	if len(plan.command) < 2 || (method != "config.get" && method != "channels.status") {
		return errors.New("不支持的只读状态调用")
	}
	configPath := filepath.Join(plan.identity.Home, ".openclaw", "openclaw.json")
	raw, err := readOpenClawUserConfig(target.User, &plan.identity, configPath)
	if err != nil || rejectOpenClawRuntimeConfigSelectors(raw) != nil || rejectOpenClawAccountProxyOverrides(raw) != nil {
		return errors.New("OpenClaw 状态调用的配置不再受支持")
	}
	if err := validateOpenClawReloadRPCBinding(target, plan, raw); err != nil {
		return err
	}
	if plan.identity.UID == 0 {
		// A root-owned gateway is already trusted to run its own dependencies and
		// plugins. Do not introduce a new privilege boundary for interpreter or
		// entrypoint replacement; other users always execute under their own UID.
		for _, path := range []string{plan.command[0], plan.command[len(plan.command)-1], plan.identity.Home, filepath.Join(plan.identity.Home, ".openclaw", "openclaw.json")} {
			if err := validateOpenClawReloadRootPath(path); err != nil {
				return err
			}
		}
	}
	params := "{}"
	if method == "channels.status" {
		params = `{"channel":"telegram","probe":false}`
	}
	args := append([]string(nil), plan.command[1:]...)
	args = append(args, "gateway", "call", method, "--params", params, "--json", "--timeout", "4000", "--expect-url", "ws://127.0.0.1:"+strconv.Itoa(plan.port))
	callCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	cmd, _, err := commandAsPersistedUser(callCtx, target.User, &plan.identity, openClawLookupUserIdentity, plan.command[0], args...)
	if err != nil {
		return err
	}
	cmd.Env = append(cmd.Env, "OPENCLAW_GATEWAY_PORT="+strconv.Itoa(plan.port), "NO_COLOR=1")
	var output openClawReloadBoundedOutput
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 250 * time.Millisecond
	if err := cmd.Run(); err != nil || output.exceeded {
		return errors.New("无法取得 OpenClaw 只读状态")
	}
	if err := json.Unmarshal(output.Bytes(), result); err != nil {
		return errors.New("OpenClaw 只读状态格式不受支持")
	}
	return nil
}

func validateOpenClawReloadRootPath(path string) error {
	if err := validateNoSymlinkComponents(path, true); err != nil {
		return errors.New("root 网关状态调用路径不安全")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("root 网关状态调用路径不可用")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || info.Mode().Perm()&0o022 != 0 || (!info.Mode().IsRegular() && !info.IsDir()) {
		return fmt.Errorf("root 网关状态调用路径不是受保护的文件或目录")
	}
	return nil
}

// Recheck the original local launch binding without another systemd subprocess.
// The CLI receives a cleaned environment, not the user manager's environment.
func validateOpenClawReloadRPCBinding(target systemdTargetName, plan *openClawTelegramReloadPlan, raw []byte) error {
	if err := verifyOpenClawUserIdentity(target.User, &plan.identity); err != nil {
		return errors.New("OpenClaw 状态调用身份已变化")
	}
	if err := validateOpenClawCanonicalConfigCandidate(target.User, &plan.identity); err != nil {
		return errors.New("OpenClaw 默认配置绑定已变化")
	}
	if err := validateOpenClawRuntimeDotEnvFiles(target.User, &plan.identity); err != nil {
		return errors.New("OpenClaw 配置环境绑定已变化")
	}
	content, err := effectiveTelegramTargetUnitContent(target, &plan.identity)
	if err != nil || validateOpenClawEffectiveUnit(content, plan.identity.Home) != nil {
		return errors.New("OpenClaw 状态调用服务绑定已变化")
	}
	starts, err := effectiveServiceExecStarts(content)
	if err != nil || len(starts) != 1 {
		return errors.New("OpenClaw 状态调用启动命令已变化")
	}
	command, port, err := openClawReloadCommand(starts[0], raw)
	if err != nil || !slices.Equal(command, plan.command) || port != plan.port {
		return errors.New("OpenClaw 状态调用入口或端口已变化")
	}
	return nil
}
