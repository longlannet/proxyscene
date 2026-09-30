package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

const restartTestState = "LoadState=loaded\nActiveState=active\nJob=\nTimeoutStopUSec=5min 30s\nTimeoutStartUSec=30s\n"

// This helper only runs the Go test executable. No test invokes systemctl,
// modifies host service state, or depends on a running systemd instance.
func TestSystemdRestartHelperProcess(t *testing.T) {
	if os.Getenv("PROXYSCENE_RESTART_TEST_CHILD") != "1" {
		return
	}
	if os.Getenv("PROXYSCENE_RESTART_TEST_ACTION") == "show" {
		fmt.Print(os.Getenv("PROXYSCENE_RESTART_TEST_STATE"))
		os.Exit(0)
	}
	if delay, err := time.ParseDuration(os.Getenv("PROXYSCENE_RESTART_TEST_DELAY")); err == nil {
		time.Sleep(delay)
	}
	if os.Getenv("PROXYSCENE_RESTART_TEST_FAIL") == "1" {
		fmt.Fprintln(os.Stderr, "secret-proxy-password-must-not-escape")
		os.Exit(7)
	}
	os.Exit(0)
}

func restartTestCommand(t *testing.T, state string, delay time.Duration, fail bool, calls *[][]string) systemdCommandBuilder {
	t.Helper()
	return func(ctx context.Context, args ...string) (*exec.Cmd, error) {
		*calls = append(*calls, append([]string(nil), args...))
		if args[0] == "show" {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 5*time.Second || time.Until(deadline) <= 0 {
				t.Fatalf("show lacks bounded deadline: %v, %v", deadline, ok)
			}
		}
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSystemdRestartHelperProcess$")
		cmd.Env = append(os.Environ(),
			"PROXYSCENE_RESTART_TEST_CHILD=1",
			"PROXYSCENE_RESTART_TEST_ACTION="+args[0],
			"PROXYSCENE_RESTART_TEST_STATE="+state,
			"PROXYSCENE_RESTART_TEST_DELAY="+delay.String(),
			fmt.Sprintf("PROXYSCENE_RESTART_TEST_FAIL=%d", map[bool]int{false: 0, true: 1}[fail]),
			"GORACE=atexit_sleep_ms=0")
		return cmd, nil
	}
}

func TestSystemdRestartBudget(t *testing.T) {
	for _, test := range []struct {
		name, stop, start string
		want              time.Duration
	}{
		{"openclaw330seconds", "5min 30s", "30s", 390 * time.Second},
		{"minimum", "1s", "30s", 5 * time.Minute},
		{"goDuration", "5m30s", "30.5s", 390500 * time.Millisecond},
		{"mixedUnits", "5min 30000ms", "30000000us", 390 * time.Second},
		{"maximum", "14min", "30s", 15 * time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := systemdRestartBudget(test.stop, test.start)
			if err != nil || got != test.want {
				t.Fatalf("budget = %v, %v; want %v", got, err, test.want)
			}
		})
	}
	for _, value := range []string{"", "infinity", "infinite", "0", "0s", "NaN", "-1s", "+30s", "30s -1s", "1garbage", "18446744073709551615us", "2562047h 2562047h"} {
		if _, err := systemdRestartBudget(value, "30s"); err == nil {
			t.Errorf("accepted unsafe stop duration %q", value)
		}
		if _, err := systemdRestartBudget("30s", value); err == nil {
			t.Errorf("accepted unsafe start duration %q", value)
		}
	}
	for _, pair := range [][2]string{{"14min", "31s"}, {"1h", "30s"}, {"2562047h", "2562047h"}} {
		if _, err := systemdRestartBudget(pair[0], pair[1]); err == nil {
			t.Errorf("accepted excessive budget %v", pair)
		}
	}
}

