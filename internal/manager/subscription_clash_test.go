package manager

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const clashTestUUID = "11111111-1111-1111-1111-111111111111"

func clashTestCases() map[string]struct{ entry, uri string } {
	return map[string]struct{ entry, uri string }{
		"vless websocket": {
			`{name: '中文 name/#', type: vless, server: node.example, port: 443, uuid: ` + clashTestUUID + `, tls: true, servername: tls.example, network: ws, udp: true, ws-opts: {path: /ws, headers: {Host: cdn.example}, max-early-data: 2048, early-data-header-name: Sec-WebSocket-Protocol}}`,
			"vless://" + clashTestUUID + "@node.example:443?security=tls&sni=tls.example&type=ws&host=cdn.example&path=%2Fws&ed=2048#%E4%B8%AD%E6%96%87%20name%2F%23",
		},
		"vmess grpc": {
			`{name: vmess, type: vmess, server: node.example, port: '443', uuid: ` + clashTestUUID + `, alterId: 0, cipher: auto, tls: true, servername: tls.example, network: grpc, grpc-opts: {grpc-service-name: svc, grpc-authority: cdn.example}}`,
			"",
		},
		"trojan grpc": {
			`{name: trojan, type: trojan, server: '2001:db8::1', port: 443, password: 'p@ss:word+%', sni: tls.example, alpn: [h2], network: grpc, grpc-opts: {grpc-service-name: svc}}`,
			"trojan://p%40ss%3Aword+%25@[2001:db8::1]:443?security=tls&sni=tls.example&alpn=h2&type=grpc&serviceName=svc#trojan",
		},
		"ss": {
			`{name: ss, type: ss, server: node.example, port: 8388, cipher: aes-256-gcm, password: 'p@ss:word+%', udp: true}`,
			"ss://aes-256-gcm:p%40ss%3Aword+%25@node.example:8388#ss",
		},
		"ss2022": {
			`{name: ss2022, type: ss, server: node.example, port: 8388, cipher: 2022-blake3-aes-128-gcm, password: AAAAAAAAAAAAAAAAAAAAAA==}`,
			"ss://2022-blake3-aes-128-gcm:AAAAAAAAAAAAAAAAAAAAAA==@node.example:8388#ss2022",
		},
		"hysteria2": {
			`{name: hy2, type: hysteria2, server: node.example, port: 443, password: 'p@ss:word+%', sni: tls.example, alpn: [h3], obfs: salamander, obfs-password: secret}`,
			"hysteria2://p%40ss%3Aword+%25@node.example:443?sni=tls.example&alpn=h3&obfs=salamander&obfs-password=secret#hy2",
		},
		"hysteria2 hopping bandwidth": {
			`{name: hy2-advanced, type: hysteria2, server: node.example, port: 443, password: secret, up: 1.5, down: '50 Mbps', ports: '8443,8444-8445', hop-interval: 15, obfs: salamander, obfs-password: secret}`,
			"hysteria2://secret@node.example:443?up=1.5&down=50%20Mbps&mport=8443,8444-8445&hop-interval=15&obfs=salamander&obfs-password=secret#hy2-advanced",
		},
		"reality": {
			`{name: reality, type: vless, server: node.example, port: 443, uuid: ` + clashTestUUID + `, tls: true, servername: tls.example, client-fingerprint: firefox, flow: xtls-rprx-vision, reality-opts: {public-key: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA, short-id: abcd}}`,
			"vless://" + clashTestUUID + "@node.example:443?security=reality&sni=tls.example&fp=firefox&flow=xtls-rprx-vision&pbk=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA&sid=abcd#reality",
		},
		"xhttp": {
			`{name: xhttp, type: vless, server: node.example, port: 443, uuid: ` + clashTestUUID + `, tls: true, network: xhttp, xhttp-opts: {host: cdn.example, path: /x, mode: packet-up, extra: {xPaddingBytes: '100-200'}}}`,
			"vless://" + clashTestUUID + "@node.example:443?security=tls&type=xhttp&host=cdn.example&path=%2Fx&mode=packet-up&extra=%7B%22xPaddingBytes%22%3A%22100-200%22%7D#xhttp",
		},
		"Mihomo ws upgrade": {
			`{name: upgrade, type: vless, server: node.example, port: 443, uuid: ` + clashTestUUID + `, tls: true, network: ws, ws-opts: {path: /up, headers: {Host: cdn.example}, v2ray-http-upgrade: true}}`,
			"vless://" + clashTestUUID + "@node.example:443?security=tls&type=httpupgrade&path=%2Fup&host=cdn.example#upgrade",
		},
		"Mihomo WS early data path": {
			`{name: early, type: vless, server: node.example, port: 443, uuid: ` + clashTestUUID + `, tls: true, network: ws, ws-opts: {path: '/ws?ed=2048', max-early-data: 2048}}`,
			"vless://" + clashTestUUID + "@node.example:443?security=tls&type=ws&path=%2Fws%3Fed%3D2048#early",
		},
		"httpupgrade": {
			`{name: upgrade, type: vless, server: node.example, port: 443, uuid: ` + clashTestUUID + `, tls: true, network: httpupgrade, http-upgrade-opts: {path: /up, host: cdn.example}}`,
			"vless://" + clashTestUUID + "@node.example:443?security=tls&type=httpupgrade&path=%2Fup&host=cdn.example#upgrade",
		},
	}
}

