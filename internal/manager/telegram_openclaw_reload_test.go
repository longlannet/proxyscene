package manager

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func reloadBool(value bool) *bool { return &value }

func readyReloadAccount(start int64) openClawReloadAccount {
	return openClawReloadAccount{AccountID: "default", Enabled: reloadBool(true), Configured: reloadBool(true), Running: reloadBool(true), Connected: reloadBool(true), Lifecycle: "ready", LastStartAt: start}
}

func reloadChannels(accounts ...openClawReloadAccount) openClawReloadChannels {
	return openClawReloadChannels{Accounts: map[string][]openClawReloadAccount{"telegram": accounts}}
}

func TestOpenClawReloadRequiresFreshReadyAccounts(t *testing.T) {
	before := map[string]openClawReloadAccount{"default": readyReloadAccount(100)}
	for _, tc := range []struct {
		name   string
		change func(*openClawReloadChannels)
		want   bool
	}{
		{"new ready account", func(*openClawReloadChannels) {}, true},
		{"old ready state", func(s *openClawReloadChannels) { s.Accounts["telegram"][0].LastStartAt = 100 }, false},
		{"starting", func(s *openClawReloadChannels) { s.Accounts["telegram"][0].Lifecycle = "starting" }, false},
		{"disconnected", func(s *openClawReloadChannels) { s.Accounts["telegram"][0].Connected = reloadBool(false) }, false},
		{"missing connected", func(s *openClawReloadChannels) { s.Accounts["telegram"][0].Connected = nil }, false},
		{"restart pending", func(s *openClawReloadChannels) { s.Accounts["telegram"][0].RestartPending = true }, false},
		{"channel error", func(s *openClawReloadChannels) { value := "failure"; s.Accounts["telegram"][0].LastError = &value }, false},
		{"partial snapshot", func(s *openClawReloadChannels) { s.Partial = true }, false},
		{"reload warning", func(s *openClawReloadChannels) {
			s.Warnings = []json.RawMessage{json.RawMessage(`"reload in progress"`)}
		}, false},
		{"different account", func(s *openClawReloadChannels) { s.Accounts["telegram"][0].AccountID = "new" }, false},
		{"missing account", func(s *openClawReloadChannels) { s.Accounts["telegram"] = nil }, false},
		{"duplicated account", func(s *openClawReloadChannels) {
			s.Accounts["telegram"] = append(s.Accounts["telegram"], readyReloadAccount(200))
		}, false},
		{"concurrently disabled", func(s *openClawReloadChannels) { s.Accounts["telegram"][0].Enabled = reloadBool(false) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := reloadChannels(readyReloadAccount(200))
			tc.change(&snapshot)
			if got := openClawReloadAccountsReady(before, snapshot); got != tc.want {
				t.Fatalf("ready = %v, want %v", got, tc.want)
			}
		})
	}
	disabled := readyReloadAccount(0)
	disabled.Enabled = reloadBool(false)
	disabled.Running = reloadBool(false)
	if !openClawReloadAccountsReady(map[string]openClawReloadAccount{"default": disabled}, reloadChannels(disabled)) {
		t.Fatal("unchanged stopped disabled account requires unnecessary restart")
	}
	disabled.Running = reloadBool(true)
	if openClawReloadAccountsReady(map[string]openClawReloadAccount{"default": disabled}, reloadChannels(disabled)) {
		t.Fatal("running disabled account accepted")
	}
}

