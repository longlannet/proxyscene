package manager

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func vmessURL(t *testing.T, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal vmess: %v", err)
	}
	return "vmess://" + base64.StdEncoding.EncodeToString(b)
}

func vmessStream(t *testing.T, pn *parsedNode) map[string]any {
	t.Helper()
	s, ok := pn.Outbound["streamSettings"].(map[string]any)
	if !ok {
		t.Fatalf("vmess 缺少 streamSettings：%+v", pn.Outbound)
	}
	return s
}

func TestParseVMessTLSSNIFallsBackToHost(t *testing.T) {
	// add 是裸 IP、伪装域名在 host：SNI 应回退到 host 而非 IP，否则 TLS 握手被拒。
	raw := vmessURL(t, map[string]any{
		"v": "2", "ps": "n", "add": "1.2.3.4", "port": "443",
		"id":  "11111111-1111-1111-1111-111111111111",
		"net": "ws", "tls": "tls", "host": "cdn.example.com", "path": "/ws",
	})
	pn, err := parseNode(raw)
	if err != nil {
		t.Fatalf("parseNode vmess: %v", err)
	}
	tls, ok := vmessStream(t, pn)["tlsSettings"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 tlsSettings")
	}
	if got := tls["serverName"]; got != "cdn.example.com" {
		t.Fatalf("SNI=%v，期望回退到 host cdn.example.com", got)
	}
}

func TestParseVMessGRPCServiceNameFromPath(t *testing.T) {
	// VMess gRPC 的 serviceName 承载在 path 字段，应据此生成而非空串。
	raw := vmessURL(t, map[string]any{
		"v": "2", "add": "example.com", "port": float64(443),
		"id":  "11111111-1111-1111-1111-111111111111",
		"net": "grpc", "path": "my-grpc-svc",
	})
	pn, err := parseNode(raw)
	if err != nil {
		t.Fatalf("parseNode vmess grpc: %v", err)
	}
	grpc, ok := vmessStream(t, pn)["grpcSettings"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 grpcSettings")
	}
	if got := grpc["serviceName"]; got != "my-grpc-svc" {
		t.Fatalf("grpc serviceName=%v，期望 my-grpc-svc", got)
	}
}

func TestAddNodeSanitizesOperatorName(t *testing.T) {
	a := testApp(t)
	st := newStore()
	id, err := a.addNode(st, "trojan://secret@h:443", "ev\x1bil\x07name", "")
	if err != nil {
		t.Fatalf("addNode: %v", err)
	}
	n := st.findNode(id)
	if n == nil {
		t.Fatalf("节点未加入")
	}
	if n.Name != "evilname" {
		t.Fatalf("操作员名未清洗：%q，期望 evilname", n.Name)
	}
}

func TestRenameNodeSanitizesName(t *testing.T) {
	a := testApp(t)
	st := newStore()
	id, err := a.addNode(st, "trojan://secret@h:443", "", "")
	if err != nil {
		t.Fatalf("addNode: %v", err)
	}
	if err := a.saveStore(st); err != nil {
		t.Fatal(err)
	}
	oldRun := systemctlRun
	oldOutput := systemctlOutput
	t.Cleanup(func() {
		systemctlRun = oldRun
		systemctlOutput = oldOutput
	})
	systemctlRun = func(string, ...string) error {
		t.Fatal("renameNode must not invoke systemctl")
		return nil
	}
	systemctlOutput = func(string, ...string) (string, error) {
		t.Fatal("renameNode must not query systemctl")
		return "", nil
	}
	if err := a.renameNode(st, id, "ne\x1bw\x07"); err != nil {
		t.Fatalf("renameNode: %v", err)
	}
	for _, r := range st.findNode(id).Name {
		if r < 0x20 || r == 0x7f {
			t.Fatalf("rename 后名称仍含控制字符：%q", st.findNode(id).Name)
		}
	}
}

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
	publicKey := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if _, err := parseNode("vless://11111111-1111-1111-1111-111111111111@h:443?security=reality&pbk=" + publicKey + "&sni=x"); err != nil {
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
	if got, _ := extractNodeURLsLimited("xvless://a@h:1", -1); len(got) != 0 {
		t.Fatalf("词中出现的 scheme 不应匹配，得到 %v", got)
	}
	got, _ := extractNodeURLsLimited("vless://a@h:1\ntrojan://p@h:2", -1)
	if len(got) != 2 {
		t.Fatalf("换行分隔的两个链接应都提取，得到 %v", got)
	}
}

