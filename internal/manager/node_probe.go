package manager

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	nodeProbeConcurrency    = 3
	nodeProbeBatchTimeout   = 2 * time.Minute
	nodeProbeStartupTimeout = 5 * time.Second
	nodeProbeRequestTimeout = 10 * time.Second
	nodeProbeBodyLimit      = 64 << 10
	nodeProbeTarget         = "节点实际 HTTPS 代理请求"
)

// These options are passed only by local integration fixtures. User input and
// environment variables cannot select a different executable or TLS policy.
type nodeProbeOptions struct {
	targetURL        string
	tlsConfig        *tls.Config
	allowLocalTarget bool
	startupTimeout   time.Duration
	requestTimeout   time.Duration
	tempRoot         string
	openCoreForTest  func() (*os.File, error)
	identityForTest  *localUserIdentity
	onStartedForTest func(pid int, dir string)
}

func nodeProbeURL(raw string, allowLocal bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > maxSubscriptionURLBytes || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return nil, fmt.Errorf("节点测试地址必须是无用户信息和片段的 HTTPS URL")
	}
	if err := validateNodeHost(u.Hostname()); err != nil {
		return nil, fmt.Errorf("节点测试地址主机无效")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("节点测试地址端口无效")
		}
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if !allowLocal {
		ip := net.ParseIP(host)
		if ip != nil && !isPublicIP(ip) || host == "localhost" || strings.HasSuffix(host, ".localhost") || !strings.Contains(host, ".") && ip == nil {
			return nil, fmt.Errorf("节点测试地址必须是远程 HTTPS 地址")
		}
	}
	return u, nil
}

func (a *App) probeNode(ctx context.Context, n Node) (time.Duration, error) {
	pn, err := parseRuntimeNode(n.RawURL)
	if err != nil {
		return 0, fmt.Errorf("节点参数不受支持或不符合安全要求")
	}
	return a.probeParsedNode(ctx, pn, nodeProbeOptions{})
}

func (a *App) probeParsedNode(ctx context.Context, pn *parsedNode, opts nodeProbeOptions) (latency time.Duration, retErr error) {
	target := opts.targetURL
	if target == "" {
		target = a.cfg.TestURL
	}
	targetURL, err := nodeProbeURL(target, opts.allowLocalTarget)
	if err != nil {
		return 0, err
	}
	startup := opts.startupTimeout
	if startup <= 0 {
		startup = nodeProbeStartupTimeout
	}
	requestTimeout := opts.requestTimeout
	if requestTimeout <= 0 {
		requestTimeout = nodeProbeRequestTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, startup+requestTimeout)
	defer cancel()
	if ctx.Err() != nil {
		return 0, fmt.Errorf("节点测试已取消或超时")
	}

	var executable *os.File
	if opts.openCoreForTest != nil {
		executable, err = opts.openCoreForTest()
	} else {
		executable, err = a.openNodeProbeCore()
	}
	if err != nil {
		return 0, fmt.Errorf("无法验证用于节点测试的 Xray 核心")
	}
	defer executable.Close()
	identity, err := a.nodeProbeIdentity(ctx, opts.identityForTest)
	if err != nil {
		return 0, err
	}
	dir, err := os.MkdirTemp(opts.tempRoot, "proxyscene-node-probe-")
	if err != nil {
		return 0, fmt.Errorf("无法创建节点测试私有目录")
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("节点测试临时文件清理失败"))
		}
	}()
	if err := os.Chmod(dir, 0o700); err != nil {
		return 0, fmt.Errorf("无法保护节点测试目录")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("无法分配节点测试端口")
	}
	port := listener.Addr().(*net.TCPAddr).Port
	defer listener.Close()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return 0, fmt.Errorf("无法生成节点测试入站认证")
	}
	password := hex.EncodeToString(secret)
	config, err := renderNodeProbeConfig(pn, targetURL, port, password)
	if err != nil {
		return 0, fmt.Errorf("无法生成节点测试配置")
	}
	configFile, err := os.OpenFile(filepath.Join(dir, "config.json"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return 0, fmt.Errorf("无法创建节点测试配置")
	}
	defer configFile.Close()
	if _, err := configFile.Write(config); err != nil {
		return 0, fmt.Errorf("无法写入节点测试配置")
	}
	if _, err := configFile.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("无法读取节点测试配置")
	}
	// Execute the already-validated inode. stdin keeps the root-owned 0600
	// configuration readable without exposing the private directory to the child.
	cmd := exec.CommandContext(ctx, "/proc/self/fd/3", "run", "-format", "json", "-config", "stdin:")
	cmd.ExtraFiles = []*os.File{executable}
	cmd.Stdin = configFile
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.Env = []string{"LANG=C", "LC_ALL=C", "HOME=/nonexistent", "XRAY_LOCATION_ASSET=/nonexistent"}
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if os.Geteuid() == 0 {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(identity.UID), Gid: uint32(identity.GID), Groups: []uint32{}}
	}
	cmd.WaitDelay = time.Second
	_ = listener.Close()
	done, err := startNodeProbeProcess(cmd, cancel)
	if err != nil {
		return 0, fmt.Errorf("启动隔离 Xray 节点测试失败")
	}

	defer func() {
		// Reap the process before removing any files. Killing the actual child avoids
		// signalling a recycled PID after a completed Wait.
		select {
		case <-done:
		default:
			_ = cmd.Process.Kill()
		}
		<-done
	}()
	if opts.onStartedForTest != nil {
		opts.onStartedForTest(cmd.Process.Pid, dir)
	}
	startupCtx, startupCancel := context.WithTimeout(ctx, startup)
	err = waitNodeProbeListener(startupCtx, cmd.Process.Pid, port, done)
	startupCancel()
	if err != nil {
		return 0, err
	}
	proxyURL := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", itoa(port)), User: url.UserPassword("probe", password)}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if opts.tlsConfig != nil {
		tlsConfig = opts.tlsConfig.Clone()
	}
	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL), TLSClientConfig: tlsConfig,
		DialContext:         (&net.Dialer{Timeout: requestTimeout}).DialContext,
		TLSHandshakeTimeout: requestTimeout, ResponseHeaderTimeout: requestTimeout,
		MaxResponseHeaderBytes: 16 << 10, DisableKeepAlives: true, DisableCompression: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: requestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL.String(), nil)
	if err != nil {
		return 0, fmt.Errorf("无法创建节点测试请求")
	}
	req.Header.Set("User-Agent", "proxyscene/"+Version+" node-probe")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return time.Since(start), fmt.Errorf("节点 HTTPS 代理请求失败或超时（连接、认证或证书验证未通过）")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return time.Since(start), fmt.Errorf("节点 HTTPS 测试返回 HTTP %d", resp.StatusCode)
	}
	size, err := io.Copy(io.Discard, io.LimitReader(resp.Body, nodeProbeBodyLimit+1))
	latency = time.Since(start)
	if err != nil {
		return latency, fmt.Errorf("节点 HTTPS 测试响应未完整接收")
	}
	if size > nodeProbeBodyLimit {
		return latency, fmt.Errorf("节点 HTTPS 测试响应超过 64 KiB 限制")
	}
	if ctx.Err() != nil {
		return latency, fmt.Errorf("节点测试已取消或超时")
	}
	return latency, nil
}