func TestOpenClawReloadProxyAcknowledgement(t *testing.T) {
	const expected = `{"channels":{"telegram":{"proxy":"http://127.0.0.1:7890"}}}`
	for _, observed := range []string{`{}`, `{"channels":{"telegram":{"proxy":"http://127.0.0.1:9999"}}}`, `{"channels":{"telegram":{"proxy":"http://127.0.0.1:7890","accounts":{"work":{"proxy":"http://elsewhere"}}}}}`} {
		if openClawReloadProxyEqual([]byte(expected), []byte(observed)) {
			t.Fatal("wrong or partially overridden proxy accepted")
		}
	}
	if !openClawReloadProxyEqual([]byte(expected), []byte(expected)) {
		t.Fatal("matching proxy rejected")
	}
	if !openClawReloadProxyEqual([]byte(`{channels:{telegram:{}}}`), []byte(`{}`)) {
		t.Fatal("restored absent proxy rejected")
	}
	if openClawReloadProxyEqual([]byte(`{}`), []byte(`{"channels":{"telegram":{"proxy":null}}}`)) {
		t.Fatal("absent and explicit null confused")
	}
	config := openClawReloadConfig{Path: "/home/test/.openclaw/openclaw.json", Valid: true, ConfigRevisionHash: "new", AppliedConfigHash: "old", Config: json.RawMessage(expected)}
	if openClawReloadConfigApplied(config, config.Path) {
		t.Fatal("pending revision accepted")
	}
	config.AppliedConfigHash = "new"
	if !openClawReloadConfigApplied(config, config.Path) {
		t.Fatal("applied generation rejected")
	}
	if openClawReloadConfigApplied(config, "/other/config") {
		t.Fatal("another config path accepted")
	}
}

func TestOpenClawReloadCommandBounds(t *testing.T) {
	argv := []string{"/usr/bin/node", "/opt/openclaw/dist/index.js", "gateway", "--port", "18799"}
	command, port, err := openClawReloadCommand(argv, []byte(`{gateway:{port:18789}}`))
	if err != nil || port != 18799 || len(command) != 2 || command[1] != argv[1] {
		t.Fatalf("command=%v port=%d error=%v", command, port, err)
	}
	for _, cfg := range []string{`{gateway:{reload:{mode:'off'}}}`, `{gateway:{mode:'remote'}}`, `{gateway:{tls:{enabled:true}}}`} {
		if _, _, err := openClawReloadCommand(argv, []byte(cfg)); err == nil {
			t.Fatalf("unsupported config accepted: %s", cfg)
		}
	}
	for _, tail := range [][]string{{"--token", "secret"}, {"--port", "0"}, {"--port=65536"}, {"--port"}, {"--bind", "lan"}} {
		candidate := append(append([]string(nil), argv[:3]...), tail...)
		if _, _, err := openClawReloadCommand(candidate, []byte(`{}`)); err == nil {
			t.Fatalf("unsupported args accepted: %v", tail)
		}
	}
}

func TestOpenClawReloadStateRequiresInvocation(t *testing.T) {
	for _, raw := range []string{"ActiveState=active\n", "ActiveState=activating\n", "ActiveState=active\nInvocationID=oops\n", "ActiveState=inactive\nActiveState=active\n"} {
		if _, _, err := parseOpenClawReloadUnitState(raw); err == nil {
			t.Fatalf("ambiguous state accepted: %q", raw)
		}
	}
	if state, _, err := parseOpenClawReloadUnitState("ActiveState=inactive\nInvocationID=\n"); err != nil || state != "inactive" {
		t.Fatal("inactive service rejected")
	}
}

