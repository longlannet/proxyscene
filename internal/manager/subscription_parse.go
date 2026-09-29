package manager

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

const maxSubscriptionDiagnostics = 5

// All strings in these diagnostics come from fixed local labels. Node URLs,
// parser errors, and untrusted query keys must never reach terminal output.
type subscriptionDiagnostic struct {
	Index    int
	Protocol string
	Category string
}

type preparedSubscription struct {
	URL         string
	Format      string
	Nodes       []preparedNode
	Invalid     int
	Incomplete  bool // Loose first imports are allowed; destructive refresh requires a complete node list.
	Unsupported map[string]int
	Diagnostics []subscriptionDiagnostic
}

func (prepared preparedSubscription) diagnosticSummary() string {
	var summary strings.Builder
	fmt.Fprintf(&summary, "识别 %d 条，接受 %d 条，失败 %d 条", len(prepared.Nodes)+prepared.Invalid, len(prepared.Nodes), prepared.Invalid)
	if len(prepared.Unsupported) > 0 {
		protocols := make([]string, 0, len(prepared.Unsupported))
		for protocol := range prepared.Unsupported {
			protocols = append(protocols, protocol)
		}
		slices.Sort(protocols)
		summary.WriteString("；不支持的协议：")
		for i, protocol := range protocols {
			if i > 0 {
				summary.WriteString("、")
			}
			fmt.Fprintf(&summary, "%s %d 条", protocol, prepared.Unsupported[protocol])
		}
	}
	for _, issue := range prepared.Diagnostics {
		fmt.Fprintf(&summary, "；第 %d 条（%s）：%s", issue.Index, issue.Protocol, issue.Category)
	}
	if prepared.Invalid > len(prepared.Diagnostics) {
		fmt.Fprintf(&summary, "；其余 %d 条失败详情已省略", prepared.Invalid-len(prepared.Diagnostics))
	}
	if prepared.Incomplete {
		summary.WriteString("；内容含无法完整识别的条目或非节点文本")
	}
	if prepared.Format == "Clash YAML" {
		summary.WriteString("；仅导入 YAML proxies 节点，其他配置已忽略")
	}
	return summary.String()
}

func (prepared *preparedSubscription) recordDiagnostic(index int, protocol, category string) {
	prepared.Invalid++
	if len(prepared.Diagnostics) < maxSubscriptionDiagnostics {
		prepared.Diagnostics = append(prepared.Diagnostics, subscriptionDiagnostic{Index: index, Protocol: protocol, Category: category})
	}
}

// The broad categories intentionally omit the original error. Even a future
// parser error that embeds a password or arbitrary field name remains private.
func subscriptionErrorCategory(err error) string {
	message := err.Error()
	switch {
	case strings.HasPrefix(message, "节点不兼容"):
		return "当前核心不支持该配置"
	case strings.Contains(message, "冲突"), strings.Contains(message, "重复"):
		return "参数冲突或重复"
	case strings.Contains(message, "不受支持的查询参数"), strings.Contains(message, "无法表达参数"):
		return "参数不受支持"
	case strings.Contains(message, "transport"), strings.Contains(message, "传输"), strings.Contains(message, "XHTTP"), strings.Contains(message, "gRPC"), strings.Contains(message, "RAW"), strings.Contains(message, "early data"), strings.Contains(message, "混淆"):
		return "传输配置无效或不受支持"
	case strings.Contains(message, "TLS"), strings.Contains(message, "REALITY"), strings.Contains(message, "security"), strings.Contains(message, "证书"):
		return "连接安全配置无效或不受支持"
	case strings.Contains(message, "encryption"), strings.Contains(message, "cipher"), strings.Contains(message, "method"), strings.Contains(message, "加密"):
		return "加密配置无效或不受支持"
	case strings.Contains(message, "用户 ID"), strings.Contains(message, "UUID"), strings.Contains(message, "密码"), strings.Contains(message, "认证"):
		return "身份凭据格式无效或缺失"
	case strings.Contains(message, "主机"), strings.Contains(message, "服务器地址"), strings.Contains(message, "端口"), strings.Contains(message, "host/port"):
		return "服务器地址或端口无效"
	case strings.Contains(message, "过长"), strings.Contains(message, "超过"):
		return "字段长度超过上限"
	default:
		return "链接格式或参数无效"
	}
}

