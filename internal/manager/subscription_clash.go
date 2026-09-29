package manager

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// YAML is identified before scanning URI links: a URL inside a provider, rule,
// or script is never a subscription node. JSON is a YAML subset.
var clashYAMLStart = regexp.MustCompile(`(?m)^[ \t]*(?:[\[{}\]!&%>|]|---(?:[ \t]|$)|[?:-](?:[ \t]|$)|[^\s#][^\r\n:]*:[ \t]*(?:$|[ \t]))`)

func looksLikeClashYAML(text string) bool {
	return clashYAMLStart.MatchString(strings.TrimPrefix(text, "\ufeff"))
}

type subscriptionYAMLReader struct {
	reader   *strings.Reader
	deadline time.Time
}

func (r subscriptionYAMLReader) Read(p []byte) (int, error) {
	if time.Now().After(r.deadline) {
		return 0, fmt.Errorf("订阅处理超时")
	}
	if len(p) > 4096 {
		p = p[:4096]
	}
	return r.reader.Read(p)
}

func prepareClashSubscription(sub, text string, deadline time.Time) (preparedSubscription, error) {
	prepared := preparedSubscription{URL: sub, Format: "Clash YAML"}
	// Bound source complexity before yaml.v3 allocates its syntax tree. This
	// conservative count includes punctuation in strings, so even ignored
	// sections cannot turn a small body into millions of allocated nodes.
	if strings.Count(text, "\n")+strings.Count(text, ":")+strings.Count(text, ",")+strings.Count(text, "[")+strings.Count(text, "{") > 65536 {
		return prepared, fmt.Errorf("订阅 Clash YAML 结构超过上限")
	}
	decoder := yaml.NewDecoder(subscriptionYAMLReader{strings.NewReader(text), deadline})
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return prepared, fmt.Errorf("订阅 Clash YAML 内容无效或不完整")
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		return prepared, fmt.Errorf("订阅 Clash YAML 仅支持一个完整文档")
	}
	remaining := 131072
	if err := validateSubscriptionYAML(&document, 0, &remaining, deadline); err != nil {
		return prepared, err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return prepared, fmt.Errorf("订阅 Clash YAML 必须是包含 proxies 的对象")
	}
	root := document.Content[0]
	var proxies *yaml.Node
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == "proxies" {
			proxies = root.Content[i+1]
		}
	}
	if proxies == nil || proxies.Kind != yaml.SequenceNode {
		return prepared, fmt.Errorf("订阅 Clash YAML 缺少完整的 proxies 节点列表")
	}
	if len(proxies.Content) > maxSubscriptionNodes {
		return prepared, fmt.Errorf("订阅节点数超过上限 %d", maxSubscriptionNodes)
	}
	for index, entry := range proxies.Content {
		if time.Now().After(deadline) {
			return preparedSubscription{}, fmt.Errorf("订阅处理超时")
		}
		fields, err := newClashFields(entry)
		if err != nil {
			prepared.recordDiagnostic(index+1, "其他", "节点对象格式无效")
			continue
		}
		protocol := fields.text("type", true)
		label, supported := subscriptionProtocolLabel(protocol + "://")
		if !supported {
			if prepared.Unsupported == nil {
				prepared.Unsupported = make(map[string]int)
			}
			prepared.Unsupported[label]++
			prepared.recordDiagnostic(index+1, label, "协议不受支持")
			continue
		}
		raw, err := clashNodeURL(fields, strings.ToLower(protocol))
		if err != nil {
			prepared.recordDiagnostic(index+1, label, subscriptionErrorCategory(err))
			continue
		}
		node, err := prepareNode(raw)
		if err != nil {
			prepared.recordDiagnostic(index+1, label, subscriptionErrorCategory(err))
			continue
		}
		prepared.Nodes = append(prepared.Nodes, node)
	}
	if len(prepared.Nodes) == 0 {
		return prepared, fmt.Errorf("订阅中没有可导入节点（%s）", prepared.diagnosticSummary())
	}
	return prepared, nil
}

