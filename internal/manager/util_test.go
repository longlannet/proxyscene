package manager

import (
	"bufio"
	"strings"
	"testing"
)

func TestAskReturnsFalseOnEOF(t *testing.T) {
	old := stdinReader
	defer func() { stdinReader = old }()

	stdinReader = bufio.NewReader(strings.NewReader(""))
	if s, ok := ask("> "); ok || s != "" {
		t.Fatalf("ask EOF = (%q, %v), want (\"\", false)", s, ok)
	}
}

func TestAskReadsLines(t *testing.T) {
	old := stdinReader
	defer func() { stdinReader = old }()

	// 第二行没有结尾换行符，仍应作为有效输入返回。
	stdinReader = bufio.NewReader(strings.NewReader("  hello \nworld"))
	if s, ok := ask("> "); !ok || s != "hello" {
		t.Fatalf("ask line1 = (%q, %v), want (\"hello\", true)", s, ok)
	}
	if s, ok := ask("> "); !ok || s != "world" {
		t.Fatalf("ask line2 = (%q, %v), want (\"world\", true)", s, ok)
	}
	if s, ok := ask("> "); ok || s != "" {
		t.Fatalf("ask EOF = (%q, %v), want (\"\", false)", s, ok)
	}
}

func TestValidateTestURL(t *testing.T) {
	valid := []string{
		"https://www.google.com/generate_204",
		"http://example.com",
		"https://1.2.3.4:8443/x",
	}
	for _, u := range valid {
		if err := validateTestURL(u); err != nil {
			t.Fatalf("validateTestURL(%q) 应通过，却返回：%v", u, err)
		}
	}
	invalid := []string{"", "   ", "ftp://example.com", "example.com", "://nohost"}
	for _, u := range invalid {
		if err := validateTestURL(u); err == nil {
			t.Fatalf("validateTestURL(%q) 应失败", u)
		}
	}
}

func TestDefaultConfigValidates(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("DefaultConfig().Validate() 应通过，却返回：%v", err)
	}
}

func TestDefaultConfigIncludesRootUserHermesTarget(t *testing.T) {
	t.Setenv("PROXYSCENE_TG_SERVICES", "")
	cfg := DefaultConfig()
	wantSystem, wantUser := false, false
	for _, svc := range cfg.TGTargetServices {
		switch svc {
		case "hermes-gateway":
			wantSystem = true
		case "user:root:hermes-gateway":
			wantUser = true
		}
	}
	if !wantSystem || !wantUser {
		t.Fatalf("default TG targets = %v, want hermes-gateway and user:root:hermes-gateway", cfg.TGTargetServices)
	}
}

func TestValidateProxyHostLoopbackOnly(t *testing.T) {
	t.Setenv("PROXYSCENE_ALLOW_PUBLIC_BIND", "0")
	if err := validateProxyHost("127.0.0.1"); err != nil {
		t.Fatalf("环回地址应通过：%v", err)
	}
	if err := validateProxyHost("0.0.0.0"); err == nil {
		t.Fatalf("0.0.0.0 默认应被拒绝")
	}
	if err := validateProxyHost("8.8.8.8"); err == nil {
		t.Fatalf("公网地址默认应被拒绝")
	}
	t.Setenv("PROXYSCENE_ALLOW_PUBLIC_BIND", "1")
	if err := validateProxyHost("0.0.0.0"); err != nil {
		t.Fatalf("显式 opt-in 后应允许：%v", err)
	}
}

func TestNormalizeTargetServiceNameRejectsTemplateShorthand(t *testing.T) {
	if _, err := parseSystemdTargetName("foo@bar"); err == nil {
		t.Fatalf("模板简写 foo@bar 应被拒绝")
	}
	if _, err := parseSystemdTargetName("foo@bar.service"); err != nil {
		t.Fatalf("完整模板实例应被接受：%v", err)
	}
	tn, err := parseSystemdTargetName("hermes-gateway")
	if err != nil || tn.Service != "hermes-gateway.service" {
		t.Fatalf("普通简写归一化失败：%+v err=%v", tn, err)
	}
}

func TestParseInstallArgs(t *testing.T) {
	if _, _, err := parseInstallArgs([]string{"--skp-node"}); err == nil {
		t.Fatalf("未知 flag 应被拒绝")
	}
	raw, prompt, err := parseInstallArgs([]string{"vless://x@h:1"})
	if err != nil || raw != "vless://x@h:1" || !prompt {
		t.Fatalf("节点链接解析错误：raw=%q prompt=%v err=%v", raw, prompt, err)
	}
	if _, prompt, err := parseInstallArgs([]string{"--skip-node"}); err != nil || prompt {
		t.Fatalf("--skip-node 应设置 prompt=false：prompt=%v err=%v", prompt, err)
	}
}

func TestParsePasswdLine(t *testing.T) {
	id, err := parsePasswdLine("alice:x:1001:1002:Alice:/home/alice:/bin/bash", "alice")
	if err != nil || id.UID != 1001 || id.GID != 1002 || id.Home != "/home/alice" {
		t.Fatalf("解析失败：%+v err=%v", id, err)
	}
	if _, err := parsePasswdLine("bob:x:1:1:::", "bob"); err == nil {
		t.Fatalf("家目录为空的不完整记录应失败")
	}
}

