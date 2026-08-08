package manager

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

const (
	maxSubscriptionBytes          int64 = 4 << 20
	maxSubscriptionURLBytes             = 4096
	maxSubscriptionNodes                = 2048
	maxSubscriptions                    = 512
	maxTotalNodes                       = 4096
	maxNodeURLBytes                     = 16 << 10
	maxNodeNameBytes                    = 256
	maxNodeHostBytes                    = 253
	maxSubscriptionProcessingTime       = 5 * time.Second
)

type parsedNode struct {
	Protocol     string
	Name         string
	EndpointHost string
	EndpointPort int
	Outbound     map[string]any
}

type preparedNode struct {
	RawURL string
	Parsed *parsedNode
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

func payloadAfterScheme(raw string) string {
	if i := strings.Index(raw, "://"); i >= 0 {
		return raw[i+3:]
	}
	return raw
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
	return a.addNodeIndexed(st, raw, name, scope, nil)
}

func (a *App) addNodeIndexed(st *Store, raw, name, scope string, urlIndex map[string]int) (string, error) {
	prepared, err := prepareNode(raw)
	if err != nil {
		return "", err
	}
	return a.addPreparedNodeIndexed(st, prepared, name, scope, urlIndex)
}

func prepareNode(raw string) (preparedNode, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return preparedNode{}, fmt.Errorf("节点链接不能为空")
	}
	if len(raw) > maxNodeURLBytes {
		return preparedNode{}, fmt.Errorf("节点链接过长，最多 %d 字节", maxNodeURLBytes)
	}
	pn, err := parseNode(raw)
	if err != nil {
		return preparedNode{}, err
	}
	if len(pn.Name) > maxNodeNameBytes {
		return preparedNode{}, fmt.Errorf("节点备注过长，最多 %d 字节", maxNodeNameBytes)
	}
	return preparedNode{RawURL: raw, Parsed: pn}, nil
}

func (a *App) addPreparedNodeIndexed(st *Store, prepared preparedNode, name, scope string, urlIndex map[string]int) (string, error) {
	if prepared.Parsed == nil || prepared.RawURL == "" {
		return "", fmt.Errorf("节点解析结果无效")
	}
	// 操作员通过 `node add --stdin <name>` 或交互菜单直接提供的备注名，与订阅来源的
	// 节点名走同一条终端转义注入防护边界：在写入前清洗控制字符（见 sanitizeNodeName）。
	if name != "" {
		name = sanitizeNodeName(name)
		if len(name) > maxNodeNameBytes {
			return "", fmt.Errorf("节点备注过长，最多 %d 字节", maxNodeNameBytes)
		}
	}
	raw := prepared.RawURL
	pn := prepared.Parsed
	var old *Node
	if urlIndex != nil {
		if index, ok := urlIndex[raw]; ok && index >= 0 && index < len(st.Nodes) {
			old = &st.Nodes[index]
		}
	} else {
		old = st.findNodeByURL(raw)
	}
	if old != nil {
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
	if len(st.Nodes) >= maxTotalNodes {
		return "", fmt.Errorf("节点总数已达到上限 %d", maxTotalNodes)
	}
	id := newNodeID()
	for st.findNode(id) != nil {
		id = newNodeID()
	}
	if name == "" {
		name = pn.Name
	}
	if name == "" {
		name = pn.Protocol + "-node"
	}
	st.Nodes = append(st.Nodes, Node{ID: id, Name: name, Protocol: pn.Protocol, RawURL: raw, CreatedAt: time.Now(), UpdatedAt: time.Now()})
	if urlIndex != nil {
		urlIndex[raw] = len(st.Nodes) - 1
	}
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

func parseNode(raw string) (*parsedNode, error) {
	switch protocolFromURL(raw) {
	case "vless":
		return parseVLESS(raw)
	case "vmess":
		return parseVMess(raw)
	case "trojan":
		return parseTrojan(raw)
	case "ss":
		return parseSS(raw)
	default:
		return nil, fmt.Errorf("不支持的节点协议：%s", protocolFromURL(raw))
	}
}

func parseVLESS(raw string) (*parsedNode, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("VLESS 链接格式无效")
	}
	if u.Hostname() == "" || u.User == nil || u.User.Username() == "" {
		return nil, fmt.Errorf("VLESS 缺少服务器地址或用户 ID")
	}
	if u.EscapedPath() != "" {
		return nil, fmt.Errorf("VLESS 链接不能包含 path")
	}
	if _, hasPassword := u.User.Password(); hasPassword {
		return nil, fmt.Errorf("VLESS userinfo 只能包含用户 ID")
	}
	if err := validateNodeUUID(u.User.Username()); err != nil {
		return nil, fmt.Errorf("VLESS 用户 ID 无效：%w", err)
	}
	if err := validateNodeHost(u.Hostname()); err != nil {
		return nil, fmt.Errorf("VLESS 服务器地址无效：%w", err)
	}
	port, err := parseNodePort(u.Port(), "VLESS")
	if err != nil {
		return nil, err
	}
	q, err := parseStrictQuery(u.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("VLESS 查询参数格式无效")
	}
	if err := rejectUnknownQueryParams(q,
		"type", "network", "security", "sni", "serverName", "fp", "alpn",
		"pbk", "sid", "spx", "encryption", "flow",
		"host", "path", "headerType", "serviceName", "mode", "authority", "extra", "ed", "eh"); err != nil {
		return nil, fmt.Errorf("VLESS 查询参数无效：%w", err)
	}
	network, err := transportNetworkFromQuery(q)
	if err != nil {
		return nil, fmt.Errorf("VLESS transport 无效：%w", err)
	}
	security := strings.ToLower(q.Get("security"))
	if security == "" {
		security = "none"
	}
	if security != "none" && security != "tls" && security != "reality" {
		return nil, fmt.Errorf("VLESS security 仅支持 none/tls/reality")
	}
	if err := rejectUnusedSecurityParams(q, security, false); err != nil {
		return nil, fmt.Errorf("VLESS security 参数无效：%w", err)
	}
	encryption := firstNonEmpty(q.Get("encryption"), "none")
	if encryption != "none" {
		return nil, fmt.Errorf("VLESS encryption 当前仅支持 none")
	}
	flow := q.Get("flow")
	if flow != "" && flow != "xtls-rprx-vision" && flow != "xtls-rprx-vision-udp443" {
		return nil, fmt.Errorf("VLESS flow 无效")
	}
	if flow != "" && security == "none" {
		return nil, fmt.Errorf("VLESS flow 必须配合 TLS 或 REALITY")
	}
	stream := map[string]any{"network": network, "security": security}
	if security == "tls" {
		tlsSettings, err := buildTLSClientSettings(q, u.Hostname())
		if err != nil {
			return nil, fmt.Errorf("VLESS TLS 参数无效：%w", err)
		}
		stream["tlsSettings"] = tlsSettings
	}
	if security == "reality" {
		realitySettings, err := buildRealityClientSettings(q)
		if err != nil {
			return nil, fmt.Errorf("VLESS REALITY 参数无效：%w", err)
		}
		stream["realitySettings"] = realitySettings
	}
	if err := addTransport(stream, network, q); err != nil {
		return nil, fmt.Errorf("VLESS transport 无效：%w", err)
	}
	if security == "reality" && !transportSupportsReality(stream["network"]) {
		return nil, fmt.Errorf("VLESS REALITY 仅支持 RAW/XHTTP/gRPC transport")
	}
	if flow != "" && stream["network"] != "raw" {
		return nil, fmt.Errorf("VLESS flow 当前仅支持 RAW transport")
	}
	out := map[string]any{"tag": "", "protocol": "vless", "settings": map[string]any{"vnext": []any{map[string]any{"address": u.Hostname(), "port": port, "users": []any{map[string]any{"id": u.User.Username(), "encryption": encryption, "flow": flow}}}}}, "streamSettings": stream}
	return &parsedNode{Protocol: "vless", Name: sanitizeNodeName(remark(raw, "vless-"+u.Hostname())), EndpointHost: u.Hostname(), EndpointPort: port, Outbound: out}, nil
}

func parseVMess(raw string) (*parsedNode, error) {
	payload := payloadAfterScheme(raw)
	payload = strings.Split(payload, "#")[0]
	b, err := decodeBase64URL(payload)
	if err != nil {
		return nil, fmt.Errorf("VMess payload 不是有效 Base64")
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("VMess payload 不是有效 JSON")
	}
	if err := rejectUnknownVMessFields(v); err != nil {
		return nil, err
	}
	if version := stringVal(v, "v"); version != "" && version != "2" {
		return nil, fmt.Errorf("VMess 链接版本无效")
	}
	addr := stringVal(v, "add")
	port, err := exactIntVal(v, "port", true)
	if err != nil {
		return nil, fmt.Errorf("VMess 端口无效")
	}
	id := stringVal(v, "id")
	if addr == "" || id == "" {
		return nil, fmt.Errorf("VMess 缺少 add/id")
	}
	if err := validateNodeHost(addr); err != nil {
		return nil, fmt.Errorf("VMess 服务器地址无效：%w", err)
	}
	if err := validateNodeUUID(id); err != nil {
		return nil, fmt.Errorf("VMess 用户 ID 无效：%w", err)
	}
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("VMess 端口无效")
	}
	network := firstNonEmpty(stringVal(v, "net"), "tcp")
	tlsMode := strings.ToLower(stringVal(v, "tls"))
	if tlsMode != "" && tlsMode != "none" && tlsMode != "tls" {
		return nil, fmt.Errorf("VMess TLS 模式无效")
	}
	security := "none"
	if tlsMode == "tls" {
		security = "tls"
	}
	if security != "tls" && anyMapStringValue(v, "sni", "fp", "alpn") {
		return nil, fmt.Errorf("VMess sni/fp/alpn 仅能配合 TLS")
	}
	stream := map[string]any{"network": network, "security": security}
	if security == "tls" {
		// SNI 回退顺序 sni -> host -> add：VMess 常把伪装域名放在 host、add 填裸 IP
		// （CDN/域前置），直接回退到 add(IP) 会让 TLS 握手用 IP 作 SNI 而被服务端拒绝。
		q := url.Values{
			"sni":  []string{firstNonEmpty(stringVal(v, "sni"), stringVal(v, "host"), addr)},
			"fp":   []string{stringVal(v, "fp")},
			"alpn": []string{stringVal(v, "alpn")},
		}
		tlsSettings, err := buildTLSClientSettings(q, addr)
		if err != nil {
			return nil, fmt.Errorf("VMess TLS 参数无效：%w", err)
		}
		stream["tlsSettings"] = tlsSettings
	}
	// VMess gRPC 的 serviceName 按 v2rayN 约定承载在 path 字段（net=grpc 复用 path），
	// 优先取显式 serviceName，否则回退 path，避免生成空 serviceName 导致 gRPC 连不通。
	q := url.Values{
		"host":        []string{stringVal(v, "host")},
		"path":        []string{stringVal(v, "path")},
		"serviceName": []string{stringVal(v, "serviceName")},
		"mode":        []string{stringVal(v, "mode")},
		"authority":   []string{stringVal(v, "authority")},
		"extra":       []string{stringVal(v, "extra")},
		"ed":          []string{stringVal(v, "ed")},
		"eh":          []string{stringVal(v, "eh")},
	}
	transportType := stringVal(v, "type")
	switch strings.ToLower(network) {
	case "grpc":
		if mode := stringVal(v, "mode"); mode != "" && transportType != "" && !strings.EqualFold(mode, transportType) {
			return nil, fmt.Errorf("VMess gRPC mode/type 冲突")
		}
		q.Set("serviceName", firstNonEmpty(stringVal(v, "serviceName"), stringVal(v, "path")))
		q.Set("mode", firstNonEmpty(stringVal(v, "mode"), transportType))
	case "", "tcp", "raw":
		q.Set("headerType", transportType)
	default:
		if transportType != "" && !strings.EqualFold(transportType, "none") {
			q.Set("headerType", transportType)
		}
	}
	if err := addTransport(stream, network, q); err != nil {
		return nil, fmt.Errorf("VMess transport 无效：%w", err)
	}
	alterID, err := exactIntVal(v, "aid", false)
	if err != nil || alterID < 0 {
		return nil, fmt.Errorf("VMess alterId 无效")
	}
	cipher := strings.ToLower(firstNonEmpty(stringVal(v, "scy"), "auto"))
	if !validVMessCipher(cipher) {
		return nil, fmt.Errorf("VMess cipher 无效")
	}
	out := map[string]any{"tag": "", "protocol": "vmess", "settings": map[string]any{"vnext": []any{map[string]any{"address": addr, "port": port, "users": []any{map[string]any{"id": id, "alterId": alterID, "security": cipher}}}}}, "streamSettings": stream}
	return &parsedNode{Protocol: "vmess", Name: sanitizeNodeName(firstNonEmpty(stringVal(v, "ps"), "vmess-"+addr)), EndpointHost: addr, EndpointPort: port, Outbound: out}, nil
}