func TestIsPublicIP(t *testing.T) {
	nonPublic := []string{
		"0.1.2.3", "10.0.0.1", "100.64.0.1", "127.0.0.1", "169.254.169.254", "172.16.0.1",
		"192.0.0.1", "192.0.2.1", "192.31.196.1", "192.52.193.1", "192.88.99.1",
		"192.168.1.1", "192.175.48.1", "198.18.0.1", "198.51.100.1", "203.0.113.1",
		"224.0.0.1", "240.0.0.1", "255.255.255.255",
		"::1", "64:ff9b::1", "64:ff9b:1::1", "100::1", "2001::1", "2001:2::1",
		"2001:db8::1", "2002::1", "3fff::1", "5f00::1", "fc00::1", "fe80::1", "fec0::1", "ff00::1",
	}
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

func TestNodeParseErrorsDoNotEchoCredentials(t *testing.T) {
	for _, raw := range []string{
		"trojan://TOP-SECRET%zz@example.com:443",
		"vless://TOP-SECRET%zz@example.com:443",
		"vmess://TOP-SECRET-not-base64",
		"ss://TOP-SECRET-not-base64",
	} {
		_, err := parseNode(raw)
		if err == nil {
			t.Fatalf("malformed secret URL accepted: %s", raw)
		}
		if strings.Contains(err.Error(), "TOP-SECRET") || strings.Contains(err.Error(), raw) {
			t.Fatalf("parse error leaked node credential: %v", err)
		}
	}
}

func TestParseSSSIP002WithPath(t *testing.T) {
	// SIP002：host:port 后带可选 `/` 再接备注；路径分隔符不能污染端口。
	cases := []string{
		"ss://aes-256-gcm:secret@example.com:8388/#name",
		"ss://YWVzLTI1Ni1nY206c2VjcmV0@example.com:8388/#name",
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

func TestParseSSRejectsUnsupportedPlugin(t *testing.T) {
	for _, raw := range []string{
		"ss://aes-256-gcm:secret@example.com:8388/?plugin=v2ray-plugin",
		"ss://YWVzLTI1Ni1nY206c2VjcmV0@example.com:8388/?plugin=obfs#name",
	} {
		if _, err := parseNode(raw); err == nil {
			t.Fatalf("parseNode(%q) must reject unsupported plugin", raw)
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

func TestParseURLWithoutUserInfoReturnsError(t *testing.T) {
	for _, raw := range []string{
		"vless://example.com:443?security=tls",
		"trojan://example.com:443",
	} {
		if _, err := parseNode(raw); err == nil {
			t.Fatalf("parseNode(%q) should reject missing userinfo", raw)
		}
	}
}

func TestParseUppercaseSchemes(t *testing.T) {
	vmess := vmessURL(t, map[string]any{
		"v": "2", "add": "example.com", "port": "443",
		"id": "11111111-1111-1111-1111-111111111111", "net": "tcp",
	})
	vmess = "VMESS://" + strings.TrimPrefix(vmess, "vmess://")
	if _, err := parseNode(vmess); err != nil {
		t.Fatalf("uppercase VMess scheme rejected: %v", err)
	}
	if _, err := parseNode("SS://aes-256-gcm:secret@example.com:8388"); err != nil {
		t.Fatalf("uppercase SS scheme rejected: %v", err)
	}
}

func TestParseVMessRejectsControlCharacterHost(t *testing.T) {
	raw := vmessURL(t, map[string]any{
		"v": "2", "add": "evil\x1b[2Jhost", "port": "443",
		"id": "11111111-1111-1111-1111-111111111111", "net": "tcp",
	})
	if _, err := parseNode(raw); err == nil {
		t.Fatalf("VMess control-character host must be rejected")
	}
}

func TestSanitizeNodeNameRemovesUnicodeFormatControls(t *testing.T) {
	if got := sanitizeNodeName("a\u202eb\u2066\u200f"); got != "ab" {
		t.Fatalf("Unicode format controls not removed: %q", got)
	}
}

func TestNodeLimitsAndIndexedDeduplication(t *testing.T) {
	a := testApp(t)
	if _, err := prepareNode(strings.Repeat("x", maxNodeURLBytes+1)); err == nil {
		t.Fatalf("oversized node URL must be rejected")
	}
	st := newStore()
	index := map[string]int{}
	prepared, err := prepareNode("trojan://secret@example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	id, err := a.addPreparedNodeIndexed(st, prepared, "", "", index)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.addPreparedNodeIndexed(st, prepared, "", "", index)
	if err != nil || second != id || len(st.Nodes) != 1 {
		t.Fatalf("indexed dedupe failed: second=%q len=%d err=%v", second, len(st.Nodes), err)
	}
	full := newStore()
	full.Nodes = make([]Node, maxTotalNodes)
	if _, err := a.addPreparedNodeIndexed(full, prepared, "", "", nil); err == nil {
		t.Fatalf("node total limit must be enforced")
	}
}

func TestTransportGoldenConfigurations(t *testing.T) {
	const id = "11111111-1111-1111-1111-111111111111"
	t.Run("raw http header", func(t *testing.T) {
		pn, err := parseNode("vless://" + id + "@example.com:443?type=tcp&headerType=http&host=cdn.example.com&path=%2Fraw&security=tls")
		if err != nil {
			t.Fatal(err)
		}
		stream := pn.Outbound["streamSettings"].(map[string]any)
		want := map[string]any{
			"header": map[string]any{
				"type": "http",
				"request": map[string]any{
					"version": "1.1", "method": "GET", "path": []string{"/raw"},
					"headers": map[string]any{"Host": []string{"cdn.example.com"}},
				},
			},
		}
		if !reflect.DeepEqual(stream["rawSettings"], want) {
			t.Fatalf("rawSettings mismatch:\n got: %#v\nwant: %#v", stream["rawSettings"], want)
		}
	})

	t.Run("websocket", func(t *testing.T) {
		pn, err := parseNode("vless://" + id + "@example.com:443?type=ws&host=cdn.example.com&path=%2Fws&security=tls")
		if err != nil {
			t.Fatal(err)
		}
		stream := pn.Outbound["streamSettings"].(map[string]any)
		want := map[string]any{"host": "cdn.example.com", "path": "/ws"}
		if !reflect.DeepEqual(stream["wsSettings"], want) {
			t.Fatalf("wsSettings=%#v want %#v", stream["wsSettings"], want)
		}
	})

	t.Run("grpc multi", func(t *testing.T) {
		pn, err := parseNode("vless://" + id + "@example.com:443?type=grpc&serviceName=svc&authority=cdn.example.com&mode=multi&security=tls")
		if err != nil {
			t.Fatal(err)
		}
		stream := pn.Outbound["streamSettings"].(map[string]any)
		want := map[string]any{"serviceName": "svc", "authority": "cdn.example.com", "multiMode": true}
		if !reflect.DeepEqual(stream["grpcSettings"], want) {
			t.Fatalf("grpcSettings=%#v want %#v", stream["grpcSettings"], want)
		}
	})

	t.Run("xhttp extra", func(t *testing.T) {
		extra := url.QueryEscape(`{"scMaxBufferedPosts":2}`)
		pn, err := parseNode("vless://" + id + "@example.com:443?type=xhttp&host=cdn.example.com&path=%2Fx&mode=packet-up&extra=" + extra + "&security=tls")
		if err != nil {
			t.Fatal(err)
		}
		stream := pn.Outbound["streamSettings"].(map[string]any)
		settings := stream["xhttpSettings"].(map[string]any)
		if settings["host"] != "cdn.example.com" || settings["path"] != "/x" || settings["mode"] != "packet-up" {
			t.Fatalf("xhttpSettings mismatch: %#v", settings)
		}
		if _, ok := settings["extra"].(json.RawMessage); !ok {
			t.Fatalf("xhttp extra was not preserved as JSON: %#v", settings["extra"])
		}
	})
}

func TestTransportFailClosed(t *testing.T) {
	const base = "vless://11111111-1111-1111-1111-111111111111@example.com:443?security=tls&"
	for _, suffix := range []string{
		"type=http&host=cdn.example.com&path=%2Fold",
		"type=h2&host=cdn.example.com&path=%2Fold",
		"type=quic",
		"type=ws&seed=silently-dropped",
		"type=xhttp&mode=unknown",
		"type=grpc&mode=unknown",
		"type=ws&eh=Sec-WebSocket-Protocol",
	} {
		if _, err := parseNode(base + suffix); err == nil {
			t.Fatalf("transport must fail closed: %s", suffix)
		}
	}
}

func TestWebSocketEarlyDataUsesPinnedXrayPathSemantics(t *testing.T) {
	pn, err := parseNode("vless://11111111-1111-1111-1111-111111111111@example.com:443?security=tls&type=ws&path=%2Fws&ed=2048")
	if err != nil {
		t.Fatal(err)
	}
	stream := pn.Outbound["streamSettings"].(map[string]any)
	settings := stream["wsSettings"].(map[string]any)
	if settings["path"] != "/ws?ed=2048" {
		t.Fatalf("WS early data path=%q", settings["path"])
	}
}

func TestProtocolFieldsFailClosed(t *testing.T) {
	const id = "11111111-1111-1111-1111-111111111111"
	for _, raw := range []string{
		"vless://not-a-uuid@example.com:443?security=tls",
		"vless://" + id + "@example.com:443?security=tls&unknown=value",
		"vless://" + id + "@example.com:443?security=tls&sni=bad%20host",
		"vless://" + id + "@example.com:443?security=none&flow=xtls-rprx-vision",
		"vless://" + id + "@example.com:443?security=tls&type=ws&network=grpc&path=%2Fws",
		"vless://" + id + "@example.com:443?security=tls&sni=one.example&serverName=two.example",
		"vless://" + id + "@example.com:443?security=tls&type=grpc&serviceName=one&path=two",
		"vless://" + id + ":ignored@example.com:443?security=tls",
		"vless://" + id + "@example.com:443/ignored?security=tls",
		"vless://" + id + "@example.com:443?security=none&sni=tls.example.com",
		"vless://" + id + "@example.com:443?security=tls&pbk=ignored",
		"vless://" + id + "@example.com:443?security=tls&alpn=h2,,http%2F1.1",
		"vless://" + id + "@example.com:443?security=tls&type=raw&headerType=http&host=one.example,,two.example",
		"vless://" + id + "@example.com:443?security=tls&type=ws&path=%2Fws%3Fed%3D2048",
		"vless://" + id + "@example.com:443?security=tls&type=httpupgrade&path=%2Fup%3Feh%3DSec-WebSocket-Protocol",
		"trojan://secret@example.com:443?security=tls&unknown=value",
		"trojan://secret@example.com:443/ignored?security=tls",
		"trojan://secret@example.com:443?security=none&peer=tls.example.com",
		"ss://aes-256-gcm:secret@example.com:8388?uot=1",
		"ss://aes-256-gcm:secret@example.com:8388/anything",
		"ss://unsupported:secret@example.com:8388",
	} {
		if _, err := parseNode(raw); err == nil {
			t.Fatalf("unsupported or invalid field accepted: %s", raw)
		}
	}
}

func TestSSSingleSlashPathRemainsCompatible(t *testing.T) {
	if _, err := parseNode("ss://aes-256-gcm:secret@example.com:8388/"); err != nil {
		t.Fatalf("SIP002 optional slash rejected: %v", err)
	}
}

func TestVMessFieldsForOtherModesFailClosed(t *testing.T) {
	base := map[string]any{
		"v": "2", "add": "example.com", "port": 443,
		"id": "11111111-1111-1111-1111-111111111111", "net": "tcp",
	}
	for name, mutate := range map[string]func(map[string]any){
		"sni without tls":          func(v map[string]any) { v["sni"] = "tls.example.com" },
		"serviceName on raw":       func(v map[string]any) { v["serviceName"] = "ignored" },
		"mode on websocket":        func(v map[string]any) { v["net"] = "ws"; v["mode"] = "multi" },
		"serviceName on websocket": func(v map[string]any) { v["net"] = "ws"; v["serviceName"] = "ignored" },
	} {
		t.Run(name, func(t *testing.T) {
			value := make(map[string]any, len(base)+1)
			for key, item := range base {
				value[key] = item
			}
			mutate(value)
			if _, err := parseNode(vmessURL(t, value)); err == nil {
				t.Fatalf("VMess field for another mode was silently accepted: %#v", value)
			}
		})
	}
}

func TestRealityNormalizesPublicKeyAndDefaultsFingerprint(t *testing.T) {
	publicKey := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	raw := "vless://11111111-1111-1111-1111-111111111111@example.com:443?security=reality&type=raw&sni=reality.example.com&pbk=" + url.QueryEscape(publicKey+"\n")
	pn, err := parseNode(raw)
	if err != nil {
		t.Fatal(err)
	}
	stream := pn.Outbound["streamSettings"].(map[string]any)
	settings := stream["realitySettings"].(map[string]any)
	if settings["publicKey"] != publicKey {
		t.Fatalf("public key was not normalized: %q", settings["publicKey"])
	}
	if settings["fingerprint"] != "chrome" {
		t.Fatalf("default fingerprint=%q, want chrome", settings["fingerprint"])
	}
}

func TestVMessRejectsFractionalIntegersAndUnknownFields(t *testing.T) {
	base := map[string]any{
		"v": "2", "add": "example.com", "port": 443,
		"id": "11111111-1111-1111-1111-111111111111", "net": "tcp",
	}
	for name, mutate := range map[string]func(map[string]any){
		"fractional port":    func(v map[string]any) { v["port"] = 443.5 },
		"fractional alterId": func(v map[string]any) { v["aid"] = 0.5 },
		"unknown field":      func(v map[string]any) { v["skip-cert-verify"] = true },
		"invalid cipher":     func(v map[string]any) { v["scy"] = "unknown" },
	} {
		t.Run(name, func(t *testing.T) {
			value := make(map[string]any, len(base)+1)
			for key, item := range base {
				value[key] = item
			}
			mutate(value)
			if _, err := parseNode(vmessURL(t, value)); err == nil {
				t.Fatalf("invalid VMess value accepted: %#v", value)
			}
		})
	}
}

func TestVMessWebSocketTypeNoneRemainsCompatible(t *testing.T) {
	raw := vmessURL(t, map[string]any{
		"v": "2", "add": "example.com", "port": "443",
		"id": "11111111-1111-1111-1111-111111111111", "net": "ws",
		"type": "none", "host": "cdn.example.com", "path": "/ws",
	})
	if _, err := parseNode(raw); err != nil {
		t.Fatalf("standard VMess WS type=none rejected: %v", err)
	}
}

func TestNodeCommandSecretArgParsing(t *testing.T) {
	add, err := parseNodeAddCommandArgs([]string{"--stdin", "office"})
	if err != nil || !add.FromStdin || add.Name != "office" || add.Raw != "" {
		t.Fatalf("add --stdin parse=%+v err=%v", add, err)
	}
	if _, err := parseNodeAddCommandArgs([]string{"trojan://secret@h:443", "office"}); err == nil {
		t.Fatal("node URL positional argument must be rejected")
	}
	imp, err := parseNodeImportCommandArgs([]string{"--stdin"})
	if err != nil || !imp.FromStdin || imp.Value != "" {
		t.Fatalf("import --stdin parse=%+v err=%v", imp, err)
	}
	for _, args := range [][]string{{}, {"--stdin", "extra"}, {"one", "two"}} {
		if _, err := parseNodeImportCommandArgs(args); err == nil {
			t.Fatalf("invalid import args accepted: %#v", args)
		}
	}
	if _, err := parseNodeImportCommandArgs([]string{"https://example.com/sub?token=secret"}); err == nil {
		t.Fatal("subscription URL positional argument must be rejected")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestSubscriptionErrorsRedactURLAndStatusText(t *testing.T) {
	const secretURL = "https://user:password@example.com/sub?token=TOP-SECRET"
	t.Run("transport error", func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, errors.New("request failed: " + req.URL.String())
		})}
		_, err := downloadAndPrepareSubscriptionWithClient(secretURL, false, client)
		if err == nil {
			t.Fatalf("expected download error")
		}
		for _, secret := range []string{"TOP-SECRET", "password", secretURL} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaked %q: %v", secret, err)
			}
		}
	})

	t.Run("status reason", func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 503,
				Status:     "503 token=TOP-SECRET\x1b[2J",
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     make(http.Header),
			}, nil
		})}
		_, err := downloadAndPrepareSubscriptionWithClient(secretURL, false, client)
		if err == nil || !strings.Contains(err.Error(), "503") {
			t.Fatalf("expected numeric status error, got %v", err)
		}
		if strings.Contains(err.Error(), "TOP-SECRET") || strings.Contains(err.Error(), "\x1b") {
			t.Fatalf("status text leaked: %v", err)
		}
	})
}

