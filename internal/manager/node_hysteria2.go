package manager

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"
)

func parseHysteria2(raw string) (*parsedNode, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("Hysteria2 链接格式无效")
	}
	if u.User == nil || u.User.Username() == "" {
		return nil, fmt.Errorf("Hysteria2 认证参数无效：缺少认证信息")
	}
	if u.EscapedPath() != "" && u.EscapedPath() != "/" {
		return nil, fmt.Errorf("Hysteria2 链接不能包含非空 path")
	}
	if err := validateNodeHost(u.Hostname()); err != nil {
		return nil, fmt.Errorf("Hysteria2 服务器地址无效：%w", err)
	}
	port, err := parseNodePort(firstNonEmpty(u.Port(), "443"), "Hysteria2")
	if err != nil {
		return nil, err
	}
	q, err := parseStrictQuery(u.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("Hysteria2 查询参数无效：参数格式错误或重复")
	}
	if err := rejectUnknownQueryParams(q, "sni", "serverName", "servername", "peer", "insecure", "alpn", "obfs", "obfs-password", "mport", "ports", "hop-interval", "hopInterval", "upmbps", "downmbps", "up", "down"); err != nil {
		return nil, fmt.Errorf("Hysteria2 查询参数无效：%w", err)
	}
	if value, exists := q["insecure"]; exists && value[0] != "0" && value[0] != "false" {
		return nil, fmt.Errorf("Hysteria2 TLS 参数无效：不能跳过证书校验")
	}
	serverName, err := consistentQueryValue(q, "sni", "serverName", "servername", "peer")
	if err != nil {
		return nil, fmt.Errorf("Hysteria2 TLS 参数无效：serverName 参数冲突")
	}
	serverName = firstNonEmpty(serverName, u.Hostname())
	if err := validateNodeHost(serverName); err != nil {
		return nil, fmt.Errorf("Hysteria2 TLS 参数无效：serverName 无效")
	}
	if alpn, exists := q["alpn"]; exists && alpn[0] != "h3" {
		return nil, fmt.Errorf("Hysteria2 TLS 参数无效：ALPN 仅支持 h3")
	}
	auth := trojanPassword(u)
	if !validHysteria2Secret(auth) {
		return nil, fmt.Errorf("Hysteria2 认证参数无效")
	}
	stream := map[string]any{
		"network": "hysteria", "security": "tls",
		"tlsSettings":      map[string]any{"serverName": serverName, "alpn": []string{"h3"}},
		"hysteriaSettings": map[string]any{"version": 2, "auth": auth},
	}
	obfs, hasObfs := q["obfs"]
	password, hasPassword := q["obfs-password"]
	if hasObfs {
		if obfs[0] != "salamander" || !hasPassword || len(password[0]) < 4 || !validHysteria2Secret(password[0]) {
			return nil, fmt.Errorf("Hysteria2 混淆参数无效：仅支持带有效密码的 salamander")
		}
		stream["finalmask"] = map[string]any{"udp": []any{map[string]any{"type": "salamander", "settings": map[string]any{"password": password[0]}}}}
	} else if hasPassword {
		return nil, fmt.Errorf("Hysteria2 混淆参数无效：obfs-password 必须配合 salamander")
	}
	if err := applyHysteria2TransportOptions(q, stream); err != nil {
		return nil, err
	}
	return &parsedNode{
		Protocol: "hysteria2", Name: sanitizeNodeName(remark(raw, "hysteria2-"+u.Hostname())),
		EndpointHost: u.Hostname(), EndpointPort: port,
		Outbound: map[string]any{
			"tag": "", "protocol": "hysteria",
			"settings":       map[string]any{"version": 2, "address": u.Hostname(), "port": port},
			"streamSettings": stream,
		},
	}, nil
}

func validHysteria2Secret(value string) bool {
	return value != "" && len(value) <= 4096 && strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) < 0
}
