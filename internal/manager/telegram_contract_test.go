package manager

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// Fixture expectations come from the actual interpreter libraries and Hermes
// leaf matcher, independently of the Go scanner. See fixture provenance.
func TestTelegramInterpreterContractCorpus(t *testing.T) {
	var corpus struct {
		Dotenv []struct {
			Parser, Input string
			Keys          []string
		}
		NoProxy []struct {
			Input  string
			Bypass bool
		} `json:"no_proxy"`
	}
	raw, err := os.ReadFile("testdata/telegram-interpreter-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	for i, test := range corpus.Dotenv {
		t.Run(fmt.Sprintf("dotenv_%s_%d", test.Parser, i), func(t *testing.T) {
			got := dotenvDeclaredKeys([]byte(test.Input))
			if test.Parser == "node" {
				got = openClawDotEnvDeclaredKeys([]byte(test.Input))
			}
			for _, key := range test.Keys {
				if !slices.Contains(got, key) {
					t.Errorf("upstream declaration %q was missed in %q; got %q", key, test.Input, got)
				}
			}
		})
	}
	for i, test := range corpus.NoProxy {
		t.Run(fmt.Sprintf("no_proxy_%d", i), func(t *testing.T) {
			_, got := firstHermesTelegramNoProxyMatch(test.Input)
			if got != test.Bypass {
				t.Fatalf("upstream bypass=%v, validator=%v for %q", test.Bypass, got, test.Input)
			}
		})
	}
}

func contractTestIdentity(t *testing.T) (string, *persistedUserIdentity) {
	t.Helper()
	user := "contract-fixture"
	identity := &persistedUserIdentity{UID: os.Getuid(), GID: os.Getgid(), Home: t.TempDir()}
	lookup := func(name string) (localUserIdentity, error) {
		if name != user {
			return localUserIdentity{}, errors.New("unexpected fixture user")
		}
		return localUserIdentity{Name: user, UID: identity.UID, GID: identity.GID, UIDText: strconv.Itoa(identity.UID), GIDText: strconv.Itoa(identity.GID), Home: identity.Home}, nil
	}
	oldHermes, oldOpenClaw := telegramLookupUserIdentity, openClawLookupUserIdentity
	telegramLookupUserIdentity, openClawLookupUserIdentity = lookup, lookup
	t.Cleanup(func() { telegramLookupUserIdentity, openClawLookupUserIdentity = oldHermes, oldOpenClaw })
	return user, identity
}