func subscriptionProtocolLabel(raw string) (label string, supported bool) {
	scheme, _, _ := strings.Cut(raw, "://")
	switch strings.ToLower(scheme) {
	case "vless":
		return "VLESS", true
	case "vmess":
		return "VMess", true
	case "trojan":
		return "Trojan", true
	case "ss", "shadowsocks":
		return "Shadowsocks", true
	case "hysteria2", "hy2":
		return "Hysteria2", true
	case "hysteria", "hy":
		return "Hysteria", false
	case "tuic":
		return "TUIC", false
	case "snell":
		return "Snell", false
	case "wireguard", "wg":
		return "WireGuard", false
	case "socks", "socks4", "socks5":
		return "SOCKS", false
	case "http", "https":
		return "HTTP(S)", false
	default:
		return "其他", false
	}
}

func prepareSubscriptionBody(sub string, body []byte) (preparedSubscription, error) {
	if int64(len(body)) > maxSubscriptionBytes {
		return preparedSubscription{}, fmt.Errorf("订阅内容过大，超过 %d 字节", maxSubscriptionBytes)
	}
	deadline := time.Now().Add(maxSubscriptionProcessingTime)
	text := string(body)
	if looksLikeClashYAML(text) {
		return prepareClashSubscription(sub, text, deadline)
	}
	urls, tooMany := extractSubscriptionURLsLimited(text, maxSubscriptionNodes)
	if tooMany {
		return preparedSubscription{}, fmt.Errorf("订阅节点数超过上限 %d", maxSubscriptionNodes)
	}
	if len(urls) == 0 {
		if time.Now().After(deadline) {
			return preparedSubscription{}, fmt.Errorf("订阅处理超时")
		}
		if decoded, err := decodeBase64URL(strings.TrimSpace(string(body))); err == nil {
			text = string(decoded)
			if looksLikeClashYAML(text) {
				return prepareClashSubscription(sub, text, deadline)
			}
			urls, tooMany = extractSubscriptionURLsLimited(text, maxSubscriptionNodes)
			if tooMany {
				return preparedSubscription{}, fmt.Errorf("订阅节点数超过上限 %d", maxSubscriptionNodes)
			}
		}
	}
	prepared := preparedSubscription{URL: sub, Nodes: make([]preparedNode, 0, len(urls)), Incomplete: !slices.Equal(strings.Fields(text), urls)}
	for index, raw := range urls {
		if time.Now().After(deadline) {
			return preparedSubscription{}, fmt.Errorf("订阅处理超时")
		}
		protocol, supported := subscriptionProtocolLabel(raw)
		if !supported {
			if prepared.Unsupported == nil {
				prepared.Unsupported = make(map[string]int)
			}
			prepared.Unsupported[protocol]++
			prepared.Incomplete = true
			prepared.recordDiagnostic(index+1, protocol, "协议不受支持")
			continue
		}
		node, err := prepareNode(raw)
		if err != nil {
			prepared.recordDiagnostic(index+1, protocol, subscriptionErrorCategory(err))
			continue
		}
		prepared.Nodes = append(prepared.Nodes, node)
	}
	if len(prepared.Nodes) == 0 {
		return prepared, fmt.Errorf("订阅中没有可导入节点（%s）", prepared.diagnosticSummary())
	}
	return prepared, nil
}

// extractNodeURLs 从订阅文本中提取节点链接。协议 scheme 前要求一个边界（行首或
// 空白/引号/括号等），避免把 "xvless://..." 这类词中出现的 scheme 误当成链接。
// 注意：URL 字符集仍保留逗号，因为部分链接的查询参数（如 ws host 列表）合法含逗号；
// 订阅标准是按行分隔，逗号拼接属非标准用法。
var nodeURLPattern = regexp.MustCompile(`(?i)(?:^|[\s'"<>(){}])((?:vless|vmess|trojan|ss|shadowsocks|hysteria2|hy2)://[^\s<>'"]+)`)

// Unknown schemes are counted without printing their arbitrary names. This
// also keeps a large unsupported feed under the same work and node limits.
var subscriptionURLPattern = regexp.MustCompile(`(?i)(?:^|[\s'"<>(){}])([a-z][a-z0-9+.-]*://[^\s<>'"]+)`)

func extractSubscriptionURLsLimited(s string, limit int) ([]string, bool) {
	return extractURLsMatchingLimited(subscriptionURLPattern, s, limit)
}

func extractNodeURLsLimited(s string, limit int) ([]string, bool) {
	return extractURLsMatchingLimited(nodeURLPattern, s, limit)
}

func extractURLsMatchingLimited(pattern *regexp.Regexp, s string, limit int) ([]string, bool) {
	matchLimit := limit
	if limit >= 0 {
		matchLimit++
	}
	matches := pattern.FindAllStringSubmatch(s, matchLimit)
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
