package manager

import (
	"strings"
	"testing"
)

func TestTelegramProxyEnvPairsOnlyTelegramScoped(t *testing.T) {
	cfg := DefaultConfig()
	pairs := telegramProxyEnvPairs(cfg)

	// 只注入 TELEGRAM_PROXY（Hermes 实际消费的唯一 telegram 专用变量）。
	want := map[string]string{
		"TELEGRAM_PROXY": "http://127.0.0.1:7892",
	}
	if len(pairs) != len(want) {
		t.Fatalf("telegramProxyEnvPairs() returned %d pairs, want %d: %v", len(pairs), len(want), pairs)
	}
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			t.Fatalf("env pair missing '=': %q", pair)
		}
		if !strings.HasPrefix(key, "TELEGRAM_") {
			t.Fatalf("telegram proxy env must not inject broad proxy variable %q", key)
		}
		if wantValue, ok := want[key]; !ok {
			t.Fatalf("unexpected telegram proxy env key %q", key)
		} else if value != wantValue {
			t.Fatalf("%s=%q, want %q", key, value, wantValue)
		}
		delete(want, key)
	}
	for key := range want {
		t.Fatalf("missing telegram proxy env key %q", key)
	}
}

func TestTelegramProxySystemdEnvironmentLinesDoNotInjectBroadProxy(t *testing.T) {
	lines := telegramProxySystemdEnvironmentLines(DefaultConfig())
	for _, line := range strings.Split(strings.TrimSpace(lines), "\n") {
		line = strings.TrimPrefix(line, "Environment=")
		line = strings.Trim(line, "\"")
		key, _, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("systemd environment line missing env assignment: %q", line)
		}
		switch key {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy":
			t.Fatalf("systemd env lines include forbidden broad proxy %q in:\n%s", key, lines)
		}
	}
	for _, required := range []string{
		`Environment="TELEGRAM_PROXY=http://127.0.0.1:7892"`,
	} {
		if !strings.Contains(lines, required) {
			t.Fatalf("systemd env lines missing %s in:\n%s", required, lines)
		}
	}
}

func TestStaleTelegramTargets(t *testing.T) {
	// 模拟 ≤v0.5.0 时代的存量记录：升级后默认/发现集合只剩 hermes-gateway，
	// 其余三个旧目标应被识别为需差集清理的残留。
	stored := []string{
		"openclaw.service",
		"hermes.service",
		"hermes-gateway.service",
		"user:root:hermes-gateway.service",
		"hermes.service", // 重复记录只应出现一次
		"bad//name",      // 非法记录应被跳过而不是中断
	}
	current := []systemdTargetName{{Service: "hermes-gateway.service"}}
	stale := staleTelegramTargets(stored, current)
	got := map[string]bool{}
	for _, target := range stale {
		got[canonicalTelegramTargetName(target)] = true
	}
	want := []string{"openclaw.service", "hermes.service", "user:root:hermes-gateway.service"}
	if len(stale) != len(want) {
		t.Fatalf("stale 数量 = %d，期望 %d：%+v", len(stale), len(want), stale)
	}
	for _, name := range want {
		if !got[name] {
			t.Fatalf("缺少应清理的目标 %s：%+v", name, stale)
		}
	}
	if got["hermes-gateway.service"] {
		t.Fatalf("仍在集合内的目标不应被判为残留")
	}
	// 全部仍在集合内时应返回空。
	if s := staleTelegramTargets([]string{"hermes-gateway.service"}, current); len(s) != 0 {
		t.Fatalf("无残留时应为空，得到 %+v", s)
	}
}