func TestSystemdRestartReadsBudgetBeforeSubmitting(t *testing.T) {
	var calls [][]string
	command := restartTestCommand(t, restartTestState, 0, false, &calls)
	var gotBudget time.Duration
	wrapped := func(ctx context.Context, args ...string) (*exec.Cmd, error) {
		if args[0] == "try-restart" {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("restart has no deadline")
			}
			gotBudget = time.Until(deadline)
		}
		return command(ctx, args...)
	}
	if err := runSystemdRestart("重启测试服务", "test-only.service", wrapped); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0][0] != "show" || !reflect.DeepEqual(calls[1], []string{"try-restart", "--", "test-only.service"}) {
		t.Fatalf("unexpected commands: %v", calls)
	}
	for _, property := range []string{"LoadState", "ActiveState", "Job", "TimeoutStopUSec", "TimeoutStartUSec"} {
		if !strings.Contains(strings.Join(calls[0], " "), "--property="+property) {
			t.Errorf("did not request %s", property)
		}
	}
	if gotBudget < 389*time.Second || gotBudget > 390*time.Second {
		t.Fatalf("restart deadline budget %v; want 390s", gotBudget)
	}
}

func TestSystemdRestartPreflightNeverSubmitsUnsafeJob(t *testing.T) {
	for _, test := range []struct {
		name, state string
		unsettled   bool
	}{
		{"pendingJob", strings.Replace(restartTestState, "Job=", "Job=127 /org/freedesktop/systemd1/job/127", 1), true},
		{"activating", strings.Replace(restartTestState, "ActiveState=active", "ActiveState=activating", 1), true},
		{"deactivating", strings.Replace(restartTestState, "ActiveState=active", "ActiveState=deactivating", 1), true},
		{"reloading", strings.Replace(restartTestState, "ActiveState=active", "ActiveState=reloading", 1), true},
		{"unknownState", strings.Replace(restartTestState, "ActiveState=active", "ActiveState=unknown", 1), true},
		{"unloaded", strings.Replace(restartTestState, "LoadState=loaded", "LoadState=not-found", 1), false},
		{"unbounded", strings.Replace(restartTestState, "5min 30s", "infinity", 1), false},
		{"malformedTimeout", strings.Replace(restartTestState, "5min 30s", "garbage", 1), false},
		{"overLimit", strings.Replace(restartTestState, "5min 30s", "30min", 1), false},
		{"missingProperty", strings.Replace(restartTestState, "Job=\n", "", 1), false},
		{"duplicateProperty", restartTestState + "Job=\n", false},
		{"oversizedOutput", strings.Repeat("x", 4097), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls [][]string
			err := runSystemdRestart("重启测试服务", "test-only.service", restartTestCommand(t, test.state, 0, false, &calls))
			if err == nil || errors.Is(err, errSystemdRestartUnsettled) != test.unsettled {
				t.Fatalf("unexpected error classification: %v", err)
			}
			if len(calls) != 1 || calls[0][0] != "show" {
				t.Fatalf("unsafe preflight submitted restart: %v", calls)
			}
		})
	}
}

func TestSystemdRestartNonemptyZeroJobIsUnsettled(t *testing.T) {
	state, err := parseSystemdRestartState(strings.Replace(restartTestState, "Job=", "Job=0", 1))
	if err != nil || !errors.Is(state.ready(), errSystemdRestartUnsettled) {
		t.Fatalf("nonempty job must remain unsettled: %v, %+v", err, state)
	}
}

