package manager

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Version 和 Commit 可在构建时通过 -ldflags "-X proxyscene/internal/manager.Version=..."
// 注入（release 工作流会用 git tag 和 commit 覆盖）；源码直接构建时使用下面的默认值。
var (
	Version = "dev"
	Commit  = ""
)

// VersionString 返回带可选 commit 短哈希的版本字符串。
func VersionString() string {
	if Commit == "" {
		return Version
	}
	short := Commit
	if len(short) > 12 {
		short = short[:12]
	}
	return Version + " (" + short + ")"
}

type Scene string

const (
	SceneGlobal   Scene = "global"
	SceneDev      Scene = "dev"
	SceneTelegram Scene = "telegram"
)

type Config struct {
	CoreDir              string
	ProxyHost            string
	DevHTTPPort          int
	TGHTTPPort           int
	TGSocksPort          int
	GlobalHTTPPort       int
	GlobalSocksPort      int
	InstallBin           string
	SystemdService       string
	RestoreService       string
	XrayServiceUser      string
	TGTargetServices     []string
	DevTargetUser        string
	TestURL              string
	ManageOpenClawConfig bool
	AllowPublicBind      bool
	runtimeOverrides     runtimeConfigOverrideMask
}

func DefaultConfig() Config {
	const defaultTGTargets = "hermes-gateway user:root:hermes-gateway"
	cfg := Config{
		CoreDir:         envString("PROXYSCENE_MANAGER_DIR", "/opt/proxyscene"),
		ProxyHost:       "127.0.0.1",
		DevHTTPPort:     7891,
		TGHTTPPort:      7892,
		TGSocksPort:     7893,
		GlobalHTTPPort:  7890,
		GlobalSocksPort: 7894,
		InstallBin:      envString("PROXYSCENE_SWITCH_BIN", "/usr/local/bin/proxyscene"),
		SystemdService:  envString("PROXYSCENE_SYSTEMD_SERVICE_NAME", "proxyscene.service"),
		RestoreService:  envString("PROXYSCENE_BOOT_RESTORE_SERVICE_NAME", "proxyscene-restore.service"),
		XrayServiceUser: "proxyscene",
		// 默认锚定规范的系统级 hermes 网关和 root 的用户级 hermes 网关；openclaw 网关、
		// hermes 的 profile 实例、其它用户级单元都由精确自动发现覆盖（见 telegram_discovery.go）。
		// applyTelegram 对不存在的目标会跳过注入（不生成 phantom drop-in），因此这里锚定
		// root 用户级 hermes 不会在未安装时留下残留 drop-in。
		TGTargetServices:     splitFields(defaultTGTargets),
		TestURL:              envString("PROXYSCENE_TEST_URL", "https://www.google.com/generate_204"),
		ManageOpenClawConfig: true,
	}

	cfg.ProxyHost, cfg.runtimeOverrides.ProxyHost = runtimeStringEnv("PROXYSCENE_HOST", cfg.ProxyHost)
	cfg.DevHTTPPort, cfg.runtimeOverrides.DevHTTPPort = runtimePortEnv("PROXYSCENE_DEV_HTTP_PORT", cfg.DevHTTPPort)
	cfg.TGHTTPPort, cfg.runtimeOverrides.TGHTTPPort = runtimePortEnv("PROXYSCENE_TG_HTTP_PORT", cfg.TGHTTPPort)
	cfg.TGSocksPort, cfg.runtimeOverrides.TGSocksPort = runtimePortEnv("PROXYSCENE_TG_SOCKS_PORT", cfg.TGSocksPort)
	cfg.GlobalHTTPPort, cfg.runtimeOverrides.GlobalHTTPPort = runtimePortEnv("PROXYSCENE_GLOBAL_HTTP_PORT", cfg.GlobalHTTPPort)
	cfg.GlobalSocksPort, cfg.runtimeOverrides.GlobalSocksPort = runtimePortEnv("PROXYSCENE_GLOBAL_SOCKS_PORT", cfg.GlobalSocksPort)
	cfg.XrayServiceUser, cfg.runtimeOverrides.XrayServiceUser = runtimeStringEnv("PROXYSCENE_SERVICE_USER", cfg.XrayServiceUser)
	if raw, explicit := runtimeOptionalStringEnv("PROXYSCENE_TG_SERVICES", defaultTGTargets); explicit {
		cfg.TGTargetServices = splitFields(raw)
		cfg.runtimeOverrides.TGTargetServices = true
	}
	cfg.DevTargetUser, cfg.runtimeOverrides.DevTargetUser = runtimeOptionalStringEnv("PROXYSCENE_DEV_TARGET_USER", "")
	cfg.ManageOpenClawConfig, cfg.runtimeOverrides.ManageOpenClawConfig = runtimeBoolEnv("PROXYSCENE_MANAGE_OPENCLAW_CONFIG", true)
	cfg.AllowPublicBind, cfg.runtimeOverrides.AllowPublicBind = runtimeBoolEnv("PROXYSCENE_ALLOW_PUBLIC_BIND", false)
	if cfg.runtimeOverrides.ProxyHost && net.ParseIP(cfg.ProxyHost) == nil {
		warnInvalidRuntimeOverride("PROXYSCENE_HOST", cfg.ProxyHost)
		cfg.ProxyHost = "127.0.0.1"
		cfg.runtimeOverrides.ProxyHost = false
	}
	if cfg.runtimeOverrides.XrayServiceUser && validateUserName(cfg.XrayServiceUser) != nil {
		warnInvalidRuntimeOverride("PROXYSCENE_SERVICE_USER", cfg.XrayServiceUser)
		cfg.XrayServiceUser = "proxyscene"
		cfg.runtimeOverrides.XrayServiceUser = false
	}
	if cfg.runtimeOverrides.DevTargetUser && validateUserName(cfg.DevTargetUser) != nil {
		warnInvalidRuntimeOverride("PROXYSCENE_DEV_TARGET_USER", cfg.DevTargetUser)
		cfg.DevTargetUser = ""
		cfg.runtimeOverrides.DevTargetUser = false
	}
	if cfg.runtimeOverrides.TGTargetServices {
		for _, target := range cfg.TGTargetServices {
			if safeSystemdTargetName(target) != nil {
				warnInvalidRuntimeOverride("PROXYSCENE_TG_SERVICES", strings.Join(cfg.TGTargetServices, " "))
				cfg.TGTargetServices = splitFields(defaultTGTargets)
				cfg.runtimeOverrides.TGTargetServices = false
				break
			}
		}
	}
	return cfg
}

