package manager

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
	t.Setenv("PROXYSCENE_TG_SERVICES", "hermes-gateway user:root:hermes-gateway")
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

func TestDefaultConfigAllowsExplicitEmptyOptionalRuntimeOverrides(t *testing.T) {
	t.Setenv("PROXYSCENE_TG_SERVICES", "")
	t.Setenv("PROXYSCENE_DEV_TARGET_USER", "")
	cfg := DefaultConfig()
	if len(cfg.TGTargetServices) != 0 || !cfg.runtimeOverrides.TGTargetServices {
		t.Fatalf("explicit empty Telegram targets not preserved: %+v", cfg)
	}
	if cfg.DevTargetUser != "" || !cfg.runtimeOverrides.DevTargetUser {
		t.Fatalf("explicit automatic dev user selection not preserved: %+v", cfg)
	}
}

func TestValidateProxyHostLoopbackOnly(t *testing.T) {
	if err := validateProxyHost("127.0.0.1", false); err != nil {
		t.Fatalf("环回地址应通过：%v", err)
	}
	if err := validateProxyHost("0.0.0.0", false); err == nil {
		t.Fatalf("0.0.0.0 默认应被拒绝")
	}
	if err := validateProxyHost("8.8.8.8", false); err == nil {
		t.Fatalf("公网地址默认应被拒绝")
	}
	if err := validateProxyHost("0.0.0.0", true); err != nil {
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
	if _, _, err := parseInstallArgs([]string{"vless://x@h:1"}); err == nil {
		t.Fatalf("节点凭据位置参数必须被拒绝，避免泄漏到 argv")
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

func TestOutputAsUserUsesIdentityHomeWithoutCallerEnvironment(t *testing.T) {
	identity, err := lookupLocalUserIdentity("root")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", "")
	t.Setenv("GIT_CONFIG_GLOBAL", "/tmp/attacker-git-config")
	t.Setenv("NPM_CONFIG_USERCONFIG", "/tmp/attacker-npm-config")
	t.Setenv("HTTPS_PROXY", "http://attacker.invalid:8080")
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/attacker-runtime")

	out, err := outputAsUser("root", "/bin/sh", "-c", `printf '%s\n%s\n%s\n%s\n%s\n' "$HOME" "$XDG_RUNTIME_DIR" "${GIT_CONFIG_GLOBAL-unset}" "${NPM_CONFIG_USERCONFIG-unset}" "${HTTPS_PROXY-unset}"`)
	if err != nil {
		t.Fatal(err)
	}
	want := identity.Home + "\n/run/user/" + identity.UIDText + "\nunset\nunset\nunset\n"
	if out != want {
		t.Fatalf("sanitized user command environment = %q, want %q", out, want)
	}
	if _, err := os.Stat(identity.Home); err != nil {
		t.Fatalf("identity home disappeared during test: %v", err)
	}
}

func TestCommandAsPersistedUserPinsNumericIdentityAfterNameRemap(t *testing.T) {
	oldEffectiveUID := userCommandEffectiveUID
	t.Cleanup(func() { userCommandEffectiveUID = oldEffectiveUID })
	userCommandEffectiveUID = func() int { return 0 }

	home := t.TempDir()
	recorded := localUserIdentity{Name: "alice", UID: 12345, GID: 23456, UIDText: "12345", GIDText: "23456", Home: home}
	remapped := localUserIdentity{Name: "alice", UID: 34567, GID: 45678, UIDText: "34567", GIDText: "45678", Home: t.TempDir()}
	current := recorded
	lookupCalls := 0
	lookup := func(user string) (localUserIdentity, error) {
		if user != "alice" {
			t.Fatalf("lookup user = %q, want alice", user)
		}
		lookupCalls++
		result := current
		// Simulate an NSS/passwd remap immediately after verification returns.
		current = remapped
		return result, nil
	}
	persisted := &persistedUserIdentity{UID: recorded.UID, GID: recorded.GID, Home: recorded.Home}
	cmd, _, err := commandAsPersistedUser(context.Background(), "alice", persisted, lookup, "/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	if lookupCalls != 1 {
		t.Fatalf("username was resolved %d times, want exactly once for verification", lookupCalls)
	}
	if filepath.Base(cmd.Path) != "true" {
		t.Fatalf("command path = %q, want direct /bin/true execution", cmd.Path)
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential == nil {
		t.Fatal("root execution did not install a numeric syscall credential")
	}
	cred := cmd.SysProcAttr.Credential
	if cred.Uid != uint32(recorded.UID) || cred.Gid != uint32(recorded.GID) {
		t.Fatalf("credential = uid:%d gid:%d, want recorded uid:%d gid:%d", cred.Uid, cred.Gid, recorded.UID, recorded.GID)
	}
	if len(cred.Groups) != 0 || cred.NoSetGroups {
		t.Fatalf("supplementary group policy = groups:%v noSetGroups:%v, want explicit setgroups(0)", cred.Groups, cred.NoSetGroups)
	}
	env := strings.Join(cmd.Env, "\n")
	for _, want := range []string{"HOME=" + recorded.Home, "USER=alice", "LOGNAME=alice", "XDG_RUNTIME_DIR=/run/user/12345"} {
		if !strings.Contains(env, want) {
			t.Fatalf("command environment lacks %q: %q", want, env)
		}
	}
	if strings.Contains(env, remapped.Home) {
		t.Fatalf("command environment adopted replacement account home: %q", env)
	}
}

func TestCommandAsPersistedUserRejectsRemapBeforeVerification(t *testing.T) {
	recorded := &persistedUserIdentity{UID: 12345, GID: 23456, Home: t.TempDir()}
	lookup := func(string) (localUserIdentity, error) {
		return localUserIdentity{Name: "alice", UID: 34567, GID: 45678, UIDText: "34567", GIDText: "45678", Home: t.TempDir()}, nil
	}
	cmd, _, err := commandAsPersistedUser(context.Background(), "alice", recorded, lookup, "/bin/true")
	if err == nil || !strings.Contains(err.Error(), "身份已变化") {
		t.Fatalf("replacement account was not rejected: cmd=%v err=%v", cmd, err)
	}
}

func TestCommandForUserIdentityAllowsMatchingNonRootIdentity(t *testing.T) {
	oldEffectiveUID := userCommandEffectiveUID
	oldEffectiveGID := userCommandEffectiveGID
	t.Cleanup(func() {
		userCommandEffectiveUID = oldEffectiveUID
		userCommandEffectiveGID = oldEffectiveGID
	})
	userCommandEffectiveUID = func() int { return 12345 }
	userCommandEffectiveGID = func() int { return 23456 }
	identity := localUserIdentity{Name: "alice", UID: 12345, GID: 23456, Home: t.TempDir()}
	cmd, _, err := commandForUserIdentity(context.Background(), "alice", identity, "/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Credential != nil {
		t.Fatalf("matching non-root EUID should execute directly without setuid credential: %+v", cmd.SysProcAttr.Credential)
	}
}

func TestCommandForUserIdentityRejectsDifferentNonRootEUID(t *testing.T) {
	oldEffectiveUID := userCommandEffectiveUID
	t.Cleanup(func() { userCommandEffectiveUID = oldEffectiveUID })
	userCommandEffectiveUID = func() int { return 1000 }
	identity := localUserIdentity{Name: "alice", UID: 1001, GID: 1001, Home: t.TempDir()}
	cmd, _, err := commandForUserIdentity(context.Background(), "alice", identity, "/bin/true")
	if err == nil || cmd != nil || !strings.Contains(err.Error(), "拒绝通过用户名切换身份") {
		t.Fatalf("different non-root target was not rejected: cmd=%v err=%v", cmd, err)
	}
}

func TestCommandForUserIdentityRejectsDifferentNonRootEGID(t *testing.T) {
	oldEffectiveUID := userCommandEffectiveUID
	oldEffectiveGID := userCommandEffectiveGID
	t.Cleanup(func() {
		userCommandEffectiveUID = oldEffectiveUID
		userCommandEffectiveGID = oldEffectiveGID
	})
	userCommandEffectiveUID = func() int { return 1001 }
	userCommandEffectiveGID = func() int { return 2000 }
	identity := localUserIdentity{Name: "alice", UID: 1001, GID: 1001, Home: t.TempDir()}
	cmd, _, err := commandForUserIdentity(context.Background(), "alice", identity, "/bin/true")
	if err == nil || cmd != nil || !strings.Contains(err.Error(), "拒绝通过用户名切换身份") {
		t.Fatalf("different non-root primary group was not rejected: cmd=%v err=%v", cmd, err)
	}
}

func TestOutputUserSystemctlQuietPersistedUsesRecordedIdentityAndBusEnvironment(t *testing.T) {
	oldStat := userSystemctlStat
	oldExecutable := userSystemctlExecutable
	t.Cleanup(func() {
		userSystemctlStat = oldStat
		userSystemctlExecutable = oldExecutable
	})
	userSystemctlStat = func(string) (os.FileInfo, error) { return nil, nil }
	script := filepath.Join(t.TempDir(), "systemctl-test")
	content := "#!/bin/sh\nprintf '%s\\n%s\\n%s\\n%s\\n%s\\n%s\\n%s\\n' \"$HOME\" \"$USER\" \"$LOGNAME\" \"$XDG_RUNTIME_DIR\" \"$DBUS_SESSION_BUS_ADDRESS\" \"$1\" \"$2\"\n"
	if err := os.WriteFile(script, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	userSystemctlExecutable = script

	uid, gid := os.Geteuid(), os.Getegid()
	home := t.TempDir()
	identity := &persistedUserIdentity{UID: uid, GID: gid, Home: home}
	lookupCalls := 0
	lookup := func(user string) (localUserIdentity, error) {
		lookupCalls++
		return localUserIdentity{Name: user, UID: uid, GID: gid, UIDText: strconv.Itoa(uid), GIDText: strconv.Itoa(gid), Home: home}, nil
	}
	out, err := outputUserSystemctlQuietPersisted("alice", identity, lookup, "show-environment")
	if err != nil {
		t.Fatal(err)
	}
	runtimeDir := "/run/user/" + strconv.Itoa(uid)
	want := strings.Join([]string{
		home,
		"alice",
		"alice",
		runtimeDir,
		"unix:path=" + runtimeDir + "/bus",
		"--user",
		"show-environment",
		"",
	}, "\n")
	if out != want {
		t.Fatalf("user systemctl output = %q, want %q", out, want)
	}
	if lookupCalls != 2 {
		t.Fatalf("user identity verification calls = %d, want pre-bus and pre-exec checks", lookupCalls)
	}
}

func TestOutputUserSystemctlQuietPersistedRejectsRemapBeforeExec(t *testing.T) {
	oldStat := userSystemctlStat
	oldExecutable := userSystemctlExecutable
	t.Cleanup(func() {
		userSystemctlStat = oldStat
		userSystemctlExecutable = oldExecutable
	})
	userSystemctlStat = func(string) (os.FileInfo, error) { return nil, nil }
	userSystemctlExecutable = "/bin/true"

	identity := &persistedUserIdentity{UID: 12345, GID: 23456, Home: t.TempDir()}
	lookupCalls := 0
	lookup := func(user string) (localUserIdentity, error) {
		lookupCalls++
		if lookupCalls == 1 {
			return localUserIdentity{Name: user, UID: identity.UID, GID: identity.GID, Home: identity.Home}, nil
		}
		return localUserIdentity{Name: user, UID: identity.UID + 1, GID: identity.GID + 1, Home: t.TempDir()}, nil
	}
	out, err := outputUserSystemctlQuietPersisted("alice", identity, lookup, "show-environment")
	if err == nil || out != "" || !strings.Contains(err.Error(), "身份已变化") {
		t.Fatalf("same-name remap was not rejected before systemctl exec: output=%q err=%v", out, err)
	}
	if lookupCalls != 2 {
		t.Fatalf("identity checks = %d, want 2", lookupCalls)
	}
}

func TestOpenUserFileDirForIdentityValidatesOpenedHomeOwner(t *testing.T) {
	home := t.TempDir()
	var st syscall.Stat_t
	if err := syscall.Stat(home, &st); err != nil {
		t.Fatal(err)
	}
	identity := localUserIdentity{
		Name: "alice",
		UID:  int(st.Uid) + 1,
		GID:  int(st.Gid) + 1,
		Home: home,
	}
	path := filepath.Join(home, ".config", "managed.conf")
	for _, create := range []bool{false, true} {
		_, fd, err := openUserFileDirForIdentity("alice", identity, path, create)
		if fd >= 0 {
			_ = syscall.Close(fd)
		}
		if err == nil || !strings.Contains(err.Error(), "打开后属主与记录身份不一致") {
			t.Fatalf("create=%v did not reject replaced/mismatched home: fd=%d err=%v", create, fd, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(home, ".config")); !os.IsNotExist(err) {
		t.Fatalf("mismatched home was modified before rejection: %v", err)
	}
}

func TestValidateProxyHostRequiresIPLiteral(t *testing.T) {
	// 主机名不再被接受：xray listen 不支持主机名，晚失败会拖到配置检查阶段。
	for _, host := range []string{"localhost", "example.com", "proxy.internal"} {
		if err := validateProxyHost(host, false); err == nil {
			t.Fatalf("主机名 %q 应被拒绝", host)
		}
	}
	if err := validateProxyHost("::1", false); err != nil {
		t.Fatalf("IPv6 环回应通过：%v", err)
	}
	if err := validateProxyHost("2001:db8::1", false); err == nil {
		t.Fatalf("IPv6 非环回默认应被拒绝")
	}
	if err := validateProxyHost("2001:db8::1", true); err != nil {
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
