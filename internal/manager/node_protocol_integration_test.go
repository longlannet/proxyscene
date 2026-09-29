package manager

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// These fixtures exercise real protocol handshakes and HTTPS forwarding with the
// release-pinned core. All credentials are generated locally; no subscription
// or production configuration is read. Every listener is bound to loopback.
type protocolFixtureTLS struct {
	certificate         tls.Certificate
	roots               *x509.CertPool
	certLines, keyLines []string
}

func newProtocolFixtureTLS(t *testing.T) protocolFixtureTLS {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "proxyscene local protocol test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"probe.test"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:        true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		t.Fatal("could not trust local test CA")
	}
	return protocolFixtureTLS{cert, roots, strings.Split(strings.TrimSpace(string(certPEM)), "\n"), strings.Split(strings.TrimSpace(string(keyPEM)), "\n")}
}

func protocolFixturePort(t *testing.T, network string) int {
	t.Helper()
	if network == "udp" {
		conn, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := conn.LocalAddr().(*net.UDPAddr).Port
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		return port
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func startProtocolFixtureServer(t *testing.T, bin string, inbound map[string]any, target string) {
	t.Helper()
	readyPort := protocolFixturePort(t, "tcp")
	targetURL, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	config := map[string]any{
		"log": map[string]any{"loglevel": "debug"},
		"inbounds": []any{inbound, map[string]any{
			"listen": "127.0.0.1", "port": readyPort, "protocol": "http",
			"settings": map[string]any{"accounts": []any{map[string]any{"user": "fixture", "pass": "fixture-local-only"}}},
		}},
		"outbounds": []any{map[string]any{"protocol": "freedom", "settings": map[string]any{"finalRules": []any{map[string]any{"action": "allow", "network": "tcp", "ip": []string{"127.0.0.1"}, "port": targetURL.Port()}, map[string]any{"action": "block", "blockDelay": "0"}}}}},
	}
	body, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "server.json")
	if err := os.WriteFile(configPath, body, 0600); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.OpenFile(filepath.Join(dir, "server.log"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cmd := exec.CommandContext(ctx, bin, "run", "-config", configPath)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	done, err := startNodeProbeProcess(cmd, cancel)
	if err != nil {
		cancel()
		logFile.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		<-done
		logFile.Close()
		if t.Failed() {
			output, _ := os.ReadFile(logFile.Name())
			t.Logf("server fixture log: %s", output)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(readyPort)), 30*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		select {
		case <-done:
			output, _ := os.ReadFile(logFile.Name())
			t.Fatalf("local core exited before readiness: %s", output)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	output, _ := os.ReadFile(logFile.Name())
	t.Fatalf("local pinned-core server did not become ready: %s", output)
}

func protocolFixtureNode(t *testing.T, kind string, ca protocolFixtureTLS) (string, map[string]any) {
	t.Helper()
	const id = "11111111-1111-1111-1111-111111111111"
	const password = "synthetic-local-protocol-secret"
	network := "tcp"
	if strings.HasPrefix(kind, "hysteria2") {
		network = "udp"
	}
	port := protocolFixturePort(t, network)
	endpoint := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	stream := map[string]any{"network": "raw", "security": "none"}
	inbound := map[string]any{"listen": "127.0.0.1", "port": port, "streamSettings": stream}
	tlsServer := func(alpn []string) {
		stream["security"] = "tls"
		stream["tlsSettings"] = map[string]any{"alpn": alpn, "certificates": []any{map[string]any{"certificate": ca.certLines, "key": ca.keyLines}}}
	}
	switch {
	case strings.HasPrefix(kind, "vless"):
		inbound["protocol"] = "vless"
		settings := map[string]any{"decryption": "none", "clients": []any{map[string]any{"id": id}}}
		inbound["settings"] = settings
		q := url.Values{"security": {"none"}, "type": {"raw"}}
		if kind == "vless-encryption" {
			key, err := ecdh.X25519().GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			settings["decryption"] = "mlkem768x25519plus.native.600s." + base64.RawURLEncoding.EncodeToString(key.Bytes())
			q.Set("encryption", "mlkem768x25519plus.native.1rtt."+base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()))
		}
		switch kind {
		case "vless-ws":
			stream["network"] = "ws"
			stream["wsSettings"] = map[string]any{"path": "/probe"}
			q.Set("type", "ws")
			q.Set("path", "/probe")
		case "vless-httpupgrade":
			stream["network"] = "httpupgrade"
			stream["httpupgradeSettings"] = map[string]any{"path": "/probe"}
			q.Set("type", "httpupgrade")
			q.Set("path", "/probe")
		case "vless-grpc":
			stream["network"] = "grpc"
			stream["grpcSettings"] = map[string]any{"serviceName": "probe"}
			q.Set("type", "grpc")
			q.Set("serviceName", "probe")
		case "vless-xhttp":
			stream["network"] = "xhttp"
			stream["xhttpSettings"] = map[string]any{"path": "/probe", "mode": "packet-up"}
			q.Set("type", "xhttp")
			q.Set("path", "/probe")
			q.Set("mode", "packet-up")
		}
		return "vless://" + id + "@" + endpoint + "?" + q.Encode(), inbound
	case kind == "vmess":
		inbound["protocol"] = "vmess"
		inbound["settings"] = map[string]any{"clients": []any{map[string]any{"id": id, "alterId": 0}}}
		return vmessURL(t, map[string]any{"v": "2", "add": "127.0.0.1", "port": port, "id": id, "aid": 0, "net": "tcp", "type": "none", "scy": "auto"}), inbound
	case kind == "trojan":
		inbound["protocol"] = "trojan"
		inbound["settings"] = map[string]any{"clients": []any{map[string]any{"password": password}}}
		tlsServer([]string{"http/1.1"})
		return "trojan://" + password + "@" + endpoint + "?sni=probe.test", inbound
	case strings.HasPrefix(kind, "ss"):
		method, key := "aes-128-gcm", password
		if kind == "ss2022-aes" {
			method, key = "2022-blake3-aes-128-gcm", base64.StdEncoding.EncodeToString([]byte("local-test-key16"))
		}
		if kind == "ss2022-chacha" {
			method, key = "2022-blake3-chacha20-poly1305", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
		}
		inbound["protocol"] = "shadowsocks"
		inbound["settings"] = map[string]any{"method": method, "password": key, "network": "tcp,udp"}
		return "ss://" + base64.RawURLEncoding.EncodeToString([]byte(method+":"+key)) + "@" + endpoint, inbound
	case strings.HasPrefix(kind, "hysteria2"):
		inbound["protocol"] = "hysteria"
		inbound["settings"] = map[string]any{"version": 2, "clients": []any{map[string]any{"auth": password}}}
		stream["network"] = "hysteria"
		stream["hysteriaSettings"] = map[string]any{"version": 2}
		tlsServer([]string{"h3"})
		suffix := "?sni=probe.test"
		if kind == "hysteria2-salamander" || kind == "hysteria2-hop-salamander" {
			stream["finalmask"] = map[string]any{"udp": []any{map[string]any{"type": "salamander", "settings": map[string]any{"password": "synthetic-obfs"}}}}
			suffix += "&obfs=salamander&obfs-password=synthetic-obfs"
		}
		if kind == "hysteria2-hop-salamander" {
			suffix += "&mport=" + strconv.Itoa(port) + "&hop-interval=5&upmbps=10&downmbps=20"
			endpoint = "127.0.0.1:1" // Only the declared hopping port has a UDP server.
		}
		return "hy2://" + password + "@" + endpoint + suffix, inbound
	}
	t.Fatalf("unknown fixture protocol %q", kind)
	return "", nil
}

func trustProtocolFixtureCA(pn *parsedNode, ca protocolFixtureTLS) {
	stream, _ := pn.Outbound["streamSettings"].(map[string]any)
	settings, ok := stream["tlsSettings"].(map[string]any)
	if ok {
		settings["certificates"] = []any{map[string]any{"certificate": ca.certLines, "usage": "verify"}}
		settings["disableSystemRoot"] = true
	}
}

func TestNodeProtocolsForwardHTTPSWithPinnedXray(t *testing.T) {
	bin := os.Getenv("PROXYSCENE_TEST_XRAY")
	if bin == "" {
		t.Skip("set PROXYSCENE_TEST_XRAY for real protocol integration")
	}
	bin = copyProtocolFixtureCore(t, bin)
	ca := newProtocolFixtureTLS(t)
	var requests atomic.Int64
	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/probe" {
			http.NotFound(w, r)
			return
		}
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	target.TLS = &tls.Config{Certificates: []tls.Certificate{ca.certificate}, MinVersion: tls.VersionTLS12}
	target.StartTLS()
	defer target.Close()
	for _, kind := range []string{"vless", "vless-encryption", "vless-ws", "vless-httpupgrade", "vless-grpc", "vless-xhttp", "vmess", "trojan", "ss", "ss2022-aes", "ss2022-chacha", "hysteria2", "hysteria2-salamander", "hysteria2-hop-salamander"} {
		t.Run(kind, func(t *testing.T) {
			raw, inbound := protocolFixtureNode(t, kind, ca)
			startProtocolFixtureServer(t, bin, inbound, target.URL)
			pn, err := parseRuntimeNode(raw)
			if err != nil {
				t.Fatal(err)
			}
			trustProtocolFixtureCA(pn, ca)
			a, opts := protocolFixtureProbe(t, bin)
			opts.targetURL = target.URL + "/probe"
			opts.allowLocalTarget = true
			opts.tlsConfig = &tls.Config{RootCAs: ca.roots, MinVersion: tls.VersionTLS12}
			opts.requestTimeout = 5 * time.Second
			before := requests.Load()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			latency, err := a.probeParsedNode(ctx, pn, opts)
			if err != nil {
				t.Fatalf("real %s forwarding failed: %v", kind, err)
			}
			if latency <= 0 || requests.Load() != before+1 {
				t.Fatal("probe succeeded without one real HTTPS request")
			}
		})
	}
}

// Defined separately from the server fixture so this same protocol matrix runs
// on both unprivileged CI runners and privileged local validation.
func protocolFixtureProbe(t *testing.T, bin string) (*App, nodeProbeOptions) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.XrayServiceUser = "nobody"
	opts := nodeProbeOptions{tempRoot: t.TempDir(), openCoreForTest: func() (*os.File, error) { return os.Open(bin) }}
	if os.Geteuid() != 0 {
		current, err := user.Current()
		if err != nil {
			t.Fatal(err)
		}
		identity, err := lookupLocalUserIdentity(current.Username)
		if err != nil {
			t.Fatal(err)
		}
		opts.identityForTest = &identity
	}
	return NewApp(cfg), opts
}

func copyProtocolFixtureCore(t *testing.T, bin string) string {
	t.Helper()
	source, err := os.Open(bin)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	path := filepath.Join(t.TempDir(), "xray")
	dest, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dest, source); err != nil {
		dest.Close()
		t.Fatal(err)
	}
	if err := dest.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func protocolHTTPSFixture(t *testing.T, ca protocolFixtureTLS) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	requests := new(atomic.Int64)
	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(http.StatusNoContent) }))
	target.TLS = &tls.Config{Certificates: []tls.Certificate{ca.certificate}, MinVersion: tls.VersionTLS12}
	target.StartTLS()
	t.Cleanup(target.Close)
	return target, requests
}

