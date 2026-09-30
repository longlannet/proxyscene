package manager

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func probeFixtureOptions(t *testing.T, target *httptest.Server) nodeProbeOptions {
	t.Helper()
	bin := os.Getenv("PROXYSCENE_TEST_XRAY")
	if bin == "" {
		t.Skip("set PROXYSCENE_TEST_XRAY for isolated core integration")
	}
	copyPath := filepath.Join(t.TempDir(), "xray")
	src, err := os.Open(bin)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(copyPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	bin = copyPath
	name := "nobody"
	if os.Geteuid() != 0 {
		u, err := user.Current()
		if err != nil {
			t.Fatal(err)
		}
		name = u.Username
	}
	id, err := lookupLocalUserIdentity(name)
	if err != nil {
		t.Fatal(err)
	}
	opts := nodeProbeOptions{allowLocalTarget: true, tempRoot: t.TempDir(), openCoreForTest: func() (*os.File, error) { return os.Open(bin) }, identityForTest: &id, startupTimeout: 2 * time.Second, requestTimeout: 2 * time.Second}
	if target != nil {
		opts.targetURL = target.URL
		opts.tlsConfig = target.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	}
	return opts
}

func TestNodeProbeHTTPSRequestAndCleanup(t *testing.T) {
	var count atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { count.Add(1); w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	opts := probeFixtureOptions(t, server)
	// The fixture outbound is intentionally local freedom. Protocol-specific
	// integrations separately prove that each real node protocol carries HTTPS.
	pn := &parsedNode{Outbound: map[string]any{"protocol": "freedom"}}
	a := &App{cfg: DefaultConfig()}
	latency, err := a.probeParsedNode(context.Background(), pn, opts)
	if err != nil {
		t.Fatal(err)
	}
	if count.Load() != 1 || latency < 0 {
		t.Fatalf("request count=%d latency=%v", count.Load(), latency)
	}
	entries, err := os.ReadDir(opts.tempRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary probe state retained: %v %v", entries, err)
	}
}

func TestNodeProbeRefusesUnverifiedHTTPSAndHTTPFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		trust  bool
		delay  time.Duration
	}{
		{"untrusted certificate", http.StatusNoContent, "", false, 0},
		{"http failure", http.StatusBadGateway, "", true, 0},
		{"redirect", http.StatusFound, "", true, 0},
		{"oversize response", http.StatusOK, strings.Repeat("x", nodeProbeBodyLimit+1), true, 0},
		{"timeout", http.StatusNoContent, "", true, 250 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.delay > 0 {
					select {
					case <-r.Context().Done():
						return
					case <-time.After(tc.delay):
					}
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			opts := probeFixtureOptions(t, server)
			if !tc.trust {
				opts.tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
			}
			if tc.delay > 0 {
				opts.requestTimeout = 50 * time.Millisecond
			}
			_, err := (&App{}).probeParsedNode(context.Background(), &parsedNode{Outbound: map[string]any{"protocol": "freedom"}}, opts)
			if err == nil {
				t.Fatal("unsafe or failed request succeeded")
			}
			want := "节点 HTTPS"
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("failed before intended request check: %v", err)
			}
			if strings.Contains(err.Error(), server.URL) {
				t.Fatalf("error exposed target URL: %v", err)
			}
			entries, _ := os.ReadDir(opts.tempRoot)
			if len(entries) != 0 {
				t.Fatal("probe files remain")
			}
		})
	}
}

func TestNodeProbeCancellationCleansUp(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() }))
	defer server.Close()
	opts := probeFixtureOptions(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (&App{}).probeParsedNode(ctx, &parsedNode{Outbound: map[string]any{"protocol": "freedom"}}, opts)
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("probe exited before request: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("probe failed to start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation left running core")
	}
	entries, _ := os.ReadDir(opts.tempRoot)
	if len(entries) != 0 {
		t.Fatal("cancelled probe retained files")
	}
}

func TestNodeProbeOnlyOwnChildListenerIsAccepted(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	if !nodeProbeOwnsListener(os.Getpid(), port) {
		t.Fatal("did not recognize own socket")
	}
	if nodeProbeOwnsListener(os.Getppid(), port) {
		t.Fatal("accepted another process socket")
	}
}

