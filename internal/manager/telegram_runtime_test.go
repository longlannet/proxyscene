package manager

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func openClawRuntimeUnit(extra string) string {
	return "[Service]\n" +
		"ExecStart=/usr/bin/node /opt/openclaw/dist/index.js gateway --port 18789\n" +
		"Environment=HOME=/home/alice\n" +
		"Environment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\n" + extra
}

func hermesRuntimeUnit(extra string) string {
	return "[Service]\n" +
		"ExecStart=/opt/hermes/venv/bin/python -m hermes_cli.main gateway run\n" + extra
}

func TestValidateOpenClawEffectiveUnitAcceptsDefaultRuntime(t *testing.T) {
	for name, extra := range map[string]string{
		"default user-manager cwd": "",
		"home shorthand":           "WorkingDirectory=~\n",
		"explicit home":            "WorkingDirectory=/home/alice/\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateOpenClawEffectiveUnit(openClawRuntimeUnit(extra), "/home/alice"); err != nil {
				t.Fatalf("default OpenClaw runtime rejected: %v", err)
			}
		})
	}
	if err := validateOpenClawEffectiveUnit(openClawRuntimeUnit("Environment=OPENCLAW_STATE_DIR=\n"), "/home/alice/"); err != nil {
		t.Fatalf("empty selector or equivalent HOME rejected: %v", err)
	}
}

func TestValidateOpenClawCanonicalConfigCandidate(t *testing.T) {
	oldLookup := openClawLookupUserIdentity
	t.Cleanup(func() { openClawLookupUserIdentity = oldLookup })

	for name, files := range map[string][]string{
		"none":                       nil,
		"canonical":                  {".openclaw/openclaw.json"},
		"canonical wins over legacy": {".openclaw/openclaw.json", ".openclaw/clawdbot.json"},
		"legacy openclaw clawdbot":   {".openclaw/clawdbot.json"},
		"legacy clawdbot openclaw":   {".clawdbot/openclaw.json"},
		"legacy clawdbot clawdbot":   {".clawdbot/clawdbot.json"},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			user := "candidate-test"
			identity := &persistedUserIdentity{UID: os.Getuid(), GID: os.Getgid(), Home: home}
			openClawLookupUserIdentity = func(got string) (localUserIdentity, error) {
				if got != user {
					return localUserIdentity{}, errors.New("unexpected user")
				}
				return localUserIdentity{Name: user, UID: identity.UID, GID: identity.GID, UIDText: strconv.Itoa(identity.UID), GIDText: strconv.Itoa(identity.GID), Home: home}, nil
			}
			for _, relative := range files {
				path := filepath.Join(home, filepath.FromSlash(relative))
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := validateOpenClawCanonicalConfigCandidate(user, identity)
			legacyOnly := len(files) == 1 && files[0] != ".openclaw/openclaw.json"
			if legacyOnly && err == nil {
				t.Fatal("active legacy OpenClaw config candidate was accepted")
			}
			if !legacyOnly && err != nil {
				t.Fatalf("canonical candidate state rejected: %v", err)
			}
		})
	}
}

func TestValidateOpenClawEffectiveUnitRequiresDirectGatewayExec(t *testing.T) {
	for name, unit := range map[string]string{
		"nodejs":        strings.Replace(openClawRuntimeUnit(""), "/usr/bin/node ", "/usr/bin/nodejs ", 1),
		"safe prefixes": strings.Replace(openClawRuntimeUnit(""), "/usr/bin/node ", ":-/usr/bin/node ", 1),
	} {
		t.Run("accept/"+name, func(t *testing.T) {
			if err := validateOpenClawEffectiveUnit(unit, "/home/alice"); err != nil {
				t.Fatalf("direct OpenClaw gateway rejected: %v", err)
			}
		})
	}

	baseExec := "ExecStart=/usr/bin/node /opt/openclaw/dist/index.js gateway --port 18789"
	for name, replacement := range map[string]string{
		"unrelated":      "ExecStart=/usr/bin/true",
		"shell wrapper":  "ExecStart=/bin/sh -c '/usr/bin/node /opt/openclaw/dist/index.js gateway'",
		"env wrapper":    "ExecStart=/usr/bin/env /usr/bin/node /opt/openclaw/dist/index.js gateway",
		"chroot wrapper": "ExecStart=/usr/sbin/chroot /srv/root /usr/bin/node /opt/openclaw/dist/index.js gateway",
		"relative node":  "ExecStart=node /opt/openclaw/dist/index.js gateway",
		"wrong script":   "ExecStart=/usr/bin/node /opt/not-openclaw/dist/index.js gateway",
		"not gateway":    "ExecStart=/usr/bin/node /opt/openclaw/dist/index.js doctor",
	} {
		t.Run("reject/"+name, func(t *testing.T) {
			unit := strings.Replace(openClawRuntimeUnit(""), baseExec, replacement, 1)
			if err := validateOpenClawEffectiveUnit(unit, "/home/alice"); err == nil {
				t.Fatal("non-direct OpenClaw gateway was accepted")
			}
		})
	}

	for name, extra := range map[string]string{
		"reset replacement":  "ExecStart=\nExecStart=/usr/bin/true\n",
		"additional command": "ExecStart=/usr/bin/true\n",
	} {
		t.Run("reject/"+name, func(t *testing.T) {
			if err := validateOpenClawEffectiveUnit(openClawRuntimeUnit(extra), "/home/alice"); err == nil {
				t.Fatal("unsafe effective ExecStart set was accepted")
			}
		})
	}
}