func TestClashSubscriptionPreservesNodeSemantics(t *testing.T) {
	cases := clashTestCases()
	vmess := cases["vmess grpc"]
	vmess.uri = vmessURL(t, map[string]any{"v": "2", "ps": "vmess", "add": "node.example", "port": 443, "id": clashTestUUID, "aid": 0, "scy": "auto", "tls": "tls", "sni": "tls.example", "net": "grpc", "serviceName": "svc", "authority": "cdn.example"})
	cases["vmess grpc"] = vmess
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			prepared, err := prepareSubscriptionBody("https://subscription.example", []byte("proxies:\n - "+tc.entry+"\n"))
			if err != nil || len(prepared.Nodes) != 1 || prepared.Invalid != 0 || prepared.Incomplete {
				t.Fatalf("node import failed: accepted=%d invalid=%d err=%v", len(prepared.Nodes), prepared.Invalid, err)
			}
			if err := validateSubscriptionRefresh(prepared); err != nil {
				t.Fatal(err)
			}
			expected, err := prepareNode(tc.uri)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(prepared.Nodes[0].Parsed, expected.Parsed) {
				actual, _ := json.Marshal(prepared.Nodes[0].Parsed)
				want, _ := json.Marshal(expected.Parsed)
				t.Fatalf("mapping changed semantics\nactual=%s\nexpected=%s", actual, want)
			}
			if !strings.Contains(prepared.diagnosticSummary(), "仅导入 YAML proxies") {
				t.Fatal("YAML scope notice missing")
			}
		})
	}
}

func TestClashSubscriptionIgnoresNonNodeConfiguration(t *testing.T) {
	body := `# A complete Mihomo document, including provider URLs and scripts.
mixed-port: 7890
allow-lan: true
proxies:
 - {name: only-node, type: trojan, server: node.example, port: 443, password: secret}
proxy-providers:
 remote: {type: http, url: 'trojan://must-not-import@provider.example:443', path: /tmp/must-not-write}
proxy-groups:
 - {name: select, type: select, proxies: [only-node], use: [remote]}
rules: ['MATCH,select']
script: {code: 'trojan://must-not-run@script.example:443'}
`
	for _, text := range []string{body, base64.StdEncoding.EncodeToString([]byte(body)), "\ufeff" + body} {
		prepared, err := prepareSubscriptionBody("https://subscription.example", []byte(text))
		if err != nil || len(prepared.Nodes) != 1 || prepared.Nodes[0].Parsed.Name != "only-node" || prepared.Incomplete {
			t.Fatalf("non-node configuration changed import: count=%d err=%v", len(prepared.Nodes), err)
		}
		if err := validateSubscriptionRefresh(prepared); err != nil {
			t.Fatal(err)
		}
	}
}

func TestClashSubscriptionStableCanonicalIdentity(t *testing.T) {
	first := "proxies:\n - {name: same, type: trojan, server: node.example, port: 443, password: 'p@ss:word+%', sni: tls.example}\n"
	second := "{\"proxies\":[{\"sni\":\"tls.example\",\"password\":\"p@ss:word+%\",\"port\":\"443\",\"server\":\"node.example\",\"type\":\"trojan\",\"name\":\"same\"}]}"
	a := prepareUpdateBody(t, "https://subscription.example", first)
	b := prepareUpdateBody(t, "https://subscription.example", second)
	if a.Nodes[0].RawURL != b.Nodes[0].RawURL {
		t.Fatal("YAML formatting changed node identity")
	}
}

