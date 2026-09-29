package manager

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const subscriptionFixtureBody = "trojan://test-password@node.example:443#fixture\n"

func subscriptionFixtureResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestSubscriptionDirectFirstAndFallback(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		body         string
		networkError bool
		proxy        bool
		wantProxy    int
		wantError    bool
	}{
		{"direct success", 200, subscriptionFixtureBody, false, true, 0, false},
		{"network fallback", 0, "", true, true, 1, false},
		{"HTTP fallback", 403, "WAF", false, true, 1, false},
		{"invalid nodes do not retry", 200, "trojan://invalid", false, true, 0, true},
		{"HTML does not retry", 200, "<html>login</html>", false, true, 0, true},
		{"partial response does not retry", 206, subscriptionFixtureBody, false, true, 0, true},
		{"disabled global", 0, "", true, false, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var order []string
			direct := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				order = append(order, "direct")
				if req.Header.Get("User-Agent") != subscriptionUserAgent {
					t.Error("missing explicit subscription User-Agent")
				}
				if tc.networkError {
					return nil, errors.New("connection failed token=PRIVATE")
				}
				return subscriptionFixtureResponse(tc.status, tc.body), nil
			})}
			var proxy *http.Client
			if tc.proxy {
				proxy = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					order = append(order, "proxy")
					return subscriptionFixtureResponse(200, subscriptionFixtureBody), nil
				})}
			}
			d := &subscriptionDownloader{direct: direct, proxy: proxy}
			p, err := d.Prepare(context.Background(), "https://user:password@example.com/sub?token=PRIVATE")
			if (err != nil) != tc.wantError {
				t.Fatalf("unexpected result: %v", err)
			}
			if len(order) != 1+tc.wantProxy || order[0] != "direct" {
				t.Fatalf("route order: %v", order)
			}
			if err == nil && len(p.Nodes) != 1 {
				t.Fatalf("nodes=%d", len(p.Nodes))
			}
			if err != nil {
				for _, secret := range []string{"PRIVATE", "password", "https://user"} {
					if strings.Contains(err.Error(), secret) {
						t.Fatal("credential in error")
					}
				}
			}
		})
	}
}

func TestSubscriptionPolicyAndLimitsNeverFallBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func() (*http.Response, error)
	}{
		{"private destination", func() (*http.Response, error) { return nil, fmt.Errorf("dial: %w", errSubscriptionPolicy) }},
		{"oversized", func() (*http.Response, error) {
			r := subscriptionFixtureResponse(200, "")
			r.ContentLength = maxSubscriptionBytes + 1
			return r, nil
		}},
		{"content range", func() (*http.Response, error) {
			r := subscriptionFixtureResponse(200, subscriptionFixtureBody)
			r.Header.Set("Content-Range", "bytes 0-1/2")
			return r, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			d := &subscriptionDownloader{direct: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return tc.run() })}, proxy: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return subscriptionFixtureResponse(200, subscriptionFixtureBody), nil
			})}}
			_, err := d.Prepare(context.Background(), "https://example.com/sub")
			if err == nil || calls != 0 {
				t.Fatalf("policy/limit retried: err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestSubscriptionCancellationDoesNotFallBack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	d := &subscriptionDownloader{direct: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { cancel(); return nil, context.Canceled })}, proxy: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected proxy") })}}
	if _, err := d.Prepare(ctx, "https://example.com/sub"); err == nil || calls != 0 {
		t.Fatalf("cancelled request retried: %v/%d", err, calls)
	}
}

func TestSubscriptionBothRoutesFailOnceWithoutCredentials(t *testing.T) {
	calls := 0
	fail := roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("private-url-token") })
	d := &subscriptionDownloader{direct: &http.Client{Transport: fail}, proxy: &http.Client{Transport: fail}}
	_, err := d.Prepare(context.Background(), "https://user:password@example.com/SECRET")
	if err == nil || calls != 2 || !strings.Contains(err.Error(), "全局代理") {
		t.Fatalf("unexpected retry result %v calls=%d", err, calls)
	}
	for _, secret := range []string{"password", "SECRET", "private-url-token"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("credential in combined error")
		}
	}
}

func TestSubscriptionDirectIgnoresEnvironmentProxy(t *testing.T) {
	var proxyCalls atomic.Int32
	fakeProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxyCalls.Add(1); w.WriteHeader(502) }))
	defer fakeProxy.Close()
	t.Setenv("HTTP_PROXY", fakeProxy.URL)
	t.Setenv("HTTPS_PROXY", fakeProxy.URL)
	t.Setenv("ALL_PROXY", fakeProxy.URL)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, subscriptionFixtureBody) }))
	defer target.Close()
	client := subscriptionHTTPClient(true, true)
	defer client.CloseIdleConnections()
	p, err := downloadAndPrepareSubscriptionWithClient(target.URL, true, client)
	if err != nil || len(p.Nodes) != 1 || proxyCalls.Load() != 0 {
		t.Fatalf("direct request used environment proxy: %v calls=%d", err, proxyCalls.Load())
	}
	if client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("direct transport consults a proxy")
	}
}

// A local CONNECT relay maps the validated public destination to a test TLS
// server. No external node or network is contacted.
func subscriptionTestProxy(t *testing.T, target string, connects chan<- string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		connects <- r.Host
		upstream, err := net.DialTimeout("tcp", target, time.Second)
		if err != nil {
			http.Error(w, "upstream", http.StatusBadGateway)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		var once sync.Once
		closeBoth := func() { once.Do(func() { conn.Close(); upstream.Close() }) }
		go func() { _, _ = io.Copy(upstream, rw); closeBoth() }()
		go func() { _, _ = io.Copy(conn, upstream); closeBoth() }()
	}))
}

