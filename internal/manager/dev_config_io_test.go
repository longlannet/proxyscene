package manager

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// Keep real configuration operations confined to a temporary, identity-bound
// home. Existing state-machine tests use command mocks instead.
func realDevConfigFixture(t *testing.T) (string, *persistedUserIdentity) {
	t.Helper()
	stubDevCommands(t)
	home := t.TempDir()
	user := "dev-config-test"
	current := localUserIdentity{Name: user, UID: os.Geteuid(), GID: os.Getegid(), UIDText: strconv.Itoa(os.Geteuid()), GIDText: strconv.Itoa(os.Getegid()), Home: home}
	devLookupUserIdentity = func(name string) (localUserIdentity, error) {
		if name != user {
			return localUserIdentity{}, fmt.Errorf("unexpected identity %q", name)
		}
		return current, nil
	}
	devReadNPMConfig = readDevNPMConfig
	devMutateNPMConfig = mutateDevNPMConfig
	devMutateGitConfig = mutateDevGitConfig
	devResolveGitTopology = resolveDevGitGlobalTopology
	devValidateGitTopology = validateDevGitGlobalTopology
	devGitConfigExists = devGitConfigLocationExists
	devOutputAsUser = func(user string, identity *persistedUserIdentity, name string, args ...string) (string, error) {
		if name != "git" {
			return "", fmt.Errorf("npm must not execute on a user's configuration")
		}
		return outputAsPersistedUser(user, identity, devLookupUserIdentity, name, args...)
	}
	devRunAsUser = func(user string, identity *persistedUserIdentity, name string, args ...string) error {
		if name != "git" {
			return fmt.Errorf("npm must not execute on a user's configuration")
		}
		return runAsPersistedUser(user, identity, devLookupUserIdentity, name, args...)
	}
	return user, &persistedUserIdentity{UID: current.UID, GID: current.GID, Home: home}
}

func devIOWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func requireDevGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for real Git configuration regression tests")
	}
}

func TestDevNPMBoundFileRoundTripIgnoresProjectAndRuntimeSettings(t *testing.T) {
	user, identity := realDevConfigFixture(t)
	a := testApp(t)
	a.cfg.DevTargetUser = user
	devCommandExists = func(name string) bool { return name == "npm" }
	project := t.TempDir()
	victim := filepath.Join(project, "redirected-config")
	devIOWrite(t, victim, "keep this file byte-for-byte\n")
	projectRC := "userconfig=" + victim + "\nproxy=http://project.invalid:8080\n"
	devIOWrite(t, filepath.Join(project, ".npmrc"), projectRC)
	devIOWrite(t, filepath.Join(project, "package.json"), "{}\n")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	t.Setenv("NPM_CONFIG_USERCONFIG", victim)
	t.Setenv("NPM_CONFIG_CACHE", victim)
	original := "http://user:p;ass#word@original.invalid:8080"
	unrelated := "\n; untouched comments\nuserconfig=" + victim + "\ncache=" + victim + "\nlogs-dir=" + victim + "\nregistry=https://registry.npmjs.org/\n"
	path := filepath.Join(identity.Home, ".npmrc")
	devIOWrite(t, path, "proxy=http://superseded.invalid:8080\nproxy=http://user:p\\;ass\\#word@original.invalid:8080\n"+unrelated)
	if err := a.applyDev(); err != nil {
		t.Fatal(err)
	}
	backup, err := a.loadDevBackup()
	if err != nil {
		t.Fatal(err)
	}
	if !optionalStringsEqual(backup.NPMProxy, &original) || backup.NPMHTTPSProxy != nil {
		t.Fatalf("backup did not bind actual user file: %+v", backup)
	}
	for _, key := range []string{"proxy", "https-proxy"} {
		actual, err := readDevNPMConfig(user, identity, key)
		if err != nil || actual == nil || *actual != a.cfg.HTTPAddr(SceneDev) {
			t.Fatalf("apply %s: %v, %v", key, actual, err)
		}
	}
	if err := a.restoreDev(); err != nil {
		t.Fatal(err)
	}
	actual, err := readDevNPMConfig(user, identity, "proxy")
	if err != nil || !optionalStringsEqual(actual, &original) {
		t.Fatalf("restore: %v, %v", actual, err)
	}
	if actual, err := readDevNPMConfig(user, identity, "https-proxy"); err != nil || actual != nil {
		t.Fatalf("restore absent https-proxy: %v, %v", actual, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.HasSuffix(data, []byte(unrelated)) {
		t.Fatalf("unrelated npm bytes changed: %q, %v", data, err)
	}
	for name, want := range map[string]string{victim: "keep this file byte-for-byte\n", filepath.Join(project, ".npmrc"): projectRC} {
		data, err := os.ReadFile(name)
		if err != nil || string(data) != want {
			t.Fatalf("redirect target changed: %s, %q, %v", name, data, err)
		}
	}
	if _, err := os.Stat(a.cfg.DevBackupPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed backup remains: %v", err)
	}
}

func TestDevNPMRejectsUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo", "directory", "wrong-owner"} {
		t.Run(kind, func(t *testing.T) {
			user, identity := realDevConfigFixture(t)
			path := filepath.Join(identity.Home, ".npmrc")
			victim := filepath.Join(t.TempDir(), "config")
			devIOWrite(t, victim, "proxy=http://private.invalid:8080\n")
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(victim, path)
			case "fifo":
				err = syscall.Mkfifo(path, 0o600)
			case "directory":
				err = os.Mkdir(path, 0o700)
			case "wrong-owner":
				if os.Geteuid() != 0 {
					t.Skip("requires root only inside temporary test home")
				}
				devIOWrite(t, path, "proxy=http://private.invalid:8080\n")
				err = os.Chown(path, 65534, 65534)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := readDevNPMConfig(user, identity, "proxy"); err == nil {
				t.Fatal("unsafe read accepted")
			}
			desired := "http://127.0.0.1:7891"
			if err := mutateDevNPMConfig(user, identity, "proxy", nil, &desired); err == nil {
				t.Fatal("unsafe mutation accepted")
			}
			data, err := os.ReadFile(victim)
			if err != nil || string(data) != "proxy=http://private.invalid:8080\n" {
				t.Fatalf("victim changed: %q, %v", data, err)
			}
		})
	}
}