func TestClashSubscriptionRejectsMeaningfulUnsupportedSettings(t *testing.T) {
	base := `name: valid, type: trojan, server: node.example, port: 443, password: HIDDEN-PASSWORD`
	for name, extra := range map[string]string{
		"unknown":                       `HIDDEN-QUERY-FIELD: HIDDEN-TOKEN`,
		"certificate":                   `skip-cert-verify: true`,
		"certificate type":              `skip-cert-verify: 'false'`,
		"udp disabled":                  `udp: false`,
		"TCP fast open":                 `tfo: true`,
		"multipath":                     `mptcp: true`,
		"dialer proxy":                  `dialer-proxy: another-node`,
		"client cert":                   `client-certificate: /secret/local/file`,
		"SNI conflict":                  `sni: a.example, servername: b.example`,
		"transport mismatch":            `ws-opts: {path: /ws}`,
		"unknown header":                `network: ws, ws-opts: {headers: {Authorization: HIDDEN-TOKEN}}`,
		"header conflict":               `network: ws, ws-opts: {host: one.example, headers: {Host: two.example}}`,
		"header empty duplicate":        `network: ws, ws-opts: {headers: {Host: '', host: one.example}}`,
		"header duplicate":              `network: ws, ws-opts: {headers: {Host: one.example, host: one.example}}`,
		"ws early data implicit header": `network: ws, ws-opts: {max-early-data: 2048}`,
		"upgrade fast open":             `network: ws, ws-opts: {v2ray-http-upgrade: true, v2ray-http-upgrade-fast-open: true}`,
		"upgrade early data size":       `network: httpupgrade, http-upgrade-opts: {max-early-data: 2048}`,
		"upgrade early data header":     `network: httpupgrade, http-upgrade-opts: {max-early-data: 2048, early-data-header-name: Sec-WebSocket-Protocol}`,
		"early data custom header":      `network: ws, ws-opts: {max-early-data: 2048, early-data-header-name: X-Other}`,
		"grpc advanced":                 `network: grpc, grpc-opts: {grpc-service-name: svc, grpc-multi-mode: true}`,
		"xhttp download":                `network: xhttp, xhttp-opts: {extra: {downloadSettings: null}}`,
		"xhttp session allocation":      `network: xhttp, xhttp-opts: {extra: {sessionIDTable: hex, sessionIDLength: 2147483647}}`,
		"xhttp padding":                 `network: xhttp, xhttp-opts: {extra: {xPaddingBytes: 100000}}`,
		"unrecognized network":          `network: quic`,
		"alpn wrong type":               `alpn: h2`,
		"alpn combined":                 `alpn: ['h2,http/1.1']`,
	} {
		t.Run(name, func(t *testing.T) {
			prepared, err := prepareSubscriptionBody("https://subscription.example/HIDDEN-PATH", []byte("proxies:\n - {"+base+", "+extra+"}\n"))
			if err == nil || prepared.Invalid != 1 || len(prepared.Nodes) != 0 {
				t.Fatalf("unsupported setting accepted: invalid=%d err=%v", prepared.Invalid, err)
			}
			assertSubscriptionDiagnosticPrivate(t, err.Error())
		})
	}
}