func TestNodeProtocolsRejectBadAuthenticationAndNodeTLS(t *testing.T) {
	bin := os.Getenv("PROXYSCENE_TEST_XRAY")
	if bin == "" {
		t.Skip("set PROXYSCENE_TEST_XRAY for real protocol integration")
	}
	bin = copyProtocolFixtureCore(t, bin)
	ca := newProtocolFixtureTLS(t)
	target, requests := protocolHTTPSFixture(t, ca)
	for _, kind := range []string{"vless", "vmess", "trojan", "ss", "hysteria2", "trojan-wrong-sni", "hysteria2-wrong-sni"} {
		t.Run(kind, func(t *testing.T) {
			protocol := strings.TrimSuffix(kind, "-wrong-sni")
			raw, inbound := protocolFixtureNode(t, protocol, ca)
			startProtocolFixtureServer(t, bin, inbound, target.URL)
			pn, err := parseRuntimeNode(raw)
			if err != nil {
				t.Fatal(err)
			}
			trustProtocolFixtureCA(pn, ca)
			a, opts := protocolFixtureProbe(t, bin)
			opts.targetURL, opts.allowLocalTarget = target.URL, true
			opts.tlsConfig = &tls.Config{RootCAs: ca.roots, MinVersion: tls.VersionTLS12}
			// A same-server positive control prevents a broken listener or
			// route from making this negative authentication case pass.
			controlBefore := requests.Load()
			if _, err := a.probeParsedNode(context.Background(), pn, opts); err != nil {
				t.Fatalf("positive authentication control failed: %v", err)
			}
			if requests.Load() != controlBefore+1 {
				t.Fatal("positive control did not reach HTTPS target")
			}
			settings := pn.Outbound["settings"].(map[string]any)
			if strings.HasSuffix(kind, "-wrong-sni") {
				pn.Outbound["streamSettings"].(map[string]any)["tlsSettings"].(map[string]any)["serverName"] = "wrong.probe.test"
			} else {
				switch protocol {
				case "vless", "vmess":
					settings["vnext"].([]any)[0].(map[string]any)["users"].([]any)[0].(map[string]any)["id"] = "22222222-2222-2222-2222-222222222222"
				case "trojan", "ss":
					settings["servers"].([]any)[0].(map[string]any)["password"] = "synthetic-wrong-password"
				case "hysteria2":
					pn.Outbound["streamSettings"].(map[string]any)["hysteriaSettings"].(map[string]any)["auth"] = "synthetic-wrong-password"
				}
			}
			opts.requestTimeout = 800 * time.Millisecond
			before := requests.Load()
			_, err = a.probeParsedNode(context.Background(), pn, opts)
			if err == nil || !strings.Contains(err.Error(), "节点 HTTPS 代理请求失败") {
				t.Fatalf("bad authentication/TLS must fail during HTTPS request: %v", err)
			}
			if requests.Load() != before {
				t.Fatal("bad credentials or certificate reached HTTPS target")
			}
		})
	}
}