func parseTrojan(raw string) (*parsedNode, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("无效的 Trojan 链接格式")
	}
	if u.Hostname() == "" || u.User == nil || u.User.Username() == "" {
		return nil, fmt.Errorf("该 Trojan 链接缺少服务器地址或密码")
	}
	if u.EscapedPath() != "" {
		return nil, fmt.Errorf("该 Trojan 链接不能包含 path")
	}
	if err := validateNodeHost(u.Hostname()); err != nil {
		return nil, fmt.Errorf("无效的 Trojan 服务器地址：%w", err)
	}
	port, err := parseNodePort(u.Port(), "Trojan")
	if err != nil {
		return nil, err
	}
	q, err := parseStrictQuery(u.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("无效的 Trojan 查询参数格式")
	}
	if err := rejectUnknownQueryParams(q,
		"type", "network", "security", "sni", "serverName", "peer", "fp", "alpn",
		"pbk", "sid", "spx", "host", "path", "headerType", "serviceName", "mode", "authority", "extra", "ed", "eh"); err != nil {
		return nil, fmt.Errorf("无效的 Trojan 查询参数：%w", err)
	}
	network, err := transportNetworkFromQuery(q)
	if err != nil {
		return nil, fmt.Errorf("无效的 Trojan transport：%w", err)
	}
	security := strings.ToLower(firstNonEmpty(q.Get("security"), "tls"))
	if security != "none" && security != "tls" && security != "reality" {
		return nil, fmt.Errorf("该 Trojan security 仅支持 none/tls/reality")
	}
	if err := rejectUnusedSecurityParams(q, security, true); err != nil {
		return nil, fmt.Errorf("无效的 Trojan security 参数：%w", err)
	}
	serverName, err := consistentQueryValue(q, "sni", "serverName", "peer")
	if err != nil {
		return nil, fmt.Errorf("检测到 Trojan TLS serverName 参数冲突")
	}
	if serverName != "" {
		q.Set("serverName", serverName)
	}
	stream := map[string]any{"network": network, "security": security}
	if security == "tls" {
		q.Set("serverName", firstNonEmpty(serverName, u.Hostname()))
		tlsSettings, err := buildTLSClientSettings(q, u.Hostname())
		if err != nil {
			return nil, fmt.Errorf("无效的 Trojan TLS 参数：%w", err)
		}
		stream["tlsSettings"] = tlsSettings
	}
	if security == "reality" {
		realitySettings, err := buildRealityClientSettings(q)
		if err != nil {
			return nil, fmt.Errorf("无效的 Trojan REALITY 参数：%w", err)
		}
		stream["realitySettings"] = realitySettings
	}
	if err := addTransport(stream, network, q); err != nil {
		return nil, fmt.Errorf("无效的 Trojan transport：%w", err)
	}
	if security == "reality" && !transportSupportsReality(stream["network"]) {
		return nil, fmt.Errorf("该 Trojan REALITY 仅支持 RAW/XHTTP/gRPC transport")
	}
	out := map[string]any{"tag": "", "protocol": "trojan", "settings": map[string]any{"servers": []any{map[string]any{"address": u.Hostname(), "port": port, "password": trojanPassword(u)}}}, "streamSettings": stream}
	return &parsedNode{Protocol: "trojan", Name: sanitizeNodeName(remark(raw, "trojan-"+u.Hostname())), EndpointHost: u.Hostname(), EndpointPort: port, Outbound: out}, nil
}

// trojanPassword 还原 Trojan 链接中的完整密码。url.Userinfo 会按第一个冒号把
// userinfo 拆成用户名/密码，而 Trojan 的整段 userinfo 都是密码，因此需要把两段拼回。
func trojanPassword(u *url.URL) string {
	password := u.User.Username()
	if pw, ok := u.User.Password(); ok {
		password += ":" + pw
	}
	return password
}

