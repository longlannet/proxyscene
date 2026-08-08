package manager

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

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

func TestTelegramRelatedUnitRejectsGuardByFile(t *testing.T) {
	dir := t.TempDir()
	// 写一个文件名含 openclaw、但内容无 marker 的 guard 单元。
	guardPath := filepath.Join(dir, "openclaw-xhigh-guard.service")
	guard := "[Unit]\nDescription=OpenClaw xhigh guard\n[Service]\nType=oneshot\nExecStart=/usr/bin/node /x/guard.mjs\n"
	if err := os.WriteFile(guardPath, []byte(guard), 0o644); err != nil {
		t.Fatalf("write guard unit: %v", err)
	}
	related, err := telegramRelatedUnit(guardPath, "openclaw-xhigh-guard.service")
	if err != nil {
		t.Fatal(err)
	}
	if related {
		t.Errorf("guard 单元（文件名含 openclaw 但无 marker）不应被判为 Telegram 目标")
	}

	// 写一个真正的 openclaw 网关单元。
	gwPath := filepath.Join(dir, "openclaw-gateway.service")
	gw := "[Service]\nExecStart=/usr/bin/node /x/index.js gateway\n" +
		"Environment=OPENCLAW_SERVICE_MARKER=openclaw\nEnvironment=OPENCLAW_SERVICE_KIND=gateway\n"
	if err := os.WriteFile(gwPath, []byte(gw), 0o644); err != nil {
		t.Fatalf("write gateway unit: %v", err)
	}
	related, err = telegramRelatedUnit(gwPath, "openclaw-gateway.service")
	if err != nil {
		t.Fatal(err)
	}
	if !related {
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

func TestUnsetEnvironmentUsesFinalSystemdSemantics(t *testing.T) {
	base := "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\n"
	for name, unit := range map[string]string{
		"name":        base + "UnsetEnvironment=OPENCLAW_SERVICE_MARKER\n",
		"assignment":  base + "UnsetEnvironment=OPENCLAW_SERVICE_KIND=gateway\n",
		"final-order": base + "UnsetEnvironment=OPENCLAW_SERVICE_MARKER\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw\n",
	} {
		if unitHasOpenClawGatewayMarker(unit) {
			t.Errorf("UnsetEnvironment case %s should remove the final marker", name)
		}
	}
	if !unitHasOpenClawGatewayMarker(base + "UnsetEnvironment=OPENCLAW_SERVICE_KIND=node\n") {
		t.Fatal("a non-matching NAME=value unset must not remove the marker")
	}
	if !unitHasOpenClawGatewayMarker(base + "UnsetEnvironment=OPENCLAW_SERVICE_MARKER\nUnsetEnvironment=\n") {
		t.Fatal("empty UnsetEnvironment must reset earlier unset entries")
	}
}

func TestSplitSystemdWordsDecodesCStyleEscapes(t *testing.T) {
	words, err := splitSystemdWords(`"T\x45LEGRAM_PROXY=http://evil" "NO\137PROXY=api.telegram.org" "space\svalue"`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"TELEGRAM_PROXY=http://evil", "NO_PROXY=api.telegram.org", "space value"}
	if !reflect.DeepEqual(words, want) {
		t.Fatalf("words=%q want=%q", words, want)
	}
	for _, invalid := range []string{`"bad\qescape"`, `"bad\x0"`, `"nul\x00"`} {
		if _, err := splitSystemdWords(invalid); err == nil {
			t.Errorf("invalid systemd escape accepted: %q", invalid)
		}
	}
}

func TestEscapedEnvironmentAndUnsetUseDecodedNames(t *testing.T) {
	content := "[Service]\n" +
		`Environment="T\x45LEGRAM_PROXY=http://evil" "NO\137PROXY=api.telegram.org"` + "\n" +
		`UnsetEnvironment=NO\137PROXY` + "\n"
	environment, err := effectiveServiceEnvironment(content)
	if err != nil {
		t.Fatal(err)
	}
	if environment["TELEGRAM_PROXY"] != "http://evil" {
		t.Fatalf("escaped TELEGRAM_PROXY was not decoded: %#v", environment)
	}
	if _, present := environment["NO_PROXY"]; present {
		t.Fatalf("escaped UnsetEnvironment did not remove decoded key: %#v", environment)
	}
}

func TestSystemdLogicalLinesIgnoreCommentsInsideContinuation(t *testing.T) {
	content := "[Service]\n" +
		"Environment=\"VALUE=one\\\n" +
		"# first ignored comment\n" +
		"  ; second ignored comment\n" +
		"\t# ignored comment with backslash\\\n" +
		"two\"\n" +
		"; standalone comment with backslash\\\n" +
		"Environment=FINAL=yes\n"
	environment, err := effectiveServiceEnvironment(content)
	if err != nil {
		t.Fatal(err)
	}
	if environment["VALUE"] != "one two" || environment["FINAL"] != "yes" {
		t.Fatalf("continuation comments changed logical directives: %#v", environment)
	}
}

func TestSystemdLogicalLinesRequireBackslashAsFinalByte(t *testing.T) {
	content := "[Service]\nEnvironment=VALUE=one\\  \nEnvironment=NEXT=two"
	got := systemdLogicalLines(content)
	want := []string{"[Service]", "Environment=VALUE=one\\  ", "Environment=NEXT=two"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("logical lines=%q want=%q", got, want)
	}
}

func TestHermesGatewayArgvRejectsWrappers(t *testing.T) {
	accepted := [][]string{
		{"/root/.hermes/hermes-agent/venv/bin/python", "-m", "hermes_cli.main", "gateway", "run"},
		{"-/root/.hermes/hermes-agent/venv/bin/python3.12", "-m", "hermes_cli", "gateway", "run"},
		{"/root/.hermes/hermes-agent/venv/bin/hermes_cli", "gateway", "run"},
	}
	for _, argv := range accepted {
		if !hermesGatewayArgv(argv) {
			t.Errorf("valid direct Hermes argv rejected: %q", argv)
		}
	}

	rejected := [][]string{
		{"/usr/sbin/chroot", "/srv/hermes-root", "/root/.hermes/hermes-agent/venv/bin/python", "-m", "hermes_cli.main", "gateway", "run"},
		{"/usr/bin/env", "python", "-m", "hermes_cli.main", "gateway", "run"},
		{"/usr/bin/wrapper", "-m", "hermes_cli.main", "gateway", "run"},
		{"/usr/bin/python", "-I", "-m", "hermes_cli.main", "gateway", "run"},
		{"+/usr/bin/python", "-m", "hermes_cli.main", "gateway", "run"},
		{"--/usr/bin/python", "-m", "hermes_cli.main", "gateway", "run"},
		{"/usr/bin/python-helper", "-m", "hermes_cli.main", "gateway", "run"},
		{"/usr/bin/PYTHON", "-m", "hermes_cli.main", "gateway", "run"},
		{"/usr/bin/python3.", "-m", "hermes_cli.main", "gateway", "run"},
		{"/usr/bin/python", "-m", "HERMES_CLI.main", "gateway", "run"},
	}
	for _, argv := range rejected {
		if hermesGatewayArgv(argv) {
			t.Errorf("wrapped or ambiguous Hermes argv accepted: %q", argv)
		}
	}
}

func TestDiscoverEffectiveUnitHonorsHigherPriorityDropInReset(t *testing.T) {
	high := t.TempDir()
	low := t.TempDir()
	service := "renamed-openclaw.service"
	base := "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\n"
	if err := os.WriteFile(filepath.Join(low, service), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	dropInDir := filepath.Join(high, service+".d")
	if err := os.MkdirAll(dropInDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dropInDir, "90-reset.conf"), []byte("[Service]\nEnvironment=\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := discoverEffectiveTelegramUnits([]unitSearchRoot{{Path: high, Manage: true}, {Path: low, Manage: true}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("higher-priority drop-in reset must suppress discovery: %v", got)
	}
}

func TestDiscoverEffectiveUnitHonorsHigherPriorityMask(t *testing.T) {
	high := t.TempDir()
	low := t.TempDir()
	service := "masked-openclaw.service"
	base := "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\n"
	if err := os.WriteFile(filepath.Join(low, service), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/null", filepath.Join(high, service)); err != nil {
		t.Fatal(err)
	}
	got, err := discoverEffectiveTelegramUnits([]unitSearchRoot{{Path: high, Manage: true}, {Path: low, Manage: true}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("higher-priority mask must suppress vendor unit: %v", got)
	}
}

func TestDiscoverEffectiveUnitHonorsExecStartDropInReset(t *testing.T) {
	high := t.TempDir()
	low := t.TempDir()
	service := "hermes-gateway-profile.service"
	base := "[Service]\nExecStart=/usr/bin/python -m hermes_cli.main gateway run\n"
	if err := os.WriteFile(filepath.Join(low, service), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	dropInDir := filepath.Join(high, service+".d")
	if err := os.MkdirAll(dropInDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dropInDir, "90-command.conf"), []byte("[Service]\nExecStart=\nExecStart=/usr/bin/true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := discoverEffectiveTelegramUnits([]unitSearchRoot{{Path: high, Manage: true}, {Path: low, Manage: true}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("effective non-Hermes ExecStart must suppress discovery: %v", got)
	}
}

func TestDiscoverEffectiveUnitDeduplicatesAliasesByCanonicalIdentity(t *testing.T) {
	root := t.TempDir()
	canonical := "z-openclaw.service"
	base := "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\n"
	writeTestUnitFile(t, filepath.Join(root, canonical), base)
	for _, alias := range []string{"a-openclaw.service", "m-openclaw.service"} {
		if err := os.Symlink(canonical, filepath.Join(root, alias)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := discoverEffectiveTelegramUnits([]unitSearchRoot{{Path: root, Manage: true}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != canonical {
		t.Fatalf("canonical unit and aliases must be discovered once as %s: %v", canonical, got)
	}
}

func TestDiscoverEffectiveUnitKeepsDistinctTemplateInstances(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "units")
	fragment := filepath.Join(base, "fragments", "openclaw-worker@.service")
	content := "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\n"
	writeTestUnitFile(t, fragment, content)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, service := range []string{"openclaw-alias@blue.service", "openclaw-alias@red.service"} {
		if err := os.Symlink(fragment, filepath.Join(root, service)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := discoverEffectiveTelegramUnits([]unitSearchRoot{{Path: root, Manage: true}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "openclaw-alias@blue.service" || got[1] != "openclaw-alias@red.service" {
		t.Fatalf("distinct template instances sharing one fragment must remain separate: %v", got)
	}
}

func writeTestUnitFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeTestUnitDropIn(t *testing.T, root, unit, name, content string) {
	t.Helper()
	writeTestUnitFile(t, filepath.Join(root, unit+".d", name), content)
}

func readTestEffectiveUnit(t *testing.T, roots []unitSearchRoot, service string) string {
	t.Helper()
	paths := make([]string, 0, len(roots))
	for _, root := range roots {
		paths = append(paths, root.Path)
	}
	path, ok := effectiveUnitPathInRoots(service, paths)
	if !ok {
		t.Fatalf("unit %s was not resolved", service)
	}
	content, err := readTelegramUnitContentWithDropIns(path, service, roots)
	if err != nil {
		t.Fatalf("read effective unit %s: %v", service, err)
	}
	return content
}

func TestEffectiveUnitAliasLoadsCanonicalAndAllAliasDropIns(t *testing.T) {
	root := t.TempDir()
	roots := []unitSearchRoot{{Path: root, Manage: true}}
	canonical := "openclaw-canonical.service"
	firstAlias := "openclaw-first-alias.service"
	secondAlias := "openclaw-second-alias.service"
	base := "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\n"
	writeTestUnitFile(t, filepath.Join(root, canonical), base)
	for _, alias := range []string{firstAlias, secondAlias} {
		if err := os.Symlink(canonical, filepath.Join(root, alias)); err != nil {
			t.Fatal(err)
		}
	}

	// Canonical wins when an alias has a conflicting file with the same basename.
	writeTestUnitDropIn(t, root, canonical, "50-identity.conf", "[Service]\nEnvironment=\n")
	writeTestUnitDropIn(t, root, firstAlias, "50-identity.conf", "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\n")
	if unitHasOpenClawGatewayMarker(readTestEffectiveUnit(t, roots, firstAlias)) {
		t.Fatal("same-name alias drop-in overrode the canonical drop-in")
	}

	// A differently named drop-in from another alias participates in the global
	// lexical order and may intentionally establish the final value.
	writeTestUnitDropIn(t, root, secondAlias, "60-identity.conf", "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\n")
	if !unitHasOpenClawGatewayMarker(readTestEffectiveUnit(t, roots, canonical)) {
		t.Fatal("drop-in from a second alias was not loaded for the canonical unit")
	}
}

func TestEffectiveCanonicalDropInIdentityBeatsHigherRootAlias(t *testing.T) {
	high := t.TempDir()
	low := t.TempDir()
	roots := []unitSearchRoot{{Path: high, Manage: true}, {Path: low, Manage: true}}
	canonical := "openclaw-canonical-order.service"
	alias := "openclaw-alias-order.service"
	base := "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\n"
	writeTestUnitFile(t, filepath.Join(low, canonical), base)
	if err := os.Symlink(filepath.Join(low, canonical), filepath.Join(high, alias)); err != nil {
		t.Fatal(err)
	}
	writeTestUnitDropIn(t, low, canonical, "50-identity.conf", "[Service]\nEnvironment=\n")
	writeTestUnitDropIn(t, high, alias, "50-identity.conf", "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\n")

	// Matches systemd's unit_file_find_dropin_paths(): the canonical unit id is
	// searched across every root before alias names. Root priority only decides
	// between directories for the same unit identity/hierarchy.
	if unitHasOpenClawGatewayMarker(readTestEffectiveUnit(t, roots, alias)) {
		t.Fatal("higher-root alias drop-in overrode the canonical same-name drop-in")
	}
}

func TestEffectiveAliasResolvesCanonicalByUnitSearchPrecedence(t *testing.T) {
	high := t.TempDir()
	low := t.TempDir()
	roots := []unitSearchRoot{{Path: high, Manage: true}, {Path: low, Manage: true}}
	canonical := "openclaw-priority.service"
	alias := "openclaw-priority-alias.service"
	writeTestUnitFile(t, filepath.Join(high, canonical), "[Service]\nExecStart=/usr/bin/true\n")
	writeTestUnitFile(t, filepath.Join(low, canonical), "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\n")
	if err := os.Symlink(filepath.Join(low, canonical), filepath.Join(high, alias)); err != nil {
		t.Fatal(err)
	}

	paths := []string{high, low}
	path, ok := effectiveUnitPathInRoots(alias, paths)
	if !ok || filepath.Clean(path) != filepath.Join(high, canonical) {
		t.Fatalf("alias resolved to %q, want high-priority canonical fragment", path)
	}
	if unitHasOpenClawGatewayMarker(readTestEffectiveUnit(t, roots, alias)) {
		t.Fatal("alias followed its literal low-priority target instead of the effective canonical unit")
	}
	discovered, err := discoverEffectiveTelegramUnits(roots, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(discovered) != 0 {
		t.Fatalf("discovery used the low-priority alias target: %v", discovered)
	}
}

func TestEffectiveSameNameVendorSymlinkResolvesFragment(t *testing.T) {
	high := t.TempDir()
	low := t.TempDir()
	service := "openclaw-vendor-link.service"
	writeTestUnitFile(t, filepath.Join(low, service), "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\n")
	if err := os.Symlink(filepath.Join(low, service), filepath.Join(high, service)); err != nil {
		t.Fatal(err)
	}
	path, ok := effectiveUnitPathInRoots(service, []string{high, low})
	if !ok || filepath.Clean(path) != filepath.Join(low, service) {
		t.Fatalf("same-name vendor symlink resolved to %q, ok=%v", path, ok)
	}
	content := readTestEffectiveUnit(t, []unitSearchRoot{{Path: high, Manage: true}, {Path: low, Manage: true}}, service)
	if !unitHasOpenClawGatewayMarker(content) {
		t.Fatal("same-name vendor symlink lost its fragment content")
	}
}

func TestEffectiveTemplateInstanceAndTemplateAliasDropIns(t *testing.T) {
	root := t.TempDir()
	roots := []unitSearchRoot{{Path: root, Manage: true}}
	canonicalTemplate := "openclaw-worker@.service"
	aliasTemplate := "openclaw-runner@.service"
	writeTestUnitFile(t, filepath.Join(root, canonicalTemplate), "[Service]\nExecStart=/usr/bin/node worker.js\n")
	if err := os.Symlink(canonicalTemplate, filepath.Join(root, aliasTemplate)); err != nil {
		t.Fatal(err)
	}
	writeTestUnitDropIn(t, root, canonicalTemplate, "40-identity.conf", "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\n")

	if !unitHasOpenClawGatewayMarker(readTestEffectiveUnit(t, roots, "openclaw-runner@red.service")) {
		t.Fatal("template drop-in was not inherited through a template alias instance")
	}
	writeTestUnitDropIn(t, root, "openclaw-runner@blue.service", "50-identity.conf", "[Service]\nEnvironment=\n")
	if unitHasOpenClawGatewayMarker(readTestEffectiveUnit(t, roots, "openclaw-worker@blue.service")) {
		t.Fatal("alias instance drop-in was not loaded for the canonical instance")
	}

	// An instance-level mask suppresses template fallback.
	if err := os.Symlink("/dev/null", filepath.Join(root, "openclaw-worker@masked.service")); err != nil {
		t.Fatal(err)
	}
	if path, ok := effectiveUnitPathInRoots("openclaw-worker@masked.service", []string{root}); ok || path != "" {
		t.Fatalf("masked instance fell back to its template: path=%q ok=%v", path, ok)
	}
}

func TestEffectiveTemplateInstanceSameNamePrecedence(t *testing.T) {
	t.Run("root priority within canonical hierarchy", func(t *testing.T) {
		high := t.TempDir()
		low := t.TempDir()
		roots := []unitSearchRoot{{Path: high, Manage: true}, {Path: low, Manage: true}}
		template := "openclaw-worker@.service"
		instance := "openclaw-worker@blue.service"
		writeTestUnitFile(t, filepath.Join(low, template), "[Service]\nExecStart=/usr/bin/true\n")
		writeTestUnitDropIn(t, high, template, "50-identity.conf", "[Service]\nEnvironment=\n")
		writeTestUnitDropIn(t, low, instance, "50-identity.conf", "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\n")
		if unitHasOpenClawGatewayMarker(readTestEffectiveUnit(t, roots, instance)) {
			t.Fatal("lower-root instance drop-in overrode a higher-root template file with the same basename")
		}
	})

	t.Run("instance specificity within one root", func(t *testing.T) {
		root := t.TempDir()
		roots := []unitSearchRoot{{Path: root, Manage: true}}
		template := "openclaw-worker@.service"
		instance := "openclaw-worker@blue.service"
		writeTestUnitFile(t, filepath.Join(root, template), "[Service]\nExecStart=/usr/bin/true\n")
		writeTestUnitDropIn(t, root, template, "50-identity.conf", "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\n")
		writeTestUnitDropIn(t, root, instance, "50-identity.conf", "[Service]\nEnvironment=\n")
		if unitHasOpenClawGatewayMarker(readTestEffectiveUnit(t, roots, instance)) {
			t.Fatal("template drop-in overrode the instance file with the same basename")
		}
	})
}

func TestEffectiveDashPrefixAndTypeWideDropIns(t *testing.T) {
	t.Run("different basenames follow global lexical order", func(t *testing.T) {
		root := t.TempDir()
		roots := []unitSearchRoot{{Path: root, Manage: true}}
		service := "hermes-gateway-profile.service"
		writeTestUnitFile(t, filepath.Join(root, service), "[Service]\nExecStart=/usr/bin/true\n")
		writeTestUnitDropIn(t, root, "service", "10-identity.conf", "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\n")
		writeTestUnitDropIn(t, root, "hermes-.service", "20-identity.conf", "[Service]\nEnvironment=\n")
		writeTestUnitDropIn(t, root, "hermes-gateway-.service", "30-identity.conf", "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\n")
		if !unitHasOpenClawGatewayMarker(readTestEffectiveUnit(t, roots, service)) {
			t.Fatal("type-wide and dash-prefix drop-ins were not parsed in basename order")
		}

		writeTestUnitDropIn(t, root, "hermes-.service", "40-conflict.conf", "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\n")
		writeTestUnitDropIn(t, root, "hermes-gateway-.service", "40-conflict.conf", "[Service]\nEnvironment=\n")
		if unitHasOpenClawGatewayMarker(readTestEffectiveUnit(t, roots, service)) {
			t.Fatal("generic dash-prefix drop-in overrode the more specific same-name drop-in")
		}
	})

	t.Run("name specificity beats type-wide root priority", func(t *testing.T) {
		high := t.TempDir()
		low := t.TempDir()
		roots := []unitSearchRoot{{Path: high, Manage: true}, {Path: low, Manage: true}}
		service := "openclaw-specific.service"
		writeTestUnitFile(t, filepath.Join(low, service), "[Service]\nExecStart=/usr/bin/true\n")
		writeTestUnitDropIn(t, high, "service", "50-identity.conf", "[Service]\nEnvironment=\n")
		writeTestUnitDropIn(t, low, service, "50-identity.conf", "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\n")
		if !unitHasOpenClawGatewayMarker(readTestEffectiveUnit(t, roots, service)) {
			t.Fatal("type-wide drop-in overrode a lower-root name-specific file with the same basename")
		}
	})

	t.Run("root priority beats dash specificity", func(t *testing.T) {
		high := t.TempDir()
		low := t.TempDir()
		roots := []unitSearchRoot{{Path: high, Manage: true}, {Path: low, Manage: true}}
		service := "openclaw-gateway-profile.service"
		writeTestUnitFile(t, filepath.Join(low, service), "[Service]\nExecStart=/usr/bin/true\n")
		writeTestUnitDropIn(t, high, "openclaw-.service", "50-identity.conf", "[Service]\nEnvironment=\n")
		writeTestUnitDropIn(t, low, service, "50-identity.conf", "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\n")
		if unitHasOpenClawGatewayMarker(readTestEffectiveUnit(t, roots, service)) {
			t.Fatal("low-root exact drop-in overrode a high-root dash-prefix file with the same basename")
		}
	})
}

func TestTelegramUnitFIFOIsRejectedWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.service")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := telegramRelatedUnit(path, "gateway.service"); err == nil || !strings.Contains(err.Error(), "普通文件") {
		t.Fatalf("FIFO should be rejected as a non-regular unit: %v", err)
	}
}

func TestDiscoverUserTelegramTargetsSkipsUnusableSystemHomes(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	symlinkHome := filepath.Join(root, "symlink-home")
	if err := os.Symlink(root, symlinkHome); err != nil {
		t.Fatal(err)
	}

	accounts := []localUserAccount{
		{Name: "root-home", Home: "/", UID: "1"},
		{Name: "missing-home", Home: filepath.Join(root, "missing"), UID: "2"},
		{Name: "symlink-home", Home: symlinkHome, UID: "3"},
	}
	got, err := discoverUserTelegramTargetNamesFor(accounts)
	if err != nil {
		t.Fatalf("不可管理的系统账户家目录应被跳过，不能阻断自动发现：%v", err)
	}
	if len(got) != 0 {
		t.Fatalf("不可管理的系统账户不应产生 Telegram 目标：%v", got)
	}
}

func TestUserUnitRuntimeSearchPrecedence(t *testing.T) {
	roots := userUnitSearchRootSpecsFor("/home/alice", "1001")
	paths := make([]string, 0, len(roots))
	for _, root := range roots {
		paths = append(paths, root.Path)
	}
	joined := strings.Join(paths, "\n")
	for _, want := range []string{
		"/home/alice/.config/systemd/user.control",
		"/run/user/1001/systemd/user.control",
		"/run/user/1001/systemd/transient",
		"/run/user/1001/systemd/generator.early",
		"/run/user/1001/systemd/user",
		"/run/user/1001/systemd/generator",
		"/run/user/1001/systemd/generator.late",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing user runtime load path %s:\n%s", want, joined)
		}
	}
}
