package manager

import (
	"net"
	"reflect"
	"testing"
)

func TestHysteria2PreservesAuthTLSAndSalamander(t *testing.T) {
	raw := "hy2://user%3Aname:p%40ss%3Aword@example.com:8443/?sni=tls.example.com&servername=tls.example.com&obfs=salamander&obfs-password=obfs%2Bsecret&insecure=0&alpn=h3#sample"
	pn, err := parseRuntimeNode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if pn.Protocol != "hysteria2" || pn.Outbound["protocol"] != "hysteria" || pn.EndpointPort != 8443 {
		t.Fatal("protocol or endpoint changed")
	}
	stream := pn.Outbound["streamSettings"].(map[string]any)
	if stream["network"] != "hysteria" || stream["security"] != "tls" {
		t.Fatal("wrong transport/security")
	}
	if !reflect.DeepEqual(stream["tlsSettings"], map[string]any{"serverName": "tls.example.com", "alpn": []string{"h3"}}) {
		t.Fatal("TLS validation settings changed")
	}
	if !reflect.DeepEqual(stream["hysteriaSettings"], map[string]any{"version": 2, "auth": "user:name:p@ss:word"}) {
		t.Fatal("auth was changed")
	}
	wantMask := map[string]any{"udp": []any{map[string]any{"type": "salamander", "settings": map[string]any{"password": "obfs+secret"}}}}
	if !reflect.DeepEqual(stream["finalmask"], wantMask) {
		t.Fatal("salamander configuration changed")
	}
}

func TestHysteria2DefaultsAndIPv6(t *testing.T) {
	for _, raw := range []string{"hysteria2://secret@example.com", "hy2://secret@[2001:db8::1]/"} {
		pn, err := parseRuntimeNode(raw)
		if err != nil {
			t.Fatal(err)
		}
		if pn.EndpointPort != 443 {
			t.Fatal("default port must be 443")
		}
		stream := pn.Outbound["streamSettings"].(map[string]any)
		tls := stream["tlsSettings"].(map[string]any)
		if tls["serverName"] != pn.EndpointHost {
			t.Fatal("default SNI differs from endpoint")
		}
		if _, exists := stream["finalmask"]; exists {
			t.Fatal("unexpected obfuscation")
		}
	}
}

func TestHysteria2RejectsUnsafeOrUnrepresentableParameters(t *testing.T) {
	for name, suffix := range map[string]string{
		"skip tls": "?insecure=1", "skip tls true": "?insecure=true", "skipcert": "?skipcert=true",
		"unknown": "?unknown=100", "unknown empty": "?unknown=", "duplicate": "?sni=example.com&sni=example.com",
		"conflicting sni": "?sni=one.example&servername=other.example", "wrong alpn": "?alpn=h2", "empty alpn": "?alpn=",
		"missing obfs password": "?obfs=salamander", "short obfs password": "?obfs=salamander&obfs-password=abc",
		"orphan obfs password": "?obfs-password=secret", "other obfs": "?obfs=gecko&obfs-password=secret",
		"path": "/other", "auth query": "?auth=secret", "bad port hopping": "?mport=450-443",
		"certificate file": "?ca=%2Fetc%2Fpasswd", "pin": "?pinSHA256=abc", "fingerprint": "?fp=chrome",
		"obfs control": "?obfs=salamander&obfs-password=a%0D%0Abc",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseRuntimeNode("hy2://secret@example.com:443" + suffix); err == nil {
				t.Fatal("unsupported parameters accepted")
			}
		})
	}
	for _, raw := range []string{"hy2://@example.com", "hy2://example.com", "hy2://sec%0D%0Aret@example.com", "hy2://secret@example.com:0", "hy2://secret@example.com:65536"} {
		if _, err := parseRuntimeNode(raw); err == nil {
			t.Fatal("invalid URL accepted")
		}
	}
}

func TestHysteria2ConfigsPassPinnedXray(t *testing.T) {
	cases := map[string]string{
		"default":    "hysteria2://secret@example.com",
		"salamander": "hy2://user:secret@example.com:8443?sni=tls.example.com&obfs=salamander&obfs-password=synthetic-secret",
		"ipv6":       "hy2://secret@[2001:db8::1]:443?alpn=h3&insecure=false",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) { requirePinnedNodeConfig(t, raw) })
	}
}

// A live TCP service at the same address must never make a UDP-only node win
// auto-selection. This catches the previous TCP-only probe's false positive.
func TestHysteria2TCPListenerCannotWinAutoSelection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	a := testApp(t)
	node := Node{ID: "hy2-test", RawURL: "hy2://synthetic-secret@" + listener.Addr().String()}
	if err := a.testNode(node); err == nil {
		t.Fatal("plain TCP listener unexpectedly passed proxy validation")
	}
	results, err := a.runSpeedTests([]Node{node})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Success {
		t.Fatal("UDP node incorrectly marked as TCP-reachable")
	}
	if fastestNodeID(map[string]SpeedResult{node.ID: results[0]}) != "" {
		t.Fatal("UDP node incorrectly selected as fastest")
	}
}
