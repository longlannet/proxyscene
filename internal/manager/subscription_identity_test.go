package manager

import (
	"encoding/base64"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const subscriptionIdentityUUID = "11111111-1111-1111-1111-111111111111"

func identityTestRefresh(t *testing.T, source string, raws ...string) preparedSubscription {
	t.Helper()
	prepared, err := prepareSubscriptionBody(source, []byte(strings.Join(raws, "\n")))
	if err != nil || prepared.Invalid != 0 || prepared.Incomplete || len(prepared.Nodes) == 0 {
		t.Fatalf("invalid synthetic subscription: nodes=%d invalid=%d incomplete=%t err=%v", len(prepared.Nodes), prepared.Invalid, prepared.Incomplete, err)
	}
	return prepared
}

func identityTestEquivalentLinks() map[string][]string {
	vmessA := `{"v":"2","ps":"provider old","add":"node.example","port":"443","id":"` + subscriptionIdentityUUID + `","aid":"0","scy":"auto","net":"ws","tls":"tls","sni":"tls.example","host":"cdn.example","path":"/ws"}`
	vmessB := `{"path":"\u002fws","host":"cdn.example","sni":"tls.example","tls":"tls","net":"ws","scy":"auto","aid":0,"id":"` + subscriptionIdentityUUID + `","port":443,"add":"node.example","ps":"provider renamed","v":"2"}`
	return map[string][]string{
		"VLESS": {
			"vless://" + subscriptionIdentityUUID + "@node.example:443?security=tls&sni=tls.example&type=ws&host=cdn.example&path=%2Fws#provider-old",
			"vless://" + subscriptionIdentityUUID + "@node.example:443?path=%2fws&host=cdn.example&type=ws&serverName=tls.example&security=tls#provider-renamed",
			"vless://" + subscriptionIdentityUUID + "@node.example:443?type=ws&path=/ws&security=tls&servername=tls.example&host=cdn.example#quota-remaining-123",
		},
		"VMess": {
			"vmess://" + base64.StdEncoding.EncodeToString([]byte(vmessA)),
			"vmess://" + base64.RawURLEncoding.EncodeToString([]byte(vmessB)),
			"vmess://" + base64.RawStdEncoding.EncodeToString([]byte(strings.ReplaceAll(vmessB, "provider renamed", "quota remaining 123"))),
		},
		"Trojan": {
			"trojan://secret@node.example:443?sni=tls.example&type=ws&host=cdn.example&path=%2Fws#provider-old",
			"trojan://%73ecret@node.example:443?path=%2fws&host=cdn.example&type=ws&serverName=tls.example#provider-renamed",
			"trojan://secret@node.example:443?type=ws&path=/ws&servername=tls.example&host=cdn.example#quota-remaining-123",
		},
		"Shadowsocks": {
			"ss://aes-256-gcm:secret@node.example:8388#provider-old",
			"ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:secret")) + "@node.example:8388#provider-renamed",
			"ss://" + base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:secret@node.example:8388")) + "#quota-remaining-123",
		},
		"Hysteria2": {
			"hysteria2://secret@node.example:443?sni=tls.example&obfs=salamander&obfs-password=mask-secret#provider-old",
			"hy2://%73ecret@node.example:443?obfs-password=mask-secret&obfs=salamander&serverName=tls.example#provider-renamed",
			"hysteria2://secret@node.example:443?obfs=salamander&servername=tls.example&obfs-password=mask-secret#quota-remaining-123",
		},
	}
}

