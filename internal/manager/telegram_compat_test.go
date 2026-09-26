package manager

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenClawOfficialMemoryArgumentsAndEntrypoints(t *testing.T) {
	for _, entry := range []string{"index.js", "index.mjs", "entry.js", "entry.mjs"} {
		for name, options := range map[string]string{
			"generated":    "--max-old-space-size=3969",
			"combined":     "--max-old-space-size=4096 --max-semi-space-size=32",
			"v8 spelling":  "--max_old_space_size=4096",
			"heap":         "--max-heap-size=4096",
			"percentage":   "--max-old-space-size-percentage=75",
			"node default": "--max-old-space-size=0",
		} {
			t.Run(entry+"/"+name, func(t *testing.T) {
				unit := strings.Replace(openClawRuntimeUnit(""), "/usr/bin/node /opt/openclaw/dist/index.js", "/usr/bin/node "+options+" /opt/openclaw/dist/"+entry, 1)
				if err := validateOpenClawEffectiveUnit(unit, "/home/alice"); err != nil {
					t.Fatalf("official entry with memory controls rejected: %v", err)
				}
			})
		}
	}
}

func TestOpenClawNodeOptionsRejectCodeInjectionAndMalformedLimits(t *testing.T) {
	for name, options := range map[string]string{
		"require":         "--max-old-space-size=4096 --require=/tmp/hook.js",
		"import":          "--import=/tmp/hook.mjs --max-old-space-size=4096",
		"eval":            "--eval=evil()",
		"eval short":      "-e evil()",
		"loader":          "--experimental-loader=/tmp/hook.mjs",
		"snapshot":        "--snapshot-blob=/tmp/evil.blob",
		"invalid suffix":  "--max-old-space-size=4096;evil",
		"hidden switch":   "--max-old-space-size=4096 --",
		"separated value": "--max-old-space-size 4096",
		"missing value":   "--max-old-space-size",
		"empty value":     "--max-old-space-size=",
		"negative":        "--max-old-space-size=-1",
		"overflow":        "--max-old-space-size=999999999999999999999999",
		"exponent":        "--max-old-space-size=4e3",
		"infinity":        "--max-old-space-size=Infinity",
		"percent zero":    "--max-old-space-size-percentage=0",
		"percent high":    "--max-old-space-size-percentage=101",
		"multiline":       "--max-old-space-size=4096\n--import=evil",
		"unclosed quote":  `"--max-old-space-size=4096`,
	} {
		t.Run(name, func(t *testing.T) {
			if openClawMemoryNodeOptions(options) {
				t.Fatal("unsafe NODE_OPTIONS accepted")
			}
			args := append([]string{"/usr/bin/node"}, strings.Fields(options)...)
			args = append(args, "/opt/openclaw/dist/index.js", "gateway")
			if openClawGatewayArgv(args) {
				t.Fatal("unsafe Node argv accepted")
			}
		})
	}
	for _, script := range []string{"dist/index.js", "/opt/not-openclaw/dist/index.js", "/opt/openclaw/dist/../dist/index.js", "/opt/openclaw/dist/other.mjs"} {
		if openClawGatewayArgv([]string{"/usr/bin/node", "--max-old-space-size=4096", script, "gateway"}) {
			t.Fatalf("unbound script accepted: %s", script)
		}
	}
}

