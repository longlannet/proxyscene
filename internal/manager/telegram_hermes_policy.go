package manager

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

const hermesDropPendingKey = "drop_pending_on_cold_boot"
const hermesDisableFallbackEnv = "HERMES_TELEGRAM_DISABLE_FALLBACK_IPS"

// Keep restart consent separate from ownership/configuration checks. Removing an
// owned artifact must remain possible when application configuration has changed;
// the caller applies this guard before the disruptive process restart itself.
func validateHermesTelegramRestartPolicy(target systemdTargetName, identity *persistedUserIdentity) error {
	return validateHermesTargetRuntimeWithRestartPolicy(target, identity, "", true)
}

func validateHermesFallbackDiscoveryEnvironment(environment map[string]string, required bool) error {
	if required && environment[hermesDisableFallbackEnv] != "1" {
		return fmt.Errorf("最终 %s 与受管值 1 不一致，无法排除绕过 Telegram 代理的 DNS/DoH 发现", hermesDisableFallbackEnv)
	}
	return nil
}

// Hermes 0.21.5 loads legacy gateway.json, deep-merges the managed YAML over the
// user YAML, then merges gateway.platforms, platforms, and gateway.telegram in
// that order. A root telegram block is bridged last. Explicit extra values win
// over adapter keys alongside extra. Only a literal false is accepted: resolving
// arbitrary environment/secret references or Python coercions would enlarge the
// trust boundary without making restart safe across versions.
func validateHermesTelegramRestartConfig(configPath string, userRaw, managedRaw, legacyRaw []byte) error {
	user, err := decodeHermesRestartYAML(userRaw)
	if err != nil {
		return fmt.Errorf("无法证明 Hermes 重启会保留 Telegram 积压消息：%s 不是有效的单一 YAML 映射", configPath)
	}
	managed, err := decodeHermesRestartYAML(managedRaw)
	if err != nil {
		return fmt.Errorf("无法证明 Hermes 重启会保留 Telegram 积压消息：managed config.yaml 不是有效的单一 YAML 映射")
	}
	var legacy map[string]any
	if len(bytes.TrimSpace(legacyRaw)) != 0 {
		if err := json.Unmarshal(legacyRaw, &legacy); err != nil {
			return fmt.Errorf("无法证明 Hermes 重启会保留 Telegram 积压消息：gateway.json 不是有效的 JSON 映射")
		}
	}
	if err := rejectHermesMultiplexConfig(legacy); err != nil {
		return err
	}
	merged := mergeHermesRestartMappings(user, managed)
	// A malformed sibling platform extra can abort the upstream YAML phase
	// before Telegram is merged, leaving gateway.json/defaults effective.
	// Reject those shapes instead of assuming our selected key was applied.
	if err := validateHermesRestartPlatformShapes(legacy, false); err != nil {
		return hermesUnsafeRestartPolicyError(configPath)
	}
	if err := validateHermesRestartPlatformShapes(merged, true); err != nil {
		return hermesUnsafeRestartPolicyError(configPath)
	}
	policy := hermesRestartPolicyValue{}
	for _, source := range []struct {
		document map[string]any
		path     []string
	}{
		{legacy, []string{"platforms", "telegram"}},
		{merged, []string{"gateway", "platforms", "telegram"}},
		{merged, []string{"platforms", "telegram"}},
		{merged, []string{"gateway", "telegram"}},
	} {
		block, err := hermesRestartMappingAt(source.document, source.path...)
		if err != nil {
			return hermesUnsafeRestartPolicyError(configPath)
		}
		if err := policy.merge(block, false); err != nil {
			return hermesUnsafeRestartPolicyError(configPath)
		}
	}
	rootTelegram, err := hermesRestartMappingAt(merged, "telegram")
	if err != nil {
		return hermesUnsafeRestartPolicyError(configPath)
	}
	if err := policy.merge(rootTelegram, true); err != nil {
		return hermesUnsafeRestartPolicyError(configPath)
	}
	value, present := policy.direct, policy.directPresent
	if policy.extraPresent {
		value, present = policy.extra, true
	}
	if drop, ok := value.(bool); present && ok && !drop {
		return nil
	}
	return hermesUnsafeRestartPolicyError(configPath)
}

func hermesUnsafeRestartPolicyError(configPath string) error {
	return fmt.Errorf("拒绝重启 Hermes：无法证明 Telegram 的有效 drop_pending_on_cold_boot 为布尔值 false；默认冷启动会丢弃积压消息。请先在 %s 设置 platforms.telegram.extra.drop_pending_on_cold_boot: false，并检查 managed 配置、gateway.json 及其它 Telegram 配置段没有覆盖；proxyscene 不会自动修改此消息策略", configPath)
}

func decodeHermesRestartYAML(raw []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var result map[string]any
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("配置检查失败：Hermes 配置只能含一个 YAML 文档")
	}
	return result, nil
}

// Match hermes_cli.config._deep_merge, including its ignored null-over-mapping
// rule. No values are expanded, executed, printed, or written back to Hermes.
func mergeHermesRestartMappings(base, override map[string]any) map[string]any {
	result := make(map[string]any, len(base)+len(override))
	for key, value := range base {
		result[key] = value
	}
	for key, value := range override {
		if previous, ok := result[key].(map[string]any); ok {
			if replacement, ok := value.(map[string]any); ok {
				result[key] = mergeHermesRestartMappings(previous, replacement)
				continue
			}
			if value == nil {
				continue
			}
		}
		result[key] = value
	}
	return result
}

func hermesRestartMappingAt(document map[string]any, path ...string) (map[string]any, error) {
	current := document
	for _, key := range path {
		raw, present := current[key]
		if !present {
			return nil, nil
		}
		next, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("配置检查失败：Hermes Telegram 策略路径不是映射")
		}
		current = next
	}
	return current, nil
}

type hermesRestartPolicyValue struct {
	direct        any
	directPresent bool
	extra         any
	extraPresent  bool
}

func (p *hermesRestartPolicyValue) merge(block map[string]any, root bool) error {
	value, present := block[hermesDropPendingKey]
	if present {
		if root {
			p.extra, p.extraPresent = value, true
		} else {
			p.direct, p.directPresent = value, true
		}
	}
	if raw, present := block["extra"]; present {
		extra, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("配置检查失败：Hermes Telegram extra 不是映射")
		}
		if value, present := extra[hermesDropPendingKey]; present {
			p.extra, p.extraPresent = value, true
		}
	}
	return nil
}

// Upstream expands every platform before it constructs Telegram. In each merge,
// **block.get("extra", {}) requires a mapping even for unrelated platforms.
func validateHermesRestartPlatformShapes(document map[string]any, yamlLayer bool) error {
	sections := []any{document["platforms"]}
	if yamlLayer {
		if gateway, ok := document["gateway"].(map[string]any); ok {
			sections = append(sections, gateway["platforms"])
			for key, value := range gateway {
				if key != "platforms" {
					// Unknown plugin platform names can be registered dynamically.
					sections = append(sections, map[string]any{key: value})
				}
			}
		}
	}
	for _, raw := range sections {
		platforms, _ := raw.(map[string]any)
		for _, raw := range platforms {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if raw, present := block["extra"]; present {
				if _, ok := raw.(map[string]any); !ok {
					return fmt.Errorf("配置检查失败：Hermes platform extra 不是映射")
				}
			}
		}
	}
	return nil
}