func TestSystemdRestartProgressAndTimeoutPreserveUncertainty(t *testing.T) {
	var calls [][]string
	var output bytes.Buffer
	started := time.Now()
	err := waitSystemdRestart("重启测试服务", "test-only.service", 150*time.Millisecond, 20*time.Millisecond, &output,
		restartTestCommand(t, restartTestState, 10*time.Second, false, &calls))
	if !errors.Is(err, errSystemdRestartUnsettled) || !strings.Contains(err.Error(), "不会取消") || !strings.Contains(err.Error(), "recover") {
		t.Fatalf("timeout lost systemd job uncertainty: %v", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatalf("timeout did not bound child wait: %v", time.Since(started))
	}
	if !strings.Contains(output.String(), "最长 150ms") || !strings.Contains(output.String(), "仍在等待") || !strings.Contains(output.String(), "已等待") {
		t.Fatalf("missing progress: %q", output.String())
	}
	if len(calls) != 1 {
		t.Fatalf("timeout submitted extra command: %v", calls)
	}
}

func TestSystemdRestartFailureOnlyAllowsCompensationAfterSettledCheck(t *testing.T) {
	for _, test := range []struct {
		name, after string
		unsettled   bool
	}{
		{"settled", restartTestState, false},
		{"pending", strings.Replace(restartTestState, "Job=", "Job=245", 1), true},
		{"switching", strings.Replace(restartTestState, "ActiveState=active", "ActiveState=deactivating", 1), true},
		{"unreadable", "not valid state", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls [][]string
			var output bytes.Buffer
			err := waitSystemdRestart("重启测试服务", "test-only.service", 3*time.Second, time.Second, &output,
				restartTestCommand(t, test.after, 0, true, &calls))
			if err == nil || errors.Is(err, errSystemdRestartUnsettled) != test.unsettled {
				t.Fatalf("incorrect failure classification: %v", err)
			}
			if strings.Contains(output.String()+err.Error(), "secret-proxy-password") {
				t.Fatal("command stderr escaped into diagnostics")
			}
			if len(calls) != 2 || calls[0][0] != "try-restart" || calls[1][0] != "show" {
				t.Fatalf("failure did not read settled state: %v", calls)
			}
		})
	}
}

func TestSystemdRestartBuildFailureNeverStartsCommand(t *testing.T) {
	want := errors.New("persisted identity changed")
	command := func(context.Context, ...string) (*exec.Cmd, error) { return nil, want }
	if err := runSystemdRestart("重启测试服务", "test-only.service", command); !errors.Is(err, want) || errors.Is(err, errSystemdRestartUnsettled) {
		t.Fatalf("preflight builder failure incorrectly classified: %v", err)
	}
	if err := waitSystemdRestart("重启测试服务", "test-only.service", time.Second, time.Second, &bytes.Buffer{}, command); !errors.Is(err, want) {
		t.Fatalf("restart builder failure incorrectly classified: %v", err)
	}
}

