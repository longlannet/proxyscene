package manager

import (
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

const compatibilityVLESSBase = "vless://11111111-1111-1111-1111-111111111111@example.com:443?"

func TestSubscriptionServerNameAliases(t *testing.T) {
	for _, protocol := range []string{"vless", "trojan"} {
		base := compatibilityVLESSBase
		if protocol == "trojan" {
			base = "trojan://test-password@example.com:443?"
		}
		for _, security := range []string{"tls", "reality"} {
			baseQuery := url.Values{"security": {security}}
			if security == "reality" {
				baseQuery.Set("pbk", base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
			}
			for _, alias := range []string{"servername", "serverName", "sni"} {
				t.Run(protocol+"/"+security+"/"+alias, func(t *testing.T) {
					q := maps.Clone(baseQuery)
					q.Set(alias, "sni.example.com")
					pn, err := parseRuntimeNode(base + q.Encode())
					if err != nil {
						t.Fatal(err)
					}
					stream := pn.Outbound["streamSettings"].(map[string]any)
					settings := stream[security+"Settings"].(map[string]any)
					if settings["serverName"] != "sni.example.com" {
						t.Fatalf("serverName not preserved: %#v", settings)
					}
				})
			}
			q := maps.Clone(baseQuery)
			for _, key := range []string{"sni", "serverName", "servername"} {
				q.Set(key, "sni.example.com")
			}
			if protocol == "trojan" {
				q.Set("peer", "sni.example.com")
			}
			if _, err := parseRuntimeNode(base + q.Encode()); err != nil {
				t.Fatalf("equal aliases rejected: %v", err)
			}
			q.Set("servername", "conflict.example.com")
			if _, err := parseRuntimeNode(base + q.Encode()); err == nil {
				t.Fatal("conflicting lowercase alias accepted")
			}
		}
		for _, suffix := range []string{
			"security=none&servername=sni.example.com",
			"security=tls&servername=bad%20host",
			"security=tls&servername=one.example&servername=one.example",
			"security=tls&servername=one.example&serverName=two.example",
			"security=tls&SERVERNAME=sni.example.com",
		} {
			if _, err := parseRuntimeNode(base + suffix); err == nil {
				t.Fatalf("unsafe/unknown alias accepted: %s", suffix)
			}
		}
	}
}

func TestSubscriptionRawModeCompatibility(t *testing.T) {
	for _, network := range []string{"tcp", "raw"} {
		pn, err := parseRuntimeNode(compatibilityVLESSBase + "security=tls&type=" + network + "&mode=multi")
		if err != nil {
			t.Fatal(err)
		}
		stream := pn.Outbound["streamSettings"].(map[string]any)
		if !reflect.DeepEqual(stream["network"], "raw") || stream["grpcSettings"] != nil {
			t.Fatalf("RAW mode changed transport: %#v", stream)
		}
	}
	for _, suffix := range []string{"type=tcp&mode=gun", "type=raw&mode=stream-up", "type=ws&mode=multi", "type=tcp&mode=multi&mode=multi"} {
		if _, err := parseRuntimeNode(compatibilityVLESSBase + "security=tls&" + suffix); err == nil {
			t.Fatalf("unsupported RAW mode accepted: %s", suffix)
		}
	}
}

func TestSubscriptionVMessKnownPlaceholders(t *testing.T) {
	base := map[string]any{"v": "2", "add": "example.com", "port": "443", "id": "11111111-1111-1111-1111-111111111111", "net": "tcp", "type": "none"}
	for _, tls := range []string{"", "none"} {
		for _, network := range []string{"tcp", "raw", "ws", "httpupgrade"} {
			v := cloneCompatibilityVMess(base)
			v["net"], v["tls"], v["sni"] = network, tls, "sni.example.com"
			v["host"], v["path"] = "cdn.example.com", "/template"
			pn, err := parseRuntimeNode(vmessURL(t, v))
			if err != nil {
				t.Fatal(err)
			}
			stream := pn.Outbound["streamSettings"].(map[string]any)
			if stream["security"] != "none" || stream["tlsSettings"] != nil {
				t.Fatalf("redundant SNI enabled TLS: %#v", stream)
			}
			if network == "tcp" || network == "raw" {
				if stream["rawSettings"] != nil {
					t.Fatalf("type=none enabled HTTP headers: %#v", stream)
				}
			} else if stream[network+"Settings"].(map[string]any)["path"] != "/template" {
				t.Fatal("active transport path lost")
			}
		}
	}
	for name, mutate := range map[string]func(map[string]any){
		"bad sni":              func(v map[string]any) { v["sni"] = "bad\nhost" },
		"fingerprint":          func(v map[string]any) { v["fp"] = "chrome" },
		"alpn":                 func(v map[string]any) { v["alpn"] = "h2" },
		"bad host":             func(v map[string]any) { v["host"] = "bad host" },
		"bad path":             func(v map[string]any) { v["path"] = "/bad\npath" },
		"invalid escaped path": func(v map[string]any) { v["path"] = "/bad%zz" },
		"no explicit none":     func(v map[string]any) { delete(v, "type"); v["path"] = "/template" },
		"unsupported type":     func(v map[string]any) { v["type"] = "other"; v["path"] = "/template" },
	} {
		t.Run(name, func(t *testing.T) {
			v := cloneCompatibilityVMess(base)
			mutate(v)
			if _, err := parseRuntimeNode(vmessURL(t, v)); err == nil {
				t.Fatal("invalid or semantically active field was ignored")
			}
		})
	}
	v := cloneCompatibilityVMess(base)
	v["type"], v["path"] = "http", "/active"
	pn, err := parseRuntimeNode(vmessURL(t, v))
	if err != nil || pn.Outbound["streamSettings"].(map[string]any)["rawSettings"] == nil {
		t.Fatalf("real RAW HTTP configuration lost: %v", err)
	}
}

func cloneCompatibilityVMess(base map[string]any) map[string]any {
	v := make(map[string]any, len(base))
	for key, value := range base {
		v[key] = value
	}
	return v
}

func TestSubscriptionEarlyDataPathCompatibility(t *testing.T) {
	for _, network := range []string{"ws", "httpupgrade"} {
		for _, tc := range []struct{ path, ed, want string }{
			{"/transport?ed=2048&token=value", "", "/transport?ed=2048&token=value"},
			{"/transport?ed=2048&token=value", "2048", "/transport?ed=2048&token=value"},
			{"/transport?ed=02048", "2048", "/transport?ed=2048"},
			{"/transport?token=value", "2048", "/transport?ed=2048&token=value"},
		} {
			q := url.Values{"security": {"tls"}, "type": {network}, "path": {tc.path}}
			if tc.ed != "" {
				q.Set("ed", tc.ed)
			}
			pn, err := parseRuntimeNode(compatibilityVLESSBase + q.Encode())
			if err != nil {
				t.Fatal(err)
			}
			got := pn.Outbound["streamSettings"].(map[string]any)[network+"Settings"].(map[string]any)["path"]
			if got != tc.want {
				t.Fatalf("path=%q want %q", got, tc.want)
			}
		}
		for _, tc := range []struct{ path, ed string }{
			{"/transport?ed=2048", "4096"},
			{"/transport?ed=2048&ed=2048", ""},
			{"/transport?ed=", "2048"},
			{"/transport?ed=0", ""},
			{"/transport?ed=-1", ""},
			{"/transport?ed=1.5", ""},
			{"/transport?ed=2147483648", ""},
			{"/transport?ed=bad", "2048"},
			{"/transport?eh=anything", ""},
		} {
			q := url.Values{"security": {"tls"}, "type": {network}, "path": {tc.path}, "ed": {tc.ed}}
			if _, err := parseRuntimeNode(compatibilityVLESSBase + q.Encode()); err == nil {
				t.Fatalf("invalid early data accepted: %#v", tc)
			}
		}
	}
}

func TestSubscriptionXHTTPPaddingAliases(t *testing.T) {
	for _, tc := range []struct{ alias, extra, want string }{
		{"100-200", `{"xPaddingBytes":"100-200","scMaxBufferedPosts":2}`, "100-200"},
		{"200-100", `{"xPaddingBytes":"100-200"}`, "100-200"},
		{"100", `{"xPaddingBytes":100}`, "100"},
		{"100-100", `{"xPaddingBytes":"100"}`, "100"},
		{"100-200", "", "100-200"},
		{"", `{"xPaddingBytes":"100-200"}`, "100-200"},
	} {
		q := url.Values{"security": {"tls"}, "type": {"xhttp"}}
		if tc.alias != "" {
			q.Set("x_padding_bytes", tc.alias)
		}
		if tc.extra != "" {
			q.Set("extra", tc.extra)
		}
		pn, err := parseRuntimeNode(compatibilityVLESSBase + q.Encode())
		if err != nil {
			t.Fatal(err)
		}
		settings := pn.Outbound["streamSettings"].(map[string]any)["xhttpSettings"].(map[string]any)
		var extra map[string]any
		if err := json.Unmarshal(settings["extra"].(json.RawMessage), &extra); err != nil {
			t.Fatal(err)
		}
		if extra["xPaddingBytes"] != tc.want {
			t.Fatalf("padding changed: %#v", extra)
		}
		if strings.Contains(tc.extra, "scMaxBufferedPosts") && extra["scMaxBufferedPosts"] != float64(2) {
			t.Fatal("other extra setting lost")
		}
	}
	for _, tc := range []struct{ alias, extra string }{
		{"100-200", `{"xPaddingBytes":"100-300"}`},
		{"100", `{"xPaddingBytes":null}`},
		{"0", ""}, {"-1", ""}, {"1.5", ""}, {"2147483648", ""},
		{"100", `{"xPaddingBytes":"100","xPaddingBytes":"200"}`},
		{"100", `{"xPaddingBytes":"100","XPADDINGBYTES":"200"}`},
		{"100", `[]`},
	} {
		q := url.Values{"security": {"tls"}, "type": {"xhttp"}, "x_padding_bytes": {tc.alias}}
		if tc.extra != "" {
			q.Set("extra", tc.extra)
		}
		if _, err := parseRuntimeNode(compatibilityVLESSBase + q.Encode()); err == nil {
			t.Fatalf("invalid padding accepted: %#v", tc)
		}
	}
	for _, suffix := range []string{"type=ws&x_padding_bytes=100", "type=xhttp&x_padding_bytes=100&x_padding_bytes=100"} {
		if _, err := parseRuntimeNode(compatibilityVLESSBase + "security=tls&" + suffix); err == nil {
			t.Fatalf("invalid padding context accepted: %s", suffix)
		}
	}
}

func TestSubscriptionSSTransportAliases(t *testing.T) {
	const base = "ss://aes-256-gcm:test-password@example.com:8388?"
	for _, network := range []string{"tcp", "raw"} {
		pn, err := parseRuntimeNode(base + "type=" + network)
		if err != nil {
			t.Fatal(err)
		}
		if pn.Outbound["streamSettings"] != nil {
			t.Fatal("SS RAW alias added transport settings")
		}
	}
	for _, suffix := range []string{"type=ws", "type=tcp&type=raw", "type=tcp&path=%2Fws", "type=tcp&plugin=obfs"} {
		if _, err := parseRuntimeNode(base + suffix); err == nil {
			t.Fatalf("unsupported SS transport accepted: %s", suffix)
		}
	}
}

func TestSubscriptionXHTTPRejectsIndependentDownloadSettings(t *testing.T) {
	for name, extra := range map[string]string{
		"missing destination":    `{"downloadSettings":{}}`,
		"incompatible transport": `{"downloadSettings":{"address":"example.com","port":443,"network":"raw","security":"tls"}}`,
		"plaintext download":     `{"downloadSettings":{"address":"example.com","port":80,"network":"xhttp","security":"none"}}`,
		"case alias":             `{"DOWNLOADSETTINGS":{"address":"example.com","port":80,"network":"xhttp","security":"none"}}`,
		"explicit null":          `{"downloadSettings":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			q := url.Values{"security": {"tls"}, "type": {"xhttp"}, "extra": {extra}}
			_, err := parseRuntimeNode(compatibilityVLESSBase + q.Encode())
			if err == nil || !strings.Contains(err.Error(), "暂不支持独立下载配置") {
				t.Fatalf("independent download configuration not rejected explicitly: %v", err)
			}
		})
	}
}

func TestSubscriptionXHTTPRejectsUnicodePaddingAliases(t *testing.T) {
	for _, extra := range []string{
		`{"xPaddingBytes":"100","xPaddingByteſ":"200"}`,
		`{"xPaddingBytes":"100","xPaddingByteſ":"100"}`,
		`{"xPaddingByteſ":"200","xPaddingBytes":"100"}`,
	} {
		q := url.Values{"security": {"tls"}, "type": {"xhttp"}, "extra": {extra}}
		_, err := parseRuntimeNode(compatibilityVLESSBase + q.Encode())
		if err == nil || !strings.Contains(err.Error(), "字段不能重复") {
			t.Fatalf("Unicode-equivalent padding fields were not rejected explicitly: %v", err)
		}
	}
}

func TestSubscriptionXHTTPPaddingAllocationBound(t *testing.T) {
	for _, value := range []string{"65536", "1-65536", "65536-1"} {
		for _, field := range []string{"x_padding_bytes", "extra"} {
			q := url.Values{"security": {"tls"}, "type": {"xhttp"}}
			if field == "extra" {
				encoded, _ := json.Marshal(value)
				q.Set(field, `{"xPaddingBytes":`+string(encoded)+`}`)
			} else {
				q.Set(field, value)
			}
			if _, err := parseRuntimeNode(compatibilityVLESSBase + q.Encode()); err != nil {
				t.Fatalf("64 KiB padding boundary rejected (%s, %s): %v", field, value, err)
			}
		}
	}
	for _, value := range []string{"65537", "2147483647", "1-65537", "2147483647-1"} {
		for _, field := range []string{"x_padding_bytes", "extra"} {
			q := url.Values{"security": {"tls"}, "type": {"xhttp"}}
			if field == "extra" {
				encoded, _ := json.Marshal(value)
				q.Set(field, `{"xPaddingBytes":`+string(encoded)+`}`)
			} else {
				q.Set(field, value)
			}
			_, err := parseRuntimeNode(compatibilityVLESSBase + q.Encode())
			if err == nil || !strings.Contains(err.Error(), "不能超过 65536 字节") {
				t.Fatalf("oversize padding accepted (%s, %s): %v", field, value, err)
			}
		}
	}
	for _, tc := range []struct {
		number string
		valid  bool
	}{
		{"65536", true}, {"65537", false}, {"2147483647", false},
	} {
		q := url.Values{"security": {"tls"}, "type": {"xhttp"}, "extra": {`{"xPaddingBytes":` + tc.number + `}`}}
		_, err := parseRuntimeNode(compatibilityVLESSBase + q.Encode())
		if (err == nil) != tc.valid {
			t.Fatalf("numeric JSON padding bound mismatch (%s): %v", tc.number, err)
		}
	}
}

func TestSubscriptionLegacyTransportNodesRemainReadableAndRemovable(t *testing.T) {
	cases := map[string]string{
		"independent download":          compatibilityVLESSBase + "security=tls&type=xhttp&extra=" + url.QueryEscape(`{"downloadSettings":{}}`),
		"session allocation":            compatibilityVLESSBase + "security=tls&type=xhttp&extra=" + url.QueryEscape(`{"sessionIDTable":"hex","sessionIDLength":2147483647}`),
		"upload allocation":             compatibilityVLESSBase + "security=tls&type=xhttp&extra=" + url.QueryEscape(`{"scMaxEachPostBytes":2147483647}`),
		"xmux timer overflow":           compatibilityVLESSBase + "security=tls&type=xhttp&extra=" + url.QueryEscape(`{"xmux":{"hKeepAlivePeriod":9223372036854775807}}`),
		"unknown extra option":          compatibilityVLESSBase + "security=tls&type=xhttp&extra=" + url.QueryEscape(`{"unknownOption":true}`),
		"oversize padding":              compatibilityVLESSBase + "security=tls&type=xhttp&extra=" + url.QueryEscape(`{"xPaddingBytes":2147483647}`),
		"duplicate padding":             compatibilityVLESSBase + "security=tls&type=xhttp&extra=" + url.QueryEscape(`{"xPaddingBytes":"100","xPaddingByteſ":"200"}`),
		"uint32 early data ws":          compatibilityVLESSBase + "security=tls&type=ws&ed=4294967295",
		"uint32 early data httpupgrade": compatibilityVLESSBase + "security=tls&type=httpupgrade&ed=4294967295",
		"expanded legacy path":          compatibilityVLESSBase + "security=tls&type=ws&path=" + url.QueryEscape("/"+strings.Repeat("x", 4095)) + "&ed=2048",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			prepared, err := prepareStoredNode(raw)
			if err != nil {
				t.Fatalf("legacy syntax is no longer readable: %v", err)
			}
			if _, err := prepareNode(raw); err == nil {
				t.Fatal("unsafe legacy configuration admitted as a new node")
			}
			if _, err := parseRuntimeNode(raw); err == nil {
				t.Fatal("unsafe legacy configuration admitted at runtime")
			}
			a := testApp(t)
			st := newStore()
			st.Nodes = []Node{{ID: "legacy", Name: "legacy", Protocol: prepared.Parsed.Protocol, RawURL: raw}}
			st.DefaultNodeID = "legacy"
			st.SceneEnabled[SceneGlobal] = true
			if err := a.saveStore(st); err != nil {
				t.Fatalf("legacy state fixture rejected: %v", err)
			}
			loaded, err := a.loadStore()
			if err != nil || len(loaded.Nodes) != 1 || loaded.Nodes[0].RawURL != raw {
				t.Fatalf("legacy state is not loadable without rewriting its node: %v", err)
			}
			if _, err := a.renderXrayConfig(loaded); err == nil {
				t.Fatal("unsafe stored node reached generated runtime configuration")
			}
			// Exercise the same removal mutation used by the CLI/menu without
			// touching service lifecycle; persistence lives entirely in TempDir.
			if err := removeNodeFromStore(loaded, "legacy"); err != nil {
				t.Fatal(err)
			}
			if err := a.saveStore(loaded); err != nil {
				t.Fatalf("legacy node removal could not be saved: %v", err)
			}
			reloaded, err := a.loadStore()
			if err != nil || len(reloaded.Nodes) != 0 || reloaded.DefaultNodeID != "" || hasEnabledScene(reloaded) {
				t.Fatalf("legacy node deletion did not persist: %v", err)
			}
		})
	}
}
