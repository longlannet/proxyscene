package manager

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

func (a *App) ensureCoreDirs() error {
	if err := ensureDir(a.cfg.CoreDir, 0o700); err != nil {
		return err
	}
	// 与其余写入保持一致：用原子、O_NOFOLLOW 的写法，避免跟随符号链接。
	return writeFileAtomic(a.cfg.MarkerPath(), []byte("由 proxyscene 管理\n"), 0o600)
}

func (a *App) ensureXrayInstalled() error {
	st, err := os.Stat(a.cfg.XrayBin())
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("未找到 Xray 可执行文件：%s，请先运行安装脚本安装 Xray", a.cfg.XrayBin())
		}
		return err
	}
	if st.IsDir() {
		return fmt.Errorf("Xray 路径不是文件：%s", a.cfg.XrayBin())
	}
	if st.Mode()&0o111 == 0 {
		return fmt.Errorf("Xray 文件不可执行：%s", a.cfg.XrayBin())
	}
	return nil
}

// writeCheckedXrayConfig 先把配置写入临时文件并用 `xray -test` 校验，校验通过后才
// 原子替换 config.json。这样坏配置（例如某个节点产生 Xray 不接受的 outbound）不会
// 覆盖上一份可用配置，也使 README 的"配置写入前会进行配置测试"成立。
func (a *App) writeCheckedXrayConfig(st *Store) error {
	tmp := a.cfg.XrayConfig() + ".new"
	if err := a.writeXrayConfigTo(st, tmp); err != nil {
		return err
	}
	if err := a.checkXrayConfigAt(tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, a.cfg.XrayConfig())
}

func (a *App) writeXrayConfigTo(st *Store, path string) error {
	if st == nil {
		st = newStore()
	}
	inbounds := []any{}
	rules := []any{}
	outbounds := []any{}

	addScene := func(scene Scene, inboundTags []string, outboundTag string, sceneInbounds ...any) error {
		if !st.SceneEnabled[scene] {
			return nil
		}
		outbound, err := a.outboundForScene(st, scene, outboundTag)
		if err != nil {
			return err
		}
		inbounds = append(inbounds, sceneInbounds...)
		rules = append(rules, map[string]any{"type": "field", "inboundTag": inboundTags, "outboundTag": outboundTag})
		outbounds = append(outbounds, outbound)
		return nil
	}

	if err := addScene(SceneDev, []string{"dev-http"}, "proxy-dev", inbound("dev-http", a.cfg.ProxyHost, a.cfg.DevHTTPPort, "http", nil)); err != nil {
		return err
	}
	if err := addScene(SceneTelegram, []string{"telegram-http", "telegram-socks"}, "proxy-telegram",
		inbound("telegram-http", a.cfg.ProxyHost, a.cfg.TGHTTPPort, "http", nil),
		inbound("telegram-socks", a.cfg.ProxyHost, a.cfg.TGSocksPort, "socks", map[string]any{"udp": true}),
	); err != nil {
		return err
	}
	if err := addScene(SceneGlobal, []string{"global-http", "global-socks"}, "proxy-global",
		inbound("global-http", a.cfg.ProxyHost, a.cfg.GlobalHTTPPort, "http", nil),
		inbound("global-socks", a.cfg.ProxyHost, a.cfg.GlobalSocksPort, "socks", map[string]any{"udp": true}),
	); err != nil {
		return err
	}
	if len(inbounds) == 0 {
		return fmt.Errorf("没有已开启场景，无需生成 Xray 配置")
	}

	cfg := map[string]any{
		"log":       map[string]any{"loglevel": "warning"},
		"inbounds":  inbounds,
		"routing":   map[string]any{"domainStrategy": "AsIs", "rules": rules},
		"outbounds": outbounds,
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(b, '\n'), 0o600)
}

func inbound(tag, listen string, port int, protocol string, settings map[string]any) map[string]any {
	if settings == nil {
		settings = map[string]any{}
	}
	return map[string]any{"tag": tag, "listen": listen, "port": port, "protocol": protocol, "settings": settings}
}

func (a *App) outboundForScene(st *Store, scene Scene, tag string) (map[string]any, error) {
	id := st.selectedNodeID(scene)
	if id == "" {
		return nil, fmt.Errorf("没有可用节点")
	}
	n := st.findNode(id)
	if n == nil {
		return nil, fmt.Errorf("节点不存在：%s", id)
	}
	pn, err := parseNode(n.RawURL)
	if err != nil {
		return nil, err
	}
	pn.Outbound["tag"] = tag
	return pn.Outbound, nil
}

func (a *App) checkXrayConfigAt(path string) error {
	if err := runQuietLabel("Xray 配置检查", a.cfg.XrayBin(), "run", "-test", "-config", path); err != nil {
		return err
	}
	fmt.Println("Xray 配置检查通过")
	return nil
}

func (a *App) testNode(n Node) error {
	pn, err := parseNode(n.RawURL)
	if err != nil {
		return err
	}
	if pn.EndpointHost == "" || pn.EndpointPort <= 0 {
		return fmt.Errorf("节点缺少可测速地址")
	}
	endpoint := net.JoinHostPort(pn.EndpointHost, itoa(pn.EndpointPort))
	conn, err := net.DialTimeout("tcp", endpoint, 5*time.Second)
	if err != nil {
		return fmt.Errorf("无法连接节点地址：%s", endpoint)
	}
	return conn.Close()
}

func (a *App) testProxy() error {
	st, err := a.loadStore()
	if err != nil {
		return err
	}
	if !st.SceneEnabled[SceneGlobal] {
		return fmt.Errorf("全局代理未开启，请先运行：proxyscene global on")
	}
	if st.selectedNodeID(SceneGlobal) == "" {
		return fmt.Errorf("全局代理没有可用节点，请先添加或选择节点")
	}
	proxyURL, err := url.Parse(a.cfg.HTTPAddr(SceneGlobal))
	if err != nil {
		return fmt.Errorf("全局代理地址无效：%s", a.cfg.HTTPAddr(SceneGlobal))
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
	}
	req, err := http.NewRequest(http.MethodGet, a.cfg.TestURL, nil)
	if err != nil {
		return fmt.Errorf("创建代理测试请求失败：%s", a.cfg.TestURL)
	}
	req.Header.Set("User-Agent", "proxyscene/"+Version)
	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return fmt.Errorf("全局代理测试失败：请确认全局代理已开启、节点可用，并且 %s 正在监听", a.cfg.HTTPAddr(SceneGlobal))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("全局代理测试失败：HTTP 状态码 %d，耗时 %dms", resp.StatusCode, elapsed)
	}
	fmt.Printf("全局代理测试通过：HTTP 状态码 %d，耗时 %dms\n", resp.StatusCode, elapsed)
	return nil
}