func TestClashSubscriptionRejectsUnsafeDocumentStructures(t *testing.T) {
	valid := `{name: valid, type: trojan, server: node.example, port: 443, password: HIDDEN-PASSWORD}`
	for name, body := range map[string]string{
		"indented script root":    "# comment\n  script:\n    code: 'trojan://HIDDEN-PASSWORD@hidden.example:443'\n",
		"explicit script key":     "? script\n: 'trojan://HIDDEN-PASSWORD@hidden.example:443'\n",
		"sequence of links":       "- 'trojan://HIDDEN-PASSWORD@hidden.example:443'\n",
		"commented flow mapping":  "# comment\n{script: 'trojan://HIDDEN-PASSWORD@hidden.example:443'}",
		"scalar literal":          "|\n trojan://HIDDEN-PASSWORD@hidden.example:443\n",
		"tagged scalar":           "!tag 'trojan://HIDDEN-PASSWORD@hidden.example:443'",
		"missing proxies":         "proxy-providers: {remote: {type: http, url: 'trojan://HIDDEN-PASSWORD@hidden.example:443'}}",
		"null proxies":            "proxies:",
		"wrong proxies type":      "proxies: {}",
		"empty proxies":           "proxies: []",
		"duplicate proxies":       "proxies: []\nproxies: [" + valid + "]",
		"duplicate node field":    "proxies: [{name: one, name: two, type: trojan, server: node.example, port: 443, password: HIDDEN-PASSWORD}]",
		"duplicate ignored field": "proxies: [" + valid + "]\nrules: []\nrules: []",
		"alias":                   "template: &template " + valid + "\nproxies: [*template]",
		"cyclic alias":            "template: &template [*template]\nproxies: [" + valid + "]",
		"alias explosion":         "a: &a [hello, world]\nb: &b [*a, *a, *a, *a]\nc: &c [*b, *b, *b, *b]\nproxies: [" + valid + "]",
		"merge":                   "template: &template " + valid + "\nproxies: [{<<: *template}]",
		"custom tag":              "proxies: [!secret " + valid + "]",
		"binary tag":              "proxies: [" + valid + "]\nignored: !!binary SGVsbG8=",
		"complex key":             "proxies: [" + valid + "]\n? [complex,key]\n: value",
		"multiple docs":           "proxies: [" + valid + "]\n---\nproxies: []",
		"trailing empty doc":      "proxies: [" + valid + "]\n---\n",
		"trailing malformed doc":  "proxies: [" + valid + "]\n---\n[",
		"truncated node list":     "proxies: [" + valid + ",",
		"deep ignored structure":  "proxies: [" + valid + "]\nignored: " + strings.Repeat("[", 70) + "value" + strings.Repeat("]", 70),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := prepareSubscriptionBody("https://subscription.example", []byte(body))
			if err == nil {
				t.Fatal("unsafe/incomplete YAML accepted")
			}
			assertSubscriptionDiagnosticPrivate(t, err.Error())
		})
	}
}

func TestClashSubscriptionLimitsAndDeadline(t *testing.T) {
	entry := " - {name: valid, type: trojan, server: node.example, port: 443, password: secret}\n"
	prepared, err := prepareSubscriptionBody("https://subscription.example", []byte("proxies:\n"+strings.Repeat(entry, maxSubscriptionNodes)))
	if err != nil || len(prepared.Nodes) != maxSubscriptionNodes {
		t.Fatalf("boundary rejected: count=%d err=%v", len(prepared.Nodes), err)
	}
	_, err = prepareSubscriptionBody("https://subscription.example", []byte("proxies:\n"+strings.Repeat(entry, maxSubscriptionNodes+1)))
	if err == nil || !strings.Contains(err.Error(), "节点数超过上限") {
		t.Fatalf("node cap not enforced: %v", err)
	}
	_, err = prepareSubscriptionBody("https://subscription.example", []byte("proxies: []\n#"+strings.Repeat("x", int(maxSubscriptionBytes))))
	if err == nil || !strings.Contains(err.Error(), "订阅内容过大") {
		t.Fatalf("body cap not enforced: %v", err)
	}
	_, err = prepareClashSubscription("https://subscription.example", "proxies:\n"+entry, time.Now().Add(-time.Second))
	if err == nil {
		t.Fatal("expired processing deadline accepted")
	}
	_, err = prepareSubscriptionBody("https://subscription.example", []byte("proxies: []\nignored: ["+strings.Repeat("x,", 131072)+"x]"))
	if err == nil || !strings.Contains(err.Error(), "结构超过上限") {
		t.Fatalf("AST cap not enforced: %v", err)
	}
}

func TestClashSubscriptionPartialImportRejectsRefresh(t *testing.T) {
	body := `proxies:
 - {name: valid, type: trojan, server: node.example, port: 443, password: secret}
 - {name: HIDDEN-NAME, type: tuic, server: hidden.example, password: HIDDEN-PASSWORD}
 - {name: HIDDEN-NAME, type: trojan, server: hidden.example, port: 443, password: HIDDEN-PASSWORD, HIDDEN-QUERY-FIELD: HIDDEN-TOKEN}
 - truncated
`
	prepared, err := prepareSubscriptionBody("https://subscription.example", []byte(body))
	if err != nil || len(prepared.Nodes) != 1 || prepared.Invalid != 3 || prepared.Unsupported["TUIC"] != 1 {
		t.Fatalf("wrong partial import: accepted=%d invalid=%d err=%v", len(prepared.Nodes), prepared.Invalid, err)
	}
	if err := validateSubscriptionRefresh(prepared); err == nil {
		t.Fatal("partial YAML refresh accepted")
	}
	assertSubscriptionDiagnosticPrivate(t, prepared.diagnosticSummary())
	if !strings.Contains(prepared.diagnosticSummary(), "第 3 条（Trojan）：参数不受支持") {
		t.Fatal("unsupported YAML parameter diagnostic lost")
	}
}

