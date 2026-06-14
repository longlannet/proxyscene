package manager

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxSubscriptionBytes int64 = 4 << 20

type parsedNode struct {
	Protocol     string
	Name         string
	EndpointHost string
	EndpointPort int
	Outbound     map[string]any
}

func protocolFromURL(raw string) string {
	i := strings.Index(raw, "://")
	if i < 0 {
		return ""
	}
	s := strings.ToLower(raw[:i])
	if s == "shadowsocks" {
		return "ss"
	}
	return s
}

func decodeBase64URL(s string) ([]byte, error) {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\n", ""), "\r", ""))
	encodings := []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding}
	var last error
	for _, enc := range encodings {
		b, err := enc.DecodeString(s)
		if err == nil {
			return b, nil
		}
		last = err
	}
	return nil, last
}

func (a *App) addNode(st *Store, raw, name, scope string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("节点链接不能为空")
	}
	pn, err := parseNode(raw, a.cfg)
	if err != nil {
		return "", err
	}
	if old := st.findNodeByURL(raw); old != nil {
		if name != "" {
			old.Name = name
			old.UpdatedAt = time.Now()
		}
		if scope != "" {
			if err := a.useNodeInStore(st, old.ID, scope); err != nil {
				return "", err
			}
		}
		return old.ID, nil
	}
	id := newNodeID()
	if name == "" {
		name = pn.Name
	}
	if name == "" {
		name = pn.Protocol + "-node"
	}
	st.Nodes = append(st.Nodes, Node{ID: id, Name: name, Protocol: pn.Protocol, RawURL: raw, CreatedAt: time.Now(), UpdatedAt: time.Now()})
	if st.DefaultNodeID == "" {
		st.DefaultNodeID = id
	}
	if scope != "" {
		if err := a.useNodeInStore(st, id, scope); err != nil {
			return "", err
		}
	}
	return id, nil
}

func parseNode(raw string, cfg Config) (*parsedNode, error) {
	switch protocolFromURL(raw) {
	case "vless":
		return parseVLESS(raw, cfg)
	case "vmess":
		return parseVMess(raw, cfg)
	case "trojan":
		return parseTrojan(raw, cfg)
	case "ss":
		return parseSS(raw, cfg)
	default:
		return nil, fmt.Errorf("不支持的节点协议：%s", protocolFromURL(raw))
	}
}

func parseVLESS(raw string, cfg Config) (*parsedNode, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Hostname() == "" || u.User.Username() == "" {
		return nil, fmt.Errorf("VLESS 缺少服务器地址或用户 ID")
	}
	port, err := parseNodePort(u.Port(), "VLESS")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	network := q.Get("type")
	if network == "" {
		network = q.Get("network")
	}
	if network == "" {
		network = "tcp"
	}
	security := q.Get("security")
	if security == "" {
		security = "none"
	}
	stream := map[string]any{"network": network, "security": security, "sockopt": map[string]any{"mark": cfg.OutboundMark}}
	if security == "tls" {
		sni := q.Get("sni")
		if sni == "" {
			sni = q.Get("serverName")
		}
		if sni == "" {
			sni = u.Hostname()
		}
		stream["tlsSettings"] = map[string]any{"serverName": sni}
	}
	if security == "reality" {
		sni := q.Get("sni")
		if sni == "" {
			sni = q.Get("serverName")
		}
		stream["realitySettings"] = map[string]any{"serverName": sni, "fingerprint": q.Get("fp"), "publicKey": q.Get("pbk"), "shortId": q.Get("sid"), "spiderX": firstNonEmpty(q.Get("spx"), "/")}
	}
	addTransport(stream, network, q)
	out := map[string]any{"tag": "", "protocol": "vless", "settings": map[string]any{"vnext": []any{map[string]any{"address": u.Hostname(), "port": port, "users": []any{map[string]any{"id": u.User.Username(), "encryption": firstNonEmpty(q.Get("encryption"), "none"), "flow": q.Get("flow")}}}}}, "streamSettings": stream}
	return &parsedNode{Protocol: "vless", Name: remark(raw, "vless-"+u.Hostname()), EndpointHost: u.Hostname(), EndpointPort: port, Outbound: out}, nil
}

