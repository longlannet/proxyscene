package manager

import (
	"strings"
	"testing"
)

func TestHermesRestartPolicyPreservesPendingMessages(t *testing.T) {
	const preserve = "platforms:\n  telegram:\n    extra:\n      drop_pending_on_cold_boot: false\n"
	const discard = "platforms:\n  telegram:\n    extra:\n      drop_pending_on_cold_boot: true\n"
	for _, tt := range []struct {
		name, user, managed, legacy string
		allowed                     bool
	}{
		{name: "upstream default discards"},
		{name: "literal false", user: preserve, allowed: true},
		{name: "explicit true", user: discard},
		{name: "managed preserve overrides user", user: discard, managed: preserve, allowed: true},
		{name: "managed discard overrides user", user: preserve, managed: discard},
		{name: "managed only", managed: preserve, allowed: true},
		{name: "managed unrelated leaf retains user", user: preserve, managed: "platforms: {telegram: {extra: {proxy_url: http://127.0.0.1:7890}}}", allowed: true},
		{name: "managed null does not replace mapping", user: preserve, managed: "platforms: null", allowed: true},
		{name: "managed null leaf uses default", user: preserve, managed: "platforms: {telegram: {extra: {drop_pending_on_cold_boot: null}}}"},
		{name: "quoted false is not a proven boolean", user: strings.Replace(preserve, "false", "'false'", 1)},
		{name: "zero is not a proven boolean", user: strings.Replace(preserve, "false", "0", 1)},
		{name: "environment expression is not trusted", user: strings.Replace(preserve, "false", "'${KEEP_PENDING}'", 1)},
		{name: "legacy preserve", legacy: `{"platforms":{"telegram":{"extra":{"drop_pending_on_cold_boot":false}}}}`, allowed: true},
		{name: "YAML overrides legacy", user: preserve, legacy: `{"platforms":{"telegram":{"extra":{"drop_pending_on_cold_boot":true}}}}`, allowed: true},
		{name: "legacy extra beats YAML direct", user: "platforms: {telegram: {drop_pending_on_cold_boot: false}}", legacy: `{"platforms":{"telegram":{"extra":{"drop_pending_on_cold_boot":true}}}}`},
		{name: "nested platform", user: "gateway: {platforms: {telegram: {extra: {drop_pending_on_cold_boot: false}}}}", allowed: true},
		{name: "top level platforms beat nested", user: preserve + "gateway: {platforms: {telegram: {extra: {drop_pending_on_cold_boot: true}}}}", allowed: true},
		{name: "gateway telegram wins", user: preserve + "gateway: {telegram: {extra: {drop_pending_on_cold_boot: true}}}"},
		{name: "root telegram beats gateway", user: "gateway: {telegram: {extra: {drop_pending_on_cold_boot: true}}}\ntelegram: {drop_pending_on_cold_boot: false}", allowed: true},
		{name: "root telegram can discard", user: preserve + "telegram: {drop_pending_on_cold_boot: true}"},
		{name: "root explicit extra wins", user: "telegram: {drop_pending_on_cold_boot: true, extra: {drop_pending_on_cold_boot: false}}", allowed: true},
		{name: "explicit extra wins over direct", user: "platforms: {telegram: {drop_pending_on_cold_boot: true, extra: {drop_pending_on_cold_boot: false}}}", allowed: true},
		{name: "empty overriding extra preserves earlier key", user: preserve + "gateway: {telegram: {extra: {}}}", allowed: true},
		{name: "null extra is not trusted", user: preserve + "gateway: {telegram: {extra: null}}"},
		{name: "sibling failure before Telegram merge", user: "platforms: {discord: {extra: null}, telegram: {extra: {drop_pending_on_cold_boot: false}}}"},
		{name: "legacy sibling extra cannot invalidate YAML", user: preserve, legacy: `{"platforms":{"discord":{"extra":null}}}`},
		{name: "malformed user", user: "platforms: ["},
		{name: "malformed managed", user: preserve, managed: "platforms: ["},
		{name: "malformed legacy", user: preserve, legacy: "{"},
		{name: "multiple YAML documents", user: preserve + "---\n" + discard},
		{name: "duplicate YAML keys", user: preserve + "platforms: {}\n"},
		{name: "legacy multiplex", user: preserve, legacy: `{"multiplex_profiles":true}`},
		{name: "non mapping gateway", user: preserve + "gateway: '${GATEWAY_CONFIG}'"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := validateHermesTelegramRestartConfig("/fixture/.hermes/config.yaml", []byte(tt.user), []byte(tt.managed), []byte(tt.legacy))
			if (err == nil) != tt.allowed {
				t.Fatalf("allowed=%v err=%v", tt.allowed, err)
			}
		})
	}
}

