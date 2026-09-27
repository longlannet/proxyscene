package manager

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func parseJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, b)
	}
	return m
}

type openClawTestHarness struct {
	app             *App
	user            string
	home            string
	configPath      string
	identity        localUserIdentity
	failNextCAS     error
	failNextJournal error
	beforeNextCAS   func()
}

func newOpenClawTestHarness(t *testing.T, initialConfig string) *openClawTestHarness {
	t.Helper()
	stubTelegramPlanUnitsForLifecycle(t)
	h := &openClawTestHarness{
		app:  testApp(t),
		user: "root",
		home: t.TempDir(),
	}
	h.configPath = filepath.Join(h.home, ".openclaw", "openclaw.json")
	stubTelegramServiceState(t, telegramServiceState{LoadState: "loaded", ActiveState: "active"})
	oldRestartPolicy := telegramValidateHermesRestartPolicy
	t.Cleanup(func() { telegramValidateHermesRestartPolicy = oldRestartPolicy })
	telegramValidateHermesRestartPolicy = func(systemdTargetName, *persistedUserIdentity) error { return nil }
	oldReloadState := openClawReloadUnitState
	t.Cleanup(func() { openClawReloadUnitState = oldReloadState })
	openClawReloadUnitState = func(systemdTargetName, *persistedUserIdentity) (string, error) {
		return "", errors.New("fixture has no gateway RPC")
	}

	h.identity = localUserIdentity{Name: h.user, UID: os.Getuid(), GID: os.Getgid(), UIDText: strconv.Itoa(os.Getuid()), GIDText: strconv.Itoa(os.Getgid()), Home: h.home}
	if err := os.MkdirAll(filepath.Dir(h.configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.configPath, []byte(initialConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	oldLookup := openClawLookupUserIdentity
	oldRead := openClawReadUserConfig
	oldCAS := openClawWriteUserConfigCAS
	oldJournal := openClawWriteJournalFile
	oldRuntimeValidator := openClawValidateTargetRuntime
	t.Cleanup(func() {
		openClawLookupUserIdentity = oldLookup
		openClawReadUserConfig = oldRead
		openClawWriteUserConfigCAS = oldCAS
		openClawWriteJournalFile = oldJournal
		openClawValidateTargetRuntime = oldRuntimeValidator
	})
	openClawLookupUserIdentity = func(user string) (localUserIdentity, error) {
		if user != h.user {
			return localUserIdentity{}, errors.New("unexpected test user")
		}
		return h.identity, nil
	}
	openClawReadUserConfig = func(user, path string, max int64) ([]byte, error) {
		if user != h.user || path != h.configPath {
			return nil, errors.New("unexpected test config path")
		}
		return readRegularFileNoFollow(path, max)
	}
	openClawWriteUserConfigCAS = func(user string, _ *persistedUserIdentity, path string, expected, data []byte, perm os.FileMode) error {
		if user != h.user || path != h.configPath {
			return errors.New("unexpected test config path")
		}
		if h.beforeNextCAS != nil {
			before := h.beforeNextCAS
			h.beforeNextCAS = nil
			before()
		}
		if h.failNextCAS != nil {
			err := h.failNextCAS
			h.failNextCAS = nil
			return err
		}
		current, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(current, expected) {
			return errUserFileChanged
		}
		return os.WriteFile(path, data, perm)
	}
	openClawWriteJournalFile = func(path string, data []byte, perm os.FileMode) error {
		if h.failNextJournal != nil {
			err := h.failNextJournal
			h.failNextJournal = nil
			return err
		}
		return writeFileAtomic(path, data, perm)
	}
	openClawValidateTargetRuntime = func(systemdTargetName, *persistedUserIdentity) error { return nil }
	return h
}

func (h *openClawTestHarness) target(service string) systemdTargetName {
	return systemdTargetName{UserMode: true, User: h.user, Service: service}
}

func (h *openClawTestHarness) config(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(h.configPath)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (h *openClawTestHarness) journal(t *testing.T) *openClawProxyJournal {
	t.Helper()
	journal, err := h.app.loadOpenClawProxyJournal()
	if err != nil {
		t.Fatal(err)
	}
	return journal
}

func readOpenClawJournalCopy(t *testing.T, path string) *openClawProxyJournal {
	t.Helper()
	raw, err := readRegularFileNoFollow(path, maxOpenClawJournalBytes)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := decodeOpenClawProxyJournal(raw)
	if err != nil {
		t.Fatal(err)
	}
	return journal
}

func writeOpenClawJournalCopy(t *testing.T, path string, journal *openClawProxyJournal) {
	t.Helper()
	raw, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (h *openClawTestHarness) applyAndCommit(t *testing.T, target systemdTargetName, proxy string) {
	t.Helper()
	managed, changed, err := h.app.applyOpenClawTelegramProxy(target, proxy)
	if err != nil || !managed || !changed {
		t.Fatalf("apply: managed=%v changed=%v err=%v", managed, changed, err)
	}
	if err := h.app.commitOpenClawTelegramProxyApply(target); err != nil {
		t.Fatalf("commit apply: %v", err)
	}
}

func TestSetOpenClawTelegramProxyPreservesOtherKeys(t *testing.T) {
	in := []byte(`{"channels":{"telegram":{"botToken":"SECRET-TOKEN","enabled":true}},"models":{"x":1}}`)
	out, changed, err := setOpenClawTelegramProxy(in, "http://127.0.0.1:7892")
	if err != nil || !changed {
		t.Fatalf("setOpenClawTelegramProxy: changed=%v err=%v", changed, err)
	}
	tg := parseJSON(t, out)["channels"].(map[string]any)["telegram"].(map[string]any)
	if tg["proxy"] != "http://127.0.0.1:7892" {
		t.Fatalf("proxy=%v, want http://127.0.0.1:7892", tg["proxy"])
	}
	// 关键：机密与其它字段必须原样保留。
	if tg["botToken"] != "SECRET-TOKEN" || tg["enabled"] != true {
		t.Fatalf("telegram 其它字段被改：%+v", tg)
	}
	if _, ok := parseJSON(t, out)["models"]; !ok {
		t.Fatalf("顶层 models 字段丢失")
	}
}

func TestSetOpenClawTelegramProxyIdempotent(t *testing.T) {
	in := []byte(`{"channels":{"telegram":{"proxy":"http://127.0.0.1:7892","botToken":"x"}}}`)
	out, changed, err := setOpenClawTelegramProxy(in, "http://127.0.0.1:7892")
	if err != nil || changed {
		t.Fatalf("已是同值应 changed=false：changed=%v err=%v", changed, err)
	}
	if !bytes.Equal(out, in) {
		t.Fatalf("同值时不应改写内容")
	}
}

func TestSetOpenClawTelegramProxyOverwritesExisting(t *testing.T) {
	in := []byte(`{"channels":{"telegram":{"proxy":"socks5://1.2.3.4:1080","botToken":"x"}}}`)
	out, changed, err := setOpenClawTelegramProxy(in, "http://127.0.0.1:7892")
	if err != nil || !changed {
		t.Fatalf("不同值应 changed=true：changed=%v err=%v", changed, err)
	}
	tg := parseJSON(t, out)["channels"].(map[string]any)["telegram"].(map[string]any)
	if tg["proxy"] != "http://127.0.0.1:7892" {
		t.Fatalf("proxy 未更新：%v", tg["proxy"])
	}
}

func TestSetOpenClawTelegramProxyPreservesJSON5CommentsAndFormatting(t *testing.T) {
	in := []byte("{\n" +
		"  // keep this top-level comment\n" +
		"  channels: {\n" +
		"    telegram: {\n" +
		"      botToken: 'SECRET-TOKEN', // keep this secret and comment\n" +
		"      enabled: true,\n" +
		"    },\n" +
		"  },\n" +
		"  counter: 0xdecaf,\n" +
		"}\n")
	out, changed, err := setOpenClawTelegramProxy(in, "http://127.0.0.1:7892")
	if err != nil || !changed {
		t.Fatalf("set JSON5 proxy: changed=%v err=%v", changed, err)
	}
	for _, preserved := range []string{
		"// keep this top-level comment",
		"botToken: 'SECRET-TOKEN', // keep this secret and comment",
		"counter: 0xdecaf,",
	} {
		if !bytes.Contains(out, []byte(preserved)) {
			t.Fatalf("JSON5 source fragment was not preserved: %q\n%s", preserved, out)
		}
	}
	value, present, err := openClawTelegramProxyRawValue(out)
	if err != nil || !proxyStateMatchesString(value, present, "http://127.0.0.1:7892") {
		t.Fatalf("JSON5 proxy not injected: value=%s present=%v err=%v", value, present, err)
	}
	restored, changed, err := replaceOpenClawTelegramProxyRawWithStructure(out, nil, false, true, true, true)
	if err != nil || !changed {
		t.Fatalf("restore JSON5 proxy: changed=%v err=%v", changed, err)
	}
	if !bytes.Equal(restored, in) {
		t.Fatalf("JSON5 absent-proxy round trip changed source:\nwant:\n%s\ngot:\n%s", in, restored)
	}
}

func TestOpenClawJSON5JournalRoundTripPreservesOriginalLexeme(t *testing.T) {
	initial := "{\r\n" +
		"  channels: {\r\n" +
		"    telegram: {\r\n" +
		"      proxy: 'socks5://original.example:1080', // retain quote style\r\n" +
		"      botToken: 'secret',\r\n" +
		"    },\r\n" +
		"  },\r\n" +
		"}\r\n"
	h := newOpenClawTestHarness(t, initial)
	target := h.target("openclaw-json5.service")
	h.applyAndCommit(t, target, "http://127.0.0.1:7892")
	entry := h.journal(t).Users[h.user]
	if entry == nil || entry.OriginalLexeme != "'socks5://original.example:1080'" {
		t.Fatalf("JSON5 original lexeme was not journaled: %+v", entry)
	}
	if !bytes.Contains(h.config(t), []byte("// retain quote style\r\n")) {
		t.Fatal("apply removed JSON5 comment or CRLF formatting")
	}
	restart, err := h.app.prepareRestoreOpenClawTelegramProxy(target)
	if err != nil || !restart {
		t.Fatalf("prepare JSON5 restore: restart=%v err=%v", restart, err)
	}
	if err := h.app.commitOpenClawTelegramProxyRestore(target); err != nil {
		t.Fatal(err)
	}
	if got := h.config(t); !bytes.Equal(got, []byte(initial)) {
		t.Fatalf("JSON5 journal round trip changed source:\nwant:\n%s\ngot:\n%s", initial, got)
	}
}

func TestOpenClawJSON5RejectsEffectiveAccountProxyAndDuplicateManagedKeys(t *testing.T) {
	accountOverride := []byte(`{
  channels: {
    telegram: {
      accounts: {
        default: { proxy: 'http://account.invalid:8080' },
      },
    },
  },
}`)
	if _, _, err := setOpenClawTelegramProxy(accountOverride, "http://127.0.0.1:7892"); err == nil || !strings.Contains(err.Error(), "账号级 proxy") {
		t.Fatalf("JSON5 account-level proxy was not rejected: %v", err)
	}
	duplicate := []byte(`{channels:{telegram:{proxy:'http://first',proxy:'http://second'}}}`)
	if _, _, err := setOpenClawTelegramProxy(duplicate, "http://127.0.0.1:7892"); err == nil || !strings.Contains(err.Error(), "重复定义") {
		t.Fatalf("duplicate managed key was not rejected: %v", err)
	}
}

func TestRejectOpenClawRuntimeConfigSelectorsAcceptsJSON5Syntax(t *testing.T) {
	unsafeConfig := []byte(`{
  env: {
    vars: {
      OPENCLAW_STATE_DIR: '/srv/other',
    },
  },
}`)
	if err := rejectOpenClawRuntimeConfigSelectors(unsafeConfig); err == nil || !strings.Contains(err.Error(), "OPENCLAW_STATE_DIR") {
		t.Fatalf("JSON5 runtime selector was not rejected: %v", err)
	}
	safeConfig := []byte(`{env:{shellEnv:{enabled:false,},vars:{OPENAI_API_KEY:'secret',},},}`)
	if err := rejectOpenClawRuntimeConfigSelectors(safeConfig); err != nil {
		t.Fatalf("safe JSON5 runtime config rejected: %v", err)
	}
}

func TestOpenClawProxyNullRoundTripAndNumericPrecision(t *testing.T) {
	in := []byte(`{"counter":9007199254740993123456789,"channels":{"telegram":{"proxy":null,"botToken":"x"}}}`)
	original, present, err := openClawTelegramProxyRawValue(in)
	if err != nil || !present || !bytes.Equal(original, []byte("null")) {
		t.Fatalf("proxy null state lost: value=%s present=%v err=%v", original, present, err)
	}
	out, changed, err := setOpenClawTelegramProxy(in, "http://127.0.0.1:7892")
	if err != nil || !changed {
		t.Fatalf("set from null failed: changed=%v err=%v", changed, err)
	}
	restored, changed, err := replaceOpenClawTelegramProxyRaw(out, original, true)
	if err != nil || !changed {
		t.Fatalf("restore null failed: changed=%v err=%v", changed, err)
	}
	value, present, err := openClawTelegramProxyRawValue(restored)
	if err != nil || !present || !bytes.Equal(value, []byte("null")) {
		t.Fatalf("restored proxy is not null: value=%s present=%v err=%v", value, present, err)
	}
	if !strings.Contains(string(restored), "9007199254740993123456789") {
		t.Fatalf("large integer precision changed:\n%s", restored)
	}
}

func TestOpenClawRejectsNullContainersButAllowsNullProxy(t *testing.T) {
	for _, raw := range []string{
		`null`,
		`{"channels":null}`,
		`{"channels":{"telegram":null}}`,
	} {
		if _, _, err := setOpenClawTelegramProxy([]byte(raw), "http://127.0.0.1:7892"); err == nil {
			t.Fatalf("container null should be rejected: %s", raw)
		}
	}
	if _, _, err := setOpenClawTelegramProxy([]byte(`{"channels":{"telegram":{"proxy":null}}}`), "http://127.0.0.1:7892"); err != nil {
		t.Fatalf("proxy null should be a restorable value: %v", err)
	}
}

func TestDecodeOpenClawJournalPreservesNullOriginal(t *testing.T) {
	raw := []byte(`{
  "version": 1,
  "users": {
    "root": {
      "user": "root",
	  "identity": {"uid": 0, "gid": 0, "home": "/root"},
      "original_value": null,
      "original_present": true,
      "managed_value": "http://127.0.0.1:7892",
      "targets": ["user:root:openclaw-gateway.service"],
      "phase": "active"
    }
  }
}`)
	journal, err := decodeOpenClawProxyJournal(raw)
	if err != nil {
		t.Fatal(err)
	}
	entry := journal.Users["root"]
	if !entry.OriginalPresent || !bytes.Equal(entry.OriginalValue, []byte("null")) {
		t.Fatalf("journal null original lost: %+v", entry)
	}
	if journal.Generation != 0 {
		t.Fatalf("legacy journal without generation decoded as %d, want 0", journal.Generation)
	}

	a := testApp(t)
	if err := a.saveOpenClawProxyJournal(journal); err != nil {
		t.Fatalf("migrate generation-zero journal: %v", err)
	}
	mainJournal := readOpenClawJournalCopy(t, a.openClawJournalPath())
	backupJournal := readOpenClawJournalCopy(t, a.openClawJournalBackupPath())
	if mainJournal.Generation != 1 || !reflect.DeepEqual(mainJournal, backupJournal) {
		t.Fatalf("generation-zero migration did not converge copies: main=%+v backup=%+v", mainJournal, backupJournal)
	}
}

func TestOpenClawJournalRejectsLegacyEntryWithoutIdentity(t *testing.T) {
	journal := newOpenClawProxyJournal()
	journal.Users["root"] = &openClawProxyJournalEntry{
		User:         "root",
		ManagedValue: "http://127.0.0.1:7892",
		Targets:      []string{"user:root:openclaw-legacy.service"},
		Phase:        openClawPhaseActive,
	}
	raw, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeOpenClawProxyJournal(raw); err == nil || !strings.Contains(err.Error(), "缺少 uid/gid/home") {
		t.Fatalf("legacy OpenClaw journal did not fail closed: %v", err)
	}
}

func TestOpenClawApplyCrashBoundariesAreReplayable(t *testing.T) {
	const proxy = "http://127.0.0.1:7892"

	t.Run("journal write fails before config change", func(t *testing.T) {
		h := newOpenClawTestHarness(t, `{}`)
		target := h.target("openclaw-journal.service")
		h.failNextJournal = errors.New("journal write failed")
		if _, _, err := h.app.applyOpenClawTelegramProxy(target, proxy); err == nil {
			t.Fatal("journal failure must abort apply")
		}
		if got := strings.TrimSpace(string(h.config(t))); got != `{}` {
			t.Fatalf("config changed without durable ownership: %s", got)
		}
		h.applyAndCommit(t, target, proxy)
	})

	t.Run("config write fails after prepared journal", func(t *testing.T) {
		h := newOpenClawTestHarness(t, `{}`)
		target := h.target("openclaw-config.service")
		h.failNextCAS = errors.New("config write failed")
		if _, _, err := h.app.applyOpenClawTelegramProxy(target, proxy); err == nil {
			t.Fatal("config failure must be reported")
		}
		entry := h.journal(t).Users[h.user]
		if entry == nil || entry.Phase != openClawPhasePrepared || !containsString(entry.PendingTargets, canonicalTelegramTargetName(target)) {
			t.Fatalf("prepared ownership was not retained: %+v", entry)
		}
		if got := strings.TrimSpace(string(h.config(t))); got != `{}` {
			t.Fatalf("failed CAS changed config: %s", got)
		}
		h.applyAndCommit(t, target, proxy)
	})

	t.Run("process crashes after config before restart", func(t *testing.T) {
		h := newOpenClawTestHarness(t, `{}`)
		target := h.target("openclaw-process-crash.service")
		managed, changed, err := h.app.applyOpenClawTelegramProxy(target, proxy)
		if err != nil || !managed || !changed {
			t.Fatalf("initial apply: managed=%v changed=%v err=%v", managed, changed, err)
		}
		// A new App models a new process. Seeing the desired config is insufficient:
		// prepared ownership forces the caller to replay the service restart.
		h.app = NewApp(h.app.cfg)
		managed, changed, err = h.app.applyOpenClawTelegramProxy(target, proxy)
		if err != nil || !managed || !changed {
			t.Fatalf("crash replay: managed=%v changed=%v err=%v", managed, changed, err)
		}
		entry := h.journal(t).Users[h.user]
		if entry == nil || entry.Phase != openClawPhasePrepared || len(entry.PendingTargets) != 1 || entry.PendingTargets[0] != canonicalTelegramTargetName(target) {
			t.Fatalf("restart was not replayed: entry=%+v", entry)
		}
		if err := h.app.commitOpenClawTelegramProxyApply(target); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("restart failure retains pending ownership", func(t *testing.T) {
		h := newOpenClawTestHarness(t, `{}`)
		target := h.target("openclaw-restart.service")
		if _, _, err := h.app.applyOpenClawTelegramProxy(target, proxy); err != nil {
			t.Fatal(err)
		}
		// A failed restart means commit is deliberately not called.
		entry := h.journal(t).Users[h.user]
		if entry == nil || entry.Phase != openClawPhasePrepared || len(entry.PendingTargets) != 1 {
			t.Fatalf("restart failure lost ownership: %+v", entry)
		}
		if err := h.app.commitOpenClawTelegramProxyApply(target); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("commit write failure is retryable", func(t *testing.T) {
		h := newOpenClawTestHarness(t, `{}`)
		target := h.target("openclaw-commit.service")
		if _, _, err := h.app.applyOpenClawTelegramProxy(target, proxy); err != nil {
			t.Fatal(err)
		}
		h.failNextJournal = errors.New("commit journal failed")
		if err := h.app.commitOpenClawTelegramProxyApply(target); err == nil {
			t.Fatal("commit write failure must be reported")
		}
		entry := h.journal(t).Users[h.user]
		if entry == nil || entry.Phase != openClawPhasePrepared || len(entry.PendingTargets) != 1 {
			t.Fatalf("failed commit lost pending ownership: %+v", entry)
		}
		if err := h.app.commitOpenClawTelegramProxyApply(target); err != nil {
			t.Fatalf("commit retry failed: %v", err)
		}
	})
}

func TestOpenClawPortChangeMultiTargetPartialRestart(t *testing.T) {
	h := newOpenClawTestHarness(t, `{"channels":{"telegram":{"botToken":"secret"}}}`)
	first := h.target("openclaw-first.service")
	second := h.target("openclaw-second.service")
	const oldProxy = "http://127.0.0.1:7892"
	const newProxy = "http://127.0.0.1:8892"
	h.applyAndCommit(t, first, oldProxy)
	h.applyAndCommit(t, second, oldProxy)

	managed, changed, err := h.app.applyOpenClawTelegramProxy(first, newProxy)
	if err != nil || !managed || !changed {
		t.Fatalf("port change apply: managed=%v changed=%v err=%v", managed, changed, err)
	}
	entry := h.journal(t).Users[h.user]
	if entry == nil || entry.Phase != openClawPhasePrepared || entry.PendingManagedValue != newProxy || len(entry.PendingTargets) != 2 {
		t.Fatalf("port change did not enqueue every target: %+v", entry)
	}

	// The first restart succeeds and commits, while the second fails. Ownership
	// must stay prepared until that second service has loaded the new port.
	if err := h.app.commitOpenClawTelegramProxyApply(first); err != nil {
		t.Fatal(err)
	}
	entry = h.journal(t).Users[h.user]
	if entry == nil || entry.Phase != openClawPhasePrepared || len(entry.PendingTargets) != 1 || entry.PendingTargets[0] != canonicalTelegramTargetName(second) {
		t.Fatalf("partial restart committed too early: %+v", entry)
	}
	if entry.ManagedValue != oldProxy || entry.PendingManagedValue != newProxy || len(entry.Targets) != 2 {
		t.Fatalf("partial restart lost old/new ownership: %+v", entry)
	}
	if err := h.app.commitOpenClawTelegramProxyApply(second); err != nil {
		t.Fatal(err)
	}
	entry = h.journal(t).Users[h.user]
	if entry == nil || entry.Phase != openClawPhaseActive || entry.ManagedValue != newProxy || len(entry.PendingTargets) != 0 {
		t.Fatalf("final target did not activate new port: %+v", entry)
	}
}

func TestOpenClawRestoreConcurrentChangeKeepsOwnership(t *testing.T) {
	const original = "socks5://original.example:1080"
	const managed = "http://127.0.0.1:7892"
	const userValue = "socks5://user-change.example:2080"
	h := newOpenClawTestHarness(t, `{"channels":{"telegram":{"proxy":"`+original+`","botToken":"secret"}}}`)
	target := h.target("openclaw-restore.service")
	h.applyAndCommit(t, target, managed)

	restart, err := h.app.prepareRestoreOpenClawTelegramProxy(target)
	if err != nil || !restart {
		t.Fatalf("prepare restore: restart=%v err=%v", restart, err)
	}
	entry := h.journal(t).Users[h.user]
	if entry == nil || entry.Phase != openClawPhaseRestoring {
		t.Fatalf("ownership was deleted before restart: %+v", entry)
	}
	concurrent := []byte(`{"channels":{"telegram":{"proxy":"` + userValue + `","botToken":"secret"}}}`)
	if err := os.WriteFile(h.configPath, concurrent, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.app.commitOpenClawTelegramProxyRestore(target); err == nil {
		t.Fatal("restore commit must reject a proxy changed after restart")
	}
	entry = h.journal(t).Users[h.user]
	if entry == nil || entry.Phase != openClawPhaseRestoring {
		t.Fatalf("concurrent change lost restoring ownership: %+v", entry)
	}

	restart, err = h.app.prepareRestoreOpenClawTelegramProxy(target)
	if err != nil || !restart {
		t.Fatalf("replay concurrent restore: restart=%v err=%v", restart, err)
	}
	if err := h.app.commitOpenClawTelegramProxyRestore(target); err != nil {
		t.Fatal(err)
	}
	if h.journal(t).Users[h.user] != nil {
		t.Fatal("ownership remained after the user value was successfully restarted")
	}
	value, present, err := openClawTelegramProxyRawValue(h.config(t))
	if err != nil || !proxyStateMatchesString(value, present, userValue) {
		t.Fatalf("user value was not preserved: value=%s present=%v err=%v", value, present, err)
	}
}

func TestOpenClawIdentityDriftPreservesConfigJournalAndService(t *testing.T) {
	h := newOpenClawTestHarness(t, `{"channels":{"telegram":{"botToken":"secret"}}}`)
	target := h.target("openclaw-identity-drift.service")
	const proxy = "http://127.0.0.1:7892"
	h.applyAndCommit(t, target, proxy)
	before := append([]byte(nil), h.config(t)...)

	oldUserExists := telegramUserUnitExists
	oldUserSystemctl := userSystemctlRun
	t.Cleanup(func() {
		telegramUserUnitExists = oldUserExists
		userSystemctlRun = oldUserSystemctl
	})
	telegramUserUnitExists = func(string, string) bool { return true }
	restartCalls := 0
	userSystemctlRun = func(string, *persistedUserIdentity, string, ...string) error {
		restartCalls++
		return nil
	}
	openClawReadUserConfig = func(string, string, int64) ([]byte, error) {
		t.Fatal("identity drift must be rejected before reading any user config")
		return nil, nil
	}
	h.identity.UID = 4242
	h.identity.UIDText = "4242"
	h.identity.Home = t.TempDir()

	_, err := h.app.cleanupManagedOpenClawTargets(target, true)
	if err == nil || !strings.Contains(err.Error(), "身份已变化") {
		t.Fatalf("identity drift was not rejected: %v", err)
	}
	if got := h.config(t); !bytes.Equal(got, before) {
		t.Fatalf("identity drift changed the recorded account config: %s", got)
	}
	entry := h.journal(t).Users[h.user]
	if entry == nil || entry.Phase != openClawPhaseActive {
		t.Fatalf("identity drift should retain active ownership: %+v", entry)
	}
	if restartCalls != 0 {
		t.Fatalf("identity drift restarted the replacement account: calls=%d", restartCalls)
	}
}

func TestOpenClawMissingMainRecoversLatestBackup(t *testing.T) {
	h := newOpenClawTestHarness(t, `{}`)
	target := h.target("openclaw-backup.service")
	const proxy = "http://127.0.0.1:7892"
	h.applyAndCommit(t, target, proxy)
	if err := os.Remove(h.app.openClawJournalPath()); err != nil {
		t.Fatal(err)
	}
	entry := h.journal(t).Users[h.user]
	if entry == nil || entry.Phase != openClawPhaseActive || entry.ManagedValue != proxy {
		t.Fatalf("backup was not the latest committed generation: %+v", entry)
	}
}

func TestOpenClawJournalWritesMatchingGeneratedCopies(t *testing.T) {
	h := newOpenClawTestHarness(t, `{}`)
	h.applyAndCommit(t, h.target("openclaw-matching-copies.service"), "http://127.0.0.1:7892")
	mainJournal := readOpenClawJournalCopy(t, h.app.openClawJournalPath())
	backupJournal := readOpenClawJournalCopy(t, h.app.openClawJournalBackupPath())
	if mainJournal.Generation == 0 || !reflect.DeepEqual(mainJournal, backupJournal) {
		t.Fatalf("journal copies did not converge: main=%+v backup=%+v", mainJournal, backupJournal)
	}
}

func TestOpenClawJournalSelectsNewerValidCopy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		selectPath func(*App) string
		wantValue  string
	}{
		{name: "main ahead", selectPath: func(a *App) string { return a.openClawJournalPath() }, wantValue: "http://127.0.0.1:8892"},
		{name: "backup ahead", selectPath: func(a *App) string { return a.openClawJournalBackupPath() }, wantValue: "http://127.0.0.1:9892"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newOpenClawTestHarness(t, `{}`)
			target := h.target("openclaw-ahead.service")
			h.applyAndCommit(t, target, "http://127.0.0.1:7892")
			path := tc.selectPath(h.app)
			ahead := readOpenClawJournalCopy(t, path)
			ahead.Generation++
			ahead.Users[h.user].ManagedValue = tc.wantValue
			writeOpenClawJournalCopy(t, path, ahead)

			loaded, err := h.app.loadOpenClawProxyJournal()
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Generation != ahead.Generation || loaded.Users[h.user].ManagedValue != tc.wantValue {
				t.Fatalf("newer %s copy was not selected: %+v", tc.name, loaded)
			}
		})
	}
}

func TestOpenClawJournalRejectsEqualGenerationSplitBrain(t *testing.T) {
	h := newOpenClawTestHarness(t, `{}`)
	h.applyAndCommit(t, h.target("openclaw-split-brain.service"), "http://127.0.0.1:7892")
	backup := readOpenClawJournalCopy(t, h.app.openClawJournalBackupPath())
	backup.Users[h.user].ManagedValue = "http://127.0.0.1:8892"
	writeOpenClawJournalCopy(t, h.app.openClawJournalBackupPath(), backup)
	if _, err := h.app.loadOpenClawProxyJournal(); err == nil || !strings.Contains(err.Error(), "同一 generation 内容不一致") {
		t.Fatalf("equal-generation split brain was accepted: %v", err)
	}
}

func TestOpenClawJournalMainWriteFailureRecoversAheadBackup(t *testing.T) {
	h := newOpenClawTestHarness(t, `{}`)
	target := h.target("openclaw-main-write-failure.service")
	h.applyAndCommit(t, target, "http://127.0.0.1:7892")
	journal := h.journal(t)
	journal.Users[h.user].ManagedValue = "http://127.0.0.1:8892"
	wantGeneration := journal.Generation + 1

	baseWrite := openClawWriteJournalFile
	failedMain := false
	var writes []string
	openClawWriteJournalFile = func(path string, data []byte, perm os.FileMode) error {
		writes = append(writes, path)
		if path == h.app.openClawJournalPath() && !failedMain {
			failedMain = true
			return errors.New("injected main journal failure")
		}
		return baseWrite(path, data, perm)
	}
	if err := h.app.saveOpenClawProxyJournal(journal); err == nil || !strings.Contains(err.Error(), "injected main journal failure") {
		t.Fatalf("main write failure was not returned: %v", err)
	}
	if len(writes) != 2 || writes[0] != h.app.openClawJournalBackupPath() || writes[1] != h.app.openClawJournalPath() {
		t.Fatalf("journal was not written backup-first: %v", writes)
	}
	loaded, err := h.app.loadOpenClawProxyJournal()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Generation != wantGeneration || loaded.Users[h.user].ManagedValue != "http://127.0.0.1:8892" {
		t.Fatalf("ahead backup was not recovered after main failure: %+v", loaded)
	}
}

func TestOpenClawJournalRejectsGenerationOverflow(t *testing.T) {
	h := newOpenClawTestHarness(t, `{}`)
	journal := newOpenClawProxyJournal()
	journal.Generation = ^uint64(0)
	if err := h.app.saveOpenClawProxyJournal(journal); err == nil || !strings.Contains(err.Error(), "generation 已耗尽") {
		t.Fatalf("generation overflow was accepted: %v", err)
	}
}

func TestOpenClawJournalRejectsUnreadableOversizeBeforeWrite(t *testing.T) {
	h := newOpenClawTestHarness(t, `{}`)
	target := h.target("openclaw-oversize-journal.service")
	h.applyAndCommit(t, target, "http://127.0.0.1:7892")
	journal := h.journal(t)
	originalGeneration := journal.Generation
	journal.Users[h.user].ManagedValue = strings.Repeat("x", int(maxOpenClawJournalBytes))

	writes := 0
	openClawWriteJournalFile = func(string, []byte, os.FileMode) error {
		writes++
		return nil
	}
	err := h.app.saveOpenClawProxyJournal(journal)
	if err == nil || !strings.Contains(err.Error(), "超过") {
		t.Fatalf("oversize journal was accepted: %v", err)
	}
	if writes != 0 {
		t.Fatalf("oversize journal reached disk writer: calls=%d", writes)
	}
	if journal.Generation != originalGeneration {
		t.Fatalf("rejected journal changed generation: got=%d want=%d", journal.Generation, originalGeneration)
	}
}

func TestOpenClawCASChangePreservesUserConfigAndOwnership(t *testing.T) {
	h := newOpenClawTestHarness(t, `{}`)
	target := h.target("openclaw-cas.service")
	const managed = "http://127.0.0.1:7892"
	const userValue = "socks5://user-race.example:1080"
	userConfig := []byte(`{"channels":{"telegram":{"proxy":"` + userValue + `"}}}`)
	h.beforeNextCAS = func() {
		if err := os.WriteFile(h.configPath, userConfig, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := h.app.applyOpenClawTelegramProxy(target, managed); !errors.Is(err, errUserFileChanged) {
		t.Fatalf("CAS race error = %v, want %v", err, errUserFileChanged)
	}
	if got := h.config(t); !bytes.Equal(got, userConfig) {
		t.Fatalf("CAS race overwrote user config: %s", got)
	}
	entry := h.journal(t).Users[h.user]
	if entry == nil || entry.Phase != openClawPhasePrepared || len(entry.PendingTargets) != 1 {
		t.Fatalf("CAS race deleted recovery ownership: %+v", entry)
	}
}

func TestOpenClawRejectsAccountProxyOverrideBeforeOwnership(t *testing.T) {
	initial := `{"channels":{"telegram":{"proxy":"http://base.invalid:8080","accounts":{"default":{"proxy":"http://account.invalid:8080"},"other":{"enabled":true}}}}}`
	h := newOpenClawTestHarness(t, initial)
	target := h.target("openclaw-account-proxy.service")
	managed, changed, err := h.app.applyOpenClawTelegramProxy(target, "http://127.0.0.1:7892")
	if err == nil || !strings.Contains(err.Error(), "账号级 proxy") {
		t.Fatalf("account proxy override was not rejected clearly: managed=%v changed=%v err=%v", managed, changed, err)
	}
	if managed || changed {
		t.Fatalf("rejected account override was reported as managed: managed=%v changed=%v", managed, changed)
	}
	if got := h.config(t); string(got) != initial {
		t.Fatalf("rejected account override changed config: %s", got)
	}
	if _, statErr := os.Lstat(h.app.openClawJournalPath()); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rejected account override created ownership journal: %v", statErr)
	}
}

func TestSetOpenClawProxyAllowsAccountsWithoutProxy(t *testing.T) {
	initial := []byte(`{"channels":{"telegram":{"accounts":{"default":{"enabled":true}}}}}`)
	out, changed, err := setOpenClawTelegramProxy(initial, "http://127.0.0.1:7892")
	if err != nil || !changed {
		t.Fatalf("account without proxy should be managed: changed=%v err=%v", changed, err)
	}
	cfg := parseJSON(t, out)
	telegram := cfg["channels"].(map[string]any)["telegram"].(map[string]any)
	if telegram["proxy"] != "http://127.0.0.1:7892" {
		t.Fatalf("top-level proxy not set: %+v", telegram)
	}
	accounts := telegram["accounts"].(map[string]any)
	if accounts["default"].(map[string]any)["enabled"] != true {
		t.Fatalf("account config was not preserved: %+v", accounts)
	}
}

func TestOpenClawAbsentProxyRestoresOriginalContainerStructure(t *testing.T) {
	cases := []struct {
		name    string
		initial string
		check   func(*testing.T, map[string]any)
	}{
		{
			name:    "channels absent",
			initial: `{"models":{"keep":true}}`,
			check: func(t *testing.T, cfg map[string]any) {
				if _, ok := cfg["channels"]; ok {
					t.Fatalf("apply-created channels container was not removed: %+v", cfg)
				}
			},
		},
		{
			name:    "telegram absent",
			initial: `{"channels":{"other":{"keep":true}}}`,
			check: func(t *testing.T, cfg map[string]any) {
				channels := cfg["channels"].(map[string]any)
				if _, ok := channels["telegram"]; ok {
					t.Fatalf("apply-created telegram container was not removed: %+v", channels)
				}
			},
		},
		{
			name:    "empty telegram preserved",
			initial: `{"channels":{"telegram":{}}}`,
			check: func(t *testing.T, cfg map[string]any) {
				channels := cfg["channels"].(map[string]any)
				if _, ok := channels["telegram"]; !ok {
					t.Fatalf("pre-existing empty telegram container was removed: %+v", channels)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newOpenClawTestHarness(t, tc.initial)
			target := h.target("openclaw-structure.service")
			h.applyAndCommit(t, target, "http://127.0.0.1:7892")
			restart, err := h.app.prepareRestoreOpenClawTelegramProxy(target)
			if err != nil || !restart {
				t.Fatalf("prepare restore: restart=%v err=%v", restart, err)
			}
			if err := h.app.commitOpenClawTelegramProxyRestore(target); err != nil {
				t.Fatal(err)
			}
			cfg := parseJSON(t, h.config(t))
			tc.check(t, cfg)
		})
	}
}

func TestUnitHasOpenClawGatewayMarker(t *testing.T) {
	gw := "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw\nEnvironment=OPENCLAW_SERVICE_KIND=gateway\n"
	if !unitHasOpenClawGatewayMarker(gw) {
		t.Errorf("带 marker+gateway 的单元应命中")
	}
	node := "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw\nEnvironment=OPENCLAW_SERVICE_KIND=node\n"
	if unitHasOpenClawGatewayMarker(node) {
		t.Errorf("KIND=node 不应命中")
	}
	guard := "[Unit]\nDescription=OpenClaw xhigh guard\n[Service]\nExecStart=/usr/bin/node /x/guard.mjs\n"
	if unitHasOpenClawGatewayMarker(guard) {
		t.Errorf("无 marker 的 guard 不应命中")
	}
}

func TestIsOpenClawTargetFallback(t *testing.T) {
	a := testApp(t)
	// 用磁盘上不存在的单元名，强制走「定位不到单元 -> 按名前缀回退」分支，结果确定。
	cases := map[string]bool{
		"openclaw-gateway-nx-zzz.service": true,
		"openclaw-nx-zzz.service":         true,
		"hermes-gateway-nx-zzz.service":   false,
		"telegram-bot-nx-zzz.service":     false,
	}
	for svc, want := range cases {
		got, err := a.classifyOpenClawTarget(systemdTargetName{Service: svc})
		if err != nil {
			t.Fatalf("classifyOpenClawTarget(%q): %v", svc, err)
		}
		if got != want {
			t.Errorf("classifyOpenClawTarget(%q)=%v, want %v", svc, got, want)
		}
	}
}
