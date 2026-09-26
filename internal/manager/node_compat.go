package manager

import (
	"fmt"
	"net/netip"
	"strings"
)

func parseRuntimeNode(raw string) (*parsedNode, error) {
	pn, err := parseNode(raw)
	if err != nil {
		return nil, err
	}
	if err := validateNodeRuntimeCompatibility(pn); err != nil {
		return nil, err
	}
	return pn, nil
}

func validateNodeRuntimeCompatibility(pn *parsedNode) error {
	switch pn.Protocol {
	case "vless", "trojan":
		stream := pn.Outbound["streamSettings"].(map[string]any)
		if stream["security"] == "none" && !xrayAllowsPlaintextHost(pn.EndpointHost) {
			return fmt.Errorf("节点不兼容：Xray v26.9.9 不再支持公网明文 %s；请使用 TLS/REALITY 节点", pn.Protocol)
		}
	case "ss":
		server := pn.Outbound["settings"].(map[string]any)["servers"].([]any)[0].(map[string]any)
		if server["method"] == "none" || server["method"] == "plain" {
			return fmt.Errorf("节点不兼容：Xray v26.9.9 已移除 Shadowsocks none/plain；请使用受支持的 AEAD 加密节点")
		}
	}
	return nil
}

// Match the fixed Xray 52a412d common/geodata/consts.go exception set. Do not
// resolve arbitrary DNS names here: upstream also classifies the literal name.
var xrayPlaintextPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/3"),
	netip.MustParsePrefix("::/127"), netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"), netip.MustParsePrefix("ff00::/8"),
}

func xrayAllowsPlaintextHost(host string) bool {
	if addr, err := netip.ParseAddr(host); err == nil {
		addr = addr.Unmap()
		for _, prefix := range xrayPlaintextPrefixes {
			if prefix.Contains(addr) {
				return true
			}
		}
		return false
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, domain := range []string{"lan", "localdomain", "example", "invalid", "localhost", "test", "local", "home.arpa", "internal"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	// validateNodeHost has already checked the rest of the DNS label syntax.
	return len(host) > 0 && len(host) <= 63 && !strings.Contains(host, ".") && host[0] >= 'a' && host[0] <= 'z'
}
