package manager

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsHermesGatewayUnitName(t *testing.T) {
	cases := map[string]bool{
		"hermes-gateway.service":             true,  // 默认 profile
		"hermes-gateway-coder.service":       true,  // profile 实例
		"hermes-gateway-default.service":     true,  // profile 实例
		"prometheus-hermes-exporter.service": false, // 名字含 hermes 但非网关
		"hermesd.service":                    false,
		"openclaw-gateway.service":           false, // 不是 hermes
	}
	for svc, want := range cases {
		if got := isHermesGatewayUnitName(svc); got != want {
			t.Errorf("isHermesGatewayUnitName(%q) = %v, want %v", svc, got, want)
		}
	}
}

func TestUnitLooksLikeTelegramClient(t *testing.T) {
	// OpenClaw 网关：带厂商标记 + KIND=gateway —— 命中。
	openclawGateway := "[Service]\n" +
		"ExecStart=/usr/bin/node /opt/openclaw/dist/index.js gateway --port 18789\n" +
		"Environment=OPENCLAW_SERVICE_MARKER=openclaw\n" +
		"Environment=OPENCLAW_SERVICE_KIND=gateway\n"
	if !unitLooksLikeTelegramClient(openclawGateway) {
		t.Errorf("openclaw gateway（带 marker+kind=gateway）应命中")
	}

	// OpenClaw guard：仅文件名/描述含 openclaw，无 marker —— 不命中（修复的误报）。
	openclawGuard := "[Unit]\nDescription=OpenClaw xhigh guard for pi-ai models.js\n" +
		"[Service]\nType=oneshot\nExecStart=/usr/bin/node /root/.openclaw/scripts/openclaw-xhigh-guard.mjs\n"
	if unitLooksLikeTelegramClient(openclawGuard) {
		t.Errorf("openclaw guard（无 marker）不应命中")
	}

	// OpenClaw node：有 marker 但 KIND=node（非 Telegram 网关）—— 不命中。
	openclawNode := "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw\nEnvironment=OPENCLAW_SERVICE_KIND=node\n"
	if unitLooksLikeTelegramClient(openclawNode) {
		t.Errorf("openclaw node（KIND!=gateway）不应命中")
	}

	// Hermes：ExecStart 调 hermes_cli ... gateway —— 命中。
	hermesGateway := "[Service]\n" +
		"ExecStart=/root/.hermes/hermes-agent/venv/bin/python -m hermes_cli.main gateway run\n" +
		`Environment="HERMES_HOME=/root/.hermes"` + "\n"
	if !unitLooksLikeTelegramClient(hermesGateway) {
		t.Errorf("hermes 网关（ExecStart 调 hermes_cli gateway）应命中")
	}

	// 无关单元：Description 提到 hermes，但既无 openclaw marker、ExecStart 也不调 hermes_cli。
	unrelated := "[Unit]\nDescription=Backup job for the hermes database\n" +
		"[Service]\nExecStart=/usr/bin/pg_dump hermes\n"
	if unitLooksLikeTelegramClient(unrelated) {
		t.Errorf("仅描述含 hermes 的无关单元不应命中")
	}
}

func TestIsTelegramRelatedUnitRejectsGuardByFile(t *testing.T) {
	dir := t.TempDir()
	// 写一个文件名含 openclaw、但内容无 marker 的 guard 单元。
	guardPath := filepath.Join(dir, "openclaw-xhigh-guard.service")
	guard := "[Unit]\nDescription=OpenClaw xhigh guard\n[Service]\nType=oneshot\nExecStart=/usr/bin/node /x/guard.mjs\n"
	if err := os.WriteFile(guardPath, []byte(guard), 0o644); err != nil {
		t.Fatalf("write guard unit: %v", err)
	}
	if isTelegramRelatedUnit(guardPath, "openclaw-xhigh-guard.service") {
		t.Errorf("guard 单元（文件名含 openclaw 但无 marker）不应被判为 Telegram 目标")
	}

	// 写一个真正的 openclaw 网关单元。
	gwPath := filepath.Join(dir, "openclaw-gateway.service")
	gw := "[Service]\nExecStart=/usr/bin/node /x/index.js gateway\n" +
		"Environment=OPENCLAW_SERVICE_MARKER=openclaw\nEnvironment=OPENCLAW_SERVICE_KIND=gateway\n"
	if err := os.WriteFile(gwPath, []byte(gw), 0o644); err != nil {
		t.Fatalf("write gateway unit: %v", err)
	}
	if !isTelegramRelatedUnit(gwPath, "openclaw-gateway.service") {
		t.Errorf("openclaw 网关单元应被判为 Telegram 目标")
	}
}

func TestUnitFileExistsInRoots(t *testing.T) {
	dir := t.TempDir()
	if unitFileExistsInRoots("hermes-gateway", []string{dir}) {
		t.Fatalf("missing unit should not exist")
	}
	if err := os.WriteFile(filepath.Join(dir, "hermes-gateway.service"), []byte("[Service]\n"), 0o644); err != nil {
		t.Fatalf("write unit: %v", err)
	}
	if !unitFileExistsInRoots("hermes-gateway", []string{dir}) {
		t.Fatalf("unit should be found with shorthand service name")
	}
}

func TestPrivateUserUnitRootsIncludesLocalShare(t *testing.T) {
	home := t.TempDir()
	roots := privateUserUnitRoots(home)
	want := map[string]bool{
		filepath.Join(home, ".config/systemd/user"):      true,
		filepath.Join(home, ".local/share/systemd/user"): true,
	}
	for _, root := range roots {
		delete(want, root)
	}
	if len(want) != 0 {
		t.Fatalf("private user unit roots missing entries: %v", want)
	}
}

func TestUnitOpenClawMarkerExactMatch(t *testing.T) {
	// 子串包含不应命中：赋值必须整体精确等于 marker/kind。
	fake := "[Service]\nEnvironment=FOO_OPENCLAW_SERVICE_MARKER=openclawx\nEnvironment=PREFIX_OPENCLAW_SERVICE_KIND=gatewayy\n"
	if unitHasOpenClawGatewayMarker(fake) {
		t.Errorf("前后缀近似的赋值不应命中")
	}
	// 引号 + 同行多赋值的合法写法应命中。
	quoted := "[Service]\nEnvironment=\"OPENCLAW_SERVICE_MARKER=openclaw\" \"OPENCLAW_SERVICE_KIND=gateway\"\n"
	if !unitHasOpenClawGatewayMarker(quoted) {
		t.Errorf("带引号/同行多赋值的 Environment 行应命中")
	}
}