func parseVMess(raw string, cfg Config) (*parsedNode, error) {
	payload := strings.TrimPrefix(raw, "vmess://")
	payload = strings.Split(payload, "#")[0]
	b, err := decodeBase64URL(payload)
	if err != nil {
		return nil, err
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	addr := stringVal(v, "add")
	port := intVal(v, "port")
	id := stringVal(v, "id")
	if addr == "" || id == "" {
		return nil, fmt.Errorf("VMess 缺少 add/id")
	}
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("VMess 端口无效")
	}
	network := firstNonEmpty(stringVal(v, "net"), "tcp")
	security := "none"
	if stringVal(v, "tls") == "tls" {
		security = "tls"
	}
	stream := map[string]any{"network": network, "security": security, "sockopt": map[string]any{"mark": cfg.OutboundMark}}
	if security == "tls" {
		stream["tlsSettings"] = map[string]any{"serverName": firstNonEmpty(stringVal(v, "sni"), addr)}
	}
	q := url.Values{"host": []string{stringVal(v, "host")}, "path": []string{stringVal(v, "path")}, "serviceName": []string{stringVal(v, "serviceName")}}
	addTransport(stream, network, q)
	out := map[string]any{"tag": "", "protocol": "vmess", "settings": map[string]any{"vnext": []any{map[string]any{"address": addr, "port": port, "users": []any{map[string]any{"id": id, "alterId": intVal(v, "aid"), "security": firstNonEmpty(stringVal(v, "scy"), "auto")}}}}}, "streamSettings": stream}
	return &parsedNode{Protocol: "vmess", Name: firstNonEmpty(stringVal(v, "ps"), "vmess-"+addr), EndpointHost: addr, EndpointPort: port, Outbound: out}, nil
}

func parseTrojan(raw string, cfg Config) (*parsedNode, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Hostname() == "" || u.User.Username() == "" {
		return nil, fmt.Errorf("Trojan 缺少服务器地址或密码")
	}
	port, err := parseNodePort(u.Port(), "Trojan")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	network := firstNonEmpty(q.Get("type"), q.Get("network"), "tcp")
	security := firstNonEmpty(q.Get("security"), "tls")
	stream := map[string]any{"network": network, "security": security, "sockopt": map[string]any{"mark": cfg.OutboundMark}}
	if security == "tls" {
		stream["tlsSettings"] = map[string]any{"serverName": firstNonEmpty(q.Get("sni"), q.Get("peer"), u.Hostname())}
	}
	addTransport(stream, network, q)
	out := map[string]any{"tag": "", "protocol": "trojan", "settings": map[string]any{"servers": []any{map[string]any{"address": u.Hostname(), "port": port, "password": u.User.Username()}}}, "streamSettings": stream}
	return &parsedNode{Protocol: "trojan", Name: remark(raw, "trojan-"+u.Hostname()), EndpointHost: u.Hostname(), EndpointPort: port, Outbound: out}, nil
}

func parseSS(raw string, cfg Config) (*parsedNode, error) {
	body := strings.TrimPrefix(strings.TrimPrefix(raw, "ss://"), "shadowsocks://")
	body = strings.Split(strings.Split(body, "#")[0], "?")[0]
	var userinfo, hostport string
	if strings.Contains(body, "@") {
		parts := strings.SplitN(body, "@", 2)
		userinfo, hostport = parts[0], parts[1]
		if !strings.Contains(userinfo, ":") {
			if b, err := decodeBase64URL(userinfo); err == nil {
				userinfo = string(b)
			}
		}
	} else {
		b, err := decodeBase64URL(body)
		if err != nil {
			return nil, err
		}
		parts := strings.SplitN(string(b), "@", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("SS 格式无效")
		}
		userinfo, hostport = parts[0], parts[1]
	}
	up := strings.SplitN(userinfo, ":", 2)
	if len(up) != 2 || strings.TrimSpace(up[0]) == "" || strings.TrimSpace(up[1]) == "" {
		return nil, fmt.Errorf("SS 缺少 method/password")
	}
	addr, portText, err := net.SplitHostPort(hostport)
	if err != nil {
		hp := strings.Split(hostport, ":")
		if len(hp) < 2 {
			return nil, fmt.Errorf("SS 缺少 host/port")
		}
		addr = strings.Join(hp[:len(hp)-1], ":")
		portText = hp[len(hp)-1]
		addr = strings.TrimSuffix(strings.TrimPrefix(addr, "["), "]")
	}
	if strings.TrimSpace(addr) == "" {
		return nil, fmt.Errorf("SS 缺少服务器地址")
	}
	port, err := parseNodePort(portText, "SS")
	if err != nil {
		return nil, err
	}
	out := map[string]any{"tag": "", "protocol": "shadowsocks", "settings": map[string]any{"servers": []any{map[string]any{"address": addr, "port": port, "method": up[0], "password": up[1]}}}, "streamSettings": map[string]any{"sockopt": map[string]any{"mark": cfg.OutboundMark}}}
	return &parsedNode{Protocol: "ss", Name: remark(raw, "ss-"+addr), EndpointHost: addr, EndpointPort: port, Outbound: out}, nil
}

