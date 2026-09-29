package manager

import (
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The fixed core expands port ranges into a uint32 slice. Bound that allocation
// even when a hostile subscription contains many hopping nodes.
const maxHysteria2HopPorts = 16384

func applyHysteria2TransportOptions(q url.Values, stream map[string]any) error {
	mask, _ := stream["finalmask"].(map[string]any)
	if mask == nil {
		mask = make(map[string]any)
	}
	ports, err := hysteria2Option(q, normalizeHysteria2Ports, "mport", "ports")
	if err != nil {
		return fmt.Errorf("Hysteria2 跳端口参数无效：%w", err)
	}
	interval, err := hysteria2Option(q, normalizeHysteria2HopInterval, "hop-interval", "hopInterval")
	if err != nil {
		return fmt.Errorf("Hysteria2 跳端口间隔无效：%w", err)
	}
	if interval != "" && ports == "" {
		return fmt.Errorf("Hysteria2 跳端口间隔必须配合 mport/ports")
	}
	if ports != "" {
		if interval == "" {
			interval = "30"
		}
		seconds, _ := strconv.Atoi(interval)
		masks, _ := mask["udp"].([]any)
		// Xray reverses this list before wrapping sockets. UDP hopping must be
		// applied to the raw socket first, before Salamander, so it goes last here.
		mask["udp"] = append(masks, map[string]any{"type": "udphop", "settings": map[string]any{
			"mode": "intervalLocal,intervalRemote", "interval": seconds, "remotePorts": ports,
		}})
	}
	up, err := hysteria2BandwidthOption(q, "upmbps", "up")
	if err != nil {
		return fmt.Errorf("Hysteria2 上行带宽无效：%w", err)
	}
	down, err := hysteria2BandwidthOption(q, "downmbps", "down")
	if err != nil {
		return fmt.Errorf("Hysteria2 下行带宽无效：%w", err)
	}
	if up != "" || down != "" {
		quic := map[string]any{"congestion": "brutal"}
		if up != "" {
			quic["brutalUp"] = up + " bps"
		}
		if down != "" {
			quic["brutalDown"] = down + " bps"
		}
		mask["quicParams"] = quic
	}
	if len(mask) > 0 {
		stream["finalmask"] = mask
	}
	return nil
}

func hysteria2Option(q url.Values, normalize func(string) (string, error), keys ...string) (string, error) {
	var value string
	for _, key := range keys {
		entries, exists := q[key]
		if !exists {
			continue
		}
		if len(entries) != 1 {
			return "", fmt.Errorf("参数不能重复")
		}
		candidate, err := normalize(entries[0])
		if err != nil {
			return "", err
		}
		if value != "" && candidate != value {
			return "", fmt.Errorf("等价参数值冲突")
		}
		value = candidate
	}
	return value, nil
}

func normalizeHysteria2Ports(raw string) (string, error) {
	if raw == "" || len(raw) > 2048 {
		return "", fmt.Errorf("端口表为空或过长")
	}
	parts := strings.Split(raw, ",")
	if len(parts) > 128 {
		return "", fmt.Errorf("端口范围数量超限")
	}
	type portRange struct{ start, end int }
	ranges := make([]portRange, 0, len(parts))
	parse := func(s string) (int, error) {
		if s == "" || strings.Trim(s, "0123456789") != "" {
			return 0, fmt.Errorf("端口必须为十进制整数")
		}
		n, err := strconv.ParseUint(s, 10, 16)
		if err != nil || n == 0 {
			return 0, fmt.Errorf("端口必须在 1–65535 之间")
		}
		return int(n), nil
	}
	for _, part := range parts {
		bounds := strings.Split(part, "-")
		if len(bounds) > 2 {
			return "", fmt.Errorf("端口范围格式无效")
		}
		start, err := parse(bounds[0])
		if err != nil {
			return "", err
		}
		end := start
		if len(bounds) == 2 {
			end, err = parse(bounds[1])
			if err != nil {
				return "", err
			}
		}
		if start > end {
			return "", fmt.Errorf("端口范围起止顺序无效")
		}
		ranges = append(ranges, portRange{start, end})
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].start < ranges[j].start })
	merged := make([]portRange, 0, len(ranges))
	for _, item := range ranges {
		if len(merged) == 0 || item.start > merged[len(merged)-1].end+1 {
			merged = append(merged, item)
		} else if item.end > merged[len(merged)-1].end {
			merged[len(merged)-1].end = item.end
		}
	}
	count := 0
	out := make([]string, 0, len(merged))
	for _, item := range merged {
		count += item.end - item.start + 1
		if count > maxHysteria2HopPorts {
			return "", fmt.Errorf("跳端口数量不能超过 16384")
		}
		value := strconv.Itoa(item.start)
		if item.start != item.end {
			value += "-" + strconv.Itoa(item.end)
		}
		out = append(out, value)
	}
	return strings.Join(out, ","), nil
}