func TestClashGeneratedConfigsPassPinnedXray(t *testing.T) {
	binary := os.Getenv("PROXYSCENE_TEST_XRAY")
	if binary == "" {
		t.Skip("set PROXYSCENE_TEST_XRAY for official core checks")
	}
	for name, tc := range clashTestCases() {
		t.Run(name, func(t *testing.T) {
			prepared := prepareUpdateBody(t, "https://subscription.example", "proxies:\n - "+tc.entry+"\n")
			a := testApp(t)
			st := newStore()
			if _, err := a.addPreparedNodeIndexed(st, prepared.Nodes[0], "", "default", nil); err != nil {
				t.Fatal(err)
			}
			st.SceneEnabled[SceneGlobal] = true
			config := filepath.Join(a.cfg.CoreDir, "clash.json")
			if err := a.writeXrayConfigTo(st, config); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			output, err := exec.CommandContext(ctx, binary, "run", "-test", "-format", "json", "-config", config).CombinedOutput()
			if err != nil {
				t.Fatalf("official core rejected YAML-derived config: %v\n%s", err, output)
			}
		})
	}
}

func FuzzClashSubscriptionBody(f *testing.F) {
	for _, body := range []string{"proxies: []", "proxies: [{name: n, type: trojan, server: node.example, port: 443, password: secret}]", "proxies: [&cycle [*cycle]]", "proxies: [{name: a, name: b}]"} {
		f.Add(body)
	}
	f.Fuzz(func(t *testing.T, body string) {
		if len(body) > 64<<10 {
			t.Skip()
		}
		prepared, err := prepareSubscriptionBody("https://subscription.example", []byte(body))
		if err == nil {
			if len(prepared.Nodes) == 0 || len(prepared.Nodes) > maxSubscriptionNodes {
				t.Fatal("successful import has invalid node count")
			}
			for _, node := range prepared.Nodes {
				if _, err := prepareNode(node.RawURL); err != nil {
					t.Fatalf("saved canonical node cannot be read: %v", err)
				}
			}
		}
	})
}

func TestClashSubscriptionRedactsUnknownProtocols(t *testing.T) {
	prepared, err := prepareSubscriptionBody("https://subscription.example", []byte(`proxies: [{name: HIDDEN-NAME, type: HIDDEN-SCHEME, server: hidden.example, password: HIDDEN-PASSWORD}]`))
	if err == nil || prepared.Invalid != 1 || prepared.Unsupported["其他"] != 1 {
		t.Fatalf("unknown YAML protocol not counted: %v", err)
	}
	assertSubscriptionDiagnosticPrivate(t, err.Error())
	for i := 0; i < 10; i++ {
		prepared.recordDiagnostic(i+2, "其他", "参数不受支持")
	}
	if len(prepared.Diagnostics) != maxSubscriptionDiagnostics {
		t.Fatalf("unbounded YAML diagnostics: %d", len(prepared.Diagnostics))
	}
}

func TestClashNumericScalarsPreserveYAMLSemantics(t *testing.T) {
	for name, rawPort := range map[string]string{"hexadecimal": "0x1bb", "octal": "0673", "explicit octal": "0o673", "decimal underscores": "4_43"} {
		t.Run(name, func(t *testing.T) {
			body := "proxies: [{name: numeric, type: hysteria2, server: node.example, port: " + rawPort + ", password: secret, up: 010}]"
			prepared := prepareUpdateBody(t, "https://subscription.example", body)
			expected, err := prepareNode("hysteria2://secret@node.example:443?up=8#numeric")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(prepared.Nodes[0].Parsed, expected.Parsed) {
				t.Fatal("YAML integer changed meaning during conversion")
			}
		})
	}
}

func TestClashSubscriptionRejectsExplicitEmptyNumericOptions(t *testing.T) {
	for _, option := range []string{"up", "down", "ports", "hop-interval"} {
		t.Run(option, func(t *testing.T) {
			body := "proxies: [{name: hy2, type: hysteria2, server: node.example, port: 443, password: secret, " + option + ": ''}]"
			prepared, err := prepareSubscriptionBody("https://subscription.example", []byte(body))
			if err == nil || prepared.Invalid != 1 {
				t.Fatalf("empty numeric option disappeared: invalid=%d err=%v", prepared.Invalid, err)
			}
		})
	}
}
