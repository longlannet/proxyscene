package manager

import (
	"net"
	"strings"
	"testing"
)

func TestNewNodeIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 10000; i++ {
		id := newNodeID()
		if seen[id] {
			t.Fatalf("newNodeID produced duplicate ID: %s", id)
		}
		seen[id] = true
	}
}

func outboundPassword(t *testing.T, pn *parsedNode) string {
	t.Helper()
	settings, ok := pn.Outbound["settings"].(map[string]any)
	if !ok {
		t.Fatalf("outbound 缺少 settings")
	}
	servers, ok := settings["servers"].([]any)
	if !ok || len(servers) == 0 {
		t.Fatalf("outbound 缺少 servers")
	}
	server, ok := servers[0].(map[string]any)
	if !ok {
		t.Fatalf("server 类型错误")
	}
	pw, _ := server["password"].(string)
	return pw
}

func TestParseTrojanColonPassword(t *testing.T) {
	pn, err := parseNode("trojan://pa:ss:word@example.com:443#node")
	if err != nil {
		t.Fatalf("parseNode trojan 失败：%v", err)
	}
	if got := outboundPassword(t, pn); got != "pa:ss:word" {
		t.Fatalf("trojan 密码 = %q, want %q", got, "pa:ss:word")
	}
}

func TestParseTrojanSimplePassword(t *testing.T) {
	pn, err := parseNode("trojan://secret@example.com:443")
	if err != nil {
		t.Fatalf("parseNode trojan 失败：%v", err)
	}
	if got := outboundPassword(t, pn); got != "secret" {
		t.Fatalf("trojan 密码 = %q, want %q", got, "secret")
	}
}

func TestParsersDoNotEmitSockoptMark(t *testing.T) {
	cases := []string{
		"vless://11111111-1111-1111-1111-111111111111@example.com:443?security=tls",
		"trojan://secret@example.com:443",
		"ss://aes-256-gcm:secret@example.com:8388",
	}
	for _, raw := range cases {
		pn, err := parseNode(raw)
		if err != nil {
			t.Fatalf("parseNode(%q) 失败：%v", raw, err)
		}
		if stream, ok := pn.Outbound["streamSettings"].(map[string]any); ok {
			if _, exists := stream["sockopt"]; exists {
				t.Fatalf("%q 不应再注入 sockopt mark", raw)
			}
		}
	}
}

func TestParseSSPlain(t *testing.T) {
	pn, err := parseNode("ss://aes-256-gcm:secret@example.com:8388#name")
	if err != nil {
		t.Fatalf("parseNode ss 失败：%v", err)
	}
	if pn.EndpointHost != "example.com" || pn.EndpointPort != 8388 {
		t.Fatalf("ss endpoint = %s:%d, want example.com:8388", pn.EndpointHost, pn.EndpointPort)
	}
	if got := outboundPassword(t, pn); got != "secret" {
		t.Fatalf("ss 密码 = %q, want %q", got, "secret")
	}
}

func TestParseNodeRejectsUnknownProtocol(t *testing.T) {
	if _, err := parseNode("ftp://example.com"); err == nil {
		t.Fatalf("parseNode 应拒绝未知协议")
	}
}

func TestParseVLESSRealityRequiresPbk(t *testing.T) {
	if _, err := parseNode("vless://11111111-1111-1111-1111-111111111111@h:443?security=reality&sni=x"); err == nil {
		t.Fatalf("reality 缺少 pbk 应被拒绝")
	}
	if _, err := parseNode("vless://11111111-1111-1111-1111-111111111111@h:443?security=reality&pbk=abc&sni=x"); err != nil {
		t.Fatalf("reality 带 pbk 应解析成功：%v", err)
	}
}

func TestSanitizeNodeName(t *testing.T) {
	if got := sanitizeNodeName("foo\x1b\x07bar\n"); got != "foobar" {
		t.Fatalf("sanitizeNodeName = %q, want %q", got, "foobar")
	}
	if got := sanitizeNodeName("\x1b\x07\x00"); got != "node" {
		t.Fatalf("全控制字符应回退为 node，得到 %q", got)
	}
	// 通过解析带控制字符的备注，确认节点名不含控制字符。
	pn, err := parseNode("trojan://secret@h:443#%1b%5b2K%07evil")
	if err != nil {
		t.Fatalf("parseNode: %v", err)
	}
	for _, r := range pn.Name {
		if r < 0x20 || r == 0x7f {
			t.Fatalf("节点名仍含控制字符：%q", pn.Name)
		}
	}
}

func TestExtractNodeURLsBoundary(t *testing.T) {
	if got := extractNodeURLs("xvless://a@h:1"); len(got) != 0 {
		t.Fatalf("词中出现的 scheme 不应匹配，得到 %v", got)
	}
	got := extractNodeURLs("vless://a@h:1\ntrojan://p@h:2")
	if len(got) != 2 {
		t.Fatalf("换行分隔的两个链接应都提取，得到 %v", got)
	}
}

func TestIsPublicIP(t *testing.T) {
	nonPublic := []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "172.16.0.1", "169.254.169.254", "0.0.0.0", "100.64.0.1", "::1", "fe80::1", "fc00::1"}
	for _, s := range nonPublic {
		if isPublicIP(net.ParseIP(s)) {
			t.Fatalf("%s 应判为非公网", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !isPublicIP(net.ParseIP(s)) {
			t.Fatalf("%s 应判为公网", s)
		}
	}
}

func TestParseSSSIP002WithPath(t *testing.T) {
	// SIP002：host:port 后带可选 `/`，再接 ?plugin / #name。这些过去会被误判为端口无效。
	cases := []string{
		"ss://aes-256-gcm:secret@example.com:8388/#name",
		"ss://aes-256-gcm:secret@example.com:8388/?plugin=v2ray-plugin",
		"ss://aes-256-gcm:secret@example.com:8388/?plugin=v2ray-plugin#name",
		// base64 userinfo（SIP002 标准形式）+ 路径
		"ss://YWVzLTI1Ni1nY206c2VjcmV0@example.com:8388/?plugin=obfs#name",
	}
	for _, raw := range cases {
		pn, err := parseNode(raw)
		if err != nil {
			t.Fatalf("parseNode(%q) 应成功，却失败：%v", raw, err)
		}
		if pn.EndpointHost != "example.com" || pn.EndpointPort != 8388 {
			t.Fatalf("%q 解析出 %s:%d，期望 example.com:8388", raw, pn.EndpointHost, pn.EndpointPort)
		}
		if got := outboundPassword(t, pn); got != "secret" {
			t.Fatalf("%q 密码=%q，期望 secret", raw, got)
		}
	}
}

func TestParseTrojanColonPasswordRoundTrip(t *testing.T) {
	pn, err := parseNode("trojan://a:b@h:443")
	if err != nil {
		t.Fatalf("parseNode: %v", err)
	}
	if !strings.Contains(pn.Name, "trojan") {
		t.Fatalf("默认名应含 trojan：%q", pn.Name)
	}
}
