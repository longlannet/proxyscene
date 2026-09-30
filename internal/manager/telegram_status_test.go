package manager

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func stubTelegramServiceState(t *testing.T, state telegramServiceState) {
	t.Helper()
	old := telegramReadServiceState
	t.Cleanup(func() { telegramReadServiceState = old })
	telegramReadServiceState = func(systemdTargetName, *persistedUserIdentity) (telegramServiceState, error) {
		return state, nil
	}
}

func TestTelegramStatusDoesNotTreatInactiveServiceAsConnected(t *testing.T) {
	h := newTelegramJournalTestHarness(t)
	stubTelegramServiceState(t, telegramServiceState{LoadState: "loaded", ActiveState: "inactive"})
	desired := []byte("[Service]\n" + telegramProxySystemdEnvironmentLines(h.app.cfg))
	if _, err := h.app.prepareHermesTelegramApply(h.target(), desired); err != nil {
		t.Fatal(err)
	}
	if err := h.app.commitHermesTelegramApply(h.target()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(h.app.telegramProxyJournalPath())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := h.app.telegramTargetStatuses()
	if err != nil || len(rows) != 1 || rows[0].Config != "已写入并核对" || rows[0].Service != "未运行" {
		t.Fatalf("configuration and process state conflated: %+v, %v", rows, err)
	}
	var output bytes.Buffer
	writeTelegramTargetStatuses(&output, rows, nil)
	if !strings.Contains(output.String(), "频道/代理连通性=未探测") {
		t.Fatalf("unverified channel not identified: %s", output.String())
	}
	after, err := os.ReadFile(h.app.telegramProxyJournalPath())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("status mutated ownership journal")
	}
	if err := os.WriteFile(h.systemPath, []byte("[Service]\nEnvironment=TELEGRAM_PROXY=http://operator.invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rows, err = h.app.telegramTargetStatuses()
	if err != nil || rows[0].Config != "已变化或无法验证" {
		t.Fatalf("stale ownership reported configured: %+v, %v", rows, err)
	}
}

func TestTelegramStatusOpenClawChecksConfigAndIdentity(t *testing.T) {
	h := newOpenClawTestHarness(t, `{}`)
	stubTelegramServiceState(t, telegramServiceState{LoadState: "loaded", ActiveState: "active"})
	target := h.target("openclaw-gateway.service")
	if _, _, err := h.app.applyOpenClawTelegramProxy(target, h.app.cfg.HTTPAddr(SceneTelegram)); err != nil {
		t.Fatal(err)
	}
	if err := h.app.commitOpenClawTelegramProxyApply(target); err != nil {
		t.Fatal(err)
	}
	rows, err := h.app.telegramTargetStatuses()
	if err != nil || len(rows) != 1 || rows[0].Config != "已写入并核对" || rows[0].Service != "运行中" {
		t.Fatalf("unexpected status: %+v, %v", rows, err)
	}
	if err := os.WriteFile(h.configPath, []byte(`{"channels":{"telegram":{"proxy":"http://operator.invalid"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	rows, err = h.app.telegramTargetStatuses()
	if err != nil || rows[0].Config != "已变化或无法验证" {
		t.Fatalf("changed proxy was not detected: %+v, %v", rows, err)
	}
	openClawLookupUserIdentity = func(string) (localUserIdentity, error) {
		return localUserIdentity{}, errors.New("identity changed")
	}
	telegramReadServiceState = func(systemdTargetName, *persistedUserIdentity) (telegramServiceState, error) {
		t.Fatal("status crossed a mismatched identity")
		return telegramServiceState{}, nil
	}
	rows, err = h.app.telegramTargetStatuses()
	if err != nil || rows[0].Config != "身份校验失败" || rows[0].Service != "未查询" {
		t.Fatalf("identity change not contained: %+v, %v", rows, err)
	}
}

func TestTelegramStatusDoesNotEchoSensitiveErrors(t *testing.T) {
	var output bytes.Buffer
	writeTelegramTargetStatuses(&output, nil, errors.New("http://secret:password@proxy.invalid"))
	if strings.Contains(output.String(), "password") || !strings.Contains(output.String(), "状态未知") {
		t.Fatalf("unsafe status error: %q", output.String())
	}
}

func TestTelegramServiceStateDescription(t *testing.T) {
	for _, test := range []struct{ load, active, want string }{
		{"loaded", "active", "运行中"}, {"loaded", "inactive", "未运行"},
		{"loaded", "failed", "启动失败"}, {"loaded", "activating", "切换中"},
		{"not-found", "inactive", "未安装"}, {"masked", "inactive", "未正常加载"},
		{"loaded", "unknown-value", "未知"},
	} {
		if got := (telegramServiceState{LoadState: test.load, ActiveState: test.active}).description(); got != test.want {
			t.Fatalf("%+v: got %q", test, got)
		}
	}
	var limited telegramStatusBuffer
	if _, err := io.Copy(&limited, strings.NewReader(strings.Repeat("x", 4097))); err == nil || limited.Len() != 0 {
		t.Fatal("oversized command output accepted")
	}
}

func TestTelegramServiceStateRequiresExplicitIdleJob(t *testing.T) {
	valid := "LoadState=loaded\nActiveState=active\nJob=\n"
	for _, output := range []string{
		strings.ReplaceAll(valid, "Job=\n", ""),
		valid + "Job=42\n",
		valid + "ActiveState=inactive\n",
		"LoadState=loaded\nJob=\n",
		valid + "unexpected\n",
	} {
		if _, err := parseTelegramServiceState(output); err == nil {
			t.Fatalf("accepted ambiguous service state %q", output)
		}
	}
	for _, job := range []string{"", "42", "42 /org/freedesktop/systemd1/job/42"} {
		state, err := parseTelegramServiceState(strings.Replace(valid, "Job=", "Job="+job, 1))
		if err != nil || state.Job != job {
			t.Fatalf("job %q: state=%+v err=%v", job, state, err)
		}
		if job != "" && state.description() != "切换中" {
			t.Fatal("pending job reported as running")
		}
	}
}