func TestSubscriptionIdentityEquivalentRefreshPreservesNodeAndSelections(t *testing.T) {
	for protocol, links := range identityTestEquivalentLinks() {
		t.Run(protocol, func(t *testing.T) {
			st := newStore()
			if _, err := reconcileSubscriptions(st, []preparedSubscription{identityTestRefresh(t, reconcileSourceA, links[0])}); err != nil {
				t.Fatal(err)
			}
			st.Nodes[0].Name = "operator's custom remark"
			st.Nodes[0].CreatedAt = time.Unix(100, 0)
			st.Nodes[0].UpdatedAt = time.Unix(200, 0)
			original := st.Nodes[0]
			for _, scene := range []Scene{SceneGlobal, SceneDev, SceneTelegram} {
				st.SceneNodes[scene] = original.ID
			}
			st.SceneEnabled[SceneTelegram] = false
			speed := SpeedResult{NodeID: original.ID, Target: "synthetic probe", LatencyMS: 73, Success: true, TestedAt: time.Unix(300, 0)}
			st.SpeedResults[original.ID] = speed
			for cycle := 0; cycle < 3; cycle++ {
				for i, raw := range links {
					previous := st.Nodes[0]
					started := time.Now()
					changes, err := reconcileSubscriptions(st, []preparedSubscription{identityTestRefresh(t, reconcileSourceA, raw)})
					if err != nil {
						t.Fatal(err)
					}
					if len(st.Nodes) != 1 {
						t.Fatalf("cycle %d variant %d accumulated nodes: %d", cycle, i, len(st.Nodes))
					}
					got := st.Nodes[0]
					want := original
					want.RawURL, want.UpdatedAt = raw, got.UpdatedAt
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("cycle %d variant %d failed to refresh URL while preserving identity and custom metadata", cycle, i)
					}
					if previous.RawURL == raw && !got.UpdatedAt.Equal(previous.UpdatedAt) {
						t.Fatal("identical refresh changed update timestamp")
					}
					if previous.RawURL != raw && got.UpdatedAt.Before(started) {
						t.Fatal("changed URL did not update its timestamp")
					}
					if changes.Added != 0 || changes.Existing != 1 || changes.Removed != 0 || changes.Reselected != 0 {
						t.Fatalf("equivalent refresh reported a connection change: %+v", changes)
					}
					if st.DefaultNodeID != original.ID || !reflect.DeepEqual(st.SpeedResults[original.ID], speed) {
						t.Fatal("equivalent refresh changed default selection or speed result")
					}
					for _, scene := range []Scene{SceneGlobal, SceneDev, SceneTelegram} {
						if st.SceneNodes[scene] != original.ID || st.selectedNodeID(scene) != original.ID {
							t.Fatalf("equivalent refresh changed %s selection", scene)
						}
					}
				}
			}
		})
	}
}

func TestSubscriptionIdentityEquivalentLinksInSingleResponseAreDeduplicated(t *testing.T) {
	for protocol, links := range identityTestEquivalentLinks() {
		t.Run(protocol, func(t *testing.T) {
			st := newStore()
			changes, err := reconcileSubscriptions(st, []preparedSubscription{identityTestRefresh(t, reconcileSourceA, links...)})
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Nodes) != 1 || changes.Added != 1 || changes.Existing != 0 || changes.Reselected != 0 {
				t.Fatalf("one connection with several encodings was imported repeatedly: nodes=%d changes=%+v", len(st.Nodes), changes)
			}
			if st.Nodes[0].RawURL != links[0] || !st.Nodes[0].SubscriptionManaged || !reflect.DeepEqual(st.Nodes[0].SubscriptionIDs, []string{subscriptionID(reconcileSourceA)}) {
				t.Fatal("deduplication changed the first node or its source ownership")
			}
		})
	}
}