func renderNodeProbeConfig(pn *parsedNode, target *url.URL, port int, password string) ([]byte, error) {
	if pn == nil || pn.Outbound == nil {
		return nil, fmt.Errorf("没有节点出站")
	}
	outbound := make(map[string]any, len(pn.Outbound)+1)
	for k, v := range pn.Outbound {
		outbound[k] = v
	}
	outbound["tag"] = "probe-node"
	targetPort := target.Port()
	if targetPort == "" {
		targetPort = "443"
	}
	rule := map[string]any{"type": "field", "inboundTag": []string{"probe-http"}, "network": "tcp", "port": targetPort, "outboundTag": "probe-node"}
	if ip := net.ParseIP(target.Hostname()); ip != nil {
		rule["ip"] = []string{ip.String()}
	} else {
		rule["domain"] = []string{"full:" + target.Hostname()}
	}
	return json.Marshal(map[string]any{
		"log":       map[string]any{"loglevel": "none"},
		"inbounds":  []any{inbound("probe-http", "127.0.0.1", port, "http", map[string]any{"accounts": []any{map[string]any{"user": "probe", "pass": password}}})},
		"outbounds": []any{map[string]any{"tag": "blocked", "protocol": "blackhole"}, outbound},
		"routing":   map[string]any{"domainStrategy": "AsIs", "rules": []any{rule}},
	})
}

func (a *App) openNodeProbeCore() (*os.File, error) {
	if err := a.ensureXrayInstalled(); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(a.cfg.XrayBin(), syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "node probe core")
	safe := false
	defer func() {
		if !safe {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Uid != 0 || info.Mode().Perm()&0o022 != 0 || info.Mode().Perm()&0o111 == 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return nil, fmt.Errorf("核心文件属性不安全")
	}
	executable, err := elf.NewFile(file)
	if err != nil {
		return nil, err
	}
	if (executable.Type != elf.ET_EXEC && executable.Type != elf.ET_DYN) || !elfMachineMatchesRuntime(executable.Machine) {
		return nil, fmt.Errorf("核心 ELF 类型或架构不符")
	}
	// elf.NewFile does not own file; closing its view must not close the retained FD.
	_ = executable.Close()
	safe = true
	return file, nil
}

func (a *App) nodeProbeIdentity(ctx context.Context, forTest *localUserIdentity) (localUserIdentity, error) {
	var identity localUserIdentity
	var err error
	if forTest != nil {
		identity = *forTest
	} else {
		name := a.cfg.XrayServiceUser
		if name == "root" {
			name = "nobody"
		}
		identity, err = lookupNodeProbeIdentity(ctx, name)
	}
	if err != nil || validateCoreIdentity(identity.Name, identity) != nil || identity.UID == 0 || identity.GID == 0 {
		return localUserIdentity{}, fmt.Errorf("节点测试需要已存在的非 root 核心服务账号")
	}
	if os.Geteuid() != 0 && (os.Geteuid() != identity.UID || os.Getegid() != identity.GID) {
		return localUserIdentity{}, fmt.Errorf("当前用户不能使用节点测试的核心服务身份")
	}
	return identity, nil
}

// The kernel assigns the candidate port, but Xray cannot inherit its socket.
// Verify the listening socket belongs to this exact child before handing it any
// request or credentials; a local process racing to bind the port cannot win a
// successful probe merely by serving its own proxy.
func waitNodeProbeListener(ctx context.Context, pid, port int, done <-chan struct{}) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("隔离 Xray 节点测试启动超时或已取消")
		case <-done:
			return fmt.Errorf("隔离 Xray 节点测试核心提前退出")
		default:
		}
		if nodeProbeOwnsListener(pid, port) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("隔离 Xray 节点测试启动超时或已取消")
		case <-done:
			return fmt.Errorf("隔离 Xray 节点测试核心提前退出")
		case <-ticker.C:
		}
	}
}

