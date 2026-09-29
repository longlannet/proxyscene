package manager

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// These limits guard the pinned core's roomSize big.Int loop, session ID
// allocation, upload buffering, uncancellable sleeps, and XMUX pool/timers.
// They are admission/runtime limits only: parseXHTTPExtra keeps old raw links
// readable for listing, state recovery and node removal.
func validateXHTTPExtraRuntime(raw json.RawMessage, settings map[string]any) error {
	normalized, err := normalizeXHTTPExtra(url.Values{"extra": {string(raw)}})
	if err != nil {
		return err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(normalized, &object); err != nil {
		return fmt.Errorf("XHTTP extra JSON 无效")
	}
	return validateXHTTPExtraResourceLimits(object, settings)
}

func validateXHTTPExtraResourceLimits(object map[string]json.RawMessage, settings map[string]any) error {
	fields, err := checkedXHTTPFields(object, []string{
		"host", "path", "mode",
		"headers", "xPaddingBytes", "xPaddingObfsMode", "xPaddingKey", "xPaddingHeader", "xPaddingPlacement", "xPaddingMethod",
		"uplinkHTTPMethod", "sessionIDPlacement", "sessionIDKey", "sessionIDTable", "sessionIDLength", "seqPlacement", "seqKey",
		"uplinkDataPlacement", "uplinkDataKey", "uplinkChunkSize", "noGRPCHeader", "noSSEHeader", "scMaxEachPostBytes",
		"scMinPostsIntervalMs", "scMaxBufferedPosts", "scStreamUpServerSecs", "serverMaxHeaderBytes", "xmux",
	})
	if err != nil {
		return err
	}
	// The fixed core overwrites extra.host/path/mode with the outer settings.
	// Common generators repeat these fields: accept only equal effective
	// values so a conflicting request is never silently discarded.
	for _, key := range []string{"host", "path", "mode"} {
		raw, exists := fields[key]
		if !exists {
			continue
		}
		value, err := boundedXHTTPText(raw, 4096)
		if err != nil {
			return fmt.Errorf("XHTTP extra 冗余传输参数无效")
		}
		expected, _ := settings[key].(string)
		if key == "mode" {
			if value == "" {
				value = "auto"
			}
			if expected == "" {
				expected = "auto"
			}
		}
		if value != expected {
			return fmt.Errorf("XHTTP extra %s 与外层参数冲突", key)
		}
	}
	ranges := map[string]struct {
		minimum, maximum int64
		zeroDefault      bool
	}{
		"sessionIDLength":      {1, 64, true},
		"scMaxEachPostBytes":   {1, 4 << 20, true},
		"scMinPostsIntervalMs": {0, 60000, true},
		"scStreamUpServerSecs": {1, 3600, true},
		"uplinkChunkSize":      {64, 4 << 20, true},
	}
	bounds := make(map[string][2]int64)
	for key, limit := range ranges {
		if raw, ok := fields[key]; ok {
			left, right, err := boundedXHTTPRange(raw, limit.minimum, limit.maximum, limit.zeroDefault)
			if err != nil {
				return fmt.Errorf("XHTTP extra %s 参数无效或超过安全上限", key)
			}
			bounds[key] = [2]int64{left, right}
		}
	}
	for key, maximum := range map[string]int64{"scMaxBufferedPosts": 128, "serverMaxHeaderBytes": 64 << 10} {
		if raw, ok := fields[key]; ok {
			if _, err := boundedXHTTPInteger(raw, 0, maximum); err != nil {
				return fmt.Errorf("XHTTP extra %s 参数无效或超过安全上限", key)
			}
		}
	}
	for _, key := range []string{"xPaddingObfsMode", "noGRPCHeader", "noSSEHeader"} {
		if raw, ok := fields[key]; ok {
			if string(raw) != "true" && string(raw) != "false" {
				return fmt.Errorf("XHTTP extra 布尔参数无效")
			}
		}
	}
	for _, key := range []string{"xPaddingKey", "xPaddingHeader", "xPaddingPlacement", "xPaddingMethod", "uplinkHTTPMethod", "sessionIDPlacement", "sessionIDKey", "seqPlacement", "seqKey", "uplinkDataPlacement", "uplinkDataKey"} {
		if raw, ok := fields[key]; ok {
			if _, err := boundedXHTTPText(raw, 256); err != nil {
				return fmt.Errorf("XHTTP extra 字符串参数无效或过长")
			}
		}
	}
	table := ""
	if raw, ok := fields["sessionIDTable"]; ok {
		table, err = boundedXHTTPText(raw, 256)
		if err != nil {
			return fmt.Errorf("XHTTP extra sessionIDTable 无效或过长")
		}
		for _, ch := range table {
			if ch > 127 {
				return fmt.Errorf("XHTTP extra sessionIDTable 仅支持 ASCII")
			}
		}
	}
	length := bounds["sessionIDLength"]
	if (table != "" && length[0] == 0) || (table == "" && length[1] > 0) {
		return fmt.Errorf("XHTTP extra sessionIDTable/sessionIDLength 必须配套")
	}
	if table != "" && !validXHTTPSessionSpace(table, length[0], length[1]) {
		return fmt.Errorf("XHTTP extra sessionIDTable/sessionIDLength 组合空间不足")
	}
	if raw, ok := fields["headers"]; ok {
		if err := validateXHTTPHeaders(raw); err != nil {
			return err
		}
	}
	if raw, ok := fields["xmux"]; ok {
		if err := validateXHTTPXMUX(raw); err != nil {
			return err
		}
	}
	return nil
}

// encoding/json matches ASCII case and Unicode SimpleFold variants. Validate
// the same equivalence relation so a second spelling cannot override a bound.
func checkedXHTTPFields(object map[string]json.RawMessage, allowed []string) (map[string]json.RawMessage, error) {
	fields := make(map[string]json.RawMessage, len(object))
	for key, raw := range object {
		canonical := ""
		for _, candidate := range allowed {
			if strings.EqualFold(key, candidate) {
				canonical = candidate
				break
			}
		}
		if canonical == "" {
			return nil, fmt.Errorf("XHTTP extra 存在无法表达参数")
		}
		if _, exists := fields[canonical]; exists {
			return nil, fmt.Errorf("XHTTP extra 字段不能重复")
		}
		fields[canonical] = raw
	}
	return fields, nil
}

func boundedXHTTPRange(raw json.RawMessage, minimum, maximum int64, zeroDefault bool) (int64, int64, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		var number int64
		if err := json.Unmarshal(raw, &number); err != nil || strings.TrimSpace(string(raw)) == "null" {
			return 0, 0, fmt.Errorf("范围格式无效")
		}
		text = strconv.FormatInt(number, 10)
	}
	parts := strings.Split(text, "-")
	if len(parts) < 1 || len(parts) > 2 {
		return 0, 0, fmt.Errorf("范围格式无效")
	}
	values := make([]int64, len(parts))
	for i, part := range parts {
		if part == "" || strings.Trim(part, "0123456789") != "" {
			return 0, 0, fmt.Errorf("范围格式无效")
		}
		n, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("范围格式无效")
		}
		values[i] = n
	}
	left, right := values[0], values[len(values)-1]
	if left > right {
		left, right = right, left
	}
	if zeroDefault && left == 0 && right == 0 {
		return 0, 0, nil
	}
	if left < minimum || right > maximum {
		return 0, 0, fmt.Errorf("范围超过安全上限")
	}
	return left, right, nil
}

