package manager

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setMainMenuTestInput(t *testing.T, input string) {
	t.Helper()
	old := stdinReader
	stdinReader = bufio.NewReader(strings.NewReader(input))
	t.Cleanup(func() { stdinReader = old })
}

func captureMainMenuTestOutput(t *testing.T, run func() error) (string, error) {
	t.Helper()
	output, err := os.CreateTemp(t.TempDir(), "menu-output-")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	original := os.Stdout
	os.Stdout = output
	defer func() { os.Stdout = original }()
	runErr := run()
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(output)
	if err != nil {
		t.Fatal(err)
	}
	return string(data), runErr
}

func TestMenuConfirmationRequiresSubmittedAffirmative(t *testing.T) {
	for _, input := range []string{"", "y", "yes", "n\n", "\n", "q\n", "yesplease\n", "y\n", "YES\r\n"} {
		t.Run(input, func(t *testing.T) {
			setMainMenuTestInput(t, input)
			confirmed, err := menuConfirm("confirm")
			want := input == "y\n" || input == "YES\r\n"
			if confirmed != want {
				t.Fatalf("input %q authorized=%t, want %t", input, confirmed, want)
			}
			if !strings.Contains(input, "\n") && err != errMenuClosed {
				t.Fatalf("unterminated input did not end interaction: %v", err)
			}
			if input == "q\n" && err != errMenuCancelled {
				t.Fatalf("q did not cancel: %v", err)
			}
		})
	}
}

func TestMenuSceneSummaryResolvesDefaultAndDedicatedNodes(t *testing.T) {
	st := newStore()
	st.Nodes = []Node{{ID: "default", Name: "默认节点"}, {ID: "dedicated", Name: "独立节点"}}
	st.DefaultNodeID = "default"
	st.SceneNodes[SceneDev] = "dedicated"
	st.SceneEnabled[SceneGlobal] = true
	var output bytes.Buffer
	writeMenuSceneSummary(&output, st)
	for _, want := range []string{
		"全局代理：已开启；默认节点 [default]（跟随默认）",
		"开发代理：已关闭；独立节点 [dedicated]（单独指定）",
		"Telegram 服务代理：已关闭；默认节点 [default]（跟随默认）",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing effective selection %q: %s", want, output.String())
		}
	}
}

func TestMenuCancellationDoesNotCreateInstallation(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("maintenance preflight requires root")
	}
	for _, input := range []string{
		"8\n2\nn\n0\n0\n", "8\n2\nq\n0\n9\n", "8\n2\ny", "8\n2\n",
		"8\n1\nq\n0\n0\n", "8\n1\n", "8\n1\ntrojan://secret@example.invalid:443",
	} {
		t.Run(input, func(t *testing.T) {
			root := t.TempDir()
			cfg := DefaultConfig()
			cfg.CoreDir = filepath.Join(root, "uncreated-core")
			oldInstall, oldHost, oldOwnership := installLockPath, hostLockPath, hostOwnershipPath
			installLockPath = filepath.Join(root, "install.lock")
			hostLockPath = filepath.Join(root, "host.lock")
			hostOwnershipPath = filepath.Join(root, "owner.json")
			t.Cleanup(func() { installLockPath, hostLockPath, hostOwnershipPath = oldInstall, oldHost, oldOwnership })
			setMainMenuTestInput(t, input)
			_, err := captureMainMenuTestOutput(t, NewApp(cfg).menu)
			if err != nil && err != errMenuClosed {
				t.Fatalf("cancelled menu failed: %v", err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("cancellation reached a mutating operation: entries=%v err=%v", entries, err)
			}
		})
	}
}

func TestMenuNavigationAndLegacyExitLeaveStateUntouched(t *testing.T) {
	app := testApp(t)
	st := newStore()
	st.RuntimeConfig = app.cfg.runtimeConfig()
	if err := app.saveStore(st); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(app.cfg.StorePath())
	if err != nil {
		t.Fatal(err)
	}
	setMainMenuTestInput(t, "invalid\n4\n0\n5\n0\n6\n0\n7\n0\n8\n0\n9\n")
	output, err := captureMainMenuTestOutput(t, app.menu)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"无效选项", "节点管理", "订阅管理", "状态与连接检测", "程序更新", "安装与维护"} {
		if !strings.Contains(output, text) {
			t.Fatalf("missing menu route %s", text)
		}
	}
	after, err := os.ReadFile(app.cfg.StorePath())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("navigation mutated state: %v", err)
	}
}

func TestMenuEOFStopsNestedMenusAndPreservesCleanupErrors(t *testing.T) {
	app := testApp(t)
	for _, input := range []string{"4\n", "5\n", "6\n", "7\n", "8\n"} {
		t.Run(input, func(t *testing.T) {
			setMainMenuTestInput(t, input)
			output, err := captureMainMenuTestOutput(t, app.menu)
			if err != errMenuClosed {
				t.Fatalf("nested EOF did not propagate: %v", err)
			}
			if strings.Count(output, "========== proxyscene") != 1 {
				t.Fatal("EOF reopened the parent menu")
			}
		})
	}
	cleanupErr := errors.New("stage cleanup failed")
	joined := errors.Join(errMenuClosed, cleanupErr)
	if err := reportMenuAction(joined); !errors.Is(err, cleanupErr) {
		t.Fatalf("cleanup failure discarded: %v", err)
	}
}