func TestSubscriptionIdentityEquivalentSourcesShareOwnership(t *testing.T) {
	links := identityTestEquivalentLinks()["Trojan"]
	anchor := reconcileTestNode(t, "identity-anchor", false)
	st := reconcileTestStore(anchor)
	if _, err := reconcileSubscriptions(st, []preparedSubscription{identityTestRefresh(t, reconcileSourceA, links[0])}); err != nil {
		t.Fatal(err)
	}
	shared := st.Nodes[1]
	shared.Name = "shared custom remark"
	st.Nodes[1] = shared
	speed := SpeedResult{NodeID: shared.ID, LatencyMS: 81, Success: true}
	st.SpeedResults[shared.ID] = speed
	changes, err := reconcileSubscriptions(st, []preparedSubscription{identityTestRefresh(t, reconcileSourceB, links[1])})
	if err != nil {
		t.Fatal(err)
	}
	both := []string{subscriptionID(reconcileSourceA), subscriptionID(reconcileSourceB)}
	slices.Sort(both)
	got := st.findNode(shared.ID)
	if len(st.Nodes) != 2 || got == nil || !reflect.DeepEqual(got.SubscriptionIDs, both) || got.Name != shared.Name || changes.Added != 0 {
		t.Fatalf("equivalent second source did not join the existing connection: nodes=%d changes=%+v", len(st.Nodes), changes)
	}
	if _, err := reconcileSubscriptions(st, []preparedSubscription{identityTestRefresh(t, reconcileSourceA, anchor.RawURL)}); err != nil {
		t.Fatal(err)
	}
	got = st.findNode(shared.ID)
	if got == nil || !reflect.DeepEqual(got.SubscriptionIDs, []string{subscriptionID(reconcileSourceB)}) || !reflect.DeepEqual(st.SpeedResults[shared.ID], speed) {
		t.Fatal("removing source A lost source B's connection or speed result")
	}
	changes, err = reconcileSubscriptions(st, []preparedSubscription{identityTestRefresh(t, reconcileSourceB, anchor.RawURL)})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Nodes) != 1 || st.findNode(shared.ID) != nil || changes.Removed != 1 {
		t.Fatalf("unselected connection survived loss of its last source: nodes=%d changes=%+v", len(st.Nodes), changes)
	}
	if _, exists := st.SpeedResults[shared.ID]; exists {
		t.Fatal("removed shared connection left a speed result")
	}
}

func TestSubscriptionIdentityEquivalentManualNodeRemainsUnmanaged(t *testing.T) {
	links := identityTestEquivalentLinks()["Trojan"]
	prepared, err := prepareNode(links[0])
	if err != nil {
		t.Fatal(err)
	}
	manual := Node{ID: "manual-identity", Name: "my manually saved node", Protocol: prepared.Parsed.Protocol, RawURL: prepared.RawURL}
	anchor := reconcileTestNode(t, "manual-anchor", false)
	st := reconcileTestStore(anchor, manual)
	changes, err := reconcileSubscriptions(st, []preparedSubscription{identityTestRefresh(t, reconcileSourceA, links[1])})
	if err != nil {
		t.Fatal(err)
	}
	got := st.findNode(manual.ID)
	if len(st.Nodes) != 2 || got == nil || got.SubscriptionManaged || got.RawURL != manual.RawURL || got.Name != manual.Name || changes.Added != 0 {
		t.Fatalf("matching a manual connection duplicated or claimed it: nodes=%d changes=%+v", len(st.Nodes), changes)
	}
	if !reflect.DeepEqual(got.SubscriptionIDs, []string{subscriptionID(reconcileSourceA)}) {
		t.Fatal("matching a manual connection did not record its source")
	}
	if _, err := reconcileSubscriptions(st, []preparedSubscription{identityTestRefresh(t, reconcileSourceA, anchor.RawURL)}); err != nil {
		t.Fatal(err)
	}
	got = st.findNode(manual.ID)
	if got == nil || got.SubscriptionManaged || len(got.SubscriptionIDs) != 0 || got.RawURL != manual.RawURL {
		t.Fatal("source disappearance removed or reclassified a manual connection")
	}
}

