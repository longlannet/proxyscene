package manager

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func xhttpLimitedTestURL(extra string) string {
	return compatibilityVLESSBase + "security=tls&type=xhttp&extra=" + url.QueryEscape(extra)
}

func TestXHTTPResourceLimitsRejectUnsafeCoreInputs(t *testing.T) {
	for name, extra := range map[string]string{
		"session allocation":         `{"sessionIDTable":"hex","sessionIDLength":2147483647}`,
		"session loop":               `{"sessionIDTable":"hex","sessionIDLength":"1-2147483647"}`,
		"session range overflow":     `{"sessionIDTable":"hex","sessionIDLength":"4294967297-4294967297"}`,
		"session negative":           `{"sessionIDTable":"hex","sessionIDLength":-1}`,
		"session table only":         `{"sessionIDTable":"hex"}`,
		"session length only":        `{"sessionIDLength":16}`,
		"session insufficient space": `{"sessionIDTable":"hex","sessionIDLength":4}`,
		"session null":               `{"sessionIDLength":null}`,
		"session nonascii":           `{"sessionIDTable":"中文","sessionIDLength":16}`,
		"session table huge":         `{"sessionIDTable":"` + strings.Repeat("a", 257) + `","sessionIDLength":16}`,
		"post allocation":            `{"scMaxEachPostBytes":2147483647}`,
		"post zero lower panic":      `{"scMaxEachPostBytes":"0-1000000"}`,
		"post negative":              `{"scMaxEachPostBytes":-1}`,
		"post int overflow":          `{"scMaxEachPostBytes":"4294967295"}`,
		"uncancellable sleep":        `{"scMinPostsIntervalMs":2147483647}`,
		"upload chunks":              `{"uplinkChunkSize":2147483647}`,
		"tiny upload chunks":         `{"uplinkChunkSize":1}`,
		"queue allocation":           `{"scMaxBufferedPosts":9223372036854775807}`,
		"queue negative":             `{"scMaxBufferedPosts":-1}`,
		"queue float":                `{"scMaxBufferedPosts":1.5}`,
		"queue string":               `{"scMaxBufferedPosts":"30"}`,
		"server busyloop":            `{"scStreamUpServerSecs":"0-1"}`,
		"header allocation":          `{"serverMaxHeaderBytes":2147483647}`,
		"mux connection storm":       `{"xmux":{"maxConnections":2147483647}}`,
		"mux overflow":               `{"xmux":{"hKeepAlivePeriod":9223372036854775807}}`,
		"mux negative":               `{"xmux":{"maxConcurrency":-1}}`,
		"mux conflict":               `{"xmux":{"maxConnections":2,"maxConcurrency":2}}`,
		"mux unknown":                `{"xmux":{"unknown":true}}`,
		"mux alias conflict":         `{"xmux":{"maxConnections":2,"maxConnectionſ":3}}`,
		"unknown ignored":            `{"unknown":true}`,
		"outer path overwritten":     `{"path":"/ignored"}`,
		"nested extra ignored":       `{"extra":{"sessionIDLength":2147483647}}`,
		"unicode duplicate":          `{"sessionIDTable":"hex","sessionIDLength":16,"ſeſſionIDLength":2147483647}`,
		"type invalid":               `{"noGRPCHeader":"true"}`,
		"type null":                  `{"noGRPCHeader":null}`,
		"header control":             `{"headers":{"X-Test":"a\r\nb"}}`,
		"header null":                `{"headers":{"X-Test":null}}`,
		"header duplicate":           `{"headers":{"X-Test":"a","x-test":"b"}}`,
		"header huge":                `{"headers":{"X-Test":"` + strings.Repeat("a", 4097) + `"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			raw := xhttpLimitedTestURL(extra)
			if _, err := parseNode(raw); err != nil {
				t.Fatalf("syntax-only legacy parsing rejected: %v", err)
			}
			if _, err := parseRuntimeNode(raw); err == nil {
				t.Fatal("unsafe XHTTP extra reached runtime admission")
			}
		})
	}
}

func TestXHTTPResourceLimitsPreserveValidCoreSettings(t *testing.T) {
	for name, extra := range map[string]string{
		"defaults":                  `{"sessionIDLength":0,"scMaxEachPostBytes":0,"scMinPostsIntervalMs":0,"scStreamUpServerSecs":0,"uplinkChunkSize":0,"scMaxBufferedPosts":0,"serverMaxHeaderBytes":0}`,
		"bounded session":           `{"sessionIDTable":"hex","sessionIDLength":"16-64"}`,
		"unicode alias":             `{"ſeſſionIDTable":"hex","ſeſſionIDLength":16}`,
		"bounded upload":            `{"scMaxEachPostBytes":"1000000-4194304","scMinPostsIntervalMs":"1-60000","uplinkChunkSize":"64-4194304"}`,
		"bounded server":            `{"scStreamUpServerSecs":"20-3600","scMaxBufferedPosts":128,"serverMaxHeaderBytes":65536}`,
		"mux":                       `{"xmux":{"maxConnections":64,"cMaxReuseTimes":1000000,"hMaxRequestTimes":1000000,"hMaxReusableSecs":86400,"hKeepAlivePeriod":3600}}`,
		"mux concurrency":           `{"xmux":{"maxConcurrency":1024,"hKeepAlivePeriod":-1}}`,
		"headers booleans":          `{"headers":{"User-Agent":"synthetic-test","X-Test":"value"},"noGRPCHeader":true,"noSSEHeader":false,"xPaddingObfsMode":true}`,
		"defaults explicit strings": `{"xPaddingKey":"x_padding","xPaddingHeader":"X-Padding","xPaddingPlacement":"queryInHeader","xPaddingMethod":"repeat-x","uplinkHTTPMethod":"POST","sessionIDPlacement":"path","sessionIDKey":"","seqPlacement":"path","seqKey":"","uplinkDataPlacement":"body","uplinkDataKey":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			raw := xhttpLimitedTestURL(extra)
			pn, err := parseRuntimeNode(raw)
			if err != nil {
				t.Fatal(err)
			}
			got := pn.Outbound["streamSettings"].(map[string]any)["xhttpSettings"].(map[string]any)["extra"].(json.RawMessage)
			if string(got) != extra {
				t.Fatal("resource validation rewrote existing core settings")
			}
			requirePinnedNodeConfig(t, raw)
		})
	}
}