func (c Config) ValidateLocators() error {
	if err := safeCoreDir(c.CoreDir, "PROXYSCENE_MANAGER_DIR"); err != nil {
		return err
	}
	if err := safePath(c.InstallBin, "PROXYSCENE_SWITCH_BIN", true); err != nil {
		return err
	}
	if err := safeSystemdServiceName(c.SystemdService); err != nil {
		return err
	}
	if err := safeSystemdServiceName(c.RestoreService); err != nil {
		return err
	}
	if c.SystemdService == c.RestoreService {
		return fmt.Errorf("配置中的 Xray 主服务和开机恢复服务不能使用同一个 systemd unit：%s", c.SystemdService)
	}
	if err := safeProxysceneServiceName(c.SystemdService); err != nil {
		return err
	}
	if err := safeProxysceneServiceName(c.RestoreService); err != nil {
		return err
	}
	return nil
}

func (c Config) ValidateRuntime() error {
	if err := validateProxyHost(c.ProxyHost, c.AllowPublicBind); err != nil {
		return err
	}
	if c.XrayServiceUser == "" {
		return fmt.Errorf("PROXYSCENE_SERVICE_USER 不能为空")
	}
	if err := validateUserName(c.XrayServiceUser); err != nil {
		return err
	}
	ports := map[string]int{
		"PROXYSCENE_DEV_HTTP_PORT":     c.DevHTTPPort,
		"PROXYSCENE_TG_HTTP_PORT":      c.TGHTTPPort,
		"PROXYSCENE_TG_SOCKS_PORT":     c.TGSocksPort,
		"PROXYSCENE_GLOBAL_HTTP_PORT":  c.GlobalHTTPPort,
		"PROXYSCENE_GLOBAL_SOCKS_PORT": c.GlobalSocksPort,
	}
	seen := map[int]string{}
	for name, port := range ports {
		if err := validPort(port, name); err != nil {
			return err
		}
		if old := seen[port]; old != "" {
			return fmt.Errorf("端口冲突：%s 和 %s 都使用 %d", old, name, port)
		}
		seen[port] = name
	}
	if err := validateTestURL(c.TestURL); err != nil {
		return err
	}
	if len(c.TGTargetServices) > maxTelegramTargets {
		return fmt.Errorf("PROXYSCENE_TG_SERVICES 目标数超过上限 %d", maxTelegramTargets)
	}
	seenTargets := make(map[string]bool, len(c.TGTargetServices))
	for _, svc := range c.TGTargetServices {
		if svc == "" || seenTargets[svc] {
			return fmt.Errorf("PROXYSCENE_TG_SERVICES 含空值或重复目标")
		}
		if err := safeSystemdTargetName(svc); err != nil {
			return err
		}
		seenTargets[svc] = true
	}
	if err := validateUserName(c.DevTargetUser); err != nil {
		return err
	}
	return nil
}

