package manager

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestSubscriptionDiagnosticsSummarizeWithoutSecrets(t *testing.T) {
	body := strings.Join([]string{
		"trojan://PUBLIC-TEST-PASSWORD@accepted.example:443",
		"trojan://HIDDEN-PASSWORD@rejected.example:443?HIDDEN-QUERY-FIELD=HIDDEN-TOKEN",
		"vless://HIDDEN-INVALID-UUID@rejected.example:443?security=tls",
		"tuic://HIDDEN-TUIC-CREDENTIAL@rejected.example:443",
		"HIDDEN-SCHEME://HIDDEN-OTHER-CREDENTIAL@rejected.example:443",
		"trojan://HIDDEN-PASSWORD@rejected.example:443?sni=one.example&serverName=two.example",
	}, "\n")
	prepared, err := prepareSubscriptionBody("https://subscription.example/HIDDEN-PATH?token=HIDDEN-TOKEN", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Nodes) != 1 || prepared.Invalid != 5 || !prepared.Incomplete {
		t.Fatalf("unexpected diagnostic counts: accepted=%d invalid=%d incomplete=%v", len(prepared.Nodes), prepared.Invalid, prepared.Incomplete)
	}
	if prepared.Unsupported["TUIC"] != 1 || prepared.Unsupported["其他"] != 1 || len(prepared.Unsupported) != 2 {
		t.Fatalf("unexpected unsupported protocol counts: %v", prepared.Unsupported)
	}
	summary := prepared.diagnosticSummary()
	for _, want := range []string{"识别 6 条，接受 1 条，失败 5 条", "TUIC 1 条", "其他 1 条", "第 2 条（Trojan）：参数不受支持", "第 3 条（VLESS）：身份凭据格式无效或缺失", "第 6 条（Trojan）：参数冲突或重复"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q: %s", want, summary)
		}
	}
	assertSubscriptionDiagnosticPrivate(t, summary)
	if err := validateSubscriptionRefresh(prepared); err == nil {
		t.Fatal("diagnostics must not permit a partial refresh")
	}
}

func TestSubscriptionDiagnosticsAllRejectedAndBase64(t *testing.T) {
	body := "tuic://HIDDEN-CREDENTIAL@rejected.example:443\ntrojan://HIDDEN-PASSWORD@rejected.example:invalid"
	for _, encoded := range []bool{false, true} {
		t.Run(fmt.Sprint(encoded), func(t *testing.T) {
			input := body
			if encoded {
				input = base64.StdEncoding.EncodeToString([]byte(body))
			}
			prepared, err := prepareSubscriptionBody("https://subscription.example?key=HIDDEN-TOKEN", []byte(input))
			if err == nil || !strings.Contains(err.Error(), "订阅中没有可导入节点") || !strings.Contains(err.Error(), "识别 2 条，接受 0 条，失败 2 条") {
				t.Fatalf("all-rejected feed did not explain its failures: %v", err)
			}
			if prepared.Invalid != 2 || len(prepared.Diagnostics) != 2 {
				t.Fatalf("all-rejected feed lost diagnostics: invalid=%d diagnostic count=%d", prepared.Invalid, len(prepared.Diagnostics))
			}
			assertSubscriptionDiagnosticPrivate(t, err.Error())
		})
	}
}

func TestSubscriptionDiagnosticsBoundDetails(t *testing.T) {
	const count = maxSubscriptionDiagnostics + 8
	body := strings.Repeat("trojan://HIDDEN-PASSWORD@invalid.example:invalid\n", count)
	prepared, err := prepareSubscriptionBody("https://subscription.example", []byte(body))
	if err == nil || prepared.Invalid != count || len(prepared.Diagnostics) != maxSubscriptionDiagnostics {
		t.Fatalf("unexpected bounded diagnostics: invalid=%d details=%d err=%v", prepared.Invalid, len(prepared.Diagnostics), err)
	}
	if !strings.Contains(err.Error(), "其余 8 条失败详情已省略") || strings.Contains(err.Error(), "第 6 条") || len(err.Error()) > 2048 {
		t.Fatalf("failure detail cap was not applied: %s", err)
	}
	assertSubscriptionDiagnosticPrivate(t, err.Error())
}