func TestDevNPMConcurrentReplacementWins(t *testing.T) {
	user, identity := realDevConfigFixture(t)
	path := filepath.Join(identity.Home, ".npmrc")
	original, desired := "http://original.invalid:8080", "http://127.0.0.1:7891"
	devIOWrite(t, path, "proxy="+original+"\n")
	concurrent := "proxy=http://concurrent.invalid:8080\nregistry=https://registry.npmjs.org/\n"
	old := userFileCASAfterQuarantine
	t.Cleanup(func() { userFileCASAfterQuarantine = old })
	userFileCASAfterQuarantine = func(actual string) {
		if actual != path {
			t.Fatalf("unexpected mutation %s", actual)
		}
		devIOWrite(t, path, concurrent)
	}
	if err := mutateDevNPMConfig(user, identity, "proxy", &original, &desired); !errors.Is(err, errUserFileChanged) {
		t.Fatalf("CAS race accepted: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != concurrent {
		t.Fatalf("concurrent edit lost: %q, %v", data, err)
	}
}

func TestDevNPMParserContractWithRealNPM(t *testing.T) {
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm is required for the parser contract check")
	}
	user, identity := realDevConfigFixture(t)
	neutral := t.TempDir()
	global := filepath.Join(neutral, "globalrc")
	devIOWrite(t, global, "")
	path := filepath.Join(identity.Home, ".npmrc")
	cases := []struct{ name, input, want string }{
		{"plain", "proxy=http://original.invalid:8080\n", "http://original.invalid:8080"},
		{"quotes", "\"proxy\"=\"http://user:p;ass#word@original.invalid:8080\"\n", "http://user:p;ass#word@original.invalid:8080"},
		{"comments", "proxy = http://user:p\\;ass\\#word@original.invalid:8080 ; comment\n", "http://user:p;ass#word@original.invalid:8080"},
		{"duplicates", "proxy=http://first.invalid:8080\r\nproxy='http://last.invalid:8080'\r\n\r\n", "http://last.invalid:8080"},
		{"raw U+2028", "proxy=http://original.invalid/a\u2028b\n", "null"},
		{"raw U+2029", "proxy=http://original.invalid/a\u2029b\n", "null"},
		{"quoted raw U+2028", "proxy=\"http://original.invalid/a\u2028b\"\n", "null"},
		{"quoted raw U+2029", "proxy=\"http://original.invalid/a\u2029b\"\n", "null"},
		{"section", "proxy=http://original.invalid:8080\n[other]\nproxy=http://nested.invalid:8080\n", "http://original.invalid:8080"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			devIOWrite(t, path, tc.input)
			got, err := readDevNPMConfig(user, identity, "proxy")
			if tc.want == "null" {
				if err == nil {
					t.Fatal("unsupported raw line separator accepted")
				}
			} else if err != nil || got == nil || *got != tc.want {
				t.Fatalf("parse: %v, %v", got, err)
			}
			cmd := exec.Command(npm, "--userconfig="+path, "--globalconfig="+global, "--cache="+filepath.Join(neutral, "cache"), "--logs-dir="+filepath.Join(neutral, "logs"), "--prefix="+neutral, "config", "get", "proxy")
			cmd.Dir = neutral
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + identity.Home, "LANG=C", "LC_ALL=C"}
			out, err := cmd.Output()
			if err != nil || strings.TrimSpace(string(out)) != tc.want {
				t.Fatalf("npm contract: %q, %v; want %q", out, err, tc.want)
			}
		})
	}
}