func (c Config) Validate() error {
	if err := c.ValidateLocators(); err != nil {
		return err
	}
	return c.ValidateRuntime()
}

const runtimeConfigVersion = 1

type RuntimeConfig struct {
	Version              int      `json:"version"`
	ProxyHost            string   `json:"proxy_host"`
	DevHTTPPort          int      `json:"dev_http_port"`
	TGHTTPPort           int      `json:"tg_http_port"`
	TGSocksPort          int      `json:"tg_socks_port"`
	GlobalHTTPPort       int      `json:"global_http_port"`
	GlobalSocksPort      int      `json:"global_socks_port"`
	XrayServiceUser      string   `json:"xray_service_user"`
	TGTargetServices     []string `json:"telegram_target_services"`
	DevTargetUser        string   `json:"dev_target_user"`
	ManageOpenClawConfig bool     `json:"manage_openclaw_config"`
	AllowPublicBind      bool     `json:"allow_public_bind"`
}

type runtimeConfigOverrideMask struct {
	ProxyHost            bool
	DevHTTPPort          bool
	TGHTTPPort           bool
	TGSocksPort          bool
	GlobalHTTPPort       bool
	GlobalSocksPort      bool
	XrayServiceUser      bool
	TGTargetServices     bool
	DevTargetUser        bool
	ManageOpenClawConfig bool
	AllowPublicBind      bool
}

func (m runtimeConfigOverrideMask) any() bool {
	return m.ProxyHost || m.DevHTTPPort || m.TGHTTPPort || m.TGSocksPort ||
		m.GlobalHTTPPort || m.GlobalSocksPort || m.XrayServiceUser ||
		m.TGTargetServices || m.DevTargetUser || m.ManageOpenClawConfig ||
		m.AllowPublicBind
}

func runtimeStringEnv(key, fallback string) (string, bool) {
	raw, ok := os.LookupEnv(key)
	value := strings.TrimSpace(raw)
	if !ok || value == "" {
		return fallback, false
	}
	return value, true
}

func runtimeOptionalStringEnv(key, fallback string) (string, bool) {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return fallback, false
	}
	return strings.TrimSpace(raw), true
}

func warnInvalidRuntimeOverride(key, value string) {
	fmt.Printf("警告：环境变量 %s 的值无效（%q），忽略本次覆盖\n", key, value)
}

func runtimePortEnv(key string, fallback int) (int, bool) {
	raw, ok := os.LookupEnv(key)
	value := strings.TrimSpace(raw)
	if !ok || value == "" {
		return fallback, false
	}
	port, err := strconv.Atoi(value)
	if err != nil || port <= 0 || port > 65535 {
		fmt.Printf("警告：环境变量 %s 的值无效（%q），使用默认值 %d\n", key, value, fallback)
		return fallback, false
	}
	return port, true
}

func runtimeBoolEnv(key string, fallback bool) (bool, bool) {
	raw, ok := os.LookupEnv(key)
	value := strings.ToLower(strings.TrimSpace(raw))
	if !ok || value == "" {
		return fallback, false
	}
	switch value {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	default:
		fmt.Printf("警告：环境变量 %s 的值无法识别（%q），使用默认值 %v\n", key, raw, fallback)
		return fallback, false
	}
}