func TestSubscriptionIdentityCanonicalizesNestedXHTTPJSON(t *testing.T) {
	base := "vless://" + subscriptionIdentityUUID + "@node.example:443?security=tls&type=xhttp&host=cdn.example&path=%2Fx&mode=packet-up&extra="
	extraA := `{"headers":{"X-Test":"value","User-Agent":"synthetic-test"},"xmux":{"maxConnections":2,"hMaxRequestTimes":100},"scMaxBufferedPosts":2}`
	extraB := ` { "scMaxBufferedPosts":2, "xmux":{"hMaxRequestTimes":100,"maxConnections":2}, "headers":{"User-Agent":"synthetic-test","X-Test":"\u0076alue"} } `
	st := newStore()
	if _, err := reconcileSubscriptions(st, []preparedSubscription{identityTestRefresh(t, reconcileSourceA, base+url.QueryEscape(extraA)+"#first")}); err != nil {
		t.Fatal(err)
	}
	original := st.Nodes[0]
	changes, err := reconcileSubscriptions(st, []preparedSubscription{identityTestRefresh(t, reconcileSourceA, base+url.QueryEscape(extraB)+"#renamed")})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Nodes) != 1 || changes.Added != 0 || changes.Existing != 1 || changes.Reselected != 0 {
		t.Fatalf("equivalent nested XHTTP JSON accumulated or replaced a node: nodes=%d changes=%+v", len(st.Nodes), changes)
	}
	want := original
	want.RawURL, want.Name, want.UpdatedAt = base+url.QueryEscape(extraB)+"#renamed", "renamed", st.Nodes[0].UpdatedAt
	if !reflect.DeepEqual(st.Nodes[0], want) || !st.Nodes[0].UpdatedAt.After(original.UpdatedAt) {
		t.Fatal("equivalent XHTTP refresh did not update provider URL/name while retaining identity")
	}
}