func TestValidateOpenClawEffectiveUnitRejectsUnboundPaths(t *testing.T) {
	tests := map[string]string{
		"missing home":            "[Service]\nExecStart=/usr/bin/node index.js gateway\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\n",
		"wrong home":              openClawRuntimeUnit("Environment=HOME=/srv/openclaw\n"),
		"state env":               openClawRuntimeUnit("Environment=OPENCLAW_STATE_DIR=/srv/state\n"),
		"profile env":             openClawRuntimeUnit("Environment=OPENCLAW_PROFILE=blue\n"),
		"config env":              openClawRuntimeUnit("Environment=OPENCLAW_CONFIG_PATH=/srv/openclaw.json\n"),
		"container env":           openClawRuntimeUnit("Environment=OPENCLAW_CONTAINER=runtime-box\n"),
		"environment file":        openClawRuntimeUnit("EnvironmentFile=-/home/alice/openclaw.env\n"),
		"pass environment":        openClawRuntimeUnit("PassEnvironment=OPENCLAW_PROFILE\n"),
		"node options":            openClawRuntimeUnit("Environment=NODE_OPTIONS=--require=/tmp/hook.js\n"),
		"node path":               openClawRuntimeUnit("PassEnvironment=NODE_PATH\n"),
		"loader preload":          openClawRuntimeUnit("Environment=LD_PRELOAD=/tmp/hook.so\n"),
		"changed service user":    openClawRuntimeUnit("User=mallory\n"),
		"different cwd":           openClawRuntimeUnit("WorkingDirectory=/srv/openclaw\n"),
		"optional cwd":            openClawRuntimeUnit("WorkingDirectory=-/home/alice\n"),
		"optional home shorthand": openClawRuntimeUnit("WorkingDirectory=-~\n"),
		"dbus display prefix":     openClawRuntimeUnit("WorkingDirectory=!/home/alice\n"),
		"profile argv":            strings.Replace(openClawRuntimeUnit(""), "gateway --port", "--profile blue gateway --port", 1),
		"profile argv equals":     strings.Replace(openClawRuntimeUnit(""), "gateway --port", "--profile=blue gateway --port", 1),
		"dev argv":                strings.Replace(openClawRuntimeUnit(""), "gateway --port", "gateway --dev --port", 1),
		"container argv":          strings.Replace(openClawRuntimeUnit(""), "gateway --port", "--container runtime-box gateway --port", 1),
		"env argv":                strings.Replace(openClawRuntimeUnit(""), "/usr/bin/node", "/usr/bin/env OPENCLAW_PROFILE=blue /usr/bin/node", 1),
		"shell argv":              strings.Replace(openClawRuntimeUnit(""), "ExecStart=/usr/bin/node /opt/openclaw/dist/index.js gateway --port 18789", "ExecStart=/bin/sh -c 'OPENCLAW_STATE_DIR=/srv/state exec openclaw gateway'", 1),
	}
	for name, unit := range tests {
		t.Run(name, func(t *testing.T) {
			if err := validateOpenClawEffectiveUnit(unit, "/home/alice"); err == nil {
				t.Fatal("unsafe OpenClaw runtime was accepted")
			}
		})
	}
}

func TestValidateOpenClawContinuationCannotHideSelector(t *testing.T) {
	unit := "[Service]\n" +
		"ExecStart=/usr/bin/node \"/opt/openclaw/dist/index.js gateway \\\n" +
		"# ignored by systemd\n" +
		" ; ignored with backslash\\\n" +
		"--profile blue\"\n" +
		"Environment=HOME=/home/alice\n" +
		"Environment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\n"
	if err := validateOpenClawEffectiveUnit(unit, "/home/alice"); err == nil {
		t.Fatal("selector hidden after continued-line comments was accepted")
	}
}

func TestValidateTelegramUnitsRejectDynamicSystemdExpansion(t *testing.T) {
	for name, unit := range map[string]string{
		"openclaw environment specifier": openClawRuntimeUnit("Environment=SAFE_VALUE=%i\n"),
		"openclaw unset specifier":       openClawRuntimeUnit("UnsetEnvironment=%i\n"),
		"openclaw pass specifier":        openClawRuntimeUnit("PassEnvironment=%i\n"),
		"openclaw user specifier":        openClawRuntimeUnit("User=%i\n"),
		"openclaw cwd specifier":         openClawRuntimeUnit("WorkingDirectory=%h\n"),
		"openclaw exec specifier":        strings.Replace(openClawRuntimeUnit(""), "gateway --port", "gateway %i --port", 1),
		"openclaw exec variable":         openClawRuntimeUnit("Environment=EXTRA=--profile\\sblue\nExecStart=\nExecStart=/usr/bin/node /opt/openclaw/dist/index.js gateway $EXTRA\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateOpenClawEffectiveUnit(unit, "/home/alice"); err == nil {
				t.Fatal("dynamic OpenClaw unit was accepted")
			}
		})
	}
	for name, unit := range map[string]string{
		"hermes environment specifier": hermesRuntimeUnit("Environment=SAFE_VALUE=%i\n"),
		"hermes escaped specifier":     hermesRuntimeUnit(`Environment="SAFE_VALUE=\x25i"` + "\n"),
		"hermes unset specifier":       hermesRuntimeUnit("UnsetEnvironment=%i\n"),
		"hermes pass specifier":        hermesRuntimeUnit("PassEnvironment=%i\n"),
		"hermes user specifier":        hermesRuntimeUnit("User=%i\n"),
		"hermes exec specifier":        strings.Replace(hermesRuntimeUnit(""), "gateway run", "gateway run %i", 1),
		"hermes exec variable":         hermesRuntimeUnit("Environment=EXTRA=--profile\\sblue\nExecStart=\nExecStart=/opt/hermes/venv/bin/python -m hermes_cli.main gateway run $EXTRA\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateHermesEffectiveUnit(unit); err == nil {
				t.Fatal("dynamic Hermes unit was accepted")
			}
		})
	}

	if err := validateOpenClawEffectiveUnit(openClawRuntimeUnit("Environment=SAFE_VALUE=%%i\n"), "/home/alice"); err != nil {
		t.Fatalf("escaped literal percent rejected: %v", err)
	}
}

