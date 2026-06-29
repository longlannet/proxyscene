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