func TestDevNPMRejectsAmbiguousProxySyntax(t *testing.T) {
	for _, input := range []string{"proxy[]=http://a.invalid\n", "[proxy]\nhost=x\n", "[proxy.host]\nvalue=x\n", "proxy=true\n", "proxy\n", "proxy=${SECRET}\n", "proxy=\"line\\nbreak\"\n", "proxy=x\x00\n"} {
		if _, err := parseNPMProxyConfig([]byte(input)); err == nil {
			t.Errorf("ambiguous input accepted: %q", input)
		}
	}
}

func TestDevGitRealApplyRestorePreservesUnrelatedAndAddedValues(t *testing.T) {
	requireDevGit(t)
	user, identity := realDevConfigFixture(t)
	a := testApp(t)
	a.cfg.DevTargetUser = user
	devCommandExists = func(name string) bool { return name == "git" }
	path := filepath.Join(identity.Home, ".gitconfig")
	devIOWrite(t, path, "[user]\n\tname = Original User\n[http]\n\tproxy = http://one.invalid:8080\n\tproxy = http://two.invalid:8080\n")
	if err := a.applyDev(); err != nil {
		t.Fatal(err)
	}
	if err := devRunAsUser(user, identity, "git", "config", "--file", path, "--add", "http.proxy", "http://concurrent.invalid:8080"); err != nil {
		t.Fatal(err)
	}
	if err := a.restoreDev(); err != nil {
		t.Fatal(err)
	}
	got, err := getGitConfigAllForIdentity(user, identity, devGitConfigHome, "http.proxy")
	want := []string{"http://one.invalid:8080", "http://two.invalid:8080", "http://concurrent.invalid:8080"}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("restored Git values: %v, %v", got, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(data, []byte("name = Original User")) {
		t.Fatalf("unrelated value lost: %q, %v", data, err)
	}
	if _, err := os.Stat(path + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock remains: %v", err)
	}
}

func TestDevGitRestoreRejectsEditAfterLastRead(t *testing.T) {
	requireDevGit(t)
	user, identity := realDevConfigFixture(t)
	a := testApp(t)
	a.cfg.DevTargetUser = user
	devCommandExists = func(name string) bool { return name == "git" }
	path := filepath.Join(identity.Home, ".gitconfig")
	devIOWrite(t, path, "[http]\n\tproxy = http://original.invalid:8080\n")
	if err := a.applyDev(); err != nil {
		t.Fatal(err)
	}
	mutate := devMutateGitConfig
	injected := false
	devMutateGitConfig = func(user string, identity *persistedUserIdentity, location devGitConfigLocation, expected []string, args ...string) error {
		if !injected && args[2] == "http.proxy" {
			injected = true
			if err := devRunAsUser(user, identity, "git", "config", "--file", path, "--add", "http.proxy", "http://concurrent.invalid:8080"); err != nil {
				return err
			}
		}
		return mutate(user, identity, location, expected, args...)
	}
	if err := a.restoreDev(); !errors.Is(err, errUserFileChanged) {
		t.Fatalf("race accepted: %v", err)
	}
	got, err := getGitConfigAllForIdentity(user, identity, devGitConfigHome, "http.proxy")
	if err != nil || !slices.Contains(got, "http://concurrent.invalid:8080") {
		t.Fatalf("concurrent update lost: %v, %v", got, err)
	}
	if _, err := os.Stat(a.cfg.DevBackupPath()); err != nil {
		t.Fatalf("recovery backup lost: %v", err)
	}
}

func TestDevGitLockExcludesOrdinaryWriter(t *testing.T) {
	requireDevGit(t)
	user, identity := realDevConfigFixture(t)
	path := filepath.Join(identity.Home, ".gitconfig")
	devIOWrite(t, path, "[http]\n\tproxy = http://original.invalid:8080\n")
	run := devRunAsUser
	attempted := false
	devRunAsUser = func(user string, identity *persistedUserIdentity, name string, args ...string) error {
		attempted = true
		if err := run(user, identity, "git", "config", "--file", path, "--add", "http.proxy", "http://concurrent.invalid:8080"); err == nil {
			return errors.New("ordinary Git writer acquired the held real lock")
		}
		return run(user, identity, name, args...)
	}
	if err := mutateDevGitConfig(user, identity, devGitConfigHome, []string{"http://original.invalid:8080"}, "--replace-all", "--", "http.proxy", "http://127.0.0.1:7891"); err != nil {
		t.Fatal(err)
	}
	if !attempted {
		t.Fatal("no competing Git writer was attempted")
	}
	if err := run(user, identity, "git", "config", "--file", path, "--add", "http.proxy", "http://after.invalid:8080"); err != nil {
		t.Fatalf("completed transaction retained lock: %v", err)
	}
}

func TestDevGitLockRecoveryKeepsOrdinaryAndActiveLocks(t *testing.T) {
	for _, kind := range []string{"ordinary", "active", "stale"} {
		t.Run(kind, func(t *testing.T) {
			user, identity := realDevConfigFixture(t)
			path := filepath.Join(identity.Home, ".gitconfig")
			marker := "proxyscene Git configuration lock v1\n" + path + "\n"
			if kind == "ordinary" {
				marker = "[user]\n\tname = concurrent writer\n"
			}
			devIOWrite(t, path+".lock", marker)
			if kind == "active" {
				file, err := os.OpenFile(path+".lock", os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			err := withDevGitLock(user, identity, path, func() error { called = true; return nil })
			if kind == "stale" {
				if err != nil || !called {
					t.Fatalf("abandoned own lock did not recover: %v", err)
				}
				entries, err := os.ReadDir(identity.Home)
				if err != nil || len(entries) != 0 {
					t.Fatalf("recovery residue: %v, %v", entries, err)
				}
			} else {
				if err == nil || called {
					t.Fatalf("foreign/active lock stolen: %v", err)
				}
				data, err := os.ReadFile(path + ".lock")
				if err != nil || string(data) != marker {
					t.Fatalf("existing lock changed: %q, %v", data, err)
				}
			}
		})
	}
}

func TestDevApplyRejectsChangeBetweenOwnershipAndSnapshot(t *testing.T) {
	requireDevGit(t)
	for _, tool := range []string{"git", "npm"} {
		for _, existing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/existing=%v", tool, existing), func(t *testing.T) {
				user, identity := realDevConfigFixture(t)
				a := testApp(t)
				a.cfg.DevTargetUser = user
				gitPath, npmPath := filepath.Join(identity.Home, ".gitconfig"), filepath.Join(identity.Home, ".npmrc")
				original, updated := "http://original.invalid:8080", "http://edited.invalid:8080"
				devIOWrite(t, gitPath, "[http]\n\tproxy = "+original+"\n")
				devIOWrite(t, npmPath, "proxy="+original+"\n")
				if existing {
					devCommandExists = func(name string) bool { return (name == "git" || name == "npm") && name != tool }
					if err := a.applyDev(); err != nil {
						t.Fatal(err)
					}
				}
				devCommandExists = func(name string) bool { return name == tool || existing && (name == "git" || name == "npm") }
				injected := false
				if tool == "git" {
					output := devOutputAsUser
					devOutputAsUser = func(user string, identity *persistedUserIdentity, name string, args ...string) (string, error) {
						out, err := output(user, identity, name, args...)
						if err == nil && !injected && slices.Contains(args, "--get-all") && args[len(args)-1] == "http.proxy" {
							injected = true
							devIOWrite(t, gitPath, "[http]\n\tproxy = "+updated+"\n")
						}
						return out, err
					}
				} else {
					read := devReadNPMConfig
					devReadNPMConfig = func(user string, identity *persistedUserIdentity, key string) (*string, error) {
						value, err := read(user, identity, key)
						if err == nil && key == "proxy" && !injected {
							injected = true
							devIOWrite(t, npmPath, "proxy="+updated+"\n")
						}
						return value, err
					}
				}
				if err := a.applyDev(); !errors.Is(err, errUserFileChanged) {
					t.Fatalf("backup/snapshot race accepted: %v", err)
				}
				if !injected {
					t.Fatal("race did not execute")
				}
				backup, err := a.loadDevBackup()
				if existing {
					if err != nil || tool == "git" && backup.GitManaged || tool == "npm" && backup.NPMManaged {
						t.Fatalf("unapplied acquisition retained: %+v, %v", backup, err)
					}
				} else if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("stale first ownership retained: %+v, %v", backup, err)
				}
				if err := a.applyDev(); err != nil {
					t.Fatalf("retry failed to acquire current value: %v", err)
				}
				if err := a.restoreDev(); err != nil {
					t.Fatal(err)
				}
				if tool == "git" {
					values, err := getGitConfigAllForIdentity(user, identity, devGitConfigHome, "http.proxy")
					if err != nil || !slices.Equal(values, []string{updated}) {
						t.Fatalf("retry lost concurrent edit: %v, %v", values, err)
					}
				} else {
					value, err := readDevNPMConfig(user, identity, "proxy")
					if err != nil || !optionalStringsEqual(value, &updated) {
						t.Fatalf("retry lost concurrent edit: %v, %v", value, err)
					}
				}
			})
		}
	}
}