// Decode only into yaml.Node; never expand aliases or decode arbitrary types.
// The upstream parser also bounds syntactic nesting; this tighter traversal
// bounds all consumed structures, including the ignored top-level sections.
func validateSubscriptionYAML(n *yaml.Node, depth int, remaining *int, deadline time.Time) error {
	*remaining--
	if *remaining < 0 || depth > 64 {
		return fmt.Errorf("订阅 Clash YAML 结构超过上限")
	}
	if time.Now().After(deadline) {
		return fmt.Errorf("订阅处理超时")
	}
	if n.Kind == yaml.AliasNode {
		return fmt.Errorf("订阅 Clash YAML 不支持别名或合并引用")
	}
	switch n.Tag {
	case "", "!!map", "!!seq", "!!str", "!!int", "!!float", "!!bool", "!!null":
	default:
		return fmt.Errorf("订阅 Clash YAML 不支持自定义或特殊标签")
	}
	if n.Kind == yaml.MappingNode {
		seen := make(map[string]bool, len(n.Content)/2)
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Value == "<<" {
				return fmt.Errorf("订阅 Clash YAML 对象字段名无效")
			}
			if seen[key.Value] {
				return fmt.Errorf("订阅 Clash YAML 包含重复字段")
			}
			seen[key.Value] = true
		}
	}
	for _, child := range n.Content {
		if err := validateSubscriptionYAML(child, depth+1, remaining, deadline); err != nil {
			return err
		}
	}
	return nil
}

// Every node field must be consumed. Unsupported settings must never silently
// change transport, certificate checking, authentication, or UDP behavior.
type clashFields struct {
	values map[string]*yaml.Node
	err    error
}

func newClashFields(n *yaml.Node) (*clashFields, error) {
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("节点对象格式无效")
	}
	f := &clashFields{values: make(map[string]*yaml.Node, len(n.Content)/2)}
	for i := 0; i < len(n.Content); i += 2 {
		f.values[n.Content[i].Value] = n.Content[i+1]
	}
	return f, nil
}
func (f *clashFields) take(key string) *yaml.Node {
	n := f.values[key]
	delete(f.values, key)
	return n
}
func (f *clashFields) fail(message string) {
	if f.err == nil {
		f.err = fmt.Errorf("%s", message)
	}
}
func (f *clashFields) text(key string, required bool) string {
	n := f.take(key)
	if n == nil {
		if required {
			f.fail("节点参数缺失")
		}
		return ""
	}
	if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
		f.fail("节点参数类型无效")
		return ""
	}
	if required && n.Value == "" {
		f.fail("节点参数缺失")
	}
	return n.Value
}
func (f *clashFields) numberText(key string, required bool) string {
	n := f.take(key)
	if n == nil {
		if required {
			f.fail("端口或数值参数缺失")
		}
		return ""
	}
	if n.Value == "" {
		f.fail("数值参数不能为空")
		return ""
	}
	if n.Kind != yaml.ScalarNode || (n.Tag != "!!str" && n.Tag != "!!int") {
		f.fail("端口或数值参数无效")
		return ""
	}
	if n.Tag == "!!int" {
		var value int64
		if err := n.Decode(&value); err != nil {
			f.fail("整数参数无效")
			return ""
		}
		return strconv.FormatInt(value, 10)
	}
	return n.Value
}
func (f *clashFields) bandwidthText(key string) string {
	n := f.take(key)
	if n == nil {
		return ""
	}
	if n.Value == "" {
		f.fail("带宽参数不能为空")
		return ""
	}
	if n.Kind != yaml.ScalarNode || (n.Tag != "!!str" && n.Tag != "!!int" && n.Tag != "!!float") {
		f.fail("带宽参数类型无效")
		return ""
	}
	if n.Tag == "!!int" {
		var value int64
		if err := n.Decode(&value); err != nil {
			f.fail("带宽参数无效")
			return ""
		}
		return strconv.FormatInt(value, 10)
	}
	return n.Value
}
func (f *clashFields) boolean(key string, fallback bool) bool {
	n := f.take(key)
	if n == nil {
		return fallback
	}
	if n.Kind != yaml.ScalarNode || n.Tag != "!!bool" {
		f.fail("布尔参数类型无效")
		return fallback
	}
	value, err := strconv.ParseBool(n.Value)
	if err != nil {
		f.fail("布尔参数类型无效")
	}
	return value
}
func (f *clashFields) object(key string) *clashFields {
	n := f.take(key)
	if n == nil {
		return nil
	}
	child, err := newClashFields(n)
	if err != nil {
		f.fail("传输参数必须是对象")
		return nil
	}
	return child
}
func (f *clashFields) finish() error {
	if f.err != nil {
		return f.err
	}
	if len(f.values) != 0 {
		return fmt.Errorf("存在无法表达参数")
	}
	return nil
}
func (f *clashFields) finishChild(child *clashFields) {
	if child != nil {
		if err := child.finish(); err != nil {
			f.fail(err.Error())
		}
	}
}
func (f *clashFields) stringList(key string) string {
	n := f.take(key)
	if n == nil {
		return ""
	}
	if n.Kind != yaml.SequenceNode || len(n.Content) == 0 {
		f.fail("列表参数无效")
		return ""
	}
	values := make([]string, 0, len(n.Content))
	for _, value := range n.Content {
		if value.Kind != yaml.ScalarNode || value.Tag != "!!str" || value.Value == "" || strings.Contains(value.Value, ",") {
			f.fail("列表参数无效")
			return ""
		}
		values = append(values, value.Value)
	}
	return strings.Join(values, ",")
}

