package manager

import (
	"fmt"
	"path/filepath"
	"time"
)

// Version 和 Commit 可在构建时通过 -ldflags "-X proxyscene/internal/manager.Version=..."
// 注入（release 工作流会用 git tag 和 commit 覆盖）；源码直接构建时使用下面的默认值。
var (
	Version = "0.4.0"
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
	CoreDir          string
	ProxyHost        string
	DevHTTPPort      int
	TGHTTPPort       int
	TGSocksPort      int
	GlobalHTTPPort   int
	GlobalSocksPort  int
	InstallBin       string
	SystemdService   string
	RestoreService   string
	XrayServiceUser  string
	TGTargetServices []string
	DevTargetUser    string
	TestURL          string
}

func DefaultConfig() Config {
	return Config{
		CoreDir:          envString("PROXYSCENE_MANAGER_DIR", "/opt/proxyscene"),
		ProxyHost:        envString("PROXYSCENE_HOST", "127.0.0.1"),
		DevHTTPPort:      envInt("PROXYSCENE_DEV_HTTP_PORT", 7891),
		TGHTTPPort:       envInt("PROXYSCENE_TG_HTTP_PORT", 7892),
		TGSocksPort:      envInt("PROXYSCENE_TG_SOCKS_PORT", 7893),
		GlobalHTTPPort:   envInt("PROXYSCENE_GLOBAL_HTTP_PORT", 7890),
		GlobalSocksPort:  envInt("PROXYSCENE_GLOBAL_SOCKS_PORT", 7894),
		InstallBin:       envString("PROXYSCENE_SWITCH_BIN", "/usr/local/bin/proxyscene"),
		SystemdService:   envString("PROXYSCENE_SYSTEMD_SERVICE_NAME", "proxyscene.service"),
		RestoreService:   envString("PROXYSCENE_BOOT_RESTORE_SERVICE_NAME", "proxyscene-restore.service"),
		XrayServiceUser:  envString("PROXYSCENE_SERVICE_USER", "proxyscene"),
		TGTargetServices: splitFields(envString("PROXYSCENE_TG_SERVICES", "openclaw hermes hermes-gateway user:root:hermes-gateway")),
		DevTargetUser:    envString("PROXYSCENE_DEV_TARGET_USER", ""),
		TestURL:          envString("PROXYSCENE_TEST_URL", "https://www.google.com/generate_204"),
	}
}

func (c Config) Validate() error {
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
	if err := validateProxyHost(c.ProxyHost); err != nil {
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

func (c Config) XrayBin() string         { return filepath.Join(c.CoreDir, "xray") }
func (c Config) XrayConfig() string      { return filepath.Join(c.CoreDir, "config.json") }
func (c Config) StorePath() string       { return filepath.Join(c.CoreDir, "state.json") }
func (c Config) StoreLockPath() string   { return filepath.Join(c.CoreDir, ".state.lock") }
func (c Config) StoreBackupPath() string { return filepath.Join(c.CoreDir, "state.json.bak") }
func (c Config) MarkerPath() string      { return filepath.Join(c.CoreDir, ".managed-by-proxyscene") }
func (c Config) DevBackupPath() string   { return filepath.Join(c.CoreDir, "dev-proxy-backup.json") }
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
