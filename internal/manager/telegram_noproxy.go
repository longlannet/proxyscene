package manager

import (
	"net"
	"net/netip"
	"strconv"
	"strings"
)

func firstHermesTelegramNoProxyMatch(value string) (string, bool) {
	for _, entry := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || dotEnvSpace(r) }) {
		entry = strings.TrimSpace(entry)
		if entry != "" && hermesNoProxyEntryMatchesTelegram(entry) {
			return entry, true
		}
	}
	return "", false
}

func hermesNoProxyEntryMatchesTelegram(entry string) bool {
	token := strings.ToLower(strings.TrimSpace(entry))
	if token == "*" {
		return true
	}
	host, hasPort := noProxyTokenHost(token)
	if host == "" || hasPort {
		return false
	}
	if prefix, ok := parseHermesNoProxyPrefix(host); ok {
		return hermesNoProxyIPv4PrefixCanMatchFallback(prefix.Masked())
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return hermesFallbackIPv4Allowed(addr)
	}
	apiHost := "api.telegram.org"
	// Current Hermes treats both '*.domain' and '.domain' as apex plus
	// subdomains. This is also a conservative superset of the older matcher.
	host = strings.TrimPrefix(strings.TrimPrefix(host, "*"), ".")
	return apiHost == host || strings.HasSuffix(apiHost, "."+host)
}

func hermesFallbackIPv4Allowed(addr netip.Addr) bool {
	return addr.Is4() && !addr.IsPrivate() && !addr.IsLoopback() &&
		!addr.IsLinkLocalUnicast() && !addr.IsUnspecified()
}

func hermesNoProxyIPv4PrefixCanMatchFallback(prefix netip.Prefix) bool {
	if !prefix.Addr().Is4() {
		return false
	}
	// Hermes accepts arbitrary non-private IPv4 fallback addresses from config
	// and DoH. A prefix is safe only when every address is inside one of the
	// categories Hermes rejects. This deliberately treats other reserved ranges
	// conservatively because Python's ipaddress classification varies by release.
	for _, internal := range []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
	} {
		if internal.Bits() <= prefix.Bits() && internal.Contains(prefix.Addr()) {
			return false
		}
	}
	return !(prefix.Bits() == 32 && prefix.Addr().IsUnspecified())
}

// Python ip_network(strict=False) accepts dotted IPv4 netmasks and hostmasks
// as well as CIDR lengths. Missing these would let a public fallback range
// bypass the managed proxy even though the validator accepted NO_PROXY.
func parseHermesNoProxyPrefix(host string) (netip.Prefix, bool) {
	if prefix, err := netip.ParsePrefix(host); err == nil {
		return prefix.Masked(), true
	}
	address, maskText, ok := strings.Cut(host, "/")
	if !ok {
		return netip.Prefix{}, false
	}
	addr, addrErr := netip.ParseAddr(address)
	maskAddr, maskErr := netip.ParseAddr(maskText)
	if addrErr != nil || maskErr != nil || !addr.Is4() || !maskAddr.Is4() {
		return netip.Prefix{}, false
	}
	mask := maskAddr.As4()
	if mask[0] == 0 && mask != [4]byte{} {
		for i := range mask {
			mask[i] = ^mask[i]
		}
	}
	ones, size := net.IPMask(mask[:]).Size()
	if size != 32 {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(addr, ones).Masked(), true
}

func noProxyNumericPort(value string, urlPort bool) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	if !urlPort {
		return true
	}
	_, err := strconv.ParseUint(value, 10, 16)
	return err == nil
}

func noProxyTokenHost(token string) (string, bool) {
	if separator := strings.Index(token, "://"); separator >= 0 || strings.HasPrefix(token, "//") {
		authority := token[2:]
		if separator >= 0 {
			authority = token[separator+3:]
		}
		if end := strings.IndexAny(authority, "/?#"); end >= 0 {
			authority = authority[:end]
		}
		if at := strings.LastIndexByte(authority, '@'); at >= 0 {
			authority = authority[at+1:]
		}
		if strings.HasPrefix(authority, "[") {
			if end := strings.IndexByte(authority, ']'); end >= 0 {
				return strings.TrimRight(authority[1:end], "."), strings.HasPrefix(authority[end+1:], ":") && noProxyNumericPort(authority[end+2:], true)
			}
			return "", false
		}
		host, port, _ := strings.Cut(authority, ":")
		// urlsplit retains hostname when a URL port is malformed or out of range,
		// then reports port=None. Do not drop that hostname on a URL parse error.
		return strings.TrimRight(host, "."), noProxyNumericPort(port, true)
	}
	if strings.HasPrefix(token, "[") {
		end := strings.IndexByte(token, ']')
		if end < 0 {
			return strings.Trim(token, "[]"), false
		}
		rest := token[end+1:]
		return strings.TrimRight(token[1:end], "."), strings.HasPrefix(rest, ":") && noProxyNumericPort(rest[1:], false)
	}
	if strings.Count(token, ":") == 1 {
		host, port, _ := strings.Cut(token, ":")
		if noProxyNumericPort(port, false) {
			return strings.TrimRight(host, "."), true
		}
	}
	return strings.TrimRight(strings.Trim(token, "[]"), "."), false
}