func nodeProbeOwnsListener(pid, port int) bool {
	dir := filepath.Join("/proc", strconv.Itoa(pid))
	entries, err := os.ReadDir(filepath.Join(dir, "fd"))
	if err != nil {
		return false
	}
	sockets := map[string]bool{}
	for _, entry := range entries {
		link, err := os.Readlink(filepath.Join(dir, "fd", entry.Name()))
		if err == nil && strings.HasPrefix(link, "socket:[") && strings.HasSuffix(link, "]") {
			sockets[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")] = true
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "net", "tcp"))
	if err != nil {
		return false
	}
	want := fmt.Sprintf("0100007F:%04X", port)
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 9 && fields[1] == want && fields[3] == "0A" && sockets[fields[9]] {
			return true
		}
	}
	return false
}

func runNodeProbes(ctx context.Context, nodes []Node, probe func(context.Context, Node) (time.Duration, error)) []SpeedResult {
	results := make([]SpeedResult, len(nodes))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < nodeProbeConcurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				r := SpeedResult{NodeID: nodes[i].ID, Target: nodeProbeTarget, TestedAt: time.Now()}
				if ctx.Err() != nil {
					r.Error = "节点测试总预算耗尽或已取消"
				} else {
					elapsed, err := probe(ctx, nodes[i])
					r.LatencyMS = elapsed.Milliseconds()
					r.Success = err == nil
					if err != nil {
						r.Error = err.Error()
					}
				}
				results[i] = r
			}
		}()
	}
	for i := range nodes {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return results
}

func nodeProbeSignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// The normal account resolver permits 30 seconds for NSS. Probes share their
// shorter cancellation budget, including identity resolution.
func lookupNodeProbeIdentity(ctx context.Context, name string) (localUserIdentity, error) {
	if err := validateUserName(name); err != nil {
		return localUserIdentity{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	path, err := exec.LookPath("getent")
	if err == nil && validatePrivilegedExecutable(path, "getent") == nil {
		cmd := exec.CommandContext(ctx, path, "passwd", name)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
		var output telegramStatusBuffer
		cmd.Stdout = &output
		cmd.WaitDelay = time.Second
		if err := cmd.Run(); err == nil {
			line, _, _ := strings.Cut(strings.TrimSpace(output.String()), "\n")
			if id, err := parsePasswdLine(line, name); err == nil {
				return id, nil
			}
		}
	}
	if ctx.Err() != nil {
		return localUserIdentity{}, ctx.Err()
	}
	raw, err := readRegularFileNoFollow("/etc/passwd", 1<<20)
	if err != nil {
		return localUserIdentity{}, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, name+":") {
			return parsePasswdLine(line, name)
		}
	}
	return localUserIdentity{}, fmt.Errorf("没有本地测试账号")
}

// PR_SET_NO_NEW_PRIVS affects a thread permanently. Keep the launch thread
// locked for the complete child lifetime, then let Go retire it by returning
// without UnlockOSThread. No other manager work inherits the changed privilege
// state, and Pdeathsig remains attached to a live thread until the child exits.
func startNodeProbeProcess(cmd *exec.Cmd, cancel context.CancelFunc) (<-chan struct{}, error) {
	started := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer close(done)
		defer cancel()
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			started <- err
			return
		}
		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		started <- nil
		_ = cmd.Wait()
	}()
	if err := <-started; err != nil {
		<-done
		return nil, err
	}
	return done, nil
}

func runNodeProbeBatch(ctx context.Context, nodes []Node, probe func(context.Context, Node) (time.Duration, error)) ([]SpeedResult, error) {
	results := runNodeProbes(ctx, nodes, probe)
	if err := ctx.Err(); err != nil {
		return results, fmt.Errorf("节点测试已取消或总预算耗尽，未保存结果或切换节点：%w", err)
	}
	return results, nil
}