func TestHermesRestartPolicyErrorIsActionableAndDoesNotExposeValues(t *testing.T) {
	err := validateHermesTelegramRestartConfig("/fixture/.hermes/config.yaml", []byte("platforms: {telegram: {extra: {drop_pending_on_cold_boot: 'secret-value'}}}"), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "platforms.telegram.extra.drop_pending_on_cold_boot: false") || !strings.Contains(err.Error(), "/fixture/.hermes/config.yaml") {
		t.Fatalf("missing actionable policy advice: %v", err)
	}
	if strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("policy error exposed a configuration value: %v", err)
	}
}

func TestHermesFallbackDiscoveryCannotBeOverriddenByApplicationFiles(t *testing.T) {
	for name, raw := range map[string]string{
		"user dotenv":              hermesDisableFallbackEnv + "=0\n",
		"empty dotenv":             hermesDisableFallbackEnv + "=\n",
		"quoted dotenv":            "export '" + hermesDisableFallbackEnv + "'=false\n",
		"repairable concatenation": "TOKEN=fixture" + hermesDisableFallbackEnv + "=0\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := rejectHermesDotEnvKeys("fixture.env", []byte(raw)); err == nil {
				t.Fatal("fallback discovery override accepted")
			}
		})
	}
	for name, raw := range map[string]string{
		"top level scalar":   hermesDisableFallbackEnv + ": false\n",
		"onepassword source": "secrets: {onepassword: {enabled: true, env: {" + hermesDisableFallbackEnv + ": 'op://fixture/value'}}}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := rejectHermesConfigOverrides("fixture.yaml", []byte(raw)); err == nil {
				t.Fatal("fallback discovery YAML override accepted")
			}
		})
	}
}

func TestHermesFallbackDiscoveryMustRemainDisabledAfterUnitMerge(t *testing.T) {
	for _, value := range []string{"", "0", "true", "false"} {
		if err := validateHermesFallbackDiscoveryEnvironment(map[string]string{hermesDisableFallbackEnv: value}, true); err == nil {
			t.Errorf("noncanonical managed value accepted: %q", value)
		}
	}
	if err := validateHermesFallbackDiscoveryEnvironment(map[string]string{hermesDisableFallbackEnv: "1"}, true); err != nil {
		t.Fatal(err)
	}
	if err := validateHermesFallbackDiscoveryEnvironment(nil, false); err != nil {
		t.Fatalf("pre-takeover absence must be permitted: %v", err)
	}
	if _, err := validateHermesEffectiveUnit(hermesRuntimeUnit("PassEnvironment=" + hermesDisableFallbackEnv + "\n")); err == nil {
		t.Fatal("manager-controlled fallback override accepted")
	}
}

func TestHermesRestartPolicyRejectsManagedScopeSuppression(t *testing.T) {
	// Hermes tests suppress /etc/hermes on key presence, including an empty
	// string. Do not mistake a managed false for the effective restart policy.
	for _, value := range []string{"", "fixture"} {
		err := validateHermesRuntimeFilesWithRestartPolicy(systemdTargetName{}, nil, "", "", map[string]string{"PYTEST_CURRENT_TEST": value}, true)
		if err == nil || !strings.Contains(err.Error(), "PYTEST_CURRENT_TEST") {
			t.Fatalf("managed-scope suppression accepted: %v", err)
		}
	}
	if err := rejectHermesDotEnvKeys("fixture.env", []byte("PYTEST_CURRENT_TEST=\n")); err == nil {
		t.Fatal("dotenv could suppress managed restart policy")
	}
	if err := rejectHermesConfigOverrides("fixture.yaml", []byte("PYTEST_CURRENT_TEST: fixture\n")); err == nil {
		t.Fatal("YAML could suppress managed restart policy")
	}
}
