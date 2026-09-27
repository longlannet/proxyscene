package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

type fakeExitError int

func (e fakeExitError) Error() string { return "exit failure" }
func (e fakeExitError) ExitCode() int { return int(e) }

func gitNullOutput(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return strings.Join(values, "\x00") + "\x00"
}

func parseDevTestGitMutation(args []string) (action, key, value string, err error) {
	if len(args) < 7 || args[0] != "config" || args[1] != "--file" || args[2] == "" ||
		args[3] != "--no-includes" || args[5] != "--" {
		return "", "", "", fmt.Errorf("unexpected bound Git argv: %v", args)
	}
	action, key = args[4], args[6]
	switch action {
	case "--unset-all":
		if len(args) != 7 {
			return "", "", "", fmt.Errorf("unexpected unset argv: %v", args)
		}
	case "--add", "--replace-all":
		if len(args) != 8 {
			return "", "", "", fmt.Errorf("unexpected value argv: %v", args)
		}
		value = args[7]
	default:
		return "", "", "", fmt.Errorf("unexpected Git action: %v", args)
	}
	return action, key, value, nil
}

func stubDevCommands(t *testing.T) {
	t.Helper()
	oldExists := devCommandExists
	oldOutput := devOutputAsUser
	oldRun := devRunAsUser
	oldRemove := devRemoveBackup
	oldFsync := devBackupDirFsync
	oldLookup := devLookupUserIdentity
	oldResolveTopology := devResolveGitTopology
	oldValidateTopology := devValidateGitTopology
	oldGitConfigExists := devGitConfigExists
	oldReadNPM := devReadNPMConfig
	oldMutateNPM := devMutateNPMConfig
	oldMutateGit := devMutateGitConfig
	devReadNPMConfig = func(user string, identity *persistedUserIdentity, key string) (*string, error) {
		out, err := devOutputAsUser(user, identity, "npm", "config", "get", key)
		if err != nil {
			return nil, err
		}
		lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
		value := ""
		for i := len(lines) - 1; i >= 0; i-- {
			if trimmed := strings.TrimSpace(lines[i]); trimmed != "" {
				value = trimmed
				break
			}
		}
		if value == "" || value == "undefined" || value == "null" {
			return nil, nil
		}
		return &value, nil
	}
	devMutateNPMConfig = func(user string, identity *persistedUserIdentity, key string, expected, desired *string) error {
		if desired != nil {
			return runDevAsPersistedUser(user, identity, "npm", "config", "set", key, *desired)
		}
		return runDevAsPersistedUser(user, identity, "npm", "config", "delete", key)
	}
	devMutateGitConfig = func(user string, identity *persistedUserIdentity, location devGitConfigLocation, expected []string, args ...string) error {
		return runGitConfigMutationForIdentity(user, identity, location, args...)
	}
	devResolveGitTopology = func(string, *persistedUserIdentity) (devGitConfigLocation, error) { return devGitConfigHome, nil }
	devValidateGitTopology = func(string, *persistedUserIdentity, devGitConfigLocation) error { return nil }
	devGitConfigExists = func(*persistedUserIdentity, devGitConfigLocation) (bool, error) { return true, nil }
	t.Cleanup(func() {
		devCommandExists = oldExists
		devOutputAsUser = oldOutput
		devRunAsUser = oldRun
		devRemoveBackup = oldRemove
		devBackupDirFsync = oldFsync
		devLookupUserIdentity = oldLookup
		devResolveGitTopology = oldResolveTopology
		devValidateGitTopology = oldValidateTopology
		devGitConfigExists = oldGitConfigExists
		devReadNPMConfig = oldReadNPM
		devMutateNPMConfig = oldMutateNPM
		devMutateGitConfig = oldMutateGit
	})
}