func TestSubscriptionIdentityNeverMergesDifferentConnectionSettings(t *testing.T) {
	vless := "vless://" + subscriptionIdentityUUID + "@node.example:443?security=tls&sni=tls.example"
	trojan := "trojan://secret@node.example:443?sni=tls.example"
	hy2 := "hysteria2://secret@node.example:443?sni=tls.example"
	xhttp := vless + "&type=xhttp&extra="
	reality := "vless://" + subscriptionIdentityUUID + "@node.example:443?security=reality&sni=tls.example&pbk=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA&sid=abcd"
	vmess := vmessURL(t, map[string]any{"v": "2", "add": "node.example", "port": 443, "id": subscriptionIdentityUUID, "net": "tcp", "tls": "tls", "scy": "auto"})
	vmessDifferentCipher := vmessURL(t, map[string]any{"v": "2", "add": "node.example", "port": 443, "id": subscriptionIdentityUUID, "net": "tcp", "tls": "tls", "scy": "aes-128-gcm"})
	cases := map[string][2]string{
		"VLESS UUID":           {vless, strings.Replace(vless, subscriptionIdentityUUID, "22222222-2222-2222-2222-222222222222", 1)},
		"server address":       {vless, strings.Replace(vless, "node.example", "other.example", 1)},
		"server port":          {vless, strings.Replace(vless, ":443", ":8443", 1)},
		"TLS SNI":              {vless, strings.Replace(vless, "tls.example", "other.example", 1)},
		"TLS fingerprint":      {vless + "&fp=chrome", vless + "&fp=firefox"},
		"TLS ALPN order":       {vless + "&alpn=h2,http/1.1", vless + "&alpn=http/1.1,h2"},
		"VLESS flow":           {vless, vless + "&flow=xtls-rprx-vision"},
		"transport":            {vless, vless + "&type=ws&path=%2Fws"},
		"websocket path":       {vless + "&type=ws&path=%2Fa", vless + "&type=ws&path=%2Fb"},
		"websocket host":       {vless + "&type=ws&host=cdn.example", vless + "&type=ws&host=other.example"},
		"websocket early data": {vless + "&type=ws&ed=1024", vless + "&type=ws&ed=2048"},
		"gRPC service":         {vless + "&type=grpc&serviceName=a", vless + "&type=grpc&serviceName=b"},
		"gRPC mode":            {vless + "&type=grpc&mode=gun", vless + "&type=grpc&mode=multi"},
		"REALITY public key":   {reality, strings.Replace(reality, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE", 1)},
		"REALITY short ID":     {reality, strings.Replace(reality, "sid=abcd", "sid=abce", 1)},
		"Trojan password":      {trojan, strings.Replace(trojan, "secret@", "new-secret@", 1)},
		"VMess cipher":         {vmess, vmessDifferentCipher},
		"SS password":          {"ss://aes-256-gcm:secret@node.example:8388", "ss://aes-256-gcm:new-secret@node.example:8388"},
		"SS cipher":            {"ss://aes-256-gcm:secret@node.example:8388", "ss://aes-128-gcm:secret@node.example:8388"},
		"Hysteria2 auth":       {hy2, strings.Replace(hy2, "secret@", "new-secret@", 1)},
		"Hysteria2 bandwidth":  {hy2 + "&up=10", hy2 + "&up=20"},
		"Hysteria2 hopping":    {hy2 + "&mport=8443,8444", hy2 + "&mport=8443,8445"},
		"Hysteria2 obfs":       {hy2 + "&obfs=salamander&obfs-password=mask-one", hy2 + "&obfs=salamander&obfs-password=mask-two"},
		"XHTTP header value":   {xhttp + url.QueryEscape(`{"headers":{"X-Test":"one"}}`), xhttp + url.QueryEscape(`{"headers":{"X-Test":"two"}}`)},
		"XHTTP nested limit":   {xhttp + url.QueryEscape(`{"xmux":{"maxConnections":2}}`), xhttp + url.QueryEscape(`{"xmux":{"maxConnections":3}}`)},
	}
	for name, pair := range cases {
		t.Run(name, func(t *testing.T) {
			st := newStore()
			if _, err := reconcileSubscriptions(st, []preparedSubscription{identityTestRefresh(t, reconcileSourceA, pair[0])}); err != nil {
				t.Fatal(err)
			}
			original := st.Nodes[0]
			for _, scene := range []Scene{SceneGlobal, SceneDev, SceneTelegram} {
				st.SceneNodes[scene] = original.ID
			}
			st.SpeedResults[original.ID] = SpeedResult{NodeID: original.ID, Success: true}
			changes, err := reconcileSubscriptions(st, []preparedSubscription{identityTestRefresh(t, reconcileSourceA, pair[1])})
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Nodes) != 1 || changes.Added != 1 || changes.Reselected != 1 || changes.Existing != 0 || changes.Removed != 1 {
				t.Fatalf("changed connection did not replace the withdrawn selected connection: nodes=%d changes=%+v", len(st.Nodes), changes)
			}
			got := st.Nodes[0]
			if st.findNode(original.ID) != nil || got.ID == original.ID || got.RawURL != pair[1] || st.DefaultNodeID != got.ID {
				t.Fatal("refresh retained old connection data or did not migrate the default")
			}
			if !reflect.DeepEqual(got.SubscriptionIDs, []string{subscriptionID(reconcileSourceA)}) {
				t.Fatal("new connection did not acquire source ownership")
			}
			for _, scene := range []Scene{SceneGlobal, SceneDev, SceneTelegram} {
				if st.SceneNodes[scene] != got.ID {
					t.Fatalf("refresh did not migrate %s binding", scene)
				}
			}
			if len(st.SpeedResults) != 0 {
				t.Fatal("changed connection inherited stale speed results")
			}
		})
	}
}

func TestSubscriptionIdentityEquivalentRefreshUpdatesProviderName(t *testing.T) {
	for protocol, links := range identityTestEquivalentLinks() {
		t.Run(protocol, func(t *testing.T) {
			st := newStore()
			if _, err := reconcileSubscriptions(st, []preparedSubscription{identityTestRefresh(t, reconcileSourceA, links[0])}); err != nil {
				t.Fatal(err)
			}
			st.Nodes[0].CreatedAt, st.Nodes[0].UpdatedAt = time.Unix(100, 0), time.Unix(200, 0)
			original := st.Nodes[0]
			for _, raw := range links[1:] {
				prepared := identityTestRefresh(t, reconcileSourceA, raw)
				changes, err := reconcileSubscriptions(st, []preparedSubscription{prepared})
				if err != nil {
					t.Fatal(err)
				}
				if len(st.Nodes) != 1 {
					t.Fatal("provider rename duplicated the connection")
				}
				got := st.Nodes[0]
				if got.ID != original.ID || got.RawURL != raw || got.Name != prepared.Nodes[0].Parsed.Name || !got.CreatedAt.Equal(original.CreatedAt) || !got.UpdatedAt.After(original.UpdatedAt) || changes.Reselected != 0 {
					t.Fatalf("provider metadata did not follow refreshed subscription: changes=%+v", changes)
				}
			}
		})
	}
}
