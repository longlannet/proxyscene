package manager

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// VMess generators sometimes populate HTTP header templates even with an
// explicit type=none. Restrict this compatibility exception to that call site;
// VLESS/Trojan RAW and VMess without an explicit header choice stay strict.
func discardVMessRawHeaderPlaceholders(q url.Values) error {
	hosts, err := splitCSV(q.Get("host"))
	if err != nil {
		return fmt.Errorf("VMess 冗余 RAW host 无效")
	}
	for _, host := range hosts {
		if err := validateTransportHost("host", host); err != nil {
			return err
		}
	}
	if path := q.Get("path"); path != "" {
		if err := validateTransportPath(path); err != nil {
			return err
		}
		if _, err := url.Parse(path); err != nil {
			return fmt.Errorf("VMess 冗余 RAW path 无效")
		}
	}
	q.Del("host")
	q.Del("path")
	return nil
}

// The pinned Xray consumes early data from the path query, for both WS and
// HTTPUpgrade. Both share-link spellings must describe the same positive size;
// repeated path parameters and unsupported early-data headers remain errors.
func normalizeHTTPEarlyData(path, separate string, enabled bool) (string, error) {
	u, err := url.Parse(path)
	if err != nil {
		return "", fmt.Errorf("transport path 无效")
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", fmt.Errorf("transport path query 无效")
	}
	if _, ok := values["eh"]; ok {
		return "", fmt.Errorf("transport path 不能直接包含 eh 参数")
	}
	pathValues, hasPathValue := values["ed"]
	if hasPathValue && len(pathValues) != 1 {
		return "", fmt.Errorf("transport path ed 参数不能重复")
	}
	separate = strings.TrimSpace(separate)
	if !hasPathValue && separate == "" {
		return path, nil
	}
	if !enabled {
		return "", fmt.Errorf("当前 transport 不支持 early data")
	}
	parseSize := func(raw string) (uint64, error) {
		// Keep the old uint32 syntax readable in saved nodes. Imports and
		// rendering apply the portable runtime bound separately below.
		value, err := strconv.ParseUint(raw, 10, 32)
		if err != nil || value == 0 {
			return 0, fmt.Errorf("early data 参数无效")
		}
		return value, nil
	}
	var size uint64
	if hasPathValue {
		size, err = parseSize(pathValues[0])
		if err != nil {
			return "", err
		}
	}
	if separate != "" {
		separateSize, err := parseSize(separate)
		if err != nil {
			return "", err
		}
		if hasPathValue && size != separateSize {
			return "", fmt.Errorf("path ed 与独立 ed 参数冲突")
		}
		size = separateSize
	}
	values.Set("ed", strconv.FormatUint(size, 10))
	u.RawQuery = values.Encode()
	return u.String(), nil
}

// Existing extra objects must remain readable so operators can remove old
// nodes after validation becomes stricter. Only the new alias requires eager
// normalization; old releases never accepted that spelling in stored links.
func parseXHTTPExtra(q url.Values) (json.RawMessage, error) {
	if _, exists := q["x_padding_bytes"]; exists {
		return normalizeXHTTPExtra(q)
	}
	raw := q.Get("extra")
	if raw == "" {
		return nil, nil
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(raw), &object); err != nil || object == nil {
		return nil, fmt.Errorf("XHTTP extra 必须是 JSON 对象")
	}
	return json.RawMessage(raw), nil
}

// Apply safety limits at both admission and configuration rendering, leaving
// syntax-only parsing usable by state recovery, listing and node removal.
func validateNodeTransportRuntimeCompatibility(pn *parsedNode) error {
	stream, ok := pn.Outbound["streamSettings"].(map[string]any)
	if !ok {
		return nil
	}
	network, _ := stream["network"].(string)
	switch network {
	case "xhttp":
		settings, _ := stream["xhttpSettings"].(map[string]any)
		if extra, ok := settings["extra"].(json.RawMessage); ok {
			return validateXHTTPExtraRuntime(extra, settings)
		}
	case "ws", "httpupgrade":
		settings, _ := stream[network+"Settings"].(map[string]any)
		path, _ := settings["path"].(string)
		if err := validateTransportPath(path); err != nil {
			return err
		}
		u, err := url.Parse(path)
		if err != nil {
			return fmt.Errorf("transport path 无效")
		}
		// Xray uses strconv.Atoi, including on supported 32-bit targets.
		// Fail before runtime rather than silently overflowing its ed value.
		if ed := u.Query().Get("ed"); ed != "" {
			if _, err := strconv.ParseUint(ed, 10, 31); err != nil {
				return fmt.Errorf("early data 不能超过 2147483647 字节")
			}
		}
	}
	return nil
}