func (c Config) runtimeConfig() *RuntimeConfig {
	return &RuntimeConfig{
		Version:              runtimeConfigVersion,
		ProxyHost:            c.ProxyHost,
		DevHTTPPort:          c.DevHTTPPort,
		TGHTTPPort:           c.TGHTTPPort,
		TGSocksPort:          c.TGSocksPort,
		GlobalHTTPPort:       c.GlobalHTTPPort,
		GlobalSocksPort:      c.GlobalSocksPort,
		XrayServiceUser:      c.XrayServiceUser,
		TGTargetServices:     append([]string(nil), c.TGTargetServices...),
		DevTargetUser:        c.DevTargetUser,
		ManageOpenClawConfig: c.ManageOpenClawConfig,
		AllowPublicBind:      c.AllowPublicBind,
	}
}

func (r *RuntimeConfig) applyTo(c Config) Config {
	if r == nil {
		return c
	}
	c.ProxyHost = r.ProxyHost
	c.DevHTTPPort = r.DevHTTPPort
	c.TGHTTPPort = r.TGHTTPPort
	c.TGSocksPort = r.TGSocksPort
	c.GlobalHTTPPort = r.GlobalHTTPPort
	c.GlobalSocksPort = r.GlobalSocksPort
	c.XrayServiceUser = r.XrayServiceUser
	c.TGTargetServices = append([]string(nil), r.TGTargetServices...)
	c.DevTargetUser = r.DevTargetUser
	c.ManageOpenClawConfig = r.ManageOpenClawConfig
	c.AllowPublicBind = r.AllowPublicBind
	return c
}

func (c Config) applyRuntimeOverrides(from Config) Config {
	mask := from.runtimeOverrides
	if mask.ProxyHost {
		c.ProxyHost = from.ProxyHost
	}
	if mask.DevHTTPPort {
		c.DevHTTPPort = from.DevHTTPPort
	}
	if mask.TGHTTPPort {
		c.TGHTTPPort = from.TGHTTPPort
	}
	if mask.TGSocksPort {
		c.TGSocksPort = from.TGSocksPort
	}
	if mask.GlobalHTTPPort {
		c.GlobalHTTPPort = from.GlobalHTTPPort
	}
	if mask.GlobalSocksPort {
		c.GlobalSocksPort = from.GlobalSocksPort
	}
	if mask.XrayServiceUser {
		c.XrayServiceUser = from.XrayServiceUser
	}
	if mask.TGTargetServices {
		c.TGTargetServices = append([]string(nil), from.TGTargetServices...)
	}
	if mask.DevTargetUser {
		c.DevTargetUser = from.DevTargetUser
	}
	if mask.ManageOpenClawConfig {
		c.ManageOpenClawConfig = from.ManageOpenClawConfig
	}
	if mask.AllowPublicBind {
		c.AllowPublicBind = from.AllowPublicBind
	}
	return c
}

func runtimeConfigsEqual(left, right *RuntimeConfig) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Version == right.Version &&
		left.ProxyHost == right.ProxyHost &&
		left.DevHTTPPort == right.DevHTTPPort &&
		left.TGHTTPPort == right.TGHTTPPort &&
		left.TGSocksPort == right.TGSocksPort &&
		left.GlobalHTTPPort == right.GlobalHTTPPort &&
		left.GlobalSocksPort == right.GlobalSocksPort &&
		left.XrayServiceUser == right.XrayServiceUser &&
		slices.Equal(left.TGTargetServices, right.TGTargetServices) &&
		left.DevTargetUser == right.DevTargetUser &&
		left.ManageOpenClawConfig == right.ManageOpenClawConfig &&
		left.AllowPublicBind == right.AllowPublicBind
}