func TestSubscriptionRedirectStripsSensitiveHeaders(t *testing.T) {
	client := subscriptionHTTPClient(true, true)
	original, _ := http.NewRequest(http.MethodGet, "https://source.example:443/sub?token=secret", nil)
	newRequest := func(target string) *http.Request {
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Referer", original.URL.String())
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set("Proxy-Authorization", "Basic secret")
		req.Header.Set("Cookie", "session=secret")
		return req
	}

	same := newRequest("https://source.example/next")
	if err := client.CheckRedirect(same, []*http.Request{original}); err != nil {
		t.Fatal(err)
	}
	if same.Header.Get("Referer") != "" {
		t.Fatalf("same-origin redirect retained Referer")
	}
	if same.Header.Get("Authorization") == "" || same.Header.Get("Cookie") == "" {
		t.Fatalf("same-origin credentials unexpectedly stripped: %#v", same.Header)
	}

	cross := newRequest("https://other.example/next")
	if err := client.CheckRedirect(cross, []*http.Request{original}); err != nil {
		t.Fatal(err)
	}
	for _, header := range []string{"Referer", "Authorization", "Proxy-Authorization", "Cookie"} {
		if value := cross.Header.Get(header); value != "" {
			t.Fatalf("cross-origin redirect retained %s=%q", header, value)
		}
	}
}