func normalizeHysteria2HopInterval(raw string) (string, error) {
	var duration time.Duration
	var err error
	if raw != "" && strings.Trim(raw, "0123456789") == "" {
		raw += "s"
	}
	duration, err = time.ParseDuration(raw)
	if err != nil || duration < 5*time.Second || duration > 24*time.Hour || duration%time.Second != 0 {
		return "", fmt.Errorf("跳端口间隔必须为 5–86400 的整数秒")
	}
	return strconv.FormatInt(int64(duration/time.Second), 10), nil
}

var hysteria2BandwidthPattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([a-z]*)$`)

func hysteria2BandwidthOption(q url.Values, mbpsKey, generalKey string) (string, error) {
	var value string
	for _, key := range []string{mbpsKey, generalKey} {
		entries, exists := q[key]
		if !exists {
			continue
		}
		if len(entries) != 1 {
			return "", fmt.Errorf("参数不能重复")
		}
		candidate, err := normalizeHysteria2Bandwidth(entries[0], key == mbpsKey)
		if err != nil {
			return "", err
		}
		if value != "" && value != candidate {
			return "", fmt.Errorf("等价参数值冲突")
		}
		value = candidate
	}
	return value, nil
}

func normalizeHysteria2Bandwidth(raw string, mbpsOnly bool) (string, error) {
	if len(raw) > 48 {
		return "", fmt.Errorf("带宽值过长")
	}
	match := hysteria2BandwidthPattern.FindStringSubmatch(strings.ToLower(strings.TrimSpace(raw)))
	if match == nil {
		return "", fmt.Errorf("带宽必须为非负十进制数")
	}
	unit := match[2]
	if mbpsOnly && unit != "" {
		return "", fmt.Errorf("upmbps/downmbps 必须使用 Mbps 数值")
	}
	multiplier := int64(1000000) // Bare up/down values follow Clash's Mbps convention.
	switch unit {
	case "", "m", "mbps":
	case "b", "bps":
		multiplier = 1
	case "k", "kbps":
		multiplier = 1000
	case "g", "gbps":
		multiplier = 1000000000
	case "t", "tbps":
		multiplier = 1000000000000
	default:
		return "", fmt.Errorf("带宽单位仅支持 bps/kbps/mbps/gbps/tbps")
	}
	value, ok := new(big.Rat).SetString(match[1])
	if !ok {
		return "", fmt.Errorf("带宽数值无效")
	}
	value.Mul(value, big.NewRat(multiplier, 1))
	if !value.IsInt() || !value.Num().IsUint64() {
		return "", fmt.Errorf("带宽必须能精确表示为整数 bps")
	}
	bps := value.Num().Uint64()
	// Xray requires at least 65536 bytes/s and uses signed arithmetic in its
	// congestion controller. Zero preserves automatic BBR negotiation.
	if bps != 0 && (bps < 524288 || bps > 1000000000000) {
		return "", fmt.Errorf("带宽必须为 0 或 524288–1000000000000 bps")
	}
	// Core's Mbps suffix is binary (1024^2), while share links use decimal Mbps.
	// Emit integer bits/s to preserve the requested rate exactly.
	return strconv.FormatUint(bps, 10), nil
}
