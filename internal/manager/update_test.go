package manager

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func setUpdateTestVersion(t *testing.T, version, commit string) {
	t.Helper()
	oldVersion, oldCommit := Version, Commit
	Version, Commit = version, commit
	t.Cleanup(func() { Version, Commit = oldVersion, oldCommit })
}

func TestParseUpdateOptions(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want updateOptions
	}{
		{nil, updateOptions{Source: "mirror"}},
		{[]string{"--check"}, updateOptions{Source: "mirror", Check: true}},
		{[]string{"--yes", "--source", "github"}, updateOptions{Source: "github", Yes: true}},
		{[]string{"--source", "mirror"}, updateOptions{Source: "mirror"}},
	} {
		got, err := parseUpdateOptions(tc.args)
		if err != nil || got != tc.want {
			t.Fatalf("parse %v: got=%+v err=%v", tc.args, got, err)
		}
	}
	for _, args := range [][]string{{"--yes", "--check"}, {"--source"}, {"--source", "https://private?token=SECRET"}, {"--source", "github", "--source", "mirror"}, {"--yes", "--yes"}, {"v9.9.9"}, {"SECRET"}} {
		_, err := parseUpdateOptions(args)
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("bad update arguments accepted/leaked: %v", err)
		}
	}
}

func TestUpdateCheckNeverApplies(t *testing.T) {
	for _, version := range []string{"dev", "0.9.2", "0.9.3", "0.9.4"} {
		t.Run(version, func(t *testing.T) {
			setUpdateTestVersion(t, version, strings.Repeat("a", 40))
			app := &App{}
			calls := 0
			attempted, err := app.checkAndUpdate(updateOptions{Check: true}, func(ctx context.Context) (updateRelease, error) {
				calls++
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("update metadata request lacks deadline")
				}
				return updateRelease{Tag: "v0.9.3", Commit: Commit}, nil
			}, func(updateRelease) (bool, error) { t.Fatal("read-only check invoked installation"); return true, nil })
			if err != nil || attempted || calls != 1 {
				t.Fatalf("read-only update check failed: %v %v", attempted, err)
			}
		})
	}
}

func TestUpdateRejectsIdentityMismatchAndDownloadError(t *testing.T) {
	setUpdateTestVersion(t, "0.9.2", strings.Repeat("a", 40))
	app := &App{}
	for _, networkError := range []bool{false, true} {
		attempted, err := app.checkAndUpdate(updateOptions{Check: true}, func(context.Context) (updateRelease, error) {
			if networkError {
				return updateRelease{}, errors.New("failed metadata")
			}
			return updateRelease{Tag: "v0.9.2", Commit: strings.Repeat("b", 40)}, nil
		}, func(updateRelease) (bool, error) { t.Fatal("invalid identity reached installation"); return true, nil })
		if err == nil || attempted {
			t.Fatal("identity/network failure accepted")
		}
	}
}

func TestUpdateApplyOnlyForNewStableVersionAndPropagatesExit(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("mutating update requires root")
	}
	for _, version := range []string{"dev", "0.9.2", "0.9.3", "0.9.4"} {
		t.Run(version, func(t *testing.T) {
			setUpdateTestVersion(t, version, strings.Repeat("a", 40))
			called := false
			sentinel := errors.New("installer failure after file commit")
			attempted, err := (&App{}).checkAndUpdate(updateOptions{Source: "mirror", Yes: true}, func(context.Context) (updateRelease, error) {
				return updateRelease{Tag: "v0.9.3", Commit: Commit}, nil
			}, func(updateRelease) (bool, error) { called = true; return true, sentinel })
			if version == "0.9.2" {
				if !called || !attempted || !errors.Is(err, sentinel) {
					t.Fatalf("installer exit signal lost: called=%t attempted=%t err=%v", called, attempted, err)
				}
			} else if called || attempted || (version == "dev" && err == nil) {
				t.Fatalf("unsupported/same/older installation attempted: %s, %v", version, err)
			}
		})
	}
}