func parseNodePort(raw, protocol string) (int, error) {
	port, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || port <= 0 || port > 65535 {
		return 0, fmt.Errorf("%s 端口无效", protocol)
	}
	return port, nil
}

func addTransport(stream map[string]any, network string, q url.Values) {
	switch network {
	case "ws":
		stream["wsSettings"] = map[string]any{"path": firstNonEmpty(q.Get("path"), "/"), "headers": map[string]any{"Host": q.Get("host")}}
	case "grpc":
		stream["grpcSettings"] = map[string]any{"serviceName": q.Get("serviceName")}
	case "h2", "http":
		stream["httpSettings"] = map[string]any{"path": firstNonEmpty(q.Get("path"), "/"), "host": splitCSV(q.Get("host"))}
	}
}

func remark(raw, fallback string) string {
	if i := strings.LastIndex(raw, "#"); i >= 0 && i+1 < len(raw) {
		if s, err := url.QueryUnescape(raw[i+1:]); err == nil && s != "" {
			return s
		}
	}
	return fallback
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}
func stringVal(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}
func intVal(m map[string]any, k string) int {
	switch v := m[k].(type) {
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}

func (a *App) nodeCommand(args []string) error {
	if len(args) == 0 {
		return a.nodeMenu()
	}
	if args[0] == "list" {
		st, err := a.loadStore()
		if err != nil {
			return err
		}
		return a.listNodes(st)
	}
	if err := requireRoot(); err != nil {
		return err
	}
	return a.withStoreLock(func() error {
		st, err := a.loadStore()
		if err != nil {
			return err
		}
		switch args[0] {
		case "add":
			raw := arg(args, 1)
			name := arg(args, 2)
			_, err := a.addNode(st, raw, name, "default")
			if err == nil {
				err = a.saveStore(st)
			}
			if err == nil {
				err = a.reloadIfEnabled(st)
			}
			return err
		case "remove", "delete":
			return a.removeNode(st, arg(args, 1))
		case "rename":
			return a.renameNode(st, arg(args, 1), arg(args, 2))
		case "use":
			id := arg(args, 1)
			scope := firstNonEmpty(arg(args, 2), "default")
			if err := a.useNodeInStore(st, id, scope); err != nil {
				return err
			}
			if err := a.saveStore(st); err != nil {
				return err
			}
			return a.reloadIfEnabled(st)
		case "import":
			return a.importSubscription(st, arg(args, 1))
		case "test":
			return a.speedTest(st)
		case "auto":
			scope := firstNonEmpty(arg(args, 1), "default")
			return a.autoSelect(st, scope)
		default:
			return fmt.Errorf("未知节点命令：%s", args[0])
		}
	})
}

func arg(args []string, i int) string {
	if len(args) > i {
		return args[i]
	}
	return ""
}

func (a *App) nodeMenu() error {
	for {
		st, err := a.loadStore()
		if err != nil {
			return err
		}
		fmt.Println("\n========== 节点管理 ==========")
		_ = a.listNodes(st)
		fmt.Println("1. 查看多节点列表")
		fmt.Println("2. 添加节点")
		fmt.Println("3. 导入订阅链接")
		fmt.Println("4. 节点测速")
		fmt.Println("5. 测速后自动选择默认节点")
		fmt.Println("6. 选择默认节点")
		fmt.Println("7. 为全局代理选择节点")
		fmt.Println("8. 为开发代理选择节点")
		fmt.Println("9. 为电报服务代理选择节点")
		fmt.Println("10. 修改节点备注")
		fmt.Println("11. 删除节点")
		fmt.Println("12. 返回")
		switch ask("请输入选项 [1-12]: ") {
		case "1":
			_ = a.listNodes(st)
		case "2":
			raw := ask("节点链接: ")
			name := ask("备注名: ")
			if err := a.withLockedStoreRoot(func(st *Store) error {
				_, err := a.addNode(st, raw, name, "default")
				if err == nil {
					err = a.saveStore(st)
				}
				if err == nil {
					err = a.reloadIfEnabled(st)
				}
				return err
			}); err != nil {
				fmt.Println(err)
			}
		case "3":
			sub := ask("订阅链接: ")
			if err := a.withLockedStoreRoot(func(st *Store) error { return a.importSubscription(st, sub) }); err != nil {
				fmt.Println(err)
			}
		case "4":
			if err := a.withLockedStoreRoot(func(st *Store) error { return a.speedTest(st) }); err != nil {
				fmt.Println(err)
			}
		case "5":
			if err := a.withLockedStoreRoot(func(st *Store) error { return a.autoSelect(st, "default") }); err != nil {
				fmt.Println(err)
			}
		case "6":
			id := ask("节点 ID: ")
			if err := a.useNodeWithLock(id, "default"); err != nil {
				fmt.Println(err)
			}
		case "7":
			id := ask("节点 ID: ")
			if err := a.useNodeWithLock(id, string(SceneGlobal)); err != nil {
				fmt.Println(err)
			}
		case "8":
			id := ask("节点 ID: ")
			if err := a.useNodeWithLock(id, string(SceneDev)); err != nil {
				fmt.Println(err)
			}
		case "9":
			id := ask("节点 ID: ")
			if err := a.useNodeWithLock(id, string(SceneTelegram)); err != nil {
				fmt.Println(err)
			}
		case "10":
			id := ask("节点 ID: ")
			name := ask("新备注: ")
			if err := a.withLockedStoreRoot(func(st *Store) error { return a.renameNode(st, id, name) }); err != nil {
				fmt.Println(err)
			}
		case "11":
			id := ask("节点 ID: ")
			if err := a.withLockedStoreRoot(func(st *Store) error { return a.removeNode(st, id) }); err != nil {
				fmt.Println(err)
			}
		case "12":
			return nil
		}
	}
}

func (a *App) listNodes(st *Store) error {
	if st == nil || len(st.Nodes) == 0 {
		fmt.Println("节点列表为空")
		return nil
	}
	for _, n := range st.Nodes {
		usage := []string{}
		if st.DefaultNodeID == n.ID {
			usage = append(usage, "默认")
		}
		for sc, id := range st.SceneNodes {
			if id == n.ID {
				usage = append(usage, sceneName(sc))
			}
		}
		usageText := strings.Join(usage, "、")
		if usageText == "" {
			usageText = "未指定"
		}
		fmt.Printf("%s [%s] %s（用途：%s）\n", n.ID, n.Protocol, n.Name, usageText)
	}
	return nil
}

func (a *App) removeNode(st *Store, id string) error {
	if id == "" {
		return fmt.Errorf("节点 ID 不能为空")
	}
	if st.findNode(id) == nil {
		return fmt.Errorf("节点不存在：%s", id)
	}
	out := st.Nodes[:0]
	for _, n := range st.Nodes {
		if n.ID != id {
			out = append(out, n)
		}
	}
	st.Nodes = out
	if st.DefaultNodeID == id {
		st.DefaultNodeID = st.firstNodeID()
	}
	for sc, nid := range st.SceneNodes {
		if nid == id {
			delete(st.SceneNodes, sc)
		}
	}
	delete(st.SpeedResults, id)
	if len(st.Nodes) == 0 && hasEnabledScene(st) {
		st.SceneEnabled[SceneGlobal] = false
		st.SceneEnabled[SceneDev] = false
		st.SceneEnabled[SceneTelegram] = false
		if err := a.applySavedScenes(st); err != nil {
			return err
		}
		if err := a.stopXrayService(); err != nil {
			return err
		}
	}
	if err := a.saveStore(st); err != nil {
		return err
	}
	return a.reloadIfEnabled(st)
}

func (a *App) renameNode(st *Store, id, name string) error {
	n := st.findNode(id)
	if n == nil {
		return fmt.Errorf("节点不存在：%s", id)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("节点备注不能为空")
	}
	n.Name = name
	n.UpdatedAt = time.Now()
	if err := a.saveStore(st); err != nil {
		return err
	}
	return a.reloadIfEnabled(st)
}

func (a *App) useNodeInStore(st *Store, id, scope string) error {
	if id == "" {
		return fmt.Errorf("节点 ID 不能为空")
	}
	if st.findNode(id) == nil {
		return fmt.Errorf("节点不存在：%s", id)
	}
	switch scope {
	case "", "default":
		st.DefaultNodeID = id
	case "global", "dev", "telegram":
		st.SceneNodes[Scene(scope)] = id
	case "tg":
		st.SceneNodes[SceneTelegram] = id
	case "all":
		st.DefaultNodeID = id
		st.SceneNodes[SceneGlobal] = id
		st.SceneNodes[SceneDev] = id
		st.SceneNodes[SceneTelegram] = id
	default:
		return fmt.Errorf("未知节点使用范围：%s，可用范围：default/global/dev/telegram/all", scope)
	}
	return nil
}

func (a *App) importSubscription(st *Store, sub string) error {
	sub = strings.TrimSpace(sub)
	if sub == "" {
		return fmt.Errorf("订阅链接不能为空")
	}
	subURL, err := url.Parse(sub)
	if err != nil || subURL.Host == "" {
		return fmt.Errorf("订阅链接必须是有效的 https 地址")
	}
	switch subURL.Scheme {
	case "https":
	case "http":
		if !envBool("XRAY_PROXY_ALLOW_HTTP_SUBSCRIPTION", false) {
			return fmt.Errorf("订阅链接必须使用 https；如确需导入明文 HTTP 订阅，请设置 XRAY_PROXY_ALLOW_HTTP_SUBSCRIPTION=1")
		}
		fmt.Println("警告：正在导入明文 HTTP 订阅，内容可能被中间人篡改")
	default:
		return fmt.Errorf("订阅链接必须是 https 地址")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(sub)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("订阅下载失败：%s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxSubscriptionBytes+1))
	if err != nil {
		return err
	}
	if int64(len(b)) > maxSubscriptionBytes {
		return fmt.Errorf("订阅内容过大，超过 %d 字节", maxSubscriptionBytes)
	}
	text := string(b)
	urls := extractNodeURLs(text)
	if len(urls) == 0 {
		if dec, err := decodeBase64URL(strings.TrimSpace(text)); err == nil {
			urls = extractNodeURLs(string(dec))
		}
	}
	before := len(st.Nodes)
	accepted := 0
	for _, raw := range urls {
		if _, err := a.addNode(st, raw, "", ""); err == nil {
			accepted++
		}
	}
	added := len(st.Nodes) - before
	if accepted == 0 {
		return fmt.Errorf("订阅中没有可导入节点")
	}
	fmt.Printf("订阅导入完成：新增 %d 个节点，已存在 %d 个节点\n", added, accepted-added)
	if !containsString(st.Subscriptions, sub) {
		st.Subscriptions = append(st.Subscriptions, sub)
	}
	if err := a.saveStore(st); err != nil {
		return err
	}
	return a.reloadIfEnabled(st)
}

func extractNodeURLs(s string) []string {
	re := regexp.MustCompile(`(?i)(vless|vmess|trojan|ss|shadowsocks)://[^\s<>'"]+`)
	matches := re.FindAllString(s, -1)
	urls := make([]string, 0, len(matches))
	for _, raw := range matches {
		raw = strings.TrimRight(raw, ".,;，；。)]}>")
		if raw != "" {
			urls = append(urls, raw)
		}
	}
	return urls
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func (a *App) speedTest(st *Store) error {
	if len(st.Nodes) == 0 {
		return fmt.Errorf("没有可测速节点")
	}
	current := map[string]bool{}
	for _, n := range st.Nodes {
		current[n.ID] = true
		start := time.Now()
		err := a.testNode(n)
		ms := time.Since(start).Milliseconds()
		result := SpeedResult{NodeID: n.ID, Target: "节点地址 TCP 连通性", LatencyMS: ms, Success: err == nil, TestedAt: time.Now()}
		if err != nil {
			result.Error = err.Error()
			fmt.Printf("%s 失败：%v\n", n.Name, err)
		} else {
			fmt.Printf("%s %dms\n", n.Name, ms)
		}
		st.SpeedResults[n.ID] = result
	}
	for id := range st.SpeedResults {
		if !current[id] {
			delete(st.SpeedResults, id)
		}
	}
	return a.saveStore(st)
}

func (a *App) withLockedStoreRoot(fn func(*Store) error) error {
	if err := requireRoot(); err != nil {
		return err
	}
	return a.withStoreLock(func() error {
		st, err := a.loadStore()
		if err != nil {
			return err
		}
		return fn(st)
	})
}

func (a *App) useNodeWithLock(id, scope string) error {
	return a.withLockedStoreRoot(func(st *Store) error {
		if err := a.useNodeInStore(st, id, scope); err != nil {
			return err
		}
		if err := a.saveStore(st); err != nil {
			return err
		}
		return a.reloadIfEnabled(st)
	})
}

func (a *App) autoSelect(st *Store, scope string) error {
	if err := a.speedTest(st); err != nil {
		return err
	}
	ids := make([]string, 0, len(st.Nodes))
	for _, n := range st.Nodes {
		if result, ok := st.SpeedResults[n.ID]; ok && result.Success {
			ids = append(ids, n.ID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return st.SpeedResults[ids[i]].LatencyMS < st.SpeedResults[ids[j]].LatencyMS })
	for _, id := range ids {
		if err := a.useNodeInStore(st, id, scope); err != nil {
			return err
		}
		if err := a.saveStore(st); err != nil {
			return err
		}
		return a.reloadIfEnabled(st)
	}
	return fmt.Errorf("没有可用节点")
}
