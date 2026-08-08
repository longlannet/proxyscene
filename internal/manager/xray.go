package manager

import (
	"bytes"
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"syscall"
	"time"
)

func (a *App) ensureCoreDirs() error {
	info, err := os.Lstat(a.cfg.CoreDir)
	if errors.Is(err, os.ErrNotExist) {
		if err := ensureDir(a.cfg.CoreDir, 0o700); err != nil {
			return err
		}
		return writeFileAtomic(a.cfg.MarkerPath(), []byte(managerMarkerText), 0o600)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("proxyscene 管理目录必须是非符号链接目录：%s", a.cfg.CoreDir)
	}
	if err := ensureDir(a.cfg.CoreDir, 0o700); err != nil {
		return err
	}
	marker, markerErr := readRegularFileNoFollow(a.cfg.MarkerPath(), 256)
	if markerErr == nil {
		if !bytes.Equal(marker, []byte(managerMarkerText)) && !bytes.Equal(marker, []byte(installerMarkerText)) {
			return fmt.Errorf("管理目录 ownership marker 内容无效：%s", a.cfg.MarkerPath())
		}
		return nil
	}
	if !errors.Is(markerErr, os.ErrNotExist) {
		return fmt.Errorf("读取管理目录 ownership marker 失败：%w", markerErr)
	}
	entries, err := os.ReadDir(a.cfg.CoreDir)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("拒绝接管未标记的非空目录：%s", a.cfg.CoreDir)
	}
	return writeFileAtomic(a.cfg.MarkerPath(), []byte(managerMarkerText), 0o600)
}

func (a *App) ensureXrayInstalled() error {
	path := a.cfg.XrayBin()
	st, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("未找到 Xray 可执行文件：%s，请先运行安装脚本安装 Xray", path)
		}
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("该 Xray 路径必须是普通文件且不能是符号链接：%s", path)
	}
	if st.Mode()&0o111 == 0 {
		return fmt.Errorf("该 Xray 文件不可执行：%s", path)
	}
	if st.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return fmt.Errorf("该 Xray 文件不能带 setuid/setgid 位：%s", path)
	}
	if st.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("该 Xray 文件不能允许组或其他用户写入：%s", path)
	}
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return fmt.Errorf("该 Xray 文件必须属于 root：%s", path)
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("安全打开 Xray 文件失败：%w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	openedStat, ok := opened.Sys().(*syscall.Stat_t)
	if !ok || openedStat.Dev != stat.Dev || openedStat.Ino != stat.Ino {
		return fmt.Errorf("检测到 Xray 文件在校验期间发生变化：%s", path)
	}
	if !opened.Mode().IsRegular() || opened.Mode().Perm()&0o022 != 0 || opened.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 || openedStat.Uid != 0 {
		return fmt.Errorf("检测到 Xray 文件属性在校验期间变得不安全：%s", path)
	}
	elfFile, err := elf.NewFile(file)
	if err != nil {
		return fmt.Errorf("该 Xray 文件不是有效 ELF 可执行文件：%s", path)
	}
	defer elfFile.Close()
	if elfFile.Type != elf.ET_EXEC && elfFile.Type != elf.ET_DYN {
		return fmt.Errorf("该 Xray ELF 不是可执行类型：%s", path)
	}
	if !elfMachineMatchesRuntime(elfFile.Machine) {
		return fmt.Errorf("该 Xray ELF 架构与当前系统不匹配：%s", path)
	}
	return nil
}

func elfMachineMatchesRuntime(machine elf.Machine) bool {
	switch runtime.GOARCH {
	case "amd64":
		return machine == elf.EM_X86_64
	case "arm64":
		return machine == elf.EM_AARCH64
	case "386":
		return machine == elf.EM_386
	case "arm":
		return machine == elf.EM_ARM
	default:
		return false
	}
}

// writeCheckedXrayConfig 先把配置写入临时文件并用 `xray -test` 校验，校验通过后才
// 原子替换 config.json。这样坏配置（例如某个节点产生 Xray 不接受的 outbound）不会
// 覆盖上一份可用配置，也使 README 的"配置写入前会进行配置测试"成立。
func (a *App) writeCheckedXrayConfig(st *Store) (retErr error) {
	tmp := a.cfg.XrayConfig() + ".new"
	committed := false
	defer func() {
		if committed {
			return
		}
		retErr = errors.Join(retErr, removeXrayTempConfig(tmp, a.cfg.CoreDir))
	}()
	if err := a.writeXrayConfigTo(st, tmp); err != nil {
		return err
	}
	if err := a.checkXrayConfigAt(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, a.cfg.XrayConfig()); err != nil {
		return err
	}
	committed = true
	// 与 writeFileAtomic 的持久化语义一致：rename 后 fsync 父目录，崩溃后不丢这次改名。
	return fsyncDir(a.cfg.CoreDir)
}

func removeXrayTempConfig(path, dir string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("删除未通过校验的 Xray 临时配置失败：%w", err)
	}
	if err := fsyncDir(dir); err != nil {
		return fmt.Errorf("持久化 Xray 临时配置清理失败：%w", err)
	}
	return nil
}

// clearXrayConfig removes the last active configuration when no scene remains.
// A future enable always regenerates it from state, so retaining it only keeps
// deleted node credentials on disk.
func (a *App) clearXrayConfig() error {
	err := os.Remove(a.cfg.XrayConfig())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return fsyncDir(a.cfg.CoreDir)
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
	// 必须显式 -format json：临时文件名是 config.json.new，Xray 默认按扩展名推断格式，
	// 而 ".new" 不是已知格式，会以 "Failed to get format" 报错（exit 23）导致任何场景都无法启用。
	// proxyscene 始终生成 JSON，固定指定格式既正确又与临时文件名解耦。
	if err := runQuietLabel("Xray 配置检查", a.cfg.XrayBin(), "run", "-test", "-format", "json", "-config", path); err != nil {
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