func parseSS(raw string) (*parsedNode, error) {
	if queryStart := strings.IndexByte(raw, '?'); queryStart >= 0 {
		query := raw[queryStart+1:]
		if fragmentStart := strings.IndexByte(query, '#'); fragmentStart >= 0 {
			query = query[:fragmentStart]
		}
		values, err := url.ParseQuery(query)
		if err != nil {
			return nil, fmt.Errorf("SS 查询参数无效")
		}
		if values.Get("plugin") != "" {
			return nil, fmt.Errorf("不支持带 plugin 的 SS 节点")
		}
		if err := rejectUnknownQueryParams(values, "plugin"); err != nil {
			return nil, fmt.Errorf("SS 查询参数无效：%w", err)
		}
	}
	body := payloadAfterScheme(raw)
	body = strings.Split(strings.Split(body, "#")[0], "?")[0]
	var userinfo, hostport string
	if strings.Contains(body, "@") {
		parts := strings.SplitN(body, "@", 2)
		userinfo, hostport = parts[0], parts[1]
		// SIP002 允许 host:port 后带一个可选的 `/`（如 ss://...@host:port/?plugin=... 或
		// ss://...@host:port/#name）。前面已去掉 ? 和 #，这里再去掉路径分隔符，否则
		// net.SplitHostPort 会因端口含 `/` 失败、整条链接被当作无效丢弃。
		if i := strings.IndexByte(hostport, '/'); i >= 0 {
			if hostport[i:] != "/" {
				return nil, fmt.Errorf("SS 链接不能包含非空 path")
			}
			hostport = hostport[:i]
		}
		if !strings.Contains(userinfo, ":") {
			if b, err := decodeBase64URL(userinfo); err == nil {
				userinfo = string(b)
			}
		}
	} else {
		b, err := decodeBase64URL(body)
		if err != nil {
			return nil, fmt.Errorf("SS payload 不是有效 Base64")
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
	method := strings.ToLower(strings.TrimSpace(up[0]))
	if !validShadowsocksMethod(method) {
		return nil, fmt.Errorf("SS method 不受当前 Xray 支持")
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
	if err := validateNodeHost(addr); err != nil {
		return nil, fmt.Errorf("SS 服务器地址无效：%w", err)
	}
	port, err := parseNodePort(portText, "SS")
	if err != nil {
		return nil, err
	}
	out := map[string]any{"tag": "", "protocol": "shadowsocks", "settings": map[string]any{"servers": []any{map[string]any{"address": addr, "port": port, "method": method, "password": up[1]}}}}
	return &parsedNode{Protocol: "ss", Name: sanitizeNodeName(remark(raw, "ss-"+addr)), EndpointHost: addr, EndpointPort: port, Outbound: out}, nil
}

func parseNodePort(raw, protocol string) (int, error) {
	port, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || port <= 0 || port > 65535 {
		return 0, fmt.Errorf("%s 端口无效", protocol)
	}
	return port, nil
}

func validateNodeHost(host string) error {
	if host == "" || len(host) > maxNodeHostBytes {
		return fmt.Errorf("主机名为空或超过 %d 字节", maxNodeHostBytes)
	}
	for _, r := range host {
		if unicode.IsControl(r) || unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("主机名含控制字符或空白")
		}
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Zone() != "" {
			return fmt.Errorf("IP 地址不能带 zone")
		}
		return nil
	}
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return fmt.Errorf("主机名为空")
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("主机名标签无效")
		}
		for _, c := range label {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-') {
				return fmt.Errorf("主机名含非法字符")
			}
		}
	}
	return nil
}

func validateNodeUUID(id string) error {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return fmt.Errorf("必须是标准 UUID")
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return fmt.Errorf("必须是标准 UUID")
		}
	}
	return nil
}

func buildTLSClientSettings(q url.Values, fallbackServerName string) (map[string]any, error) {
	serverName, err := consistentQueryValue(q, "sni", "serverName")
	if err != nil {
		return nil, err
	}
	serverName = firstNonEmpty(serverName, fallbackServerName)
	if err := validateNodeHost(serverName); err != nil {
		return nil, err
	}
	fingerprint := strings.ToLower(strings.TrimSpace(q.Get("fp")))
	if fingerprint == "" {
		fingerprint = "chrome"
	}
	if !validTLSFingerprint(fingerprint, false) {
		return nil, fmt.Errorf("fingerprint 无效")
	}
	settings := map[string]any{"serverName": serverName}
	if fingerprint != "" {
		settings["fingerprint"] = fingerprint
	}
	alpn, err := splitCSV(q.Get("alpn"))
	if err != nil {
		return nil, fmt.Errorf("alpn 无效：%w", err)
	}
	if len(alpn) > 0 {
		for _, value := range alpn {
			if err := validateTransportText("alpn", value, 255); err != nil {
				return nil, err
			}
		}
		settings["alpn"] = alpn
	}
	return settings, nil
}

func buildRealityClientSettings(q url.Values) (map[string]any, error) {
	publicKey := strings.TrimSpace(q.Get("pbk"))
	decoded, err := decodeBase64URL(publicKey)
	if err != nil || len(decoded) != 32 {
		return nil, fmt.Errorf("publicKey(pbk) 必须是 32 字节 X25519 公钥")
	}
	fingerprint := strings.ToLower(strings.TrimSpace(q.Get("fp")))
	if fingerprint == "" {
		fingerprint = "chrome"
	}
	if !validTLSFingerprint(fingerprint, true) {
		return nil, fmt.Errorf("fingerprint 无效")
	}
	shortID := strings.TrimSpace(q.Get("sid"))
	if len(shortID) > 16 || len(shortID)%2 != 0 {
		return nil, fmt.Errorf("shortId 必须是不超过 16 位的偶数长度十六进制")
	}
	if shortID != "" {
		if _, err := hex.DecodeString(shortID); err != nil {
			return nil, fmt.Errorf("shortId 必须是十六进制")
		}
	}
	serverName, err := consistentQueryValue(q, "sni", "serverName")
	if err != nil {
		return nil, err
	}
	if err := validateNodeHost(serverName); err != nil {
		return nil, err
	}
	spiderX := firstNonEmpty(q.Get("spx"), "/")
	if err := validateTransportText("spiderX", spiderX, 4096); err != nil || !strings.HasPrefix(spiderX, "/") {
		return nil, fmt.Errorf("spiderX 必须是以 / 开头的有效路径")
	}
	return map[string]any{
		"serverName":  serverName,
		"fingerprint": fingerprint,
		"publicKey":   publicKey,
		"shortId":     shortID,
		"spiderX":     spiderX,
	}, nil
}

func validTLSFingerprint(value string, reality bool) bool {
	if value == "" {
		return true
	}
	allowed := map[string]bool{
		"chrome": true, "firefox": true, "safari": true, "ios": true,
		"android": true, "edge": true, "360": true, "qq": true,
		"random": true, "randomized": true, "randomizednoalpn": true,
	}
	if !reality {
		allowed["hellogolang"] = true
	}
	return allowed[value]
}

func validVMessCipher(value string) bool {
	switch value {
	case "auto", "aes-128-gcm", "chacha20-poly1305", "none", "zero":
		return true
	default:
		return false
	}
}

func validShadowsocksMethod(value string) bool {
	switch value {
	case "aes-128-gcm", "aead_aes_128_gcm",
		"aes-256-gcm", "aead_aes_256_gcm",
		"chacha20-poly1305", "aead_chacha20_poly1305", "chacha20-ietf-poly1305",
		"xchacha20-poly1305", "aead_xchacha20_poly1305", "xchacha20-ietf-poly1305",
		"none", "plain":
		return true
	default:
		return false
	}
}

func transportSupportsReality(network any) bool {
	value, _ := network.(string)
	return value == "raw" || value == "xhttp" || value == "grpc"
}

