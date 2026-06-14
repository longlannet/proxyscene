package manager

import (
	"fmt"
	"path/filepath"
	"time"
)

const Version = "0.2.0"

type Scene string

const (
	SceneGlobal   Scene = "global"
	SceneDev      Scene = "dev"
	SceneTelegram Scene = "telegram"
)

type Config struct {
	CoreDir          string
	ProxyHost        string
	DevHTTPPort      int
	TGHTTPPort       int
	TGSocksPort      int
	GlobalHTTPPort   int
	GlobalSocksPort  int
	TransparentPort  int
	RedirectPort     int
	TransparentIPv6  int
	RedirectIPv6     int
	TProxyMark       int
	OutboundMark     int
	InstallBin       string
	SystemdService   string
	RestoreService   string
	XrayServiceUser  string
	TGTargetServices []string
	DevTargetUser    string
}

func DefaultConfig() Config {
	return Config{
		CoreDir:          envString("XRAY_PROXY_MANAGER_DIR", "/opt/xray-proxy-manager"),
		ProxyHost:        envString("XRAY_PROXY_HOST", "127.0.0.1"),
		DevHTTPPort:      envInt("XRAY_DEV_HTTP_PORT", 7891),
		TGHTTPPort:       envIntAny([]string{"TG_HTTP_PROXY_PORT", "XRAY_TG_HTTP_PORT"}, 7892),
		TGSocksPort:      envIntAny([]string{"TG_SOCKS_PROXY_PORT", "XRAY_TG_SOCKS_PORT"}, 7893),
		GlobalHTTPPort:   envIntAny([]string{"GLOBAL_HTTP_PROXY_PORT", "XRAY_GLOBAL_HTTP_PORT"}, 7890),
		GlobalSocksPort:  envIntAny([]string{"GLOBAL_SOCKS_PROXY_PORT", "XRAY_GLOBAL_SOCKS_PORT"}, 7894),
		TransparentPort:  envInt("XRAY_GLOBAL_TRANSPARENT_PORT", 7895),
		RedirectPort:     envInt("XRAY_GLOBAL_REDIRECT_PORT", 7896),
		TransparentIPv6:  envInt("XRAY_GLOBAL_TRANSPARENT_IPV6_PORT", 7897),
		RedirectIPv6:     envInt("XRAY_GLOBAL_REDIRECT_IPV6_PORT", 7898),
		TProxyMark:       envInt("XRAY_TPROXY_MARK", 1),
		OutboundMark:     envInt("XRAY_OUTBOUND_MARK", 255),
		InstallBin:       envString("XRAY_PROXY_SWITCH_BIN", "/usr/local/bin/xray-proxy"),
		SystemdService:   envString("XRAY_SYSTEMD_SERVICE_NAME", "xray-proxy-manager.service"),
		RestoreService:   envString("XRAY_PROXY_BOOT_RESTORE_SERVICE_NAME", "xray-proxy-state.service"),
		XrayServiceUser:  envString("XRAY_PROXY_SERVICE_USER", "xray-proxy"),
		TGTargetServices: splitFields(envString("TG_PROXY_SERVICES", "openclaw hermes hermes-gateway user:root:hermes-gateway")),
		DevTargetUser:    envString("DEV_PROXY_TARGET_USER", ""),
	}
}

func (c Config) Validate() error {
	if err := safeCoreDir(c.CoreDir, "XRAY_PROXY_MANAGER_DIR"); err != nil {
		return err
	}
	if err := safePath(c.InstallBin, "XRAY_PROXY_SWITCH_BIN", true); err != nil {
		return err
	}
	if err := safeSystemdServiceName(c.SystemdService); err != nil {
		return err
	}
	if err := safeSystemdServiceName(c.RestoreService); err != nil {
		return err
	}
	if err := validateProxyHost(c.ProxyHost); err != nil {
		return err
	}
	if c.XrayServiceUser == "" {
		return fmt.Errorf("XRAY_PROXY_SERVICE_USER 不能为空")
	}
	if err := validateUserName(c.XrayServiceUser); err != nil {
		return err
	}
	ports := map[string]int{
		"XRAY_DEV_HTTP_PORT":                c.DevHTTPPort,
		"XRAY_TG_HTTP_PORT":                 c.TGHTTPPort,
		"XRAY_TG_SOCKS_PORT":                c.TGSocksPort,
		"XRAY_GLOBAL_HTTP_PORT":             c.GlobalHTTPPort,
		"XRAY_GLOBAL_SOCKS_PORT":            c.GlobalSocksPort,
		"XRAY_GLOBAL_TRANSPARENT_PORT":      c.TransparentPort,
		"XRAY_GLOBAL_REDIRECT_PORT":         c.RedirectPort,
		"XRAY_GLOBAL_TRANSPARENT_IPV6_PORT": c.TransparentIPv6,
		"XRAY_GLOBAL_REDIRECT_IPV6_PORT":    c.RedirectIPv6,
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
	if c.TProxyMark <= 0 || c.OutboundMark <= 0 {
		return fmt.Errorf("mark 配置必须为正整数")
	}
	for _, svc := range c.TGTargetServices {
		if svc == "" {
			continue
		}
		if err := safeSystemdTargetName(svc); err != nil {
			return err
		}
	}
	if err := validateUserName(c.DevTargetUser); err != nil {
		return err
	}
	return nil
}

func (c Config) XrayBin() string       { return filepath.Join(c.CoreDir, "xray") }
func (c Config) XrayConfig() string    { return filepath.Join(c.CoreDir, "config.json") }
func (c Config) StorePath() string     { return filepath.Join(c.CoreDir, "state.json") }
func (c Config) StoreLockPath() string { return filepath.Join(c.CoreDir, ".state.lock") }
func (c Config) LogPath() string       { return filepath.Join(c.CoreDir, "xray.log") }
func (c Config) MarkerPath() string    { return filepath.Join(c.CoreDir, ".managed-by-xray-proxy-go") }
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
func (c Config) HTTPAddr(scene Scene) string {
	switch scene {
	case SceneDev:
		return "http://" + c.ProxyHost + ":" + itoa(c.DevHTTPPort)
	case SceneTelegram:
		return "http://" + c.ProxyHost + ":" + itoa(c.TGHTTPPort)
	default:
		return "http://" + c.ProxyHost + ":" + itoa(c.GlobalHTTPPort)
	}
}
func (c Config) TGSocksAddr() string { return "socks5h://" + c.ProxyHost + ":" + itoa(c.TGSocksPort) }
func (c Config) GlobalSocksAddr() string {
	return "socks5h://" + c.ProxyHost + ":" + itoa(c.GlobalSocksPort)
}

type Node struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Protocol  string    `json:"protocol"`
	RawURL    string    `json:"raw_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
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