func TestDevGitFirstCommitErrorPersistsAndReplaysRollback(t *testing.T) {
	requireDevGit(t)
	user, identity := realDevConfigFixture(t)
	a := testApp(t)
	a.cfg.DevTargetUser = user
	devCommandExists = func(name string) bool { return name == "git" }
	path := filepath.Join(identity.Home, ".gitconfig")
	original := "http://original.invalid:8080"
	devIOWrite(t, path, "[http]\n\tproxy = "+original+"\n")
	old := userFileCASAfterQuarantine
	t.Cleanup(func() { userFileCASAfterQuarantine = old })
	injected := false
	userFileCASAfterQuarantine = func(actual string) {
		if injected {
			return
		}
		if actual != path {
			t.Fatalf("unexpected commit %s", actual)
		}
		injected = true
		// Commit continues, but replacing its held lock forces the final cleanup
		// check to report an error after the new config reached its final name.
		if err := os.Remove(path + ".lock"); err != nil {
			t.Fatal(err)
		}
		devIOWrite(t, path+".lock", "ordinary writer still holds this lock\n")
	}
	if err := a.applyDev(); !errors.Is(err, errDevConfigMutationUncertain) {
		t.Fatalf("post-commit failure was not classified: %v", err)
	}
	backup, err := a.loadDevBackup()
	if err != nil || backup.ApplyRollback == nil {
		t.Fatalf("first-write rollback was not journaled: %+v, %v", backup, err)
	}
	values, err := getGitConfigAllForIdentity(user, identity, devGitConfigHome, "http.proxy")
	if err != nil || !slices.Equal(values, []string{a.cfg.HTTPAddr(SceneDev)}) {
		t.Fatalf("fault did not follow actual commit: %v, %v", values, err)
	}
	if err := os.Remove(path + ".lock"); err != nil {
		t.Fatal(err)
	}
	if err := a.resumePendingDevApplyRollback(); err != nil {
		t.Fatalf("durable rollback did not replay: %v", err)
	}
	values, err = getGitConfigAllForIdentity(user, identity, devGitConfigHome, "http.proxy")
	if err != nil || !slices.Equal(values, []string{original}) {
		t.Fatalf("replay lost original values: %v, %v", values, err)
	}
	backup, err = a.loadDevBackup()
	if err != nil || backup.ApplyRollback != nil {
		t.Fatalf("completed rollback not committed: %+v, %v", backup, err)
	}
	if err := a.restoreDev(); err != nil {
		t.Fatal(err)
	}
}