func TestValidateOpenClawRuntimeDotEnvFilesChecksGatewayCwd(t *testing.T) {
	oldLookup := openClawLookupUserIdentity
	t.Cleanup(func() { openClawLookupUserIdentity = oldLookup })

	home := t.TempDir()
	user := "cwd-dotenv-test"
	identity := &persistedUserIdentity{UID: os.Getuid(), GID: os.Getgid(), Home: home}
	openClawLookupUserIdentity = func(got string) (localUserIdentity, error) {
		if got != user {
			return localUserIdentity{}, errors.New("unexpected user")
		}
		return localUserIdentity{
			Name: user, UID: identity.UID, GID: identity.GID,
			UIDText: strconv.Itoa(identity.UID), GIDText: strconv.Itoa(identity.GID), Home: home,
		}, nil
	}
	path := filepath.Join(home, ".env")
	for name, content := range map[string]string{
		"equals":            "OPENCLAW_STATE_DIR=/srv/other\n",
		"Node colon syntax": "OPENCLAW_STATE_DIR: /srv/other\n",
		"after bare CR":     "OPENAI_API_KEY=secret\rOPENCLAW_PROFILE=blue\r",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := validateOpenClawRuntimeDotEnvFiles(user, identity); err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("%s gateway cwd selector was not rejected with its path: %v", name, err)
		}
	}
	if err := os.WriteFile(path, []byte("OPENAI_API_KEY=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateOpenClawRuntimeDotEnvFiles(user, identity); err != nil {
		t.Fatalf("unrelated gateway cwd dotenv key rejected: %v", err)
	}
}

func TestValidateTelegramUnitsRejectLateEnvironmentAndPathNamespaces(t *testing.T) {
	directives := map[string]string{
		"DynamicUser":          "yes",
		"ExecCondition":        "/usr/bin/true",
		"ExecStartPre":         "/usr/bin/true",
		"ExecStartPost":        "/usr/bin/true",
		"ExecStop":             "/usr/bin/true",
		"ExecStopPost":         "/usr/bin/true",
		"PAMName":              "pam_env",
		"RootDirectory":        "/srv/root",
		"RootImage":            "/srv/root.raw",
		"BindPaths":            "/srv/other:/home/alice",
		"BindReadOnlyPaths":    "/srv/other:/home/alice",
		"TemporaryFileSystem":  "/home/alice",
		"InaccessiblePaths":    "/home/alice/.openclaw/openclaw.json",
		"MountImages":          "/srv/config.raw:/home/alice",
		"ExtensionImages":      "/srv/extension.raw",
		"ExtensionDirectories": "/srv/extension",
		"ProtectHome":          "tmpfs",
		"Type":                 "oneshot",
		"PrivateNetwork":       "yes",
		"NetworkNamespacePath": "/run/netns/isolated",
	}
	for directive, value := range directives {
		t.Run(directive+"/openclaw", func(t *testing.T) {
			if err := validateOpenClawEffectiveUnit(openClawRuntimeUnit(directive+"="+value+"\n"), "/home/alice"); err == nil {
				t.Fatal("OpenClaw unit with alternate environment or file view was accepted")
			}
		})
		t.Run(directive+"/hermes", func(t *testing.T) {
			if _, err := validateHermesEffectiveUnit(hermesRuntimeUnit(directive + "=" + value + "\n")); err == nil {
				t.Fatal("Hermes unit with alternate environment or file view was accepted")
			}
		})
	}

	reset := "PAMName=pam_env\nPAMName=\nRootDirectory=/srv/root\nRootDirectory=\nBindPaths=/srv/other:/home/alice\nBindPaths=\nProtectHome=tmpfs\nProtectHome=no\nDynamicUser=yes\nDynamicUser=no\nType=oneshot\nType=simple\nPrivateNetwork=yes\nPrivateNetwork=no\nNetworkNamespacePath=/run/netns/isolated\nNetworkNamespacePath=\nExecCondition=/usr/bin/false\nExecCondition=\nExecStartPre=/usr/bin/false\nExecStartPre=\nExecStartPost=/usr/bin/false\nExecStartPost=\nExecStop=/usr/bin/false\nExecStop=\nExecStopPost=/usr/bin/false\nExecStopPost=\n"
	if err := validateOpenClawEffectiveUnit(openClawRuntimeUnit(reset), "/home/alice"); err != nil {
		t.Fatalf("fully reset directives rejected for OpenClaw: %v", err)
	}
	if _, err := validateHermesEffectiveUnit(hermesRuntimeUnit(reset)); err != nil {
		t.Fatalf("fully reset directives rejected for Hermes: %v", err)
	}

	for name, unit := range map[string]string{
		"openclaw": "[Unit]\nJoinsNamespaceOf=isolated.service\n" + openClawRuntimeUnit(""),
		"hermes":   "[Unit]\nJoinsNamespaceOf=isolated.service\n" + hermesRuntimeUnit(""),
	} {
		t.Run("JoinsNamespaceOf/"+name, func(t *testing.T) {
			var err error
			if name == "openclaw" {
				err = validateOpenClawEffectiveUnit(unit, "/home/alice")
			} else {
				_, err = validateHermesEffectiveUnit(unit)
			}
			if err == nil {
				t.Fatal("unit sharing another network namespace was accepted")
			}
		})
	}
	joinsReset := "[Unit]\nJoinsNamespaceOf=isolated.service\nJoinsNamespaceOf=\n"
	if err := validateOpenClawEffectiveUnit(joinsReset+openClawRuntimeUnit(""), "/home/alice"); err != nil {
		t.Fatalf("reset OpenClaw JoinsNamespaceOf rejected: %v", err)
	}
	if _, err := validateHermesEffectiveUnit(joinsReset + hermesRuntimeUnit("")); err != nil {
		t.Fatalf("reset Hermes JoinsNamespaceOf rejected: %v", err)
	}

	for _, directive := range []string{"ExecCondition", "ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost"} {
		extra := directive + "=/usr/bin/true\n" + directive + "=/usr/bin/false\n"
		if err := validateOpenClawEffectiveUnit(openClawRuntimeUnit(extra), "/home/alice"); err == nil {
			t.Errorf("multiple active %s definitions were accepted for OpenClaw", directive)
		}
		if _, err := validateHermesEffectiveUnit(hermesRuntimeUnit(extra)); err == nil {
			t.Errorf("multiple active %s definitions were accepted for Hermes", directive)
		}
	}
}