func clashNodeURL(f *clashFields, protocol string) (string, error) {
	if protocol == "shadowsocks" {
		protocol = "ss"
	}
	if protocol == "hy2" {
		protocol = "hysteria2"
	}
	name := f.text("name", true)
	host := f.text("server", true)
	port := f.numberText("port", true)
	if err := validateNodeHost(host); err != nil {
		return "", fmt.Errorf("服务器地址无效")
	}
	portNumber, err := parseNodePort(port, protocol)
	if err != nil {
		return "", err
	}
	if !f.boolean("udp", true) {
		f.fail("无法表达参数 udp=false")
	}
	for _, option := range []string{"tfo", "tcp-fast-open", "mptcp"} {
		if f.boolean(option, false) {
			f.fail("存在无法表达参数")
		}
	}
	q := make(url.Values)
	set := func(key, value string) {
		if value != "" {
			q.Set(key, value)
		}
	}
	var credential string
	switch protocol {
	case "vless", "vmess":
		credential = f.text("uuid", true)
	default:
		credential = f.text("password", true)
	}
	if protocol == "ss" {
		method := f.text("cipher", true)
		if err := f.finish(); err != nil {
			return "", err
		}
		encoded := base64.RawURLEncoding.EncodeToString([]byte(method + ":" + credential))
		u := url.URL{Scheme: "ss", Host: net.JoinHostPort(host, strconv.Itoa(portNumber)), User: url.User(encoded), Fragment: name}
		return u.String(), nil
	}
	if f.boolean("skip-cert-verify", false) {
		f.fail("TLS 不能跳过证书校验")
	}
	sni, serverName := f.text("sni", false), f.text("servername", false)
	if sni != "" && serverName != "" && sni != serverName {
		f.fail("TLS servername 参数冲突")
	}
	set("sni", firstNonEmpty(sni, serverName))
	set("alpn", f.stringList("alpn"))
	if protocol == "hysteria2" {
		set("obfs", f.text("obfs", false))
		set("obfs-password", f.text("obfs-password", false))
		set("up", f.bandwidthText("up"))
		set("down", f.bandwidthText("down"))
		set("mport", f.numberText("ports", false))
		set("hop-interval", f.numberText("hop-interval", false))
	} else {
		tlsEnabled := f.boolean("tls", protocol == "trojan")
		security := "none"
		if tlsEnabled {
			security = "tls"
		}
		reality := f.object("reality-opts")
		if reality != nil {
			if !tlsEnabled || protocol == "vmess" {
				f.fail("REALITY 必须配合 TLS 的 VLESS 或 Trojan")
			}
			security = "reality"
			set("pbk", reality.text("public-key", true))
			set("sid", reality.text("short-id", false))
			f.finishChild(reality)
		}
		q.Set("security", security)
		set("fp", f.text("client-fingerprint", false))
		network := strings.ToLower(firstNonEmpty(f.text("network", false), "tcp"))
		q.Set("type", network)
		if protocol == "vless" {
			set("flow", f.text("flow", false))
			set("encryption", f.text("encryption", false))
		}
		clashTransportOptions(f, network, q)
	}
	if protocol == "vmess" {
		aid := firstNonEmpty(f.numberText("alterId", false), "0")
		cipher := firstNonEmpty(f.text("cipher", false), "auto")
		if err := f.finish(); err != nil {
			return "", err
		}
		payload := map[string]any{"v": "2", "ps": name, "add": host, "port": portNumber, "id": credential, "aid": aid, "scy": cipher, "net": q.Get("type"), "tls": q.Get("security")}
		for _, key := range []string{"sni", "fp", "alpn", "host", "path", "serviceName", "mode", "authority", "extra", "ed"} {
			if value := q.Get(key); value != "" {
				payload[key] = value
			}
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return "", fmt.Errorf("节点参数无效")
		}
		return "vmess://" + base64.RawStdEncoding.EncodeToString(encoded), nil
	}
	if err := f.finish(); err != nil {
		return "", err
	}
	u := url.URL{Scheme: protocol, Host: net.JoinHostPort(host, strconv.Itoa(portNumber)), User: url.User(credential), RawQuery: q.Encode(), Fragment: name}
	return u.String(), nil
}