func TestDevNPMFirstAmbiguousCommitRollsBack(t *testing.T) {
	user, identity := realDevConfigFixture(t)
	a := testApp(t)
	a.cfg.DevTargetUser = user
	devCommandExists = func(name string) bool { return name == "npm" }
	path := filepath.Join(identity.Home, ".npmrc")
	original := "http://original.invalid:8080"
	devIOWrite(t, path, "proxy="+original+"\n")
	old := userFileCASAfterQuarantine
	t.Cleanup(func() { userFileCASAfterQuarantine = old })
	injected := false
	userFileCASAfterQuarantine = func(actual string) {
		if injected {
			return
		}
		injected = true
		devIOWrite(t, actual, "proxy=\""+a.cfg.HTTPAddr(SceneDev)+"\"\n")
	}
	if err := a.applyDev(); !errors.Is(err, errDevConfigMutationUncertain) {
		t.Fatalf("ambiguous first commit accepted: %v", err)
	}
	value, err := readDevNPMConfig(user, identity, "proxy")
	if err != nil || !optionalStringsEqual(value, &original) {
		t.Fatalf("first ambiguous write was not rolled back: %v, %v", value, err)
	}
	if err := a.restoreDev(); err != nil {
		t.Fatal(err)
	}
}

func TestDevGitWritesAndReadsSnapshotAsRecordedNumericUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root only for dropping privileges inside temporary directories")
	}
	requireDevGit(t)
	user, identity := realDevConfigFixture(t)
	identity.UID, identity.GID = 65534, 65534
	if err := os.Chmod(filepath.Dir(identity.Home), 0o711); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(identity.Home, identity.UID, identity.GID); err != nil {
		t.Fatal(err)
	}
	devLookupUserIdentity = func(string) (localUserIdentity, error) {
		return localUserIdentity{Name: user, UID: identity.UID, GID: identity.GID, UIDText: "65534", GIDText: "65534", Home: identity.Home}, nil
	}
	path := filepath.Join(identity.Home, ".gitconfig")
	devIOWrite(t, path, "[http]\n\tproxy = http://original.invalid:8080\n")
	if err := os.Chown(path, identity.UID, identity.GID); err != nil {
		t.Fatal(err)
	}
	run := devRunAsUser
	devRunAsUser = func(user string, identity *persistedUserIdentity, name string, args ...string) error {
		if err := run(user, identity, name, args...); err != nil {
			return err
		}
		inner := filepath.Dir(args[2])
		if err := runAsPersistedUser(user, identity, devLookupUserIdentity, "mv", inner, inner+"-replaced"); err == nil {
			return errors.New("target UID replaced the pinned Git snapshot directory")
		}
		return nil
	}
	if err := mutateDevGitConfig(user, identity, devGitConfigHome, []string{"http://original.invalid:8080"}, "--replace-all", "--", "http.proxy", "http://127.0.0.1:7891"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if stat := info.Sys().(*syscall.Stat_t); int(stat.Uid) != identity.UID || int(stat.Gid) != identity.GID {
		t.Fatalf("final ownership changed: %+v", stat)
	}
}