func TestSubscriptionProxyPinsIPAndPreservesTLSIdentity(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS.ServerName != "example.com" || !strings.HasPrefix(r.Host, "example.com:") {
			t.Error("original TLS/HTTP hostname lost")
		}
		_, _ = io.WriteString(w, subscriptionFixtureBody)
	}))
	defer target.Close()
	_, port, _ := net.SplitHostPort(target.Listener.Addr().String())
	connects := make(chan string, 4)
	proxy := subscriptionTestProxy(t, target.Listener.Addr().String(), connects)
	defer proxy.Close()
	dialer := &subscriptionProxyDialer{proxyAddress: proxy.Listener.Addr().String(), lookupIP: func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}}
	client := subscriptionHTTPClient(false, false)
	tr := client.Transport.(*http.Transport)
	tr.DialContext = dialer.DialContext
	roots := x509.NewCertPool()
	roots.AddCert(target.Certificate())
	tr.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	defer client.CloseIdleConnections()
	p, err := downloadAndPrepareSubscriptionWithClient("https://example.com:"+port+"/secret", false, client)
	if err != nil || len(p.Nodes) != 1 {
		t.Fatalf("proxy download: %v", err)
	}
	if got := <-connects; got != net.JoinHostPort("8.8.8.8", port) {
		t.Fatalf("CONNECT did not pin validated IP: %s", got)
	}
	if _, err := downloadAndPrepareSubscriptionWithClient("https://wrong.example.net:"+port+"/secret", false, client); err == nil {
		t.Fatal("wrong TLS hostname accepted")
	}
}

func TestSubscriptionProxyRejectsPrivateAndRebindingDestinations(t *testing.T) {
	for _, tc := range []struct {
		name, address string
		ips           []net.IPAddr
	}{
		{"literal loopback", "127.0.0.1:443", nil},
		{"metadata", "169.254.169.254:80", nil},
		{"IPv6 loopback", "[::1]:443", nil},
		{"DNS private", "example.com:443", []net.IPAddr{{IP: net.ParseIP("10.1.2.3")}}},
		{"mixed DNS", "example.com:443", []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("127.0.0.1")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &subscriptionProxyDialer{proxyAddress: "127.0.0.1:1", lookupIP: func(context.Context, string) ([]net.IPAddr, error) { return tc.ips, nil }}
			_, err := d.DialContext(context.Background(), "tcp", tc.address)
			if !errors.Is(err, errSubscriptionPolicy) {
				t.Fatalf("private destination reached proxy: %v", err)
			}
		})
	}
}

func TestSubscriptionProxyRedirectCannotReachPrivateHost(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
	}))
	defer target.Close()
	_, port, _ := net.SplitHostPort(target.Listener.Addr().String())
	connects := make(chan string, 4)
	proxy := subscriptionTestProxy(t, target.Listener.Addr().String(), connects)
	defer proxy.Close()
	d := &subscriptionProxyDialer{proxyAddress: proxy.Listener.Addr().String(), lookupIP: func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}}
	client := subscriptionHTTPClient(true, false)
	tr := client.Transport.(*http.Transport)
	tr.DialContext = d.DialContext
	roots := x509.NewCertPool()
	roots.AddCert(target.Certificate())
	tr.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	defer client.CloseIdleConnections()
	_, err := downloadAndPrepareSubscriptionWithClient("https://example.com:"+port+"/secret", true, client)
	if err == nil || !strings.Contains(err.Error(), "安全策略") || len(connects) != 1 {
		t.Fatalf("redirect policy failed: %v CONNECTs=%d", err, len(connects))
	}
}

func TestSubscriptionUsesSavedGlobalPortAndEnabledState(t *testing.T) {
	t.Setenv("PROXYSCENE_ALLOW_PRIVATE_SUBSCRIPTION", "1")
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, subscriptionFixtureBody) }))
	defer target.Close()
	connects := make(chan string, 2)
	proxy := subscriptionTestProxy(t, target.Listener.Addr().String(), connects)
	defer proxy.Close()
	cfg := DefaultConfig()
	cfg.ProxyHost = "0.0.0.0"
	cfg.AllowPublicBind = true
	_, portText, _ := net.SplitHostPort(proxy.Listener.Addr().String())
	var port int
	_, _ = fmt.Sscan(portText, &port)
	cfg.GlobalHTTPPort = port
	st := newStore()
	st.RuntimeConfig = cfg.runtimeConfig()
	st.SceneEnabled[SceneGlobal] = true
	app := NewApp(cfg)
	app.cfg.ProxyHost = "127.0.0.2"
	app.cfg.GlobalHTTPPort = 1
	app.cfg.runtimeOverrides.GlobalHTTPPort = true
	d, err := app.subscriptionDownloaderForStore(st)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.allowHTTP = true
	d.direct = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("test direct outage") })}
	p, err := d.Prepare(context.Background(), target.URL)
	if err != nil || len(p.Nodes) != 1 || len(connects) != 1 {
		t.Fatalf("saved global proxy not used: %v", err)
	}
	st.SceneEnabled[SceneGlobal] = false
	off, err := app.subscriptionDownloaderForStore(st)
	if err != nil {
		t.Fatal(err)
	}
	defer off.Close()
	if off.proxy != nil {
		t.Fatal("disabled global scene created fallback")
	}
}