func boundedXHTTPInteger(raw json.RawMessage, minimum, maximum int64) (int64, error) {
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(string(raw)) == "null" || value < minimum || value > maximum {
		return 0, fmt.Errorf("数值参数无效或超过安全上限")
	}
	return value, nil
}

func boundedXHTTPText(raw json.RawMessage, maximum int) (string, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(string(raw)) == "null" || len(value) > maximum || !utf8.ValidString(value) || strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) >= 0 {
		return "", fmt.Errorf("字符串参数无效")
	}
	return value, nil
}

func validateXHTTPHeaders(raw json.RawMessage) error {
	var headers map[string]json.RawMessage
	if err := json.Unmarshal(raw, &headers); err != nil || headers == nil || len(headers) > 32 {
		return fmt.Errorf("XHTTP extra headers 参数无效或过多")
	}
	total := 0
	seen := make(map[string]bool, len(headers))
	for key, value := range headers {
		canonical := strings.ToLower(key)
		if seen[canonical] {
			return fmt.Errorf("XHTTP extra headers 字段不能重复")
		}
		seen[canonical] = true
		if key == "" || len(key) > 256 {
			return fmt.Errorf("XHTTP extra header 名无效")
		}
		for _, ch := range key {
			if !strings.ContainsRune("!#$%&'*+-.^_`|~0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ", ch) {
				return fmt.Errorf("XHTTP extra header 名无效")
			}
		}
		text, err := boundedXHTTPText(value, 4096)
		if err != nil {
			return fmt.Errorf("XHTTP extra header 值无效或过长")
		}
		total += len(key) + len(text)
		if total > 8192 {
			return fmt.Errorf("XHTTP extra headers 超过安全上限")
		}
	}
	return nil
}