func addTransport(stream map[string]any, network string, q url.Values) error {
	network = strings.ToLower(strings.TrimSpace(network))
	switch network {
	case "", "tcp", "raw":
		stream["network"] = "raw"
		if err := rejectTransportParams(q, "headerType", "host", "path"); err != nil {
			return err
		}
		headerType := strings.ToLower(strings.TrimSpace(q.Get("headerType")))
		switch headerType {
		case "", "none":
			if q.Get("host") != "" || q.Get("path") != "" {
				return fmt.Errorf("RAW 未启用 HTTP header 时不能携带 host/path")
			}
		case "http":
			path := firstNonEmpty(q.Get("path"), "/")
			if err := validateTransportPath(path); err != nil {
				return err
			}
			headers := map[string]any{}
			hosts, err := splitCSV(q.Get("host"))
			if err != nil {
				return fmt.Errorf("RAW HTTP Host 无效：%w", err)
			}
			if len(hosts) > 0 {
				for _, host := range hosts {
					if err := validateTransportHost("host", host); err != nil {
						return err
					}
				}
				headers["Host"] = hosts
			}
			request := map[string]any{"version": "1.1", "method": "GET", "path": []string{path}}
			if len(headers) > 0 {
				request["headers"] = headers
			}
			stream["rawSettings"] = map[string]any{"header": map[string]any{"type": "http", "request": request}}
		default:
			return fmt.Errorf("RAW 仅支持 none/http header")
		}
	case "ws", "websocket":
		stream["network"] = "ws"
		if err := rejectTransportParams(q, "host", "path", "ed"); err != nil {
			return err
		}
		settings, err := httpLikeTransportSettings(q, true)
		if err != nil {
			return err
		}
		stream["wsSettings"] = settings
	case "httpupgrade":
		stream["network"] = "httpupgrade"
		if err := rejectTransportParams(q, "host", "path", "ed"); err != nil {
			return err
		}
		settings, err := httpLikeTransportSettings(q, true)
		if err != nil {
			return err
		}
		stream["httpupgradeSettings"] = settings
	case "grpc":
		stream["network"] = "grpc"
		if err := rejectTransportParams(q, "host", "serviceName", "path", "mode", "authority"); err != nil {
			return err
		}
		serviceName, err := consistentQueryValue(q, "serviceName", "path")
		if err != nil {
			return fmt.Errorf("gRPC serviceName/path 参数冲突")
		}
		authority, err := consistentQueryValue(q, "authority", "host")
		if err != nil {
			return fmt.Errorf("gRPC authority/host 参数冲突")
		}
		if err := validateTransportText("serviceName", serviceName, 4096); err != nil {
			return err
		}
		if err := validateTransportHost("authority", authority); err != nil {
			return err
		}
		settings := map[string]any{"serviceName": serviceName}
		if authority != "" {
			settings["authority"] = authority
		}
		switch strings.ToLower(strings.TrimSpace(q.Get("mode"))) {
		case "", "gun":
		case "multi":
			settings["multiMode"] = true
		default:
			return fmt.Errorf("gRPC mode 仅支持 gun/multi")
		}
		stream["grpcSettings"] = settings
	case "xhttp", "splithttp":
		stream["network"] = "xhttp"
		if err := rejectTransportParams(q, "host", "path", "mode", "extra"); err != nil {
			return err
		}
		host := q.Get("host")
		path := firstNonEmpty(q.Get("path"), "/")
		if err := validateTransportHost("host", host); err != nil {
			return err
		}
		if err := validateTransportPath(path); err != nil {
			return err
		}
		mode := strings.ToLower(strings.TrimSpace(q.Get("mode")))
		switch mode {
		case "", "auto", "packet-up", "stream-up", "stream-one":
		default:
			return fmt.Errorf("XHTTP mode 无效")
		}
		settings := map[string]any{"path": path}
		if host != "" {
			settings["host"] = host
		}
		if mode != "" {
			settings["mode"] = mode
		}
		if extra := q.Get("extra"); extra != "" {
			var object map[string]any
			if err := json.Unmarshal([]byte(extra), &object); err != nil || object == nil {
				return fmt.Errorf("XHTTP extra 必须是 JSON 对象")
			}
			settings["extra"] = json.RawMessage(extra)
		}
		stream["xhttpSettings"] = settings
	case "h2", "h3", "http", "quic":
		return fmt.Errorf("当前 Xray 已移除此 transport，请改用 XHTTP")
	default:
		return fmt.Errorf("不支持的 transport")
	}
	return nil
}

var transportParameterNames = []string{
	"headerType", "host", "path", "seed", "quicSecurity", "key", "serviceName", "mode", "authority", "extra", "ed", "eh",
}

func rejectTransportParams(q url.Values, allowed ...string) error {
	allowedSet := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = true
	}
	for _, key := range transportParameterNames {
		if !allowedSet[key] && q.Get(key) != "" {
			return fmt.Errorf("当前 transport 无法表达参数 %s", key)
		}
	}
	return nil
}

func httpLikeTransportSettings(q url.Values, earlyData bool) (map[string]any, error) {
	host := q.Get("host")
	path := firstNonEmpty(q.Get("path"), "/")
	if err := validateTransportHost("host", host); err != nil {
		return nil, err
	}
	if err := validateTransportPath(path); err != nil {
		return nil, err
	}
	pathURL, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("transport path 无效")
	}
	pathQuery, err := url.ParseQuery(pathURL.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("transport path query 无效")
	}
	for _, key := range []string{"ed", "eh"} {
		if _, exists := pathQuery[key]; exists {
			return nil, fmt.Errorf("transport path 不能直接包含 %s 参数", key)
		}
	}
	if ed := strings.TrimSpace(q.Get("ed")); ed != "" {
		if !earlyData {
			return nil, fmt.Errorf("当前 transport 不支持 early data")
		}
		value, err := strconv.ParseUint(ed, 10, 32)
		if err != nil || value == 0 {
			return nil, fmt.Errorf("early data 参数无效")
		}
		// Xray v26.3.27 no longer exposes maxEarlyData/earlyDataHeaderName
		// fields. Its WS and HTTPUpgrade loaders consume `ed` from the path
		// query itself, so preserve the share-link value in exactly that form.
		u := pathURL
		values := pathQuery
		values.Set("ed", strconv.FormatUint(value, 10))
		u.RawQuery = values.Encode()
		path = u.String()
	}
	settings := map[string]any{"path": path}
	if host != "" {
		settings["host"] = host
	}
	return settings, nil
}

func validateTransportText(field, value string, maxBytes int) error {
	if len(value) > maxBytes {
		return fmt.Errorf("transport %s 超过 %d 字节", field, maxBytes)
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("transport %s 含控制字符", field)
		}
	}
	return nil
}

func validateTransportHost(field, value string) error {
	if value == "" {
		return nil
	}
	if err := validateTransportText(field, value, 1024); err != nil {
		return err
	}
	host := value
	if parsedHost, port, err := net.SplitHostPort(value); err == nil {
		host = parsedHost
		if _, err := parseNodePort(port, "transport"); err != nil {
			return err
		}
	}
	if err := validateNodeHost(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")); err != nil {
		return fmt.Errorf("transport %s 主机无效：%w", field, err)
	}
	return nil
}

func validateTransportPath(path string) error {
	if err := validateTransportText("path", path, 4096); err != nil {
		return err
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("transport path 必须以 / 开头")
	}
	return nil
}

func remark(raw, fallback string) string {
	if i := strings.LastIndex(raw, "#"); i >= 0 && i+1 < len(raw) {
		if s, err := url.QueryUnescape(raw[i+1:]); err == nil && s != "" {
			return s
		}
	}
	return fallback
}

// sanitizeNodeName 移除节点名中的控制字符（含 ESC/BEL/CR/LF 等，字节 < 0x20 及 0x7f），
// 防止攻击者通过订阅节点名注入终端转义序列，污染 root 操作员的终端输出。
func sanitizeNodeName(s string) string {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" {
		return "node"
	}
	return cleaned
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
func splitCSV(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
		if parts[i] == "" {
			return nil, fmt.Errorf("CSV 参数不能包含空元素")
		}
	}
	return parts, nil
}
func stringVal(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}
func exactIntVal(m map[string]any, key string, required bool) (int, error) {
	value, exists := m[key]
	if !exists || value == nil || value == "" {
		if required {
			return 0, fmt.Errorf("缺少整数字段")
		}
		return 0, nil
	}
	switch value := value.(type) {
	case float64:
		maxInt := float64(int64(^uint(0) >> 1))
		minInt := -maxInt - 1
		if math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value || value > maxInt || value < minInt {
			return 0, fmt.Errorf("整数字段无效")
		}
		return int(value), nil
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0, fmt.Errorf("整数字段无效")
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("整数字段类型无效")
	}
}

func rejectUnknownQueryParams(q url.Values, allowed ...string) error {
	allowedSet := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = true
	}
	for key, values := range q {
		if !allowedSet[key] {
			return fmt.Errorf("含不受支持的查询参数")
		}
		if len(values) != 1 {
			return fmt.Errorf("查询参数不能重复")
		}
	}
	return nil
}

func parseStrictQuery(raw string) (url.Values, error) {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return nil, err
	}
	for _, entries := range values {
		if len(entries) != 1 {
			return nil, fmt.Errorf("查询参数不能重复")
		}
	}
	return values, nil
}

func transportNetworkFromQuery(q url.Values) (string, error) {
	fromType := strings.TrimSpace(q.Get("type"))
	fromNetwork := strings.TrimSpace(q.Get("network"))
	if fromType != "" && fromNetwork != "" && !strings.EqualFold(fromType, fromNetwork) {
		return "", fmt.Errorf("type/network 参数冲突")
	}
	return firstNonEmpty(fromType, fromNetwork, "tcp"), nil
}

func consistentQueryValue(q url.Values, keys ...string) (string, error) {
	value := ""
	for _, key := range keys {
		candidate := strings.TrimSpace(q.Get(key))
		if candidate == "" {
			continue
		}
		if value != "" && candidate != value {
			return "", fmt.Errorf("等价参数值冲突")
		}
		value = candidate
	}
	return value, nil
}