func TestDevApplyFailureBeforeMutationReacquiresOwnershipOnRetry(t *testing.T) {
	requireDevGit(t)
	for _, phase := range []string{"npm-snapshot-syntax", "git-writer-lock"} {
		t.Run(phase, func(t *testing.T) {
			user, identity := realDevConfigFixture(t)
			a := testApp(t)
			a.cfg.DevTargetUser = user
			path := filepath.Join(identity.Home, ".npmrc")
			original, updated := "http://original.invalid:8080", "http://edited.invalid:8080"
			devCommandExists = func(name string) bool { return name == "npm" }
			devIOWrite(t, path, "proxy="+original+"\n")
			if phase == "npm-snapshot-syntax" {
				read := devReadNPMConfig
				injected := false
				devReadNPMConfig = func(user string, identity *persistedUserIdentity, key string) (*string, error) {
					value, err := read(user, identity, key)
					if err == nil && key == "https-proxy" && !injected {
						injected = true
						devIOWrite(t, path, "proxy=${ADMIN_PROXY}\n")
					}
					return value, err
				}
			} else {
				path = filepath.Join(identity.Home, ".gitconfig")
				devCommandExists = func(name string) bool { return name == "git" }
				devIOWrite(t, path, "[http]\n\tproxy = "+original+"\n")
				devIOWrite(t, path+".lock", "ordinary writer\n")
			}
			if err := a.applyDev(); err == nil {
				t.Fatal("pre-write failure was accepted")
			}
			if _, err := a.loadDevBackup(); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unapplied stale ownership remains: %v", err)
			}
			if phase == "git-writer-lock" {
				data, err := os.ReadFile(path + ".lock")
				if err != nil || string(data) != "ordinary writer\n" {
					t.Fatalf("ordinary lock changed: %q, %v", data, err)
				}
				if err := os.Remove(path + ".lock"); err != nil {
					t.Fatal(err)
				}
				devIOWrite(t, path, "[http]\n\tproxy = "+updated+"\n")
			} else {
				devIOWrite(t, path, "proxy="+updated+"\n")
			}
			if err := a.applyDev(); err != nil {
				t.Fatalf("retry failed: %v", err)
			}
			if err := a.restoreDev(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Contains(data, []byte(updated)) || bytes.Contains(data, []byte(original)) {
				t.Fatalf("retry restored stale baseline: %q, %v", data, err)
			}
		})
	}
}