func TestOpenClawMemoryNodeOptionsAcrossConfigurationSources(t *testing.T) {
	user, identity := contractTestIdentity(t)
	path := filepath.Join(identity.Home, ".env")
	for name, options := range map[string]string{
		"generated":     "--max-old-space-size=3969",
		"quoted tokens": `"--max-old-space-size=4096" "--max-heap-size=8192"`,
	} {
		t.Run(name, func(t *testing.T) {
			assignment, _ := json.Marshal("NODE_OPTIONS=" + options)
			unit := openClawRuntimeUnit("Environment=" + string(assignment) + "\n")
			if err := validateOpenClawEffectiveUnit(unit, "/home/alice"); err != nil {
				t.Fatalf("unit: %v", err)
			}
			if err := validateOpenClawManagerEnvironment(openClawRuntimeUnit("PassEnvironment=NODE_OPTIONS\n"), map[string]string{"NODE_OPTIONS": options}); err != nil {
				t.Fatalf("manager: %v", err)
			}
			value, _ := json.Marshal(options)
			for _, config := range []string{`{"env":{"NODE_OPTIONS":` + string(value) + `}}`, `{"env":{"vars":{"NODE_OPTIONS":` + string(value) + `}}}`} {
				if err := rejectOpenClawRuntimeConfigSelectors([]byte(config)); err != nil {
					t.Fatalf("config: %v", err)
				}
			}
			if err := os.WriteFile(path, []byte("export NODE_OPTIONS='"+options+"' # heap budget\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := rejectOpenClawSelectorDotEnv(user, identity, path); err != nil {
				t.Fatalf("dotenv: %v", err)
			}
		})
	}
	unit := openClawRuntimeUnit("PassEnvironment=NODE_OPTIONS\n")
	if err := validateOpenClawEffectiveUnit(unit, "/home/alice"); err != nil {
		t.Fatalf("NODE_OPTIONS inheritance rejected before effective value validation: %v", err)
	}
	if err := validateOpenClawManagerEnvironment(unit, map[string]string{"NODE_OPTIONS": "--import=/tmp/hook.mjs"}); err == nil {
		t.Fatal("inherited code injection accepted")
	}
	if err := validateOpenClawManagerEnvironment(openClawRuntimeUnit("Environment=NODE_OPTIONS=--max-old-space-size=4096\n"), map[string]string{"NODE_OPTIONS": "--import=/tmp/hook.mjs"}); err != nil {
		t.Fatalf("safe effective override rejected: %v", err)
	}
}

func TestOpenClawDotEnvNodeOptionsUsesConservativeSingleLineGrammar(t *testing.T) {
	user, identity := contractTestIdentity(t)
	path := filepath.Join(identity.Home, ".env")
	for name, raw := range map[string]string{
		"plain":           "NODE_OPTIONS=--max-old-space-size=3969\n",
		"colon":           "NODE_OPTIONS: --max-old-space-size=3969\n",
		"export":          "export NODE_OPTIONS=--max-old-space-size=3969 # memory\n",
		"quotes":          "NODE_OPTIONS=\"--max-old-space-size=3969 --max-semi-space-size=32\"\n",
		"empty":           "NODE_OPTIONS=''\n",
		"duplicates safe": "NODE_OPTIONS=--max-old-space-size=3969\nNODE_OPTIONS=--max-old-space-size=2048\n",
	} {
		t.Run("accept/"+name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if err := rejectOpenClawSelectorDotEnv(user, identity, path); err != nil {
				t.Fatal(err)
			}
		})
	}
	for name, raw := range map[string]string{
		"import":                 "NODE_OPTIONS=--max-old-space-size=3969 --import=/tmp/hook.mjs\n",
		"newline attached quote": "NODE_OPTIONS=\n \"--require=/tmp/hook.js\"\n",
		"distant attached quote": "NODE_OPTIONS=\n\n `--import=/tmp/hook.mjs`\n",
		"bare":                   "NODE_OPTIONS\n=--max-old-space-size=3969\n",
		"colon multiline":        "NODE_OPTIONS:\n --require=/tmp/hook.js\n",
		"quoted multiline":       "NODE_OPTIONS='--max-old-space-size=3969\n --import=/tmp/hook.mjs'\n",
		"escaped quote":          "NODE_OPTIONS=\"--max-old-space-size=3969\\\" --import=/tmp/hook.mjs\"\n",
		"ambiguous quoted key":   "'NODE_OPTIONS'=--max-old-space-size=3969\n",
		"duplicate unsafe":       "NODE_OPTIONS=--max-old-space-size=3969\nNODE_OPTIONS=--require=/tmp/hook.js\n",
		"adjacent string":        "NODE_OPTIONS='--max-old-space-size=3969' --require=/tmp/hook.js\n",
		"another selector":       "NODE_OPTIONS=--max-old-space-size=3969\nOPENCLAW_CONFIG_PATH=/tmp/other.json\n",
	} {
		t.Run("reject/"+name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if err := rejectOpenClawSelectorDotEnv(user, identity, path); err == nil {
				t.Fatal("unsafe dotenv accepted")
			}
		})
	}
}

