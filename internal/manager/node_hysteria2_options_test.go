package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHysteria2BandwidthPreservesDecimalRateAndAliases(t *testing.T) {
	for name, query := range map[string]string{
		"mbps":               "upmbps=100&downmbps=12.5",
		"units":              "up=0.1gbps&down=12500kbps",
		"numeric":            "up=100&down=12.5",
		"consistent aliases": "up=100mbps&upmbps=100&down=12.5&downmbps=12.5",
	} {
		t.Run(name, func(t *testing.T) {
			pn, err := parseRuntimeNode("hy2://secret@example.com?" + query)
			if err != nil {
				t.Fatal(err)
			}
			mask := pn.Outbound["streamSettings"].(map[string]any)["finalmask"].(map[string]any)
			want := map[string]any{"congestion": "brutal", "brutalUp": "100000000 bps", "brutalDown": "12500000 bps"}
			if !reflect.DeepEqual(mask["quicParams"], want) {
				t.Fatalf("bandwidth meaning changed: %#v", mask["quicParams"])
			}
		})
	}
	for raw, want := range map[string]string{"0": "0", "0.524288mbps": "524288", "1tbps": "1000000000000"} {
		got, err := normalizeHysteria2Bandwidth(raw, false)
		if err != nil || got != want {
			t.Fatalf("rate %q: got %q, %v", raw, got, err)
		}
	}
}

func TestHysteria2PortHoppingPreservesMaskOrderAndAliases(t *testing.T) {
	pn, err := parseRuntimeNode("hy2://secret@example.com:443?mport=8002,8000-8001,443&ports=443,8000-8002&hopInterval=30&hop-interval=30s&obfs=salamander&obfs-password=synthetic-secret")
	if err != nil {
		t.Fatal(err)
	}
	if pn.EndpointPort != 443 {
		t.Fatal("base endpoint changed")
	}
	mask := pn.Outbound["streamSettings"].(map[string]any)["finalmask"].(map[string]any)
	want := []any{
		map[string]any{"type": "salamander", "settings": map[string]any{"password": "synthetic-secret"}},
		map[string]any{"type": "udphop", "settings": map[string]any{"mode": "intervalLocal,intervalRemote", "interval": 30, "remotePorts": "443,8000-8002"}},
	}
	if !reflect.DeepEqual(mask["udp"], want) {
		t.Fatalf("bad hopping configuration: %#v", mask["udp"])
	}
}

func TestHysteria2RejectsInvalidExtendedOptions(t *testing.T) {
	cases := []string{
		"mport=", "mport=0", "mport=65536", "mport=-1", "mport=8000-7999", "mport=443,", "mport=443,,444",
		"mport=443-444-445", "mport=1-65535", "mport=env:PORT", "mport=443%20", "mport=443&ports=444",
		"mport=443&mport=443", "hop-interval=30", "mport=443&hop-interval=", "mport=443&hop-interval=4",
		"mport=443&hop-interval=-5", "mport=443&hop-interval=5.1s", "mport=443&hop-interval=86401s",
		"mport=443&hop-interval=99999999999999999999s", "mport=443&hop-interval=5&hopInterval=6",
		"upmbps=", "upmbps=-1", "upmbps=NaN", "upmbps=Inf", "upmbps=1e3", "upmbps=1Mbps",
		"upmbps=0.5", "upmbps=1000001", "upmbps=9999999999999999999999999999999999999999999999999999",
		"upmbps=0.5242881", "upmbps=100&up=101", "upmbps=100&upmbps=100", "upmbps=0&up=1",
		"down=-1", "down=0.5mbps", "down=100megabits", "down=1gbps&downmbps=999", "down=",
		"up=100&congestion=bbr", "disable-mtu-discovery=1", "pinSHA256=0011",
	}
	for _, query := range cases {
		t.Run(query, func(t *testing.T) {
			if _, err := parseRuntimeNode("hy2://secret@example.com?" + query); err == nil {
				t.Fatal("invalid parameter accepted")
			}
		})
	}
}