func TestValidateProxyHostRequiresIPLiteral(t *testing.T) {
	t.Setenv("PROXYSCENE_ALLOW_PUBLIC_BIND", "0")
	// 主机名不再被接受：xray listen 不支持主机名，晚失败会拖到配置检查阶段。
	for _, host := range []string{"localhost", "example.com", "proxy.internal"} {
		if err := validateProxyHost(host); err == nil {
			t.Fatalf("主机名 %q 应被拒绝", host)
		}
	}
	if err := validateProxyHost("::1"); err != nil {
		t.Fatalf("IPv6 环回应通过：%v", err)
	}
	if err := validateProxyHost("2001:db8::1"); err == nil {
		t.Fatalf("IPv6 非环回默认应被拒绝")
	}
	t.Setenv("PROXYSCENE_ALLOW_PUBLIC_BIND", "1")
	if err := validateProxyHost("2001:db8::1"); err != nil {
		t.Fatalf("显式 opt-in 后 IPv6 非环回应允许：%v", err)
	}
}

func TestHTTPAddrIPv6Brackets(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ProxyHost = "::1"
	if got := cfg.HTTPAddr(SceneGlobal); got != "http://[::1]:7890" {
		t.Fatalf("HTTPAddr(global) = %q, want http://[::1]:7890", got)
	}
	if got := cfg.TGSocksAddr(); got != "socks5h://[::1]:7893" {
		t.Fatalf("TGSocksAddr = %q, want socks5h://[::1]:7893", got)
	}
	cfg.ProxyHost = "127.0.0.1"
	if got := cfg.HTTPAddr(SceneDev); got != "http://127.0.0.1:7891" {
		t.Fatalf("HTTPAddr(dev) = %q, want http://127.0.0.1:7891", got)
	}
}

func TestEnvBoolStrictTokens(t *testing.T) {
	t.Setenv("PROXYSCENE_TEST_BOOL", "off")
	if envBool("PROXYSCENE_TEST_BOOL", true) {
		t.Fatalf("off 应解析为 false")
	}
	t.Setenv("PROXYSCENE_TEST_BOOL", "YES")
	if !envBool("PROXYSCENE_TEST_BOOL", false) {
		t.Fatalf("YES 应解析为 true")
	}
	// 无法识别的值应回退默认值，而不是静默当 false。
	t.Setenv("PROXYSCENE_TEST_BOOL", "ture")
	if !envBool("PROXYSCENE_TEST_BOOL", true) {
		t.Fatalf("无法识别的值应回退默认值 true")
	}
}

func TestEnvIntInvalidFallsBack(t *testing.T) {
	t.Setenv("PROXYSCENE_TEST_INT", "not-a-number")
	if got := envInt("PROXYSCENE_TEST_INT", 42); got != 42 {
		t.Fatalf("非法值应回退默认值：got %d", got)
	}
	t.Setenv("PROXYSCENE_TEST_INT", "-1")
	if got := envInt("PROXYSCENE_TEST_INT", 42); got != 42 {
		t.Fatalf("非正数应回退默认值：got %d", got)
	}
	t.Setenv("PROXYSCENE_TEST_INT", "8080")
	if got := envInt("PROXYSCENE_TEST_INT", 42); got != 8080 {
		t.Fatalf("合法值应生效：got %d", got)
	}
}

func TestNeedsPrivilegedPortCap(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.needsPrivilegedPortCap() {
		t.Fatalf("默认端口(789x)不应需要特权端口能力")
	}
	cfg.GlobalHTTPPort = 443
	if !cfg.needsPrivilegedPortCap() {
		t.Fatalf("端口 443 应需要 CAP_NET_BIND_SERVICE")
	}
}

func TestUseNodeInStoreChineseScopeAliases(t *testing.T) {
	a := testApp(t)
	st := newStore()
	id, err := a.addNode(st, "trojan://secret@h:443", "", "")
	if err != nil {
		t.Fatalf("addNode: %v", err)
	}
	if err := a.useNodeInStore(st, id, "全局"); err != nil {
		t.Fatalf("中文范围 全局 应被接受：%v", err)
	}
	if st.SceneNodes[SceneGlobal] != id {
		t.Fatalf("全局 未生效：%+v", st.SceneNodes)
	}
	if err := a.useNodeInStore(st, id, "全部"); err != nil {
		t.Fatalf("中文范围 全部 应被接受：%v", err)
	}
	if st.DefaultNodeID != id || st.SceneNodes[SceneDev] != id || st.SceneNodes[SceneTelegram] != id {
		t.Fatalf("全部 未生效：default=%q scenes=%+v", st.DefaultNodeID, st.SceneNodes)
	}
	if err := a.useNodeInStore(st, id, "bogus"); err == nil {
		t.Fatalf("未知范围应报错")
	}
}