func TestOpenClawRejectsNestedIncludesBeforeClaimingProxyOwnership(t *testing.T) {
	for name, raw := range map[string]string{
		"telegram account bypass": `{channels:{telegram:{$include:'telegram.json', proxy:'http://127.0.0.1:7890'}}}`,
		"environment bypass":      `{env:{$include:'runtime.json'}}`,
		"array element":           `{plugins:{entries:[{name:'fixture', config:{$include:['first.json','second.json']}}]}}`,
		"nested array":            `{items:[[{items:[{$include:'deep.json'}]}]]}`,
		"escaped key":             `{channels:{telegram:{'\u0024include':'telegram.json'}}}`,
		"null include":            `{channels:{telegram:{$include:null}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := rejectOpenClawRuntimeConfigSelectors([]byte(raw)); err == nil || !strings.Contains(err.Error(), "$include") {
				t.Fatalf("nested include not identified: %v", err)
			}
		})
	}
	// The current JSON5 library cannot decode identifier escapes; refusing the
	// complete document also prevents an escaped include from bypassing checks.
	if err := rejectOpenClawRuntimeConfigSelectors([]byte(`{channels:{telegram:{$incl\u0075de:'telegram.json'}}}`)); err == nil {
		t.Fatal("escaped include identifier accepted")
	}
	if err := rejectOpenClawRuntimeConfigSelectors([]byte(`{description:'literal $include', /* $include:'ignored.json' */ env:{vars:{NODE_OPTIONS:'--max-old-space-size=3969'}}, items:[null, true, 42, 'text']}`)); err != nil {
		t.Fatalf("ordinary JSON5 rejected: %v", err)
	}
}

func TestHermesOfficialPlannedStopHookIsBoundToProject(t *testing.T) {
	stop := "ExecStop=-/opt/hermes/venv/bin/python -m gateway.systemd_stop_mark\n"
	for name, unit := range map[string]string{
		"official":           hermesRuntimeUnit(stop),
		"reset":              hermesRuntimeUnit("ExecStop=/usr/bin/false\nExecStop=\n" + stop),
		"with cleanup":       hermesRuntimeUnit(stop + "ExecStopPost=-/opt/hermes/venv/bin/python -m gateway.cgroup_cleanup\n"),
		"console entrypoint": "[Service]\nExecStart=/opt/hermes/venv/bin/hermes-agent gateway run\n" + stop,
	} {
		t.Run("accept/"+name, func(t *testing.T) {
			if _, err := validateHermesEffectiveUnit(unit); err != nil {
				t.Fatal(err)
			}
		})
	}
	for name, command := range map[string]string{
		"other project":           "-/srv/hermes/venv/bin/python -m gateway.systemd_stop_mark",
		"other python":            "-/opt/hermes/venv/bin/python3 -m gateway.systemd_stop_mark",
		"other module":            "-/opt/hermes/venv/bin/python -m gateway.untrusted",
		"explicit PID":            "-/opt/hermes/venv/bin/python -m gateway.systemd_stop_mark 123",
		"variable PID":            "-/opt/hermes/venv/bin/python -m gateway.systemd_stop_mark $MAINPID",
		"module search injection": "-/opt/hermes/venv/bin/python -c evil",
		"no failure prefix":       "/opt/hermes/venv/bin/python -m gateway.systemd_stop_mark",
		"privilege prefix":        "+-/opt/hermes/venv/bin/python -m gateway.systemd_stop_mark",
		"shell":                   "-/bin/sh -c '/opt/hermes/venv/bin/python -m gateway.systemd_stop_mark'",
		"extra command":           "-/opt/hermes/venv/bin/python -m gateway.systemd_stop_mark\nExecStop=/usr/bin/true",
		"systemd specifier":       "-/opt/%u/venv/bin/python -m gateway.systemd_stop_mark",
	} {
		t.Run("reject/"+name, func(t *testing.T) {
			if _, err := validateHermesEffectiveUnit(hermesRuntimeUnit("ExecStop=" + command + "\n")); err == nil {
				t.Fatal("unbound stop hook accepted")
			}
		})
	}
	if err := validateOpenClawEffectiveUnit(openClawRuntimeUnit(stop), "/home/alice"); err == nil {
		t.Fatal("Hermes stop hook accepted for OpenClaw")
	}
}