func devTestIdentity(t *testing.T, user string) *persistedUserIdentity {
	t.Helper()
	identity, err := capturePersistedUserIdentity(user, devLookupUserIdentity)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func TestGetGitConfigAllDistinguishesUnsetFromFailure(t *testing.T) {
	stubDevCommands(t)
	devOutputAsUser = func(string, *persistedUserIdentity, string, ...string) (string, error) { return "", fakeExitError(1) }
	identity := &persistedUserIdentity{Home: "/tmp"}
	values, err := getGitConfigAll("root", identity, devGitConfigHome, "http.proxy")
	if err != nil || values != nil {
		t.Fatalf("exit 1 should mean unset: values=%v err=%v", values, err)
	}

	devOutputAsUser = func(string, *persistedUserIdentity, string, ...string) (string, error) { return "", fakeExitError(2) }
	if _, err := getGitConfigAll("root", identity, devGitConfigHome, "http.proxy"); err == nil {
		t.Fatal("non-1 git failure must be propagated")
	}
}

func TestValidateDevGitGlobalTopologyFailsClosed(t *testing.T) {
	stubDevCommands(t)
	identityForHome := func(home string) *persistedUserIdentity {
		info, err := os.Stat(home)
		if err != nil {
			t.Fatal(err)
		}
		stat := info.Sys().(*syscall.Stat_t)
		current := localUserIdentity{Name: "alice", UID: int(stat.Uid), GID: int(stat.Gid), UIDText: strconv.Itoa(int(stat.Uid)), GIDText: strconv.Itoa(int(stat.Gid)), Home: home}
		devLookupUserIdentity = func(user string) (localUserIdentity, error) {
			if user != current.Name {
				return localUserIdentity{}, errors.New("unexpected user")
			}
			return current, nil
		}
		return &persistedUserIdentity{UID: current.UID, GID: current.GID, Home: home}
	}

	t.Run("two global files", func(t *testing.T) {
		home := t.TempDir()
		if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = Alice\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		xdgDir := filepath.Join(home, ".config", "git")
		if err := os.MkdirAll(xdgDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(xdgDir, "config"), []byte("[user]\n\temail = alice@example.invalid\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		devOutputAsUser = func(string, *persistedUserIdentity, string, ...string) (string, error) {
			t.Fatal("dual global files must fail before invoking git")
			return "", nil
		}
		_, err := resolveDevGitGlobalTopology("alice", identityForHome(home))
		if err == nil || !strings.Contains(err.Error(), "多个 Git global 配置文件") {
			t.Fatalf("dual global files were not rejected: %v", err)
		}
	})

	t.Run("include directive", func(t *testing.T) {
		home := t.TempDir()
		if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[include]\n\tpath = other\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		devOutputAsUser = func(_ string, _ *persistedUserIdentity, name string, args ...string) (string, error) {
			if name != "git" || !slices.Equal(args, []string{"config", "--file", filepath.Join(home, ".gitconfig"), "--no-includes", "--null", "--name-only", "--list"}) {
				return "", errors.New("unexpected topology query")
			}
			return "user.name\x00include.path\x00", nil
		}
		_, err := resolveDevGitGlobalTopology("alice", identityForHome(home))
		if err == nil || !strings.Contains(err.Error(), "跨 include 精确恢复") {
			t.Fatalf("global include was not rejected: %v", err)
		}
	})

	t.Run("symlinked config", func(t *testing.T) {
		home := t.TempDir()
		target := filepath.Join(home, "managed-gitconfig")
		if err := os.WriteFile(target, []byte("[user]\n\tname = Alice\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(home, ".gitconfig")); err != nil {
			t.Fatal(err)
		}
		_, err := resolveDevGitGlobalTopology("alice", identityForHome(home))
		if err == nil || !strings.Contains(err.Error(), "拒绝符号链接") {
			t.Fatalf("symlinked global config was not rejected: %v", err)
		}
	})

	t.Run("single direct global file", func(t *testing.T) {
		home := t.TempDir()
		if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = Alice\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		devOutputAsUser = func(string, *persistedUserIdentity, string, ...string) (string, error) {
			return "user.name\x00user.email\x00", nil
		}
		location, err := resolveDevGitGlobalTopology("alice", identityForHome(home))
		if err != nil {
			t.Fatalf("single direct global file was rejected: %v", err)
		}
		if location != devGitConfigHome {
			t.Fatalf("single home config location = %q", location)
		}
	})

	t.Run("no global file defaults to home", func(t *testing.T) {
		home := t.TempDir()
		devOutputAsUser = func(string, *persistedUserIdentity, string, ...string) (string, error) {
			t.Fatal("missing config must not invoke git during topology resolution")
			return "", nil
		}
		location, err := resolveDevGitGlobalTopology("alice", identityForHome(home))
		if err != nil || location != devGitConfigHome {
			t.Fatalf("missing config resolved to %q: %v", location, err)
		}
	})

	t.Run("single xdg file", func(t *testing.T) {
		home := t.TempDir()
		xdgDir := filepath.Join(home, ".config", "git")
		if err := os.MkdirAll(xdgDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(xdgDir, "config"), []byte("[user]\n\tname = Alice\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		devOutputAsUser = func(_ string, _ *persistedUserIdentity, name string, args ...string) (string, error) {
			want := []string{"config", "--file", filepath.Join(xdgDir, "config"), "--no-includes", "--null", "--name-only", "--list"}
			if name != "git" || !slices.Equal(args, want) {
				return "", fmt.Errorf("unexpected XDG topology query: %s %v", name, args)
			}
			return "user.name\x00", nil
		}
		location, err := resolveDevGitGlobalTopology("alice", identityForHome(home))
		if err != nil || location != devGitConfigXDG {
			t.Fatalf("single XDG config resolved to %q: %v", location, err)
		}
	})

	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, home string)
	}{
		{
			name: "symlinked dot-config directory",
			setup: func(t *testing.T, home string) {
				t.Helper()
				target := t.TempDir()
				if err := os.Symlink(target, filepath.Join(home, ".config")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlinked git directory",
			setup: func(t *testing.T, home string) {
				t.Helper()
				if err := os.Mkdir(filepath.Join(home, ".config"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), filepath.Join(home, ".config", "git")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "fifo final file",
			setup: func(t *testing.T, home string) {
				t.Helper()
				if err := syscall.Mkfifo(filepath.Join(home, ".gitconfig"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "directory final file",
			setup: func(t *testing.T, home string) {
				t.Helper()
				if err := os.Mkdir(filepath.Join(home, ".gitconfig"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "socket final file",
			setup: func(t *testing.T, home string) {
				t.Helper()
				listener, err := net.Listen("unix", filepath.Join(home, ".gitconfig"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = listener.Close() })
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			tc.setup(t, home)
			devOutputAsUser = func(string, *persistedUserIdentity, string, ...string) (string, error) {
				t.Fatal("unsafe topology must fail before invoking git")
				return "", nil
			}
			if _, err := resolveDevGitGlobalTopology("alice", identityForHome(home)); err == nil {
				t.Fatal("unsafe Git global topology was accepted")
			}
		})
	}
}

func TestValidateDevGitGlobalTopologyRejectsEveryIncludeForm(t *testing.T) {
	stubDevCommands(t)
	home := t.TempDir()
	info, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	identity := &persistedUserIdentity{UID: int(stat.Uid), GID: int(stat.Gid), Home: home}
	devLookupUserIdentity = func(string) (localUserIdentity, error) {
		return localUserIdentity{Name: "alice", UID: identity.UID, GID: identity.GID, UIDText: strconv.Itoa(identity.UID), GIDText: strconv.Itoa(identity.GID), Home: home}, nil
	}
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = Alice\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"include.path", "Include.Path", "includeIf.gitdir:/work/.path", "INCLUDEIF.ONBRANCH:main.PATH"} {
		t.Run(key, func(t *testing.T) {
			devOutputAsUser = func(string, *persistedUserIdentity, string, ...string) (string, error) {
				return "user.name\x00" + key + "\x00", nil
			}
			if err := validateDevGitGlobalTopology("alice", identity, devGitConfigHome); err == nil || !strings.Contains(err.Error(), "跨 include") {
				t.Fatalf("include key %q was not rejected: %v", key, err)
			}
		})
	}
}

func TestParseGitNullOutputPreservesValuesExactly(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want []string
	}{
		{name: "multiple", out: "one\x00two\x00", want: []string{"one", "two"}},
		{name: "empty value", out: "\x00", want: []string{""}},
		{name: "trailing whitespace", out: "value \x00", want: []string{"value "}},
		{name: "embedded newline", out: "line one\nline two\x00", want: []string{"line one\nline two"}},
		{name: "unset", out: "", want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseGitNullOutput(tc.out)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("parseGitNullOutput(%q) = %#v, %v; want %#v", tc.out, got, err, tc.want)
			}
		})
	}
	if _, err := parseGitNullOutput("unterminated"); err == nil {
		t.Fatal("unterminated Git output was accepted")
	}
}

func TestGitConfigOperationsUseBoundFileAndNullProtocol(t *testing.T) {
	stubDevCommands(t)
	identity := devTestIdentity(t, "root")
	path := filepath.Join(identity.Home, ".config", "git", "config")
	devOutputAsUser = func(_ string, _ *persistedUserIdentity, name string, args ...string) (string, error) {
		want := []string{"config", "--file", path, "--no-includes", "--null", "--get-all", "--", "http.proxy"}
		if name != "git" || !slices.Equal(args, want) {
			return "", fmt.Errorf("unexpected Git read argv: %s %v", name, args)
		}
		return "value with space \x00", nil
	}
	values, err := getGitConfigAllForIdentity("root", identity, devGitConfigXDG, "http.proxy")
	if err != nil || !slices.Equal(values, []string{"value with space "}) {
		t.Fatalf("bound Git read = %#v, %v", values, err)
	}
	devRunAsUser = func(_ string, _ *persistedUserIdentity, name string, args ...string) error {
		want := []string{"config", "--file", path, "--no-includes", "--replace-all", "--", "http.proxy", "http://127.0.0.1:7891"}
		if name != "git" || !slices.Equal(args, want) {
			return fmt.Errorf("unexpected Git write argv: %s %v", name, args)
		}
		return nil
	}
	if err := runGitConfigForIdentity("root", identity, devGitConfigXDG, "--replace-all", "--", "http.proxy", "http://127.0.0.1:7891"); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyManagedGitProxyRejectsAdditionalValues(t *testing.T) {
	stubDevCommands(t)
	identity := devTestIdentity(t, "root")
	devOutputAsUser = func(string, *persistedUserIdentity, string, ...string) (string, error) {
		return "http://127.0.0.1:7891\x00http://operator.invalid:8080\x00", nil
	}
	err := verifyManagedGitProxy("root", identity, devGitConfigHome, "http.proxy", "http://127.0.0.1:7891")
	if err == nil || !strings.Contains(err.Error(), "不是唯一受管值") {
		t.Fatalf("additional Git proxy value was not rejected: %v", err)
	}
}

func TestGetNPMConfigDistinguishesUnsetFromFailure(t *testing.T) {
	stubDevCommands(t)
	devOutputAsUser = func(string, *persistedUserIdentity, string, ...string) (string, error) { return "undefined\n", nil }
	value, err := getNPMConfig("root", nil, "proxy")
	if err != nil || value != nil {
		t.Fatalf("undefined should mean unset: value=%v err=%v", value, err)
	}

	devOutputAsUser = func(string, *persistedUserIdentity, string, ...string) (string, error) {
		return "", errors.New("timeout")
	}
	if _, err := getNPMConfig("root", nil, "proxy"); err == nil {
		t.Fatal("npm command failure must be propagated")
	}
}

func TestBackupDevConfigAbortsOnReadFailure(t *testing.T) {
	stubDevCommands(t)
	a := testApp(t)
	devCommandExists = func(name string) bool { return name == "git" }
	devOutputAsUser = func(string, *persistedUserIdentity, string, ...string) (string, error) { return "", fakeExitError(7) }

	err := a.backupDevConfig("root")
	if err == nil || !strings.Contains(err.Error(), "读取 git http.proxy 原值失败") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(a.cfg.DevBackupPath()); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("backup must not be written after a read failure: %v", statErr)
	}
}

func TestBackupDevConfigBindsGitLocationAndRejectsLaterSwitch(t *testing.T) {
	stubDevCommands(t)
	a := testApp(t)
	home := t.TempDir()
	info, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	identity := localUserIdentity{Name: "alice", UID: int(stat.Uid), GID: int(stat.Gid), UIDText: strconv.Itoa(int(stat.Uid)), GIDText: strconv.Itoa(int(stat.Gid)), Home: home}
	devLookupUserIdentity = func(user string) (localUserIdentity, error) {
		if user != "alice" {
			return localUserIdentity{}, fmt.Errorf("unexpected user %s", user)
		}
		return identity, nil
	}
	devResolveGitTopology = resolveDevGitGlobalTopology
	devValidateGitTopology = validateDevGitGlobalTopology
	devGitConfigExists = devGitConfigLocationExists
	homeConfig := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(homeConfig, []byte("[user]\n\tname = Alice\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	devCommandExists = func(name string) bool { return name == "git" }
	devOutputAsUser = func(_ string, _ *persistedUserIdentity, name string, args ...string) (string, error) {
		if name != "git" {
			return "", fmt.Errorf("unexpected command %s", name)
		}
		if slices.Contains(args, "--name-only") {
			return "user.name\x00", nil
		}
		return "operator-original\x00", nil
	}
	if err := a.backupDevConfig("alice"); err != nil {
		t.Fatal(err)
	}
	backup, err := a.loadDevBackup()
	if err != nil {
		t.Fatal(err)
	}
	if backup.GitConfigLocation != devGitConfigHome {
		t.Fatalf("recorded location = %q, want home", backup.GitConfigLocation)
	}
	if err := os.Remove(homeConfig); err != nil {
		t.Fatal(err)
	}
	xdgDir := filepath.Join(home, ".config", "git")
	if err := os.MkdirAll(xdgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdgDir, "config"), []byte("[user]\n\tname = Alice\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	devOutputAsUser = func(string, *persistedUserIdentity, string, ...string) (string, error) {
		t.Fatal("location switch must fail before invoking git")
		return "", nil
	}
	if err := a.backupDevConfig("alice"); err == nil || !strings.Contains(err.Error(), "拓扑已偏离记录位置") {
		t.Fatalf("Git global location switch was not rejected: %v", err)
	}
	if _, err := os.Stat(a.cfg.DevBackupPath()); err != nil {
		t.Fatalf("location drift removed ownership backup: %v", err)
	}
}

func TestRestoreDevGitFailsClosedOnSecondFileOrInclude(t *testing.T) {
	for _, tc := range []struct {
		name       string
		addHazard  func(t *testing.T, home string)
		includeKey string
	}{
		{
			name: "second global file",
			addHazard: func(t *testing.T, home string) {
				t.Helper()
				xdgDir := filepath.Join(home, ".config", "git")
				if err := os.MkdirAll(xdgDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(xdgDir, "config"), []byte("[user]\n\tname = Alice\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{name: "new inactive includeIf", includeKey: "includeIf.gitdir:/never-matches/.path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubDevCommands(t)
			a := testApp(t)
			home := t.TempDir()
			info, err := os.Stat(home)
			if err != nil {
				t.Fatal(err)
			}
			stat := info.Sys().(*syscall.Stat_t)
			identity := &persistedUserIdentity{UID: int(stat.Uid), GID: int(stat.Gid), Home: home}
			devLookupUserIdentity = func(string) (localUserIdentity, error) {
				return localUserIdentity{Name: "alice", UID: identity.UID, GID: identity.GID, UIDText: strconv.Itoa(identity.UID), GIDText: strconv.Itoa(identity.GID), Home: home}, nil
			}
			devValidateGitTopology = validateDevGitGlobalTopology
			devGitConfigExists = devGitConfigLocationExists
			if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = Alice\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			proxy := a.cfg.HTTPAddr(SceneDev)
			backup := &devProxyBackup{
				Version:             devProxyBackupVersion,
				User:                "alice",
				Identity:            identity,
				ToolsRecorded:       true,
				GitManaged:          true,
				GitConfigLocation:   devGitConfigHome,
				ManagedHTTPProxies:  []string{proxy},
				ManagedHTTPSProxies: []string{proxy},
			}
			if err := a.writeDevBackup(backup); err != nil {
				t.Fatal(err)
			}
			if tc.addHazard != nil {
				tc.addHazard(t, home)
			}
			devCommandExists = func(name string) bool { return name == "git" }
			devOutputAsUser = func(_ string, _ *persistedUserIdentity, name string, args ...string) (string, error) {
				if name != "git" {
					return "", fmt.Errorf("unexpected command %s", name)
				}
				if slices.Contains(args, "--name-only") {
					if tc.includeKey == "" {
						return "user.name\x00", nil
					}
					return "user.name\x00" + tc.includeKey + "\x00", nil
				}
				return proxy + "\x00", nil
			}
			devRunAsUser = func(string, *persistedUserIdentity, string, ...string) error {
				t.Fatal("unsafe restore topology must not mutate Git config")
				return nil
			}
			if err := a.restoreDev(); err == nil {
				t.Fatal("unsafe restore topology was accepted")
			}
			if _, err := os.Stat(a.cfg.DevBackupPath()); err != nil {
				t.Fatalf("unsafe restore removed ownership backup: %v", err)
			}
		})
	}
}

func TestDevSnapshotGitLocationMismatchFailsClosed(t *testing.T) {
	stubDevCommands(t)
	a := testApp(t)
	identity := devTestIdentity(t, "root")
	proxy := a.cfg.HTTPAddr(SceneDev)
	ownership := &devProxyBackup{
		Version:             devProxyBackupVersion,
		User:                "root",
		Identity:            identity,
		ToolsRecorded:       true,
		GitManaged:          true,
		GitConfigLocation:   devGitConfigHome,
		ManagedHTTPProxies:  []string{proxy},
		ManagedHTTPSProxies: []string{proxy},
	}
	if err := a.writeDevBackup(ownership); err != nil {
		t.Fatal(err)
	}
	snapshot := &devProxyBackup{
		Version:           devProxyBackupVersion,
		User:              "root",
		Identity:          identity,
		ToolsRecorded:     true,
		GitManaged:        true,
		GitConfigLocation: devGitConfigXDG,
	}
	devCommandExists = func(string) bool {
		t.Fatal("location mismatch must fail before tool access")
		return false
	}
	if err := a.restoreDevProxySnapshotDurable(snapshot, proxy); err == nil {
		t.Fatalf("snapshot location mismatch was not rejected: %v", err)
	}
	if _, err := os.Stat(a.cfg.DevBackupPath()); err != nil {
		t.Fatalf("snapshot mismatch removed ownership backup: %v", err)
	}
}

func TestDevBackupV2RequiresGitLocationAndExactValidValues(t *testing.T) {
	identity := &persistedUserIdentity{UID: 0, GID: 0, Home: "/root"}
	base := devProxyBackup{Version: devProxyBackupVersion, User: "root", Identity: identity, ToolsRecorded: true}
	tests := []struct {
		name   string
		mutate func(*devProxyBackup)
	}{
		{name: "managed without location", mutate: func(b *devProxyBackup) { b.GitManaged = true }},
		{name: "location without managed", mutate: func(b *devProxyBackup) { b.GitConfigLocation = devGitConfigHome }},
		{name: "empty Git value", mutate: func(b *devProxyBackup) {
			b.GitManaged = true
			b.GitConfigLocation = devGitConfigHome
			b.GitHTTPProxy = []string{""}
		}},
		{name: "trailing whitespace Git value", mutate: func(b *devProxyBackup) {
			b.GitManaged = true
			b.GitConfigLocation = devGitConfigHome
			b.GitHTTPProxy = []string{"value "}
		}},
		{name: "v1 missing binding", mutate: func(b *devProxyBackup) { b.Version = 1; b.GitManaged = true }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			backup := base
			tc.mutate(&backup)
			if err := validateDevProxyBackup(&backup); err == nil {
				t.Fatal("invalid backup was accepted")
			}
		})
	}
	valid := base
	valid.GitManaged = true
	valid.GitConfigLocation = devGitConfigHome
	valid.GitHTTPProxy = []string{"line one\nline two"}
	if err := validateDevProxyBackup(&valid); err != nil {
		t.Fatalf("embedded-newline Git value was not preserved as valid: %v", err)
	}
}

func TestRestoreDevOnlyRequiresToolsRecordedAsManaged(t *testing.T) {
	stubDevCommands(t)
	a := testApp(t)
	backup := devProxyBackup{
		Version:             devProxyBackupVersion,
		User:                "root",
		Identity:            devTestIdentity(t, "root"),
		ToolsRecorded:       true,
		NPMManaged:          true,
		ManagedHTTPProxies:  []string{a.cfg.HTTPAddr(SceneDev)},
		ManagedHTTPSProxies: []string{a.cfg.HTTPAddr(SceneDev)},
	}
	raw, err := json.Marshal(backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(a.cfg.DevBackupPath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	devCommandExists = func(name string) bool { return name == "npm" }
	devOutputAsUser = func(_ string, _ *persistedUserIdentity, name string, _ ...string) (string, error) {
		if name != "npm" {
			return "", errors.New("unexpected tool read")
		}
		return "undefined\n", nil
	}
	devRunAsUser = func(_ string, _ *persistedUserIdentity, name string, _ ...string) error {
		if name != "npm" {
			return errors.New("unexpected tool write")
		}
		return nil
	}
	if err := a.restoreDev(); err != nil {
		t.Fatalf("npm-only backup should restore without git: %v", err)
	}
}

func TestRestoreDevWithoutBackupDoesNotInferOwnershipFromValue(t *testing.T) {
	stubDevCommands(t)
	a := testApp(t)
	devCommandExists = func(string) bool {
		t.Fatal("missing backup must not inspect git/npm")
		return false
	}
	devOutputAsUser = func(string, *persistedUserIdentity, string, ...string) (string, error) {
		t.Fatal("missing backup must not read operator configuration")
		return "", nil
	}
	devRunAsUser = func(string, *persistedUserIdentity, string, ...string) error {
		t.Fatal("missing backup must not modify operator configuration")
		return nil
	}
	if err := a.restoreDev(); err != nil {
		t.Fatalf("missing ownership backup should be a conservative no-op: %v", err)
	}
}

func TestRestoreDevIdentityDriftRetainsBackupWithoutToolAccess(t *testing.T) {
	stubDevCommands(t)
	a := testApp(t)
	identity := localUserIdentity{Name: "root", UID: 0, GID: 0, UIDText: "0", GIDText: "0", Home: t.TempDir()}
	devLookupUserIdentity = func(user string) (localUserIdentity, error) {
		if user != "root" {
			return localUserIdentity{}, errors.New("unexpected user")
		}
		return identity, nil
	}
	devCommandExists = func(string) bool { return false }
	if err := a.backupDevConfig("root"); err != nil {
		t.Fatal(err)
	}
	devCommandExists = func(string) bool {
		t.Fatal("identity drift must be rejected before checking managed tools")
		return false
	}
	devOutputAsUser = func(string, *persistedUserIdentity, string, ...string) (string, error) {
		t.Fatal("identity drift must not read replacement-account config")
		return "", nil
	}
	devRunAsUser = func(string, *persistedUserIdentity, string, ...string) error {
		t.Fatal("identity drift must not write replacement-account config")
		return nil
	}
	identity.UID = 4242
	identity.UIDText = "4242"
	identity.Home = t.TempDir()

	err := a.restoreDev()
	if err == nil || !strings.Contains(err.Error(), "身份已变化") {
		t.Fatalf("identity drift was not rejected: %v", err)
	}
	if _, statErr := os.Stat(a.cfg.DevBackupPath()); statErr != nil {
		t.Fatalf("identity drift must retain durable backup: %v", statErr)
	}
}

func TestRestoreDevLegacyBackupWithoutIdentityFailsClosed(t *testing.T) {
	stubDevCommands(t)
	a := testApp(t)
	legacy := []byte(`{"user":"root","tools_recorded":true,"git_managed":true}`)
	if err := writeFileAtomic(a.cfg.DevBackupPath(), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	devCommandExists = func(string) bool {
		t.Fatal("legacy backup must fail before checking tools")
		return false
	}
	devOutputAsUser = func(string, *persistedUserIdentity, string, ...string) (string, error) {
		t.Fatal("legacy backup must not read same-name current account")
		return "", nil
	}
	devRunAsUser = func(string, *persistedUserIdentity, string, ...string) error {
		t.Fatal("legacy backup must not write same-name current account")
		return nil
	}

	err := a.restoreDev()
	if err == nil || !strings.Contains(err.Error(), "版本无效或缺少安全身份绑定") {
		t.Fatalf("legacy backup did not fail closed: %v", err)
	}
	if _, statErr := os.Stat(a.cfg.DevBackupPath()); statErr != nil {
		t.Fatalf("legacy backup evidence was not retained: %v", statErr)
	}
}

func TestApplyDevRollsBackEveryToolAfterPartialFailure(t *testing.T) {
	stubDevCommands(t)
	a := testApp(t)
	a.cfg.DevTargetUser = "root"
	proxy := a.cfg.HTTPAddr(SceneDev)
	gitValues := map[string][]string{
		"http.proxy":  {"git-http-original", "git-http-duplicate", "git-http-duplicate"},
		"https.proxy": {"git-https-original"},
	}
	npmValues := map[string]*string{}
	npmOriginal := "npm-original"
	npmValues["proxy"] = &npmOriginal
	var gitReplaceKeys []string

	devCommandExists = func(name string) bool { return name == "git" || name == "npm" }
	devOutputAsUser = func(_ string, _ *persistedUserIdentity, name string, args ...string) (string, error) {
		switch name {
		case "git":
			key := args[len(args)-1]
			values := gitValues[key]
			if len(values) == 0 {
				return "", fakeExitError(1)
			}
			return gitNullOutput(values), nil
		case "npm":
			key := args[len(args)-1]
			if value := npmValues[key]; value != nil {
				return *value + "\n", nil
			}
			return "undefined\n", nil
		default:
			return "", errors.New("unexpected command")
		}
	}
	failNPMHTTPSOnce := true
	devRunAsUser = func(_ string, _ *persistedUserIdentity, name string, args ...string) error {
		switch name {
		case "git":
			action, key, value, err := parseDevTestGitMutation(args)
			if err != nil {
				return err
			}
			if action == "--unset-all" {
				delete(gitValues, key)
				return nil
			}
			if action == "--add" {
				gitValues[key] = append(gitValues[key], value)
				return nil
			}
			if action != "--replace-all" {
				return errors.New("git apply must use --replace-all")
			}
			gitReplaceKeys = append(gitReplaceKeys, key)
			gitValues[key] = []string{value}
			return nil
		case "npm":
			action := args[1]
			key := args[2]
			if action == "set" {
				if key == "https-proxy" && args[3] == proxy && failNPMHTTPSOnce {
					failNPMHTTPSOnce = false
					return errors.New("npm https apply failed")
				}
				value := args[3]
				npmValues[key] = &value
				return nil
			}
			delete(npmValues, key)
			return nil
		default:
			return errors.New("unexpected command")
		}
	}

	err := a.applyDev()
	if err == nil || !strings.Contains(err.Error(), "npm https apply failed") {
		t.Fatalf("partial apply failure not returned: %v", err)
	}
	if !slices.Equal(gitReplaceKeys, []string{"http.proxy", "https.proxy"}) {
		t.Fatalf("git apply did not replace every existing value: %v", gitReplaceKeys)
	}
	wantHTTP := []string{"git-http-original", "git-http-duplicate", "git-http-duplicate"}
	if strings.Join(gitValues["http.proxy"], "\x00") != strings.Join(wantHTTP, "\x00") {
		t.Fatalf("git http values were not exactly rolled back: %v", gitValues["http.proxy"])
	}
	if got := gitValues["https.proxy"]; len(got) != 1 || got[0] != "git-https-original" {
		t.Fatalf("git https values were not rolled back: %v", got)
	}
	if got := npmValues["proxy"]; got == nil || *got != npmOriginal {
		t.Fatalf("npm proxy was not rolled back: %v", got)
	}
	if got := npmValues["https-proxy"]; got != nil {
		t.Fatalf("npm https-proxy should remain unset: %v", *got)
	}
	if _, err := os.Stat(a.cfg.DevBackupPath()); err != nil {
		t.Fatalf("durable backup must remain for outer recovery: %v", err)
	}
}

func TestApplyDevFirstWriteFailureDoesNotRewriteSnapshot(t *testing.T) {
	stubDevCommands(t)
	a := testApp(t)
	a.cfg.DevTargetUser = "root"
	devCommandExists = func(name string) bool { return name == "git" }
	devOutputAsUser = func(string, *persistedUserIdentity, string, ...string) (string, error) {
		return "managed-looking-original\x00extra\x00extra\x00", nil
	}
	calls := 0
	devRunAsUser = func(string, *persistedUserIdentity, string, ...string) error {
		calls++
		return errors.New("first atomic git write failed")
	}
	if err := a.applyDev(); err == nil || !strings.Contains(err.Error(), "first atomic git write failed") {
		t.Fatalf("first write failure not returned: %v", err)
	}
	if calls != 1 {
		t.Fatalf("first write failure triggered unnecessary snapshot rewrite: calls=%d", calls)
	}
}

func TestApplyDevRollsBackFirstGitWriteWhenPostCheckFails(t *testing.T) {
	stubDevCommands(t)
	a := testApp(t)
	a.cfg.DevTargetUser = "root"
	proxy := a.cfg.HTTPAddr(SceneDev)
	values := map[string][]string{
		"http.proxy":  {"http-original"},
		"https.proxy": {"https-original"},
	}
	devCommandExists = func(name string) bool { return name == "git" }
	devOutputAsUser = func(_ string, _ *persistedUserIdentity, name string, args ...string) (string, error) {
		if name != "git" {
			return "", fmt.Errorf("unexpected command %s", name)
		}
		current := values[args[len(args)-1]]
		if len(current) == 0 {
			return "", fakeExitError(1)
		}
		return gitNullOutput(current), nil
	}
	failNextTopologyCheck := false
	devValidateGitTopology = func(string, *persistedUserIdentity, devGitConfigLocation) error {
		if failNextTopologyCheck {
			failNextTopologyCheck = false
			return errors.New("post-write topology changed")
		}
		return nil
	}
	devRunAsUser = func(_ string, _ *persistedUserIdentity, name string, args ...string) error {
		if name != "git" {
			return fmt.Errorf("unexpected command %s", name)
		}
		action, key, value, err := parseDevTestGitMutation(args)
		if err != nil {
			return err
		}
		switch action {
		case "--replace-all":
			values[key] = []string{value}
			if key == "http.proxy" && value == proxy {
				failNextTopologyCheck = true
			}
		case "--unset-all":
			delete(values, key)
		case "--add":
			values[key] = append(values[key], value)
		}
		return nil
	}

	err := a.applyDev()
	if err == nil || !strings.Contains(err.Error(), "post-write topology changed") {
		t.Fatalf("post-write topology failure not returned: %v", err)
	}
	if !slices.Equal(values["http.proxy"], []string{"http-original"}) ||
		!slices.Equal(values["https.proxy"], []string{"https-original"}) {
		t.Fatalf("first Git write was not rolled back: %v", values)
	}
	if _, err := os.Stat(a.cfg.DevBackupPath()); err != nil {
		t.Fatalf("outer ownership backup was not retained: %v", err)
	}
}

func TestDevApplyRollbackReplaysCrashAcrossAllKeys(t *testing.T) {
	cases := []struct {
		name    string
		crashAt string
	}{
		{name: "git-http-after-unset", crashAt: "git --unset-all http.proxy"},
		{name: "git-https-after-add", crashAt: "git --add https.proxy"},
		{name: "npm-proxy-after-set", crashAt: "npm set proxy"},
		{name: "npm-https-after-delete", crashAt: "npm delete https-proxy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubDevCommands(t)
			a := testApp(t)
			proxy := a.cfg.HTTPAddr(SceneDev)
			identity := devTestIdentity(t, "root")
			ownership := &devProxyBackup{
				Version:             devProxyBackupVersion,
				User:                "root",
				Identity:            identity,
				ToolsRecorded:       true,
				GitManaged:          true,
				GitConfigLocation:   devGitConfigHome,
				NPMManaged:          true,
				GitHTTPProxy:        []string{"ownership-http"},
				GitHTTPSProxy:       []string{"ownership-https"},
				ManagedHTTPProxy:    proxy,
				ManagedHTTPSProxy:   proxy,
				ManagedHTTPProxies:  []string{proxy},
				ManagedHTTPSProxies: []string{proxy},
			}
			if err := a.writeDevBackup(ownership); err != nil {
				t.Fatal(err)
			}
			npmBefore := "npm-before"
			snapshot := &devProxyBackup{
				Version:           devProxyBackupVersion,
				User:              "root",
				Identity:          identity,
				ToolsRecorded:     true,
				GitManaged:        true,
				GitConfigLocation: devGitConfigHome,
				NPMManaged:        true,
				GitHTTPProxy:      []string{"http-before-a", "http-before-b"},
				GitHTTPSProxy:     []string{"https-before"},
				NPMProxy:          &npmBefore,
			}
			gitValues := map[string][]string{
				"http.proxy":  {proxy},
				"https.proxy": {proxy},
			}
			npmHTTP := proxy
			npmHTTPS := proxy
			npmValues := map[string]*string{
				"proxy":       &npmHTTP,
				"https-proxy": &npmHTTPS,
			}
			devCommandExists = func(name string) bool { return name == "git" || name == "npm" }
			devOutputAsUser = func(_ string, _ *persistedUserIdentity, name string, args ...string) (string, error) {
				key := args[len(args)-1]
				switch name {
				case "git":
					if len(gitValues[key]) == 0 {
						return "", fakeExitError(1)
					}
					return gitNullOutput(gitValues[key]), nil
				case "npm":
					if npmValues[key] == nil {
						return "undefined\n", nil
					}
					return *npmValues[key] + "\n", nil
				default:
					return "", errors.New("unexpected command")
				}
			}
			crashEnabled := true
			devRunAsUser = func(_ string, _ *persistedUserIdentity, name string, args ...string) error {
				call := ""
				switch name {
				case "git":
					action, key, value, err := parseDevTestGitMutation(args)
					if err != nil {
						return err
					}
					call = "git " + action + " " + key
					if action == "--unset-all" {
						delete(gitValues, key)
					} else if action == "--add" {
						gitValues[key] = append(gitValues[key], value)
					} else {
						return errors.New("unexpected git operation")
					}
				case "npm":
					action := args[1]
					key := args[2]
					call = "npm " + action + " " + key
					if action == "set" {
						value := args[3]
						npmValues[key] = &value
					} else if action == "delete" {
						delete(npmValues, key)
					} else {
						return errors.New("unexpected npm operation")
					}
				default:
					return errors.New("unexpected command")
				}
				if crashEnabled && call == tc.crashAt {
					panic("simulated process crash")
				}
				return nil
			}

			var crashed any
			func() {
				defer func() { crashed = recover() }()
				_ = a.restoreDevProxySnapshotDurable(snapshot, proxy)
			}()
			if crashed == nil {
				t.Fatalf("rollback did not reach crash point %q", tc.crashAt)
			}
			persisted, err := a.loadDevBackup()
			if err != nil {
				t.Fatal(err)
			}
			if persisted.ApplyRollback == nil {
				t.Fatal("complete apply rollback snapshot was not durable before mutation")
			}
			if !slices.Equal(persisted.GitHTTPProxy, ownership.GitHTTPProxy) || !slices.Equal(persisted.GitHTTPSProxy, ownership.GitHTTPSProxy) {
				t.Fatalf("apply rollback overwrote outer ownership snapshot: %+v", persisted)
			}

			crashEnabled = false
			if err := a.resumePendingDevApplyRollback(); err != nil {
				t.Fatalf("resuming apply rollback failed: %v", err)
			}
			if !slices.Equal(gitValues["http.proxy"], snapshot.GitHTTPProxy) || !slices.Equal(gitValues["https.proxy"], snapshot.GitHTTPSProxy) {
				t.Fatalf("git snapshot was not restored: %+v", gitValues)
			}
			if npmValues["proxy"] == nil || *npmValues["proxy"] != npmBefore || npmValues["https-proxy"] != nil {
				t.Fatalf("npm snapshot was not restored: %+v", npmValues)
			}
			persisted, err = a.loadDevBackup()
			if err != nil {
				t.Fatal(err)
			}
			if persisted.ApplyRollback != nil {
				t.Fatalf("completed apply rollback journal was not cleared: %+v", persisted.ApplyRollback)
			}
			if !slices.Equal(persisted.GitHTTPProxy, ownership.GitHTTPProxy) {
				t.Fatalf("outer ownership backup was not retained: %v", persisted.GitHTTPProxy)
			}
		})
	}
}

func TestDevApplyRollbackRejectsAdministratorDivergence(t *testing.T) {
	stubDevCommands(t)
	a := testApp(t)
	proxy := a.cfg.HTTPAddr(SceneDev)
	identity := devTestIdentity(t, "root")
	backup := &devProxyBackup{
		Version:             devProxyBackupVersion,
		User:                "root",
		Identity:            identity,
		ToolsRecorded:       true,
		GitManaged:          true,
		GitConfigLocation:   devGitConfigHome,
		ManagedHTTPProxy:    proxy,
		ManagedHTTPSProxy:   proxy,
		ManagedHTTPProxies:  []string{proxy},
		ManagedHTTPSProxies: []string{proxy},
	}
	if err := a.writeDevBackup(backup); err != nil {
		t.Fatal(err)
	}
	snapshot := &devProxyBackup{
		Version:           devProxyBackupVersion,
		User:              "root",
		Identity:          identity,
		ToolsRecorded:     true,
		GitManaged:        true,
		GitConfigLocation: devGitConfigHome,
		GitHTTPProxy:      []string{"before-a", "before-b"},
	}
	values := map[string][]string{"http.proxy": {proxy}}
	devCommandExists = func(name string) bool { return name == "git" }
	devOutputAsUser = func(_ string, _ *persistedUserIdentity, _ string, args ...string) (string, error) {
		current := values[args[len(args)-1]]
		if len(current) == 0 {
			return "", fakeExitError(1)
		}
		return gitNullOutput(current), nil
	}
	crash := true
	devRunAsUser = func(_ string, _ *persistedUserIdentity, _ string, args ...string) error {
		action, key, value, err := parseDevTestGitMutation(args)
		if err != nil {
			return err
		}
		if action == "--unset-all" {
			delete(values, key)
			return nil
		}
		values[key] = append(values[key], value)
		if crash {
			panic("simulated process crash")
		}
		return nil
	}
	func() {
		defer func() { _ = recover() }()
		_ = a.restoreDevProxySnapshotDurable(snapshot, proxy)
	}()
	values["http.proxy"] = []string{"before-a", "administrator-new"}
	crash = false
	if err := a.resumePendingDevApplyRollback(); err == nil || !strings.Contains(err.Error(), "并发修改") {
		t.Fatalf("administrator divergence was not rejected: %v", err)
	}
	if !slices.Equal(values["http.proxy"], []string{"before-a", "administrator-new"}) {
		t.Fatalf("administrator value was overwritten: %v", values["http.proxy"])
	}
	persisted, err := a.loadDevBackup()
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ApplyRollback == nil {
		t.Fatal("divergent apply rollback evidence was deleted")
	}
}

func TestRestoreDevResumesCrashedApplyRollbackBeforeFinalCleanup(t *testing.T) {
	stubDevCommands(t)
	a := testApp(t)
	proxy := a.cfg.HTTPAddr(SceneDev)
	identity := devTestIdentity(t, "root")
	backup := &devProxyBackup{
		Version:             devProxyBackupVersion,
		User:                "root",
		Identity:            identity,
		ToolsRecorded:       true,
		GitManaged:          true,
		GitConfigLocation:   devGitConfigHome,
		GitHTTPProxy:        []string{"original"},
		ManagedHTTPProxy:    proxy,
		ManagedHTTPSProxy:   proxy,
		ManagedHTTPProxies:  []string{proxy},
		ManagedHTTPSProxies: []string{proxy},
	}
	if err := a.writeDevBackup(backup); err != nil {
		t.Fatal(err)
	}
	snapshot := &devProxyBackup{
		Version:           devProxyBackupVersion,
		User:              "root",
		Identity:          identity,
		ToolsRecorded:     true,
		GitManaged:        true,
		GitConfigLocation: devGitConfigHome,
		GitHTTPProxy:      []string{"original"},
	}
	values := map[string][]string{"http.proxy": {proxy}}
	devCommandExists = func(name string) bool { return name == "git" }
	devOutputAsUser = func(_ string, _ *persistedUserIdentity, _ string, args ...string) (string, error) {
		current := values[args[len(args)-1]]
		if len(current) == 0 {
			return "", fakeExitError(1)
		}
		return gitNullOutput(current), nil
	}
	crash := true
	devRunAsUser = func(_ string, _ *persistedUserIdentity, _ string, args ...string) error {
		action, key, value, err := parseDevTestGitMutation(args)
		if err != nil {
			return err
		}
		if action == "--unset-all" {
			delete(values, key)
			if crash {
				panic("simulated process crash after unset")
			}
			return nil
		}
		values[key] = append(values[key], value)
		return nil
	}
	func() {
		defer func() { _ = recover() }()
		_ = a.restoreDevProxySnapshotDurable(snapshot, proxy)
	}()
	if len(values["http.proxy"]) != 0 {
		t.Fatalf("crash did not leave the historical loss window: %v", values["http.proxy"])
	}
	persisted, err := a.loadDevBackup()
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ApplyRollback == nil {
		t.Fatal("apply rollback was not durable after crash")
	}

	crash = false
	if err := a.restoreDev(); err != nil {
		t.Fatalf("final restore did not resume apply rollback: %v", err)
	}
	if !slices.Equal(values["http.proxy"], []string{"original"}) {
		t.Fatalf("original Git value was lost after final cleanup: %v", values["http.proxy"])
	}
	if _, err := os.Lstat(a.cfg.DevBackupPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed final restore retained ownership backup: %v", err)
	}
}

func TestWriteDevBackupRejectsOversizedValidState(t *testing.T) {
	stubDevCommands(t)
	a := testApp(t)
	value := strings.Repeat("x", maxDevProxyValueBytes)
	backup := &devProxyBackup{
		Version:           devProxyBackupVersion,
		User:              "root",
		Identity:          devTestIdentity(t, "root"),
		ToolsRecorded:     true,
		GitManaged:        true,
		GitConfigLocation: devGitConfigHome,
		GitHTTPProxy:      make([]string, 17),
	}
	for i := range backup.GitHTTPProxy {
		backup.GitHTTPProxy[i] = value
	}
	if err := a.writeDevBackup(backup); err == nil || !strings.Contains(err.Error(), "超过大小限制") {
		t.Fatalf("oversized backup was not rejected: %v", err)
	}
	if _, err := os.Lstat(a.cfg.DevBackupPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized backup created an unreadable journal: %v", err)
	}
}

func TestValidateDevApplyRollbackRejectsInvalidState(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*devProxyBackup)
	}{
		{
			name: "invalid-nested-phase",
			mutate: func(backup *devProxyBackup) {
				backup.ApplyRollback.GitHTTPRestore = &devGitRestorePlan{Phase: "invalid", Before: []string{"managed"}, Desired: []string{"original"}}
			},
		},
		{
			name: "next-outside-current-plan",
			mutate: func(backup *devProxyBackup) {
				backup.ApplyRollback.GitHTTPRestore = &devGitRestorePlan{Phase: devRestorePhasePrepared, Before: []string{"managed"}, Desired: []string{"original"}, Next: 2}
			},
		},
		{
			name: "missing-tool-record",
			mutate: func(backup *devProxyBackup) {
				backup.ToolsRecorded = false
			},
		},
		{
			name: "unowned-managed-value",
			mutate: func(backup *devProxyBackup) {
				backup.ApplyRollback.ManagedProxy = "http://127.0.0.1:65535"
			},
		},
		{
			name: "tool-outside-ownership",
			mutate: func(backup *devProxyBackup) {
				backup.GitManaged = false
			},
		},
		{
			name: "unmanaged-tool-state",
			mutate: func(backup *devProxyBackup) {
				backup.ApplyRollback.GitManaged = false
				backup.ApplyRollback.NPMManaged = true
				backup.ApplyRollback.GitHTTPProxy = []string{"unexpected"}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stubDevCommands(t)
			a := testApp(t)
			proxy := a.cfg.HTTPAddr(SceneDev)
			backup := &devProxyBackup{
				Version:             devProxyBackupVersion,
				User:                "root",
				Identity:            devTestIdentity(t, "root"),
				ToolsRecorded:       true,
				GitManaged:          true,
				GitConfigLocation:   devGitConfigHome,
				NPMManaged:          true,
				ManagedHTTPProxy:    proxy,
				ManagedHTTPSProxy:   proxy,
				ManagedHTTPProxies:  []string{proxy},
				ManagedHTTPSProxies: []string{proxy},
				ApplyRollback: &devApplyRollback{
					GitManaged:   true,
					ManagedProxy: proxy,
				},
			}
			tc.mutate(backup)
			if err := a.writeDevBackup(backup); err == nil {
				t.Fatal("invalid apply rollback state was accepted")
			}
			if _, err := os.Lstat(a.cfg.DevBackupPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid state created a backup: %v", err)
			}
		})
	}
}

func TestDevApplyRollbackRejectsSnapshotIdentityMismatch(t *testing.T) {
	stubDevCommands(t)
	a := testApp(t)
	proxy := a.cfg.HTTPAddr(SceneDev)
	identity := devTestIdentity(t, "root")
	backup := &devProxyBackup{
		Version:             devProxyBackupVersion,
		User:                "root",
		Identity:            identity,
		ToolsRecorded:       true,
		GitManaged:          true,
		GitConfigLocation:   devGitConfigHome,
		ManagedHTTPProxy:    proxy,
		ManagedHTTPSProxy:   proxy,
		ManagedHTTPProxies:  []string{proxy},
		ManagedHTTPSProxies: []string{proxy},
	}
	if err := a.writeDevBackup(backup); err != nil {
		t.Fatal(err)
	}
	drifted := *identity
	drifted.UID++
	snapshot := &devProxyBackup{
		Version:           devProxyBackupVersion,
		User:              "root",
		Identity:          &drifted,
		ToolsRecorded:     true,
		GitManaged:        true,
		GitConfigLocation: devGitConfigHome,
	}
	devCommandExists = func(string) bool {
		t.Fatal("identity mismatch must fail before tool access")
		return false
	}
	devOutputAsUser = func(string, *persistedUserIdentity, string, ...string) (string, error) {
		t.Fatal("identity mismatch must fail before reading tool config")
		return "", nil
	}
	devRunAsUser = func(string, *persistedUserIdentity, string, ...string) error {
		t.Fatal("identity mismatch must fail before mutation")
		return nil
	}
	if err := a.restoreDevProxySnapshotDurable(snapshot, proxy); err == nil || !strings.Contains(err.Error(), "身份不一致") {
		t.Fatalf("snapshot identity mismatch was not rejected: %v", err)
	}
	persisted, err := a.loadDevBackup()
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ApplyRollback != nil {
		t.Fatalf("identity mismatch created rollback ownership: %+v", persisted.ApplyRollback)
	}
}

func TestRestoreDevReportsBackupDeletionFailure(t *testing.T) {
	stubDevCommands(t)
	a := testApp(t)
	backup := devProxyBackup{Version: devProxyBackupVersion, User: "root", Identity: devTestIdentity(t, "root"), ToolsRecorded: true}
	raw, err := json.Marshal(backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(a.cfg.DevBackupPath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	devCommandExists = func(string) bool { return false }
	devRemoveBackup = func(string) error { return errors.New("remove denied") }
	if err := a.restoreDev(); err == nil || !strings.Contains(err.Error(), "删除开发代理备份失败") {
		t.Fatalf("backup deletion failure was not returned: %v", err)
	}
	if _, err := os.Stat(a.cfg.DevBackupPath()); err != nil {
		t.Fatalf("failed deletion must leave backup retryable: %v", err)
	}
}

func TestRestoreDevRetriesDirectoryBarrierAfterBackupWasRemoved(t *testing.T) {
	stubDevCommands(t)
	a := testApp(t)
	backup := devProxyBackup{Version: devProxyBackupVersion, User: "root", Identity: devTestIdentity(t, "root"), ToolsRecorded: true}
	raw, err := json.Marshal(backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(a.cfg.DevBackupPath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	devCommandExists = func(string) bool { return false }
	fsyncCalls := 0
	devBackupDirFsync = func(int) error {
		fsyncCalls++
		if fsyncCalls == 1 {
			return errors.New("injected directory fsync failure")
		}
		return nil
	}

	if err := a.restoreDev(); err == nil || !strings.Contains(err.Error(), "injected directory fsync failure") {
		t.Fatalf("directory barrier failure was not returned: %v", err)
	}
	if _, err := os.Lstat(a.cfg.DevBackupPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup should already be unlinked: %v", err)
	}
	if err := a.restoreDev(); err != nil {
		t.Fatalf("retry did not complete the missing-file directory barrier: %v", err)
	}
	if fsyncCalls != 2 {
		t.Fatalf("directory barrier calls = %d, want 2", fsyncCalls)
	}
}

func TestRestoreDevReplaysGitCrashFromDurablePlan(t *testing.T) {
	cases := []struct {
		name           string
		crashAfterAdds int
	}{
		{name: "after-unset-before-add", crashAfterAdds: 0},
		{name: "after-1-add", crashAfterAdds: 1},
		{name: "after-2-adds", crashAfterAdds: 2},
		{name: "after-all-adds-before-done", crashAfterAdds: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubDevCommands(t)
			a := testApp(t)
			proxy := a.cfg.HTTPAddr(SceneDev)
			backup := &devProxyBackup{
				Version:             devProxyBackupVersion,
				User:                "root",
				Identity:            devTestIdentity(t, "root"),
				ToolsRecorded:       true,
				GitManaged:          true,
				GitConfigLocation:   devGitConfigHome,
				GitHTTPProxy:        []string{"http-original-a", "http-original-b"},
				GitHTTPSProxy:       []string{"https-original"},
				ManagedHTTPProxies:  []string{proxy},
				ManagedHTTPSProxies: []string{proxy},
			}
			if err := a.writeDevBackup(backup); err != nil {
				t.Fatal(err)
			}
			values := map[string][]string{
				"http.proxy":  {proxy, "http-concurrent"},
				"https.proxy": {proxy, "https-concurrent"},
			}
			devCommandExists = func(name string) bool { return name == "git" }
			devOutputAsUser = func(_ string, _ *persistedUserIdentity, name string, args ...string) (string, error) {
				if name != "git" {
					return "", errors.New("unexpected command")
				}
				current := values[args[len(args)-1]]
				if len(current) == 0 {
					return "", fakeExitError(1)
				}
				return gitNullOutput(current), nil
			}
			adds := 0
			panicAfter := tc.crashAfterAdds
			crashEnabled := true
			devRunAsUser = func(_ string, _ *persistedUserIdentity, name string, args ...string) error {
				if name != "git" {
					return errors.New("unexpected command")
				}
				action, key, value, err := parseDevTestGitMutation(args)
				if err != nil {
					return err
				}
				switch action {
				case "--unset-all":
					delete(values, key)
					if crashEnabled && panicAfter == 0 {
						panic("simulated process crash")
					}
				case "--add":
					values[key] = append(values[key], value)
					adds++
					if crashEnabled && panicAfter > 0 && adds == panicAfter {
						panic("simulated process crash")
					}
				default:
					return errors.New("unexpected git operation")
				}
				return nil
			}

			var crashed any
			func() {
				defer func() { crashed = recover() }()
				_ = a.restoreDev()
			}()
			if crashed == nil {
				t.Fatal("restore did not reach the injected process crash")
			}
			persisted, err := a.loadDevBackup()
			if err != nil {
				t.Fatal(err)
			}
			if persisted.GitHTTPRestore == nil || persisted.GitHTTPRestore.Phase != devRestorePhasePrepared {
				t.Fatalf("prepared Git restore plan was not durable: %+v", persisted.GitHTTPRestore)
			}
			if persisted.GitHTTPRestore.Next != tc.crashAfterAdds {
				t.Fatalf("durable next step = %d, want %d", persisted.GitHTTPRestore.Next, tc.crashAfterAdds)
			}
			wantHTTP := []string{"http-original-a", "http-original-b", "http-concurrent"}
			if !slices.Equal(persisted.GitHTTPRestore.Desired, wantHTTP) {
				t.Fatalf("durable desired values = %v, want %v", persisted.GitHTTPRestore.Desired, wantHTTP)
			}

			crashEnabled = false
			if err := a.restoreDev(); err != nil {
				t.Fatalf("crash replay failed: %v", err)
			}
			if !slices.Equal(values["http.proxy"], wantHTTP) {
				t.Fatalf("HTTP values after replay = %v, want %v", values["http.proxy"], wantHTTP)
			}
			wantHTTPS := []string{"https-original", "https-concurrent"}
			if !slices.Equal(values["https.proxy"], wantHTTPS) {
				t.Fatalf("HTTPS values after replay = %v, want %v", values["https.proxy"], wantHTTPS)
			}
			if _, err := os.Lstat(a.cfg.DevBackupPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("completed restore retained backup: %v", err)
			}
		})
	}
}

func TestRestoreDevGitPlanRejectsConcurrentDivergence(t *testing.T) {
	stubDevCommands(t)
	a := testApp(t)
	proxy := a.cfg.HTTPAddr(SceneDev)
	backup := &devProxyBackup{
		Version:             devProxyBackupVersion,
		User:                "root",
		Identity:            devTestIdentity(t, "root"),
		ToolsRecorded:       true,
		GitManaged:          true,
		GitConfigLocation:   devGitConfigHome,
		GitHTTPProxy:        []string{"original"},
		ManagedHTTPProxies:  []string{proxy},
		ManagedHTTPSProxies: []string{proxy},
	}
	if err := a.writeDevBackup(backup); err != nil {
		t.Fatal(err)
	}
	values := map[string][]string{"http.proxy": {proxy}}
	devCommandExists = func(name string) bool { return name == "git" }
	devOutputAsUser = func(_ string, _ *persistedUserIdentity, _ string, args ...string) (string, error) {
		current := values[args[len(args)-1]]
		if len(current) == 0 {
			return "", fakeExitError(1)
		}
		return gitNullOutput(current), nil
	}
	crash := true
	devRunAsUser = func(_ string, _ *persistedUserIdentity, _ string, args ...string) error {
		action, key, value, err := parseDevTestGitMutation(args)
		if err != nil {
			return err
		}
		if action == "--unset-all" {
			delete(values, key)
			return nil
		}
		values[key] = append(values[key], value)
		if crash {
			panic("simulated process crash")
		}
		return nil
	}
	func() {
		defer func() { _ = recover() }()
		_ = a.restoreDev()
	}()
	values["http.proxy"] = []string{"operator-new"}
	crash = false
	if err := a.restoreDev(); err == nil || !strings.Contains(err.Error(), "并发修改") {
		t.Fatalf("concurrent divergence was not rejected: %v", err)
	}
	if !slices.Equal(values["http.proxy"], []string{"operator-new"}) {
		t.Fatalf("operator value was overwritten: %v", values["http.proxy"])
	}
	if _, err := os.Stat(a.cfg.DevBackupPath()); err != nil {
		t.Fatalf("backup was not retained after divergence: %v", err)
	}
}

func TestRestoreDevGitPlanRejectsPrefixesOutsideCurrentStep(t *testing.T) {
	cases := []struct {
		name    string
		plan    *devGitRestorePlan
		current []string
	}{
		{
			name: "prefix-of-before-after-prepare",
			plan: &devGitRestorePlan{
				Phase:   devRestorePhasePrepared,
				Before:  []string{"managed", "operator-extra"},
				Desired: []string{"original", "operator-extra"},
				Next:    0,
			},
			current: []string{"managed"},
		},
		{
			name: "stale-desired-prefix-after-progress",
			plan: &devGitRestorePlan{
				Phase:   devRestorePhasePrepared,
				Before:  []string{"managed"},
				Desired: []string{"original-a", "original-b", "original-c"},
				Next:    2,
			},
			current: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubDevCommands(t)
			a := testApp(t)
			backup := &devProxyBackup{
				Version:           devProxyBackupVersion,
				User:              "root",
				Identity:          devTestIdentity(t, "root"),
				ToolsRecorded:     true,
				GitManaged:        true,
				GitConfigLocation: devGitConfigHome,
				GitHTTPRestore:    tc.plan,
			}
			if err := a.writeDevBackup(backup); err != nil {
				t.Fatal(err)
			}
			devCommandExists = func(name string) bool { return name == "git" }
			devOutputAsUser = func(_ string, _ *persistedUserIdentity, name string, _ ...string) (string, error) {
				if name != "git" {
					return "", errors.New("unexpected command")
				}
				if len(tc.current) == 0 {
					return "", fakeExitError(1)
				}
				return gitNullOutput(tc.current), nil
			}
			devRunAsUser = func(string, *persistedUserIdentity, string, ...string) error {
				t.Fatal("divergent state must be rejected before mutation")
				return nil
			}

			err := a.restoreDevGitProxy(backup, "http.proxy", nil, []string{"managed"}, &backup.GitHTTPRestore)
			if err == nil || !strings.Contains(err.Error(), "并发修改") {
				t.Fatalf("out-of-step prefix was not rejected: %v", err)
			}
			persisted, loadErr := a.loadDevBackup()
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if persisted.GitHTTPRestore.Next != tc.plan.Next || persisted.GitHTTPRestore.Phase != devRestorePhasePrepared {
				t.Fatalf("rejected plan advanced unexpectedly: %+v", persisted.GitHTTPRestore)
			}
		})
	}
}

func TestRestoreDevGitPlanDoesNotAdvanceAfterResultMismatch(t *testing.T) {
	stubDevCommands(t)
	a := testApp(t)
	plan := &devGitRestorePlan{
		Phase:   devRestorePhasePrepared,
		Before:  []string{"managed"},
		Desired: []string{"original"},
		Next:    0,
	}
	backup := &devProxyBackup{
		Version:           devProxyBackupVersion,
		User:              "root",
		Identity:          devTestIdentity(t, "root"),
		ToolsRecorded:     true,
		GitManaged:        true,
		GitConfigLocation: devGitConfigHome,
		GitHTTPRestore:    plan,
	}
	if err := a.writeDevBackup(backup); err != nil {
		t.Fatal(err)
	}
	devCommandExists = func(name string) bool { return name == "git" }
	devOutputAsUser = func(string, *persistedUserIdentity, string, ...string) (string, error) {
		return "managed\x00", nil
	}
	runs := 0
	devRunAsUser = func(string, *persistedUserIdentity, string, ...string) error {
		runs++
		return nil
	}

	err := a.restoreDevGitProxy(backup, "http.proxy", nil, []string{"managed"}, &backup.GitHTTPRestore)
	if err == nil || !strings.Contains(err.Error(), "结果与持久化计划不一致") {
		t.Fatalf("result mismatch was not rejected: %v", err)
	}
	if runs != 1 {
		t.Fatalf("git mutation calls = %d, want 1", runs)
	}
	persisted, loadErr := a.loadDevBackup()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if persisted.GitHTTPRestore.Next != 0 || persisted.GitHTTPRestore.Phase != devRestorePhasePrepared {
		t.Fatalf("mismatched result advanced plan: %+v", persisted.GitHTTPRestore)
	}
}

func TestRestoreDevReplaysNPMCrashAfterMutation(t *testing.T) {
	stubDevCommands(t)
	a := testApp(t)
	proxy := a.cfg.HTTPAddr(SceneDev)
	original := "http://npm-original.invalid:8080"
	backup := &devProxyBackup{
		Version:             devProxyBackupVersion,
		User:                "root",
		Identity:            devTestIdentity(t, "root"),
		ToolsRecorded:       true,
		NPMManaged:          true,
		NPMProxy:            &original,
		ManagedHTTPProxies:  []string{proxy},
		ManagedHTTPSProxies: []string{proxy},
	}
	if err := a.writeDevBackup(backup); err != nil {
		t.Fatal(err)
	}
	npmProxy := proxy + "/"
	values := map[string]*string{"proxy": &npmProxy, "https-proxy": &npmProxy}
	devCommandExists = func(name string) bool { return name == "npm" }
	devOutputAsUser = func(_ string, _ *persistedUserIdentity, _ string, args ...string) (string, error) {
		value := values[args[len(args)-1]]
		if value == nil {
			return "undefined\n", nil
		}
		return *value + "\n", nil
	}
	crash := true
	devRunAsUser = func(_ string, _ *persistedUserIdentity, _ string, args ...string) error {
		key := args[2]
		if args[1] == "set" {
			value := args[3]
			values[key] = &value
		} else {
			values[key] = nil
		}
		if crash {
			panic("simulated process crash")
		}
		return nil
	}
	func() {
		defer func() { _ = recover() }()
		_ = a.restoreDev()
	}()
	persisted, err := a.loadDevBackup()
	if err != nil || persisted.NPMProxyRestore == nil || persisted.NPMProxyRestore.Phase != devRestorePhasePrepared {
		t.Fatalf("prepared npm restore plan was not durable: plan=%+v err=%v", persisted.NPMProxyRestore, err)
	}
	crash = false
	if err := a.restoreDev(); err != nil {
		t.Fatalf("npm crash replay failed: %v", err)
	}
	if values["proxy"] == nil || *values["proxy"] != original || values["https-proxy"] != nil {
		t.Fatalf("npm values after replay: %+v", values)
	}
}

func TestContainsManagedNPMProxyAllowsOnlySingleCanonicalTrailingSlash(t *testing.T) {
	managed := []string{"http://127.0.0.1:7891"}
	for _, current := range []string{"http://127.0.0.1:7891", "http://127.0.0.1:7891/"} {
		if !containsManagedNPMProxy(managed, current) {
			t.Fatalf("npm canonical proxy was not recognized: %q", current)
		}
	}
	for _, current := range []string{"http://127.0.0.1:7891//", "http://127.0.0.1:7892/", "http://operator.invalid:7891/"} {
		if containsManagedNPMProxy(managed, current) {
			t.Fatalf("unmanaged npm proxy was accepted: %q", current)
		}
	}
}

func TestRestoreDevNPMPlanRereadsAfterPrepareBeforeMutation(t *testing.T) {
	stubDevCommands(t)
	a := testApp(t)
	proxy := a.cfg.HTTPAddr(SceneDev)
	original := "npm-original"
	backup := &devProxyBackup{
		Version:             devProxyBackupVersion,
		User:                "root",
		Identity:            devTestIdentity(t, "root"),
		ToolsRecorded:       true,
		NPMManaged:          true,
		NPMProxy:            &original,
		ManagedHTTPProxy:    proxy,
		ManagedHTTPSProxy:   proxy,
		ManagedHTTPProxies:  []string{proxy},
		ManagedHTTPSProxies: []string{proxy},
	}
	if err := a.writeDevBackup(backup); err != nil {
		t.Fatal(err)
	}
	reads := 0
	devCommandExists = func(name string) bool { return name == "npm" }
	devOutputAsUser = func(_ string, _ *persistedUserIdentity, _ string, _ ...string) (string, error) {
		reads++
		if reads == 1 {
			return proxy + "\n", nil
		}
		return "administrator-new\n", nil
	}
	devRunAsUser = func(string, *persistedUserIdentity, string, ...string) error {
		t.Fatal("npm value changed after prepare must be rejected before mutation")
		return nil
	}

	err := a.restoreDevNPMProxy(backup, "proxy", backup.NPMProxy, []string{proxy}, &backup.NPMProxyRestore)
	if err == nil || !strings.Contains(err.Error(), "并发修改") {
		t.Fatalf("post-prepare npm divergence was not rejected: %v", err)
	}
	if reads != 2 {
		t.Fatalf("npm reads = %d, want initial + post-prepare", reads)
	}
	persisted, loadErr := a.loadDevBackup()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if persisted.NPMProxyRestore == nil || persisted.NPMProxyRestore.Phase != devRestorePhasePrepared {
		t.Fatalf("prepared npm plan was not retained: %+v", persisted.NPMProxyRestore)
	}
}

func TestRestoreDevContinuesAfterOneKeyFailsAndRetainsBackup(t *testing.T) {
	stubDevCommands(t)
	a := testApp(t)
	proxy := a.cfg.HTTPAddr(SceneDev)
	backup := devProxyBackup{
		Version:             devProxyBackupVersion,
		User:                "root",
		Identity:            devTestIdentity(t, "root"),
		ToolsRecorded:       true,
		GitManaged:          true,
		GitConfigLocation:   devGitConfigHome,
		NPMManaged:          true,
		ManagedHTTPProxies:  []string{proxy},
		ManagedHTTPSProxies: []string{proxy},
		ManagedHTTPProxy:    proxy,
		ManagedHTTPSProxy:   proxy,
		GitHTTPProxy:        nil,
		GitHTTPSProxy:       nil,
		NPMProxy:            nil,
		NPMHTTPSProxy:       nil,
	}
	raw, err := json.Marshal(backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(a.cfg.DevBackupPath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	devCommandExists = func(name string) bool { return name == "git" || name == "npm" }
	devOutputAsUser = func(_ string, _ *persistedUserIdentity, name string, _ ...string) (string, error) {
		if name == "git" {
			return proxy + "\x00", nil
		}
		return proxy + "\n", nil
	}
	calls := []string{}
	devRunAsUser = func(_ string, _ *persistedUserIdentity, name string, args ...string) error {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		if name == "git" && strings.Contains(call, "--unset-all -- http.proxy") {
			return errors.New("http restore failed")
		}
		return nil
	}

	err = a.restoreDev()
	if err == nil || !strings.Contains(err.Error(), "http restore failed") {
		t.Fatalf("first restore failure not returned: %v", err)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{"--unset-all -- https.proxy", "npm config delete proxy", "npm config delete https-proxy"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("later key %q was not attempted:\n%s", want, joined)
		}
	}
	if _, err := os.Stat(a.cfg.DevBackupPath()); err != nil {
		t.Fatalf("backup must remain after partial restore: %v", err)
	}
}

func TestDevRuntimeUserTransitionRecoversCrashBeforeStoreCommit(t *testing.T) {
	stubDevCommands(t)
	oldApp := testApp(t)
	oldApp.cfg.DevTargetUser = "root"
	oldApp.cfg.DevHTTPPort = 18091
	newCfg := oldApp.cfg
	newCfg.DevTargetUser = "nobody"
	newCfg.DevHTTPPort = 28091
	newApp := NewApp(newCfg)

	values := map[string]map[string][]string{
		"root": {
			"http.proxy":  {"root-http-original"},
			"https.proxy": {"root-https-original"},
		},
		"nobody": {
			"http.proxy":  {"nobody-http-original"},
			"https.proxy": {"nobody-https-original"},
		},
	}
	devCommandExists = func(name string) bool { return name == "git" }
	devOutputAsUser = func(user string, _ *persistedUserIdentity, name string, args ...string) (string, error) {
		if name != "git" {
			return "", errors.New("unexpected command")
		}
		current := values[user][args[len(args)-1]]
		if len(current) == 0 {
			return "", fakeExitError(1)
		}
		return gitNullOutput(current), nil
	}
	devRunAsUser = func(user string, _ *persistedUserIdentity, name string, args ...string) error {
		if name != "git" || len(args) < 4 {
			return errors.New("unexpected write")
		}
		action, key, value, err := parseDevTestGitMutation(args)
		if err != nil {
			return err
		}
		if action == "--unset-all" {
			delete(values[user], key)
			return nil
		}
		if action == "--add" {
			values[user][key] = append(values[user][key], value)
			return nil
		}
		if action == "--replace-all" {
			values[user][key] = []string{value}
			return nil
		}
		return errors.New("unexpected git operation")
	}

	oldProxy := oldApp.cfg.HTTPAddr(SceneDev)
	newProxy := newApp.cfg.HTTPAddr(SceneDev)
	if err := oldApp.applyDev(); err != nil {
		t.Fatal(err)
	}
	if got := values["root"]["http.proxy"]; len(got) != 1 || got[0] != oldProxy {
		t.Fatalf("old user was not managed: %v", got)
	}
	state := newStore()
	state.SceneEnabled[SceneDev] = true
	state.RuntimeConfig = oldApp.cfg.runtimeConfig()
	if err := oldApp.restoreScene(state, SceneDev); err != nil {
		t.Fatal(err)
	}
	if got := values["root"]["http.proxy"]; len(got) != 1 || got[0] != "root-http-original" {
		t.Fatalf("old user ownership was not restored before migration: %v", got)
	}
	if err := newApp.applyDev(); err != nil {
		t.Fatal(err)
	}
	if got := values["nobody"]["http.proxy"]; len(got) != 1 || got[0] != newProxy {
		t.Fatalf("new user was not managed: %v", got)
	}
	if got := values["root"]["http.proxy"]; len(got) != 1 || got[0] != "root-http-original" {
		t.Fatalf("migration left old user proxied: %v", got)
	}

	// Simulate a process crash after the candidate user was fully applied but
	// before state.json committed. A fresh process resolves the old durable Store
	// and must use the candidate backup to release nobody before rebuilding root.
	restarted, err := newApp.appForStoreRuntime(state)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.cfg.DevTargetUser != "root" || restarted.cfg.DevHTTPPort != oldApp.cfg.DevHTTPPort {
		t.Fatalf("restart did not resolve the old durable runtime: %+v", restarted.cfg)
	}
	if err := restarted.applyDev(); err != nil {
		t.Fatal(err)
	}
	if got := values["nobody"]["http.proxy"]; len(got) != 1 || got[0] != "nobody-http-original" {
		t.Fatalf("candidate user was not restored during rollback: %v", got)
	}
	if got := values["root"]["http.proxy"]; len(got) != 1 || got[0] != oldProxy {
		t.Fatalf("old user was not rebuilt during rollback: %v", got)
	}
	backupData, err := os.ReadFile(oldApp.cfg.DevBackupPath())
	if err != nil {
		t.Fatal(err)
	}
	var backup devProxyBackup
	if err := json.Unmarshal(backupData, &backup); err != nil {
		t.Fatal(err)
	}
	if backup.User != "root" {
		t.Fatalf("rollback backup ownership = %q, want root", backup.User)
	}
}