func TestSubscriptionDiagnosticsPreserveCompleteness(t *testing.T) {
	valid := "trojan://PUBLIC-TEST-PASSWORD@accepted.example:443"
	for _, tc := range []struct {
		name       string
		body       string
		incomplete bool
		invalid    int
	}{
		{"plain list", valid + "\n" + valid, false, 0},
		{"HTML wrapper", "<html>\n" + valid + "\n</html>", true, 0},
		{"unknown scheme", valid + "\nsecret-scheme://HIDDEN-CREDENTIAL@unknown.example:443", true, 1},
		{"extra prose", valid + "\nnot a node", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepared, err := prepareSubscriptionBody("https://subscription.example", []byte(tc.body))
			if err != nil || prepared.Incomplete != tc.incomplete || prepared.Invalid != tc.invalid {
				t.Fatalf("unexpected completeness: incomplete=%v invalid=%d err=%v", prepared.Incomplete, prepared.Invalid, err)
			}
			if err := validateSubscriptionRefresh(prepared); (err != nil) != tc.incomplete {
				t.Fatalf("refresh completeness changed: %v", err)
			}
		})
	}
}

func TestSubscriptionDiagnosticsCountUnknownAgainstLimits(t *testing.T) {
	body := strings.Repeat("unknown://HIDDEN-CREDENTIAL@unknown.example:443\n", maxSubscriptionNodes+1)
	_, err := prepareSubscriptionBody("https://subscription.example", []byte(body))
	if err == nil || !strings.Contains(err.Error(), "节点数超过上限") {
		t.Fatalf("unknown schemes bypassed the subscription node limit: %v", err)
	}
	_, err = prepareSubscriptionBody("https://subscription.example", []byte(strings.Repeat("x", int(maxSubscriptionBytes)+1)))
	if err == nil || !strings.Contains(err.Error(), "订阅内容过大") {
		t.Fatalf("oversized content reached parsing: %v", err)
	}
}

func TestSubscriptionDiagnosticsHysteriaAliasesRecognized(t *testing.T) {
	body := "hy2://PUBLIC-TEST-PASSWORD@one.example:443\nhysteria2://PUBLIC-TEST-PASSWORD@two.example:443"
	urls, tooMany := extractNodeURLsLimited(body, maxSubscriptionNodes)
	if tooMany || len(urls) != 2 {
		t.Fatalf("Hysteria2 aliases were not extracted: count=%d tooMany=%v", len(urls), tooMany)
	}
	for _, raw := range urls {
		if label, supported := subscriptionProtocolLabel(raw); label != "Hysteria2" || !supported {
			t.Fatalf("Hysteria2 alias was counted as unsupported: label=%q supported=%v", label, supported)
		}
	}
	prepared, err := prepareSubscriptionBody("https://subscription.example", []byte(body))
	if err != nil || len(prepared.Nodes) != 2 || prepared.Invalid != 0 || prepared.Incomplete || len(prepared.Unsupported) != 0 {
		t.Fatalf("Hysteria2 feed was not completely accepted: accepted=%d invalid=%d incomplete=%v err=%v", len(prepared.Nodes), prepared.Invalid, prepared.Incomplete, err)
	}
}

func TestSubscriptionDiagnosticsNeverEchoParserErrors(t *testing.T) {
	for _, message := range []string{
		"HIDDEN-QUERY-FIELD=HIDDEN-TOKEN\x1b[2J",
		"节点不兼容：HIDDEN-PASSWORD",
		"TLS 参数无效：https://hidden.example/HIDDEN-PATH?token=HIDDEN-TOKEN",
		"当前 transport 无法表达参数 HIDDEN-QUERY-FIELD",
	} {
		category := subscriptionErrorCategory(errors.New(message))
		assertSubscriptionDiagnosticPrivate(t, category)
		if len(category) > 120 {
			t.Fatalf("unbounded error category: %q", category)
		}
	}
}

func assertSubscriptionDiagnosticPrivate(t *testing.T, output string) {
	t.Helper()
	for _, secret := range []string{"HIDDEN", "hidden", "PUBLIC-TEST-PASSWORD", "rejected.example", "accepted.example", "subscription.example", "://", "\x1b"} {
		if strings.Contains(output, secret) {
			t.Fatalf("diagnostic exposed private input %q: %q", secret, output)
		}
	}
}