func TestNodeProbeURLPolicy(t *testing.T) {
	for _, raw := range []string{"http://example.com/", "https://user:password@example.com/", "https://example.com/#secret", "https://127.0.0.1/", "https://[::1]/", "https://localhost/", "https://service.localhost/", "https://example.com:0/"} {
		if _, err := nodeProbeURL(raw, false); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, err := nodeProbeURL("https://www.google.com/generate_204", false); err != nil {
		t.Fatal(err)
	}
}

func TestNodeProbeBudgetAndLatency(t *testing.T) {
	nodes := make([]Node, 80)
	for i := range nodes {
		nodes[i].ID = itoa(i)
	}
	var active, maximum, calls atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	results := runNodeProbes(ctx, nodes, func(ctx context.Context, n Node) (time.Duration, error) {
		calls.Add(1)
		now := active.Add(1)
		defer active.Add(-1)
		for {
			old := maximum.Load()
			if old >= now || maximum.CompareAndSwap(old, now) {
				break
			}
		}
		if n.ID == "0" {
			time.Sleep(10 * time.Millisecond)
			return 2 * time.Millisecond, nil
		}
		<-ctx.Done()
		return 0, errors.New("cancelled")
	})
	if maximum.Load() > nodeProbeConcurrency || calls.Load() > nodeProbeConcurrency+1 {
		t.Fatalf("unbounded probes: concurrent=%d calls=%d", maximum.Load(), calls.Load())
	}
	if !results[0].Success || results[0].LatencyMS != 2 || results[0].Target != nodeProbeTarget {
		t.Fatalf("request duration lost: %+v", results[0])
	}
	for i := 1; i < len(results); i++ {
		if results[i].Success || results[i].Error == "" {
			t.Fatalf("unstarted/cancelled node succeeded: %+v", results[i])
		}
	}
}

func TestNodeProbeDoesNotModifyProductionFiles(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	opts := probeFixtureOptions(t, server)
	cfg := DefaultConfig()
	cfg.CoreDir = t.TempDir()
	marker := []byte("production remains unchanged")
	if err := os.WriteFile(filepath.Join(cfg.CoreDir, "config.json"), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	old := systemctlRun
	systemctlRun = func(string, ...string) error { t.Error("probe invoked systemctl"); return errors.New("forbidden") }
	t.Cleanup(func() { systemctlRun = old })
	_, err := (&App{cfg: cfg}).probeParsedNode(context.Background(), &parsedNode{Outbound: map[string]any{"protocol": "freedom"}}, opts)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(cfg.CoreDir, "config.json"))
	if err != nil || string(raw) != string(marker) {
		t.Fatal("production config modified")
	}
}

func TestNodeProbeChildPrivilegeAndPrivateState(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	opts := probeFixtureOptions(t, server)
	var pid int
	opts.onStartedForTest = func(child int, dir string) {
		pid = child
		// Start observes the exec error pipe closing before /proc necessarily
		// exposes the new argv. Retry only empty reads, never an unsafe argv.
		deadline := time.Now().Add(time.Second)
		for {
			argv, err := os.ReadFile(filepath.Join("/proc", itoa(pid), "cmdline"))
			if err != nil {
				t.Error(err)
				return
			}
			if len(argv) > 0 {
				if !strings.Contains(string(argv), "stdin:") || strings.Contains(string(argv), "config.json") {
					t.Errorf("probe argv exposes configuration path: %q", argv)
				}
				break
			}
			if !time.Now().Before(deadline) {
				t.Error("probe argv remained empty after exec")
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		for path, mode := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "config.json"): 0o600} {
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != mode {
				t.Errorf("private probe state permissions: %v mode=%v", err, mode)
			}
		}
		status, err := os.ReadFile(filepath.Join("/proc", itoa(pid), "status"))
		if err != nil {
			t.Error(err)
			return
		}
		if err := coreProcCredentials(status, *opts.identityForTest); err != nil {
			t.Error(err)
		}
		for key, want := range map[string]string{"NoNewPrivs": "1", "Groups": "", "CapEff": "0000000000000000", "CapPrm": "0000000000000000", "CapAmb": "0000000000000000"} {
			if key == "Groups" && os.Geteuid() != 0 {
				continue
			}
			found := false
			for _, line := range strings.Split(string(status), "\n") {
				name, value, ok := strings.Cut(line, ":")
				if ok && name == key {
					found = true
					if strings.TrimSpace(value) != want {
						t.Errorf("unsafe child %s=%s", key, value)
					}
				}
			}
			if !found {
				t.Errorf("missing child status %s", key)
			}
		}
		env, err := os.ReadFile(filepath.Join("/proc", itoa(pid), "environ"))
		if err != nil {
			t.Error(err)
		}
		if strings.Contains(string(env), "PROXY=") {
			t.Error("probe inherited proxy environment")
		}
	}
	_, err := (&App{}).probeParsedNode(context.Background(), &parsedNode{Outbound: map[string]any{"protocol": "freedom"}}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if pid == 0 {
		t.Fatal("no observed child")
	}
	if _, err := os.Stat(filepath.Join("/proc", itoa(pid))); !os.IsNotExist(err) {
		t.Fatalf("probe child not reaped: %v", err)
	}
}

func TestNodeProbeCancelledBatchDoesNotReturnSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	results, err := runNodeProbeBatch(ctx, []Node{{ID: "previously-good"}}, func(context.Context, Node) (time.Duration, error) { calls++; cancel(); return time.Millisecond, nil })
	if !errors.Is(err, context.Canceled) || calls != 1 || len(results) != 1 {
		t.Fatalf("cancelled batch permitted caller to commit: results=%+v err=%v", results, err)
	}
}

func TestNodeProbeInboundAuthenticationAndTargetRestriction(t *testing.T) {
	var targetRequests, otherRequests atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherRequests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer other.Close()
	opts := probeFixtureOptions(t, target)
	opts.onStartedForTest = func(pid int, dir string) {
		raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if err != nil {
			t.Error(err)
			return
		}
		var cfg struct {
			Inbounds []struct {
				Port     int `json:"port"`
				Settings struct {
					Accounts []struct{ User, Pass string } `json:"accounts"`
				} `json:"settings"`
			} `json:"inbounds"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil || len(cfg.Inbounds) != 1 || len(cfg.Inbounds[0].Settings.Accounts) != 1 {
			t.Errorf("invalid probe inbound: %v", err)
			return
		}
		in := cfg.Inbounds[0]
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := waitNodeProbeListener(ctx, pid, in.Port, make(chan struct{})); err != nil {
			t.Error(err)
			return
		}
		for _, tc := range []struct{ name, password, target string }{
			{"missing auth", "", target.URL},
			{"wrong auth", "incorrect", target.URL},
			{"off target", in.Settings.Accounts[0].Pass, other.URL},
		} {
			proxy := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", itoa(in.Port))}
			if tc.password != "" {
				proxy.User = url.UserPassword("probe", tc.password)
			}
			transport := &http.Transport{Proxy: http.ProxyURL(proxy), TLSClientConfig: opts.tlsConfig.Clone()}
			client := &http.Client{Transport: transport, Timeout: 100 * time.Millisecond}
			resp, err := client.Get(tc.target)
			transport.CloseIdleConnections()
			if err == nil {
				resp.Body.Close()
				t.Errorf("%s reached HTTPS", tc.name)
			}
		}
	}
	_, err := (&App{}).probeParsedNode(context.Background(), &parsedNode{Outbound: map[string]any{"protocol": "freedom"}}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if targetRequests.Load() != 1 || otherRequests.Load() != 0 {
		t.Fatalf("inbound allowed unintended request: target=%d other=%d", targetRequests.Load(), otherRequests.Load())
	}
}

func TestNodeProbeProductionTrustAndDroppedIdentity(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("production root-owned core trust requires privileged test")
	}
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer target.Close()
	opts := probeFixtureOptions(t, target)
	source, err := opts.openCoreForTest()
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	cfg := DefaultConfig()
	cfg.CoreDir = t.TempDir()
	cfg.XrayServiceUser = "nobody"
	dst, err := os.OpenFile(cfg.XrayBin(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, source); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	opts.openCoreForTest = nil
	opts.identityForTest = nil
	if _, err := (&App{cfg: cfg}).probeParsedNode(context.Background(), &parsedNode{Outbound: map[string]any{"protocol": "freedom"}}, opts); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg.XrayBin(), 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := (&App{cfg: cfg}).probeParsedNode(context.Background(), &parsedNode{Outbound: map[string]any{"protocol": "freedom"}}, opts); err == nil || !strings.Contains(err.Error(), "无法验证") {
		t.Fatalf("untrusted executable was permitted: %v", err)
	}
}

func TestNodeProbeEarlyCoreExitCleansPrivateState(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("invalid core reached HTTPS target") }))
	defer target.Close()
	opts := probeFixtureOptions(t, target)
	_, err := (&App{}).probeParsedNode(context.Background(), &parsedNode{Outbound: map[string]any{"protocol": "invalid-local-fixture"}}, opts)
	if err == nil || !strings.Contains(err.Error(), "隔离 Xray") {
		t.Fatalf("invalid core config accepted: %v", err)
	}
	entries, readErr := os.ReadDir(opts.tempRoot)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("failed start retained files: %v %v", entries, readErr)
	}
}