func TestPrepareSubscriptionEnforcesNodeLimit(t *testing.T) {
	body := strings.Repeat("trojan://secret@example.com:443\n", maxSubscriptionNodes+1)
	if _, err := prepareSubscriptionBody("https://example.com/sub", []byte(body)); err == nil {
		t.Fatalf("subscription node limit must be enforced")
	}
}

func TestMergeSubscriptionEnforcesHistoryLimit(t *testing.T) {
	a := testApp(t)
	st := newStore()
	node, err := prepareNode("trojan://secret@example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	st.Nodes = []Node{{ID: "existing", Name: "existing", Protocol: "trojan", RawURL: node.RawURL}}
	st.DefaultNodeID = "existing"
	st.Subscriptions = make([]string, maxSubscriptions)
	for i := range st.Subscriptions {
		st.Subscriptions[i] = "https://example.com/old/" + strconv.Itoa(i)
	}
	err = a.mergePreparedSubscription(st, preparedSubscription{
		URL:   "https://example.com/new",
		Nodes: []preparedNode{node},
	})
	if err == nil {
		t.Fatalf("subscription history limit must be enforced")
	}
	if len(st.Subscriptions) != maxSubscriptions {
		t.Fatalf("failed merge mutated subscription history")
	}
}

func TestMergeSpeedResultsRequiresUnchangedIDAndURL(t *testing.T) {
	now := time.Now()
	snapshot := []Node{
		{ID: "same", RawURL: "trojan://a@same:443"},
		{ID: "changed", RawURL: "trojan://a@old:443"},
		{ID: "removed", RawURL: "trojan://a@removed:443"},
	}
	results := []SpeedResult{
		{NodeID: "same", Success: true, LatencyMS: 10, TestedAt: now},
		{NodeID: "changed", Success: true, LatencyMS: 1, TestedAt: now},
		{NodeID: "removed", Success: true, LatencyMS: 2, TestedAt: now},
	}
	st := newStore()
	st.Nodes = []Node{
		{ID: "same", RawURL: "trojan://a@same:443"},
		{ID: "changed", RawURL: "trojan://a@new:443"},
		{ID: "new", RawURL: "trojan://a@new-node:443"},
	}
	st.SpeedResults["changed"] = SpeedResult{NodeID: "changed", Success: true, LatencyMS: 99}
	st.SpeedResults["removed"] = SpeedResult{NodeID: "removed", Success: true, LatencyMS: 99}
	fresh := mergeSpeedResults(st, snapshot, results)
	if len(fresh) != 1 || fresh["same"].LatencyMS != 10 {
		t.Fatalf("fresh results mismatch: %+v", fresh)
	}
	if _, ok := st.SpeedResults["changed"]; ok {
		t.Fatalf("changed RawURL retained stale result")
	}
	if _, ok := st.SpeedResults["removed"]; ok {
		t.Fatalf("removed node retained stale result")
	}
}

func TestRunSpeedTestsWithMapsResultsAndBoundsConcurrency(t *testing.T) {
	nodes := make([]Node, 30)
	for i := range nodes {
		nodes[i].ID = newNodeID()
	}
	var active, maximum int32
	results := runSpeedTestsWith(nodes, func(n Node) error {
		current := atomic.AddInt32(&active, 1)
		for {
			old := atomic.LoadInt32(&maximum)
			if current <= old || atomic.CompareAndSwapInt32(&maximum, old, current) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		atomic.AddInt32(&active, -1)
		if n.ID == nodes[0].ID {
			return errors.New("unreachable")
		}
		return nil
	})
	if maximum > 10 {
		t.Fatalf("speed test concurrency=%d, want <=10", maximum)
	}
	if len(results) != len(nodes) || results[0].Success || results[0].NodeID != nodes[0].ID {
		t.Fatalf("speed results mismatch: %+v", results[0])
	}
}

func TestGeneratedTransportConfigsPassPinnedXray(t *testing.T) {
	bin := os.Getenv("PROXYSCENE_TEST_XRAY")
	if bin == "" {
		t.Skip("set PROXYSCENE_TEST_XRAY to run pinned Xray config validation")
	}
	const id = "11111111-1111-1111-1111-111111111111"
	publicKey := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	cases := map[string]string{
		"raw-http":    "vless://" + id + "@example.com:443?type=tcp&headerType=http&host=cdn.example.com&path=%2Fraw&security=tls",
		"websocket":   "vless://" + id + "@example.com:443?type=ws&host=cdn.example.com&path=%2Fws&security=tls",
		"grpc":        "vless://" + id + "@example.com:443?type=grpc&serviceName=svc&security=tls",
		"httpupgrade": "vless://" + id + "@example.com:443?type=httpupgrade&host=cdn.example.com&path=%2Fup&security=tls",
		"xhttp":       "vless://" + id + "@example.com:443?type=xhttp&host=cdn.example.com&path=%2Fx&mode=packet-up&security=tls",
		"vless-none":  "vless://" + id + "@127.0.0.1:443?type=raw&security=none",
		"vless-reality": "vless://" + id + "@example.com:443?type=raw&security=reality&sni=reality.example.com&pbk=" +
			url.QueryEscape(publicKey),
		"vless-flow-vision": "vless://" + id + "@example.com:443?type=raw&security=tls&flow=xtls-rprx-vision",
		"vless-flow-udp443": "vless://" + id + "@example.com:443?type=raw&security=tls&flow=xtls-rprx-vision-udp443",
		"trojan-none":       "trojan://secret@127.0.0.1:443?type=raw&security=none",
		"trojan-tls":        "trojan://secret@example.com:443?type=raw&security=tls&sni=tls.example.com",
		"trojan-reality": "trojan://secret@example.com:443?type=raw&security=reality&sni=reality.example.com&pbk=" +
			url.QueryEscape(publicKey),
		"vmess-tls-websocket": vmessURL(t, map[string]any{
			"v": "2", "add": "example.com", "port": 443, "id": id,
			"net": "ws", "tls": "tls", "sni": "tls.example.com", "host": "cdn.example.com", "path": "/ws",
		}),
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = 0xfb
	}
	for name, encoding := range map[string]*base64.Encoding{"raw-url": base64.RawURLEncoding, "padded-url": base64.URLEncoding, "raw-standard": base64.RawStdEncoding, "padded-standard": base64.StdEncoding} {
		cases["reality-key-"+name] = "vless://" + id + "@example.com:443?security=reality&sni=example.com&pbk=" + url.QueryEscape(encoding.EncodeToString(key))
	}
	cases["ss-escaped-password"] = "ss://aes-256-gcm:p%40ss%3Aword+%25@example.com:8388"
	cases["ss-legacy-at-password"] = "ss://" + base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:p@ss@example.com:8388"))
	for _, cipher := range []string{"auto", "aes-128-gcm", "chacha20-poly1305", "none", "zero"} {
		cases["vmess-cipher-"+cipher] = vmessURL(t, map[string]any{
			"v": "2", "add": "example.com", "port": 443, "id": id,
			"net": "raw", "tls": "none", "scy": cipher,
		})
	}
	for _, method := range []string{
		"aes-128-gcm", "aead_aes_128_gcm",
		"aes-256-gcm", "aead_aes_256_gcm",
		"chacha20-poly1305", "aead_chacha20_poly1305", "chacha20-ietf-poly1305",
		"xchacha20-poly1305", "aead_xchacha20_poly1305", "xchacha20-ietf-poly1305",
	} {
		cases["shadowsocks-method-"+method] = "ss://" + method + ":secret@example.com:8388"
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			a := testApp(t)
			st := newStore()
			if _, err := a.addNode(st, raw, name, "default"); err != nil {
				t.Fatal(err)
			}
			st.SceneEnabled[SceneGlobal] = true
			config := filepath.Join(a.cfg.CoreDir, "transport.json")
			if err := a.writeXrayConfigTo(st, config); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			output, err := exec.CommandContext(ctx, bin, "run", "-test", "-format", "json", "-config", config).CombinedOutput()
			if err != nil {
				t.Fatalf("Xray -test failed: %v\n%s", err, output)
			}
		})
	}
}