func TestValidateHermesEffectiveUnitRejectsMultipleExecStarts(t *testing.T) {
	for name, extra := range map[string]string{
		"unrelated command": "Type=oneshot\nExecStart=/usr/bin/true\n",
		"second gateway":    "Type=oneshot\nExecStart=/opt/hermes/venv/bin/python -m hermes_cli.main gateway run\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateHermesEffectiveUnit(hermesRuntimeUnit(extra)); err == nil {
				t.Fatal("Hermes unit with multiple ExecStart commands was accepted")
			}
		})
	}
}

func TestValidateHermesEffectiveUnitAllowsBoundCleanupHook(t *testing.T) {
	cleanup := "ExecStopPost=-/opt/hermes/venv/bin/python -m gateway.cgroup_cleanup\n"
	for name, unit := range map[string]string{
		"official cleanup": hermesRuntimeUnit(cleanup),
		"reset then official cleanup": hermesRuntimeUnit(
			"ExecStopPost=/usr/bin/false\nExecStopPost=\n" + cleanup,
		),
		"console script gateway": "[Service]\n" +
			"ExecStart=/opt/hermes/venv/bin/hermes-agent gateway run\n" + cleanup,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateHermesEffectiveUnit(unit); err != nil {
				t.Fatalf("bound Hermes cleanup hook rejected: %v", err)
			}
		})
	}
	if err := validateOpenClawEffectiveUnit(openClawRuntimeUnit(cleanup), "/home/alice"); err == nil {
		t.Fatal("OpenClaw unit with Hermes cleanup hook was accepted")
	}
}

func TestValidateHermesEffectiveUnitRejectsUnboundCleanupHook(t *testing.T) {
	for name, cleanup := range map[string]string{
		"missing ignore-errors prefix": "/opt/hermes/venv/bin/python -m gateway.cgroup_cleanup",
		"different project root":       "-/srv/hermes/venv/bin/python -m gateway.cgroup_cleanup",
		"different interpreter":        "-/opt/hermes/venv/bin/python3 -m gateway.cgroup_cleanup",
		"different module":             "-/opt/hermes/venv/bin/python -m gateway.other_cleanup",
		"extra argument":               "-/opt/hermes/venv/bin/python -m gateway.cgroup_cleanup --all",
		"credential prefix":            "+-/opt/hermes/venv/bin/python -m gateway.cgroup_cleanup",
		"shell wrapper":                "-/bin/sh -c /opt/hermes/venv/bin/python\\ -m\\ gateway.cgroup_cleanup",
		"additional active command":    "-/opt/hermes/venv/bin/python -m gateway.cgroup_cleanup\nExecStopPost=/usr/bin/true",
	} {
		t.Run(name, func(t *testing.T) {
			unit := hermesRuntimeUnit("ExecStopPost=" + cleanup + "\n")
			if _, err := validateHermesEffectiveUnit(unit); err == nil {
				t.Fatal("Hermes unit with unbound cleanup hook was accepted")
			}
		})
	}
}

func TestValidateHermesEffectiveUnitRejectsChrootWrapper(t *testing.T) {
	unit := "[Service]\n" +
		"ExecStart=/usr/sbin/chroot /srv/hermes-root /root/.hermes/hermes-agent/venv/bin/python -m hermes_cli.main gateway run\n"
	if unitIsHermesGatewayExec(unit) {
		t.Fatal("chroot wrapper was identified as a host Hermes gateway")
	}
	if _, err := validateHermesEffectiveUnit(unit); err == nil {
		t.Fatal("chroot wrapper was accepted by the Hermes runtime validator")
	}
}

func TestEffectiveEnvironmentFileReset(t *testing.T) {
	content := "[Service]\nEnvironmentFile=/etc/hidden.env\nEnvironmentFile=\n"
	files, err := effectiveServiceEnvironmentFiles(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("empty EnvironmentFile did not reset earlier values: %v", files)
	}
	content += "EnvironmentFile=-/etc/final.env\n"
	files, err = effectiveServiceEnvironmentFiles(content)
	if err != nil || len(files) != 1 || files[0] != "-/etc/final.env" {
		t.Fatalf("effective EnvironmentFile mismatch: files=%v err=%v", files, err)
	}
}

func TestEffectivePassEnvironmentReset(t *testing.T) {
	content := "[Service]\nPassEnvironment=NO_PROXY TELEGRAM_PROXY\nPassEnvironment=\n"
	names, err := effectiveServicePassEnvironment(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Fatalf("empty PassEnvironment did not reset earlier values: %v", names)
	}
	content += "PassEnvironment=SAFE_VALUE\n"
	names, err = effectiveServicePassEnvironment(content)
	if err != nil || len(names) != 1 || names[0] != "SAFE_VALUE" {
		t.Fatalf("effective PassEnvironment mismatch: names=%v err=%v", names, err)
	}
}