func setupOpenClawReloadFixture(t *testing.T, initial string) (*openClawTestHarness, systemdTargetName, *openClawTelegramReloadPlan, *bool) {
	t.Helper()
	h := newOpenClawTestHarness(t, initial)
	target := h.target("openclaw-gateway.service")
	unitPath := filepath.Join(h.home, ".config", "systemd", "user", target.Service)
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unitPath, []byte("[Service]\nExecStart=/usr/bin/node /opt/openclaw/dist/index.js gateway --port 18789\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldState, oldRPC, oldTimeout, oldPoll := openClawReloadUnitState, openClawReloadRPC, openClawReloadTimeout, openClawReloadPollInterval
	t.Cleanup(func() {
		openClawReloadUnitState, openClawReloadRPC, openClawReloadTimeout, openClawReloadPollInterval = oldState, oldRPC, oldTimeout, oldPoll
	})
	openClawReloadTimeout, openClawReloadPollInterval = 8*time.Millisecond, time.Millisecond
	openClawReloadUnitState = func(systemdTargetName, *persistedUserIdentity) (string, error) {
		return "ActiveState=active\nInvocationID=0123456789abcdef0123456789abcdef\n", nil
	}
	changed := false
	openClawReloadRPC = func(_ context.Context, _ systemdTargetName, _ *openClawTelegramReloadPlan, method string, result any) error {
		switch method {
		case "config.get":
			hash := "before"
			if changed {
				hash = "after"
			}
			*result.(*openClawReloadConfig) = openClawReloadConfig{Path: h.configPath, Valid: true, ConfigRevisionHash: hash, AppliedConfigHash: hash, Config: h.config(t)}
		case "channels.status":
			start := int64(100)
			if changed {
				start = 200
			}
			*result.(*openClawReloadChannels) = reloadChannels(readyReloadAccount(start))
		default:
			t.Fatalf("unexpected method %s", method)
		}
		return nil
	}
	plan := h.app.prepareOpenClawTelegramReload(target)
	if plan.beforeHash != "before" || plan.reason != "" {
		t.Fatalf("prepare failed: %s", plan.reason)
	}
	return h, target, plan, &changed
}

func TestOpenClawReloadApplyAndRestoreAcknowledged(t *testing.T) {
	h, target, plan, changed := setupOpenClawReloadFixture(t, `{"channels":{"telegram":{"botToken":"test"}}}`)
	if _, _, err := h.app.applyOpenClawTelegramProxy(target, "http://127.0.0.1:7890"); err != nil {
		t.Fatal(err)
	}
	*changed = true
	if handled, err := h.app.finishOpenClawTelegramReload(target, plan); err != nil || !handled {
		t.Fatalf("handled=%v error=%v", handled, err)
	}
	if err := h.app.commitOpenClawTelegramProxyApply(target); err != nil {
		t.Fatal(err)
	}
	// Restore is bound to the journal's original absence, not a hardcoded URL.
	*changed = false
	plan = h.app.prepareOpenClawTelegramReload(target)
	if _, err := h.app.prepareRestoreOpenClawTelegramProxy(target); err != nil {
		t.Fatal(err)
	}
	*changed = true
	if handled, err := h.app.finishOpenClawTelegramReload(target, plan); err != nil || !handled {
		t.Fatalf("restore handled=%v error=%v", handled, err)
	}
}

func TestOpenClawReloadRejectsStaleOrChangedEvidence(t *testing.T) {
	for _, mode := range []string{"stale hash", "stale channel", "different proxy", "different invocation", "rpc unavailable", "concurrent edit", "identity drift"} {
		t.Run(mode, func(t *testing.T) {
			h, target, plan, changed := setupOpenClawReloadFixture(t, `{"channels":{"telegram":{"botToken":"test"}}}`)
			if _, _, err := h.app.applyOpenClawTelegramProxy(target, "http://127.0.0.1:7890"); err != nil {
				t.Fatal(err)
			}
			*changed = true
			baseRPC := openClawReloadRPC
			openClawReloadRPC = func(ctx context.Context, target systemdTargetName, plan *openClawTelegramReloadPlan, method string, result any) error {
				if mode == "rpc unavailable" {
					return errors.New("unavailable")
				}
				if err := baseRPC(ctx, target, plan, method, result); err != nil {
					return err
				}
				if method == "config.get" {
					cfg := result.(*openClawReloadConfig)
					if mode == "stale hash" {
						cfg.ConfigRevisionHash, cfg.AppliedConfigHash = "before", "before"
					}
					if mode == "different proxy" {
						cfg.Config = json.RawMessage(`{"channels":{"telegram":{"proxy":"http://other"}}}`)
					}
				}
				if method == "channels.status" {
					if mode == "stale channel" {
						*result.(*openClawReloadChannels) = reloadChannels(readyReloadAccount(100))
					}
					if mode == "concurrent edit" {
						if err := os.WriteFile(h.configPath, append(h.config(t), '\n'), 0o600); err != nil {
							t.Fatal(err)
						}
					}
				}
				return nil
			}
			if mode == "different invocation" {
				openClawReloadUnitState = func(systemdTargetName, *persistedUserIdentity) (string, error) {
					return "ActiveState=active\nInvocationID=abcdef0123456789abcdef0123456789\n", nil
				}
			}
			if mode == "identity drift" {
				h.identity.UID++
			}
			handled, err := h.app.finishOpenClawTelegramReload(target, plan)
			if handled {
				t.Fatal("unconfirmed reload accepted")
			}
			wantErr := mode == "concurrent edit" || mode == "identity drift"
			if (err != nil) != wantErr {
				t.Fatalf("error=%v, want error=%v", err, wantErr)
			}
			if h.journal(t).Users[h.user].Phase != openClawPhasePrepared {
				t.Fatal("failed reload committed journal")
			}
		})
	}
}

func TestOpenClawReloadInactiveNeverCallsRPC(t *testing.T) {
	h := newOpenClawTestHarness(t, `{"channels":{"telegram":{"botToken":"test"}}}`)
	target := h.target("openclaw-gateway.service")
	oldState, oldRPC := openClawReloadUnitState, openClawReloadRPC
	t.Cleanup(func() { openClawReloadUnitState, openClawReloadRPC = oldState, oldRPC })
	openClawReloadUnitState = func(systemdTargetName, *persistedUserIdentity) (string, error) {
		return "ActiveState=inactive\nInvocationID=\n", nil
	}
	openClawReloadRPC = func(context.Context, systemdTargetName, *openClawTelegramReloadPlan, string, any) error {
		t.Fatal("inactive gateway queried")
		return nil
	}
	plan := h.app.prepareOpenClawTelegramReload(target)
	if !plan.inactive {
		t.Fatal("inactive state not recorded")
	}
	if _, _, err := h.app.applyOpenClawTelegramProxy(target, "http://127.0.0.1:7890"); err != nil {
		t.Fatal(err)
	}
	if handled, err := h.app.finishOpenClawTelegramReload(target, plan); err != nil || !handled {
		t.Fatalf("handled=%v error=%v", handled, err)
	}
}

func TestOpenClawReloadOutputBoundedAndRootPathGuarded(t *testing.T) {
	var output openClawReloadBoundedOutput
	if _, err := output.Write([]byte(strings.Repeat("x", openClawReloadOutputLimit))); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Write([]byte("x")); err == nil || !output.exceeded || output.Len() != openClawReloadOutputLimit {
		t.Fatal("output limit not enforced")
	}
	var copied openClawReloadBoundedOutput
	if _, err := io.Copy(&copied, strings.NewReader(strings.Repeat("x", openClawReloadOutputLimit+1))); err == nil || !copied.exceeded || copied.Len() > openClawReloadOutputLimit {
		t.Fatal("io.Copy bypassed the output bound")
	}
	path := filepath.Join(t.TempDir(), "entry.mjs")
	if err := os.WriteFile(path, []byte("// fixture"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if validateOpenClawReloadRootPath(path) == nil {
		t.Fatal("writable entry accepted")
	}
	link := filepath.Join(filepath.Dir(path), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if validateOpenClawReloadRootPath(link) == nil {
		t.Fatal("symlink entry accepted")
	}
}

func TestOpenClawReloadPreservesAdministratorProxyOnRestore(t *testing.T) {
	h, target, _, changed := setupOpenClawReloadFixture(t, `{"channels":{"telegram":{"botToken":"test"}}}`)
	h.applyAndCommit(t, target, "http://127.0.0.1:7890")
	admin := []byte(`{"channels":{"telegram":{"botToken":"test","proxy":"http://127.0.0.1:9999"}}}`)
	if err := os.WriteFile(h.configPath, admin, 0o600); err != nil {
		t.Fatal(err)
	}
	*changed = false
	plan := h.app.prepareOpenClawTelegramReload(target)
	if _, err := h.app.prepareRestoreOpenClawTelegramProxy(target); err != nil {
		t.Fatal(err)
	}
	// The source was already loaded: an unchanged generation uses the conservative
	// fallback, without rejecting the administrator's desired final value.
	if handled, err := h.app.finishOpenClawTelegramReload(target, plan); err != nil || handled {
		t.Fatalf("handled=%v error=%v", handled, err)
	}
	if string(h.config(t)) != string(admin) {
		t.Fatal("administrator proxy changed")
	}
	if err := h.app.commitOpenClawTelegramProxyRestore(target); err != nil {
		t.Fatal(err)
	}
	if h.journal(t).Users[h.user] != nil {
		t.Fatal("restored ownership retained")
	}
}

func TestOpenClawReloadRPCUsesRecordedUnprivilegedIdentity(t *testing.T) {
	home, err := os.MkdirTemp("", "proxyscene-reload-rpc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatal(err)
	}
	uid, gid := os.Geteuid(), os.Getegid()
	if uid == 0 {
		uid, gid = 65534, 65534
	}
	identity := persistedUserIdentity{UID: uid, GID: gid, Home: home}
	oldLookup, oldValidate, oldRead := openClawLookupUserIdentity, openClawValidateTargetRuntime, openClawReadUserConfig
	t.Cleanup(func() {
		openClawLookupUserIdentity, openClawValidateTargetRuntime, openClawReadUserConfig = oldLookup, oldValidate, oldRead
	})
	openClawReadUserConfig = func(_ string, path string, max int64) ([]byte, error) { return readRegularFileNoFollow(path, max) }
	openClawValidateTargetRuntime = func(systemdTargetName, *persistedUserIdentity) error { return nil }
	if err := os.Mkdir(filepath.Join(home, ".openclaw"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".openclaw", "openclaw.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		for _, path := range []string{home, filepath.Join(home, ".openclaw"), filepath.Join(home, ".openclaw", "openclaw.json")} {
			if err := os.Chown(path, uid, gid); err != nil {
				t.Fatal(err)
			}
		}
	}
	openClawLookupUserIdentity = func(name string) (localUserIdentity, error) {
		return localUserIdentity{Name: name, UID: uid, GID: gid, UIDText: strconv.Itoa(uid), GIDText: strconv.Itoa(gid), Home: home}, nil
	}
	script := filepath.Join(home, "node")
	contents := "#!/bin/sh\n" +
		"printf '{\"uid\":%s,\"gatewayPort\":\"%s\",\"configSelector\":\"%s\",\"nodeOptions\":\"%s\"}\\n' \"$(/usr/bin/id -u)\" \"$OPENCLAW_GATEWAY_PORT\" \"${OPENCLAW_CONFIG_PATH-unset}\" \"${NODE_OPTIONS-unset}\"\n"
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCLAW_CONFIG_PATH", "/unrelated/private/config")
	t.Setenv("NODE_OPTIONS", "--require=/unrelated/code")
	plan := &openClawTelegramReloadPlan{identity: identity, command: []string{script, "/opt/openclaw/dist/index.js"}, port: 18799}
	target := systemdTargetName{UserMode: true, User: "reload-test", Service: "openclaw-gateway.service"}
	unitPath := filepath.Join(home, ".config", "systemd", "user", target.Service)
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		t.Fatal(err)
	}
	unit := "[Service]\nExecStart=" + script + " /opt/openclaw/dist/index.js gateway --port 18799\nEnvironment=HOME=" + home + "\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\n"
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}
	var result struct {
		UID            int    `json:"uid"`
		GatewayPort    string `json:"gatewayPort"`
		ConfigSelector string `json:"configSelector"`
		NodeOptions    string `json:"nodeOptions"`
	}
	if err := runOpenClawReloadRPC(context.Background(), target, plan, "config.get", &result); err != nil {
		t.Fatal(err)
	}
	if result.UID != uid || result.GatewayPort != "18799" || result.ConfigSelector != "unset" || result.NodeOptions != "unset" {
		t.Fatalf("wrong subprocess identity/environment: %+v", result)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf secret-invalid-json\nprintf secret-token >&2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runOpenClawReloadRPC(context.Background(), target, plan, "config.get", &result); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("invalid output leaked: %v", err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec /usr/bin/sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := runOpenClawReloadRPC(ctx, target, plan, "config.get", &result); err == nil {
		t.Fatal("hung subprocess accepted")
	}
	if time.Since(started) > time.Second {
		t.Fatal("subprocess did not honor overall deadline")
	}
}

func TestOpenClawReloadPrepareRejectsSelectorsBeforeRPC(t *testing.T) {
	h, target, _, _ := setupOpenClawReloadFixture(t, `{}`)
	openClawReloadRPC = func(context.Context, systemdTargetName, *openClawTelegramReloadPlan, string, any) error {
		t.Fatal("unsupported config was executed")
		return nil
	}
	for _, cfg := range []string{`{"$include":"other.json"}`, `{"env":{"vars":{"NODE_OPTIONS":"--require=/tmp/injected"}}}`, `{"channels":{"telegram":{"accounts":{"work":{"proxy":"http://other"}}}}}`} {
		if err := os.WriteFile(h.configPath, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		plan := h.app.prepareOpenClawTelegramReload(target)
		if plan.reason == "" || plan.beforeHash != "" {
			t.Fatal("unsupported config produced usable plan")
		}
	}
}