func rejectUnusedSecurityParams(q url.Values, security string, allowPeer bool) error {
	tlsOrReality := []string{"sni", "serverName", "fp"}
	if allowPeer {
		tlsOrReality = append(tlsOrReality, "peer")
	}
	switch security {
	case "none":
		if anyQueryValue(q, append(tlsOrReality, "alpn", "pbk", "sid", "spx")...) {
			return fmt.Errorf("TLS/REALITY 参数不能配合 security=none")
		}
	case "tls":
		if anyQueryValue(q, "pbk", "sid", "spx") {
			return fmt.Errorf("pbk/sid/spx 仅能配合 REALITY")
		}
	case "reality":
		if anyQueryValue(q, "alpn") {
			return fmt.Errorf("REALITY 不支持 alpn 参数")
		}
	}
	return nil
}

func anyQueryValue(q url.Values, keys ...string) bool {
	for _, key := range keys {
		if q.Get(key) != "" {
			return true
		}
	}
	return false
}

func anyMapStringValue(values map[string]any, keys ...string) bool {
	for _, key := range keys {
		if stringVal(values, key) != "" {
			return true
		}
	}
	return false
}

func rejectUnknownVMessFields(values map[string]any) error {
	stringFields := map[string]bool{
		"v": true, "ps": true, "add": true, "id": true, "scy": true,
		"net": true, "type": true, "host": true, "path": true, "tls": true,
		"sni": true, "alpn": true, "fp": true, "serviceName": true,
		"mode": true, "authority": true, "extra": true, "ed": true, "eh": true,
	}
	for key, value := range values {
		if key == "port" || key == "aid" {
			continue
		}
		if !stringFields[key] {
			return fmt.Errorf("VMess 含不受支持的字段")
		}
		if _, ok := value.(string); !ok {
			return fmt.Errorf("VMess 字段类型无效")
		}
	}
	return nil
}

type storeRuntimeSyncMode uint8

const (
	storeRuntimeSyncNone storeRuntimeSyncMode = iota
	storeRuntimeSyncXray
	storeRuntimeSyncAll
	storeRuntimeSyncTransition
)

type storeRuntimeSyncOp func(*App, *Store, storeRuntimeSyncMode) error
type storeRuntimePersistOp func(*App, *Store) error
type storeRuntimeCleanupOp func(*App, *Store) error

func (a *App) syncNodeRuntime(st *Store) error {
	return a.syncXrayServiceForStore(st)
}

func (a *App) syncNodeRuntimeAndScenes(st *Store) error {
	return a.syncNodeRuntimeAndScenesWithCleanup(st, true)
}

func (a *App) syncNodeRuntimeAndScenesWithCleanup(st *Store, cleanupDisabled bool) error {
	// RuntimeConfig 包含 Xray 服务用户。即使当前没有启用场景，也必须重写 unit 并
	// 协调目录/文件属组，不能把新配置落盘后仍让 systemd 使用旧 User=。
	if err := a.installXrayService(); err != nil {
		return err
	}
	if hasEnabledScene(st) {
		if err := a.reloadIfEnabled(st); err != nil {
			return err
		}
		return a.applySavedScenesWithCleanup(st, cleanupDisabled)
	}
	if err := a.applySavedScenesWithCleanup(st, cleanupDisabled); err != nil {
		return err
	}
	if err := a.stopXrayService(); err != nil {
		return err
	}
	return a.clearXrayConfig()
}

func syncStoreRuntime(app *App, st *Store, mode storeRuntimeSyncMode) error {
	switch mode {
	case storeRuntimeSyncNone:
		return nil
	case storeRuntimeSyncXray:
		return app.syncNodeRuntime(st)
	case storeRuntimeSyncAll:
		return app.syncNodeRuntimeAndScenes(st)
	case storeRuntimeSyncTransition:
		return app.syncNodeRuntimeAndScenesWithCleanup(st, false)
	default:
		return fmt.Errorf("未知运行态同步模式：%d", mode)
	}
}

func persistStoreRuntime(app *App, st *Store) error {
	return app.saveStore(st)
}

func cleanupStoreRuntimeScenes(app *App, st *Store) error {
	var errs []error
	for _, scene := range []Scene{SceneTelegram, SceneDev, SceneGlobal} {
		// Global and Telegram cleanup is journal-driven and safe even when the
		// Store says disabled. Always reconcile both so a crash after an artifact
		// write but before Store commit cannot strand managed residue.
		if scene == SceneDev && !st.SceneEnabled[scene] {
			continue
		}
		if scene == SceneGlobal && st.SceneEnabled[scene] && st.RuntimeConfig == nil {
			if err := app.migrateLegacyGlobalProxyOwnership(); err != nil {
				errs = append(errs, fmt.Errorf("迁移旧全局代理 ownership 失败：%w", err))
				continue
			}
		}
		if err := app.restoreScene(st, scene); err != nil {
			errs = append(errs, fmt.Errorf("清理旧%s运行配置失败：%w", sceneName(scene), err))
		}
	}
	return errors.Join(errs...)
}

func (a *App) commitStoreMutation(st *Store, mutate func(*Store) error, mode storeRuntimeSyncMode) error {
	return a.commitStoreMutationWithRuntimeOps(st, mutate, mode, syncStoreRuntime, cleanupStoreRuntimeScenes, persistStoreRuntime)
}

func (a *App) commitStoreMutationWithRuntimeOps(st *Store, mutate func(*Store) error, mode storeRuntimeSyncMode, syncRuntime storeRuntimeSyncOp, cleanupRuntime storeRuntimeCleanupOp, persist storeRuntimePersistOp) error {
	before := cloneStore(st)
	if err := mutate(st); err != nil {
		restoreStore(st, before)
		return err
	}
	if st.SceneEnabled[SceneDev] && a.cfg.DevTargetUser == "" {
		user, err := a.devTargetUser()
		if err != nil {
			restoreStore(st, before)
			return fmt.Errorf("确定开发代理持久目标用户失败：%w", err)
		}
		a.cfg.DevTargetUser = user
	}
	configChanged := a.stageRuntimeConfig(st)
	if configChanged {
		mode = storeRuntimeSyncTransition
		oldApp, appErr := a.appForStoreRuntime(before)
		if appErr != nil {
			restoreStore(st, before)
			return appErr
		}
		cleanupState := cloneStore(before)
		if err := cleanupRuntime(oldApp, cleanupState); err != nil {
			rollbackState := cloneStore(before)
			rollbackErr := syncRuntime(oldApp, rollbackState, storeRuntimeSyncAll)
			restoreStore(st, rollbackState)
			return errors.Join(err, wrapRollbackError("重建旧运行状态", rollbackErr))
		}
		// TelegramTargets 仅承载旧版 drop-in 的迁移证据；旧配置清理后只保留
		// 尚未能安全协调的记录。新 Hermes/OpenClaw ownership 位于独立 journal。
		st.TelegramTargets = append([]string(nil), cleanupState.TelegramTargets...)
	}
	if err := syncRuntime(a, st, mode); err != nil {
		rollbackState, rollbackApp, rollbackErr := a.rollbackStoreRuntime(before, st, mode, configChanged, syncRuntime, cleanupRuntime)
		restoreStore(st, rollbackState)
		var journalErr error
		if !slices.Equal(before.TelegramTargets, rollbackState.TelegramTargets) {
			journalErr = persist(rollbackApp, st)
		}
		return errors.Join(err, wrapRollbackError("恢复旧运行状态", rollbackErr), wrapRollbackError("保存待重试清理状态", journalErr))
	}
	if err := persist(a, st); err != nil {
		rollbackState, rollbackApp, rollbackErr := a.rollbackStoreRuntime(before, st, mode, configChanged, syncRuntime, cleanupRuntime)
		persistErr := persist(rollbackApp, rollbackState)
		restoreStore(st, rollbackState)
		return errors.Join(err, wrapRollbackError("恢复旧运行状态", rollbackErr), wrapRollbackError("恢复旧状态文件", persistErr))
	}
	return nil
}

