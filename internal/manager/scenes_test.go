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