func TestSSPasswordEncodingForms(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"plaintext escapes", "ss://aes-256-gcm:p%40ss%3Aword+%25@example.com:8388", "p@ss:word+%"},
		{"encoded userinfo literal", "ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:p%40ss+")) + "@example.com:8388", "p%40ss+"},
		{"legacy at sign", "ss://" + base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:p@ss%40+@example.com:8388")), "p@ss%40+"},
		{"legacy ipv6", "ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:p@ss@[2001:db8::1]:8388")), "p@ss"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pn, err := parseNode(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got := outboundPassword(t, pn); got != tc.want {
				t.Fatalf("password = %q, want %q", got, tc.want)
			}
		})
	}
	if _, err := parseNode("ss://aes-256-gcm:bad%xx@example.com:8388"); err == nil {
		t.Fatal("invalid percent escape accepted")
	}
}

func TestRealityPublicKeyEncodingsNormalizeForXray(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = 0xfb
	}
	canonical := base64.RawURLEncoding.EncodeToString(key)
	for _, encoding := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding} {
		encoded := encoding.EncodeToString(key)
		t.Run(encoded, func(t *testing.T) {
			pn, err := parseNode("vless://11111111-1111-1111-1111-111111111111@example.com:443?security=reality&sni=example.com&pbk=" + url.QueryEscape(encoded))
			if err != nil {
				t.Fatal(err)
			}
			settings := pn.Outbound["streamSettings"].(map[string]any)["realitySettings"].(map[string]any)
			if settings["publicKey"] != canonical {
				t.Fatalf("publicKey = %q, want %q", settings["publicKey"], canonical)
			}
		})
	}
}