func validateXHTTPXMUX(raw json.RawMessage) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return fmt.Errorf("XHTTP extra xmux 必须是对象")
	}
	fields, err := checkedXHTTPFields(object, []string{"maxConcurrency", "maxConnections", "cMaxReuseTimes", "hMaxRequestTimes", "hMaxReusableSecs", "hKeepAlivePeriod"})
	if err != nil {
		return err
	}
	maxima := make(map[string]int64)
	for key, maximum := range map[string]int64{"maxConcurrency": 1024, "maxConnections": 64, "cMaxReuseTimes": 1000000, "hMaxRequestTimes": 1000000, "hMaxReusableSecs": 86400} {
		if value, ok := fields[key]; ok {
			_, right, err := boundedXHTTPRange(value, 0, maximum, true)
			if err != nil {
				return fmt.Errorf("XHTTP extra xmux %s 参数无效或超过安全上限", key)
			}
			maxima[key] = right
		}
	}
	if maxima["maxConnections"] > 0 && maxima["maxConcurrency"] > 0 {
		return fmt.Errorf("XHTTP extra xmux maxConnections/maxConcurrency 冲突")
	}
	if value, ok := fields["hKeepAlivePeriod"]; ok {
		// The pinned core uses -1 to disable HTTP/2 or HTTP/3 keepalives.
		if _, err := boundedXHTTPInteger(value, -1, 3600); err != nil {
			return fmt.Errorf("XHTTP extra xmux hKeepAlivePeriod 参数无效或超过安全上限")
		}
	}
	return nil
}

// Match the fixed core's 2^31 session-space floor without its unbounded
// big.Int exponentiation. The earlier range check limits this to 64 steps.
func validXHTTPSessionSpace(table string, from, to int64) bool {
	size := uint64(len(table))
	switch table {
	case "ALPHABET", "alphabet":
		size = 26
	case "Alphabet":
		size = 52
	case "BASE36", "base36":
		size = 36
	case "Base62":
		size = 62
	case "HEX", "hex":
		size = 16
	case "number":
		size = 10
	}
	const required = uint64(1 << 31)
	power, sum := uint64(1), uint64(0)
	for k := int64(1); k <= to; k++ {
		if power >= required/size+1 {
			power = required
		} else {
			power *= size
			if power > required {
				power = required
			}
		}
		if k >= from {
			sum += power
			if sum >= required {
				return true
			}
		}
	}
	return false
}