func clashTransportOptions(f *clashFields, network string, q url.Values) {
	set := func(key, value string) {
		if value != "" {
			q.Set(key, value)
		}
	}
	switch network {
	case "tcp", "raw":
	case "ws", "websocket", "httpupgrade":
		key := "ws-opts"
		if network == "httpupgrade" {
			key = "http-upgrade-opts"
		}
		opts := f.object(key)
		if opts == nil {
			return
		}
		if opts.boolean("v2ray-http-upgrade", false) {
			network = "httpupgrade"
			q.Set("type", network)
		}
		if opts.boolean("v2ray-http-upgrade-fast-open", false) {
			opts.fail("HTTPUpgrade fast-open 参数不受支持")
		}
		set("path", opts.text("path", false))
		set("host", opts.text("host", false))
		set("ed", opts.numberText("max-early-data", false))
		// WebSocket uses Sec-WebSocket-Protocol for early data. HTTPUpgrade
		// cannot preserve an explicit early-data header at all.
		header := opts.text("early-data-header-name", false)
		pathEarlyData := ""
		if parsedPath, err := url.Parse(q.Get("path")); err == nil {
			pathEarlyData = parsedPath.Query().Get("ed")
		}
		if header != "" && (network == "httpupgrade" || !strings.EqualFold(header, "Sec-WebSocket-Protocol") || firstNonEmpty(q.Get("ed"), pathEarlyData) == "") {
			opts.fail("early data header 不受支持")
		}
		// Mihomo's empty header means path-based early data, which this core
		// cannot reproduce. HTTPUpgrade's ed flag is not a byte-count limit.
		if q.Get("ed") != "" && (network == "httpupgrade" || (header == "" && pathEarlyData == "")) {
			opts.fail("early data 配置无法完整表达")
		}
		headers := opts.object("headers")
		if headers != nil {
			var host string
			seenHost := false
			for key := range headers.values {
				if !strings.EqualFold(key, "Host") {
					continue
				}
				value := headers.text(key, false)
				if seenHost {
					headers.fail("Host 参数重复")
				}
				seenHost = true
				host = value
			}
			if host != "" && q.Get("host") != "" && host != q.Get("host") {
				headers.fail("Host 参数冲突")
			}
			set("host", host)
			opts.finishChild(headers)
		}
		f.finishChild(opts)
	case "grpc":
		opts := f.object("grpc-opts")
		if opts == nil {
			return
		}
		set("serviceName", opts.text("grpc-service-name", false))
		set("authority", opts.text("grpc-authority", false))
		f.finishChild(opts)
	case "xhttp":
		opts := f.object("xhttp-opts")
		if opts == nil {
			return
		}
		for _, key := range []string{"host", "path", "mode"} {
			set(key, opts.text(key, false))
		}
		if extra := opts.take("extra"); extra != nil {
			if extra.Kind != yaml.MappingNode {
				opts.fail("XHTTP extra 必须是对象")
			} else {
				var value any
				if err := extra.Decode(&value); err != nil {
					opts.fail("XHTTP extra 参数无效")
				} else if encoded, err := json.Marshal(value); err != nil {
					opts.fail("XHTTP extra 参数无效")
				} else {
					set("extra", string(encoded))
				}
			}
		}
		f.finishChild(opts)
	default:
		f.fail("传输配置不受支持")
	}
}