func TestPinnedXrayRejectsRemovedModesButKeepsStoredNodesReadable(t *testing.T) {
	for _, raw := range []string{
		"vless://11111111-1111-1111-1111-111111111111@example.com:443?security=none",
		"trojan://secret@8.8.8.8:443?security=none",
		"ss://none:secret@example.com:8388",
		"ss://plain:secret@example.com:8388",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := prepareNode(raw); err == nil || !strings.Contains(err.Error(), "Xray v26.9.9") {
				t.Fatalf("import should explain removed mode: %v", err)
			}
			prepared, err := prepareStoredNode(raw)
			if err != nil {
				t.Fatal(err)
			}
			a := testApp(t)
			st := newStore()
			st.Nodes = []Node{{ID: "old", Name: "old", Protocol: prepared.Parsed.Protocol, RawURL: raw}}
			st.DefaultNodeID = "old"
			if err := a.saveStore(st); err != nil {
				t.Fatalf("legacy node cannot be retained for migration: %v", err)
			}
			got, err := a.loadStore()
			if err != nil || len(got.Nodes) != 1 {
				t.Fatalf("legacy node cannot be read: %+v, %v", got, err)
			}
			if _, err := a.outboundForScene(got, SceneGlobal, "test"); err == nil {
				t.Fatal("legacy unsupported node reached runtime")
			}
		})
	}
}