func TestSystemdRestartUserWrapperRetainsIdentityAndBus(t *testing.T) {
	for _, test := range []struct {
		name     string
		changeAt int
		change   string
	}{
		{name: "sameIdentity"},
		{name: "uidChangedAfterQuery", changeAt: 3, change: "uid"},
		{name: "gidChangedAfterQuery", changeAt: 3, change: "gid"},
		{name: "homeChangedAfterQuery", changeAt: 3, change: "home"},
		{name: "uidChangedImmediatelyBeforeRestart", changeAt: 4, change: "uid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			oldStat, oldExecutable := userSystemctlStat, userSystemctlExecutable
			t.Cleanup(func() {
				userSystemctlStat, userSystemctlExecutable = oldStat, oldExecutable
			})
			uid, gid := os.Geteuid(), os.Getegid()
			recordedHome := t.TempDir()
			identity := &persistedUserIdentity{UID: uid, GID: gid, Home: recordedHome}
			runtimeDir := "/run/user/" + strconv.Itoa(uid)
			busChecks := 0
			userSystemctlStat = func(path string) (os.FileInfo, error) {
				busChecks++
				if path != runtimeDir+"/bus" {
					t.Fatalf("queried replacement user's bus: %q", path)
				}
				return nil, nil
			}
			// Both show and try-restart execute this local script, never systemctl.
			// The log also records the child's real UID/GID and working directory.
			script := filepath.Join(recordedHome, "systemctl-test")
			content := `#!/bin/sh
set -eu
task_uid=$(/usr/bin/id -u)
task_gid=$(/usr/bin/id -g)
{
  printf '%s\t' "$HOME" "$USER" "$LOGNAME" "$XDG_RUNTIME_DIR" "$DBUS_SESSION_BUS_ADDRESS" "$task_uid" "$task_gid" "$PWD"
  printf '%s\t' "$@"
  printf '\n'
} >> "$HOME/restart-wrapper.log"
case "$2" in
  show)
    printf 'LoadState=loaded\nActiveState=active\nJob=\nTimeoutStopUSec=5min 30s\nTimeoutStartUSec=30s\n'
    ;;
  try-restart)
    [ "$1" = '--user' ] && [ "$3" = '--' ] && [ "$4" = 'test-only.service' ]
    ;;
  *) exit 90 ;;
esac
`
			if err := os.WriteFile(script, []byte(content), 0o700); err != nil {
				t.Fatal(err)
			}
			userSystemctlExecutable = script
			logPath := filepath.Join(recordedHome, "restart-wrapper.log")
			lookupCalls := 0
			lookup := func(user string) (localUserIdentity, error) {
				lookupCalls++
				current := localUserIdentity{Name: user, UID: uid, GID: gid, UIDText: strconv.Itoa(uid), GIDText: strconv.Itoa(gid), Home: recordedHome}
				if test.changeAt != 0 && lookupCalls >= test.changeAt {
					if _, err := os.Stat(logPath); err != nil {
						t.Fatalf("identity remap must follow the completed show command: %v", err)
					}
					switch test.change {
					case "uid":
						current.UID++
						current.UIDText = strconv.Itoa(current.UID)
					case "gid":
						current.GID++
						current.GIDText = strconv.Itoa(current.GID)
					case "home":
						current.Home = recordedHome + "-replacement"
					}
				}
				return current, nil
			}
			err := runUserSystemctlQuietPersisted("alice", identity, lookup, "try-restart", "--", "test-only.service")
			wantCalls, wantChecks := 2, 2
			if test.changeAt == 0 {
				if err != nil {
					t.Fatal(err)
				}
				if lookupCalls != 4 {
					t.Fatalf("show/restart must each recheck persisted identity twice; got %d", lookupCalls)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "身份已变化") || errors.Is(err, errSystemdRestartUnsettled) {
					t.Fatalf("identity change before dispatch was not a definite rejection: %v", err)
				}
				wantCalls = 1
				if test.changeAt == 3 {
					wantChecks = 1
				}
				if lookupCalls != test.changeAt {
					t.Fatalf("continued after identity remap: %d checks, want %d", lookupCalls, test.changeAt)
				}
			}
			if busChecks != wantChecks {
				t.Fatalf("bus checks = %d, want %d", busChecks, wantChecks)
			}
			raw, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
			if len(lines) != wantCalls {
				t.Fatalf("wrong subprocess count (restart may have bypassed identity checks): %q", raw)
			}
			wantEnvironment := []string{recordedHome, "alice", "alice", runtimeDir, "unix:path=" + runtimeDir + "/bus", strconv.Itoa(uid), strconv.Itoa(gid), recordedHome}
			wantArguments := [][]string{
				{"--user", "show", "--property=LoadState", "--property=ActiveState", "--property=Job", "--property=TimeoutStopUSec", "--property=TimeoutStartUSec", "--", "test-only.service"},
				{"--user", "try-restart", "--", "test-only.service"},
			}
			for i, line := range lines {
				fields := strings.Split(strings.TrimSuffix(line, "\t"), "\t")
				want := append(append([]string(nil), wantEnvironment...), wantArguments[i]...)
				if !reflect.DeepEqual(fields, want) {
					t.Fatalf("command %d identity/environment/args = %q, want %q", i, fields, want)
				}
			}
		})
	}
}