func TestDevRepeatApplyPreservesAdministratorEdits(t *testing.T) {
	requireDevGit(t)
	for _, tool := range []string{"git", "npm"} {
		for _, active := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/active=%v", tool, active), func(t *testing.T) {
				user, identity := realDevConfigFixture(t)
				a := testApp(t)
				a.cfg.DevTargetUser = user
				devCommandExists = func(name string) bool { return name == tool }
				path := filepath.Join(identity.Home, ".npmrc")
				original, admin := "http://original.invalid:8080", "http://admin.invalid:8080"
				initial, edit := "proxy="+original+"\n", "proxy="+admin+"\n"
				if tool == "git" {
					path = filepath.Join(identity.Home, ".gitconfig")
					initial, edit = "[http]\n\tproxy = "+original+"\n", "[http]\n\tproxy = "+admin+"\n"
					if active {
						edit = "[http]\n\tproxy = " + a.cfg.HTTPAddr(SceneDev) + "\n\tproxy = " + admin + "\n"
					}
				}
				devIOWrite(t, path, initial)
				if active {
					if err := a.applyDev(); err != nil {
						t.Fatal(err)
					}
				} else if err := a.backupDevConfig(user); err != nil {
					t.Fatal(err)
				}
				devIOWrite(t, path, edit)
				if err := a.applyDev(); !errors.Is(err, errUserFileChanged) {
					t.Fatalf("administrator edit overwritten on replay: %v", err)
				}
				data, err := os.ReadFile(path)
				if err != nil || string(data) != edit {
					t.Fatalf("edit changed: %q, %v", data, err)
				}
				if err := a.restoreDev(); err != nil {
					t.Fatal(err)
				}
				if err := a.applyDev(); err != nil {
					t.Fatalf("off/on did not reacquire safe baseline: %v", err)
				}
				if err := a.restoreDev(); err != nil {
					t.Fatal(err)
				}
				data, err = os.ReadFile(path)
				if err != nil || !bytes.Contains(data, []byte(admin)) {
					t.Fatalf("explicit off/on lost admin edit: %q, %v", data, err)
				}
			})
		}
	}
}