func TestParseAndValidateSystemdManagerEnvironment(t *testing.T) {
	environment, err := parseSystemdManagerEnvironment("OPENCLAW_PROFILE=blue\nNO_PROXY=\"api.telegram.org,localhost\"\nSPACED=\"two words\"\nEMPTY=\n")
	if err != nil {
		t.Fatal(err)
	}
	if environment["SPACED"] != "two words" || environment["EMPTY"] != "" {
		t.Fatalf("parsed manager environment mismatch: %#v", environment)
	}
	if err := validateOpenClawManagerEnvironment(openClawRuntimeUnit(""), environment); err == nil {
		t.Fatal("manager OpenClaw selector was accepted")
	}
	if err := validateOpenClawManagerEnvironment(openClawRuntimeUnit("UnsetEnvironment=OPENCLAW_PROFILE\n"), environment); err != nil {
		t.Fatalf("final UnsetEnvironment did not remove manager selector: %v", err)
	}
	if err := validateHermesManagerEnvironment(environment); err == nil {
		t.Fatal("manager Hermes NO_PROXY bypass was accepted")
	}
	if err := validateOpenClawManagerEnvironment(openClawRuntimeUnit(""), map[string]string{"NODE_OPTIONS": "--require=/tmp/hook.js"}); err == nil {
		t.Fatal("manager OpenClaw NODE_OPTIONS injection was accepted")
	}
	if err := validateHermesManagerEnvironment(map[string]string{"PYTHONPATH": "/tmp/hook"}); err == nil {
		t.Fatal("manager Hermes PYTHONPATH injection was accepted")
	}
	if err := validateHermesManagerEnvironment(map[string]string{"PYTHONSAFEPATH": "0"}); err == nil {
		t.Fatal("manager Hermes PYTHONSAFEPATH override was accepted")
	}
	if err := validateHermesManagerEnvironment(map[string]string{"PYTHONSAFEPATH": "1"}); err != nil {
		t.Fatalf("managed Hermes PYTHONSAFEPATH rejected: %v", err)
	}
	if err := validateHermesPythonSafePath(map[string]string{}, true); err == nil {
		t.Fatal("post-restart Hermes runtime accepted missing PYTHONSAFEPATH")
	}
	if err := validateHermesPythonSafePath(map[string]string{"PYTHONSAFEPATH": "1"}, true); err != nil {
		t.Fatalf("post-restart managed PYTHONSAFEPATH rejected: %v", err)
	}
	if _, err := parseSystemdManagerEnvironment("BROKEN VALUE=x\n"); err == nil {
		t.Fatal("invalid manager environment name was accepted")
	}
}

func TestEffectiveServiceEnvironmentWithBaseHonorsOverridesAndUnset(t *testing.T) {
	base := map[string]string{"NO_PROXY": "api.telegram.org", "REMOVE_ME": "manager", "KEEP": "manager"}
	content := "[Service]\nEnvironment=NO_PROXY=localhost KEEP=unit\nUnsetEnvironment=REMOVE_ME\n"
	effective, err := effectiveServiceEnvironmentWithBase(content, base)
	if err != nil {
		t.Fatal(err)
	}
	if effective["NO_PROXY"] != "localhost" || effective["KEEP"] != "unit" {
		t.Fatalf("unit overrides not applied: %#v", effective)
	}
	if _, ok := effective["REMOVE_ME"]; ok {
		t.Fatalf("UnsetEnvironment did not remove inherited value: %#v", effective)
	}
}

func TestOpenClawExecSelectorDoesNotMatchSimilarFlags(t *testing.T) {
	argv := []string{"openclaw", "gateway", "--profile-id", "abc", "--developer-mode"}
	if selector := openClawExecSelector(argv); selector != "" {
		t.Fatalf("similar but unrelated flag matched selector %q", selector)
	}
}