func TestUpdateConfirmationCancelLeavesPreparedFilesUntouched(t *testing.T) {
	oldInput := stdinReader
	t.Cleanup(func() { stdinReader = oldInput })
	for _, answer := range []string{"\n", "n\n", "yesplease\n"} {
		stdinReader = bufio.NewReader(strings.NewReader(answer))
		attempted, err := (&App{}).installPreparedUpdate(updateOptions{}, updateRelease{Tag: "v0.9.3"}, filepath.Join(t.TempDir(), "not-created"), "", "amd64", func(string, Config, string) error {
			t.Fatal("cancelled upgrade invoked installer")
			return nil
		})
		if attempted || err != nil {
			t.Fatalf("cancel should neither install nor access files: %v", err)
		}
	}
}

func TestUpdateInstallerFailureAlwaysExitsOldMenu(t *testing.T) {
	bundle := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundle, "proxyscene"), []byte("verified fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: DefaultConfig()}
	before := app.cfg
	sentinel := errors.New("service initialization failed")
	attempted, err := app.installPreparedUpdate(updateOptions{Yes: true}, updateRelease{Tag: "v0.9.3"}, bundle, strings.Repeat("a", 64), "amd64", func(dir string, cfg Config, expected string) error {
		if dir != bundle || !reflect.DeepEqual(cfg, before) || expected != strings.Repeat("a", 64) {
			t.Fatal("installer arguments changed")
		}
		return sentinel
	})
	if !attempted || !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "可能已提交") {
		t.Fatalf("post-commit failure lost exit/repair information: attempted=%t err=%v", attempted, err)
	}
}

func TestUpdateSuccessClaimRequiresInstalledBytes(t *testing.T) {
	bundle := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundle, "proxyscene"), []byte("expected new manager"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{InstallBin: filepath.Join(t.TempDir(), "manager")}}
	if err := os.WriteFile(app.cfg.InstallBin, []byte("old manager"), 0o600); err != nil {
		t.Fatal(err)
	}
	attempted, err := app.installPreparedUpdate(updateOptions{Yes: true}, updateRelease{Tag: "v0.9.3"}, bundle, strings.Repeat("a", 64), "amd64", func(string, Config, string) error { return nil })
	if !attempted || err == nil || !strings.Contains(err.Error(), "摘要") {
		t.Fatalf("installer exit alone treated as success: %v", err)
	}
}

func TestHashUpdatePathRejectsSymlinkAndFIFO(t *testing.T) {
	root := t.TempDir()
	regular := filepath.Join(root, "regular")
	if err := os.WriteFile(regular, []byte("manager"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	fifo := filepath.Join(root, "fifo")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, fifo, root} {
		if _, err := hashUpdatePath(path); err == nil {
			t.Fatalf("unsafe hash input accepted: %s", path)
		}
	}
	if got, err := hashUpdatePath(regular); err != nil || len(got) != 64 {
		t.Fatalf("regular file hash failed: %v", err)
	}
}

func TestUpdatePreflightDoesNotMutateMenuRuntimeOverrides(t *testing.T) {
	app := testApp(t)
	st := newStore()
	st.RuntimeConfig = app.cfg.runtimeConfig()
	st.RuntimeConfig.GlobalHTTPPort = 17990
	if err := app.saveStore(st); err != nil {
		t.Fatal(err)
	}
	app.cfg.GlobalHTTPPort = 18990
	app.cfg.runtimeOverrides.GlobalHTTPPort = true
	before := app.cfg
	// The test executable is not the installed manager: preflight must reject
	// it after reading the persisted config, without changing the caller App.
	if _, err := app.preflightSelfUpdate("amd64"); err == nil {
		t.Fatal("test binary unexpectedly accepted as installed manager")
	}
	if !reflect.DeepEqual(app.cfg, before) {
		t.Fatalf("failed update changed caller runtime overrides: got=%+v want=%+v", app.cfg, before)
	}
}