func (c Config) XrayBin() string         { return filepath.Join(c.CoreDir, "xray") }
func (c Config) XrayConfig() string      { return filepath.Join(c.CoreDir, "config.json") }
func (c Config) StorePath() string       { return filepath.Join(c.CoreDir, "state.json") }
func (c Config) StoreLockPath() string   { return filepath.Join(c.CoreDir, ".state.lock") }
func (c Config) StoreBackupPath() string { return filepath.Join(c.CoreDir, "state.json.bak") }
func (c Config) MarkerPath() string      { return filepath.Join(c.CoreDir, ".managed-by-proxyscene") }
func (c Config) InstallationOwnershipPath() string {
	return filepath.Join(c.CoreDir, "installation-ownership.json")
}
func (c Config) DevBackupPath() string { return filepath.Join(c.CoreDir, "dev-proxy-backup.json") }
func (c Config) TelegramDropInDir(s string) string {
	return filepath.Join("/etc/systemd/system", normalizeSystemdServiceName(s)+".d")
}
func (c Config) TelegramDropInPath(s string) string {
	return filepath.Join(c.TelegramDropInDir(s), "10-openclaw-hermes-telegram-proxy.conf")
}
func (c Config) UserTelegramDropInDir(userName, service string) (string, error) {
	home, err := userHomeDir(userName)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config/systemd/user", normalizeSystemdServiceName(service)+".d"), nil
}
func (c Config) UserTelegramDropInPath(userName, service string) (string, error) {
	dir, err := c.UserTelegramDropInDir(userName, service)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "10-openclaw-hermes-telegram-proxy.conf"), nil
}

// HTTPAddr 等用 net.JoinHostPort 拼地址：IPv6 字面量会自动加方括号（http://[::1]:7890）。
func (c Config) HTTPAddr(scene Scene) string {
	switch scene {
	case SceneDev:
		return "http://" + net.JoinHostPort(c.ProxyHost, itoa(c.DevHTTPPort))
	case SceneTelegram:
		return "http://" + net.JoinHostPort(c.ProxyHost, itoa(c.TGHTTPPort))
	default:
		return "http://" + net.JoinHostPort(c.ProxyHost, itoa(c.GlobalHTTPPort))
	}
}
func (c Config) TGSocksAddr() string {
	return "socks5h://" + net.JoinHostPort(c.ProxyHost, itoa(c.TGSocksPort))
}
func (c Config) GlobalSocksAddr() string {
	return "socks5h://" + net.JoinHostPort(c.ProxyHost, itoa(c.GlobalSocksPort))
}

// needsPrivilegedPortCap 报告是否有监听端口低于 1024（绑定需要 CAP_NET_BIND_SERVICE）。
// 默认端口均为 789x，不需要任何 capability；仅在操作员显式配置特权端口时才授予。
func (c Config) needsPrivilegedPortCap() bool {
	for _, port := range []int{c.DevHTTPPort, c.TGHTTPPort, c.TGSocksPort, c.GlobalHTTPPort, c.GlobalSocksPort} {
		if port < 1024 {
			return true
		}
	}
	return false
}

type Node struct {
	ID                  string    `json:"id"`
	Name                string    `json:"name"`
	Protocol            string    `json:"protocol"`
	RawURL              string    `json:"raw_url"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
	SubscriptionIDs     []string  `json:"subscription_ids,omitempty"`
	SubscriptionManaged bool      `json:"subscription_managed,omitempty"`
}

type SpeedResult struct {
	NodeID    string    `json:"node_id"`
	Target    string    `json:"target"`
	LatencyMS int64     `json:"latency_ms"`
	Success   bool      `json:"success"`
	TestedAt  time.Time `json:"tested_at"`
	Error     string    `json:"error,omitempty"`
}

type Store struct {
	Generation      uint64                 `json:"generation,omitempty"`
	RuntimeConfig   *RuntimeConfig         `json:"runtime_config,omitempty"`
	Nodes           []Node                 `json:"nodes"`
	DefaultNodeID   string                 `json:"default_node_id"`
	SceneNodes      map[Scene]string       `json:"scene_nodes"`
	SceneEnabled    map[Scene]bool         `json:"scene_enabled"`
	Subscriptions   []string               `json:"subscriptions"`
	SpeedResults    map[string]SpeedResult `json:"speed_results"`
	TelegramTargets []string               `json:"telegram_targets,omitempty"`
}

func newStore() *Store {
	return &Store{
		SceneNodes:   map[Scene]string{},
		SceneEnabled: map[Scene]bool{},
		SpeedResults: map[string]SpeedResult{},
	}
}