func TestTelegramDotEnvRuntimeRejectsAmbiguousAndWhitespaceSelectors(t *testing.T) {
	user, identity := contractTestIdentity(t)
	path := filepath.Join(identity.Home, ".env")
	whitespace := []rune{'\t', '\v', '\f', ' ', '\x1c', '\x1d', '\x1e', '\x1f', '\u0085', '\u00a0', '\u1680', '\u2000', '\u2001', '\u2002', '\u2003', '\u2004', '\u2005', '\u2006', '\u2007', '\u2008', '\u2009', '\u200a', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000', '\ufeff'}
	for _, space := range whitespace {
		t.Run(fmt.Sprintf("U+%04X", space), func(t *testing.T) {
			for _, raw := range []string{"TELEGRAM_PROXY" + string(space) + "=http://override.invalid\n", "export" + string(space) + "'TELEGRAM_PROXY'=http://override.invalid\n"} {
				if err := rejectHermesDotEnvKeys("fixture.env", []byte(raw)); err == nil {
					t.Fatalf("Hermes declaration accepted: %q", raw)
				}
			}
			raw := []byte("OPENCLAW_STATE_DIR" + string(space) + "=/tmp/other\n")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := rejectOpenClawSelectorDotEnv(user, identity, path); err == nil {
				t.Fatalf("OpenClaw selector accepted: %q", raw)
			}
		})
	}
	for _, raw := range [][]byte{
		[]byte("OPENCLAW_STATE_DIR:\n /tmp/other\n"),
		[]byte("OPENCLAW_STATE_DIR: \n"),
		[]byte("OPENCLAW_PROFILE\n=blue\n"),
		[]byte("TOKEN='multiline\nOPENCLAW_PROFILE=blue\n'\n"),
		[]byte("'OPENCLAW_PROFILE=blue\n"),
		{0xff, 'x', '=', '1'}, {'X', '=', 0},
	} {
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err := rejectOpenClawSelectorDotEnv(user, identity, path); err == nil {
			t.Fatalf("ambiguous OpenClaw selector accepted: %q", raw)
		}
	}
}

func TestHermesMultiplexActivationSourcesRejected(t *testing.T) {
	for _, value := range []string{"1", "true", "yes", "on", "auto", "unknown"} {
		t.Run(value, func(t *testing.T) {
			if err := validateHermesManagerEnvironment(map[string]string{"GATEWAY_MULTIPLEX_PROFILES": value}); err == nil {
				t.Fatal("manager multiplex accepted")
			}
			if _, err := validateHermesEffectiveUnit(hermesRuntimeUnit("Environment=GATEWAY_MULTIPLEX_PROFILES=" + value + "\n")); err == nil {
				t.Fatal("unit multiplex accepted")
			}
		})
	}
	for _, config := range []string{
		"gateway:\n  multiplex_profiles: true\n", "multiplex_profiles: true\n",
		"gateway:\n  multiplex_profiles: unknown\n",
		"gateway:\n  1: ignored\n  multiplex_profiles: true\n",
		"gateway:\n  multiplex_profiles: \"\"\n",
		"gateway:\n  multiplex_profiles: \"   \"\n",
		"gateway:\n  multiplex_profiles: \"\ufefffalse\"\n", "gateway:\n  multiplex_profiles: [false]\n",
		"multiplex_profiles: true\ngateway:\n  multiplex_profiles: false\n",
		"GATEWAY_MULTIPLEX_PROFILES: false\n",
		"secrets:\n  onepassword:\n    enabled: true\n    env:\n      GATEWAY_MULTIPLEX_PROFILES: op://v/i/f\n",
	} {
		if err := rejectHermesConfigOverrides("fixture.yaml", []byte(config)); err == nil {
			t.Fatalf("multiplex source accepted: %q", config)
		}
	}
	if _, err := validateHermesEffectiveUnit(hermesRuntimeUnit("PassEnvironment=GATEWAY_MULTIPLEX_PROFILES\n")); err == nil {
		t.Fatal("inherited multiplex selector accepted")
	}
	for _, value := range []string{"false", "0", ""} {
		if err := rejectHermesDotEnvKeys("fixture.env", []byte("GATEWAY_MULTIPLEX_PROFILES="+value+"\n")); err == nil {
			t.Fatal("dotenv multiplex declaration accepted")
		}
	}
}

func TestHermesMultiplexRequiresSingleProfileEvenWhenFalse(t *testing.T) {
	user, identity := contractTestIdentity(t)
	root := filepath.Join(identity.Home, ".hermes")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	check := func() error { return validateHermesSingleProfile(user, identity, root, root) }
	if err := check(); err != nil {
		t.Fatalf("single profile rejected: %v", err)
	}
	profiles := filepath.Join(root, "profiles")
	if err := os.MkdirAll(filepath.Join(profiles, ".metadata"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profiles, "README"), []byte("metadata"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := check(); err != nil {
		t.Fatalf("metadata rejected: %v", err)
	}
	if err := os.Mkdir(filepath.Join(profiles, "blue"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, config := range []string{"", "multiplex_profiles: false\n", "gateway:\n  multiplex_profiles: false\n  standalone: true\n"} {
		if err := rejectHermesConfigOverrides("fixture.yaml", []byte(config)); err != nil {
			t.Fatalf("false alone rejected before topology check: %v", err)
		}
		if err := check(); err == nil || !strings.Contains(err.Error(), "multiplex") {
			t.Fatalf("automatic multiplex not excluded with %q: %v", config, err)
		}
	}
	if err := validateHermesSingleProfile(user, identity, root, filepath.Join(profiles, "blue")); err == nil {
		t.Fatal("named active profile accepted")
	}
	if err := os.Remove(filepath.Join(profiles, "blue")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(profiles, "blue")); err != nil {
		t.Fatal(err)
	}
	if err := check(); err == nil {
		t.Fatal("profile symlink accepted")
	}
	if err := os.RemoveAll(profiles); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(identity.Home, profiles); err != nil {
		t.Fatal(err)
	}
	if err := check(); err == nil {
		t.Fatal("profiles directory symlink accepted")
	}
}

func TestHermesRuntimeRejectsAlternateNativeRoot(t *testing.T) {
	user, identity := contractTestIdentity(t)
	target := systemdTargetName{UserMode: true, User: user, Service: "hermes-fixture.service"}
	env := map[string]string{"HOME": identity.Home, "HERMES_HOME": filepath.Join(identity.Home, ".hermes", "nested")}
	err := validateHermesRuntimeFiles(target, identity, hermesRuntimeUnit(""), "/opt/hermes", env)
	if err == nil || !strings.Contains(err.Error(), "profile 枚举根") {
		t.Fatalf("ambiguous native root accepted: %v", err)
	}
}

func TestHermesMultiplexRecoveryPreservesOwnershipAndArtifacts(t *testing.T) {
	for _, phase := range []string{telegramPhaseActive, telegramPhasePrepared, telegramPhaseRestoring} {
		for _, operation := range []string{"apply", "restore"} {
			t.Run(phase+"_"+operation, func(t *testing.T) {
				h := newTelegramJournalTestHarness(t)
				target := h.target()
				if _, err := h.app.applyTelegram(newStore(), []systemdTargetName{target}); err != nil {
					t.Fatal(err)
				}
				journal := h.journal(t)
				key := canonicalTelegramTargetName(target)
				journal.Targets[key].Phase = phase
				if phase == telegramPhasePrepared {
					journal.Targets[key].PendingManagedContent = journal.Targets[key].ManagedContent
				}
				if err := h.app.saveTelegramProxyJournal(journal); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(h.systemPath)
				if err != nil {
					t.Fatal(err)
				}
				journalBefore, err := os.ReadFile(h.app.telegramProxyJournalPath())
				if err != nil {
					t.Fatal(err)
				}
				root := filepath.Join(h.dir, ".hermes")
				if err := os.MkdirAll(filepath.Join(root, "profiles", "blue"), 0700); err != nil {
					t.Fatal(err)
				}
				telegramValidateHermesTarget = func(systemdTargetName, *persistedUserIdentity, string) error {
					return validateHermesSingleProfile(h.identity.Name, &persistedUserIdentity{UID: h.identity.UID, GID: h.identity.GID, Home: h.identity.Home}, root, root)
				}
				serviceCalls := 0
				systemctlRun = func(string, ...string) error { serviceCalls++; return nil }
				if operation == "apply" {
					_, err = h.app.applyTelegram(newStore(), []systemdTargetName{target})
				} else {
					err = h.app.restoreTelegram(newStore())
				}
				if err == nil || !strings.Contains(err.Error(), "multiplex") {
					t.Fatalf("recovery accepted multiplex: %v", err)
				}
				after, err := os.ReadFile(h.systemPath)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("rejected recovery changed artifact: %v", err)
				}
				journalAfter, err := os.ReadFile(h.app.telegramProxyJournalPath())
				if err != nil || !bytes.Equal(journalBefore, journalAfter) {
					t.Fatalf("rejected recovery changed ownership: %v", err)
				}
				if serviceCalls != 0 {
					t.Fatalf("rejected recovery touched service manager %d times", serviceCalls)
				}
			})
		}
	}
}

func TestOpenClawJournalRejectsUntrustedOpenedInode(t *testing.T) {
	for _, kind := range []string{"world readable", "group writable", "foreign owner", "symlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "foreign owner" && os.Geteuid() != 0 {
				t.Skip("changing ownership requires root")
			}
			path := filepath.Join(t.TempDir(), "journal.json")
			writeOpenClawJournalCopy(t, path, newOpenClawProxyJournal())
			switch kind {
			case "world readable":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "group writable":
				if err := os.Chmod(path, 0620); err != nil {
					t.Fatal(err)
				}
			case "foreign owner":
				if err := os.Chown(path, 1, 1); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+".target"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".target", path); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := readOpenClawJournalFile(path); err == nil {
				t.Fatal("untrusted journal inode accepted")
			}
		})
	}
}

func TestOpenClawJournalTrustPrecedesGenerationAndConfigWrite(t *testing.T) {
	h := newOpenClawTestHarness(t, `{}`)
	target := h.target("openclaw-trust.service")
	const proxy = "http://127.0.0.1:7892"
	h.applyAndCommit(t, target, proxy)
	original := h.journal(t)
	forged := readOpenClawJournalCopy(t, h.app.openClawJournalPath())
	forged.Generation++
	forged.Users[h.user].ManagedValue = "http://forged.invalid:9999"
	writeOpenClawJournalCopy(t, h.app.openClawJournalPath(), forged)
	if err := os.Chmod(h.app.openClawJournalPath(), 0666); err != nil {
		t.Fatal(err)
	}
	loaded, err := h.app.loadOpenClawProxyJournal()
	if err != nil || loaded.Generation != original.Generation || loaded.Users[h.user].ManagedValue != proxy {
		t.Fatalf("untrusted newer copy beat valid backup: loaded=%+v err=%v", loaded, err)
	}
	if err := os.Chmod(h.app.openClawJournalBackupPath(), 0644); err != nil {
		t.Fatal(err)
	}
	before := h.config(t)
	if _, _, err := h.app.applyOpenClawTelegramProxy(target, "http://127.0.0.1:8892"); err == nil {
		t.Fatal("both untrusted journals accepted")
	}
	if !bytes.Equal(before, h.config(t)) {
		t.Fatal("untrusted ownership changed user config")
	}
}