func TestHysteria2RealHTTPSCanWinAutoSelection(t *testing.T) {
	bin := os.Getenv("PROXYSCENE_TEST_XRAY")
	if bin == "" {
		t.Skip("set PROXYSCENE_TEST_XRAY for real protocol integration")
	}
	bin = copyProtocolFixtureCore(t, bin)
	ca := newProtocolFixtureTLS(t)
	target, requests := protocolHTTPSFixture(t, ca)
	hyRaw, server := protocolFixtureNode(t, "hysteria2-hop-salamander", ca)
	startProtocolFixtureServer(t, bin, server, target.URL)
	tcpOnly, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpOnly.Close()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			conn, err := tcpOnly.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	defer func() { tcpOnly.Close(); <-stopped }()
	nodes := []Node{
		{ID: "tcp-false-positive", RawURL: "vless://11111111-1111-1111-1111-111111111111@" + tcpOnly.Addr().String() + "?security=none"},
		{ID: "hy2-working", RawURL: hyRaw},
	}
	a, opts := protocolFixtureProbe(t, bin)
	opts.targetURL, opts.allowLocalTarget = target.URL, true
	opts.tlsConfig = &tls.Config{RootCAs: ca.roots, MinVersion: tls.VersionTLS12}
	opts.requestTimeout = time.Second
	results := runNodeProbes(context.Background(), nodes, func(ctx context.Context, n Node) (time.Duration, error) {
		pn, err := parseRuntimeNode(n.RawURL)
		if err != nil {
			return 0, err
		}
		trustProtocolFixtureCA(pn, ca)
		return a.probeParsedNode(ctx, pn, opts)
	})
	if len(results) != 2 || results[0].Success || !results[1].Success || requests.Load() != 1 {
		t.Fatalf("false TCP positive or Hysteria2 failure: %+v", results)
	}
	store := newStore()
	store.Nodes = append([]Node(nil), nodes...)
	if got := fastestNodeID(mergeSpeedResults(store, nodes, results)); got != "hy2-working" {
		t.Fatalf("automatic selection chose %q", got)
	}
}