func (a *App) rollbackStoreRuntime(before, candidate *Store, mode storeRuntimeSyncMode, configChanged bool, syncRuntime storeRuntimeSyncOp, cleanupRuntime storeRuntimeCleanupOp) (*Store, *App, error) {
	partialTelegramTargets := append([]string(nil), candidate.TelegramTargets...)
	var cleanupErr error
	if configChanged {
		cleanupState := a.candidateRuntimeCleanupState(candidate)
		cleanupErr = cleanupRuntime(a, cleanupState)
		partialTelegramTargets = append([]string(nil), cleanupState.TelegramTargets...)
	}
	rollbackState := cloneStore(before)
	// TelegramTargets only carries legacy migration evidence. Retain unresolved
	// candidate records so rollback can retry them under the configuration that
	// originally generated their exact bytes.
	if len(partialTelegramTargets) > 0 {
		rollbackState.TelegramTargets = mergeStringSlices(rollbackState.TelegramTargets, partialTelegramTargets)
	}
	rollbackApp, err := a.appForStoreRuntime(before)
	if err != nil {
		return rollbackState, a, err
	}
	rollbackMode := mode
	if configChanged {
		rollbackMode = storeRuntimeSyncAll
	}
	return rollbackState, rollbackApp, errors.Join(cleanupErr, syncRuntime(rollbackApp, rollbackState, rollbackMode))
}

func (a *App) candidateRuntimeCleanupState(candidate *Store) *Store {
	state := cloneStore(candidate)
	// New Telegram ownership lives in dedicated Hermes/OpenClaw journals. Setting
	// the scene false makes cleanup use those journals plus any legacy evidence;
	// it must never discover paths and infer ownership from the candidate config.
	state.SceneEnabled[SceneTelegram] = false
	// applyDev writes its backup before changing git/npm. If that backup does not
	// exist, candidate Dev ownership was never established and value-only cleanup
	// could delete an operator's coincidentally identical manual proxy setting.
	if state.SceneEnabled[SceneDev] {
		if _, err := a.loadDevBackup(); errors.Is(err, os.ErrNotExist) {
			state.SceneEnabled[SceneDev] = false
		}
	}
	return state
}

// commitNodeStoreMutation applies a candidate mutation to st, synchronizes the
// generated Xray/runtime state first, and persists only after that succeeds.
// app.install can use the same helper when it imports its optional node.
func (a *App) commitNodeStoreMutation(st *Store, mutate func(*Store) error) error {
	return a.commitStoreMutation(st, mutate, storeRuntimeSyncXray)
}

