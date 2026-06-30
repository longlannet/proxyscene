package manager

import (
	"bytes"
	"encoding/json"
	"testing"
)

func parseJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, b)
	}
	return m
}

func TestSetOpenClawTelegramProxyPreservesOtherKeys(t *testing.T) {
	in := []byte(`{"channels":{"telegram":{"botToken":"SECRET-TOKEN","enabled":true}},"models":{"x":1}}`)
	out, changed, err := setOpenClawTelegramProxy(in, "http://127.0.0.1:7892")
	if err != nil || !changed {
		t.Fatalf("setOpenClawTelegramProxy: changed=%v err=%v", changed, err)
	}
	tg := parseJSON(t, out)["channels"].(map[string]any)["telegram"].(map[string]any)
	if tg["proxy"] != "http://127.0.0.1:7892" {
		t.Fatalf("proxy=%v, want http://127.0.0.1:7892", tg["proxy"])
	}
	// 关键：机密与其它字段必须原样保留。
	if tg["botToken"] != "SECRET-TOKEN" || tg["enabled"] != true {
		t.Fatalf("telegram 其它字段被改：%+v", tg)
	}
	if _, ok := parseJSON(t, out)["models"]; !ok {
		t.Fatalf("顶层 models 字段丢失")
	}
}

func TestSetOpenClawTelegramProxyIdempotent(t *testing.T) {
	in := []byte(`{"channels":{"telegram":{"proxy":"http://127.0.0.1:7892","botToken":"x"}}}`)
	out, changed, err := setOpenClawTelegramProxy(in, "http://127.0.0.1:7892")
	if err != nil || changed {
		t.Fatalf("已是同值应 changed=false：changed=%v err=%v", changed, err)
	}
	if !bytes.Equal(out, in) {
		t.Fatalf("同值时不应改写内容")
	}
}

func TestSetOpenClawTelegramProxyOverwritesExisting(t *testing.T) {
	in := []byte(`{"channels":{"telegram":{"proxy":"socks5://1.2.3.4:1080","botToken":"x"}}}`)
	out, changed, err := setOpenClawTelegramProxy(in, "http://127.0.0.1:7892")
	if err != nil || !changed {
		t.Fatalf("不同值应 changed=true：changed=%v err=%v", changed, err)
	}
	tg := parseJSON(t, out)["channels"].(map[string]any)["telegram"].(map[string]any)
	if tg["proxy"] != "http://127.0.0.1:7892" {
		t.Fatalf("proxy 未更新：%v", tg["proxy"])
	}
}

func TestClearOpenClawTelegramProxy(t *testing.T) {
	const ours = "http://127.0.0.1:7892"

	// 当前值是我们设的 -> 删除，保留 botToken。
	out, changed, err := clearOpenClawTelegramProxy([]byte(`{"channels":{"telegram":{"proxy":"`+ours+`","botToken":"x"}}}`), ours)
	if err != nil || !changed {
		t.Fatalf("我们设的值应被删除：changed=%v err=%v", changed, err)
	}
	tg := parseJSON(t, out)["channels"].(map[string]any)["telegram"].(map[string]any)
	if _, has := tg["proxy"]; has {
		t.Fatalf("proxy 应已删除：%+v", tg)
	}
	if tg["botToken"] != "x" {
		t.Fatalf("botToken 不应受影响：%+v", tg)
	}

	// 当前值是用户后改的别的值 -> 不动（不覆盖用户设置）。
	in2 := []byte(`{"channels":{"telegram":{"proxy":"socks5://user-own:1080"}}}`)
	out2, changed2, err := clearOpenClawTelegramProxy(in2, ours)
	if err != nil || changed2 {
		t.Fatalf("非我们的值不应改动：changed=%v err=%v", changed2, err)
	}
	if !bytes.Equal(out2, in2) {
		t.Fatalf("用户的值应原样保留")
	}

	// 当前无 proxy -> 不动。
	in3 := []byte(`{"channels":{"telegram":{"botToken":"x"}}}`)
	if _, changed3, err := clearOpenClawTelegramProxy(in3, ours); err != nil || changed3 {
		t.Fatalf("无 proxy 应 changed=false：changed=%v err=%v", changed3, err)
	}
}

func TestJSONNestedHelpers(t *testing.T) {
	m := map[string]any{}
	setJSONString(m, "v", "channels", "telegram", "proxy") // 应创建中间层级
	if got, ok := getJSONString(m, "channels", "telegram", "proxy"); !ok || got != "v" {
		t.Fatalf("set/get nested 失败：got=%q ok=%v", got, ok)
	}
	if _, ok := getJSONString(m, "channels", "discord", "proxy"); ok {
		t.Fatalf("不存在路径不应返回 ok")
	}
	setJSONString(m, "T", "channels", "telegram", "botToken")
	deleteJSONKey(m, "channels", "telegram", "proxy")
	if _, ok := getJSONString(m, "channels", "telegram", "proxy"); ok {
		t.Fatalf("proxy 应已删除")
	}
	if got, ok := getJSONString(m, "channels", "telegram", "botToken"); !ok || got != "T" {
		t.Fatalf("删除 proxy 不应影响 botToken：got=%q ok=%v", got, ok)
	}
}

func TestUnitHasOpenClawGatewayMarker(t *testing.T) {
	gw := "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw\nEnvironment=OPENCLAW_SERVICE_KIND=gateway\n"
	if !unitHasOpenClawGatewayMarker(gw) {
		t.Errorf("带 marker+gateway 的单元应命中")
	}
	node := "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw\nEnvironment=OPENCLAW_SERVICE_KIND=node\n"
	if unitHasOpenClawGatewayMarker(node) {
		t.Errorf("KIND=node 不应命中")
	}
	guard := "[Unit]\nDescription=OpenClaw xhigh guard\n[Service]\nExecStart=/usr/bin/node /x/guard.mjs\n"
	if unitHasOpenClawGatewayMarker(guard) {
		t.Errorf("无 marker 的 guard 不应命中")
	}
}

func TestIsOpenClawTargetFallback(t *testing.T) {
	a := testApp(t)
	// 用磁盘上不存在的单元名，强制走「定位不到单元 -> 按名前缀回退」分支，结果确定。
	cases := map[string]bool{
		"openclaw-gateway-nx-zzz.service": true,
		"openclaw-nx-zzz.service":         true,
		"hermes-gateway-nx-zzz.service":   false,
		"telegram-bot-nx-zzz.service":     false,
	}
	for svc, want := range cases {
		if got := a.isOpenClawTarget(systemdTargetName{Service: svc}); got != want {
			t.Errorf("isOpenClawTarget(%q)=%v, want %v", svc, got, want)
		}
	}
}