func TestClashImportedNodesForwardHTTPSWithPinnedXray(t *testing.T) {
	bin := os.Getenv("PROXYSCENE_TEST_XRAY")
	if bin == "" {
		t.Skip("set PROXYSCENE_TEST_XRAY for YAML-to-proxy integration")
	}
	bin = copyProtocolFixtureCore(t, bin)
	ca := newProtocolFixtureTLS(t)
	target, requests := protocolHTTPSFixture(t, ca)
	var entries []any
	for _, protocol := range []string{"vless", "vmess", "trojan", "ss", "hysteria2"} {
		_, inbound := protocolFixtureNode(t, protocol, ca)
		startProtocolFixtureServer(t, bin, inbound, target.URL)
		entry := map[string]any{"name": "local-" + protocol, "type": protocol, "server": "127.0.0.1", "port": inbound["port"], "udp": true}
		switch protocol {
		case "vless", "vmess":
			entry["uuid"] = "11111111-1111-1111-1111-111111111111"
		case "trojan", "hysteria2":
			entry["password"] = "synthetic-local-protocol-secret"
			entry["sni"] = "probe.test"
		case "ss":
			entry["cipher"] = "aes-128-gcm"
			entry["password"] = "synthetic-local-protocol-secret"
		}
		entries = append(entries, entry)
	}
	body, err := yaml.Marshal(map[string]any{"proxies": entries, "rules": []string{"MATCH,DIRECT"}, "proxy-providers": map[string]any{"unused": map[string]any{"url": "https://must-not-fetch.invalid/provider"}}})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareSubscriptionBody("https://synthetic.example/subscription", body)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSubscriptionRefresh(prepared); err != nil {
		t.Fatal(err)
	}
	app := testApp(t)
	store := newStore()
	if err := app.mergePreparedSubscription(store, prepared); err != nil {
		t.Fatal(err)
	}
	saved, err := app.loadStore()
	if err != nil || len(saved.Nodes) != 5 {
		t.Fatalf("YAML import did not persist all five nodes: %v", err)
	}
	for _, node := range saved.Nodes {
		t.Run(node.Protocol, func(t *testing.T) {
			pn, err := parseRuntimeNode(node.RawURL)
			if err != nil {
				t.Fatal(err)
			}
			trustProtocolFixtureCA(pn, ca)
			probe, opts := protocolFixtureProbe(t, bin)
			opts.targetURL, opts.allowLocalTarget = target.URL, true
			opts.tlsConfig = &tls.Config{RootCAs: ca.roots, MinVersion: tls.VersionTLS12}
			if _, err := probe.probeParsedNode(context.Background(), pn, opts); err != nil {
				t.Fatal(err)
			}
		})
	}
	if requests.Load() != 5 {
		t.Fatalf("got %d forwarded HTTPS requests; want 5", requests.Load())
	}
}