func wrapRollbackError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s失败：%w", action, err)
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
	switch args[0] {
	case "add":
		parsed, err := parseNodeAddCommandArgs(args[1:])
		if err != nil {
			return err
		}
		raw, err := resolveNodeSecretArg(parsed.Raw, parsed.FromStdin, "节点链接: ", "节点链接")
		if err != nil {
			return err
		}
		prepared, err := prepareNode(raw)
		if err != nil {
			return err
		}
		return a.withStoreLock(func() error {
			st, err := a.loadStore()
			if err != nil {
				return err
			}
			return a.commitNodeStoreMutation(st, func(candidate *Store) error {
				_, err := a.addPreparedNodeIndexed(candidate, prepared, parsed.Name, "default", nil)
				return err
			})
		})
	case "import":
		parsed, err := parseNodeImportCommandArgs(args[1:])
		if err != nil {
			return err
		}
		sub, err := resolveNodeSecretArg(parsed.Value, parsed.FromStdin, "订阅链接: ", "订阅链接")
		if err != nil {
			return err
		}
		return a.importSubscriptionWithLock(sub)
	case "test":
		return a.speedTestWithLock()
	case "auto":
		return a.autoSelectWithLock(firstNonEmpty(arg(args, 1), "default"))
	}
	return a.withStoreLock(func() error {
		st, err := a.loadStore()
		if err != nil {
			return err
		}
		switch args[0] {
		case "remove", "delete":
			return a.removeNode(st, arg(args, 1))
		case "rename":
			return a.renameNode(st, arg(args, 1), arg(args, 2))
		case "use":
			id := arg(args, 1)
			scope := firstNonEmpty(arg(args, 2), "default")
			return a.commitNodeStoreMutation(st, func(candidate *Store) error {
				return a.useNodeInStore(candidate, id, scope)
			})
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

type nodeAddCommandArgs struct {
	Raw       string
	Name      string
	FromStdin bool
}

type nodeSecretCommandArg struct {
	Value     string
	FromStdin bool
}

func parseNodeAddCommandArgs(args []string) (nodeAddCommandArgs, error) {
	if len(args) == 0 {
		return nodeAddCommandArgs{}, fmt.Errorf("节点链接必须通过标准输入提供：node add --stdin [备注]")
	}
	if args[0] != "--stdin" {
		return nodeAddCommandArgs{}, fmt.Errorf("node add 不接受节点链接位置参数（避免凭据出现在进程 argv）；请使用 node add --stdin [备注]")
	}
	if len(args) > 2 {
		return nodeAddCommandArgs{}, fmt.Errorf("node add --stdin 最多接受一个备注参数")
	}
	return nodeAddCommandArgs{Name: arg(args, 1), FromStdin: true}, nil
}

func parseNodeImportCommandArgs(args []string) (nodeSecretCommandArg, error) {
	if len(args) == 0 {
		return nodeSecretCommandArg{}, fmt.Errorf("订阅链接必须通过标准输入提供：node import --stdin")
	}
	if args[0] != "--stdin" {
		return nodeSecretCommandArg{}, fmt.Errorf("node import 不接受订阅链接位置参数（避免凭据出现在进程 argv）；请使用 node import --stdin")
	}
	if len(args) != 1 {
		return nodeSecretCommandArg{}, fmt.Errorf("node import --stdin 不接受其他参数")
	}
	return nodeSecretCommandArg{FromStdin: true}, nil
}

func resolveNodeSecretArg(value string, fromStdin bool, prompt, label string) (string, error) {
	if !fromStdin {
		return value, nil
	}
	value, ok := ask(prompt)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("未从标准输入读取到%s", label)
	}
	return value, nil
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
		choice, ok := ask("请输入选项 [1-12]: ")
		if !ok {
			fmt.Println()
			return nil
		}
		switch choice {
		case "1":
			_ = a.listNodes(st)
		case "2":
			raw, _ := ask("节点链接: ")
			name, _ := ask("备注名: ")
			prepared, err := prepareNode(raw)
			if err != nil {
				fmt.Println(err)
				continue
			}
			if err := a.withLockedStoreRoot(func(st *Store) error {
				return a.commitNodeStoreMutation(st, func(candidate *Store) error {
					_, err := a.addPreparedNodeIndexed(candidate, prepared, name, "default", nil)
					return err
				})
			}); err != nil {
				fmt.Println(err)
			}
		case "3":
			sub, _ := ask("订阅链接: ")
			if err := a.importSubscriptionWithLock(sub); err != nil {
				fmt.Println(err)
			}
		case "4":
			if err := a.speedTestWithLock(); err != nil {
				fmt.Println(err)
			}
		case "5":
			if err := a.autoSelectWithLock("default"); err != nil {
				fmt.Println(err)
			}
		case "6":
			id, _ := ask("节点 ID: ")
			if err := a.useNodeWithLock(id, "default"); err != nil {
				fmt.Println(err)
			}
		case "7":
			id, _ := ask("节点 ID: ")
			if err := a.useNodeWithLock(id, string(SceneGlobal)); err != nil {
				fmt.Println(err)
			}
		case "8":
			id, _ := ask("节点 ID: ")
			if err := a.useNodeWithLock(id, string(SceneDev)); err != nil {
				fmt.Println(err)
			}
		case "9":
			id, _ := ask("节点 ID: ")
			if err := a.useNodeWithLock(id, string(SceneTelegram)); err != nil {
				fmt.Println(err)
			}
		case "10":
			id, _ := ask("节点 ID: ")
			name, _ := ask("新备注: ")
			if err := a.withLockedStoreRoot(func(st *Store) error { return a.renameNode(st, id, name) }); err != nil {
				fmt.Println(err)
			}
		case "11":
			id, _ := ask("节点 ID: ")
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
	lastEnabledNode := len(st.Nodes) == 1 && hasEnabledScene(st)
	mode := storeRuntimeSyncXray
	if lastEnabledNode {
		mode = storeRuntimeSyncAll
	}
	return a.commitStoreMutation(st, func(candidate *Store) error {
		if id == "" {
			return fmt.Errorf("节点 ID 不能为空")
		}
		if candidate.findNode(id) == nil {
			return fmt.Errorf("节点不存在：%s", id)
		}
		out := make([]Node, 0, len(candidate.Nodes)-1)
		for _, n := range candidate.Nodes {
			if n.ID != id {
				out = append(out, n)
			}
		}
		candidate.Nodes = out
		if candidate.DefaultNodeID == id {
			candidate.DefaultNodeID = candidate.firstNodeID()
		}
		for sc, nid := range candidate.SceneNodes {
			if nid == id {
				delete(candidate.SceneNodes, sc)
			}
		}
		delete(candidate.SpeedResults, id)
		if len(candidate.Nodes) == 0 && hasEnabledScene(candidate) {
			candidate.SceneEnabled[SceneGlobal] = false
			candidate.SceneEnabled[SceneDev] = false
			candidate.SceneEnabled[SceneTelegram] = false
		}
		return nil
	}, mode)
}

func (a *App) renameNode(st *Store, id, name string) error {
	return a.commitNodeStoreMutation(st, func(candidate *Store) error {
		n := candidate.findNode(id)
		if n == nil {
			return fmt.Errorf("节点不存在：%s", id)
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return fmt.Errorf("节点备注不能为空")
		}
		name = sanitizeNodeName(name)
		if len(name) > maxNodeNameBytes {
			return fmt.Errorf("节点备注过长，最多 %d 字节", maxNodeNameBytes)
		}
		n.Name = name
		n.UpdatedAt = time.Now()
		return nil
	})
}

// normalizeScope 把帮助文本中的中文范围别名归一化为内部 token，使
// `node use <id> 全局` 这类照着帮助输入的用法也能工作。
func normalizeScope(scope string) string {
	switch strings.TrimSpace(scope) {
	case "默认":
		return "default"
	case "全局":
		return "global"
	case "开发":
		return "dev"
	case "电报", "tg":
		return "telegram"
	case "全部":
		return "all"
	}
	return strings.TrimSpace(scope)
}

func (a *App) useNodeInStore(st *Store, id, scope string) error {
	if id == "" {
		return fmt.Errorf("节点 ID 不能为空")
	}
	if st.findNode(id) == nil {
		return fmt.Errorf("节点不存在：%s", id)
	}
	switch scope := normalizeScope(scope); scope {
	case "", "default":
		st.DefaultNodeID = id
	case "global", "dev", "telegram":
		st.SceneNodes[Scene(scope)] = id
	case "all":
		st.DefaultNodeID = id
		st.SceneNodes[SceneGlobal] = id
		st.SceneNodes[SceneDev] = id
		st.SceneNodes[SceneTelegram] = id
	default:
		return fmt.Errorf("未知节点使用范围：%s，可用范围：default(默认)/global(全局)/dev(开发)/telegram(电报)/all(全部)", scope)
	}
	return nil
}

type preparedSubscription struct {
	URL     string
	Nodes   []preparedNode
	Invalid int
}

func (a *App) importSubscriptionWithLock(sub string) error {
	if err := requireRoot(); err != nil {
		return err
	}
	prepared, err := a.downloadAndPrepareSubscription(sub)
	if err != nil {
		return err
	}
	return a.withStoreLock(func() error {
		st, err := a.loadStore()
		if err != nil {
			return err
		}
		return a.mergePreparedSubscription(st, prepared)
	})
}

func (a *App) downloadAndPrepareSubscription(sub string) (preparedSubscription, error) {
	allowHTTP := envBool("PROXYSCENE_ALLOW_HTTP_SUBSCRIPTION", false)
	allowPrivate := envBool("PROXYSCENE_ALLOW_PRIVATE_SUBSCRIPTION", false)
	client := subscriptionHTTPClient(allowHTTP, allowPrivate)
	defer client.CloseIdleConnections()
	return downloadAndPrepareSubscriptionWithClient(sub, allowHTTP, client)
}

func downloadAndPrepareSubscriptionWithClient(sub string, allowHTTP bool, client *http.Client) (preparedSubscription, error) {
	sub = strings.TrimSpace(sub)
	if sub == "" {
		return preparedSubscription{}, fmt.Errorf("订阅链接不能为空")
	}
	if len(sub) > maxSubscriptionURLBytes {
		return preparedSubscription{}, fmt.Errorf("订阅链接过长，最多 %d 字节", maxSubscriptionURLBytes)
	}
	subURL, err := url.Parse(sub)
	if err != nil || subURL.Host == "" || subURL.Hostname() == "" {
		return preparedSubscription{}, fmt.Errorf("订阅链接必须是有效的 https 地址")
	}
	subURL.Scheme = strings.ToLower(subURL.Scheme)
	switch subURL.Scheme {
	case "https":
	case "http":
		if !allowHTTP {
			return preparedSubscription{}, fmt.Errorf("订阅链接必须使用 https；如确需导入明文 HTTP 订阅，请设置 PROXYSCENE_ALLOW_HTTP_SUBSCRIPTION=1")
		}
		fmt.Println("警告：正在导入明文 HTTP 订阅，内容可能被中间人篡改")
	default:
		return preparedSubscription{}, fmt.Errorf("订阅链接必须是 https 地址")
	}
	canonicalURL := subURL.String()
	resp, err := client.Get(canonicalURL)
	if err != nil {
		// net/http errors normally include the complete URL. Subscription URLs
		// commonly carry bearer tokens, so never wrap or return the transport error.
		return preparedSubscription{}, fmt.Errorf("订阅下载失败（目标主机 %s）", safeSubscriptionHost(subURL))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return preparedSubscription{}, fmt.Errorf("订阅下载失败：HTTP 状态码 %d", resp.StatusCode)
	}
	if resp.ContentLength > maxSubscriptionBytes {
		return preparedSubscription{}, fmt.Errorf("订阅内容过大，超过 %d 字节", maxSubscriptionBytes)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxSubscriptionBytes+1))
	if err != nil {
		return preparedSubscription{}, fmt.Errorf("读取订阅内容失败")
	}
	if int64(len(b)) > maxSubscriptionBytes {
		return preparedSubscription{}, fmt.Errorf("订阅内容过大，超过 %d 字节", maxSubscriptionBytes)
	}
	return prepareSubscriptionBody(canonicalURL, b)
}

func safeSubscriptionHost(u *url.URL) string {
	host := sanitizeNodeName(u.Hostname())
	if host == "node" {
		return "未知"
	}
	return host
}

func prepareSubscriptionBody(sub string, body []byte) (preparedSubscription, error) {
	deadline := time.Now().Add(maxSubscriptionProcessingTime)
	urls, tooMany := extractNodeURLsLimited(string(body), maxSubscriptionNodes)
	if tooMany {
		return preparedSubscription{}, fmt.Errorf("订阅节点数超过上限 %d", maxSubscriptionNodes)
	}
	if len(urls) == 0 {
		if time.Now().After(deadline) {
			return preparedSubscription{}, fmt.Errorf("订阅处理超时")
		}
		if decoded, err := decodeBase64URL(strings.TrimSpace(string(body))); err == nil {
			urls, tooMany = extractNodeURLsLimited(string(decoded), maxSubscriptionNodes)
			if tooMany {
				return preparedSubscription{}, fmt.Errorf("订阅节点数超过上限 %d", maxSubscriptionNodes)
			}
		}
	}
	prepared := preparedSubscription{URL: sub, Nodes: make([]preparedNode, 0, len(urls))}
	for _, raw := range urls {
		if time.Now().After(deadline) {
			return preparedSubscription{}, fmt.Errorf("订阅处理超时")
		}
		node, err := prepareNode(raw)
		if err != nil {
			prepared.Invalid++
			continue
		}
		prepared.Nodes = append(prepared.Nodes, node)
	}
	if len(prepared.Nodes) == 0 {
		return preparedSubscription{}, fmt.Errorf("订阅中没有可导入节点")
	}
	return prepared, nil
}

func (a *App) mergePreparedSubscription(st *Store, prepared preparedSubscription) error {
	urlIndex := make(map[string]int, len(st.Nodes)+len(prepared.Nodes))
	for i := range st.Nodes {
		if _, exists := urlIndex[st.Nodes[i].RawURL]; !exists {
			urlIndex[st.Nodes[i].RawURL] = i
		}
	}
	added, existing, failed := 0, 0, prepared.Invalid
	err := a.commitNodeStoreMutation(st, func(candidate *Store) error {
		for _, node := range prepared.Nodes {
			before := len(candidate.Nodes)
			if _, err := a.addPreparedNodeIndexed(candidate, node, "", "", urlIndex); err != nil {
				failed++
				continue
			}
			if len(candidate.Nodes) > before {
				added++
			} else {
				existing++
			}
		}
		if added == 0 && existing == 0 {
			return fmt.Errorf("订阅中没有可导入节点")
		}
		if !containsString(candidate.Subscriptions, prepared.URL) {
			if len(candidate.Subscriptions) >= maxSubscriptions {
				return fmt.Errorf("订阅记录数已达到上限 %d", maxSubscriptions)
			}
			candidate.Subscriptions = append(candidate.Subscriptions, prepared.URL)
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Printf("订阅导入完成：新增 %d 个，已存在/重复 %d 个，跳过无效 %d 个\n", added, existing, failed)
	return nil
}

// extractNodeURLs 从订阅文本中提取节点链接。协议 scheme 前要求一个边界（行首或
// 空白/引号/括号等），避免把 "xvless://..." 这类词中出现的 scheme 误当成链接。
// 注意：URL 字符集仍保留逗号，因为部分链接的查询参数（如 ws host 列表）合法含逗号；
// 订阅标准是按行分隔，逗号拼接属非标准用法。
var nodeURLPattern = regexp.MustCompile(`(?i)(?:^|[\s'"<>(){}])((?:vless|vmess|trojan|ss|shadowsocks)://[^\s<>'"]+)`)

func extractNodeURLsLimited(s string, limit int) ([]string, bool) {
	matchLimit := limit
	if limit >= 0 {
		matchLimit++
	}
	matches := nodeURLPattern.FindAllStringSubmatch(s, matchLimit)
	tooMany := limit >= 0 && len(matches) > limit
	if tooMany {
		matches = matches[:limit]
	}
	urls := make([]string, 0, len(matches))
	for _, m := range matches {
		raw := strings.TrimRight(m[1], ".,;，；。)]}>")
		if raw != "" {
			urls = append(urls, raw)
		}
	}
	return urls, tooMany
}

// subscriptionHTTPClient 构造抓取订阅用的 HTTP 客户端，带两层 SSRF 防护：
//  1. CheckRedirect 在每一跳重新校验协议，禁止 https 被重定向降级到非允许协议；
//  2. Dialer.Control 在 DNS 解析后、连接前校验目标 IP，默认拒绝环回/私网/链路本地/
//     CGNAT 等非公网地址（含云元数据 169.254.169.254），可防 DNS rebinding。
//     如确需抓取部署在内网的订阅，设置 PROXYSCENE_ALLOW_PRIVATE_SUBSCRIPTION=1。
func subscriptionHTTPClient(allowHTTP, allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if !allowPrivate {
		dialer.Control = func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("无法解析订阅目标地址：%s", address)
			}
			if !isPublicIP(ip) {
				return fmt.Errorf("订阅目标指向非公网地址，已拒绝（如确需可设 PROXYSCENE_ALLOW_PRIVATE_SUBSCRIPTION=1）：%s", host)
			}
			return nil
		}
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{DialContext: dialer.DialContext},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Subscription paths and queries often contain credentials. Never send
			// their source URL as Referer, even for a same-origin redirect.
			req.Header.Del("Referer")
			if len(via) == 0 || !sameSubscriptionOrigin(req.URL, via[0].URL) {
				req.Header.Del("Authorization")
				req.Header.Del("Proxy-Authorization")
				req.Header.Del("Cookie")
			}
			if len(via) >= 10 {
				return fmt.Errorf("订阅重定向次数过多")
			}
			scheme := strings.ToLower(req.URL.Scheme)
			if scheme == "https" || (allowHTTP && scheme == "http") {
				return nil
			}
			return fmt.Errorf("订阅重定向到不允许的协议：%s", scheme)
		},
	}
}

func sameSubscriptionOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectiveURLPort(a) == effectiveURLPort(b)
}

func effectiveURLPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

// isPublicIP 报告 ip 是否为可路由的公网地址。
func isPublicIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range nonPublicSubscriptionPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

var nonPublicSubscriptionPrefixes = []netip.Prefix{
	// IPv4 special-use, private, documentation, benchmarking and reserved space.
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.31.196.0/24"),
	netip.MustParsePrefix("192.52.193.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("192.175.48.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	// IPv6 local, transition, special protocol, documentation and reserved space.
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("fec0::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func (a *App) runSpeedTests(nodes []Node) []SpeedResult {
	return runSpeedTestsWith(nodes, a.testNode)
}

func runSpeedTestsWith(nodes []Node, test func(Node) error) []SpeedResult {
	// 并发测速，限制并发上限，避免 N 个节点串行各等 5s 超时导致整体阻塞 N*5s。
	results := make([]SpeedResult, len(nodes))
	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup
	for i := range nodes {
		// 在循环体内先占用信号量再起 goroutine：否则会先为每个节点各起一个 goroutine
		// 再在内部阻塞等待信号量，使存活 goroutine 数随（可由订阅影响的）节点数线性增长，
		// 失去并发上限的意义。这样可把同时存活的 goroutine 真正限制在信号量容量内。
		sem <- struct{}{}
		wg.Add(1)
		go func(i int, n Node) {
			defer wg.Done()
			defer func() { <-sem }()
			start := time.Now()
			err := test(n)
			r := SpeedResult{NodeID: n.ID, Target: "节点地址 TCP 连通性", LatencyMS: time.Since(start).Milliseconds(), Success: err == nil, TestedAt: time.Now()}
			if err != nil {
				r.Error = err.Error()
			}
			results[i] = r
		}(i, nodes[i])
	}
	wg.Wait()
	return results
}

func printSpeedResults(nodes []Node, results []SpeedResult) {
	for i, node := range nodes {
		if i >= len(results) {
			break
		}
		if results[i].Success {
			fmt.Printf("%s %dms\n", node.Name, results[i].LatencyMS)
		} else {
			fmt.Printf("%s 失败：%s\n", node.Name, results[i].Error)
		}
	}
}

// mergeSpeedResults only accepts a result when both the node ID and RawURL
// still match the lock-free snapshot. Concurrent replacements cannot inherit a
// stale result merely by reusing an ID.
func mergeSpeedResults(st *Store, snapshot []Node, results []SpeedResult) map[string]SpeedResult {
	normalizeStore(st)
	snapshotURLs := make(map[string]string, len(snapshot))
	resultByID := make(map[string]SpeedResult, len(results))
	for i, node := range snapshot {
		snapshotURLs[node.ID] = node.RawURL
		if i < len(results) && results[i].NodeID == node.ID {
			resultByID[node.ID] = results[i]
		}
	}
	current := map[string]bool{}
	fresh := make(map[string]SpeedResult, len(results))
	for _, n := range st.Nodes {
		current[n.ID] = true
		if raw, ok := snapshotURLs[n.ID]; ok {
			if raw != n.RawURL {
				delete(st.SpeedResults, n.ID)
				continue
			}
			if result, ok := resultByID[n.ID]; ok {
				st.SpeedResults[n.ID] = result
				fresh[n.ID] = result
			}
		}
	}
	for id := range st.SpeedResults {
		if !current[id] {
			delete(st.SpeedResults, id)
		}
	}
	return fresh
}

func (a *App) loadNodeSnapshotWithLock() ([]Node, error) {
	var nodes []Node
	err := a.withStoreLock(func() error {
		st, err := a.loadStore()
		if err != nil {
			return err
		}
		if len(st.Nodes) == 0 {
			return fmt.Errorf("没有可测速节点")
		}
		nodes = append([]Node(nil), st.Nodes...)
		return nil
	})
	return nodes, err

}

func (a *App) speedTestWithLock() error {
	if err := requireRoot(); err != nil {
		return err
	}
	nodes, err := a.loadNodeSnapshotWithLock()
	if err != nil {
		return err
	}
	results := a.runSpeedTests(nodes)
	printSpeedResults(nodes, results)
	return a.withStoreLock(func() error {
		st, err := a.loadStore()
		if err != nil {
			return err
		}
		return a.commitStoreMutation(st, func(candidate *Store) error {
			mergeSpeedResults(candidate, nodes, results)
			return nil
		}, storeRuntimeSyncNone)
	})
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
		return a.commitNodeStoreMutation(st, func(candidate *Store) error {
			return a.useNodeInStore(candidate, id, scope)
		})
	})
}

func fastestNodeID(results map[string]SpeedResult) string {
	fastest := ""
	var latency int64
	for id, result := range results {
		if !result.Success {
			continue
		}
		if fastest == "" || result.LatencyMS < latency || (result.LatencyMS == latency && id < fastest) {
			fastest = id
			latency = result.LatencyMS
		}
	}
	return fastest
}

func (a *App) autoSelectWithLock(scope string) error {
	if err := requireRoot(); err != nil {
		return err
	}
	nodes, err := a.loadNodeSnapshotWithLock()
	if err != nil {
		return err
	}
	results := a.runSpeedTests(nodes)
	printSpeedResults(nodes, results)
	return a.withStoreLock(func() error {
		st, err := a.loadStore()
		if err != nil {
			return err
		}
		return a.commitNodeStoreMutation(st, func(candidate *Store) error {
			fresh := mergeSpeedResults(candidate, nodes, results)
			id := fastestNodeID(fresh)
			if id == "" {
				return fmt.Errorf("没有可用节点")
			}
			return a.useNodeInStore(candidate, id, scope)
		})
	})
}