func TestRejectOpenClawRuntimeConfigSelectors(t *testing.T) {
	rejected := map[string]string{
		"include":        `{"$include":"shared.json","channels":{"telegram":{}}}`,
		"vars state":     `{"env":{"vars":{"OPENCLAW_STATE_DIR":"/srv/state"}}}`,
		"direct profile": `{"env":{"OPENCLAW_PROFILE":"blue"}}`,
		"home":           `{"env":{"vars":{"HOME":"/srv/openclaw"}}}`,
		"shell env":      `{"env":{"shellEnv":{"enabled":true}}}`,
	}
	for name, raw := range rejected {
		t.Run(name, func(t *testing.T) {
			if err := rejectOpenClawRuntimeConfigSelectors([]byte(raw)); err == nil {
				t.Fatal("config selector was accepted")
			}
		})
	}
	for name, raw := range map[string]string{
		"unrelated env":      `{"env":{"vars":{"OPENAI_API_KEY":"secret"}}}`,
		"disabled shell env": `{"env":{"shellEnv":{"enabled":false}}}`,
		"empty selector":     `{"env":{"vars":{"OPENCLAW_PROFILE":""}}}`,
		"no env":             `{"channels":{"telegram":{}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := rejectOpenClawRuntimeConfigSelectors([]byte(raw)); err != nil {
				t.Fatalf("safe config rejected: %v", err)
			}
		})
	}
}

func TestDotenvDeclaredKeysFindsSelectionDeclarations(t *testing.T) {
	raw := []byte("\uFEFFOPENCLAW_STATE_DIR=/tmp/other\r# comment\n export\vOPENCLAW_PROFILE = blue\nOPENAI_API_KEY=x\r\n\tHOME=/srv/openclaw\nexport 'TELEGRAM_PROXY'=http://evil\n")
	got := dotenvDeclaredKeys(raw)
	want := []string{"OPENCLAW_STATE_DIR", "OPENCLAW_PROFILE", "OPENAI_API_KEY", "HOME", "TELEGRAM_PROXY"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("dotenv keys=%v want=%v", got, want)
	}
}

func TestOpenClawDotEnvDeclaredKeysMatchesNodeColonAssignments(t *testing.T) {
	raw := []byte("OPENAI_API_KEY=x\rOPENCLAW_STATE_DIR: /srv/other\nOPENCLAW_PROFILE:\tblue\nNOT_NODE_SYNTAX:value\nNOT_NODE_SYNTAX_EITHER : value\n")
	got := openClawDotEnvDeclaredKeys(raw)
	want := []string{"OPENAI_API_KEY", "OPENCLAW_STATE_DIR", "OPENCLAW_PROFILE"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("OpenClaw dotenv keys=%v want=%v", got, want)
	}
	if got := dotenvDeclaredKeys([]byte("OPENCLAW_STATE_DIR: /srv/other\n")); len(got) != 0 {
		t.Fatalf("python-dotenv scanner accepted Node-only colon assignment: %v", got)
	}
}

func TestHermesNoProxyMatchingMirrorsTelegramTargets(t *testing.T) {
	for _, entry := range []string{
		"*",
		"api.telegram.org",
		"telegram.org",
		".telegram.org",
		"*.telegram.org",
		"149.154.166.110",
		"149.154.160.0/20",
		"149.154.0.0/16",
		"149.155.0.0/16",
		"8.8.8.8",
		"8.8.8.0/24",
		"0.0.0.0/0",
	} {
		if !hermesNoProxyEntryMatchesTelegram(entry) {
			t.Errorf("expected Telegram bypass match for %q", entry)
		}
	}
	for _, entry := range []string{
		"localhost",
		"internal.example",
		"exampletelegram.org",
		"10.0.0.0/8",
		"127.0.0.1",
		"169.254.10.20",
		"192.168.1.1",
		"::1",
		"149.154.166.110:443",
		"api.telegram.org:443",
	} {
		if hermesNoProxyEntryMatchesTelegram(entry) {
			t.Errorf("unexpected Telegram bypass match for %q", entry)
		}
	}
}

func TestValidateHermesEffectiveUnitRejectsProxyBypass(t *testing.T) {
	for name, unit := range map[string]string{
		"upper":            hermesRuntimeUnit("Environment=NO_PROXY=localhost,api.telegram.org\n"),
		"lower":            hermesRuntimeUnit("Environment=no_proxy=149.154.160.0/20\n"),
		"wildcard":         hermesRuntimeUnit("Environment=NO_PROXY=*\n"),
		"environment file": hermesRuntimeUnit("EnvironmentFile=-/etc/hermes.env\n"),
		"pass environment": hermesRuntimeUnit("PassEnvironment=NO_PROXY\n"),
		"python home":      hermesRuntimeUnit("Environment=PYTHONHOME=/tmp/python\n"),
		"python path":      hermesRuntimeUnit("PassEnvironment=PYTHONPATH\n"),
		"python safe path": hermesRuntimeUnit("Environment=PYTHONSAFEPATH=0\n"),
		"pass safe path":   hermesRuntimeUnit("PassEnvironment=PYTHONSAFEPATH\n"),
		"loader preload":   hermesRuntimeUnit("Environment=LD_PRELOAD=/tmp/hook.so\n"),
		"exec environment": strings.Replace(hermesRuntimeUnit(""), "/opt/hermes/venv/bin/python", "/usr/bin/env NO_PROXY=api.telegram.org /opt/hermes/venv/bin/python", 1),
		"exec home":        strings.Replace(hermesRuntimeUnit(""), "/opt/hermes/venv/bin/python", "/usr/bin/env HOME=/srv/other /opt/hermes/venv/bin/python", 1),
		"s6 environment":   hermesRuntimeUnit("Environment=HERMES_S6_SUPERVISED_CHILD=1\n"),
		"s6 exec":          strings.Replace(hermesRuntimeUnit(""), "/opt/hermes/venv/bin/python", "/usr/bin/env HERMES_S6_SUPERVISED_CHILD=1 /opt/hermes/venv/bin/python", 1),
		"profile":          strings.Replace(hermesRuntimeUnit(""), "gateway run", "gateway run --profile blue", 1),
		"escaped no proxy": hermesRuntimeUnit(`Environment="N\x4f_PROXY=api.telegram.org"` + "\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateHermesEffectiveUnit(unit); err == nil {
				t.Fatal("Hermes bypass runtime was accepted")
			}
		})
	}
}

func TestRejectHermesDotEnvKeys(t *testing.T) {
	for _, raw := range []string{
		"NO_PROXY=localhost\n",
		"no_proxy=api.telegram.org\n",
		"TELEGRAM_PROXY=\n",
		"TELEGRAM_FALLBACK_IPS=8.8.8.8\n",
		"HERMES_HOME=/srv/hermes\n",
		"HERMES_MANAGED_DIR=/srv/managed\n",
		"HERMES_S6_SUPERVISED_CHILD=1\n",
		"PYTHONHOME=/tmp/python\n",
		"PYTHONPATH=/tmp/hook\n",
		"PYTHONSAFEPATH=0\n",
		"LD_PRELOAD=/tmp/hook.so\n",
		"'TELEGRAM_PROXY'=http://evil\n",
		"export 'NO_PROXY'=api.telegram.org\n",
	} {
		if err := rejectHermesDotEnvKeys("test.env", []byte(raw)); err == nil {
			t.Fatalf("routing declaration was accepted: %q", raw)
		}
	}
	if err := rejectHermesDotEnvKeys("test.env", []byte("TELEGRAM_BOT_TOKEN=secret\n")); err != nil {
		t.Fatalf("unrelated Hermes dotenv key rejected: %v", err)
	}
	if err := rejectHermesDotEnvKeys("test.env", []byte("OPENAI_API_KEY=secret\rNO_PROXY=api.telegram.org\r")); err == nil {
		t.Fatal("Hermes routing key after a bare CR was accepted")
	}
}

func TestRejectHermesDotEnvKeysHandlesUpstreamEncodings(t *testing.T) {
	for name, raw := range map[string][]byte{
		"utf16 little endian": encodeASCIIDotEnvUTF16("NO_PROXY=api.telegram.org\n", true),
		"utf16 big endian":    encodeASCIIDotEnvUTF16("TELEGRAM_PROXY=http://evil\n", false),
	} {
		t.Run(name, func(t *testing.T) {
			if err := rejectHermesDotEnvKeys("test.env", raw); err == nil {
				t.Fatal("UTF-16 routing override was accepted")
			}
		})
	}
	if err := rejectHermesDotEnvKeys("test.env", encodeASCIIDotEnvUTF16("OPENAI_API_KEY=secret\n", true)); err != nil {
		t.Fatalf("safe UTF-16 dotenv rejected: %v", err)
	}
	if err := rejectHermesDotEnvKeys("test.env", append([]byte{0xef, 0xbb, 0xbf}, []byte("OPENAI_API_KEY=secret\n")...)); err != nil {
		t.Fatalf("safe UTF-8 BOM dotenv rejected: %v", err)
	}

	for name, raw := range map[string][]byte{
		"utf32 little endian": {0xff, 0xfe, 0x00, 0x00, 'N', 0x00, 0x00, 0x00},
		"utf32 big endian":    {0x00, 0x00, 0xfe, 0xff, 0x00, 0x00, 0x00, 'N'},
		"no bom nul padded":   {'N', 0x00, 'O', 0x00, '_', 0x00, 'P', 0x00},
		"invalid utf8":        {0xc3, 0x28},
		"latin1 fallback":     {'O', 'P', 'E', 'N', 'A', 'I', '_', 'K', 'E', 'Y', '=', 0xe9},
	} {
		t.Run(name, func(t *testing.T) {
			if err := rejectHermesDotEnvKeys("test.env", raw); err == nil {
				t.Fatal("ambiguous or unsupported dotenv encoding was accepted")
			}
		})
	}
}

func TestRejectHermesDotEnvKeysFindsSanitizerSplitKey(t *testing.T) {
	raw := []byte("OPENAI_API_KEY=xTELEGRAM_PROXY=http://evil\n")
	if err := rejectHermesDotEnvKeys("test.env", raw); err == nil {
		t.Fatal("routing key repaired from a concatenated value was accepted")
	}
}

func encodeASCIIDotEnvUTF16(value string, littleEndian bool) []byte {
	encoded := []byte{0xfe, 0xff}
	if littleEndian {
		encoded = []byte{0xff, 0xfe}
	}
	for i := 0; i < len(value); i++ {
		if littleEndian {
			encoded = append(encoded, value[i], 0)
		} else {
			encoded = append(encoded, 0, value[i])
		}
	}
	return encoded
}

func TestValidateHermesManagerEnvironmentRejectsS6ProfileBypass(t *testing.T) {
	for _, value := range []string{"1", " "} {
		if err := validateHermesManagerEnvironment(map[string]string{"HERMES_S6_SUPERVISED_CHILD": value}); err == nil {
			t.Fatalf("manager HERMES_S6_SUPERVISED_CHILD=%q was accepted", value)
		}
	}
	if err := validateHermesManagerEnvironment(map[string]string{"HERMES_S6_SUPERVISED_CHILD": ""}); err != nil {
		t.Fatalf("empty manager HERMES_S6_SUPERVISED_CHILD rejected: %v", err)
	}
}

func TestRejectHermesConfigOverrides(t *testing.T) {
	for name, raw := range map[string]string{
		"bulk":             "secrets:\n  bitwarden:\n    enabled: true\n",
		"mapped":           "secrets:\n  onepassword:\n    enabled: true\n    env:\n      NO_PROXY: op://vault/item/value\n",
		"code mapped":      "secrets:\n  onepassword:\n    enabled: true\n    env:\n      PYTHONPATH: op://vault/item/value\n",
		"safe path mapped": "secrets:\n  onepassword:\n    enabled: true\n    env:\n      PYTHONSAFEPATH: op://vault/item/value\n",
		"plugin":           "secrets:\n  custom:\n    enabled: true\n",
		"bad enabled":      "secrets:\n  bitwarden:\n    enabled: yes-please\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := rejectHermesConfigOverrides("test-config.yaml", []byte(raw)); err == nil {
				t.Fatal("unsafe secret source was accepted")
			}
		})
	}
	for name, raw := range map[string]string{
		"upper no proxy":      "NO_PROXY: api.telegram.org\n",
		"lower no proxy":      "no_proxy: api.telegram.org\n",
		"fallback ips":        "TELEGRAM_FALLBACK_IPS: 8.8.8.8\n",
		"numeric selector":    "HERMES_S6_SUPERVISED_CHILD: 1\n",
		"boolean selector":    "PYTHONSAFEPATH: false\n",
		"duplicate route key": "NO_PROXY: localhost\nNO_PROXY: api.telegram.org\n",
		"malformed yaml":      "model: [unterminated\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := rejectHermesConfigOverrides("test-config.yaml", []byte(raw)); err == nil {
				t.Fatal("unsafe top-level Hermes config was accepted")
			}
		})
	}
	for name, raw := range map[string]string{
		"disabled bulk":    "secrets:\n  bitwarden:\n    enabled: false\n",
		"safe mapped":      "secrets:\n  onepassword:\n    enabled: true\n    env:\n      OPENAI_API_KEY: op://vault/item/value\n",
		"no secrets":       "model:\n  default: test\n",
		"unrelated scalar": "OPENAI_API_KEY: secret\n",
		"route mapping":    "NO_PROXY:\n  nested: value\n",
		"route list":       "TELEGRAM_FALLBACK_IPS:\n  - 8.8.8.8\n",
		"route null":       "no_proxy: null\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := rejectHermesConfigOverrides("test-config.yaml", []byte(raw)); err != nil {
				t.Fatalf("safe secret config rejected: %v", err)
			}
		})
	}
}

func TestRejectHermesManagedConfigFileSafety(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("managed Hermes files must be root-owned")
	}
	dir, err := os.MkdirTemp(".", ".hermes-managed-test-")
	if err != nil {
		t.Fatal(err)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(absDir) })
	path := filepath.Join(absDir, "config.yaml")

	if err := rejectHermesManagedConfig(path); err != nil {
		t.Fatalf("missing managed config rejected: %v", err)
	}
	if err := os.WriteFile(path, []byte("model: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rejectHermesManagedConfig(path); err != nil {
		t.Fatalf("safe managed config rejected: %v", err)
	}
	if err := os.WriteFile(path, []byte("NO_PROXY: api.telegram.org\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rejectHermesManagedConfig(path); err == nil {
		t.Fatal("managed top-level proxy bypass was accepted")
	}
	if err := os.WriteFile(path, []byte("model: [unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rejectHermesManagedConfig(path); err == nil {
		t.Fatal("malformed managed config was accepted")
	}
	if err := os.WriteFile(path, []byte("model: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o622); err != nil {
		t.Fatal(err)
	}
	if err := rejectHermesManagedConfig(path); err == nil {
		t.Fatal("group-writable managed config was accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := rejectHermesManagedConfig(path); err == nil {
		t.Fatal("non-root-owned managed config was accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", path); err != nil {
		t.Fatal(err)
	}
	if err := rejectHermesManagedConfig(path); err == nil {
		t.Fatal("managed config symlink was accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rejectHermesManagedConfig(path); err == nil {
		t.Fatal("managed config FIFO was accepted")
	}
}

func TestValidateHermesRuntimeFilesRejectsMismatchedHome(t *testing.T) {
	target := systemdTargetName{Service: "hermes-test.service"}
	environment := map[string]string{"HOME": "/srv/other", "HERMES_HOME": "/root/.hermes"}
	content := hermesRuntimeUnit("User=root\n")
	if err := validateHermesRuntimeFiles(target, nil, content, "/opt/hermes", environment); err == nil {
		t.Fatal("mismatched HOME was accepted")
	}
}

func TestValidHermesProfileName(t *testing.T) {
	for _, name := range []string{"default", "coder-2", "2nd_profile"} {
		if !validHermesProfileName(name) {
			t.Errorf("valid profile rejected: %q", name)
		}
	}
	for _, name := range []string{"", "../root", "Upper", strings.Repeat("a", 65)} {
		if validHermesProfileName(name) {
			t.Errorf("invalid profile accepted: %q", name)
		}
	}
}

func TestValidateHermesEffectiveUnitHonorsFinalEnvironment(t *testing.T) {
	for name, unit := range map[string]string{
		"no bypass":              hermesRuntimeUnit("Environment=NO_PROXY=localhost,.internal.example\n"),
		"managed safe path":      hermesRuntimeUnit("Environment=PYTHONSAFEPATH=1\n"),
		"safe path reset":        hermesRuntimeUnit("Environment=PYTHONSAFEPATH=0\nEnvironment=PYTHONSAFEPATH=\n"),
		"unset":                  hermesRuntimeUnit("Environment=NO_PROXY=api.telegram.org\nUnsetEnvironment=NO_PROXY\n"),
		"environment reset":      hermesRuntimeUnit("Environment=NO_PROXY=api.telegram.org\nEnvironment=\n"),
		"environment file reset": hermesRuntimeUnit("EnvironmentFile=/etc/hidden.env\nEnvironmentFile=\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateHermesEffectiveUnit(unit); err != nil {
				t.Fatalf("safe final Hermes runtime rejected: %v", err)
			}
		})
	}
}

func TestHermesGatewayKeepsBoundProjectRoot(t *testing.T) {
	for name, unit := range map[string]string{
		"python module":          hermesRuntimeUnit(""),
		"console script":         "[Service]\nExecStart=/opt/hermes/venv/bin/hermes-agent gateway run\n",
		"reset replaces project": "[Service]\nExecStart=/old/hermes/venv/bin/python -m hermes_cli.main gateway run\nExecStart=\nExecStart=/opt/hermes/venv/bin/python -m hermes_cli.main gateway run\n",
	} {
		t.Run(name, func(t *testing.T) {
			root, err := validateHermesEffectiveUnit(unit)
			if err != nil || root != "/opt/hermes" {
				t.Fatalf("validated project root=%q err=%v", root, err)
			}
		})
	}
}

func TestLocalInstalledTelegramRuntimeValidation(t *testing.T) {
	if os.Getenv("PROXYSCENE_LOCAL_RUNTIME_TEST") != "1" {
		t.Skip("set PROXYSCENE_LOCAL_RUNTIME_TEST=1 for the read-only installed-service check")
	}
	identity, err := capturePersistedUserIdentity("root", lookupLocalUserIdentity)
	if err != nil {
		t.Fatal(err)
	}
	openClawTarget := systemdTargetName{UserMode: true, User: "root", Service: "openclaw-gateway.service"}
	if err := validateOpenClawTargetRuntime(openClawTarget, identity); err != nil {
		t.Fatalf("installed OpenClaw runtime is not safely manageable: %v", err)
	}
	configPath := filepath.Join(identity.Home, ".openclaw", "openclaw.json")
	raw, err := readOpenClawRuntimeFile("root", identity, configPath, maxOpenClawConfigBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := rejectOpenClawRuntimeConfigSelectors(raw); err != nil {
		t.Fatalf("installed OpenClaw config selects another runtime path: %v", err)
	}
	if err := rejectOpenClawAccountProxyOverrides(raw); err != nil {
		t.Fatalf("installed OpenClaw config has account-level proxy overrides: %v", err)
	}

	hermesTarget := systemdTargetName{Service: "hermes-gateway.service"}
	if err := validateHermesTargetRuntime(hermesTarget, nil, ""); err != nil {
		t.Fatalf("installed Hermes runtime would bypass TELEGRAM_PROXY: %v", err)
	}
}