// x_padding_bytes is a share-link alias for extra.xPaddingBytes, not a
// separate transport option. Preserve every other extra setting and reject
// conflicting values instead of allowing JSON decoder precedence to decide.
func normalizeXHTTPExtra(q url.Values) (json.RawMessage, error) {
	raw := q.Get("extra")
	object := make(map[string]json.RawMessage)
	if raw != "" {
		if err := validateUpdateJSON([]byte(raw)); err != nil {
			return nil, fmt.Errorf("XHTTP extra JSON 无效或包含重复字段")
		}
		if err := json.Unmarshal([]byte(raw), &object); err != nil || object == nil {
			return nil, fmt.Errorf("XHTTP extra 必须是 JSON 对象")
		}
	}
	var padding string
	seenPadding := false
	for key, value := range object {
		// Independent download streams bypass the outer node's destination,
		// transport and TLS validation. The pinned core also has unchecked
		// runtime assumptions here that its configuration test does not catch.
		// Reject every spelling accepted by encoding/json until this separate
		// stream can be validated in full, including explicit null values.
		if strings.EqualFold(key, "downloadSettings") {
			return nil, fmt.Errorf("XHTTP extra 暂不支持独立下载配置")
		}
		if strings.EqualFold(key, "xPaddingBytes") {
			// EqualFold also matches Unicode variants such as long s, which
			// strings.ToLower does not collapse to the same JSON member name.
			if seenPadding {
				return nil, fmt.Errorf("XHTTP extra xPaddingBytes 字段不能重复")
			}
			seenPadding = true
			var err error
			padding, err = normalizeXHTTPPaddingJSON(value)
			if err != nil {
				return nil, err
			}
			delete(object, key)
		}
	}
	if alias, exists := q["x_padding_bytes"]; exists {
		if len(alias) != 1 {
			return nil, fmt.Errorf("XHTTP x_padding_bytes 不能重复")
		}
		normalized, err := normalizeXHTTPPadding(alias[0])
		if err != nil {
			return nil, err
		}
		if padding != "" && padding != normalized {
			return nil, fmt.Errorf("XHTTP x_padding_bytes 与 extra.xPaddingBytes 冲突")
		}
		padding = normalized
	}
	if padding != "" {
		object["xPaddingBytes"], _ = json.Marshal(padding)
	}
	if raw == "" && len(object) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("XHTTP extra 无效")
	}
	return encoded, nil
}

func normalizeXHTTPPaddingJSON(raw json.RawMessage) (string, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		var value int32
		if err := json.Unmarshal(raw, &value); err != nil || string(raw) == "null" {
			return "", fmt.Errorf("XHTTP xPaddingBytes 必须是正整数或范围")
		}
		text = strconv.FormatInt(int64(value), 10)
	}
	return normalizeXHTTPPadding(text)
}

func normalizeXHTTPPadding(raw string) (string, error) {
	parts := strings.Split(raw, "-")
	if len(parts) < 1 || len(parts) > 2 {
		return "", fmt.Errorf("XHTTP xPaddingBytes 必须是正整数或范围")
	}
	bounds := make([]uint64, len(parts))
	for i, part := range parts {
		value, err := strconv.ParseUint(part, 10, 31)
		if err != nil || value == 0 {
			return "", fmt.Errorf("XHTTP xPaddingBytes 必须是正整数或范围")
		}
		// The core allocates padding buffers directly from this range.
		// Bound both endpoints before any subscription can reach it.
		if value > 64<<10 {
			return "", fmt.Errorf("XHTTP xPaddingBytes 不能超过 65536 字节")
		}
		bounds[i] = value
	}
	if len(bounds) == 1 || bounds[0] == bounds[1] {
		return strconv.FormatUint(bounds[0], 10), nil
	}
	// Xray normalizes reversed bounds too; compare their effective range.
	if bounds[0] > bounds[1] {
		bounds[0], bounds[1] = bounds[1], bounds[0]
	}
	return strconv.FormatUint(bounds[0], 10) + "-" + strconv.FormatUint(bounds[1], 10), nil
}