func TestXHTTPLegacyPaddingAliasResourceLimitsRemainReadable(t *testing.T) {
	raw := xhttpLimitedTestURL(`{"sessionIDTable":"hex","sessionIDLength":2147483647}`) + "&x_padding_bytes=100"
	prepared, err := prepareStoredNode(raw)
	if err != nil {
		t.Fatalf("old padding alias prevented legacy recovery: %v", err)
	}
	if _, err := prepareNode(raw); err == nil {
		t.Fatal("unsafe alias node admitted")
	}
	a := testApp(t)
	st := newStore()
	st.Nodes = []Node{{ID: "legacy-alias", Name: "legacy-alias", Protocol: prepared.Parsed.Protocol, RawURL: raw}}
	st.DefaultNodeID = "legacy-alias"
	st.SceneEnabled[SceneGlobal] = true
	if err := a.saveStore(st); err != nil {
		t.Fatal(err)
	}
	loaded, err := a.loadStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.renderXrayConfig(loaded); err == nil {
		t.Fatal("unsafe alias rendered")
	}
	if err := removeNodeFromStore(loaded, "legacy-alias"); err != nil {
		t.Fatal(err)
	}
	if err := a.saveStore(loaded); err != nil {
		t.Fatal(err)
	}
	reloaded, err := a.loadStore()
	if err != nil || len(reloaded.Nodes) != 0 {
		t.Fatalf("legacy alias removal failed: %v", err)
	}
}

func TestXHTTPMatchingRedundantOuterFields(t *testing.T) {
	for name, tc := range map[string]struct{ outer, extra string }{
		"matching mode":         {"&mode=auto", `{"mode":"auto","xPaddingBytes":"100-200"}`},
		"default mode":          {"", `{"mode":"auto"}`},
		"empty extra mode":      {"&mode=auto", `{"mode":""}`},
		"empty default mode":    {"", `{"mode":""}`},
		"matching host path":    {"&host=cdn.example&path=%2Fproxy&mode=packet-up", `{"host":"cdn.example","path":"/proxy","mode":"packet-up"}`},
		"matching default path": {"", `{"host":"","path":"/"}`},
		"equivalent field case": {"&host=cdn.example&mode=auto", `{"hoſt":"cdn.example","MODE":"auto"}`},
	} {
		t.Run(name, func(t *testing.T) {
			raw := xhttpLimitedTestURL(tc.extra) + tc.outer
			if _, err := parseRuntimeNode(raw); err != nil {
				t.Fatal(err)
			}
			requirePinnedNodeConfig(t, raw)
		})
	}
}

func TestXHTTPRedundantOuterFieldsRejectConflicts(t *testing.T) {
	for name, extra := range map[string]string{
		"mode conflict":          `{"mode":"stream-up"}`,
		"host conflict":          `{"host":"other.example"}`,
		"path conflict":          `{"path":"/other"}`,
		"mode invalid case":      `{"mode":"AUTO"}`,
		"mode null":              `{"mode":null}`,
		"mode duplicate":         `{"mode":"auto","MODE":"auto"}`,
		"host unicode duplicate": `{"host":"cdn.example","hoſt":"cdn.example"}`,
		"path duplicate":         `{"path":"/proxy","PATH":"/proxy"}`,
	} {
		t.Run(name, func(t *testing.T) {
			raw := xhttpLimitedTestURL(extra) + "&host=cdn.example&path=%2Fproxy&mode=auto"
			if _, err := prepareStoredNode(raw); err != nil {
				t.Fatalf("legacy node became unreadable: %v", err)
			}
			if _, err := parseRuntimeNode(raw); err == nil {
				t.Fatal("conflicting or duplicate redundant field accepted")
			}
		})
	}
}