func TestHysteria2ExtendedConfigsPassPinnedXray(t *testing.T) {
	for name, query := range map[string]string{
		"bandwidth":          "upmbps=100&downmbps=12.5",
		"automatic":          "upmbps=0&downmbps=0",
		"boundary bandwidth": "up=524288bps&down=1tbps",
		"hopping":            "mport=443,8000-8005",
		"hopping bounds":     "mport=1,65535&hop-interval=24h",
		"combined":           "upmbps=100&downmbps=12.5&mport=8000-8005&hop-interval=5s&obfs=salamander&obfs-password=synthetic-secret",
	} {
		t.Run(name, func(t *testing.T) { requirePinnedNodeConfig(t, "hy2://secret@example.com?"+query) })
	}
}

// A configuration check alone does not instantiate UDP masks. Actually trigger
// the core dial and observe the QUIC datagram at the hopping port, distinct from
// the URI's base port. The combined case detects reversed mask-order mistakes.
func TestHysteria2PinnedCoreUsesHoppingPort(t *testing.T) {
	bin := os.Getenv("PROXYSCENE_TEST_XRAY")
	if bin == "" {
		t.Skip("set PROXYSCENE_TEST_XRAY for runtime UDP hopping validation")
	}
	for _, obfs := range []bool{false, true} {
		t.Run(fmt.Sprint("salamander=", obfs), func(t *testing.T) {
			base, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer base.Close()
			hopped, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer hopped.Close()
			port := hopped.LocalAddr().(*net.UDPAddr).Port
			raw := "hy2://synthetic-secret@" + base.LocalAddr().String() + "?mport=" + strconv.Itoa(port) + "&hop-interval=5&upmbps=10&downmbps=20"
			if obfs {
				raw += "&obfs=salamander&obfs-password=synthetic-secret"
			}
			pn, err := parseRuntimeNode(raw)
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := listener.Addr().String()
			httpPort := listener.Addr().(*net.TCPAddr).Port
			listener.Close()
			config := map[string]any{"log": map[string]any{"loglevel": "none"}, "inbounds": []any{map[string]any{"listen": "127.0.0.1", "port": httpPort, "protocol": "http"}}, "outbounds": []any{pn.Outbound}}
			data, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "hopping-test.json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			cmd := exec.CommandContext(ctx, bin, "run", "-config", path)
			if err := cmd.Start(); err != nil {
				cancel()
				t.Fatal(err)
			}
			defer func() { cancel(); _ = cmd.Wait() }()
			deadline := time.Now().Add(3 * time.Second)
			for {
				conn, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
				if err == nil {
					conn.Close()
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("test core did not start")
				}
				time.Sleep(10 * time.Millisecond)
			}
			proxyURL, _ := url.Parse("http://" + address)
			transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 4 * time.Second}
			requestCtx, stopRequest := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				req, _ := http.NewRequestWithContext(requestCtx, http.MethodGet, "https://example.com/", nil)
				resp, err := client.Do(req)
				if err == nil {
					resp.Body.Close()
				}
			}()
			defer func() { stopRequest(); <-done }()
			_ = hopped.SetReadDeadline(time.Now().Add(3 * time.Second))
			packet := make([]byte, 4096)
			n, _, err := hopped.ReadFrom(packet)
			if err != nil || n == 0 {
				t.Fatalf("core did not send UDP to configured hopping port: %v", err)
			}
			_ = base.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			if _, _, err := base.ReadFrom(packet); err == nil {
				t.Fatal("core sent UDP to base port instead of hopping selection")
			}
		})
	}
}

func TestHysteria2PortRangeNormalizationIsBounded(t *testing.T) {
	for raw, want := range map[string]string{"65535,1": "1,65535", "443,443,444-446,445-447": "443-447", "1-16384": "1-16384"} {
		got, err := normalizeHysteria2Ports(raw)
		if err != nil || got != want {
			t.Fatalf("ports %q: %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"1-16385", strings.Repeat("1,", 128) + "1", strings.Repeat("1", 2049)} {
		if _, err := normalizeHysteria2Ports(raw); err == nil {
			t.Fatal("unbounded port list accepted")
		}
	}
}
